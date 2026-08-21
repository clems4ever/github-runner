package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/clems4ever/github-runner/internal/model"
)

// fakeGitHub records what was asked of it and answers from a table, so every
// test here runs without a network or a credential.
type fakeGitHub struct {
	t        *testing.T
	requests []string
	handler  func(w http.ResponseWriter, r *http.Request)
}

func (f *fakeGitHub) start() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			f.t.Errorf("the credential did not reach GitHub: %q", auth)
		}
		if v := r.Header.Get("X-GitHub-Api-Version"); v == "" {
			f.t.Error("no API version pinned, so a future change to the default would break this silently")
		}
		f.handler(w, r)
	}))
	f.t.Cleanup(srv.Close)
	return srv
}

func newClient(t *testing.T, handler func(http.ResponseWriter, *http.Request)) (*Client, *fakeGitHub) {
	f := &fakeGitHub{t: t, handler: handler}
	srv := f.start()
	return New("test-token", WithBaseURL(srv.URL)), f
}

func repoScope() Scope { return Scope{Kind: model.ScopeRepository, Path: "clems4ever/runyard"} }
func orgScope() Scope  { return Scope{Kind: model.ScopeOrganization, Path: "runyard-ai"} }

func TestRegistrationToken(t *testing.T) {
	client, fake := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"token":"AAAA-registration","expires_at":"2026-01-01T00:00:00Z"}`))
	})

	token, err := client.RegistrationToken(context.Background(), repoScope())
	if err != nil {
		t.Fatal(err)
	}
	if token != "AAAA-registration" {
		t.Fatalf("got %q", token)
	}
	want := "POST /repos/clems4ever/runyard/actions/runners/registration-token"
	if fake.requests[0] != want {
		t.Fatalf("called %q, want %q", fake.requests[0], want)
	}
}

// An organisation is a different endpoint, not a different client.
func TestRegistrationTokenForAnOrganisation(t *testing.T) {
	client, fake := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"token":"AAAA"}`))
	})
	if _, err := client.RegistrationToken(context.Background(), orgScope()); err != nil {
		t.Fatal(err)
	}
	want := "POST /orgs/runyard-ai/actions/runners/registration-token"
	if fake.requests[0] != want {
		t.Fatalf("called %q, want %q", fake.requests[0], want)
	}
}

func TestRegistrationTokenRejectsAnEmptyAnswer(t *testing.T) {
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	})
	if _, err := client.RegistrationToken(context.Background(), repoScope()); err == nil {
		t.Fatal("an empty token was accepted, which would boot a runner that cannot register")
	}
}

func TestStates(t *testing.T) {
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_count":4,"runners":[
			{"id":1,"name":"web-1","status":"online","busy":true},
			{"id":2,"name":"web-2","status":"online","busy":false},
			{"id":3,"name":"web-3","status":"offline","busy":false},
			{"id":4,"name":"web-4","status":"offline","busy":true}
		]}`))
	})

	states, err := client.States(context.Background(), repoScope())
	if err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]State{
		"web-1": StateBusy,
		"web-2": StateIdle,
		"web-3": StateOffline,
		// Offline wins over busy: GitHub can report a stale busy flag for a
		// runner it has lost, and treating that as busy would block the
		// reconciler from ever replacing a dead runner.
		"web-4": StateOffline,
	} {
		if states[name] != want {
			t.Errorf("%s is %q, want %q", name, states[name], want)
		}
	}
}

func TestStatesOnAnEmptyFleet(t *testing.T) {
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"total_count":0,"runners":[]}`))
	})
	states, err := client.States(context.Background(), repoScope())
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 0 {
		t.Fatalf("got %v", states)
	}
}

func TestDeregisterFindsTheIDFirst(t *testing.T) {
	client, fake := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"runners":[{"id":42,"name":"web-2","status":"online","busy":false}]}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	if err := client.Deregister(context.Background(), repoScope(), "web-2"); err != nil {
		t.Fatal(err)
	}
	if len(fake.requests) != 2 {
		t.Fatalf("want a lookup and a delete, got %v", fake.requests)
	}
	if want := "DELETE /repos/clems4ever/runyard/actions/runners/42"; fake.requests[1] != want {
		t.Fatalf("deleted %q, want %q", fake.requests[1], want)
	}
}

// Deregistering something GitHub has already forgotten is the outcome that was
// wanted, not an error to retry for ever.
func TestDeregisterIsQuietWhenAlreadyGone(t *testing.T) {
	client, fake := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"runners":[]}`))
	})
	if err := client.Deregister(context.Background(), repoScope(), "web-2"); err != nil {
		t.Fatalf("want no error, got %v", err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("it tried to delete something that is not there: %v", fake.requests)
	}
}

func TestErrorsSayWhatToCheck(t *testing.T) {
	tests := []struct {
		status int
		scope  Scope
		want   string
	}{
		{http.StatusUnauthorized, repoScope(), "the token itself is wrong"},
		{http.StatusForbidden, repoScope(), "Administration: Read and write"},
		{http.StatusForbidden, orgScope(), "Self-hosted runners: Read and write"},
		{http.StatusNotFound, repoScope(), "cannot see the repository"},
		// The one that has already cost time once: an organisation URL that is
		// really a personal account.
		{http.StatusNotFound, orgScope(), "no account-level runners"},
	}

	for _, tt := range tests {
		client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tt.status)
			w.Write([]byte(`{"message":"Not Found"}`))
		})

		_, err := client.RegistrationToken(context.Background(), tt.scope)
		if err == nil {
			t.Fatalf("%d was not reported as an error", tt.status)
		}
		var apiErr *Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("want a *github.Error, got %T", err)
		}
		if apiErr.Status != tt.status {
			t.Errorf("status is %d, want %d", apiErr.Status, tt.status)
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%d on a %s says %q, want it to mention %q", tt.status, tt.scope.Kind, err, tt.want)
		}
	}
}

func TestErrorCarriesGitHubsOwnMessage(t *testing.T) {
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))
	})
	_, err := client.Runners(context.Background(), repoScope())
	if err == nil || !strings.Contains(err.Error(), "Resource not accessible") {
		t.Fatalf("GitHub's own message was dropped: %v", err)
	}
}

func TestUnparseableBodyIsAnError(t *testing.T) {
	client, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>a proxy got in the way</html>`))
	})
	_, err := client.Runners(context.Background(), repoScope())
	if err == nil || !strings.Contains(err.Error(), "not the expected JSON") {
		t.Fatalf("got %v", err)
	}
}

func TestScopeOf(t *testing.T) {
	p := model.Pool{ScopeKind: model.ScopeOrganization, Scope: "runyard-ai"}
	if got := ScopeOf(p); got.Kind != model.ScopeOrganization || got.Path != "runyard-ai" {
		t.Fatalf("got %+v", got)
	}
}
