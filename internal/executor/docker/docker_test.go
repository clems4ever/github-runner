package docker

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/containerimage"
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
	// stats is the statistics document per container name. A name that is not
	// in here is a container Docker refuses to talk about.
	stats map[string]string
}

func (f *fakeDocker) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1.44")
		// The query is recorded too: some of what the daemon asks for — the
		// label filter on a listing, one-shot statistics — is in it, and a test
		// that could not see it could not tell that it had been dropped.
		recorded := r.Method + " " + path
		if r.URL.RawQuery != "" {
			recorded += "?" + r.URL.RawQuery
		}
		f.mu.Lock()
		f.requests = append(f.requests, recorded)
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
		case strings.HasSuffix(path, "/stats"):
			name := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/stats")
			f.mu.Lock()
			document, known := f.stats[name]
			f.mu.Unlock()
			if !known {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(`{"message":"cannot read cgroup"}`))
				return
			}
			w.Write([]byte(document))
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
	if err := layout.EnsureDirs(paths.CurrentOwner()); err != nil {
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
	// Something has to reap. PID 1 in here would otherwise be the agent, and
	// PID 1 is where a job's orphans land: a zombie keeps its pid, answers
	// `kill(pid, 0)` and keeps its start time in /proc, so a job's own "is it
	// still running" check reads a process that exited as one that is alive.
	// A machine runner has systemd and never shows this, which is the part
	// that makes it worth a test — the bug is a difference BETWEEN runtimes.
	if host["Init"] != true {
		t.Fatalf("no init process: a job's orphans would never be reaped, and a "+
			"zombie answers every check a job makes about whether it is still "+
			"running (HostConfig was %v)", host)
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

// The memory figure is deliberately not Docker's "usage": that includes the
// page cache, so a container that has cloned a large repository reports most of
// its limit used and looks about to die. Docker's own CLI subtracts the
// inactive file cache before printing, and so does this.
func TestUsageSubtractsThePageCacheFromMemory(t *testing.T) {
	e, fake := newExecutor(t)
	fake.containers = []container{{
		ID: "1", State: "running",
		Labels: map[string]string{LabelRunner: "api-1", LabelPool: "api", LabelGeneration: "g"},
	}}
	fake.stats = map[string]string{
		"api-1": `{"cpu_stats":{"cpu_usage":{"total_usage":1000000000}},
			"memory_stats":{"usage":1073741824,"stats":{"inactive_file":536870912}}}`,
	}

	usage, err := e.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 {
		t.Fatalf("got %v", usage)
	}
	if usage[0].MemoryBytes != 536870912 {
		t.Fatalf("want the cache left out, got %d", usage[0].MemoryBytes)
	}
	if usage[0].Name != "api-1" || usage[0].Pool != "api" || usage[0].Runtime != "container" {
		t.Fatalf("the row does not say which runner this is: %+v", usage[0])
	}
	// One reading, so no rate yet.
	if usage[0].CPUPercent != nil {
		t.Fatalf("want no figure from one reading, got %v", *usage[0].CPUPercent)
	}
}

// Cgroup v1 spells the same number differently, and a host on it must not
// report every container as using its whole page cache.
func TestUsageUnderstandsBothCgroupVersions(t *testing.T) {
	e, fake := newExecutor(t)
	fake.containers = []container{{
		ID: "1", State: "running",
		Labels: map[string]string{LabelRunner: "api-1", LabelPool: "api"},
	}}
	fake.stats = map[string]string{
		"api-1": `{"memory_stats":{"usage":1000,"stats":{"total_inactive_file":400}}}`,
	}

	usage, err := e.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if usage[0].MemoryBytes != 600 {
		t.Fatalf("got %d", usage[0].MemoryBytes)
	}
}

// Asked without one-shot, Docker holds the request open for a second per
// container so it can work out a percentage of its own. Across a fleet that is
// a second of the daemon's attention each, every sample, for a number this
// works out for itself from the counter.
func TestUsageAsksForOneShotStatistics(t *testing.T) {
	e, fake := newExecutor(t)
	fake.containers = []container{{
		ID: "1", State: "running", Labels: map[string]string{LabelRunner: "api-1"},
	}}
	fake.stats = map[string]string{"api-1": `{"memory_stats":{"usage":1}}`}

	if _, err := e.Usage(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !fake.called("one-shot=true") || !fake.called("stream=false") {
		t.Fatalf("requests were %v", fake.requests)
	}
}

// A stopped container has no cgroup to ask about, and a row of zeroes reads as
// a runner doing nothing rather than as a runner that is not there.
func TestUsageLeavesOutContainersThatAreNotRunning(t *testing.T) {
	e, fake := newExecutor(t)
	fake.containers = []container{
		{ID: "1", State: "exited", Labels: map[string]string{LabelRunner: "api-1"}},
		{ID: "2", State: "running", Labels: map[string]string{LabelRunner: "api-2"}},
	}
	fake.stats = map[string]string{"api-2": `{"memory_stats":{"usage":2048}}`}

	usage, err := e.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].Name != "api-2" {
		t.Fatalf("got %+v", usage)
	}
	if fake.called("/containers/api-1/stats") {
		t.Fatal("a stopped container was asked about")
	}
}

// Nine containers answered and the tenth did not. Throwing the nine away would
// leave a page saying a busy host is empty, so the rows come back with an error
// naming what is missing.
func TestUsageKeepsWhatItCouldReadAndNamesWhatItCouldNot(t *testing.T) {
	e, fake := newExecutor(t)
	fake.containers = []container{
		{ID: "1", State: "running", Labels: map[string]string{LabelRunner: "api-1"}},
		{ID: "2", State: "running", Labels: map[string]string{LabelRunner: "api-2"}},
	}
	fake.stats = map[string]string{"api-1": `{"memory_stats":{"usage":4096}}`}

	usage, err := e.Usage(context.Background())
	if err == nil {
		t.Fatal("want the container that could not be read reported")
	}
	if !strings.Contains(err.Error(), "api-2") {
		t.Fatalf("the error should name it: %v", err)
	}
	if len(usage) != 1 || usage[0].Name != "api-1" {
		t.Fatalf("the readable row was thrown away: %+v", usage)
	}
}

// A host without Docker has no container runners, which is a true answer
// rather than an error — the same rule listing follows.
func TestUsageIsQuietWhenThereIsNoDocker(t *testing.T) {
	e, _ := newExecutor(t)
	e.socket = "/nowhere/docker.sock"

	usage, err := e.Usage(context.Background())
	if err != nil || usage != nil {
		t.Fatalf("got %v, %v", usage, err)
	}
}

// A pool without docker in docker gets none of what one needs.
//
// The privileged flag is the whole of the boundary a container pool has left,
// so it is worth a test that says out loud when it has been handed to a pool
// that never asked.
func TestCreateWithoutDindIsNotPrivileged(t *testing.T) {
	e, fake := newExecutor(t)
	if err := e.Create(context.Background(), testSpec("api-1")); err != nil {
		t.Fatal(err)
	}

	body := fake.bodies["/containers/create"]
	host, _ := body["HostConfig"].(map[string]any)
	if _, ok := host["Privileged"]; ok {
		t.Fatalf("a pool that did not ask for docker got a privileged container: %v", host)
	}
	if _, ok := body["Volumes"]; ok {
		t.Fatalf("a pool that did not ask for docker got a daemon's storage: %v", body)
	}
	if body["User"] != nil {
		t.Fatalf("a pool that did not ask for docker was started as %v rather than the image's own user", body["User"])
	}
	for _, entry := range body["Env"].([]any) {
		if entry == "FLEET_DOCKER=dind" {
			t.Fatal("the agent was told to start a daemon in a pool that asked for none")
		}
	}
}

// What a dind pool is made of, in one place.
//
// Each of these is load-bearing and none of them is obvious from the outside:
// without the volume the daemon silently falls back to the vfs storage driver
// and copies every layer of every image; without the private cgroup namespace
// the job's containers land beside the runner's limits rather than inside
// them; without root there is nothing that can start a daemon at all.
func TestCreateWithDind(t *testing.T) {
	e, fake := newExecutor(t)
	spec := testSpec("api-1")
	spec.Docker = model.DockerDind
	if err := e.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	body := fake.bodies["/containers/create"]
	host, _ := body["HostConfig"].(map[string]any)
	if host["Privileged"] != true {
		t.Fatalf("a dind runner cannot start a daemon without it: %v", host)
	}
	if host["CgroupnsMode"] != "private" {
		t.Fatalf("cgroup namespace is %v, so a job's containers would escape the pool's limits", host["CgroupnsMode"])
	}
	if body["User"] != "root" {
		t.Fatalf("user is %v, and nothing else can start a daemon", body["User"])
	}
	volumes, _ := body["Volumes"].(map[string]any)
	if _, ok := volumes["/var/lib/docker"]; !ok {
		t.Fatalf("the daemon has nowhere to keep its images but the container's overlay: %v", volumes)
	}

	var told bool
	for _, entry := range body["Env"].([]any) {
		if entry == "FLEET_DOCKER=dind" {
			told = true
		}
	}
	if !told {
		t.Fatalf("the agent was never told to start a daemon: %v", body["Env"])
	}

	// The limits still apply. They are what contains a job's containers, since
	// those are started inside this one.
	if host["Memory"] != float64(4096*1024*1024) || host["NanoCpus"] != float64(2_000_000_000) {
		t.Fatalf("a dind runner was let out of its pool's size: %v", host)
	}
}

// The daemon's storage goes when the runner does.
//
// An anonymous volume per container is only cheap if it is removed with the
// container; left behind, a fleet of ephemeral runners fills the host with the
// image stores of runners that no longer exist.
func TestRemoveTakesTheDaemonsStorageWithIt(t *testing.T) {
	e, fake := newExecutor(t)
	if err := e.Remove(context.Background(), "api-1"); err != nil {
		t.Fatal(err)
	}
	if !fake.called("DELETE /containers/api-1?v=1") {
		t.Fatalf("the volume was left on the host: %v", fake.requests)
	}
}

// The build context is a tar the classic builder can read: the Dockerfile the
// spec generates, and the recipe beside it.
func TestTheBuildContextCarriesTheDockerfileAndTheRecipe(t *testing.T) {
	var got struct {
		contentType string
		query       url.Values
		files       map[string]string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1.44/build") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		got.contentType = r.Header.Get("Content-Type")
		got.query = r.URL.Query()
		got.files = map[string]string{}
		tr := tar.NewReader(r.Body)
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			body, _ := io.ReadAll(tr)
			got.files[h.Name] = string(body)
		}
		_, _ = io.WriteString(w, `{"stream":"Step 1/3 : FROM base\n"}`+"\n")
	}))
	defer srv.Close()

	e := New(paths.Layout{}, "/agent", WithHTTPClient(srv.Client(), srv.URL))
	spec := containerimage.Spec{Base: "base", Packages: []string{"make"}, Recipe: "echo hello"}
	var journal strings.Builder
	if err := e.BuildImage(context.Background(), spec, &journal); err != nil {
		t.Fatalf("build: %v", err)
	}

	if got.contentType != "application/x-tar" {
		t.Errorf("content type %q: the daemon reads the context as a tar", got.contentType)
	}
	if tag := got.query.Get("t"); tag != spec.Name() {
		t.Errorf("tagged %q, and the pool asks for %q", tag, spec.Name())
	}
	if !strings.Contains(got.files["Dockerfile"], "FROM base") {
		t.Errorf("the context's Dockerfile is %q", got.files["Dockerfile"])
	}
	if !strings.Contains(got.files[containerimage.RecipeFile], "echo hello") {
		t.Errorf("the recipe did not travel with the context: %q", got.files)
	}
	if !strings.Contains(journal.String(), "Step 1/3") {
		t.Errorf("what the builder printed is not in the journal: %q", journal.String())
	}
}

