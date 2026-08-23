package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/model"
)

// budgetPool is a pool of a stated size and shape, so a test can say "four
// machines of two processors and four gigabytes" and have the arithmetic be
// obvious from the name.
func budgetPool(name string, min, max, cpus, memoryMB int) model.Pool {
	p := model.Pool{
		ID: 1, Name: name, ScopeKind: model.ScopeRepository, Scope: "o/" + name,
		Runtime: model.RuntimeVM, MinReplicas: min, MaxReplicas: max,
		CPUs: cpus, MemoryMB: memoryMB, CredentialID: 1, Enabled: true,
	}
	p.Defaults()
	return p
}

// asked is what the autoscaler decided, before the budget has had its say.
func asked(pool string, target int) map[string]Scale {
	return map[string]Scale{pool: {Target: target, ScaledUp: true, Reason: "every runner is busy"}}
}

func targets(scaling map[string]Scale) map[string]int {
	out := make(map[string]int, len(scaling))
	for name, scale := range scaling {
		out[name] = scale.Target
	}
	return out
}

// A host with no budget behaves exactly as it did before budgets existed. Every
// install has this until somebody sets one, so anything else here is a
// regression on every host at once.
func TestNoBudgetChangesNothing(t *testing.T) {
	pools := []model.Pool{budgetPool("web", 1, 10, 4, 8192)}
	scaling := asked("web", 9)

	got := Ration(pools, scaling, model.Budget{})

	if got["web"].Target != 9 {
		t.Fatalf("target is %d, want the 9 the autoscaler asked for", got["web"].Target)
	}
	if !got["web"].ScaledUp || got["web"].Reason != "every runner is busy" {
		t.Fatalf("the autoscaler's decision was rewritten: %+v", got["web"])
	}
}

// The ordinary case: a pool that would grow past what the host may spend is
// held where the money runs out.
func TestGrowthStopsWhereTheBudgetDoes(t *testing.T) {
	// Four processors each, and twelve to go round: three machines.
	pools := []model.Pool{budgetPool("web", 1, 10, 4, 4096)}

	got := Ration(pools, asked("web", 9), model.Budget{CPUs: 12})

	if got["web"].Target != 3 {
		t.Fatalf("target is %d, want 3 — four processors each out of twelve", got["web"].Target)
	}
}

// Memory is the other half of the same rule, and either dimension alone binds.
func TestMemoryBindsTheSameWay(t *testing.T) {
	pools := []model.Pool{budgetPool("web", 1, 10, 1, 8192)}

	got := Ration(pools, asked("web", 9), model.Budget{MemoryMB: 24576})

	if got["web"].Target != 3 {
		t.Fatalf("target is %d, want 3 — eight gigabytes each out of twenty-four", got["web"].Target)
	}
}

// Whichever runs out first is the one that binds.
func TestTheTighterDimensionWins(t *testing.T) {
	// Processors would allow eight; memory allows two.
	pools := []model.Pool{budgetPool("web", 1, 10, 1, 8192)}

	got := Ration(pools, asked("web", 8), model.Budget{CPUs: 8, MemoryMB: 16384})

	if got["web"].Target != 2 {
		t.Fatalf("target is %d, want 2 — memory ran out first", got["web"].Target)
	}
}

// A dimension the budget does not name is not a refusal. A budget that caps
// memory and says nothing about processors is a memory budget.
func TestAnUnnamedDimensionIsNotAConstraint(t *testing.T) {
	pools := []model.Pool{budgetPool("web", 1, 64, 8, 512)}

	got := Ration(pools, asked("web", 20), model.Budget{MemoryMB: 1024 * 1024})

	if got["web"].Target != 20 {
		t.Fatalf("target is %d: a memory budget capped the processors too", got["web"].Target)
	}
}

