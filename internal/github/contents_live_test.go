package github

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The unit tests in contents_test.go answer "does this client send what this
// file thinks it sends", against a handler this repository wrote. They cannot
// answer the question the layer feature rests on: does GitHub, asked with no
// ref at all, answer from the default branch — and does a repository with no
// definition come back as "there is no file" rather than as a failure.
//
// The second one is not a nicety. Every repository in a pool with layers on is
// asked about a file almost none of them have, thirty seconds after the daemon
// starts and every five minutes after that. If GitHub's answer to that arrived
// as an error, every such pool would carry a permanent note about a file
// nobody ever intended to write, and the note would be indistinguishable from
// a real credential problem.
//
//	FLEET_LIVE_TOKEN=… FLEET_LIVE_REPO=owner/name go test ./internal/github -run Live -v
//
// It only reads. Nothing here writes to the repository it is pointed at.

// The read really does land on the default branch, proved against the same
// file fetched at the default branch's head commit. Without this the "no ref"
// property is only a claim about a URL — the point is what GitHub does with
// it.
func TestLiveTheFileComesFromTheDefaultBranch(t *testing.T) {
	client, scope := live(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner, repo, _ := strings.Cut(scope.Path, "/")
	var about struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := client.do(ctx, http.MethodGet, "/repos/"+owner+"/"+repo, &about, scope); err != nil {
		t.Fatal(err)
	}
	if about.DefaultBranch == "" {
		t.Fatal("GitHub named no default branch")
	}

	// README.md rather than the definition path: this is about which branch
	// was read, and it needs a file that is actually there.
	got, err := client.DefaultBranchFile(ctx, scope, "README.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("read nothing; this test needs a repository with a README on its default branch")
	}

	// The same file, asked for by name of the branch this time. Equal bytes is
	// the assertion: a client that had somehow read a fork's default, or a
	// cached ref, would differ here.
	var pinned struct {
		Content string `json:"content"`
	}
	if err := client.do(ctx, http.MethodGet,
		"/repos/"+owner+"/"+repo+"/contents/README.md?ref="+about.DefaultBranch,
		&pinned, scope); err != nil {
		t.Fatal(err)
	}
	if want := unwrap(t, pinned.Content); string(got) != want {
		t.Fatalf("the file read with no ref is not the file on %s (%d bytes against %d)",
			about.DefaultBranch, len(got), len(want))
	}
}

// GitHub answers a directory with a 200 and a JSON *array*, which is the
// shape the unit test for this could not have guessed — it was written against
// {"type":"dir"}, which is what the documentation describes and not what
// arrives. A definition somebody turned into a directory has to be refused by
// name, not reported as unparseable JSON.
func TestLiveADirectoryIsNotAFile(t *testing.T) {
	client, scope := live(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := client.DefaultBranchFile(ctx, scope, ".github")
	if err == nil {
		t.Fatal("read a directory as if it were a definition")
	}
	if !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("refused a directory with %q, which does not say why", err)
	}
}

// The contents API against real GitHub, for the same reason as the rest of
// this file: the wire format is the part a fake cannot be trusted about. The
// base64 comes back wrapped at 60 columns, which Go's decoder refuses, and a
// fake that did not wrap it would have hidden that.
func TestLiveDefaultBranchFileReadsAndMissesCleanly(t *testing.T) {
	client, scope := live(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got, err := client.DefaultBranchFile(ctx, scope, "go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "module ") {
		t.Fatalf("read %q, want the go.mod on the default branch", first(got, 60))
	}

	// The common case by far: a repository that has never heard of this.
	missing, err := client.DefaultBranchFile(ctx, scope, ".github/definitely-not-here.yml")
	if err != nil {
		t.Fatalf("a missing file is not an error: %v", err)
	}
	if missing != nil {
		t.Fatalf("read %q out of nothing", missing)
	}
}

func first(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}

// unwrap decodes the base64 GitHub sends, which is wrapped at 60 columns.
func unwrap(t *testing.T, content string) string {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(
		strings.NewReplacer("\n", "", "\r", "").Replace(content))
	if err != nil {
		t.Fatal(err)
	}
	return string(decoded)
}
