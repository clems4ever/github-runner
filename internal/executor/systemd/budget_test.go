package systemd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/paths"
)

// newBudgetedExecutor is newExecutor with the slice written somewhere the test
// owns, so what systemd would read is what the test can read.
func newBudgetedExecutor(t *testing.T) (*Executor, *fakeCommander, string) {
	t.Helper()
	layout := paths.Under(t.TempDir())
	if err := layout.EnsureDirs(paths.CurrentOwner()); err != nil {
		t.Fatal(err)
	}
	slicePath := layout.Etc + "/" + Slice
	cmd := &fakeCommander{output: map[string]string{}}
	e := New(layout, "/usr/local/bin/runner-fleet", "runner-fleet",
		WithCommander(cmd), WithUnitPath(layout.Etc+"/gh-runner@.service"),
		WithSlicePath(slicePath))
	return e, cmd, slicePath
}

func applied(t *testing.T, e *Executor, path string, budget model.Budget) string {
	t.Helper()
	if err := e.ApplyBudget(context.Background(), budget); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no slice was written: %v", err)
	}
	return string(body)
}

// Every machine goes into the group whether or not there is a budget, and this
// is the assertion that matters most in this file.
//
// A unit joins its slice when it starts. If membership were conditional on a
// budget existing, then setting one would mean draining every machine on the
// host before it meant anything — on a host that had just been told it was
// using too much, which is the worst possible moment to be replacing a fleet.
// Grouping unconditionally makes setting a budget a property change on a group
// that already holds the runners.
func TestEveryMachineIsInTheFleetsGroupBudgetOrNot(t *testing.T) {
	e, _, _ := newExecutor(t)
	unit := e.renderUnit()

	if !strings.Contains(unit, "Slice="+Slice) {
		t.Fatalf("the runners are not in the fleet's group:\n%s", unit)
	}
	// And there is exactly one, or two runners would be in two groups and
	// neither would be the fleet.
	if strings.Count(unit, "Slice=") != 1 {
		t.Errorf("the unit names more than one slice:\n%s", unit)
	}
}

// The daemon is not in the group, and must never be. A fleet pressed against
// its memory ceiling would otherwise be able to take down the only thing an
// operator could use to raise it.
func TestTheDaemonIsNotInTheFleetsGroup(t *testing.T) {
	unit, err := os.ReadFile("../../../packaging/runner-fleetd.service")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unit), Slice) {
		t.Fatal("the daemon put itself inside the budget it enforces")
	}
}

// A host with no budget still gets a slice: it is where the fleet lives, the
// accounting on it is what the figures come from, and having it there means a
// budget set later changes a property rather than moving anything.
func TestAnUncappedFleetStillHasItsGroup(t *testing.T) {
	e, _, path := newBudgetedExecutor(t)
	body := applied(t, e, path, model.Budget{})

	for _, want := range []string{"[Slice]", "CPUAccounting=yes", "MemoryAccounting=yes"} {
		if !strings.Contains(body, want) {
			t.Errorf("the slice is missing %q:\n%s", want, body)
		}
	}
	for _, forbidden := range []string{"CPUQuota=", "MemoryHigh=", "MemoryMax=", "CPUWeight="} {
		if strings.Contains(body, forbidden) {
			t.Errorf("a fleet with no budget was capped with %q:\n%s", forbidden, body)
		}
	}
}

func TestACapBecomesTheGroupsLimits(t *testing.T) {
	e, _, path := newBudgetedExecutor(t)
	body := applied(t, e, path, model.Budget{CPUs: 6, MemoryMB: 24576})

	// Six processors across the whole fleet is 600% of one.
	if !strings.Contains(body, "CPUQuota=600%") {
		t.Errorf("six processors did not become a quota:\n%s", body)
	}
	if !strings.Contains(body, "MemoryHigh=24576M") {
		t.Errorf("the memory ceiling is not on the group:\n%s", body)
	}
}

