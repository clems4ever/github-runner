package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/clems4ever/github-runner/internal/secrets"
)

// capture runs f with stdout and stderr redirected, and returns what each got.
//
// They are kept apart because which stream a thing goes to is part of the
// contract here: the key is the only thing on stdout, so that piping this command
// into a file or a secret manager gets the key and not a sentence about it.
func capture(t *testing.T, f func() error) (stdout, stderr string, err error) {
	t.Helper()
	outR, outW, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal(pipeErr)
	}
	errR, errW, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal(pipeErr)
	}
	realOut, realErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW

	err = f()

	os.Stdout, os.Stderr = realOut, realErr
	outW.Close()
	errW.Close()

	outBytes := readAll(t, outR)
	errBytes := readAll(t, errR)
	return outBytes, errBytes, err
}

func readAll(t *testing.T, r *os.File) string {
	t.Helper()
	defer r.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			return sb.String()
		}
	}
}

// A host provisioned by a script has no browser on it. This is the whole point of
// the command: a key without anyone setting a password first.
func TestAPIKeyCreateListRevokeWithoutABrowser(t *testing.T) {
	root := t.TempDir()

	stdout, stderr, err := capture(t, func() error {
		return apikeyCommand([]string{"create", "--name", "monitoring", "--scope", "read", "--root", root})
	})
	if err != nil {
		t.Fatalf("create: %v (%s)", err, stderr)
	}

	// Exactly the key on stdout, and nothing else, so `apikey create > secret`
	// writes a usable file.
	key := strings.TrimSpace(stdout)
	if !strings.HasPrefix(key, secrets.APIKeyPrefix) {
		t.Fatalf("stdout was %q, want just the key", stdout)
	}
	if strings.Contains(key, " ") || strings.Contains(key, "\n") {
		t.Errorf("stdout carries more than the key: %q", stdout)
	}
	// The explanation goes to stderr, where a pipe does not pick it up.
	if !strings.Contains(stderr, "shown once") {
		t.Errorf("stderr does not say the key cannot be recovered: %q", stderr)
	}
	if strings.Contains(stderr, key) {
		t.Error("the key is on stderr as well, so it lands in logs that keep stderr")
	}

	// The daemon agrees it is a key: the same database, looked up the way a
	// request would be.
	db, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	found, err := db.APIKeyByHash(context.Background(), secrets.HashAPIKey(key))
	if err != nil {
		t.Fatalf("the key the CLI printed is not in the database: %v", err)
	}
	if found.Name != "monitoring" || string(found.Scope) != "read" {
		t.Errorf("stored as %+v, want a read key called monitoring", found)
	}

	// Listed by what an operator has to go on, and never the key itself.
	stdout, _, err = capture(t, func() error {
		return apikeyCommand([]string{"list", "--root", root})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"monitoring", "read", "never"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the list does not mention %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, key) {
		t.Error("the list prints the key back")
	}

	// And revoking it is final.
	if _, _, err := capture(t, func() error {
		return apikeyCommand([]string{"revoke", "--name", "monitoring", "--root", root})
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := db.APIKeyByHash(context.Background(), secrets.HashAPIKey(key)); err == nil {
		t.Error("a revoked key still resolves")
	}
}

func TestAPIKeyCreateDefaultsToRead(t *testing.T) {
	root := t.TempDir()
	stdout, _, err := capture(t, func() error {
		return apikeyCommand([]string{"create", "--name", "ci", "--root", root})
	})
	if err != nil {
		t.Fatal(err)
	}

	db, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	found, err := db.APIKeyByHash(context.Background(), secrets.HashAPIKey(strings.TrimSpace(stdout)))
	if err != nil {
		t.Fatal(err)
	}
	// The safer of the two when the operator said nothing.
	if string(found.Scope) != "read" {
		t.Errorf("scope is %q, want read", found.Scope)
	}
}

func TestAPIKeyCreateWithAnExpiry(t *testing.T) {
	root := t.TempDir()
	stdout, stderr, err := capture(t, func() error {
		return apikeyCommand([]string{"create", "--name", "ci", "--expires-days", "30", "--root", root})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "stops working on") {
		t.Errorf("the operator was not told when it expires: %q", stderr)
	}

	db, err := openStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	found, err := db.APIKeyByHash(context.Background(), secrets.HashAPIKey(strings.TrimSpace(stdout)))
	if err != nil {
		t.Fatal(err)
	}
	if found.ExpiresAt == nil {
		t.Fatal("no expiry was stored")
	}
	if days := found.ExpiresAt.Sub(found.CreatedAt).Hours() / 24; days < 29 || days > 31 {
		t.Errorf("expires in %.1f days, want about 30", days)
	}
}

func TestAPIKeyRefusesWhatItCannotDo(t *testing.T) {
	root := t.TempDir()

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no action", []string{"--root", root}, "create, list or revoke"},
		{"unknown action", []string{"rotate", "--root", root}, "create, list or revoke"},
		{"no name", []string{"create", "--root", root}, "needs a name"},
		{"bad scope", []string{"create", "--name", "ci", "--scope", "write", "--root", root}, "api key scope"},
		{"revoke without a name", []string{"revoke", "--root", root}, "pass --name"},
		{"revoke one that is not there", []string{"revoke", "--name", "ghost", "--root", root}, "no api key called"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, _, err := capture(t, func() error { return apikeyCommand(tc.args) })
			if err == nil {
				t.Fatalf("%v was accepted", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			// Nothing half-done: a refused create must not have printed a key.
			if strings.Contains(stdout, secrets.APIKeyPrefix) {
				t.Errorf("a refused command printed something key-shaped: %q", stdout)
			}
		})
	}
}

func TestAPIKeyListSaysWhenThereAreNone(t *testing.T) {
	root := t.TempDir()
	stdout, _, err := capture(t, func() error {
		return apikeyCommand([]string{"list", "--root", root})
	})
	if err != nil {
		t.Fatal(err)
	}
	// A word rather than empty output, which reads as a command that failed.
	if !strings.Contains(stdout, "no api keys") {
		t.Errorf("an empty list printed %q", stdout)
	}
}
