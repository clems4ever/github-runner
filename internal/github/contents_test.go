package github

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
)

// wrapped is base64 the way GitHub sends it: in 60-column lines, which Go's
// decoder refuses without help.
func wrapped(content string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	var out strings.Builder
	for len(encoded) > 60 {
		out.WriteString(encoded[:60] + "\n")
		encoded = encoded[60:]
	}
	out.WriteString(encoded + "\n")
	return out.String()
}

func TestDefaultBranchFileReadsTheFile(t *testing.T) {
	var asked string
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.String()
		w.Write([]byte(`{"type":"file","encoding":"base64","content":"` +
			strings.ReplaceAll(wrapped("packages:\n  - git\n"), "\n", `\n`) + `"}`))
	})

	got, err := client.DefaultBranchFile(context.Background(), repoScope(), ".github/runner-fleet.yml")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "packages:\n  - git\n" {
		t.Fatalf("read %q", got)
	}
	if want := "/repos/clems4ever/runyard/contents/.github/runner-fleet.yml"; asked != want {
		t.Fatalf("asked for %q, want %q", asked, want)
	}
}

// The security property, asserted rather than assumed: this endpoint takes no
// ref, so no caller can point it at a pull request's head. The file decides
// what the host builds and runs as root; reading it from an unmerged branch
// would mean opening a pull request is enough to run code on the host.
func TestDefaultBranchFileAsksForNoRef(t *testing.T) {
	var query string
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Write([]byte(`{"type":"file","encoding":"base64","content":""}`))
	})

	if _, err := client.DefaultBranchFile(
		context.Background(), repoScope(), ".github/runner-fleet.yml"); err != nil {
		t.Fatal(err)
	}
	if query != "" {
		t.Fatalf("sent %q; the default branch is the only branch this may read", query)
	}
}

// Most repositories will never have one. Asking every repository in a pool
// about a file that is usually absent must not look like a failure.
func TestDefaultBranchFileIsQuietWhenThereIsNone(t *testing.T) {
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	})

	got, err := client.DefaultBranchFile(context.Background(), repoScope(), ".github/runner-fleet.yml")
	if err != nil {
		t.Fatalf("a repository without the file is not an error: %v", err)
	}
	if got != nil {
		t.Fatalf("read %q out of nothing", got)
	}
}

// A symlink is a path this did not ask for, and following one would let a
// repository read a file by naming it somewhere else.
func TestDefaultBranchFileRefusesWhatIsNotAFile(t *testing.T) {
	for _, kind := range []string{"symlink", "dir", "submodule"} {
		client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"type":"` + kind + `","encoding":"base64","content":""}`))
		})
		if _, err := client.DefaultBranchFile(
			context.Background(), repoScope(), ".github/runner-fleet.yml"); err == nil {
			t.Errorf("read a %s as a file", kind)
		}
	}
}

// Over a megabyte GitHub sends the metadata with the content left out. Nothing
// this reads is near that, so a file that hits it is not a definition somebody
// wrote by hand — and returning empty content as an empty file would be worse
// than saying so.
func TestDefaultBranchFileRefusesAFileTooLargeToComeBackWithIt(t *testing.T) {
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"type":"file","encoding":"none","content":"","size":2000000}`))
	})
	if _, err := client.DefaultBranchFile(
		context.Background(), repoScope(), ".github/runner-fleet.yml"); err == nil {
		t.Fatal("returned an empty file rather than saying it could not read it")
	}
}

// An organisation has no default branch. Asking for one is a caller mistake,
// and a scope that is quietly skipped is a pool that silently never layers.
func TestDefaultBranchFileRefusesAnOrganisation(t *testing.T) {
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("called GitHub for an organisation's file")
	})
	if _, err := client.DefaultBranchFile(
		context.Background(), orgScope(), ".github/runner-fleet.yml"); err == nil {
		t.Fatal("accepted an organisation")
	}
}

// A directory is not what the documentation's schema suggests — GitHub answers
// it with an array of entries, not an object with type "dir". Decoded straight
// into the file struct that is a parse error naming nothing; the path is
// somebody's mistake to correct, so it is named.
func TestDefaultBranchFileRefusesADirectory(t *testing.T) {
	client, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`[{"name":"workflows","path":".github/workflows","type":"dir"}]`))
	})

	_, err := client.DefaultBranchFile(context.Background(), repoScope(), ".github")
	if err == nil {
		t.Fatal("read a directory as if it were a definition")
	}
	if !strings.Contains(err.Error(), ".github is a directory") {
		t.Fatalf("refused with %q, which does not say what is wrong", err)
	}
}
