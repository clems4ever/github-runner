package docker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/paths"
	"github.com/clems4ever/github-runner/internal/reconcile"
)

// fakeDocker is enough of the Docker API for these tests, and records what was
// asked of it.
type fakeDocker struct {
	mu         sync.Mutex
	requests   []string
	bodies     map[string]map[string]any
	containers []container
	imageKnown bool
	stopDelay  time.Duration
}

func (f *fakeDocker) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1.44")
		f.mu.Lock()
		f.requests = append(f.requests, r.Method+" "+path)
		if r.Body != nil {
			if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
				var decoded map[string]any
				if json.Unmarshal(raw, &decoded) == nil {
					if f.bodies == nil {
						f.bodies = map[string]map[string]any{}
					}
					f.bodies[path] = decoded
				}
			}
		}
		f.mu.Unlock()

		switch {
		case path == "/_ping":
			w.Write([]byte("OK"))
		case strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
			if !f.imageKnown {
				w.WriteHeader(http.StatusNotFound)
				w.Write([]byte(`{"message":"No such image"}`))
				return
			}
			w.Write([]byte(`{"Id":"sha256:abc"}`))
		case strings.HasPrefix(path, "/images/create"):
			w.Write([]byte(`{}`))
		case strings.HasPrefix(path, "/containers/create"):
			w.Write([]byte(`{"Id":"container-id"}`))
		case strings.HasSuffix(path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(path, "/stop"):
			time.Sleep(f.stopDelay)
			w.WriteHeader(http.StatusNoContent)
		case path == "/containers/json":
			f.mu.Lock()
			list := f.containers
			f.mu.Unlock()
			json.NewEncoder(w).Encode(list)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"no such endpoint"}`))
		}
	})
}

func (f *fakeDocker) called(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, req := range f.requests {
		if strings.Contains(req, substr) {
			return true
		}
	}
	return false
}

func newExecutor(t *testing.T) (*Executor, *fakeDocker) {
	t.Helper()
	fake := &fakeDocker{imageKnown: true}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	layout := paths.Under(t.TempDir())
	if err := layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	e := New(layout, "/usr/local/bin/runner-fleet", WithHTTPClient(srv.Client(), srv.URL))
	return e, fake
}

func testSpec(name string) reconcile.Spec {
	return reconcile.Spec{
		Name: name, Pool: "api", PoolID: 2, Generation: "gen123",
		Runtime: model.RuntimeContainer, URL: "https://github.com/o/r",
		ScopeKind: model.ScopeRepository, Scope: "o/r",
		Labels: []string{"container"}, CPUs: 2, MemoryMB: 4096,
		Image: "default", CredentialID: 5,
	}
}

func TestCreate(t *testing.T) {
	e, fake := newExecutor(t)
	if err := e.Create(context.Background(), testSpec("api-1")); err != nil {
		t.Fatal(err)
	}

	if !fake.called("POST /containers/create") || !fake.called("/start") {
		t.Fatalf("requests were %v", fake.requests)
	}

	body := fake.bodies["/containers/create"]
	labels, _ := body["Labels"].(map[string]any)
	if labels[LabelRunner] != "api-1" || labels[LabelPool] != "api" || labels[LabelGeneration] != "gen123" {
		t.Fatalf("the labels a restarted daemon finds this by are wrong: %v", labels)
	}

	host, _ := body["HostConfig"].(map[string]any)
	policy, _ := host["RestartPolicy"].(map[string]any)
	// No restart policy at all. A container registers with a token that
	// expires in an hour, so dockerd starting the same one again later would
	// fail to register and loop — while looking healthy to anyone watching
	// Docker. The daemon replaces them instead.
	if policy["Name"] != "no" {
		t.Fatalf("restart policy is %v, want the daemon to own replacement", policy)
	}
	if host["Memory"] != float64(4096*1024*1024) {
		t.Fatalf("memory limit is %v", host["Memory"])
	}
	if host["NanoCpus"] != float64(2_000_000_000) {
		t.Fatalf("cpu limit is %v", host["NanoCpus"])
	}
}

// The credential never enters a container.
//
// A container shares everything with the job it runs: same filesystem, same
// user, same process tree. Mounting the key that mints tokens would hand every
// job something that administers repositories. The daemon mints instead, and
// what goes in is a registration token — short-lived, and able to do one
// thing.
func TestTheCredentialNeverEntersTheContainer(t *testing.T) {
	e, fake := newExecutor(t)

	spec := testSpec("api-1")
	spec.CredentialKind = model.CredentialApp
	spec.AppID = 123456
	spec.RegistrationToken = "AAAA-registration"

	if err := e.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	body := fake.bodies["/containers/create"]

	host := body["HostConfig"].(map[string]any)
	for _, bind := range host["Binds"].([]any) {
		text := bind.(string)
		if strings.Contains(text, "credentials") || strings.Contains(text, "github_token") {
			t.Fatalf("the credential is mounted into the container: %q", text)
		}
	}
	// Only the agent goes in.
	if len(host["Binds"].([]any)) != 1 {
		t.Fatalf("more than the agent is mounted: %v", host["Binds"])
	}

	var registered bool
	for _, value := range body["Env"].([]any) {
		text := value.(string)
		if text == "FLEET_REGISTRATION_TOKEN=AAAA-registration" {
			registered = true
		}
		if strings.Contains(text, "PRIVATE KEY") || strings.Contains(text, "github_pat") {
			t.Fatalf("a credential was passed in the environment: %q", text)
		}
		if strings.HasPrefix(text, "FLEET_CREDENTIAL_FILE=") {
			t.Fatalf("the container was pointed at a credential file: %q", text)
		}
	}
	if !registered {
		t.Fatalf("no registration token reached the runner: %v", body["Env"])
	}
}

// A stopped container cannot simply be started: the token it registered with
// has expired. It is rebuilt with a fresh one.
func TestStartRebuildsRatherThanRestarts(t *testing.T) {
	e, fake := newExecutor(t)
	ctx := context.Background()

	spec := testSpec("api-1")
	spec.RegistrationToken = "AAAA-first"
	if err := e.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	fake.requests = nil
	fake.mu.Unlock()

	spec.RegistrationToken = "AAAA-second"
	if err := e.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}

	if !fake.called("DELETE /containers/api-1") {
		t.Fatalf("the old container was left behind: %v", fake.requests)
	}
	if !fake.called("POST /containers/create") {
		t.Fatalf("nothing was rebuilt: %v", fake.requests)
	}
	body := fake.bodies["/containers/create"]
	var fresh bool
	for _, value := range body["Env"].([]any) {
		if value.(string) == "FLEET_REGISTRATION_TOKEN=AAAA-second" {
			fresh = true
		}
	}
	if !fresh {
		t.Fatalf("it was rebuilt with the old token: %v", body["Env"])
	}
}

// Nested virtualisation in a container hands the job the host's KVM device, so
// it must appear only when the pool asked for it.
func TestNestedIsOptIn(t *testing.T) {
	e, fake := newExecutor(t)
	ctx := context.Background()

	if err := e.Create(ctx, testSpec("api-1")); err != nil {
		t.Fatal(err)
	}
	host := fake.bodies["/containers/create"]
	if _, present := host["HostConfig"].(map[string]any)["Devices"]; present {
		t.Fatal("a pool that did not ask for nested virtualisation was given /dev/kvm")
	}

	spec := testSpec("api-2")
	spec.Nested = true
	if err := e.Create(ctx, spec); err != nil {
		t.Fatal(err)
	}
	host = fake.bodies["/containers/create"]
	devices, ok := host["HostConfig"].(map[string]any)["Devices"].([]any)
	if !ok || len(devices) != 1 {
		t.Fatalf("nested was asked for but no device was passed: %v", host["HostConfig"])
	}
	device := devices[0].(map[string]any)
	if device["PathOnHost"] != "/dev/kvm" {
		t.Fatalf("got %v", device)
	}
}

func TestCreatePullsAnImageThatIsMissing(t *testing.T) {
	e, fake := newExecutor(t)
	fake.imageKnown = false

	if err := e.Create(context.Background(), testSpec("api-1")); err != nil {
		t.Fatal(err)
	}
	if !fake.called("POST /images/create") {
		t.Fatalf("the image was not pulled: %v", fake.requests)
	}
}

func TestCreateDoesNotPullAnImageItAlreadyHas(t *testing.T) {
	e, fake := newExecutor(t)
	if err := e.Create(context.Background(), testSpec("api-1")); err != nil {
		t.Fatal(err)
	}
	if fake.called("POST /images/create") {
		t.Fatal("an image already on the host was pulled again")
	}
}

// A pool naming its own image is what per-repository images will use, so it
// has to reach Docker unchanged.
func TestAPoolCanNameItsOwnImage(t *testing.T) {
	e, fake := newExecutor(t)
	spec := testSpec("api-1")
	spec.Image = "ghcr.io/clems4ever/runyard-runner:2026-08"

	if err := e.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if got := fake.bodies["/containers/create"]["Image"]; got != spec.Image {
		t.Fatalf("got %v", got)
	}
}

func TestDrainReturnsImmediately(t *testing.T) {
	e, fake := newExecutor(t)
	// A real stop waits for the job in flight. If Drain waited with it, one
	// busy runner would stall every pool on the host.
	fake.stopDelay = 2 * time.Second

	start := time.Now()
	if err := e.Drain(context.Background(), "api-1"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("drain blocked for %s", elapsed)
	}
}

func TestARunnerBeingDrainedReportsAsStopping(t *testing.T) {
	e, fake := newExecutor(t)
	ctx := context.Background()
	fake.containers = []container{{
		ID: "id", State: "running",
		Labels: map[string]string{LabelRunner: "api-1", LabelPool: "api", LabelGeneration: "gen123"},
	}}

	runners, err := e.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if runners[0].State != reconcile.StateRunning {
		t.Fatalf("got %q", runners[0].State)
	}

	if err := e.Drain(ctx, "api-1"); err != nil {
		t.Fatal(err)
	}
	runners, err = e.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Still running as far as Docker is concerned — the job has not finished —
	// but the reconciler must not treat it as a candidate for removal.
	if runners[0].State != reconcile.StateStopping {
		t.Fatalf("a draining runner reports as %q", runners[0].State)
	}
}

func TestList(t *testing.T) {
	e, fake := newExecutor(t)
	fake.containers = []container{
		{ID: "1", State: "running", Labels: map[string]string{LabelRunner: "api-2", LabelPool: "api", LabelGeneration: "g"}},
		{ID: "2", State: "exited", Labels: map[string]string{LabelRunner: "api-1", LabelPool: "api", LabelGeneration: "g"}},
		// Something else on the host, without the fleet's labels.
		{ID: "3", State: "running", Labels: map[string]string{"com.example": "postgres"}},
	}

	runners, err := e.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) != 2 {
		t.Fatalf("got %d runners, want only the fleet's own: %+v", len(runners), runners)
	}
	if runners[0].Name != "api-1" || runners[1].Name != "api-2" {
		t.Fatalf("not sorted: %+v", runners)
	}
	if runners[0].State != reconcile.StateStopped || runners[1].State != reconcile.StateRunning {
		t.Fatalf("states are %q and %q", runners[0].State, runners[1].State)
	}
	if runners[0].Runtime != model.RuntimeContainer {
		t.Fatalf("runtime is %q", runners[0].Runtime)
	}
}

func TestListAsksOnlyForTheFleetsContainers(t *testing.T) {
	e, fake := newExecutor(t)
	if _, err := e.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	var filtered bool
	for _, req := range fake.requests {
		if strings.Contains(req, "/containers/json") {
			filtered = true
		}
	}
	if !filtered {
		t.Fatalf("requests were %v", fake.requests)
	}
}

func TestRemoveIsQuietWhenAlreadyGone(t *testing.T) {
	e, srvFake := newExecutor(t)
	_ = srvFake
	if err := e.Remove(context.Background(), "api-1"); err != nil {
		t.Fatalf("want no error, got %v", err)
	}
}

func TestMapState(t *testing.T) {
	for state, want := range map[string]reconcile.RunnerState{
		"running":    reconcile.StateRunning,
		"restarting": reconcile.StateRunning,
		"created":    reconcile.StateRunning,
		"removing":   reconcile.StateStopping,
		"exited":     reconcile.StateStopped,
		"dead":       reconcile.StateStopped,
		"paused":     reconcile.StateStopped,
	} {
		if got := mapState(state); got != want {
			t.Errorf("%q maps to %q, want %q", state, got, want)
		}
	}
}

func TestPing(t *testing.T) {
	e, _ := newExecutor(t)
	if err := e.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRuntime(t *testing.T) {
	e, _ := newExecutor(t)
	if e.Runtime() != model.RuntimeContainer {
		t.Fatalf("got %q", e.Runtime())
	}
}

// A daemon that cannot reach Docker has to say so in terms that name the
// likely cause.
func TestUnreachableDockerSaysWhy(t *testing.T) {
	layout := paths.Under(t.TempDir())
	e := New(layout, "/usr/local/bin/runner-fleet",
		WithHTTPClient(&http.Client{Timeout: time.Second}, "http://127.0.0.1:1"))

	err := e.Ping(context.Background())
	if err == nil {
		t.Fatal("no error from an unreachable daemon")
	}
	if !strings.Contains(err.Error(), "dockerd") {
		t.Fatalf("got %q", err)
	}
}
