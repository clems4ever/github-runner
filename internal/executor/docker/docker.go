// Package docker runs each container runner as a Docker container.
//
// Like the systemd executor, this one does not supervise anything: containers
// carry a restart policy, so dockerd brings them back after a crash or a
// reboot, and the daemon can be replaced without a job noticing. Containers
// are found again by their labels rather than by anything the daemon
// remembers.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/paths"
	"github.com/clems4ever/github-runner/internal/reconcile"
)

// Labels the daemon stamps on every container it owns. They are how a
// restarted daemon tells its own containers from everything else on the host,
// and how it knows which configuration each was built from.
const (
	LabelRunner     = "io.runner-fleet.runner"
	LabelPool       = "io.runner-fleet.pool"
	LabelGeneration = "io.runner-fleet.generation"
)

// DefaultSocket is where dockerd listens.
const DefaultSocket = "/var/run/docker.sock"

// DefaultImage carries the GitHub runner itself. The agent binary is
// bind-mounted in and used as the entrypoint, so the image only has to provide
// the runner and its dependencies.
const DefaultImage = "ghcr.io/actions/actions-runner:latest"

// stopTimeout is how long dockerd waits after SIGTERM before killing a
// container. It matches the VM side: a runner finishes the job it is on, and
// an hour is longer than any job worth waiting for.
const stopTimeout = 3600

// Executor creates, drains and removes container runners.
type Executor struct {
	layout paths.Layout
	http   *http.Client
	host   string // base URL; for a unix socket this is a placeholder host
	binary string

	// draining is the set of runners that have been asked to stop.
	//
	// Docker has nowhere to record this: a container's labels cannot be
	// changed once it is running, and a stop that waits for a job cannot be
	// waited on inline. A restarted daemon loses this and asks again, which is
	// harmless — a second stop of a container that is already stopping changes
	// nothing.
	mu       sync.Mutex
	draining map[string]bool
}

// Option configures the executor.
type Option func(*Executor)

// WithHTTPClient replaces the transport, which is how the tests point this at
// an httptest server instead of a socket.
func WithHTTPClient(c *http.Client, host string) Option {
	return func(e *Executor) { e.http, e.host = c, host }
}

// New builds an executor talking to the local Docker daemon.
func New(layout paths.Layout, binary string, opts ...Option) *Executor {
	e := &Executor{
		layout:   layout,
		binary:   binary,
		host:     "http://docker",
		draining: map[string]bool{},
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", DefaultSocket)
				},
			},
			Timeout: 60 * time.Second,
		},
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Runtime is what this executor runs.
func (e *Executor) Runtime() model.Runtime { return model.RuntimeContainer }

// Create builds and starts a container runner.
func (e *Executor) Create(ctx context.Context, spec reconcile.Spec) error {
	image := spec.Image
	if image == "" || image == "default" {
		image = DefaultImage
	}
	if err := e.ensureImage(ctx, image); err != nil {
		return err
	}

	config := map[string]any{
		"Image": image,
		"Env":   env(spec, e.layout),
		"Labels": map[string]string{
			LabelRunner:     spec.Name,
			LabelPool:       spec.Pool,
			LabelGeneration: spec.Generation,
		},
		// The agent is the entrypoint: it registers the runner with a token it
		// mints itself, then runs it. Bind-mounting one binary is what keeps
		// the image free of anything fleet-specific.
		"Entrypoint": []string{"/usr/local/bin/runner-fleet", "agent", "--name", spec.Name},
		"HostConfig": e.hostConfig(spec),
	}

	var created struct {
		ID string `json:"Id"`
	}
	if err := e.do(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(spec.Name), config, &created); err != nil {
		return err
	}
	return e.do(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, nil)
}

func (e *Executor) hostConfig(spec reconcile.Spec) map[string]any {
	host := map[string]any{
		// unless-stopped, not always: a container the daemon has deliberately
		// stopped — one being drained — must stay stopped, or draining would
		// be undone by dockerd a second later.
		"RestartPolicy": map[string]any{"Name": "unless-stopped"},
		"Binds": []string{
			// The credential lives on tmpfs and is mounted read-only. It is
			// never baked into the image or the container's environment.
			e.layout.Credential(spec.CredentialID) + ":/run/secrets/github_token:ro",
			e.binary + ":/usr/local/bin/runner-fleet:ro",
		},
		"Memory":   int64(spec.MemoryMB) * 1024 * 1024,
		"NanoCpus": int64(spec.CPUs) * 1_000_000_000,
	}

	if spec.Nested {
		// Nested virtualisation in a container means handing the job the
		// host's KVM device. It is a real hole in the boundary — far weaker
		// than the VM runtime — which is why it is off unless a pool asks for
		// it, and why the label says so.
		host["Devices"] = []map[string]string{{
			"PathOnHost":        "/dev/kvm",
			"PathInContainer":   "/dev/kvm",
			"CgroupPermissions": "rwm",
		}}
	}
	return host
}

