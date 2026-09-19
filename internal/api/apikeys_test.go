package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/secrets"
	"github.com/clems4ever/github-runner/internal/store"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// key mints one and hands back the string a client would be given.
func (h *harness) key(name string, scope model.APIKeyScope) string {
	h.t.Helper()
	_, secret, err := h.store.CreateAPIKey(context.Background(), model.APIKey{Name: name, Scope: scope})
	if err != nil {
		h.t.Fatal(err)
	}
	return secret
}

// withKey makes a request as a machine: a key, and no password.
func (h *harness) withKey(key, method, path string, body any) *http.Response {
	h.t.Helper()
	req := h.request(method, path, body)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := h.server.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

// request builds one without deciding how it authenticates.
func (h *harness) request(method, path string, body any) *http.Request {
	h.t.Helper()
	var reader io.Reader = strings.NewReader("")
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	return req
}

func (h *harness) body(resp *http.Response) string {
	h.t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// A key gets in
// ---------------------------------------------------------------------------

func TestAPIKeyReadsTheFleet(t *testing.T) {
	h := newHarness(t)
	key := h.key("monitoring", model.APIKeyRead)

	resp := h.withKey(key, "GET", "/api/pools", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a read key was answered %d on GET /api/pools", resp.StatusCode)
	}
}

// A key is a whole credential on its own: nothing about the request mentions the
// operator's password, and the browser's login box never enters into it.
func TestAPIKeyNeedsNoPasswordAlongside(t *testing.T) {
	h := newHarness(t)
	key := h.key("monitoring", model.APIKeyRead)

	req := h.request("GET", "/api/runners", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("answered %d", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") != "" {
		t.Error("a valid key was still asked for a password")
	}
}

func TestAPIKeyAdminChangesTheFleet(t *testing.T) {
	h := newHarness(t)
	key := h.key("terraform", model.APIKeyAdmin)

	resp := h.withKey(key, "POST", "/api/pools", h.samplePool())
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("an admin key was answered %d creating a pool: %s", resp.StatusCode, h.body(resp))
	}
}

// The point of having two scopes: the key a dashboard holds cannot delete a pool
// even though it can see one.
func TestAPIKeyReadCannotWrite(t *testing.T) {
	h := newHarness(t)
	pool := h.createPool("web")
	key := h.key("monitoring", model.APIKeyRead)

	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/pools"},
		{"PUT", "/api/pools/" + fmt.Sprint(pool.ID)},
		{"DELETE", "/api/pools/" + fmt.Sprint(pool.ID)},
		{"POST", "/api/reconcile"},
		{"POST", "/api/credentials"},
		{"PUT", "/api/credentials/" + fmt.Sprint(h.credID) + "/secret"},
		{"DELETE", "/api/credentials/" + fmt.Sprint(h.credID)},
		{"PUT", "/api/settings/budget"},
		{"POST", "/api/pools/import"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			resp := h.withKey(key, tc.method, tc.path, map[string]any{})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("a read key was answered %d, want 403", resp.StatusCode)
			}
			if !strings.Contains(h.body(resp), "read-only") {
				t.Error("the refusal does not say why")
			}
		})
	}

	// And the pool it was not allowed to delete is still there, so the refusal
	// happened before the handler rather than after it.
	if _, err := h.store.Pool(context.Background(), pool.ID); err != nil {
		t.Errorf("the pool a read key was refused permission to delete is gone: %v", err)
	}
}