// A pool held by the budget is not scaling up, whatever the autoscaler thought.
//
// This is not cosmetic. ScaledUp is what makes the daemon come back in three
// seconds instead of thirty, and coming back in three seconds to be refused
// again is a busy loop that reconciles the whole host, asks GitHub about every
// runner, and changes nothing — for as long as the pool stays busy.
func TestAPoolTheBudgetIsHoldingIsNotRamping(t *testing.T) {
	pools := []model.Pool{budgetPool("web", 1, 10, 4, 4096)}

	got := Ration(pools, asked("web", 9), model.Budget{CPUs: 8})

	if got["web"].ScaledUp {
		t.Fatal("a pool that was refused reports as having scaled up, so the daemon will" +
			" come back in three seconds to be refused again")
	}
}

// And it says so, in the terms the budget was set in, so the reason on the
// pools page can be read without opening the settings page.
func TestTheReasonSaysWhatIsHoldingThePool(t *testing.T) {
	pools := []model.Pool{budgetPool("web", 1, 10, 4, 4096)}

	got := Ration(pools, asked("web", 9), model.Budget{CPUs: 8, MemoryMB: 16384})

	reason := got["web"].Reason
	for _, want := range []string{"budget", "8 cpus", "16384 MiB"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the reason does not mention %q: %q", want, reason)
		}
	}
}

// A pool that fits is left entirely alone, including its reason. Rewriting it
// would put "the fleet budget is spent" on a pool that is idle.
func TestAPoolThatFitsIsUntouched(t *testing.T) {
	pools := []model.Pool{budgetPool("web", 1, 4, 2, 2048)}
	scaling := map[string]Scale{"web": {Target: 2, Reason: "nothing has been busy for 5m0s"}}

	got := Ration(pools, scaling, model.Budget{CPUs: 64, MemoryMB: 65536})

	if got["web"].Target != 2 || got["web"].Reason != "nothing has been busy for 5m0s" {
		t.Fatalf("an idle pool inside the budget was rewritten: %+v", got["web"])
	}
}

// The minimums are a promise the budget does not get to break.
//
// A pool with no runner cannot accept a job, so it can never discover that it
// needs one. A budget that scaled pools to nothing would not slow the fleet
// down — it would switch it off, and nothing would ever switch it back on.
func TestTheMinimumsAreAlwaysPaid(t *testing.T) {
	pools := []model.Pool{
		budgetPool("api", 2, 8, 4, 4096),
		budgetPool("web", 2, 8, 4, 4096),
	}
	scaling := map[string]Scale{
		"api": {Target: 6, ScaledUp: true},
		"web": {Target: 6, ScaledUp: true},
	}

	// Sixteen minimum processors promised, four available.
	got := Ration(pools, scaling, model.Budget{CPUs: 4})

	if got["api"].Target != 2 || got["web"].Target != 2 {
		t.Fatalf("a pool was scaled below its minimum and can no longer see demand: %v", targets(got))
	}
}

// And an operator who has promised more minimums than the host can afford is
// told that, rather than being told the budget is merely spent — the two need
// different things done about them.
func TestOvercommittedMinimumsSayWhatIsWrong(t *testing.T) {
	pools := []model.Pool{budgetPool("web", 4, 8, 4, 4096)}
	got := Ration(pools, asked("web", 8), model.Budget{CPUs: 4})

	reason := got["web"].Reason
	for _, want := range []string{"minimums", "exceed"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the reason does not mention %q: %q", want, reason)
		}
	}
	if !strings.Contains(reason, "raise the budget") && !strings.Contains(reason, "lower a minimum") {
		t.Errorf("the reason does not say what to do about it: %q", reason)
	}
}

// What is left after the minimums is shared a runner at a time, in rounds, so
// that a pool asking for a lot does not empty the budget before a pool asking
// for a little is looked at.
func TestTheSurplusIsSharedRoundByRound(t *testing.T) {
	pools := []model.Pool{
		budgetPool("api", 1, 8, 1, 1024),
		budgetPool("web", 1, 8, 1, 1024),
	}
	scaling := map[string]Scale{
		"api": {Target: 2, ScaledUp: true}, // wants one more
		"web": {Target: 8, ScaledUp: true}, // wants six more
	}

	// Two for the minimums, four to share.
	got := Ration(pools, scaling, model.Budget{CPUs: 6})

	// api wanted one more and got it; web took everything that was left.
	if got["api"].Target != 2 {
		t.Errorf("the smaller pool was starved: api got %d, want 2", got["api"].Target)
	}
	if got["web"].Target != 4 {
		t.Errorf("web got %d, want 4 — the rest of the budget", got["web"].Target)
	}
}

