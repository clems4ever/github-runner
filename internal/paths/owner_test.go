package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLookupOwnerFindsAnAccount(t *testing.T) {
	// nobody is on every Linux, and is what a service user looks like: a real
	// account that is not this process.
	owner, err := LookupOwner("nobody")
	if err != nil {
		t.Fatalf("look up nobody: %v", err)
	}
	if !owner.Known {
		t.Fatal("nobody was not resolved")
	}
	if owner.Name != "nobody" {
		t.Errorf("name: %q", owner.Name)
	}
}

// A missing account is reported, and reported by name: the daemon carries on
// without it, so this message is the only sign that machine pools are about to
// fail.
func TestLookupOwnerSaysWhenTheAccountIsMissing(t *testing.T) {
	owner, err := LookupOwner("no-such-service-user")
	if err == nil {
		t.Fatal("a missing account was accepted")
	}
	if !strings.Contains(err.Error(), "no-such-service-user") {
		t.Errorf("the message does not name the account: %v", err)
	}
	if owner.Known {
		t.Error("a missing account came back as resolved")
	}
}

func TestAnUnnamedOwnerIsNobodyToHandTo(t *testing.T) {
	owner, err := LookupOwner("")
	if err != nil {
		t.Fatal(err)
	}
	if owner.Known {
		t.Fatal("an empty name resolved to an account")
	}
	// And handing over is then a no-op rather than an error, which is what lets
	// a developer run the daemon as themselves.
	file := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(file, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := owner.Give(file); err != nil {
		t.Fatalf("give: %v", err)
	}
}

func TestGivingToYourselfChangesNothing(t *testing.T) {
	owner := Owner{UID: os.Getuid(), GID: os.Getgid(), Name: "me", Known: true}
	file := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(file, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := owner.Give(file); err != nil {
		t.Fatalf("give: %v", err)
	}
	// Even for a path that does not exist: there is nothing to do, so there is
	// nothing to fail.
	if err := owner.Give(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("give a missing path: %v", err)
	}
}

// The directories a machine agent needs of its own, as opposed to the ones that
// stay root's. Getting this list wrong is how the runners ended up unable to
// read their credential.
func TestTheAgentGetsTheDirectoriesItUses(t *testing.T) {
	layout := Under(t.TempDir())
	if err := layout.EnsureDirs(CurrentOwner()); err != nil {
		t.Fatal(err)
	}

	handed := map[string]bool{}
	for _, dir := range layout.AgentDirs() {
		handed[dir] = true
	}
	// Where a machine finds the token it registers with, and where it builds
	// its images and disks.
	for _, needed := range []string{layout.CredentialsDir(), layout.State, layout.ImagesDir()} {
		if !handed[needed] {
			t.Errorf("%s is not handed to the runners, and an agent cannot use it", needed)
		}
	}
	// The database and the master key are the daemon's alone.
	if handed[layout.Etc] {
		t.Error("the configuration directory is handed to the runners; it holds the database and the master key")
	}

	for _, dir := range layout.AgentDirs() {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("%s was not created: %v", dir, err)
		}
		if mode := info.Mode().Perm(); mode != 0o700 {
			t.Errorf("%s is %o, and these hold credentials and disk images", dir, mode)
		}
	}
}
