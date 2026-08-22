package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/model"
)

// Executor is one way of running a runner. There is one per runtime, and
// nothing above this interface knows whether a runner is a virtual machine or
// a container.
//
// Every method is expected to return promptly. Drain in particular starts a
// graceful stop and returns; the stop itself can take an hour, because it
// waits for the job in flight, and a reconcile loop that blocked on it would
// stall every other pool on the host.
type Executor interface {
	Runtime() model.Runtime
	// Recipe describes how this executor builds a runner for a pool — the
	// golden image it would boot, the container image it would run. When it
	// changes, the runners built from the old one are no longer what the pool
	// asked for, and the generation says so without anybody having to remember.
	Recipe(pool model.Pool) string
	List(ctx context.Context) ([]Runner, error)
	Create(ctx context.Context, spec Spec) error
	Start(ctx context.Context, spec Spec) error
	Drain(ctx context.Context, name string) error
	Remove(ctx context.Context, name string) error
}

// Fleet is the part of the store the reconciler needs.
type Fleet interface {
	ListPools(ctx context.Context) ([]model.Pool, error)
	CredentialFingerprint(ctx context.Context, id int64) (string, error)
	Secret(ctx context.Context, id int64) (model.Secret, error)
	RecordSamples(ctx context.Context, at time.Time, samples []model.Sample) error
	RecordJobs(ctx context.Context, at time.Time, samples []model.JobSample) error
}

// GitHubClient is the part of GitHub the reconciler needs.
type GitHubClient interface {
	States(ctx context.Context, scope github.Scope) (map[string]github.State, error)
	Deregister(ctx context.Context, scope github.Scope, name string) error
	RegistrationToken(ctx context.Context, scope github.Scope) (string, error)
}

// ClientFactory builds a GitHub client for one credential, whichever kind it
// is. Injected rather than reached for, so the fleet's rules can be tested
// without a network.
type ClientFactory func(secret model.Secret) (GitHubClient, error)

// CredentialWriter puts a decrypted secret where a runner can read it without
// the daemon's help — on tmpfs, so it never reaches a disk.
type CredentialWriter func(id int64, secret string) error

// Reconciler drives the fleet towards what the store asks for.
type Reconciler struct {
	store       Fleet
	executors   map[model.Runtime]Executor
	newClient   ClientFactory
	writeSecret CredentialWriter
	log         *slog.Logger

	// pass serialises whole reconcile passes. The daemon's loop is not the
	// only caller: the UI reconciles when somebody presses refresh, and saving
	// a pool asks for one too. A pass reads the host and then acts on what it
	// read, so two at once both see a runner missing and both create it — a
	// container name conflict, and a fleet where each pass undoes what the
	// other just did.
	pass sync.Mutex

	mu     sync.Mutex
	last   Result
	poolOf map[string]string // runner name -> pool, for reporting
	// busySince records when each pool last had a runner working.
	//
	// It lives in memory on purpose. Losing it — on a restart, or an upgrade —
	// makes the next scale-down wait out the stabilisation window again, which
	// is the harmless direction to be wrong in: a pool stays one size too big
	// for a few minutes rather than shedding a runner it was about to need.
	busySince map[string]time.Time
	scaling   map[string]Scale

	// busyLast is which runners had a job on them at the previous pass, and
	// lastPass is when that pass looked. Between them they are the whole of
	// the job accounting's memory: a runner busy now that was not busy then
	// has picked up a job, and the gap between the two is the time a runner
	// still busy has been working for.
	//
	// In memory like busySince, and lost on a restart for the same reason it
	// is not worth keeping: the cost is that jobs in flight are counted a
	// second time when the daemon comes back, which is a handful of jobs
	// against a tally kept in months.
	busyLast map[string]bool
	lastPass time.Time

	// now is the clock, injectable so the autoscaler's timing can be tested
	// without waiting for it.
	now func() time.Time
}