// The greedy pool does not get to go first just because it asked for more.
func TestNoPoolEmptiesTheBudgetBeforeTheOthersAreLookedAt(t *testing.T) {
	pools := []model.Pool{
		budgetPool("api", 1, 20, 1, 1024),
		budgetPool("web", 1, 20, 1, 1024),
	}
	scaling := map[string]Scale{
		"api": {Target: 20, ScaledUp: true},
		"web": {Target: 20, ScaledUp: true},
	}

	got := Ration(pools, scaling, model.Budget{CPUs: 8})

	if got["api"].Target != 4 || got["web"].Target != 4 {
		t.Fatalf("two identical pools did not split the budget: %v", targets(got))
	}
}

// A budget spent on a large pool still leaves a small one able to grow: the
// round-robin skips whoever no longer fits rather than stopping.
func TestASmallPoolStillFitsAfterALargeOneStops(t *testing.T) {
	pools := []model.Pool{
		budgetPool("big", 1, 8, 8, 1024),
		budgetPool("small", 1, 8, 1, 1024),
	}
	scaling := map[string]Scale{
		"big":   {Target: 8, ScaledUp: true},
		"small": {Target: 8, ScaledUp: true},
	}

	// Nine minimum, and twenty to spend: big can afford one more (17), then
	// small takes the remaining three one at a time.
	got := Ration(pools, scaling, model.Budget{CPUs: 20})

	if got["big"].Target != 2 {
		t.Errorf("big got %d, want 2", got["big"].Target)
	}
	if got["small"].Target != 4 {
		t.Errorf("small got %d, want 4 — the processors big could not use", got["small"].Target)
	}
	if spent := got["big"].Target*8 + got["small"].Target*1; spent > 20 {
		t.Fatalf("the budget of 20 was overspent by %d", spent-20)
	}
}

// Container pools are counted by neither rule. They are not in the slice, so
// charging them against a ceiling they are not subject to would make the figure
// mean two different things at once.
func TestContainerPoolsAreOutsideTheBudget(t *testing.T) {
	machines := budgetPool("web", 1, 8, 4, 4096)
	containers := budgetPool("ci", 1, 8, 4, 4096)
	containers.Runtime = model.RuntimeContainer

	scaling := map[string]Scale{
		"web": {Target: 4, ScaledUp: true},
		"ci":  {Target: 4, ScaledUp: true},
	}
	got := Ration([]model.Pool{machines, containers}, scaling, model.Budget{CPUs: 8})

	if got["ci"].Target != 4 {
		t.Errorf("a container pool was rationed against a group it is not in: got %d", got["ci"].Target)
	}
	if got["web"].Target != 2 {
		t.Errorf("the machines got %d, want 2 — and the containers did not take any of it",
			got["web"].Target)
	}
}

// Lowering the budget shrinks the fleet, down to the minimums and no further.
// Nothing is killed by it: a target below what is running is a drain, and a
// drain waits for the job.
func TestLoweringTheBudgetShrinksTheFleet(t *testing.T) {
	pools := []model.Pool{budgetPool("web", 2, 10, 4, 4096)}
	scaling := map[string]Scale{"web": {Target: 8}}

	got := Ration(pools, scaling, model.Budget{CPUs: 12})
	if got["web"].Target != 3 {
		t.Fatalf("target is %d, want 3", got["web"].Target)
	}

	got = Ration(pools, scaling, model.Budget{CPUs: 1})
	if got["web"].Target != 2 {
		t.Fatalf("target is %d, want the minimum of 2", got["web"].Target)
	}
}

