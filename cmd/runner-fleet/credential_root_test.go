//go:build root

// These tests need root, because the bug they cover needs two users.
//
// The daemon runs as root and the runners' agents do not. Every test that ran
// as one user agreed the credential was written correctly, and every machine
// runner on a packaged install died reading it:
//
//	runner-fleet: read the credential: open /run/runner-fleet/credentials/1:
//	permission denied
//
// A file is 0600 and owned by whoever wrote it, so "the daemon can read what
// the daemon wrote" is true and worthless. What matters is whether somebody
// else can, and only a second user can answer that.
//
//	go test -tags root -c -o /tmp/roottest ./cmd/runner-fleet/ && sudo /tmp/roottest
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/clems4ever/github-runner/internal/paths"
)

// theOtherUser is an unprivileged account that exists on every Linux, standing
// in for the service user a packaged install creates.
const theOtherUser = "nobody"

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Getuid() == 0 {
		return
	}
	// Skipping quietly is how this class of bug survives: the test that covers
	// it never runs, and the tick is green either way. CI sets this.
	if os.Getenv("REQUIRE_ROOT") != "" {
		t.Fatal("REQUIRE_ROOT is set and this is not root; the cross-user checks cannot run")
	}
	t.Skip("not root: the cross-user checks need two users")
}

// canRead answers the only question that matters, by being the other user.
//
// The read happens in a process that has dropped to that account — not with
// sudo, which is not installed everywhere this runs, and not by inspecting the
// mode, which is the reasoning that produced the bug.
func canRead(t *testing.T, path string) error {
	t.Helper()
	owner, err := paths.LookupOwner(theOtherUser)
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("/bin/cat", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uint32(owner.UID), Gid: uint32(owner.GID)},
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return &readFailure{output: strings.TrimSpace(string(out)), err: err}
	}
	return nil
}

// reachableTempDir is a temporary directory another account can walk into.
//
// Go's own temporary directories are 0700 and root's, all the way up, so
// without this every one of these tests would fail on the path rather than on
// what is at the end of it — and would keep failing after the bug was fixed.
// A real install puts these under /run and /var/lib, which are traversable.
func reachableTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for path := dir; path != "/" && path != filepath.Dir(path); path = filepath.Dir(path) {
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if path == os.TempDir() {
			break
		}
	}
	return dir
}

type readFailure struct {
	output string
	err    error
}

func (f *readFailure) Error() string { return f.err.Error() + ": " + f.output }

// The bug, in one test: the daemon writes a credential, and the account the
// runners run as reads it.
func TestTheServiceUserCanReadTheCredentialTheDaemonWrote(t *testing.T) {
	requireRoot(t)

	owner, err := paths.LookupOwner(theOtherUser)
	if err != nil {
		t.Fatal(err)
	}
	layout := paths.Under(reachableTempDir(t))
	if err := layout.EnsureDirs(owner); err != nil {
		t.Fatal(err)
	}

	write := credentialWriter(layout, owner)
	if err := write(1, "github_pat_the_credential"); err != nil {
		t.Fatalf("write the credential: %v", err)
	}

	if err := canRead(t, layout.Credential(1)); err != nil {
		t.Fatalf("the service user cannot read the credential, so every machine runner would fail to register: %v", err)
	}
}

// Rewriting an unchanged credential still hands it over. A host upgraded from
// a version that wrote these as root has the right contents and the wrong
// owner, and the writer's early return would otherwise leave it that way for
// ever.
func TestAnUnchangedCredentialIsStillHandedOver(t *testing.T) {
	requireRoot(t)

	owner, err := paths.LookupOwner(theOtherUser)
	if err != nil {
		t.Fatal(err)
	}
	layout := paths.Under(reachableTempDir(t))
	if err := layout.EnsureDirs(owner); err != nil {
		t.Fatal(err)
	}

	// What the previous release left behind: right contents, root's file.
	if err := os.WriteFile(layout.Credential(1), []byte("github_pat_the_credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := canRead(t, layout.Credential(1)); err == nil {
		t.Fatal("this test cannot tell anything: the file was readable before the daemon touched it")
	}

	write := credentialWriter(layout, owner)
	if err := write(1, "github_pat_the_credential"); err != nil {
		t.Fatal(err)
	}
	if err := canRead(t, layout.Credential(1)); err != nil {
		t.Fatalf("an upgraded host leaves its runners unable to read the credential: %v", err)
	}
}

// The credential is still nobody else's business. Handing it to one account is
// not the same as publishing it.
func TestTheCredentialIsNotReadableByEveryone(t *testing.T) {
	requireRoot(t)

	owner, err := paths.LookupOwner(theOtherUser)
	if err != nil {
		t.Fatal(err)
	}
	layout := paths.Under(t.TempDir())
	if err := layout.EnsureDirs(owner); err != nil {
		t.Fatal(err)
	}
	if err := credentialWriter(layout, owner)(1, "github_pat_the_credential"); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(layout.Credential(1))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("the credential is %o; it holds a token that administers repositories", mode)
	}
	dir, err := os.Stat(layout.CredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	if mode := dir.Mode().Perm(); mode != 0o700 {
		t.Fatalf("the credentials directory is %o", mode)
	}
}
