package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/clems4ever/github-runner/internal/model"
)

// setBudget is an operator on the settings page, saving what the whole fleet
// may take from this host.
func (f *fleet) setBudget(budget model.Budget) {
	f.t.Helper()
	f.mustRequest("PUT", "/api/settings/budget", budget, http.StatusOK)
}

// budget is what the settings page shows.
func (f *fleet) budget() model.Budget {
	f.t.Helper()
	payload := f.mustRequest("GET", "/api/settings", nil, http.StatusOK)
	var body struct {
		Budget model.Budget `json:"budget"`
	}
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		f.t.Fatal(err)
	}
	return body.Budget
}

// sizedVMPool is an elastic pool whose machines are a stated size, so a test
// can say what the fleet costs and what the budget will therefore allow.
func sizedVMPool(credentialID int64, min, max, cpus, memoryMB int) map[string]any {
	pool := elasticVMPool(credentialID, min, max)
	pool["cpus"] = cpus
	pool["memoryMb"] = memoryMB
	pool["diskGb"] = 40
	return pool
}

// The whole promise, driven the way an operator drives it: the pool would grow
// to eight machines and the host may only pay for three of them.
func TestABudgetHoldsTheFleetBelowThePoolsCeiling(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	// Four processors each, and twelve to go round.
	f.addPool(sizedVMPool(credential, 1, 8, 4, 4096))
	f.setBudget(model.Budget{CPUs: 12})
	f.reconcileNow()

	// It climbs while there is work and while there is money.
	for i := 0; i < 6; i++ {
		f.busy(f.vm.names()...)
		f.reconcileNow()
	}

	if got := strings.Join(f.vm.names(), ","); got != "web-1,web-2,web-3" {
		t.Fatalf("the fleet is %q; three machines of four processors is the whole budget", got)
	}
}

// Without the budget the same pool reaches its own ceiling, which is what makes
// the test above about the budget rather than about the autoscaler.
func TestTheSamePoolReachesItsCeilingWithoutABudget(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(sizedVMPool(credential, 1, 8, 4, 4096))
	f.reconcileNow()

	for i := 0; i < 10; i++ {
		f.busy(f.vm.names()...)
		f.reconcileNow()
	}

	if got := len(f.vm.names()); got != 8 {
		t.Fatalf("an unbudgeted pool reached %d of its maximum of 8", got)
	}
}

// Raising the budget lets the fleet grow into it, without anything being
// restarted or reconfigured.
func TestRaisingTheBudgetLetsTheFleetGrow(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(sizedVMPool(credential, 1, 8, 4, 4096))
	f.setBudget(model.Budget{CPUs: 8})
	f.reconcileNow()
	for i := 0; i < 4; i++ {
		f.busy(f.vm.names()...)
		f.reconcileNow()
	}
	if got := len(f.vm.names()); got != 2 {
		t.Fatalf("the fleet is %d machines, want the 2 the budget pays for", got)
	}

	// The host got bigger, or something else on it went away.
	f.setBudget(model.Budget{CPUs: 24})
	for i := 0; i < 4; i++ {
		f.busy(f.vm.names()...)
		f.reconcileNow()
	}

	if got := len(f.vm.names()); got != 6 {
		t.Fatalf("the fleet is %d machines, want the 6 the raised budget pays for", got)
	}
}

// Lowering it under a working fleet drains the excess rather than killing it: a
// machine asked to stop finishes the job it is on first, however long that
// takes. A budget is not allowed to cost anybody a job.
func TestLoweringTheBudgetNeverFailsAJob(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(sizedVMPool(credential, 1, 8, 4, 4096))
	f.setBudget(model.Budget{CPUs: 24})
	f.reconcileNow()
	for i := 0; i < 6; i++ {
		f.busy(f.vm.names()...)
		f.reconcileNow()
	}
	if got := len(f.vm.names()); got != 6 {
		t.Fatalf("the fleet did not reach six: %d", got)
	}

	// Every machine has a job on it, and the host is needed for something else.
	f.busy(f.vm.names()...)
	f.setBudget(model.Budget{CPUs: 8})
	f.reconcileNow()

	// Nothing has gone: they are all still working.
	if got := len(f.vm.names()); got != 6 {
		t.Fatalf("a machine was taken away from a job: %d machines left", got)
	}

	// The jobs end, the machines stop, and only then are they removed.
	f.everyRunnerIdle()
	f.vm.jobsFinish()
	f.reconcileNow()

	if got := len(f.vm.names()); got != 2 {
		t.Fatalf("the fleet is %d machines, want the 2 the lowered budget pays for", got)
	}
}

