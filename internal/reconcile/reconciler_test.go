package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/model"
)

// fakeExecutor stands in for systemd and Docker. It records what it was asked
// to do and keeps a plausible amount of state, so a test can run several
// passes and watch a fleet converge.
type fakeExecutor struct {
	runtime model.Runtime
	recipe  string
	// onList runs when the host is read, which is where a pass spends its time
	// and therefore where two passes would be caught overlapping.
	onList  func()
	runners map[string]*Runner
	calls   []string
	started []Spec
	created []Spec
	failOn  map[string]error
}

func newFakeExecutor(runtime model.Runtime) *fakeExecutor {
	return &fakeExecutor{runtime: runtime, runners: map[string]*Runner{}, failOn: map[string]error{}}
}

func (f *fakeExecutor) Runtime() model.Runtime { return f.runtime }

// Recipe is how this executor would build a runner. The fake carries one so a
// test can change it, which is how "the daemon builds runners differently now"
// is expressed.
func (f *fakeExecutor) Recipe(model.Pool) string {
	if f.recipe == "" {
		return "image"
	}
	return f.recipe
}

func (f *fakeExecutor) List(ctx context.Context) ([]Runner, error) {
	if f.onList != nil {
		f.onList()
	}
	if err := f.failOn["list"]; err != nil {
		return nil, err
	}
	out := make([]Runner, 0, len(f.runners))
	for _, r := range f.runners {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeExecutor) Create(ctx context.Context, spec Spec) error {
	f.calls = append(f.calls, "create "+spec.Name)
	f.created = append(f.created, spec)
	if err := f.failOn["create "+spec.Name]; err != nil {
		return err
	}
	f.runners[spec.Name] = &Runner{
		Name: spec.Name, Pool: spec.Pool, Generation: spec.Generation,
		Runtime: f.runtime, State: StateRunning,
	}
	return nil
}

func (f *fakeExecutor) Start(ctx context.Context, spec Spec) error {
	f.calls = append(f.calls, "start "+spec.Name)
	if r, ok := f.runners[spec.Name]; ok {
		r.State = StateRunning
	}
	f.started = append(f.started, spec)
	return nil
}

// Drain returns immediately and leaves the runner stopping, the way a real
// graceful stop behaves: the job in flight decides when it is over.
func (f *fakeExecutor) Drain(ctx context.Context, name string) error {
	f.calls = append(f.calls, "drain "+name)
	if r, ok := f.runners[name]; ok {
		r.State = StateStopping
	}
	return nil
}

func (f *fakeExecutor) Remove(ctx context.Context, name string) error {
	f.calls = append(f.calls, "remove "+name)
	if err := f.failOn["remove "+name]; err != nil {
		return err
	}
	delete(f.runners, name)
	return nil
}

// finishDraining is the job ending: what a real host does on its own.
func (f *fakeExecutor) finishDraining() {
	for _, r := range f.runners {
		if r.State == StateStopping {
			r.State = StateStopped
		}
	}
}

type fakeStore struct {
	pools       []model.Pool
	fingerprint string
	tokenErr    error
	samples     []model.Sample
}

func (f *fakeStore) RecordSamples(ctx context.Context, at time.Time, samples []model.Sample) error {
	f.samples = append(f.samples, samples...)
	return nil
}

func (f *fakeStore) ListPools(ctx context.Context) ([]model.Pool, error) { return f.pools, nil }
func (f *fakeStore) CredentialFingerprint(ctx context.Context, id int64) (string, error) {
	if f.tokenErr != nil {
		return "", f.tokenErr
	}
	if f.fingerprint == "" {
		return "fp", nil
	}
	return f.fingerprint, nil
}
func (f *fakeStore) Secret(ctx context.Context, id int64) (model.Secret, error) {
	if f.tokenErr != nil {
		return model.Secret{}, f.tokenErr
	}
	return model.Secret{Kind: model.CredentialPAT, Token: fmt.Sprintf("token-%d", id)}, nil
}

type fakeGitHub struct {
	states       map[string]github.State
	deregistered []string
	scopeCalls   int
	minted       int
	err          error
}

func (f *fakeGitHub) States(ctx context.Context, scope github.Scope) (map[string]github.State, error) {
	f.scopeCalls++
	if f.err != nil {
		return nil, f.err
	}
	return f.states, nil
}

func (f *fakeGitHub) Deregister(ctx context.Context, scope github.Scope, name string) error {
	f.deregistered = append(f.deregistered, name)
	return nil
}

func (f *fakeGitHub) RegistrationToken(ctx context.Context, scope github.Scope) (string, error) {
	f.minted++
	return fmt.Sprintf("AAAA-registration-%d", f.minted), nil
}

// testPool is a fixed-size pool: minimum equal to maximum, so these tests are
// about the reconciler rather than about the autoscaler.
func testPool(name string, replicas int) model.Pool {
	p := model.Pool{
		ID: 1, Name: name, ScopeKind: model.ScopeRepository, Scope: "o/" + name,
		Runtime: model.RuntimeVM, MinReplicas: replicas, MaxReplicas: replicas,
		CredentialID: 1, Enabled: true,
	}
	p.Defaults()
	return p
}

type harness struct {
	store   *fakeStore
	vm      *fakeExecutor
	docker  *fakeExecutor
	gh      *fakeGitHub
	secrets map[int64]string
	rec     *Reconciler
}

func newHarness(pools ...model.Pool) *harness {
	h := &harness{
		store:   &fakeStore{pools: pools},
		vm:      newFakeExecutor(model.RuntimeVM),
		docker:  newFakeExecutor(model.RuntimeContainer),
		gh:      &fakeGitHub{states: map[string]github.State{}},
		secrets: map[int64]string{},
	}
	h.rec = New(h.store, []Executor{h.vm, h.docker},
		func(model.Secret) (GitHubClient, error) { return h.gh, nil },
		func(id int64, token string) error { h.secrets[id] = token; return nil },
		discardLogger())
	return h
}

func TestReconcileCreatesAndThenSettles(t *testing.T) {
	h := newHarness(testPool("web", 3))
	ctx := context.Background()

	result := h.rec.Once(ctx)
	if len(result.Errors) != 0 {
		t.Fatalf("errors: %v", result.Errors)
	}
	if got := strings.Join(h.vm.calls, "; "); got != "create web-1; create web-2; create web-3" {
		t.Fatalf("got %q", got)
	}

	// The second pass must do nothing at all. A reconciler that keeps acting
	// on a fleet that is already right would restart runners for ever.
	h.vm.calls = nil
	result = h.rec.Once(ctx)
	if len(h.vm.calls) != 0 {
		t.Fatalf("a settled fleet was touched again: %v", h.vm.calls)
	}
	if len(result.Actions) != 0 {
		t.Fatalf("actions on a settled fleet: %+v", result.Actions)
	}
}

// The property the architecture exists for: the daemon can be replaced without
// the runners noticing.
func TestANewDaemonAdoptsTheRunningFleet(t *testing.T) {
	pool := testPool("web", 2)
	h := newHarness(pool)
	ctx := context.Background()
	h.rec.Once(ctx)

	// A new reconciler over the same host and the same store — a restarted or
	// upgraded daemon.
	fresh := New(h.store, []Executor{h.vm, h.docker},
		func(model.Secret) (GitHubClient, error) { return h.gh, nil }, nil, discardLogger())

	h.vm.calls = nil
	result := fresh.Once(ctx)
	if len(h.vm.calls) != 0 {
		t.Fatalf("a restarted daemon disturbed the fleet: %v", h.vm.calls)
	}
	if len(result.Actions) != 0 {
		t.Fatalf("a restarted daemon planned %+v", result.Actions)
	}
}

func TestScaleUpAndDown(t *testing.T) {
	pool := testPool("web", 1)
	h := newHarness(pool)
	ctx := context.Background()
	h.rec.Once(ctx)

	pool.MinReplicas, pool.MaxReplicas = 3, 3
	h.store.pools = []model.Pool{pool}
	h.vm.calls = nil
	h.rec.Once(ctx)
	if got := strings.Join(h.vm.calls, "; "); got != "create web-2; create web-3" {
		t.Fatalf("scaling up did %q", got)
	}

	pool.MinReplicas, pool.MaxReplicas = 1, 1
	h.store.pools = []model.Pool{pool}
	h.vm.calls = nil
	h.rec.Once(ctx)
	if got := strings.Join(h.vm.calls, "; "); got != "drain web-2; drain web-3" {
		t.Fatalf("scaling down did %q, and it must drain rather than remove", got)
	}

	// Once the jobs are over the host reports them stopped, and only then are
	// they removed.
	h.vm.finishDraining()
	h.vm.calls = nil
	h.rec.Once(ctx)
	if got := strings.Join(h.vm.calls, "; "); got != "remove web-2; remove web-3" {
		t.Fatalf("after draining it did %q", got)
	}
	if strings.Join(h.gh.deregistered, ",") != "web-2,web-3" {
		t.Fatalf("removed runners were left registered on GitHub: %v", h.gh.deregistered)
	}
}

func TestReconfiguringAPoolReplacesRunnersGracefully(t *testing.T) {
	pool := testPool("web", 2)
	h := newHarness(pool)
	ctx := context.Background()
	h.rec.Once(ctx)

	// A label change is a new generation: the runners are registered with the
	// wrong labels and have to be rebuilt.
	pool.Labels = []string{"gpu"}
	h.store.pools = []model.Pool{pool}
	h.vm.calls = nil
	h.rec.Once(ctx)
	if got := strings.Join(h.vm.calls, "; "); got != "drain web-1; drain web-2" {
		t.Fatalf("a reconfigured pool did %q, and it must not kill a job to apply a label", got)
	}

	h.vm.finishDraining()
	h.vm.calls = nil
	h.rec.Once(ctx)
	if got := strings.Join(h.vm.calls, "; "); got != "remove web-1; create web-1; remove web-2; create web-2" {
		t.Fatalf("the replacement did %q", got)
	}

	// And the rebuilt runners carry the new configuration.
	for _, r := range h.vm.runners {
		if r.Generation != pool.Generation("fp", "image") {
			t.Fatalf("%s came back on the old generation", r.Name)
		}
	}
}

func TestABusyRunnerIsNeverRemoved(t *testing.T) {
	pool := testPool("web", 2)
	h := newHarness(pool)
	ctx := context.Background()
	h.rec.Once(ctx)

	// Scale to nothing while a job is running on web-1.
	pool.MinReplicas, pool.MaxReplicas = 0, 0
	h.store.pools = []model.Pool{pool}
	h.gh.states["web-1"] = github.StateBusy
	h.gh.states["web-2"] = github.StateIdle

	h.rec.Once(ctx)
	h.vm.finishDraining() // both units have now stopped as far as the host knows
	h.vm.calls = nil
	h.rec.Once(ctx)

	if got := strings.Join(h.vm.calls, "; "); got != "remove web-2" {
		t.Fatalf("got %q: the busy runner must be left alone even once its unit reports stopped", got)
	}
	if _, stillThere := h.vm.runners["web-1"]; !stillThere {
		t.Fatal("the runner with a job on it was removed")
	}
}

func TestDisablingAPoolDrainsItWithoutLosingIt(t *testing.T) {
	pool := testPool("web", 2)
	h := newHarness(pool)
	ctx := context.Background()
	h.rec.Once(ctx)

	pool.Enabled = false
	h.store.pools = []model.Pool{pool}
	h.vm.calls = nil
	h.rec.Once(ctx)
	if got := strings.Join(h.vm.calls, "; "); got != "drain web-1; drain web-2" {
		t.Fatalf("got %q", got)
	}

	// Turning it back on brings the fleet back.
	h.vm.finishDraining()
	h.rec.Once(ctx) // removes the drained ones
	pool.Enabled = true
	h.store.pools = []model.Pool{pool}
	h.vm.calls = nil
	h.rec.Once(ctx)
	if got := strings.Join(h.vm.calls, "; "); got != "create web-1; create web-2" {
		t.Fatalf("got %q", got)
	}
}

func TestARunnerThatDiedIsStartedAgain(t *testing.T) {
	h := newHarness(testPool("web", 1))
	ctx := context.Background()
	h.rec.Once(ctx)

	h.vm.runners["web-1"].State = StateStopped
	h.vm.calls = nil
	h.rec.Once(ctx)
	if got := strings.Join(h.vm.calls, "; "); got != "start web-1" {
		t.Fatalf("got %q", got)
	}
}

func TestPoolsAreDispatchedToTheirRuntime(t *testing.T) {
	vmPool := testPool("web", 1)
	containerPool := testPool("api", 2)
	containerPool.ID = 2
	containerPool.Runtime = model.RuntimeContainer

	h := newHarness(vmPool, containerPool)
	h.rec.Once(context.Background())

	if got := strings.Join(h.vm.calls, "; "); got != "create web-1" {
		t.Fatalf("the vm executor did %q", got)
	}
	if got := strings.Join(h.docker.calls, "; "); got != "create api-1; create api-2" {
		t.Fatalf("the docker executor did %q", got)
	}
}

// One pool with a broken credential must not stop the rest of the host being
// maintained.
func TestOnePoolFailingDoesNotStopTheOthers(t *testing.T) {
	good := testPool("web", 1)
	h := newHarness(good)

	failing := &fakeStore{pools: []model.Pool{good}, tokenErr: errors.New("credential 9: not found")}
	rec := New(&splitStore{good: h.store, bad: failing, badPool: "api"},
		[]Executor{h.vm}, func(model.Secret) (GitHubClient, error) { return h.gh, nil }, nil, discardLogger())

	result := rec.Once(context.Background())
	if len(result.Errors) == 0 {
		t.Fatal("the broken pool was not reported")
	}
	if got := strings.Join(h.vm.calls, "; "); got != "create web-1" {
		t.Fatalf("the healthy pool was not maintained: %q", got)
	}
}

// splitStore fails for one named pool and works for the rest.
type splitStore struct {
	good    Fleet
	bad     Fleet
	badPool string
}

func (s *splitStore) ListPools(ctx context.Context) ([]model.Pool, error) {
	pools, err := s.good.ListPools(ctx)
	if err != nil {
		return nil, err
	}
	broken := testPool(s.badPool, 1)
	broken.ID = 99
	broken.CredentialID = 99
	return append(pools, broken), nil
}

func (s *splitStore) CredentialFingerprint(ctx context.Context, id int64) (string, error) {
	if id == 99 {
		return "", errors.New("credential 99: not found")
	}
	return s.good.CredentialFingerprint(ctx, id)
}

func (s *splitStore) Secret(ctx context.Context, id int64) (model.Secret, error) {
	if id == 99 {
		return model.Secret{}, errors.New("credential 99: not found")
	}
	return s.good.Secret(ctx, id)
}

func (s *splitStore) RecordSamples(ctx context.Context, at time.Time, samples []model.Sample) error {
	return s.good.RecordSamples(ctx, at, samples)
}

// Losing GitHub must not make the daemon destructive: without an answer about
// what is busy, it still only removes what the host says has stopped.
func TestGitHubBeingUnreachableIsNotFatal(t *testing.T) {
	h := newHarness(testPool("web", 1))
	h.gh.err = errors.New("dial tcp: no route to host")

	result := h.rec.Once(context.Background())
	if len(result.Errors) == 0 {
		t.Fatal("the failure was swallowed")
	}
	if got := strings.Join(h.vm.calls, "; "); got != "create web-1" {
		t.Fatalf("the fleet was not maintained: %q", got)
	}
}

func TestOneGitHubCallPerScope(t *testing.T) {
	pool := testPool("web", 5)
	h := newHarness(pool)
	h.rec.Once(context.Background())
	if h.gh.scopeCalls != 1 {
		t.Fatalf("asked GitHub %d times for one repository", h.gh.scopeCalls)
	}
}

func TestCredentialsAreHandedToTheRunners(t *testing.T) {
	h := newHarness(testPool("web", 1))
	h.rec.Once(context.Background())
	if h.secrets[1] != "token-1" {
		t.Fatalf("the runners were not given a credential: %v", h.secrets)
	}
}

func TestAFailedActionIsReportedAndTheRestContinue(t *testing.T) {
	h := newHarness(testPool("web", 3))
	h.vm.failOn["create web-2"] = errors.New("no space left on device")

	result := h.rec.Once(context.Background())
	if len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "no space left") {
		t.Fatalf("errors: %v", result.Errors)
	}
	if _, ok := h.vm.runners["web-3"]; !ok {
		t.Fatal("one failure stopped the rest of the pass")
	}
}

