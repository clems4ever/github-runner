package agent

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/clems4ever/github-runner/internal/paths"
)

// A machine deletes its own working directory when it stops — `defer
// os.RemoveAll` in runVM, which is the only thing that ever did. A deferred
// call does not run when the process is killed, so a machine that was SIGKILLed,
// OOM-killed, or lost to a host crash leaves its overlay behind for ever.
// Nothing swept them, and the executor's Remove deletes the runner's
// environment file and nothing else.
//
// So the daemon sweeps at startup. The environment files are the enumeration
// of what this host is responsible for — written before a unit starts and
// removed after it is gone — so a working directory with no environment file
// beside it belongs to a runner that no longer exists.

// SweepVMDirs deletes the working directories of machines that are gone.
//
// known is asked whether a runner still exists, which the caller answers from
// the environment files rather than from systemd: a unit that failed is still a
// runner this daemon is responsible for, and its disk is not garbage.
//
// busy is asked whether anything still has the directory open. It is the guard
// against the one case the environment file cannot cover — a daemon that went
// down between asking a machine to stop and seeing it stop, leaving a runner
// with no environment file and a QEMU still draining a job inside it.
//
// It returns what it deleted and how much that freed, for the log. A directory
// that cannot be deleted is reported and skipped rather than failing the sweep:
// one unreadable directory must not stop the rest from being reclaimed.
func SweepVMDirs(stateDir string, known func(runner string) bool, busy func(dir string) bool) (
	swept []string, freed int64, err error) {

	vms := filepath.Join(stateDir, "vms")
	entries, err := os.ReadDir(vms)
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}

	var problems []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runner := entry.Name()
		if known(runner) {
			continue
		}
		dir := filepath.Join(vms, runner)
		if busy != nil && busy(dir) {
			// Something is still running in there. It has no environment file,
			// so it is on its way out — but a job may still be in it, and this
			// is not the thing that cuts one short.
			continue
		}
		size := dirSize(dir)
		if err := os.RemoveAll(dir); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", runner, err))
			continue
		}
		swept = append(swept, runner)
		freed += size
	}

	if len(problems) > 0 {
		return swept, freed, fmt.Errorf("could not delete %s", strings.Join(problems, "; "))
	}
	return swept, freed, nil
}

// dirSize is what a directory occupies on disk. Best effort: a sweep reports
// how much it freed, and a number that could not be measured is worth less
// than the reclaimed space is worth having.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if info, err := entry.Info(); err == nil {
			total += paths.OnDisk(info)
		}
		return nil
	})
	return total
}

// InUseByProcess reports whether any process has a file open under a
// directory.
//
// Read from /proc rather than by asking for a lock or running lsof: the
// question is "is a QEMU still alive in here", the answer is in the kernel,
// and a daemon that shelled out for it would be one more thing to install.
func InUseByProcess(dir string) bool {
	target, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	procs, err := os.ReadDir("/proc")
	if err != nil {
		// No /proc is no answer, and no answer is treated as "busy": the cost
		// of guessing wrong the other way is somebody's job.
		return true
	}
	for _, proc := range procs {
		if !proc.IsDir() || proc.Name()[0] < '0' || proc.Name()[0] > '9' {
			continue
		}
		fds := filepath.Join("/proc", proc.Name(), "fd")
		open, err := os.ReadDir(fds)
		if err != nil {
			// Almost always a process this daemon may not look at, or one that
			// exited while being read. Neither is evidence of anything.
			continue
		}
		for _, fd := range open {
			path, err := os.Readlink(filepath.Join(fds, fd.Name()))
			if err != nil {
				continue
			}
			if path == target || strings.HasPrefix(path, target+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}