// Pressure by default, and no wall. The kernel's out-of-memory killer picks the
// largest machine in the group rather than the one that overspent, so the
// default answer to a fleet at its ceiling is a slower fleet, not a failed job
// belonging to somebody who did nothing wrong.
func TestTheDefaultMemoryLimitSlowsTheFleetRatherThanKillingAJob(t *testing.T) {
	e, _, path := newBudgetedExecutor(t)
	body := applied(t, e, path, model.Budget{MemoryMB: 8192})

	if !strings.Contains(body, "MemoryHigh=8192M") {
		t.Errorf("no ceiling was written:\n%s", body)
	}
	if strings.Contains(body, "MemoryMax=") {
		t.Fatalf("a memory budget brought the out-of-memory killer with it:\n%s", body)
	}
}

// And the wall when it is asked for, above the ceiling rather than on it, so
// the reclaim has somewhere to happen first.
func TestAHardLimitSitsAboveTheCeiling(t *testing.T) {
	e, _, path := newBudgetedExecutor(t)
	body := applied(t, e, path, model.Budget{MemoryMB: 10000, HardMemory: true})

	if !strings.Contains(body, "MemoryHigh=10000M") {
		t.Errorf("the ceiling is missing:\n%s", body)
	}
	if !strings.Contains(body, "MemoryMax=10500M") {
		t.Errorf("the wall is not five per cent above the ceiling:\n%s", body)
	}
}

// A weight is a different question from a cap, and both can be set: the fleet
// is held to four processors, and yields even those to a busier neighbour.
func TestAWeightAndACapAreBothWritten(t *testing.T) {
	e, _, path := newBudgetedExecutor(t)
	body := applied(t, e, path, model.Budget{CPUs: 4, CPUWeight: 20})

	if !strings.Contains(body, "CPUQuota=400%") || !strings.Contains(body, "CPUWeight=20") {
		t.Fatalf("a fleet that is both capped and polite:\n%s", body)
	}
}

// A weight on its own is legitimate: the fleet may use the whole host, and
// gives way the moment anything else wants it.
func TestAWeightWithoutACapIsAllowed(t *testing.T) {
	e, _, path := newBudgetedExecutor(t)
	body := applied(t, e, path, model.Budget{CPUWeight: 20})

	if !strings.Contains(body, "CPUWeight=20") {
		t.Errorf("the weight was not written:\n%s", body)
	}
	if strings.Contains(body, "CPUQuota=") {
		t.Errorf("a weight became a cap:\n%s", body)
	}
}

// The budget is applied on every reconcile pass, so an unchanged one has to
// cost a file read and nothing else. A daemon-reload every thirty seconds for
// ever is how a quiet host ends up with a noisy journal.
func TestReapplyingAnUnchangedBudgetIsSilent(t *testing.T) {
	e, cmd, path := newBudgetedExecutor(t)
	budget := model.Budget{CPUs: 4, MemoryMB: 8192}

	applied(t, e, path, budget)
	if !cmd.called("systemctl daemon-reload") {
		t.Fatal("systemd was never told to read the new slice")
	}

	cmd.calls = nil
	applied(t, e, path, budget)
	if len(cmd.calls) != 0 {
		t.Fatalf("an unchanged budget reloaded systemd anyway: %v", cmd.calls)
	}
}

// And a changed one does reload, which is what makes a budget set in the UI
// reach the machines that are already running rather than the next ones.
func TestChangingTheBudgetReachesTheFleetThatIsRunning(t *testing.T) {
	e, cmd, path := newBudgetedExecutor(t)
	applied(t, e, path, model.Budget{CPUs: 8, MemoryMB: 32768})

	cmd.calls = nil
	body := applied(t, e, path, model.Budget{CPUs: 2, MemoryMB: 4096})

	if !cmd.called("systemctl daemon-reload") {
		t.Fatal("the fleet was left under the old budget until something restarted")
	}
	if !strings.Contains(body, "CPUQuota=200%") || strings.Contains(body, "CPUQuota=800%") {
		t.Fatalf("the old limits are still on the group:\n%s", body)
	}
	// Nothing was drained to make that happen. Limits are properties of a group
	// that already holds the fleet.
	for _, call := range cmd.calls {
		if strings.Contains(call, "stop") || strings.Contains(call, "restart") {
			t.Fatalf("lowering the budget touched the machines: %v", cmd.calls)
		}
	}
}

