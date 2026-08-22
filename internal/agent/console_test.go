package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clems4ever/github-runner/internal/model"
)

// deprecatedConsole is the real thing, from a host where every machine had been
// booting and powering itself off twice a minute for hours. The fleet showed a
// runner in perfect health throughout.
const deprecatedConsole = `[  OK  ] Started github-runner.service - GitHub Actions self-hosted runner.
[    8.708344] run-runner.sh[1343]: registering runner 'ci-vm-1' on https://github.com/clems4ever/github-runner
[    9.004386] run-runner.sh[1405]: # Authentication
[   11.261525] run-runner.sh[1405]: √ Connected to GitHub
[   11.877782] run-runner.sh[1405]: √ Successfully replaced the runner
[   13.885720] run-runner.sh[1435]: √ Connected to GitHub
[   14.845087] run-runner.sh[1435]: 2026-08-22 13:46:17Z: Listening for Jobs
[   15.154721] run-runner.sh[1435]: An error occured: Runner version v2.330.0 is deprecated and cannot receive messages.
[   15.170375] run-runner.sh[1431]: Runner listener exit with terminated error, stop the service, no retry needed.
[   15.170547] run-runner.sh[1343]: Exiting runner...
[  OK  ] Stopped github-runner.service - GitHub Actions self-hosted runner.
[  OK  ] Reached target poweroff.target - System Power Off.
`

func writeConsole(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "console.log")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A runner that took a job and finished is the ordinary case, and must not be
// reported as a failure however quickly it happened.
func TestAMachineThatDidItsWorkIsNotAFailure(t *testing.T) {
	console := writeConsole(t, strings.Join([]string{
		"[   14.8] run-runner.sh[1435]: 2026-08-22 13:46:17Z: " + listening,
		"[   99.1] run-runner.sh[1435]: Running job: build",
		"[  120.4] run-runner.sh[1435]: Job build completed with result: Succeeded",
	}, "\n"))

	if got := whyItCouldNotWork(console); got != "" {
		t.Fatalf("a runner that worked was reported as broken: %q", got)
	}
}

// A machine with no console to read says nothing rather than inventing a
// failure: a healthy fleet must not report problems it cannot support.
func TestNoConsoleIsNotAFailure(t *testing.T) {
	if got := whyItCouldNotWork(filepath.Join(t.TempDir(), "missing")); got != "" {
		t.Fatalf("got %q", got)
	}
}

// The failure this exists for. What comes back has to be the runner's own
// words, because "the machine powered off" is true of every case and useful in
// none of them.
func TestADeprecatedRunnerIsReportedWithWhatItSaid(t *testing.T) {
	problem := whyItCouldNotWork(writeConsole(t, deprecatedConsole))
	if problem == "" {
		t.Fatal("a machine whose runner was refused by GitHub reported as healthy")
	}
	if !strings.Contains(problem, "deprecated") {
		t.Fatalf("the reason is not in the message:\n%s", problem)
	}
	// The runner's lines, not the operating system booting around them.
	if strings.Contains(problem, "OK") || strings.Contains(problem, "poweroff.target") {
		t.Errorf("the message carries the shutdown log rather than the runner:\n%s", problem)
	}
}

// "Listening for Jobs" appears in that console too — the runner reached it and
// was then refused. Reaching it is not the same as being able to work, so the
// last word wins.
func TestListeningIsNotEnoughIfItWasThenRefused(t *testing.T) {
	if !strings.Contains(deprecatedConsole, listening) {
		t.Fatal("this test is checking something that is not in the fixture")
	}
	if whyItCouldNotWork(writeConsole(t, deprecatedConsole)) == "" {
		t.Fatal("a runner that was refused after connecting reported as healthy")
	}
}

func TestAConsoleWithNothingFromTheRunnerSaysSo(t *testing.T) {
	console := writeConsole(t, "[  OK  ] Reached target multi-user.target\n[  OK  ] Reached target poweroff.target\n")
	problem := whyItCouldNotWork(console)
	if !strings.Contains(problem, "nothing from the runner") {
		t.Fatalf("got %q", problem)
	}
}