// The scope rule is the method, decided in one place, so a route added later is
// covered without anyone remembering to cover it.
func TestAPIKeyScopeIsDecidedByMethodAlone(t *testing.T) {
	for _, tc := range []struct {
		scope  model.APIKeyScope
		method string
		want   bool
	}{
		{model.APIKeyRead, http.MethodGet, true},
		{model.APIKeyRead, http.MethodHead, true},
		{model.APIKeyRead, http.MethodPost, false},
		{model.APIKeyRead, http.MethodPut, false},
		{model.APIKeyRead, http.MethodPatch, false},
		{model.APIKeyRead, http.MethodDelete, false},
		// Not a read, so not allowed: an unrecognised method errs closed.
		{model.APIKeyRead, http.MethodOptions, false},
		{model.APIKeyRead, "BREW", false},
		{model.APIKeyAdmin, http.MethodGet, true},
		{model.APIKeyAdmin, http.MethodDelete, true},
		{model.APIKeyAdmin, "BREW", true},
		// A scope that is not one of the two grants nothing at all.
		{model.APIKeyScope("write"), http.MethodGet, true},
		{model.APIKeyScope("write"), http.MethodPost, false},
	} {
		if got := scopeAllows(tc.scope, tc.method); got != tc.want {
			t.Errorf("scopeAllows(%q, %s) = %v, want %v", tc.scope, tc.method, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// A key that should not get in
// ---------------------------------------------------------------------------

func TestAPIKeyUnknownIsRefused(t *testing.T) {
	h := newHarness(t)
	resp := h.withKey("rf_this-was-never-issued-by-anything", "GET", "/api/pools", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unknown key was answered %d, want 401", resp.StatusCode)
	}
	// Bearer, not Basic: a browser that lands here should show the error, not ask
	// for a password that would not help.
	if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
		t.Errorf("challenged with %q, want a Bearer challenge", got)
	}
}

func TestAPIKeyRevokedStopsWorking(t *testing.T) {
	h := newHarness(t)
	key := h.key("ci", model.APIKeyAdmin)

	var keys []model.APIKey
	h.decode(h.do("GET", "/api/api-keys", nil), &keys)
	if len(keys) != 1 {
		t.Fatalf("expected the one key, got %d", len(keys))
	}
	if resp := h.do("DELETE", fmt.Sprintf("/api/api-keys/%d", keys[0].ID), nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoking answered %d", resp.StatusCode)
	}

	resp := h.withKey(key, "GET", "/api/pools", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a revoked key was answered %d, want 401", resp.StatusCode)
	}
}

func TestAPIKeyExpiredIsRefusedAndSaysSo(t *testing.T) {
	h := newHarness(t)

	// Created valid and then waited out, because the store refuses to mint one
	// that has already expired. Moving the daemon's clock rather than the row is
	// the same thing from the key's point of view, and it exercises the check
	// that runs on every request.
	//
	// The clock is installed before the first request and moved through an
	// atomic afterwards: the handler reads it from another goroutine, so
	// reassigning nowFor between requests would be a data race whether or not
	// the detector happened to catch it.
	clock := &atomic.Int64{}
	clock.Store(time.Now().UnixNano())
	h.srv.keys.nowFor = func() time.Time { return time.Unix(0, clock.Load()) }

	at := time.Now().Add(time.Hour)
	_, key, err := h.store.CreateAPIKey(context.Background(),
		model.APIKey{Name: "ci", Scope: model.APIKeyAdmin, ExpiresAt: &at})
	if err != nil {
		t.Fatal(err)
	}

	// It works right up to the expiry.
	before := h.withKey(key, "GET", "/api/pools", nil)
	before.Body.Close()
	if before.StatusCode != http.StatusOK {
		t.Fatalf("a key with an hour left was answered %d", before.StatusCode)
	}

	clock.Store(at.Add(time.Minute).UnixNano())

	resp := h.withKey(key, "GET", "/api/pools", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an expired key was answered %d, want 401", resp.StatusCode)
	}
	// The one failure here with an obvious fix, so it is named rather than folded
	// into "unknown key".
	if !strings.Contains(h.body(resp), "expired") {
		t.Error("the refusal does not say the key expired")
	}
}

// A request carries one Authorization header, so the scheme in it is the whole of
// what credential is being offered — a client cannot present both.
//
// A Bearer that does not verify is therefore refused as a key, and not retried as
// a password. Falling through would mean a client holding a stale key and a
// cached password carried on working, with nobody finding out until the day the
// password changed.
func TestABadKeyIsRefusedAsAKeyAndNotAsAPasswordPrompt(t *testing.T) {
	h := newHarness(t)

	req := h.request("GET", "/api/pools", nil)
	// The password first and the key second, so the key is what the request
	// actually carries: SetBasicAuth writes the same header.
	req.SetBasicAuth("admin", "correct-horse")
	req.Header.Set("Authorization", "Bearer rf_not-a-key")

	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a bad key was answered %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
		t.Errorf("a bad key was turned into a password prompt (%q)", got)
	}
}

// An empty or malformed Authorization header is not a key. It falls through to
// the password, which then asks for one, rather than being reported as a bad key.
func TestAMalformedAuthorizationHeaderAsksForThePassword(t *testing.T) {
	h := newHarness(t)

	for _, header := range []string{"Bearer", "Bearer ", "Bearer    ", "Token abc", "rf_looks-like-a-key"} {
		req := h.request("GET", "/api/pools", nil)
		req.Header.Set("Authorization", header)
		resp, err := h.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization %q was answered %d, want 401", header, resp.StatusCode)
		}
		if !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic") {
			t.Errorf("Authorization %q was not treated as a request with no credentials", header)
		}
	}
}

