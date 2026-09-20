package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The runner's account is read off the runner itself rather than assumed.
//
// The official image uses runner:docker at 1001:123 and an image somebody
// built themselves uses whatever they chose, which is the same reason the
// runner is found by looking for config.sh instead of by path.
func TestRunnerAccountComesFromTheRunner(t *testing.T) {
	home := t.TempDir()
	acct, err := runnerAccount(home)
	if err != nil {
		t.Fatal(err)
	}
	if acct.uid != uint32(os.Getuid()) || acct.gid != uint32(os.Getgid()) {
		t.Fatalf("account is %d:%d, want the owner of %s", acct.uid, acct.gid, home)
	}
}

// Nothing is dropped to when the agent is not root, which is every container
// that has no daemon in it: the image's own USER is already in effect.
func TestRunnerAccountDoesNotSwitchWhenItIsNotRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this says something only when the test is not root")
	}
	acct, err := runnerAccount(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if acct.switching {
		t.Fatal("an agent that is not root tried to change user, which it cannot do")
	}
}

func TestRunnerAccountSaysWhichDirectoryItCouldNotRead(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-here")
	_, err := runnerAccount(missing)
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("error was %v, and has to name what it looked at", err)
	}
}

// HOME is replaced rather than appended.
//
// The agent's own is root's, the runner inherits the whole environment, and a
// second HOME in it is resolved by whoever reads it first. A runner that wrote
// its credentials and its work directory into /root would work until the first
// job that looked in the home directory it claimed to have.
func TestHomeIsReplacedNotAppended(t *testing.T) {
	env := withEnv([]string{"PATH=/usr/bin", "HOME=/root", "LANG=C"}, "HOME", "/home/runner")

	var homes []string
	for _, entry := range env {
		if strings.HasPrefix(entry, "HOME=") {
			homes = append(homes, entry)
		}
	}
	if len(homes) != 1 || homes[0] != "HOME=/home/runner" {
		t.Fatalf("HOME entries are %v", homes)
	}
	if len(env) != 3 {
		t.Fatalf("the rest of the environment was not kept: %v", env)
	}
}

// An image that cannot run a daemon is refused before the runner registers,
// with the fix in the message.
//
// dockerd without iptables dies several seconds in, inside a message about a
// network controller, and a pool whose image is wrong then looks like a pool
// whose runners restart for no reason.
func TestDockerPresentNamesWhatIsMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := dockerPresent()
	if err == nil {
		t.Fatal("an image with no daemon in it was accepted")
	}
	if !strings.Contains(err.Error(), "dockerd") || !strings.Contains(err.Error(), "images/dind") {
		t.Fatalf("error was %q, and has to say what is missing and where the fix is", err)
	}
}

func TestDockerPresentAcceptsAnImageThatHasBoth(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"dockerd", "iptables"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	if err := dockerPresent(); err != nil {
		t.Fatal(err)
	}
}

// Cgroup v2 has a rule with no exceptions: a cgroup may hold processes, or it
// may have controllers enabled for its children, but not both. A container's
// namespace root holds every process in the container, so a daemon inside it
// cannot create a container with `cpu` or `memory` on it — which arrives as
// "cannot enter cgroupv2 ... it is in threaded mode", naming a cgroup rather
// than the rule it broke.
func TestNestingEmptiesTheRootAndDelegatesTheControllers(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "cgroup.controllers"), "cpuset cpu io memory pids")
	write(t, filepath.Join(root, "cgroup.procs"), "1\n42\n99\n")
	write(t, filepath.Join(root, "cgroup.subtree_control"), "")

	withCgroupRoot(t, root)
	if err := nestCgroups(quietLogger()); err != nil {
		t.Fatalf("nesting: %v", err)
	}

	moved := read(t, filepath.Join(root, "init", "cgroup.procs"))
	for _, pid := range []string{"1", "42", "99"} {
		if !strings.Contains(moved, pid) {
			t.Errorf("pid %s was left in the root, which is what stops the daemon "+
				"delegating anything: %q", pid, moved)
		}
	}

	enabled := read(t, filepath.Join(root, "cgroup.subtree_control"))
	for _, controller := range []string{"+cpu", "+memory", "+pids"} {
		if !strings.Contains(enabled, controller) {
			t.Errorf("%s was not delegated to this container's children: %q", controller, enabled)
		}
	}
}

// A host on cgroup v1 has none of this and never had the problem. It must not
// become an error on the way past.
func TestNestingIsANoOpOnCgroupV1(t *testing.T) {
	root := t.TempDir() // no cgroup.controllers: this is v1
	withCgroupRoot(t, root)
	if err := nestCgroups(quietLogger()); err != nil {
		t.Fatalf("a cgroup v1 host was refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "init")); !os.IsNotExist(err) {
		t.Error("a cgroup v1 host had a leaf made in it")
	}
}

// A root that is already empty is one somebody else nested, or a runner
// restarted in place. There is nothing to move and the delegation still has to
// happen.
func TestNestingAnEmptyRootStillDelegates(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "cgroup.controllers"), "cpu memory")
	write(t, filepath.Join(root, "cgroup.procs"), "")
	write(t, filepath.Join(root, "cgroup.subtree_control"), "")

	withCgroupRoot(t, root)
	if err := nestCgroups(quietLogger()); err != nil {
		t.Fatalf("nesting an empty root: %v", err)
	}
	if got := read(t, filepath.Join(root, "cgroup.subtree_control")); !strings.Contains(got, "+cpu") {
		t.Errorf("an already-nested root was not delegated: %q", got)
	}
}

// The failure a job would otherwise see is a container that will not start, so
// the one this reports instead has to say what was being attempted.
func TestNestingSaysWhatItCouldNotDo(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "cgroup.controllers"), "cpu")
	write(t, filepath.Join(root, "cgroup.procs"), "")
	// No subtree_control to write to, and the directory is read-only.
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	withCgroupRoot(t, root)
	err := nestCgroups(quietLogger())
	if err == nil {
		t.Fatal("a delegation that could not be written was reported as having worked")
	}
	if !strings.Contains(err.Error(), "+cpu") || !strings.Contains(err.Error(), "start containers") {
		t.Errorf("the error does not say what was being attempted: %v", err)
	}
}

func withCgroupRoot(t *testing.T, dir string) {
	t.Helper()
	was := cgroupRoot
	cgroupRoot = dir
	t.Cleanup(func() { cgroupRoot = was })
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