// lastBusy is when the pool last had a runner with a job on it. A pool that is
// busy right now is busy as of now.
func (r *Reconciler) lastBusy(pool string, runners []Runner, states map[string]github.State) time.Time {
	// Working, or just finished working. An ephemeral runner that is on its way
	// back has by definition just completed a job, and that is as good a proof
	// of demand as catching it mid-job — better, in fact, since a job shorter
	// than the gap between two passes is never seen busy at all.
	busy := false
	for _, runner := range runners {
		if states[runner.Name] == github.StateBusy {
			busy = true
			break
		}
		if _, known := states[runner.Name]; !known &&
			runner.State == StateRunning && runner.Up > 0 && runner.Up < Registering {
			busy = true
			break
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if busy {
		r.busySince[pool] = r.now()
	} else if _, seen := r.busySince[pool]; !seen {
		// First sight of a quiet pool: treat it as busy just now, so a daemon
		// that has only just started does not immediately shrink a fleet whose
		// history it never saw.
		r.busySince[pool] = r.now()
	}
	return r.busySince[pool]
}

func (r *Reconciler) rememberScaling(scaling map[string]Scale) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scaling = scaling
}

// Scaling is the most recent decision per pool, for the UI.
func (r *Reconciler) Scaling() map[string]Scale {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Scale, len(r.scaling))
	for pool, scale := range r.scaling {
		out[pool] = scale
	}
	return out
}

// WithClock replaces the clock, for tests.
func (r *Reconciler) WithClock(now func() time.Time) *Reconciler {
	r.now = now
	return r
}

// New builds a reconciler.
func New(store Fleet, executors []Executor, newClient ClientFactory, writeSecret CredentialWriter, log *slog.Logger) *Reconciler {
	byRuntime := make(map[model.Runtime]Executor, len(executors))
	for _, e := range executors {
		byRuntime[e.Runtime()] = e
	}
	if log == nil {
		log = slog.Default()
	}
	return &Reconciler{
		store:       store,
		executors:   byRuntime,
		newClient:   newClient,
		writeSecret: writeSecret,
		log:         log,
		poolOf:      map[string]string{},
		busySince:   map[string]time.Time{},
		busyLast:    map[string]bool{},
		scaling:     map[string]Scale{},
		now:         time.Now,
	}
}

// Result is what one pass did.
type Result struct {
	Actions []Action         `json:"actions"`
	Errors  []string         `json:"errors"`
	Scaling map[string]Scale `json:"scaling"`
	// ScaledUp says a pool grew because everything in it was busy. The daemon
	// uses it to come back sooner than the next tick: a burst of jobs should
	// ramp in seconds, not at one runner per interval.
	ScaledUp bool `json:"scaledUp"`
}