// The UI is password-only. A leaked key should not be usable to sit in a browser
// looking at the fleet.
func TestAKeyCannotLoadTheUI(t *testing.T) {
	h := newHarness(t)
	key := h.key("monitoring", model.APIKeyAdmin)

	for _, path := range []string{"/", "/index.html"} {
		resp := h.withKey(key, "GET", path, nil)
		body := h.body(resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s answered %d to a key, want 401", path, resp.StatusCode)
		}
		if strings.Contains(body, "fleet</html>") {
			t.Errorf("%s served the app to a key", path)
		}
	}
}

// ---------------------------------------------------------------------------
// What no key may ever do
// ---------------------------------------------------------------------------

// The constraint the whole design rests on: a key cannot read, create or revoke a
// key, and cannot touch the web password. So a leaked key cannot mint its own
// replacement, cannot enumerate what else exists to steal, and cannot lock the
// operator out — which is what makes revoking one final.
func TestNoAPIKeyCanReachKeysOrThePassword(t *testing.T) {
	h := newHarness(t)

	for _, scope := range []model.APIKeyScope{model.APIKeyRead, model.APIKeyAdmin} {
		t.Run(string(scope), func(t *testing.T) {
			key := h.key("k-"+string(scope), scope)
			victim := h.key("victim-"+string(scope), model.APIKeyRead)

			for _, tc := range []struct {
				method, path string
				body         any
			}{
				{"GET", "/api/api-keys", nil},
				{"POST", "/api/api-keys", map[string]any{"name": "minted-by-a-key", "scope": "admin"}},
				{"DELETE", "/api/api-keys/1", nil},
				{"PUT", "/api/settings/auth", map[string]any{"user": "attacker", "password": "hunter2hunter2"}},
			} {
				resp := h.withKey(key, tc.method, tc.path, tc.body)
				body := h.body(resp)
				if resp.StatusCode != http.StatusForbidden {
					t.Errorf("%s %s answered %d to a %s key, want 403", tc.method, tc.path, resp.StatusCode, scope)
				}
				if strings.Contains(body, secrets.APIKeyPrefix) {
					t.Errorf("%s %s leaked something key-shaped: %s", tc.method, tc.path, body)
				}
			}

			// Nothing got through: no key was minted, the other key still works,
			// and the operator's password is unchanged.
			keys, err := h.store.ListAPIKeys(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, existing := range keys {
				if existing.Name == "minted-by-a-key" {
					t.Error("a key created another key")
				}
			}
			resp := h.withKey(victim, "GET", "/api/pools", nil)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Error("a key revoked another key")
			}
			if resp := h.do("GET", "/api/pools", nil); resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				t.Error("the operator's password no longer works")
			} else {
				resp.Body.Close()
			}
		})
	}
}