// A pool that was never sized — its credential could not be read, and the pass
// has already said so — must not be invented here, or the budget would be spent
// on runners nothing is going to create.
func TestAPoolThatWasNotSizedIsNotRationed(t *testing.T) {
	pools := []model.Pool{
		budgetPool("web", 1, 8, 4, 4096),
		budgetPool("broken", 1, 8, 4, 4096),
	}
	scaling := map[string]Scale{"web": {Target: 8, ScaledUp: true}}

	got := Ration(pools, scaling, model.Budget{CPUs: 8})

	if _, invented := got["broken"]; invented {
		t.Fatal("a pool that could not be sized was given a target anyway")
	}
	if got["web"].Target != 2 {
		t.Errorf("web got %d, want 2 — the broken pool did not reserve anything",
			got["web"].Target)
	}
}

// A pool that is switched off has a ceiling of zero runners and costs nothing,
// so switching one off is a way to free the budget for the others.
func TestASwitchedOffPoolCostsNothing(t *testing.T) {
	off := budgetPool("idle", 1, 8, 8, 8192)
	off.Enabled = false
	pools := []model.Pool{off, budgetPool("web", 1, 8, 4, 4096)}
	scaling := map[string]Scale{
		"idle": {Target: 0, Reason: "the pool is switched off"},
		"web":  {Target: 4, ScaledUp: true},
	}

	got := Ration(pools, scaling, model.Budget{CPUs: 16})

	if got["web"].Target != 4 {
		t.Fatalf("web got %d, want 4 — a pool that is off reserved nothing", got["web"].Target)
	}
	if got["idle"].Reason != "the pool is switched off" {
		t.Errorf("a pool that is off was told about the budget: %q", got["idle"].Reason)
	}
}

// Same pools, same budget, same answer — twice in a row and in any order the
// map happens to iterate in. A plan that shuffles is a plan nobody can review,
// and this one is upstream of the plan.
func TestRationingIsDeterministic(t *testing.T) {
	pools := []model.Pool{
		budgetPool("a", 1, 8, 3, 1024),
		budgetPool("b", 1, 8, 3, 1024),
		budgetPool("c", 1, 8, 3, 1024),
	}
	scaling := map[string]Scale{
		"a": {Target: 8, ScaledUp: true},
		"b": {Target: 8, ScaledUp: true},
		"c": {Target: 8, ScaledUp: true},
	}

	first := targets(Ration(pools, scaling, model.Budget{CPUs: 25}))
	for i := 0; i < 20; i++ {
		again := targets(Ration(pools, scaling, model.Budget{CPUs: 25}))
		for name, target := range first {
			if again[name] != target {
				t.Fatalf("run %d gave %v, the first gave %v", i, again, first)
			}
		}
	}
}

// Rationing must not scribble on what it was handed. The reconciler keeps the
// autoscaler's decisions and reports them, and a map mutated underneath it
// would have the pools page showing conclusions nothing reached.
func TestRationingDoesNotMutateWhatItWasGiven(t *testing.T) {
	pools := []model.Pool{budgetPool("web", 1, 10, 4, 4096)}
	scaling := map[string]Scale{"web": {Target: 9, ScaledUp: true, Reason: "every runner is busy"}}

	Ration(pools, scaling, model.Budget{CPUs: 4})

	if scaling["web"].Target != 9 || !scaling["web"].ScaledUp {
		t.Fatalf("the caller's map was rewritten: %+v", scaling["web"])
	}
}

// A weight decides who yields when the host is contended. It is not a ceiling,
// and rationing against it would refuse growth to a fleet that is allowed to
// use the whole machine.
func TestAWeightRationsNothing(t *testing.T) {
	pools := []model.Pool{budgetPool("web", 1, 20, 8, 8192)}

	got := Ration(pools, asked("web", 20), model.Budget{CPUWeight: 10})

	if got["web"].Target != 20 {
		t.Fatalf("target is %d: a weight was enforced as a cap", got["web"].Target)
	}
}