// Removing a cap has to remove it. A budget file that only ever gained lines
// would leave a ceiling behind that nobody could find in the UI.
func TestRemovingACapRemovesIt(t *testing.T) {
	e, _, path := newBudgetedExecutor(t)
	applied(t, e, path, model.Budget{CPUs: 4, MemoryMB: 8192, HardMemory: true})

	body := applied(t, e, path, model.Budget{})
	for _, forbidden := range []string{"CPUQuota=", "MemoryHigh=", "MemoryMax="} {
		if strings.Contains(body, forbidden) {
			t.Errorf("%q outlived the budget it came from:\n%s", forbidden, body)
		}
	}
}

// A budget that could only be a mistake is refused before it reaches the host,
// where it would be a ceiling no machine could boot inside.
func TestANonsenseBudgetNeverReachesTheHost(t *testing.T) {
	e, cmd, path := newBudgetedExecutor(t)

	if err := e.ApplyBudget(context.Background(), model.Budget{MemoryMB: 8}); err == nil {
		t.Fatal("half a megabyte of fleet was written to the host")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a slice was written for a budget that was refused")
	}
	if len(cmd.calls) != 0 {
		t.Errorf("systemd was involved anyway: %v", cmd.calls)
	}
}

// The migration nobody would otherwise be told about.
//
// A unit joins its slice when it starts, so every machine that was already
// running when the daemon first wrote the slice stays outside it until it is
// replaced. On an ephemeral pool that is a job or two; on an idle fixed one it
// is indefinite — and that is exactly the case where somebody would be reading
// a ceiling that half the fleet is not subject to.
func TestAMachineOutsideTheGroupIsReported(t *testing.T) {
	e, cmd, path := newBudgetedExecutor(t)
	if err := e.Create(context.Background(), testSpec("web-1")); err != nil {
		t.Fatal(err)
	}
	applied(t, e, path, model.Budget{CPUs: 4, MemoryMB: 8192})

	// Still in system.slice, which is where it was started.
	cmd.output["systemctl show --timestamp=unix --property=Id,MemoryCurrent"] =
		"Id=gh-runner@web-1.service\nMemoryCurrent=1000\nCPUUsageNSec=1\n" +
			"ControlGroup=/system.slice/gh-runner@web-1.service\n"

	usage, err := e.Usage(context.Background())
	if err == nil {
		t.Fatal("a machine spending on top of the budget rather than out of it was not mentioned")
	}
	if !strings.Contains(err.Error(), "web-1") {
		t.Errorf("the warning does not say which machine: %v", err)
	}
	// Warned about, not hidden: it is still a runner, and it is still using
	// what it is using.
	if len(usage) != 1 || usage[0].MemoryBytes != 1000 {
		t.Fatalf("the machine fell off the page: %+v", usage)
	}
}

func TestAMachineInsideTheGroupIsNotReported(t *testing.T) {
	e, cmd, path := newBudgetedExecutor(t)
	if err := e.Create(context.Background(), testSpec("web-1")); err != nil {
		t.Fatal(err)
	}
	applied(t, e, path, model.Budget{CPUs: 4, MemoryMB: 8192})

	cmd.output["systemctl show --timestamp=unix --property=Id,MemoryCurrent"] =
		"Id=gh-runner@web-1.service\nMemoryCurrent=1000\nCPUUsageNSec=1\n" +
			"ControlGroup=/" + Slice + "/gh-runner@web-1.service\n"

	if _, err := e.Usage(context.Background()); err != nil {
		t.Fatalf("a machine in the fleet's own group was reported as outside it: %v", err)
	}
}