func TestStatusReportsBothViews(t *testing.T) {
	pool := testPool("web", 2)
	h := newHarness(pool)
	ctx := context.Background()
	h.rec.Once(ctx)
	h.gh.states["web-1"] = github.StateBusy

	status, errs := h.rec.Status(ctx)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(status) != 2 {
		t.Fatalf("got %d runners", len(status))
	}
	if status[0].Name != "web-1" || status[0].State != StateRunning || status[0].Job != "busy" {
		t.Fatalf("got %+v", status[0])
	}
	// A runner GitHub has never mentioned is unknown, not idle: those are
	// different facts and the UI shows them differently.
	if status[1].Job != "unknown" {
		t.Fatalf("got %q for a runner GitHub did not mention", status[1].Job)
	}
	if !status[0].UpToDate {
		t.Fatal("a freshly created runner is not up to date")
	}
}

func TestStatusFlagsRunnersOnAnOldConfiguration(t *testing.T) {
	pool := testPool("web", 1)
	h := newHarness(pool)
	ctx := context.Background()
	h.rec.Once(ctx)

	pool.Labels = []string{"gpu"}
	h.store.pools = []model.Pool{pool}

	status, _ := h.rec.Status(ctx)
	if status[0].UpToDate {
		t.Fatal("a runner on the previous configuration reports as up to date")
	}
}