// Once runs a single reconcile pass.
//
// A failure on one pool is collected rather than returned: a repository whose
// token has expired must not stop the other pools on the host from being
// maintained. The error is surfaced in the result, which the UI shows.
func (r *Reconciler) Once(ctx context.Context) Result {
	// One at a time. Waiting rather than skipping: whoever asked for a pass
	// wants the fleet reconciled after they asked, not before.
	r.pass.Lock()
	defer r.pass.Unlock()

	var result Result

	pools, err := r.store.ListPools(ctx)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("read the pools: %v", err))
		return result
	}

	// The host is read before anything is decided: how many runners a pool
	// should have depends on what its runners are doing, so the observation
	// has to come first.
	actual, listErrs := r.listAll(ctx)
	result.Errors = append(result.Errors, listErrs...)

	var desired []Spec
	states := map[string]github.State{}
	poolByName := map[string]model.Pool{}
	fingerprints := map[string]string{}
	secrets := map[string]model.Secret{}
	scopeSeen := map[string]bool{}

	for _, pool := range pools {
		poolByName[pool.Name] = pool

		fingerprint, err := r.store.CredentialFingerprint(ctx, pool.CredentialID)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("pool %s: %v", pool.Name, err))
			continue
		}

		// The runners need the credential without the daemon: they restart on
		// their own, after a reboot or a crash, and mint a registration token
		// each time. For an app that is its private key, which the agent
		// exchanges for an installation token itself.
		secret, err := r.store.Secret(ctx, pool.CredentialID)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("pool %s: %v", pool.Name, err))
			continue
		}
		if r.writeSecret != nil {
			if err := r.writeSecret(pool.CredentialID, secret.Token); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("pool %s: %v", pool.Name, err))
				continue
			}
		}

		fingerprints[pool.Name] = fingerprint
		secrets[pool.Name] = secret

		// One question per scope, not per runner: three replicas on one
		// repository are one call.
		scope := github.ScopeOf(pool)
		key := string(scope.Kind) + ":" + scope.Path
		if scopeSeen[key] || r.newClient == nil {
			continue
		}
		scopeSeen[key] = true
		client, err := r.newClient(secret)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("pool %s: %v", pool.Name, err))
			continue
		}
		poolStates, err := client.States(ctx, scope)
		if err != nil {
			// Not fatal, but it does change what the plan is allowed to do:
			// without an answer, nothing is known to be busy. The plan still
			// only removes runners the host says have stopped, so the worst
			// case is a slower drain, not a failed job.
			result.Errors = append(result.Errors, fmt.Sprintf("pool %s: ask GitHub what its runners are doing: %v", pool.Name, err))
			continue
		}
		for name, state := range poolStates {
			states[name] = state
		}
	}

	// Runners whose pool is gone are the ones most at risk of being removed
	// mid-job, and the loop above could not have asked about them: their pool
	// is no longer in the database to say where they are registered. They
	// carry that themselves, so they can still be asked about.
	r.statesForOrphans(ctx, actual, states, scopeSeen, &result)

	// Now that both halves are known — what is running, and what each runner is
	// doing — each pool is sized.
	byPool := map[string][]Runner{}
	for _, runner := range actual {
		byPool[runner.Pool] = append(byPool[runner.Pool], runner)
	}
	scaling := map[string]Scale{}
	for _, pool := range pools {
		fingerprint, ok := fingerprints[pool.Name]
		if !ok {
			continue // its credential could not be read; already reported
		}
		mine := byPool[pool.Name]
		scale := Autoscale(pool, mine, states, r.lastBusy(pool.Name, mine, states), r.now())
		scaling[pool.Name] = scale
		if scale.ScaledUp {
			result.ScaledUp = true
		}
		desired = append(desired,
			SpecsForCredential(pool, fingerprint, r.recipeFor(pool),
				DesiredNames(pool, mine, states, scale.Target), secrets[pool.Name])...)
	}
	result.Scaling = scaling
	r.rememberScaling(scaling)

	// What was observed is kept, so the UI can show what the fleet has been
	// doing rather than only what it is doing this second. Recorded before the
	// actions are applied: this is an observation, not a prediction.
	r.record(ctx, pools, byPool, states, scaling, &result)

	actions := Plan(desired, actual, states)
	result.Actions = actions

	r.mu.Lock()
	for _, runner := range actual {
		r.poolOf[runner.Name] = runner.Pool
	}
	r.mu.Unlock()

	for _, action := range actions {
		if err := r.apply(ctx, action, poolByName); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s %s: %v", action.Op, action.Runner, err))
			continue
		}
		r.log.Info("reconciled",
			"op", action.Op, "runner", action.Runner, "pool", action.Pool, "reason", action.Reason)
	}

	r.mu.Lock()
	r.last = result
	r.mu.Unlock()
	return result
}

// record keeps one observation per pool for the activity history, and adds
// what this pass saw to the per-pool job tally.
func (r *Reconciler) record(ctx context.Context, pools []model.Pool, byPool map[string][]Runner,
	states map[string]github.State, scaling map[string]Scale, result *Result) {

	at := r.now()

	samples := make([]model.Sample, 0, len(pools))
	for _, pool := range pools {
		sample := model.Sample{Pool: pool.Name, Scope: pool.Scope, Target: scaling[pool.Name].Target}
		for _, runner := range byPool[pool.Name] {
			if runner.State == StateStopping {
				continue
			}
			sample.Running++
			if states[runner.Name] == github.StateBusy {
				sample.Busy++
			}
		}
		samples = append(samples, sample)
	}
	if err := r.store.RecordSamples(ctx, at, samples); err != nil {
		// History is worth having, not worth failing a pass over.
		result.Errors = append(result.Errors, fmt.Sprintf("record what the fleet is doing: %v", err))
	}

	if err := r.store.RecordJobs(ctx, at, r.tally(at, pools, byPool, states)); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("record what the pools have run: %v", err))
	}
}

