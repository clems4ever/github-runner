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
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/paths"
	"github.com/clems4ever/github-runner/internal/reconcile"
	"github.com/clems4ever/github-runner/internal/resources"
)

// Labels the daemon stamps on every container it owns. They are how a
// restarted daemon tells its own containers from everything else on the host,
// and how it knows which configuration each was built from.
const (
	LabelRunner     = "io.runner-fleet.runner"
	LabelPool       = "io.runner-fleet.pool"
	LabelGeneration = "io.runner-fleet.generation"
	// The scope and credential travel with the container so that a runner
	// whose pool has been deleted can still be asked about: it is registered
	// somewhere, and the daemon has to be able to find out whether a job is on
	// it before removing it.
	LabelScopeKind  = "io.runner-fleet.scope-kind"
	LabelScope      = "io.runner-fleet.scope"
	LabelCredential = "io.runner-fleet.credential"
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
	socket string // empty when not talking to a socket, which is how the tests run
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

	// cpu remembers each container's processor counter between samples, which
	// is what makes a percentage out of it.
	cpu *resources.Rate
}

// Option configures the executor.
type Option func(*Executor)

// WithHTTPClient replaces the transport, which is how the tests point this at
// an httptest server instead of a socket.
func WithHTTPClient(c *http.Client, host string) Option {
	return func(e *Executor) { e.http, e.host, e.socket = c, host, "" }
}

