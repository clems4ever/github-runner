package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// body is what a request carried, so a test can assert on what GitHub was
// actually asked for rather than only on what came back.
func body(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the request body is not JSON: %v", err)
	}
	return out
}

func TestJITConfigAsksForOneRunner(t *testing.T) {
	var asked map[string]any
	client, fake := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		asked = body(t, r)
		w.Write([]byte(`{"encoded_jit_config":"BASE64"}`))
	})

	got, err := client.JITConfig(context.Background(), repoScope(),
		JIT{Name: "web-1", Labels: []string{"self-hosted", "vm"}})
	if err != nil {
		t.Fatal(err)
	}
	if got != "BASE64" {
		t.Fatalf("got %q", got)
	}

	want := "POST /repos/clems4ever/runyard/actions/runners/generate-jitconfig"
	if fake.requests[0] != want {
		t.Fatalf("called %q, want %q", fake.requests[0], want)
	}
	if asked["name"] != "web-1" {
		t.Errorf("asked for %v, want web-1", asked["name"])
	}
	// A repository's runners are always in group 1: runner groups are an
	// organisation idea, and the endpoint refuses a request without one.
	if asked["runner_group_id"] != float64(1) {
		t.Errorf("group %v, want the default group", asked["runner_group_id"])
	}
}

// GitHub rejects an empty label list, and a pool is allowed to name none. The
// labels a runner would have configured for itself are the ones that change
// nothing about which jobs it can take, so they are what it asks for.
func TestJITConfigNeverAsksWithNoLabels(t *testing.T) {
	var asked map[string]any
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		asked = body(t, r)
		w.Write([]byte(`{"encoded_jit_config":"BASE64"}`))
	})

	if _, err := client.JITConfig(context.Background(), repoScope(), JIT{Name: "web-1"}); err != nil {
		t.Fatal(err)
	}
	if got := labelsAsked(t, asked); !reflect.DeepEqual(got, implicitLabels) {
		t.Fatalf("labels %v, want %v", got, implicitLabels)
	}
}

// The labels a runner configures for itself are the ones Serves treats as
// implicit, and this endpoint adds none of them: a runner registered without
// them is one the fleet scales up for and GitHub never offers the job to,
// because runs-on nearly always names self-hosted.
func TestJITConfigAsksForTheImplicitLabelsToo(t *testing.T) {
	var asked map[string]any
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		asked = body(t, r)
		w.Write([]byte(`{"encoded_jit_config":"BASE64"}`))
	})

	want := JIT{Name: "web-1", Labels: []string{"vm", "ephemeral", "jit-fleet-smoke"}}
	if _, err := client.JITConfig(context.Background(), repoScope(), want); err != nil {
		t.Fatal(err)
	}

	got := labelsAsked(t, asked)
	if !Serves(got, []string{"self-hosted", "linux", "vm", "jit-fleet-smoke"}) {
		t.Fatalf("registered %v, which is not offered the job it was woken for", got)
	}
	// The pool's own labels survive, and nothing is asked for twice.
	seen := map[string]bool{}
	for _, label := range got {
		if seen[strings.ToLower(label)] {
			t.Fatalf("asked for %q twice in %v", label, got)
		}
		seen[strings.ToLower(label)] = true
	}
	for _, label := range want.Labels {
		if !seen[label] {
			t.Errorf("dropped the pool's own label %q from %v", label, got)
		}
	}
}

// labelsAsked is the label list out of a generate-jitconfig body.
func labelsAsked(t *testing.T, asked map[string]any) []string {
	t.Helper()
	raw, _ := asked["labels"].([]any)
	out := make([]string, 0, len(raw))
	for _, label := range raw {
		text, ok := label.(string)
		if !ok {
			t.Fatalf("label %v is not a string", label)
		}
		out = append(out, text)
	}
	return out
}

// A runner's name is its identity in the fleet and is reused on purpose. This
// endpoint has no --replace, so a name left behind by a machine that was killed
// rather than stopped would block that name for ever.
func TestJITConfigReplacesARunnerLeftBehind(t *testing.T) {
	conflicts := true
	client, fake := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/generate-jitconfig") && conflicts:
			conflicts = false
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"message":"Already exists - A runner with the name web-1 already exists."}`))
		case strings.HasSuffix(r.URL.Path, "/generate-jitconfig"):
			w.Write([]byte(`{"encoded_jit_config":"BASE64"}`))
		case strings.Contains(r.URL.Path, "/actions/runners/7"):
			w.WriteHeader(http.StatusNoContent)
		default:
			w.Write([]byte(`{"total_count":1,"runners":[{"id":7,"name":"web-1","status":"offline"}]}`))
		}
	})

	got, err := client.JITConfig(context.Background(), repoScope(), JIT{Name: "web-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "BASE64" {
		t.Fatalf("got %q", got)
	}
	if !strings.Contains(strings.Join(fake.requests, "\n"), "DELETE /repos/clems4ever/runyard/actions/runners/7") {
		t.Fatalf("the entry left behind was not removed:\n%s", strings.Join(fake.requests, "\n"))
	}
}

// A conflict that is not a name collision must not cost somebody a runner. The
// replacement above deletes a registration, and doing that on the strength of a
// status code alone would be the expensive kind of mistake.
func TestJITConfigDoesNotDeleteOnAnotherConflict(t *testing.T) {
	client, fake := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"message":"Repository is archived"}`))
	})

	if _, err := client.JITConfig(context.Background(), repoScope(), JIT{Name: "web-1"}); err == nil {
		t.Fatal("the conflict was swallowed")
	}
	if strings.Contains(strings.Join(fake.requests, "\n"), "DELETE") {
		t.Fatalf("deleted a runner over an unrelated conflict:\n%s", strings.Join(fake.requests, "\n"))
	}
}

// An organisation pool can name a group, and a runner in the wrong group is
// invisible to the workflows that asked for it.
func TestJITConfigResolvesAnOrganisationGroup(t *testing.T) {
	var asked map[string]any
	client, fake := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "runner-groups") {
			w.Write([]byte(`{"total_count":2,"runner_groups":[` +
				`{"id":1,"name":"Default"},{"id":4,"name":"gpu"}]}`))
			return
		}
		asked = body(t, r)
		w.Write([]byte(`{"encoded_jit_config":"BASE64"}`))
	})

	if _, err := client.JITConfig(context.Background(), orgScope(),
		JIT{Name: "gpu-1", Group: "gpu"}); err != nil {
		t.Fatal(err)
	}
	if asked["runner_group_id"] != float64(4) {
		t.Fatalf("group %v, want 4", asked["runner_group_id"])
	}

	// Asked once. Scaling a pool up mints one of these per runner, and looking
	// the group up each time would double the calls.
	if _, err := client.JITConfig(context.Background(), orgScope(),
		JIT{Name: "gpu-2", Group: "gpu"}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(fake.requests, "\n"), "runner-groups"); n != 1 {
		t.Fatalf("looked the group up %d times", n)
	}
}

// Placing the runner in the default group instead would look like a fleet that
// is not scaling up, rather than like a configuration mistake.
func TestJITConfigRefusesAGroupThatDoesNotExist(t *testing.T) {
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_count":1,"runner_groups":[{"id":1,"name":"Default"}]}`))
	})

	_, err := client.JITConfig(context.Background(), orgScope(), JIT{Name: "gpu-1", Group: "gpu"})
	if err == nil {
		t.Fatal("a missing group was accepted")
	}
	if !strings.Contains(err.Error(), "gpu") {
		t.Errorf("the message does not name the group: %v", err)
	}
}