// Checked directly as well, because this is the function that makes the rule
// impossible to forget when a route is added under one of these paths.
func TestKeysRefused(t *testing.T) {
	for _, path := range []string{
		"/api/api-keys",
		"/api/api-keys/7",
		"/api/api-keys/7/anything-added-later",
		"/api/settings/auth",
		"/api/settings/auth/whatever",
	} {
		if !keysRefused(path) {
			t.Errorf("%s should be closed to every api key", path)
		}
	}
	// And it must not swallow the routes keys exist to reach.
	for _, path := range []string{
		"/api/pools", "/api/settings", "/api/settings/budget", "/api/credentials",
		"/api/api-keysomething", "/api/runners",
	} {
		if keysRefused(path) {
			t.Errorf("%s should be reachable with a key", path)
		}
	}
}

// The operator's user name is half of the credential no key may touch, and a key
// has no business learning what to guess a password against.
func TestSettingsWithholdsTheOperatorFromAKey(t *testing.T) {
	h := newHarness(t)
	key := h.key("monitoring", model.APIKeyRead)

	var asKey map[string]any
	h.decode(h.withKey(key, "GET", "/api/settings", nil), &asKey)
	if _, present := asKey["authUser"]; present {
		t.Errorf("a key was told the operator's user name: %v", asKey["authUser"])
	}
	// The rest of it is what a key is for, so withholding the name must not have
	// emptied the response.
	if asKey["version"] != "test" {
		t.Errorf("a key was not told the version: %v", asKey)
	}
	if _, present := asKey["budget"]; !present {
		t.Error("a key was not told the budget")
	}

	// And the operator still sees it, so this is a redaction and not a removal.
	var asHuman map[string]any
	h.decode(h.do("GET", "/api/settings", nil), &asHuman)
	if asHuman["authUser"] != "admin" {
		t.Errorf("the operator was told %v, want admin", asHuman["authUser"])
	}
}

// A sweep rather than a list of specific worries: every route a key can read,
// checked for the things it must never contain. A route added later that returns
// the password hash, or a key, fails here without anyone thinking to ask.
func TestNothingAKeyCanReadContainsACredential(t *testing.T) {
	h := newHarness(t)
	pool := h.createPool("web")
	key := h.key("monitoring", model.APIKeyAdmin)
	other := h.key("another", model.APIKeyRead)

	hash, err := h.store.Setting(context.Background(), SettingAuthHash)
	if err != nil {
		t.Fatal(err)
	}
	if hash == "" {
		t.Fatal("no password hash to look for, so this test proves nothing")
	}

	forbidden := map[string]string{
		"the password hash":     hash,
		"the password":          "correct-horse",
		"the key being used":    key,
		"another key":           other,
		"the settings key name": SettingAuthHash,
		"the GitHub credential": "github_pat_test",
	}

	id := fmt.Sprint(pool.ID)
	for _, path := range []string{
		"/api/pools", "/api/pools/" + id, "/api/pools/export", "/api/runners",
		"/api/activity", "/api/jobs", "/api/pool-images", "/api/pools/" + id + "/image",
		"/api/resources", "/api/resources/history", "/api/credentials", "/api/settings",
		"/api/api-keys", "/api/health",
	} {
		resp := h.withKey(key, "GET", path, nil)
		body := h.body(resp)
		for what, secret := range forbidden {
			if strings.Contains(body, secret) {
				t.Errorf("GET %s gave a key %s", path, what)
			}
		}
		// The hint is meant to be there; the key it identifies is not.
		if strings.Contains(body, strings.TrimPrefix(key, secrets.APIKeyPrefix)) {
			t.Errorf("GET %s echoed the key it was called with", path)
		}
	}
}

// ---------------------------------------------------------------------------
// Managing keys as the operator
// ---------------------------------------------------------------------------