func env(spec reconcile.Spec, layout paths.Layout) []string {
	return []string{
		"FLEET_RUNNER=" + spec.Name,
		"FLEET_POOL=" + spec.Pool,
		"FLEET_GENERATION=" + spec.Generation,
		"FLEET_URL=" + spec.URL,
		"FLEET_SCOPE_KIND=" + string(spec.ScopeKind),
		"FLEET_SCOPE=" + spec.Scope,
		"FLEET_LABELS=" + strings.Join(spec.Labels, ","),
		fmt.Sprintf("FLEET_EPHEMERAL=%t", spec.Ephemeral),
		fmt.Sprintf("FLEET_NESTED=%t", spec.Nested),
		"FLEET_RUNTIME=container",
		// Inside the container the credential is at a fixed path, because the
		// bind mount put it there.
		"FLEET_CREDENTIAL_FILE=/run/secrets/github_token",
	}
}

// Start brings back a container that exists but has stopped.
func (e *Executor) Start(ctx context.Context, name string) error {
	e.mu.Lock()
	delete(e.draining, name)
	e.mu.Unlock()
	return e.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/start", nil, nil)
}

// Drain asks a runner to stop when its job is done, and returns immediately.
//
// Docker's stop blocks until the container is gone or the timeout expires, and
// the timeout here is an hour, so the request is made in the background. The
// runner is reported as stopping in the meantime, which keeps the reconciler
// from deleting a machine that still has a job on it.
func (e *Executor) Drain(ctx context.Context, name string) error {
	e.mu.Lock()
	already := e.draining[name]
	e.draining[name] = true
	e.mu.Unlock()
	if already {
		return nil
	}

	go func() {
		// Not the caller's context: it belongs to one reconcile pass, and this
		// outlives many of them.
		ctx, cancel := context.WithTimeout(context.Background(), (stopTimeout+60)*time.Second)
		defer cancel()
		path := fmt.Sprintf("/containers/%s/stop?t=%d", url.PathEscape(name), stopTimeout)
		_ = e.do(ctx, http.MethodPost, path, nil, nil)
	}()
	return nil
}

// Remove deletes a container that has stopped, and its volumes with it.
func (e *Executor) Remove(ctx context.Context, name string) error {
	e.mu.Lock()
	delete(e.draining, name)
	e.mu.Unlock()

	err := e.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(name)+"?v=1", nil, nil)
	if isNotFound(err) {
		return nil // already gone is the outcome that was wanted
	}
	return err
}

type container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

// List reports every container runner on this host, found by label.
func (e *Executor) List(ctx context.Context) ([]reconcile.Runner, error) {
	filters := url.QueryEscape(`{"label":["` + LabelRunner + `"]}`)
	var containers []container
	if err := e.do(ctx, http.MethodGet, "/containers/json?all=1&filters="+filters, nil, &containers); err != nil {
		return nil, err
	}

	e.mu.Lock()
	draining := make(map[string]bool, len(e.draining))
	for name := range e.draining {
		draining[name] = true
	}
	e.mu.Unlock()

	runners := make([]reconcile.Runner, 0, len(containers))
	for _, c := range containers {
		name := c.Labels[LabelRunner]
		if name == "" {
			continue
		}
		state := mapState(c.State)
		if state == reconcile.StateRunning && draining[name] {
			state = reconcile.StateStopping
		}
		runners = append(runners, reconcile.Runner{
			Name:       name,
			Pool:       c.Labels[LabelPool],
			Generation: c.Labels[LabelGeneration],
			Runtime:    model.RuntimeContainer,
			State:      state,
		})
	}
	sort.Slice(runners, func(i, j int) bool { return runners[i].Name < runners[j].Name })
	return runners, nil
}

// mapState turns Docker's vocabulary into the three states the reconciler
// reasons about.
func mapState(state string) reconcile.RunnerState {
	switch state {
	case "running", "restarting", "created":
		return reconcile.StateRunning
	case "removing":
		return reconcile.StateStopping
	default: // exited, dead, paused
		return reconcile.StateStopped
	}
}

// ensureImage pulls an image that is not on the host yet. Pulling on demand is
// what makes a pool's image field usable without an extra step for the
// operator — and it is where per-repository images will arrive, since a pool
// naming its own image needs nothing else to change.
func (e *Executor) ensureImage(ctx context.Context, image string) error {
	if err := e.do(ctx, http.MethodGet, "/images/"+url.PathEscape(image)+"/json", nil, nil); err == nil {
		return nil
	}
	ref, tag := image, "latest"
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		ref, tag = image[:i], image[i+1:]
	}
	path := fmt.Sprintf("/images/create?fromImage=%s&tag=%s", url.QueryEscape(ref), url.QueryEscape(tag))
	return e.do(ctx, http.MethodPost, path, nil, nil)
}

// Ping reports whether Docker is usable, so the UI can say that a container
// pool will not work on this host rather than failing later.
func (e *Executor) Ping(ctx context.Context) error {
	return e.do(ctx, http.MethodGet, "/_ping", nil, nil)
}

type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return fmt.Sprintf("docker: %d: %s", e.status, e.message) }

func isNotFound(err error) bool {
	var apiErr *apiError
	if ok := asAPIError(err, &apiErr); ok {
		return apiErr.status == http.StatusNotFound
	}
	return false
}

func asAPIError(err error, target **apiError) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*apiError); ok {
		*target = e
		return true
	}
	return false
}

func (e *Executor) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, e.host+"/v1.44"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := e.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker: %s %s: %w (is dockerd running, and can this user reach its socket?)", method, path, err)
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var message struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(payload, &message)
		text := message.Message
		if text == "" {
			text = strings.TrimSpace(string(payload))
		}
		return &apiError{status: resp.StatusCode, message: text}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(payload, out)
}
