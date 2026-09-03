package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// vmDir puts a machine's working directory on a fake host.
func vmDir(t *testing.T, state, runner string) string {
	t.Helper()
	dir := filepath.Join(state, "vms", runner)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disk.qcow2"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func nothingKnown(string) bool    { return false }
func everythingKnown(string) bool { return true }
func nothingBusy(string) bool     { return false }

// The leak this exists for: a machine that was killed rather than stopped
// leaves its overlay behind, and its runner is long gone.
func TestSweepDeletesTheDirectoryOfARunnerThatIsGone(t *testing.T) {
	state := t.TempDir()
	vmDir(t, state, "web-1")

	swept, freed, err := SweepVMDirs(state, nothingKnown, nothingBusy)
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 1 || swept[0] != "web-1" {
		t.Fatalf("swept %v, want web-1", swept)
	}
	if freed == 0 {
		t.Fatal("freed nothing, want the overlay's size")
	}
	if _, err := os.Stat(filepath.Join(state, "vms", "web-1")); !os.IsNotExist(err) {
		t.Fatal("the directory is still there")
	}
}

// A runner that still exists is running, or is about to be. Deleting the disk
// under it is the bug, not the fix.
func TestSweepKeepsTheDirectoryOfARunnerThatExists(t *testing.T) {
	state := t.TempDir()
	vmDir(t, state, "web-1")

	swept, _, err := SweepVMDirs(state, everythingKnown, nothingBusy)
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 0 {
		t.Fatalf("swept %v, want nothing", swept)
	}
	if _, err := os.Stat(filepath.Join(state, "vms", "web-1")); err != nil {
		t.Fatal("deleted a live runner's directory")
	}
}

// The gap the environment file cannot cover: the daemon went down between
// asking a machine to stop and seeing it stop, so the runner is unknown and a
// QEMU is still draining a job in there.
func TestSweepKeepsADirectorySomethingStillHasOpen(t *testing.T) {
	state := t.TempDir()
	vmDir(t, state, "web-1")

	swept, _, err := SweepVMDirs(state, nothingKnown, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 0 {
		t.Fatalf("swept %v while something had it open", swept)
	}
}

// A state directory with no machines in it yet is the normal case on a fresh
// host, and is not an error.
func TestSweepIsQuietWithNothingToSweep(t *testing.T) {
	swept, freed, err := SweepVMDirs(t.TempDir(), nothingKnown, nothingBusy)
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 0 || freed != 0 {
		t.Fatalf("swept %v freeing %d on an empty host", swept, freed)
	}
}

// InUseByProcess is the real thing, asked about a file this test is holding
// open. It is the guard the sweep leans on, so it is worth one test that does
// not mock it.
func TestInUseByProcessSeesAnOpenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "disk.qcow2")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if !InUseByProcess(dir) {
		t.Fatal("did not see a file this test has open")
	}

	other := t.TempDir()
	if InUseByProcess(other) {
		t.Fatal("reported a directory nobody has open as busy")
	}
}