// The console is deleted with the machine, which is exactly when it becomes
// worth reading. It is kept per runner, and the last boot is the one that
// matters.
func TestTheConsoleOutlivesTheMachine(t *testing.T) {
	state := t.TempDir()
	vmDir := filepath.Join(state, "vms", "ci-vm-1")
	if err := os.MkdirAll(vmDir, 0o700); err != nil {
		t.Fatal(err)
	}
	console := filepath.Join(vmDir, "console.log")
	if err := os.WriteFile(console, []byte(deprecatedConsole), 0o600); err != nil {
		t.Fatal(err)
	}

	kept := keepConsole(state, "ci-vm-1", console)

	// The machine goes away, as it does on every boot.
	if err := os.RemoveAll(vmDir); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(kept)
	if err != nil {
		t.Fatalf("the console did not outlive the machine: %v", err)
	}
	if !strings.Contains(string(raw), "deprecated") {
		t.Fatalf("what was kept is not the console:\n%s", raw)
	}
	if !strings.Contains(kept, "ci-vm-1") {
		t.Errorf("the kept console is not named after its runner: %s", kept)
	}

	// A second boot replaces it rather than piling up.
	if err := os.MkdirAll(vmDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(console, []byte("a later boot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if again := keepConsole(state, "ci-vm-1", console); again != kept {
		t.Fatalf("a second boot kept its console somewhere else: %s then %s", kept, again)
	}
	raw, _ = os.ReadFile(kept)
	if string(raw) != "a later boot" {
		t.Fatalf("the kept console is not the last one: %q", raw)
	}
}

// The version in the image has an expiry date that somebody else sets. These
// two are what stop that being a silent outage: a runner allowed to update
// itself when the image is stale, and a version that is kept current.
func TestAMachineRunnerMayUpdateItself(t *testing.T) {
	if strings.Contains(GuestRunnerScript, "--disableupdate") {
		t.Error("a machine's runner cannot update itself, and a golden image built months ago" +
			" carries a runner GitHub will refuse to give work to")
	}
	// The version has to be one somebody keeps current, so it is named here:
	// a bump is a deliberate act, and the weekly workflow opens the pull
	// request that prompts it.
	if RunnerVersion < "2.336.0" {
		t.Errorf("the image carries runner %s, and GitHub deprecates old ones server-side", RunnerVersion)
	}
}

// A workflow written for a GitHub-hosted runner assumes passwordless sudo, and
// most of them use it: installing a package, writing outside the workspace,
// starting a service. Without it the job fails with "runner is not in the
// sudoers file", which reads like the workflow's fault and is not.
func TestAJobCanSudoInsideTheMachine(t *testing.T) {
	script := provisionScript()

	if !strings.Contains(script, "runner ALL=(ALL) NOPASSWD:ALL") {
		t.Fatal("the runner user has no sudo, and every job that needs root fails")
	}
	if !strings.Contains(script, "/etc/sudoers.d/runner") {
		t.Error("the rule is not in sudoers.d, so it will not survive an update of /etc/sudoers")
	}
	// 0440, because sudo refuses to read a sudoers file anybody can write.
	if !strings.Contains(script, "chmod 0440 /etc/sudoers.d/runner") {
		t.Error("the sudoers file is not 0440 and sudo will refuse it")
	}
	// A malformed sudoers file locks sudo for everybody, so the build checks it
	// rather than every job discovering it.
	if !strings.Contains(script, "visudo -c -f /etc/sudoers.d/runner") {
		t.Error("the sudoers file is never validated")
	}
}

// Changing what a machine is made of has to replace the machines that were
// made of the old thing. The generation covers what an operator configured, not
// what the daemon does with it, so this constant is the only thing that can.
func TestTheSpecRevisionMovedWithTheImage(t *testing.T) {
	if model.SpecRevision < 4 {
		t.Fatalf("spec revision %d: machines built before sudo and the runner bump are still"+
			" wanted, so nothing replaces them", model.SpecRevision)
	}
}

// The bug this pair of tests exists for: revision 3 gave jobs passwordless sudo
// and was installed on a host that went on booting machines without it, because
// the change was to the build script and the image's name did not depend on the
// build script. The fix shipped, and did nothing.
func TestTheImageNameCoversTheScriptThatBuildsIt(t *testing.T) {
	spec := ImageSpec{Variant: "default"}
	before := spec.Name()

	original := provision
	t.Cleanup(func() { provision = original })
	provision = func() string { return original() + "\necho 'something new at build time'\n" }

	if after := spec.Name(); after == before {
		t.Fatalf("changing what the build does left the image called %s, so every host"+
			" would keep the image it already has", after)
	}
}

// And the other half: the same recipe has to produce the same name, or every
// daemon restart would build an image nobody asked for.
func TestTheSameRecipeIsTheSameImage(t *testing.T) {
	one := ImageSpec{Variant: "default"}
	same := ImageSpec{Variant: "default"}
	if one.Name() != same.Name() {
		t.Fatal("the same spec named two images")
	}
}

// A machine boots for one job and is destroyed, so every second of the boot is
// paid by every job on the host. These are the services a stock cloud image
// starts that a runner has no use for.
func TestTheImageDoesNotBootThingsARunnerWillNeverUse(t *testing.T) {
	script := provisionScript()

	for _, useless := range []string{"snapd.service", "ModemManager.service", "multipathd.service", "apt-daily.timer"} {
		if !strings.Contains(script, useless) {
			t.Errorf("%s still starts on every boot, and no job will ever ask for it", useless)
		}
	}
	// Disabled, not masked: a job that genuinely wants one can start it.
	if strings.Contains(script, "systemctl mask") {
		t.Error("services are masked rather than disabled, so a job cannot start one if it needs it")
	}
	// And the things a job does need are untouched.
	for _, needed := range []string{"systemctl enable docker"} {
		if !strings.Contains(script, needed) {
			t.Errorf("%q is missing from the image", needed)
		}
	}
}

// The runner's last words, which are the point at which its machine has nothing
// left to do. Taken from the console of a machine that then sat for eighteen
// minutes with no runner on it, holding a slot in a pool with twelve jobs
// queued.
func TestARunnerThatHasFinishedIsRecognised(t *testing.T) {
	done := writeConsole(t, strings.Join([]string{
		"[   54.7] run-runner.sh[1431]: 2026-08-22 15:04:38Z: Job installer completed with result: Succeeded",
		"[   55.1] run-runner.sh[1431]: √ Removed .credentials",
		"[   55.1] run-runner.sh[1431]: √ Removed .runner",
		"[   55.2] run-runner.sh[1427]: Runner listener exit with 0 return code, stop the service, no retry needed.",
		"[   55.2] run-runner.sh[1339]: Exiting runner...",
	}, "\n"))
	if !runnerFinished(done) {
		t.Fatal("a machine whose runner has gone is not recognised, so nothing will ever stop it")
	}

	// A runner waiting for work has not finished, and its machine must not be
	// stopped underneath it.
	waiting := writeConsole(t, "[   16.0] run-runner.sh[1436]: 2026-08-22 13:46:17Z: Listening for Jobs")
	if runnerFinished(waiting) {
		t.Fatal("an idle runner was taken for a finished one")
	}

	// Nor mid-job.
	working := writeConsole(t, strings.Join([]string{
		"[   16.0] run-runner.sh[1436]: Listening for Jobs",
		"[   18.1] run-runner.sh[1436]: Running job: installer",
	}, "\n"))
	if runnerFinished(working) {
		t.Fatal("a runner with a job on it was taken for a finished one")
	}

	// A console that cannot be read says nothing rather than stopping a machine
	// on a guess.
	if runnerFinished(filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("a missing console was taken as proof the runner had finished")
	}
}