// The budget is never overspent, whatever the pools are shaped like. Sizes that
// do not divide the budget evenly are the case a round-robin can get wrong.
func TestTheBudgetIsNeverOverspent(t *testing.T) {
	pools := []model.Pool{
		budgetPool("a", 1, 20, 3, 1500),
		budgetPool("b", 1, 20, 5, 2500),
		budgetPool("c", 1, 20, 7, 3500),
	}
	scaling := map[string]Scale{
		"a": {Target: 20, ScaledUp: true},
		"b": {Target: 20, ScaledUp: true},
		"c": {Target: 20, ScaledUp: true},
	}

	for _, budget := range []model.Budget{
		{CPUs: 16}, {CPUs: 31}, {CPUs: 100}, {MemoryMB: 20000}, {CPUs: 40, MemoryMB: 20000},
	} {
		got := Ration(pools, scaling, budget)
		var cpus, memory int
		for _, pool := range pools {
			cpus += got[pool.Name].Target * pool.CPUs
			memory += got[pool.Name].Target * pool.MemoryMB
		}
		if budget.CPUs > 0 && cpus > budget.CPUs {
			t.Errorf("%+v: spent %d processors", budget, cpus)
		}
		if budget.MemoryMB > 0 && memory > budget.MemoryMB {
			t.Errorf("%+v: spent %d MiB", budget, memory)
		}
	}
}

// The one case where the budget is allowed to be overspent, and the only one:
// the minimums, which are paid whatever they cost. Anything else would switch
// pools off permanently.
func TestOnlyTheMinimumsMayOverspend(t *testing.T) {
	pools := []model.Pool{budgetPool("web", 5, 10, 4, 4096)}

	got := Ration(pools, asked("web", 10), model.Budget{CPUs: 4})

	if got["web"].Target != 5 {
		t.Fatalf("target is %d, want the minimum of 5 even though it costs 20 processors of a"+
			" budget of 4", got["web"].Target)
	}
}

// ---------------------------------------------------------------------------
// The whole pass: from the settings row to the host and back
// ---------------------------------------------------------------------------

// budgetedExecutor is an executor that has somewhere to put a budget, which is
// what the machine executor is and the container one is not.
type budgetedExecutor struct {
	*fakeExecutor
	budgets []model.Budget
	err     error
}

func (b *budgetedExecutor) ApplyBudget(ctx context.Context, budget model.Budget) error {
	b.budgets = append(b.budgets, budget)
	return b.err
}

func (b *budgetedExecutor) last() model.Budget {
	if len(b.budgets) == 0 {
		return model.Budget{}
	}
	return b.budgets[len(b.budgets)-1]
}

// budgetHarness is the reconciler with a machine runtime that can be held to a
// budget and a container runtime that cannot, which is the shape of a real
// host.
type budgetHarness struct {
	store  *fakeStore
	vm     *budgetedExecutor
	docker *fakeExecutor
	gh     *fakeGitHub
	rec    *Reconciler
}

func newBudgetHarness(pools ...model.Pool) *budgetHarness {
	h := &budgetHarness{
		store:  &fakeStore{pools: pools, settings: map[string]string{}},
		vm:     &budgetedExecutor{fakeExecutor: newFakeExecutor(model.RuntimeVM)},
		docker: newFakeExecutor(model.RuntimeContainer),
		gh:     &fakeGitHub{states: map[string]github.State{}},
	}
	h.rec = New(h.store, []Executor{h.vm, h.docker},
		func(model.Secret) (GitHubClient, error) { return h.gh, nil },
		func(int64, string) error { return nil },
		discardLogger())
	return h
}

// busy is GitHub reporting that every machine on the host has a job on it,
// which is the only evidence the autoscaler has that a pool should grow.
func (h *budgetHarness) busy() {
	for name := range h.vm.runners {
		h.gh.states[name] = github.StateBusy
	}
}