func TestCreateAPIKeyShowsTheKeyOnce(t *testing.T) {
	h := newHarness(t)

	var created struct {
		APIKey model.APIKey `json:"apiKey"`
		Secret string       `json:"secret"`
	}
	resp := h.do("POST", "/api/api-keys", map[string]any{"name": "ci", "scope": "admin"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("answered %d", resp.StatusCode)
	}
	h.decode(resp, &created)

	if !strings.HasPrefix(created.Secret, secrets.APIKeyPrefix) {
		t.Errorf("the key %q is not one of ours", created.Secret)
	}
	if created.APIKey.Scope != model.APIKeyAdmin {
		t.Errorf("scope is %q, want admin", created.APIKey.Scope)
	}
	// It works, which is the only thing that makes showing it once acceptable.
	check := h.withKey(created.Secret, "GET", "/api/pools", nil)
	check.Body.Close()
	if check.StatusCode != http.StatusOK {
		t.Errorf("the key just created does not work: %d", check.StatusCode)
	}

	// And it is never in a response again, for anybody.
	var listed []model.APIKey
	h.decode(h.do("GET", "/api/api-keys", nil), &listed)
	raw, err := json.Marshal(listed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), created.Secret) {
		t.Error("the list of keys gives the key back")
	}
	for _, key := range listed {
		if key.Name == "ci" && key.Hint == "" {
			t.Error("the key cannot be identified in the list at all")
		}
	}
}

func TestCreateAPIKeyDefaultsToRead(t *testing.T) {
	h := newHarness(t)
	var created struct {
		APIKey model.APIKey `json:"apiKey"`
		Secret string       `json:"secret"`
	}
	h.decode(h.do("POST", "/api/api-keys", map[string]any{"name": "ci"}), &created)

	if created.APIKey.Scope != model.APIKeyRead {
		t.Errorf("scope is %q, want read when none was asked for", created.APIKey.Scope)
	}
	// And it really is read-only, not merely labelled so.
	resp := h.withKey(created.Secret, "POST", "/api/reconcile", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a key created without a scope could write: %d", resp.StatusCode)
	}
}

func TestCreateAPIKeyWithAnExpiry(t *testing.T) {
	h := newHarness(t)
	at := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)

	var created struct {
		APIKey model.APIKey `json:"apiKey"`
		Secret string       `json:"secret"`
	}
	h.decode(h.do("POST", "/api/api-keys", map[string]any{
		"name": "ci", "scope": "read", "expiresAt": at.Format(time.RFC3339),
	}), &created)

	if created.APIKey.ExpiresAt == nil {
		t.Fatal("the expiry was dropped")
	}
	if !created.APIKey.ExpiresAt.Equal(at) {
		t.Errorf("expires at %s, want %s", created.APIKey.ExpiresAt, at)
	}
}

func TestCreateAPIKeyRejectsWhatTheStoreRefuses(t *testing.T) {
	h := newHarness(t)

	for _, tc := range []struct {
		name string
		body map[string]any
		want int
	}{
		{"no name", map[string]any{"scope": "read"}, http.StatusBadRequest},
		{"unknown scope", map[string]any{"name": "ci", "scope": "write"}, http.StatusBadRequest},
		{"expired already", map[string]any{
			"name": "ci", "expiresAt": time.Now().Add(-time.Hour).Format(time.RFC3339),
		}, http.StatusBadRequest},
		{"unknown field", map[string]any{"name": "ci", "scopes": "read"}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.do("POST", "/api/api-keys", tc.body)
			body := h.body(resp)
			if resp.StatusCode != tc.want {
				t.Errorf("answered %d, want %d: %s", resp.StatusCode, tc.want, body)
			}
			// A validation complaint has to reach the person who caused it rather
			// than becoming a 500 with nothing in it.
			if resp.StatusCode == http.StatusInternalServerError {
				t.Errorf("a bad request became a server error: %s", body)
			}
		})
	}
}

