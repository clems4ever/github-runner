package agent

import (
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