// The budget lives in the settings table and is applied to the host by the
// reconciler, so that a change made in the UI reaches the fleet on the next
// pass without anything having to be told about it.
func TestThePassPutsTheBudgetOnTheHost(t *testing.T) {
	h := newBudgetHarness(budgetPool("web", 1, 1, 2, 2048))
	h.store.budget(t, model.Budget{CPUs: 8, MemoryMB: 16384})

	result := h.rec.Once(context.Background())

	if len(result.Errors) != 0 {
		t.Fatalf("errors: %v", result.Errors)
	}
	if got := h.vm.last(); got.CPUs != 8 || got.MemoryMB != 16384 {
		t.Fatalf("the host was given %+v", got)
	}
}

// A budget changed between two passes reaches the host on the second, which is
// the whole reason it is read every pass rather than held.
func TestARaisedBudgetReachesTheHostOnTheNextPass(t *testing.T) {
	h := newBudgetHarness(budgetPool("web", 1, 1, 2, 2048))
	ctx := context.Background()
	h.store.budget(t, model.Budget{CPUs: 4})
	h.rec.Once(ctx)

	h.store.budget(t, model.Budget{CPUs: 16})
	h.rec.Once(ctx)

	if got := h.vm.last(); got.CPUs != 16 {
		t.Fatalf("the host is still on the old budget: %+v", got)
	}
}

// A runtime with nowhere to put a budget is skipped, not warned about. It is
// not broken; it simply is not inside the group, which is the same rule the
// rationing follows.
func TestARuntimeThatCannotBeBudgetedIsQuiet(t *testing.T) {
	h := newBudgetHarness(budgetPool("web", 1, 1, 2, 2048))
	h.store.budget(t, model.Budget{CPUs: 8})

	result := h.rec.Once(context.Background())

	for _, message := range result.Errors {
		if strings.Contains(message, "container") {
			t.Fatalf("the container runtime was blamed for not having a slice: %q", message)
		}
	}
}

// A host that will not take the budget — no root, no systemd, a read-only /etc
// — still has a fleet to maintain, and the rationing holds it to the budget
// regardless. That is the half of this that does not need the host's help.
func TestAHostThatRefusesTheBudgetStillGetsItsFleet(t *testing.T) {
	h := newBudgetHarness(budgetPool("web", 2, 2, 2, 2048))
	h.store.budget(t, model.Budget{CPUs: 64})
	h.vm.err = errors.New("write /etc/systemd/system/runner-fleet.slice: read-only file system")

	result := h.rec.Once(context.Background())

	if len(result.Errors) == 0 {
		t.Error("a budget that could not be applied was not reported")
	}
	if got := strings.Join(h.vm.calls, "; "); got != "create web-1; create web-2" {
		t.Fatalf("the fleet was abandoned over a slice: %q", got)
	}
}

// A settings row that is not a budget is reported and then treated as no budget
// at all. Refusing to reconcile would take a fleet down over one row.
func TestANonsenseBudgetDoesNotStopThePass(t *testing.T) {
	h := newBudgetHarness(budgetPool("web", 2, 2, 2, 2048))
	h.store.settings[model.SettingFleetBudget] = "not a budget"

	result := h.rec.Once(context.Background())

	if len(result.Errors) == 0 {
		t.Error("a budget nobody can read was not mentioned")
	}
	if got := strings.Join(h.vm.calls, "; "); got != "create web-1; create web-2" {
		t.Fatalf("the fleet was abandoned over a settings row: %q", got)
	}
	if h.vm.last().Capped() {
		t.Error("an unreadable budget was applied to the host as if it were one")
	}
}

// A budget stored by some later version with a value this one refuses is the
// same case, and must not become a limit nobody asked for.
func TestAStoredBudgetThatFailsValidationIsIgnored(t *testing.T) {
	h := newBudgetHarness(budgetPool("web", 1, 1, 2, 2048))
	h.store.settings[model.SettingFleetBudget] = `{"cpus":4,"memoryMb":8}`

	result := h.rec.Once(context.Background())

	if len(result.Errors) == 0 {
		t.Error("a budget that cannot be used was applied silently")
	}
	if h.vm.last().Capped() {
		t.Fatalf("it was applied anyway: %+v", h.vm.last())
	}
}