// MaxGap is the longest interval one pass will account a busy runner's time to.
//
// The tally adds up runner-time a pass at a time, and a pass that lands ten
// minutes after the last one usually means the daemon was not there for most
// of it — it was stopped, or the host was asleep. Counting the whole gap as
// work would invent time the fleet may never have spent, and under-reporting a
// fleet nobody was watching is the honest direction to be wrong in.
const MaxGap = 10 * time.Minute

// tally is what this pass adds to the per-pool job accounting.
//
// A job is a runner seen with work on it that had none last time anybody
// looked. That is the most a reconcile loop can know: it polls GitHub, so its
// resolution is the interval between passes. A job shorter than one pass is
// never seen, and two jobs run back to back on the same runner with no idle
// pass between them count as one — which ephemeral runners, replaced after
// every job, cannot do.
//
// The time is the same estimate seen from the side: a sum of rectangles one
// pass wide, not a stopwatch on each job.
func (r *Reconciler) tally(at time.Time, pools []model.Pool, byPool map[string][]Runner,
	states map[string]github.State) []model.JobSample {

	r.mu.Lock()
	defer r.mu.Unlock()

	var elapsed time.Duration
	if !r.lastPass.IsZero() && at.After(r.lastPass) {
		elapsed = min(at.Sub(r.lastPass), MaxGap)
	}
	r.lastPass = at

	busy := make(map[string]bool, len(r.busyLast))
	out := make([]model.JobSample, 0, len(pools))
	for _, pool := range pools {
		sample := model.JobSample{Pool: pool.Name}
		for _, runner := range byPool[pool.Name] {
			state, answered := states[runner.Name]
			working := state == github.StateBusy
			if !answered {
				// Silence is not an answer. When GitHub could not be asked
				// about a runner — the pool's credential failed, or the call
				// errored — the previous reading is carried forward rather
				// than read as idle: a job does not stop because the daemon
				// could not ask about it, and reading the outage as idle would
				// end every job in flight and start them all again when it
				// cleared.
				working = r.busyLast[runner.Name]
			}
			busy[runner.Name] = working
			if !working {
				continue
			}
			sample.BusySeconds += elapsed.Seconds()
			if !r.busyLast[runner.Name] {
				sample.Started++
			}
		}
		out = append(out, sample)
	}

	// Rebuilt rather than updated, so a runner that has gone stops being
	// remembered. An ephemeral pool replaces every runner after every job, and
	// a map that only ever grew would end up holding the fleet's whole history.
	r.busyLast = busy
	return out
}

// statesForOrphans asks GitHub about runners that no pool claims.
func (r *Reconciler) statesForOrphans(ctx context.Context, actual []Runner, states map[string]github.State, scopeSeen map[string]bool, result *Result) {
	if r.newClient == nil {
		return
	}
	for _, runner := range actual {
		if runner.Scope == "" || runner.CredentialID == 0 {
			continue
		}
		if _, known := states[runner.Name]; known {
			continue
		}
		scope := github.Scope{Kind: runner.ScopeKind, Path: runner.Scope}
		key := string(scope.Kind) + ":" + scope.Path
		if scopeSeen[key] {
			continue
		}
		scopeSeen[key] = true

		secret, err := r.store.Secret(ctx, runner.CredentialID)
		if err != nil {
			// The credential is gone too. Nothing can be learned, so the plan
			// falls back to what the host says — and the host only reports a
			// drained runner as stopped once it really has stopped.
			continue
		}
		client, err := r.newClient(secret)
		if err != nil {
			continue
		}
		orphanStates, err := client.States(ctx, scope)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("ask GitHub about %s: %v", runner.Name, err))
			continue
		}
		for name, state := range orphanStates {
			if _, known := states[name]; !known {
				states[name] = state
			}
		}
	}
}