// Without a cap there is nothing to be outside of, and saying so would put a
// warning on every host that has never set a budget — which is all of them,
// until somebody does.
func TestNoCapMeansNoMigrationWarning(t *testing.T) {
	e, cmd, path := newBudgetedExecutor(t)
	if err := e.Create(context.Background(), testSpec("web-1")); err != nil {
		t.Fatal(err)
	}
	applied(t, e, path, model.Budget{})

	cmd.output["systemctl show --timestamp=unix --property=Id,MemoryCurrent"] =
		"Id=gh-runner@web-1.service\nMemoryCurrent=1000\nCPUUsageNSec=1\n" +
			"ControlGroup=/system.slice/gh-runner@web-1.service\n"

	if _, err := e.Usage(context.Background()); err != nil {
		t.Fatalf("an uncapped host was warned about its own fleet: %v", err)
	}
}

// A stopped unit has no group, and systemd says so by printing "[not set]".
// That is not a machine escaping the budget; it is a machine that is not there.
func TestAStoppedMachineIsNotAnEscapee(t *testing.T) {
	e, cmd, path := newBudgetedExecutor(t)
	if err := e.Create(context.Background(), testSpec("web-1")); err != nil {
		t.Fatal(err)
	}
	applied(t, e, path, model.Budget{CPUs: 4})

	cmd.output["systemctl show --timestamp=unix --property=Id,MemoryCurrent"] =
		"Id=gh-runner@web-1.service\nMemoryCurrent=[not set]\nCPUUsageNSec=[not set]\nControlGroup=[not set]\n"

	if _, err := e.Usage(context.Background()); err != nil {
		t.Fatalf("a stopped machine was reported as outside the budget: %v", err)
	}
}

// The slice and the template unit are two files and one reload each, and the
// template unit must not be rewritten just because the budget changed: that
// would be a daemon-reload on a unit nobody touched, every time somebody moved
// a slider.
func TestTheBudgetDoesNotRewriteTheRunnersUnit(t *testing.T) {
	e, _, path := newBudgetedExecutor(t)
	ctx := context.Background()
	if err := e.EnsureUnit(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(e.unitPath)
	if err != nil {
		t.Fatal(err)
	}

	applied(t, e, path, model.Budget{CPUs: 4, MemoryMB: 8192})

	after, err := os.ReadFile(e.unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("changing the budget rewrote the unit every runner is an instance of")
	}
}

// systemd itself is the only authority on whether these files are valid, and
// the mistakes worth catching here are exactly the ones a string comparison
// cannot see: a directive that does not exist, a suffix parsed as a different
// unit, a section systemd will not read on a slice.
//
// Skipped where there is no systemd-analyze, which is most developer machines
// and no CI runner this project uses.
func TestSystemdAcceptsWhatIsWrittenForIt(t *testing.T) {
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("no systemd-analyze on this host")
	}

	e, _, _ := newExecutor(t)
	for name, body := range map[string]string{
		Slice: renderSlice(model.Budget{CPUs: 6, CPUWeight: 50, MemoryMB: 10000, HardMemory: true}),
		// The uncapped slice is not the same file and is what every host has
		// until somebody sets a budget.
		"uncapped/" + Slice:       renderSlice(model.Budget{}),
		UnitTemplate + ".service": e.renderUnit(),
	} {
		dir := filepath.Join(t.TempDir(), filepath.Dir(name))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, filepath.Base(name))
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}

		out, err := exec.Command(analyze, "verify", path).CombinedOutput()
		// systemd-analyze complains about things that are not this file's
		// problem — a binary that is not on this host, a user that does not
		// exist — and says so on standard error while exiting zero. Only the
		// lines about directives and sections are worth failing over.
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "Unknown key") || strings.Contains(line, "Unknown section") ||
				strings.Contains(line, "Failed to parse") || strings.Contains(line, "Invalid") {
				t.Errorf("%s: systemd will not read %q\n%s", filepath.Base(name), line, body)
			}
		}
		if err != nil && strings.Contains(string(out), "Failed to prepare filename") {
			t.Errorf("%s: %v\n%s", filepath.Base(name), err, out)
		}
	}
}