// A store that will not answer at all is reported once and the pass carries on
// uncapped.
func TestAStoreThatWillNotSayIsReportedOnce(t *testing.T) {
	h := newBudgetHarness(budgetPool("web", 1, 1, 2, 2048))
	h.store.settingErr = errors.New("database is locked")

	result := h.rec.Once(context.Background())

	var mentions int
	for _, message := range result.Errors {
		if strings.Contains(message, "fleet budget") {
			mentions++
		}
	}
	if mentions != 1 {
		t.Fatalf("the budget was mentioned %d times in %v", mentions, result.Errors)
	}
}

// The whole point, end to end: a pool that would grow is held where the budget
// runs out, over several passes, on a fleet that is genuinely busy.
func TestABusyPoolIsHeldAtTheBudget(t *testing.T) {
	h := newBudgetHarness(budgetPool("web", 1, 6, 4, 4096))
	h.store.budget(t, model.Budget{CPUs: 8})
	ctx := context.Background()

	// The minimum, first.
	h.rec.Once(ctx)
	if len(h.vm.runners) != 1 {
		t.Fatalf("the pool did not start: %d runners", len(h.vm.runners))
	}

	// Busy, so it grows — and can afford to.
	h.busy()
	result := h.rec.Once(ctx)
	if len(h.vm.runners) != 2 {
		t.Fatalf("a busy pool inside its budget did not grow: %d runners", len(h.vm.runners))
	}
	if !result.ScaledUp {
		t.Error("growth that was allowed did not report as growth")
	}

	// Still busy, and now the eight processors are spent. Two more passes,
	// because a fleet held at a ceiling must stay held rather than creeping.
	for i := 0; i < 2; i++ {
		h.busy()
		result = h.rec.Once(ctx)
		if len(h.vm.runners) != 2 {
			t.Fatalf("pass %d: the budget of 8 processors is holding %d machines of 4",
				i, len(h.vm.runners))
		}
		if result.ScaledUp {
			t.Fatalf("pass %d: a refused pool reported as scaling up, so the daemon comes"+
				" back in three seconds to be refused again", i)
		}
	}

	// And the operator is told why the pool is not growing.
	if reason := h.rec.Scaling()["web"].Reason; !strings.Contains(reason, "budget") {
		t.Errorf("the pool does not say what is holding it: %q", reason)
	}
}

// Lowering the budget under a running fleet drains the excess rather than
// killing it: a machine that is drained finishes the job it is on first.
func TestLoweringTheBudgetDrainsRatherThanKills(t *testing.T) {
	h := newBudgetHarness(budgetPool("web", 1, 6, 4, 4096))
	h.store.budget(t, model.Budget{CPUs: 16})
	ctx := context.Background()

	h.rec.Once(ctx)
	for i := 0; i < 3; i++ {
		h.busy()
		h.rec.Once(ctx)
	}
	if len(h.vm.runners) != 4 {
		t.Fatalf("the fleet did not reach four: %d", len(h.vm.runners))
	}

	// The host is needed for something else.
	h.store.budget(t, model.Budget{CPUs: 4})
	h.vm.calls = nil
	h.rec.Once(ctx)

	var drained, removed int
	for _, call := range h.vm.calls {
		switch {
		case strings.HasPrefix(call, "drain "):
			drained++
		case strings.HasPrefix(call, "remove "):
			removed++
		}
	}
	if drained != 3 {
		t.Fatalf("want three machines asked to finish and go; got %v", h.vm.calls)
	}
	// Nothing was removed while it was still stopping: a drain waits for the
	// job, and removing underneath it is how a lowered budget would fail a job.
	if removed != 0 {
		t.Fatalf("a machine was removed mid-drain: %v", h.vm.calls)
	}
}