func TestCreateAPIKeyRefusesADuplicateName(t *testing.T) {
	h := newHarness(t)
	if resp := h.do("POST", "/api/api-keys", map[string]any{"name": "ci"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("answered %d", resp.StatusCode)
	}
	resp := h.do("POST", "/api/api-keys", map[string]any{"name": "ci"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("a duplicate name answered %d, want 409", resp.StatusCode)
	}
}

func TestListAPIKeysStartsEmpty(t *testing.T) {
	h := newHarness(t)
	body := h.body(h.do("GET", "/api/api-keys", nil))
	// [] and not null, so the UI has nothing to special-case.
	if strings.TrimSpace(body) != "[]" {
		t.Errorf("an empty list came back as %q", strings.TrimSpace(body))
	}
}

func TestDeleteAPIKeyThatIsNotThere(t *testing.T) {
	h := newHarness(t)
	resp := h.do("DELETE", "/api/api-keys/404", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("answered %d, want 404", resp.StatusCode)
	}
}

func TestDeleteAPIKeyNeedsAnID(t *testing.T) {
	h := newHarness(t)
	resp := h.do("DELETE", "/api/api-keys/not-a-number", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("answered %d, want 400", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// The lockout, which keys must stay out of
// ---------------------------------------------------------------------------

// The failure this feature exists partly to avoid. Wrong keys arrive from
// whatever address the reverse proxy uses, which is the same address the
// operator's browser comes from — so counting them towards the password lockout
// would let one CI job with a revoked key lock its owner out of their own fleet.
func TestWrongKeysDoNotLockTheOperatorOut(t *testing.T) {
	h := newHarness(t)

	for range maxAttempts * 3 {
		resp := h.withKey("rf_wrong-and-wrong-again", "GET", "/api/pools", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("a wrong key was answered %d", resp.StatusCode)
		}
	}

	resp := h.do("GET", "/api/pools", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("the operator was answered %d after a script hammered a bad key", resp.StatusCode)
	}

	// And a good key is not locked out either.
	key := h.key("ci", model.APIKeyRead)
	withKey := h.withKey(key, "GET", "/api/pools", nil)
	defer withKey.Body.Close()
	if withKey.StatusCode != http.StatusOK {
		t.Errorf("a valid key was answered %d after the bad ones", withKey.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// keyAuth on its own
// ---------------------------------------------------------------------------

type stubKeyStore struct {
	key     model.APIKey
	hash    string
	err     error
	touches []time.Time
}

func (s *stubKeyStore) APIKeyByHash(ctx context.Context, hash string) (model.APIKey, error) {
	if s.err != nil {
		return model.APIKey{}, s.err
	}
	if hash != s.hash {
		return model.APIKey{}, fmt.Errorf("api key: %w", store.ErrNotFound)
	}
	return s.key, nil
}

func (s *stubKeyStore) TouchAPIKey(ctx context.Context, id int64, at time.Time) error {
	s.touches = append(s.touches, at)
	return nil
}

// A client polling every second must not turn every read into a write, and the
// timestamp is for deciding which key to revoke — a question that does not need
// the last sixty seconds.
func TestKeyUseIsRecordedButThrottled(t *testing.T) {
	stub := &stubKeyStore{key: model.APIKey{ID: 4, Name: "ci", Scope: model.APIKeyRead}, hash: secrets.HashAPIKey("rf_k")}
	keys := newKeyAuth(stub)

	now := time.Now()
	keys.nowFor = func() time.Time { return now }

	if _, err := keys.verify(context.Background(), "rf_k"); err != nil {
		t.Fatal(err)
	}
	if len(stub.touches) != 1 {
		t.Fatalf("the first use wrote %d timestamps, want 1", len(stub.touches))
	}

	// Everything inside the interval is free.
	for range 50 {
		now = now.Add(time.Second)
		if _, err := keys.verify(context.Background(), "rf_k"); err != nil {
			t.Fatal(err)
		}
	}
	if len(stub.touches) != 1 {
		t.Errorf("fifty more uses wrote %d timestamps, want the first one only", len(stub.touches))
	}

	now = now.Add(touchInterval)
	if _, err := keys.verify(context.Background(), "rf_k"); err != nil {
		t.Fatal(err)
	}
	if len(stub.touches) != 2 {
		t.Errorf("a use after the interval wrote %d timestamps, want 2", len(stub.touches))
	}
}

// Bookkeeping, not state the fleet depends on: the key is valid whether or not
// the daemon manages to write down that it was used.
func TestAKeyStillWorksWhenItsUseCannotBeRecorded(t *testing.T) {
	stub := &failingTouchStore{stubKeyStore{
		key: model.APIKey{ID: 4, Name: "ci", Scope: model.APIKeyRead}, hash: secrets.HashAPIKey("rf_k"),
	}}
	keys := newKeyAuth(stub)

	if _, err := keys.verify(context.Background(), "rf_k"); err != nil {
		t.Errorf("a key was refused because its use could not be recorded: %v", err)
	}
}

type failingTouchStore struct{ stubKeyStore }

func (s *failingTouchStore) TouchAPIKey(ctx context.Context, id int64, at time.Time) error {
	return errors.New("database is locked")
}

// A database that cannot answer is not the same as a key that does not exist, and
// must not be reported as one.
func TestAKeyIsNotRefusedAsUnknownWhenTheStoreFails(t *testing.T) {
	stub := &stubKeyStore{err: errors.New("database is locked")}
	keys := newKeyAuth(stub)

	_, err := keys.verify(context.Background(), "rf_k")
	if err == nil {
		t.Fatal("a broken store let a key through")
	}
	if strings.Contains(err.Error(), "unknown") {
		t.Errorf("a broken store was reported as an unknown key: %v", err)
	}
}

func TestBearer(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   string
		ok     bool
	}{
		{"Bearer rf_abc", "rf_abc", true},
		// Schemes are case-insensitive in the spec, and clients differ.
		{"bearer rf_abc", "rf_abc", true},
		{"BEARER rf_abc", "rf_abc", true},
		{"Bearer  rf_abc  ", "rf_abc", true},
		{"", "", false},
		{"Bearer", "", false},
		{"Bearer ", "", false},
		{"Bearer   ", "", false},
		{"Basic YWRtaW46eA==", "", false},
		{"Token rf_abc", "", false},
	} {
		req, err := http.NewRequest("GET", "http://x/api/pools", nil)
		if err != nil {
			t.Fatal(err)
		}
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		got, ok := bearer(req)
		if ok != tc.ok || got != tc.want {
			t.Errorf("bearer(%q) = %q, %v; want %q, %v", tc.header, got, ok, tc.want, tc.ok)
		}
	}
}

// A handler reached without the middleware — which should be impossible — is
// treated as a key with no rights rather than as the operator.
func TestPrincipalDefaultsToTheSafeEnd(t *testing.T) {
	req, err := http.NewRequest("GET", "http://x/api/settings", nil)
	if err != nil {
		t.Fatal(err)
	}
	if principalOf(req).Human() {
		t.Error("a request that never authenticated was taken for a person")
	}
}

// ---------------------------------------------------------------------------
// A host with no password on it
// ---------------------------------------------------------------------------

// What makes a headless install possible: provisioned with a key from the CLI and
// never opened in a browser. The gate that refuses everything until a password is
// set exists because a daemon with no password would otherwise serve the fleet to
// anyone who asked, and a valid key is the proof of authorisation it is looking
// for.
func TestAKeyWorksBeforeAPasswordIsSet(t *testing.T) {
	dir := t.TempDir()
	ring, err := secrets.LoadOrCreateKey(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "fleet.db"), ring)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	srv := New(Options{
		Store: db, Fleet: &stubFleet{}, Version: "test",
		UI: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>fleet</html>")}},
	})
	server := httptest.NewServer(srv.Handler())
	t.Cleanup(server.Close)

	if srv.Auth().Configured(context.Background()) {
		t.Fatal("this daemon was supposed to have no password")
	}

	_, key, err := db.CreateAPIKey(context.Background(),
		model.APIKey{Name: "provisioning", Scope: model.APIKeyAdmin})
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest("GET", server.URL+"/api/pools", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("a key was answered %d on a daemon with no password", resp.StatusCode)
	}

	// The gate itself is untouched: without a key there is still no way in.
	plain, err := server.Client().Get(server.URL + "/api/pools")
	if err != nil {
		t.Fatal(err)
	}
	plain.Body.Close()
	if plain.StatusCode != http.StatusUnauthorized {
		t.Errorf("a daemon with no password answered %d to a request with no credentials", plain.StatusCode)
	}
}