// Last is the most recent pass, for the UI.
func (r *Reconciler) Last() Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

func (r *Reconciler) apply(ctx context.Context, action Action, pools map[string]model.Pool) error {
	executor, ok := r.executors[action.Runtime]
	if !ok {
		return fmt.Errorf("no executor for runtime %q", action.Runtime)
	}

	switch action.Op {
	case OpCreate, OpStart:
		if action.Spec == nil {
			return fmt.Errorf("a %s action without a spec", action.Op)
		}
		spec := *action.Spec
		// A runtime that shares everything with the job it runs must not be
		// handed the credential, so the daemon does the minting and passes on
		// a token that can do one thing and expires in an hour.
		if spec.Runtime == model.RuntimeContainer {
			minted, err := r.mintFor(ctx, spec, pools)
			if err != nil {
				return err
			}
			spec.RegistrationToken = minted
		}
		if action.Op == OpCreate {
			return executor.Create(ctx, spec)
		}
		return executor.Start(ctx, spec)
	case OpDrain:
		return executor.Drain(ctx, action.Runner)
	case OpRemove:
		if err := executor.Remove(ctx, action.Runner); err != nil {
			return err
		}
		// Deregistering is best effort and deliberately after the removal: a
		// fleet that has scaled down should not leave a list of offline
		// runners behind on the repository, but failing to tidy up is not a
		// reason to keep the runner.
		r.deregister(ctx, action, pools)
		return nil
	default:
		return fmt.Errorf("unknown operation %q", action.Op)
	}
}

// mintFor buys a registration token for one runner.
func (r *Reconciler) mintFor(ctx context.Context, spec Spec, pools map[string]model.Pool) (string, error) {
	if r.newClient == nil {
		return "", nil
	}
	pool, ok := pools[spec.Pool]
	if !ok {
		return "", fmt.Errorf("pool %s is gone", spec.Pool)
	}
	secret, err := r.store.Secret(ctx, pool.CredentialID)
	if err != nil {
		return "", err
	}
	client, err := r.newClient(secret)
	if err != nil {
		return "", err
	}
	return client.RegistrationToken(ctx, github.ScopeOf(pool))
}

func (r *Reconciler) deregister(ctx context.Context, action Action, pools map[string]model.Pool) {
	if r.newClient == nil {
		return
	}
	pool, ok := pools[action.Pool]
	if !ok {
		// The pool is gone, so there is nothing left that says which
		// credential could deregister it. GitHub drops offline runners on its
		// own eventually.
		return
	}
	secret, err := r.store.Secret(ctx, pool.CredentialID)
	if err != nil {
		return
	}
	client, err := r.newClient(secret)
	if err != nil {
		return
	}
	if err := client.Deregister(ctx, github.ScopeOf(pool), action.Runner); err != nil {
		r.log.Warn("could not deregister", "runner", action.Runner, "error", err)
	}
}

func (r *Reconciler) listAll(ctx context.Context) ([]Runner, []string) {
	var (
		all  []Runner
		errs []string
	)
	for _, runtime := range sortedRuntimes(r.executors) {
		runners, err := r.executors[runtime].List(ctx)
		if err != nil {
			errs = append(errs, fmt.Sprintf("list %s runners: %v", runtime, err))
			continue
		}
		all = append(all, runners...)
	}
	return all, errs
}

// recipeFor asks the executor that would build this pool's runners how it would
// build them. A pool whose runtime has no executor on this host gets nothing,
// which is right: there is no recipe, and there are no runners either.
func (r *Reconciler) recipeFor(pool model.Pool) string {
	executor, ok := r.executors[pool.Runtime]
	if !ok {
		return ""
	}
	return executor.Recipe(pool)
}