// discardLogger keeps the test output about the tests rather than about the
// fleet.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// A pass reads the host and then acts on what it read, so two at once act on
// the same reading twice. On a real fleet that looked like this, inside one
// second:
//
//	op=remove runner=ci-container-1  reason="configuration changed"
//	op=create runner=ci-container-1  reason="replacing the previous configuration"
//	op=remove runner=ci-container-1  reason="configuration changed"
//	problem="create ci-container-1: docker: 409: Conflict. The container name
//	         \"/ci-container-1\" is already in use"
//
// The daemon's loop is not the only caller: the UI reconciles when somebody
// presses refresh, and saving a pool asks for one too.
func TestOnlyOnePassRunsAtATime(t *testing.T) {
	h := newHarness(testPool("web", 3))

	var (
		mu      sync.Mutex
		inside  int
		overlap int
	)
	// The executor is the slow part of a real pass — Docker, systemctl — so it
	// is where an overlap would show.
	h.vm.onList = func() {
		mu.Lock()
		inside++
		if inside > 1 {
			overlap++
		}
		mu.Unlock()

		time.Sleep(2 * time.Millisecond)

		mu.Lock()
		inside--
		mu.Unlock()
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.rec.Once(context.Background())
		}()
	}
	wg.Wait()

	if overlap != 0 {
		t.Fatalf("%d passes ran while another was running; each acts on a host the other"+
			" is changing underneath it", overlap)
	}
}

// A machine takes a minute or two to boot and register, and an ephemeral one
// does that after every job, so for a busy pool "GitHub has never heard of
// this runner" is most of what anybody sees. Reporting it as unknown made a
// working fleet look broken.
func TestARunnerOnItsWayUpIsNotReportedAsUnknown(t *testing.T) {
	for _, tt := range []struct {
		name   string
		runner Runner
		want   string
	}{
		{
			"just booted",
			Runner{Name: "web-1", State: StateRunning, Up: 20 * time.Second},
			"starting",
		},
		{
			"up long enough that GitHub should know it",
			Runner{Name: "web-1", State: StateRunning, Up: 30 * time.Minute},
			"unknown",
		},
		{
			// Nothing is starting: it is not running at all.
			"stopped",
			Runner{Name: "web-1", State: StateStopped, Up: 0},
			"unknown",
		},
		{
			// A host that cannot say how long it has been up says nothing, and
			// the answer stays what it was.
			"no uptime from the host",
			Runner{Name: "web-1", State: StateRunning},
			"unknown",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := jobOfARunnerGitHubHasNotSeen(tt.runner); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
