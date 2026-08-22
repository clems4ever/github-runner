package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