func sortedRuntimes(executors map[model.Runtime]Executor) []model.Runtime {
	out := make([]model.Runtime, 0, len(executors))
	for runtime := range executors {
		out = append(out, runtime)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// RunnerStatus is one runner as the UI sees it: what the host says, and what
// GitHub says, side by side. They answer different questions — the host knows
// whether a machine is up, only GitHub knows whether a job is on it.
type RunnerStatus struct {
	Name       string      `json:"name"`
	Pool       string      `json:"pool"`
	Runtime    string      `json:"runtime"`
	State      RunnerState `json:"state"`
	Job        string      `json:"job"`
	Generation string      `json:"generation"`
	UpToDate   bool        `json:"upToDate"`
	// Trouble is what the host says is wrong with this runner, if anything.
	// A runner can be dead and look busy, and this is where it says so.
	Trouble string `json:"trouble,omitempty"`
}

// Registering is how long a runner is given to appear on GitHub before its
// absence is worth remarking on.
//
// A machine boots, runs cloud-init and registers, which is a minute or two; an
// ephemeral one does that after every job, so for a busy pool this state is
// most of what anybody sees. Reporting it as "unknown" the whole time made a
// working fleet look broken — three times in one day, to the person who built
// it.
const Registering = 4 * time.Minute

// jobOfARunnerGitHubHasNotSeen distinguishes a runner on its way up from one
// that should be there and is not.
func jobOfARunnerGitHubHasNotSeen(runner Runner) string {
	// The host is bringing it up: between two machines, or launching one. This
	// covers the seconds an ephemeral runner spends with nothing running at
	// all, which is otherwise indistinguishable from a runner that has gone.
	if runner.Coming {
		return "starting"
	}
	if runner.State == StateRunning && runner.Up > 0 && runner.Up < Registering {
		return "starting"
	}
	return "unknown"
}

// Status reports the fleet for the UI.
func (r *Reconciler) Status(ctx context.Context) ([]RunnerStatus, []string) {
	actual, errs := r.listAll(ctx)

	pools, err := r.store.ListPools(ctx)
	if err != nil {
		return nil, append(errs, err.Error())
	}

	generations := map[string]string{}
	states := map[string]github.State{}
	scopeSeen := map[string]bool{}
	for _, pool := range pools {
		fingerprint, err := r.store.CredentialFingerprint(ctx, pool.CredentialID)
		if err != nil {
			continue
		}
		generations[pool.Name] = pool.Generation(fingerprint, r.recipeFor(pool))

		scope := github.ScopeOf(pool)
		key := string(scope.Kind) + ":" + scope.Path
		if scopeSeen[key] || r.newClient == nil {
			continue
		}
		scopeSeen[key] = true
		secret, err := r.store.Secret(ctx, pool.CredentialID)
		if err != nil {
			continue
		}
		client, err := r.newClient(secret)
		if err != nil {
			errs = append(errs, fmt.Sprintf("pool %s: %v", pool.Name, err))
			continue
		}
		poolStates, err := client.States(ctx, scope)
		if err != nil {
			errs = append(errs, fmt.Sprintf("pool %s: %v", pool.Name, err))
			continue
		}
		for name, state := range poolStates {
			states[name] = state
		}
	}

	r.statesForOrphans(ctx, actual, states, scopeSeen, &Result{})

	out := make([]RunnerStatus, 0, len(actual))
	for _, runner := range sortedRunners(actual) {
		job := string(states[runner.Name])
		if job == "" {
			job = jobOfARunnerGitHubHasNotSeen(runner)
		}
		want, known := generations[runner.Pool]
		out = append(out, RunnerStatus{
			Name:       runner.Name,
			Pool:       runner.Pool,
			Runtime:    string(runner.Runtime),
			State:      runner.State,
			Job:        job,
			Generation: runner.Generation,
			UpToDate:   known && want == runner.Generation,
			Trouble:    runner.Trouble,
		})
	}
	return out, errs
}