// New builds an executor talking to the local Docker daemon.
func New(layout paths.Layout, binary string, opts ...Option) *Executor {
	e := &Executor{
		layout:   layout,
		binary:   binary,
		host:     "http://docker",
		socket:   DefaultSocket,
		draining: map[string]bool{},
		cpu:      resources.NewRate(),
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

// Recipe is the container image a runner in this pool would run.
//
// A pool that names no image gets the default, so changing that default here
// replaces the containers built from the old one rather than leaving them until
// somebody notices.
func (e *Executor) Recipe(pool model.Pool) string { return imageFor(pool.Image) }

// imageFor resolves what a pool's image field means, so the executor and the
// recipe cannot disagree about what a runner would run.
func imageFor(image string) string {
	if image == "" || image == "default" {
		return DefaultImage
	}
	return image
}

// Create builds and starts a container runner.
func (e *Executor) Create(ctx context.Context, spec reconcile.Spec) error {
	image := imageFor(spec.Image)
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
			LabelScopeKind:  string(spec.ScopeKind),
			LabelScope:      spec.Scope,
			LabelCredential: strconv.FormatInt(spec.CredentialID, 10),
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
		// No restart policy: the daemon replaces these rather than dockerd
		// restarting them. A container registers with a token that expires in
		// an hour, so starting the same one again later cannot work — it would
		// fail to register and loop, looking healthy to anyone watching
		// Docker.
		"RestartPolicy": map[string]any{"Name": "no"},
		"Binds": []string{
			// Only the agent. The credential is deliberately not here: a
			// container shares everything with the job it runs, so mounting the
			// key that mints tokens would hand every job something that
			// administers repositories. The daemon mints instead, and passes a
			// token that can do one thing.
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
		// What the runner registers with. Short-lived and single-purpose: the
		// worst a job can do with it is register another runner, where the
		// credential it replaces could administer the repository.
		"FLEET_REGISTRATION_TOKEN=" + spec.RegistrationToken,
	}
}

// Start brings back a runner that exists but has stopped, by building it
// again.
//
// Not by starting the container it left behind: that container registered with
// a token that has since expired, so starting it would fail to register, exit,
// and be started again on the next pass. A runner is cheap to rebuild and a
// loop is expensive to debug.
func (e *Executor) Start(ctx context.Context, spec reconcile.Spec) error {
	e.mu.Lock()
	delete(e.draining, spec.Name)
	e.mu.Unlock()

	if err := e.Remove(ctx, spec.Name); err != nil {
		return err
	}
	return e.Create(ctx, spec)
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
//
// A host without Docker has no container runners, which is a true answer
// rather than an error: reporting one would put a stack trace about a missing
// socket in front of an operator who only ever wanted virtual machines.
func (e *Executor) List(ctx context.Context) ([]reconcile.Runner, error) {
	if !e.available() {
		return nil, nil
	}
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
		credentialID, _ := strconv.ParseInt(c.Labels[LabelCredential], 10, 64)
		runners = append(runners, reconcile.Runner{
			Name:         name,
			Pool:         c.Labels[LabelPool],
			Generation:   c.Labels[LabelGeneration],
			Runtime:      model.RuntimeContainer,
			State:        state,
			ScopeKind:    model.ScopeKind(c.Labels[LabelScopeKind]),
			Scope:        c.Labels[LabelScope],
			CredentialID: credentialID,
		})
	}
	sort.Slice(runners, func(i, j int) bool { return runners[i].Name < runners[j].Name })
	return runners, nil
}

// containerStats is the part of Docker's stats document this needs.
//
// The memory figure is deliberately not "usage": that includes the page cache,
// so a container that has read a large repository reports most of its limit
// used and looks about to die. Docker's own CLI subtracts the inactive file
// cache before printing, and so does this — under both names the two cgroup
// versions give it.
type containerStats struct {
	CPU struct {
		Usage struct {
			Total uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
	} `json:"cpu_stats"`
	Memory struct {
		Usage int64            `json:"usage"`
		Stats map[string]int64 `json:"stats"`
	} `json:"memory_stats"`
}

func (s containerStats) memory() int64 {
	used := s.Memory.Usage
	for _, key := range []string{"inactive_file", "total_inactive_file"} {
		if cache, ok := s.Memory.Stats[key]; ok {
			used -= cache
			break
		}
	}
	if used < 0 {
		return 0
	}
	return used
}

// Usage reports what each container runner is consuming.
//
// One-shot statistics: asked without it, Docker holds the request open for a
// second so that it can compute a processor percentage from two readings of
// its own. Across a fleet that is a second of the daemon's attention per
// container, every sample, to arrive at a number this can work out for itself
// from the counter — and over a fifteen-second window rather than a one-second
// one, which is the better measurement anyway.
func (e *Executor) Usage(ctx context.Context) ([]resources.RunnerUsage, error) {
	if !e.available() {
		return nil, nil
	}
	runners, err := e.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list what the containers are using: %w", err)
	}

	usage := make([]resources.RunnerUsage, 0, len(runners))
	names := make([]string, 0, len(runners))
	var unreadable []string
	for _, runner := range runners {
		names = append(names, runner.Name)
		// A stopped container has no cgroup to ask about, and asking would only
		// produce a row of zeroes that reads as a runner doing nothing rather
		// than as a runner that is not there.
		if runner.State == reconcile.StateStopped {
			continue
		}

		var stats containerStats
		path := "/containers/" + url.PathEscape(runner.Name) + "/stats?stream=false&one-shot=true"
		if err := e.do(ctx, http.MethodGet, path, nil, &stats); err != nil {
			// Removed between the listing and the question, which is ordinary
			// for an ephemeral fleet and not worth telling anyone about.
			if !isNotFound(err) {
				unreadable = append(unreadable, runner.Name)
			}
			continue
		}
		usage = append(usage, resources.RunnerUsage{
			Name:        runner.Name,
			Pool:        runner.Pool,
			Runtime:     string(model.RuntimeContainer),
			CPUPercent:  e.cpu.Percent(runner.Name, stats.CPU.Usage.Total),
			MemoryBytes: stats.memory(),
		})
	}
	e.cpu.Keep(names)

	if len(unreadable) > 0 {
		return usage, fmt.Errorf("docker would not say what these containers are using: %s",
			strings.Join(unreadable, ", "))
	}
	return usage, nil
}

// mapState turns Docker's vocabulary into the three states the reconciler
// reasons about.
func mapState(state string) reconcile.RunnerState {
	switch state {
	case "running", "created":
		return reconcile.StateRunning
	case "restarting":
		// Only reachable for a container from before this daemon stopped using
		// a restart policy. Treated as running so it is not rebuilt underneath
		// itself; the next generation change replaces it.
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

// available reports whether there is a Docker to talk to at all. Creating a
// container still fails loudly when there is not — a pool asking for one has
// to hear about it — but listing stays quiet.
func (e *Executor) available() bool {
	if e.socket == "" {
		return true // a test server, or a Docker reached some other way
	}
	_, err := os.Stat(e.socket)
	return err == nil
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