// /build answers 200 and then reports the failure inside the stream, which is
// the one way a build fails that a status code does not say.
func TestABuildThatFailsInsideATwoHundredIsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"stream":"Step 2/3 : RUN false\n"}`+"\n"+
			`{"errorDetail":{"message":"The command '/bin/sh -c false' returned a non-zero code: 1"},`+
			`"error":"The command '/bin/sh -c false' returned a non-zero code: 1"}`+"\n")
	}))
	defer srv.Close()

	e := New(paths.Layout{}, "/agent", WithHTTPClient(srv.Client(), srv.URL))
	var journal strings.Builder
	err := e.BuildImage(context.Background(), containerimage.Spec{Recipe: "false"}, &journal)
	if err == nil {
		t.Fatal("a build that failed was reported as having worked")
	}
	if !strings.Contains(err.Error(), "non-zero code") {
		t.Errorf("the error does not say what the builder said: %v", err)
	}
	if !strings.Contains(journal.String(), "the build failed") {
		t.Errorf("the log does not end with the failure:\n%s", journal.String())
	}
}

// Which image a runner starts from: the one this daemon built when the pool
// bakes something in, and the one the pool named when it does not.
func TestAPoolRunsTheImageItBakesWhenItBakesOne(t *testing.T) {
	plain := model.Pool{Runtime: model.RuntimeContainer, Image: "ghcr.io/me/runner:v3"}
	if got := Image(plain); got != "ghcr.io/me/runner:v3" {
		t.Errorf("a pool that bakes nothing runs %q", got)
	}
	if got := Image(model.Pool{Runtime: model.RuntimeContainer}); got != DefaultImage {
		t.Errorf("a pool that names no image runs %q", got)
	}

	baking := plain
	baking.Packages = []string{"make"}
	want := containerimage.Spec{Base: plain.Image, Packages: baking.Packages}.Name()
	if got := Image(baking); got != want {
		t.Errorf("a pool that bakes something runs %q, and its image is %q", got, want)
	}
}

// A build holds one HTTP response open for as long as the build takes, so the
// client's whole-request timeout is the build's ceiling — and the client this
// executor uses for everything else has a 60-second one, which is right for a
// request that answers immediately and wrong for the only request here that
// does not.
//
// The shape is worth keeping in a test because of how it fails. The build is
// mid-step when the deadline fires, so what surfaces is `context deadline
// exceeded` next to whatever was on screen at the time — a download, an unpack
// — and reads as a problem with the thing being downloaded rather than as a
// limit on the build. A pool whose recipe fetches a browser hits it every time;
// one that installs three apt packages never does.
func TestABuildOutlastsTheClientTimeoutTheOtherCallsUse(t *testing.T) {
	const short = 100 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("the test server cannot stream, so this proves nothing")
			return
		}
		// Longer than `short`, in pieces, the way a builder reports steps.
		for i := 0; i < 5; i++ {
			_, _ = io.WriteString(w, `{"stream":"working\n"}`+"\n")
			flusher.Flush()
			time.Sleep(short / 2)
		}
	}))
	defer srv.Close()

	client := srv.Client()
	client.Timeout = short
	e := New(paths.Layout{}, "/agent", WithHTTPClient(client, srv.URL))

	var journal strings.Builder
	if err := e.BuildImage(context.Background(), containerimage.Spec{Base: "base"}, &journal); err != nil {
		t.Fatalf("a build that streamed for longer than the client's %s timeout failed: %v", short, err)
	}
	if n := strings.Count(journal.String(), "working"); n != 5 {
		t.Errorf("the journal kept %d of the 5 lines the builder streamed", n)
	}
}

// Removing the client's stopwatch does not mean a build runs forever: what
// bounds it is the context on the request, which a caller can also cancel.
func TestACancelledBuildStopsWaiting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the context tar first: the server only learns that the client
		// has gone once it is reading, so a handler that blocks without
		// finishing the request never notices the cancellation and the test
		// hangs instead of failing.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
			t.Error("the client never went away, so the cancellation did not reach the request")
		}
	}))
	defer srv.Close()

	e := New(paths.Layout{}, "/agent", WithHTTPClient(srv.Client(), srv.URL))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	var journal strings.Builder
	err := e.BuildImage(ctx, containerimage.Spec{Base: "base"}, &journal)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled build ended with %v, and the caller asked for %v", err, context.Canceled)
	}
}