// The minimums are a promise the budget does not break. A pool scaled to
// nothing could never discover that it needs a runner, so the fleet would not
// be slowed down by the budget — it would be switched off by it.
func TestABudgetTooSmallForTheMinimumsStillLeavesThePoolsAble(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(sizedVMPool(credential, 2, 8, 8, 8192))
	// Sixteen processors promised as a minimum, one available.
	f.setBudget(model.Budget{CPUs: 1})
	f.reconcileNow()

	if got := strings.Join(f.vm.names(), ","); got != "web-1,web-2" {
		t.Fatalf("the fleet is %q, want the pool's minimum of two", got)
	}

	// And the operator is told which of the two problems this is.
	reason := f.reconciler.Scaling()["web"].Reason
	if !strings.Contains(reason, "minimums") {
		t.Fatalf("the pool does not say that its minimums are the problem: %q", reason)
	}
}

// A budget survives a daemon restart: it is in the database, not in the
// process.
func TestTheBudgetSurvivesARestart(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(sizedVMPool(credential, 1, 8, 4, 4096))
	f.setBudget(model.Budget{CPUs: 12, MemoryMB: 32768, CPUWeight: 50})

	f.start()

	if got := f.budget(); got.CPUs != 12 || got.MemoryMB != 32768 || got.CPUWeight != 50 {
		t.Fatalf("the budget came back as %+v", got)
	}
	f.reconcileNow()
	for i := 0; i < 6; i++ {
		f.busy(f.vm.names()...)
		f.reconcileNow()
	}
	if got := len(f.vm.names()); got != 3 {
		t.Fatalf("the restarted daemon grew the fleet to %d, want 3", got)
	}
}

// Container pools are outside the budget, because they are outside the group it
// is enforced in. Charging them against a ceiling they are not subject to would
// make the figure mean two different things at once.
func TestContainerPoolsAreNotRationed(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	containers := map[string]any{
		"name": "ci", "scopeKind": "repository", "scope": "clems4ever/runyard",
		"runtime": "container", "minReplicas": 1, "maxReplicas": 6,
		"cpus": 4, "memoryMb": 4096, "credentialId": credential, "enabled": true,
	}
	f.addPool(containers)
	f.setBudget(model.Budget{CPUs: 4})
	f.reconcileNow()

	for i := 0; i < 6; i++ {
		f.busy(f.containers.names()...)
		f.reconcileNow()
	}

	if got := len(f.containers.names()); got != 6 {
		t.Fatalf("the container pool reached %d, want its own maximum of 6 — the machine"+
			" budget is not its budget", got)
	}
}

// Removing the budget removes the ceiling, and the fleet grows to what its
// pools allow again.
func TestRemovingTheBudgetRemovesTheCeiling(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(sizedVMPool(credential, 1, 8, 4, 4096))
	f.setBudget(model.Budget{CPUs: 8})
	f.reconcileNow()
	for i := 0; i < 4; i++ {
		f.busy(f.vm.names()...)
		f.reconcileNow()
	}
	if got := len(f.vm.names()); got != 2 {
		t.Fatalf("the budget is not holding: %d machines", got)
	}

	f.setBudget(model.Budget{})
	for i := 0; i < 8; i++ {
		f.busy(f.vm.names()...)
		f.reconcileNow()
	}

	if got := len(f.vm.names()); got != 8 {
		t.Fatalf("the fleet reached %d after the budget was removed, want its maximum of 8", got)
	}
}
