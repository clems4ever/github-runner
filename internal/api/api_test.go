package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/reconcile"
	"github.com/clems4ever/github-runner/internal/secrets"
	"github.com/clems4ever/github-runner/internal/store"
)

type stubFleet struct {
	status   []reconcile.RunnerStatus
	warnings []string
	scaling  map[string]reconcile.Scale
	passes   int
}

func (f *stubFleet) Status(ctx context.Context) ([]reconcile.RunnerStatus, []string) {
	return f.status, f.warnings
}
func (f *stubFleet) Once(ctx context.Context) reconcile.Result {
	f.passes++
	return reconcile.Result{}
}
func (f *stubFleet) Scaling() map[string]reconcile.Scale { return f.scaling }

type harness struct {
	t      *testing.T
	server *httptest.Server
	store  *store.Store
	fleet  *stubFleet
	nudges int
	credID int64
}

func newHarness(t *testing.T) *harness {
	t.Helper()
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

	h := &harness{t: t, store: db, fleet: &stubFleet{}}
	srv := New(Options{
		Store: db, Fleet: h.fleet, Version: "test",
		UI:    fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>fleet</html>")}},
		Nudge: func() { h.nudges++ },
	})
	if err := srv.Auth().SetPassword(context.Background(), "admin", "correct-horse"); err != nil {
		t.Fatal(err)
	}
	h.server = httptest.NewServer(srv.Handler())
	t.Cleanup(h.server.Close)

	cred, err := db.CreateCredential(context.Background(), model.Credential{Name: "pat"}, "github_pat_test")
	if err != nil {
		t.Fatal(err)
	}
	h.credID = cred.ID
	return h
}

// do makes an authenticated request.
func (h *harness) do(method, path string, body any) *http.Response {
	h.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		h.t.Fatal(err)
	}
	req.SetBasicAuth("admin", "correct-horse")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func (h *harness) decode(resp *http.Response, into any) {
	h.t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		h.t.Fatalf("decode: %v", err)
	}
}

func (h *harness) samplePool() map[string]any {
	return map[string]any{
		"name": "web", "scopeKind": "repository", "scope": "clems4ever/runyard",
		"runtime": "vm", "minReplicas": 2, "maxReplicas": 4, "labels": []string{"gpu"},
		"credentialId": h.credID, "enabled": true,
	}
}

// Nothing but health may be reachable without credentials: this daemon can
// create machines and holds a token that administers repositories.
func TestEverythingNeedsCredentials(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{"/api/pools", "/api/runners", "/api/credentials", "/api/settings", "/"} {
		resp, err := h.server.Client().Get(h.server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s answered %d without credentials", path, resp.StatusCode)
		}
		if resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s did not ask the browser to log in", path)
		}
	}
}

func TestHealthIsOpen(t *testing.T) {
	h := newHarness(t)
	resp, err := h.server.Client().Get(h.server.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" || body["configured"] != true {
		t.Fatalf("got %v", body)
	}
}

func TestTheWrongPasswordIsRefused(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("GET", h.server.URL+"/api/pools", nil)
	req.SetBasicAuth("admin", "wrong")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestTheWrongUserIsRefused(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest("GET", h.server.URL+"/api/pools", nil)
	req.SetBasicAuth("someone", "correct-horse")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

// A daemon with no password set serves nothing, rather than serving the fleet
// to whoever asks first.
func TestAnUnconfiguredDaemonRefusesEverything(t *testing.T) {
	dir := t.TempDir()
	ring, _ := secrets.LoadOrCreateKey(filepath.Join(dir, "master.key"))
	db, err := store.Open(filepath.Join(dir, "fleet.db"), ring)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	srv := httptest.NewServer(New(Options{Store: db, Fleet: &stubFleet{}}).Handler())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/pools", nil)
	req.SetBasicAuth("admin", "anything")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestRepeatedFailuresAreThrottled(t *testing.T) {
	h := newHarness(t)
	var lastStatus int
	for i := 0; i < maxAttempts+2; i++ {
		req, _ := http.NewRequest("GET", h.server.URL+"/api/pools", nil)
		req.SetBasicAuth("admin", "wrong")
		resp, err := h.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		lastStatus = resp.StatusCode
	}
	if lastStatus != http.StatusTooManyRequests {
		t.Fatalf("after %d wrong guesses the answer is still %d", maxAttempts+2, lastStatus)
	}
}

func TestPoolLifecycle(t *testing.T) {
	h := newHarness(t)

	resp := h.do("POST", "/api/pools", h.samplePool())
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create answered %d", resp.StatusCode)
	}
	var created model.Pool
	h.decode(resp, &created)
	if created.ID == 0 || created.Name != "web" || created.MinReplicas != 2 || created.MaxReplicas != 4 {
		t.Fatalf("got %+v", created)
	}
	// Defaults are filled in server-side, so the UI does not have to know them.
	if created.CPUs == 0 || created.MemoryMB == 0 {
		t.Fatalf("no defaults applied: %+v", created)
	}
	if h.nudges == 0 {
		t.Error("creating a pool did not ask for a reconcile, so nothing would happen until the next tick")
	}

	resp = h.do("GET", "/api/pools", nil)
	var pools []model.Pool
	h.decode(resp, &pools)
	if len(pools) != 1 {
		t.Fatalf("got %d pools", len(pools))
	}

	update := h.samplePool()
	update["maxReplicas"] = 5
	update["nested"] = true
	resp = h.do("PUT", "/api/pools/"+itoa(created.ID), update)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update answered %d", resp.StatusCode)
	}
	var updated model.Pool
	h.decode(resp, &updated)
	if updated.MaxReplicas != 5 || !updated.Nested {
		t.Fatalf("got %+v", updated)
	}

	resp = h.do("DELETE", "/api/pools/"+itoa(created.ID), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete answered %d", resp.StatusCode)
	}
}

func TestPoolValidationReachesTheOperator(t *testing.T) {
	h := newHarness(t)
	bad := h.samplePool()
	bad["name"] = "Not A Name"

	resp := h.do("POST", "/api/pools", bad)
	var body map[string]string
	h.decode(resp, &body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 with an explanation", resp.StatusCode)
	}
	if !strings.Contains(body["error"], "name") {
		t.Fatalf("the message does not say what is wrong: %q", body["error"])
	}
}

func TestDuplicateNameIsAConflict(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/api/pools", h.samplePool()).Body.Close()
	resp := h.do("POST", "/api/pools", h.samplePool())
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestUnknownPool(t *testing.T) {
	h := newHarness(t)
	resp := h.do("GET", "/api/pools/999", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

// A typo in a field name must not look like it worked.
func TestUnknownFieldsAreRejected(t *testing.T) {
	h := newHarness(t)
	body := h.samplePool()
	body["epehmeral"] = true // deliberate typo
	resp := h.do("POST", "/api/pools", body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestCredentialsNeverComeBackOut(t *testing.T) {
	h := newHarness(t)

	resp := h.do("POST", "/api/credentials", map[string]string{
		"name": "another", "secret": "github_pat_SECRETVALUE",
	})
	raw := readAll(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("got %d: %s", resp.StatusCode, raw)
	}
	if strings.Contains(raw, "SECRETVALUE") {
		t.Fatalf("the token came back in the response: %s", raw)
	}

	resp = h.do("GET", "/api/credentials", nil)
	raw = readAll(t, resp)
	if strings.Contains(raw, "SECRETVALUE") || strings.Contains(raw, "sealed") {
		t.Fatalf("the credential list leaks the token: %s", raw)
	}
	// A hint is enough to tell two apart.
	if !strings.Contains(raw, "ALUE") {
		t.Fatalf("no hint to tell credentials apart: %s", raw)
	}
}

func TestRotatingACredentialAsksForAReconcile(t *testing.T) {
	h := newHarness(t)
	before := h.nudges
	resp := h.do("PUT", "/api/credentials/"+itoa(h.credID)+"/secret", map[string]string{
		"secret": "github_pat_rotated",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if h.nudges == before {
		t.Error("rotating a token did not ask for a reconcile, so runners would keep the old one")
	}
}

func TestDeletingACredentialInUseIsRefused(t *testing.T) {
	h := newHarness(t)
	h.do("POST", "/api/pools", h.samplePool()).Body.Close()

	resp := h.do("DELETE", "/api/credentials/"+itoa(h.credID), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("got %d, want a refusal that names the reason", resp.StatusCode)
	}
}

func TestRunners(t *testing.T) {
	h := newHarness(t)
	h.fleet.status = []reconcile.RunnerStatus{
		{Name: "web-1", Pool: "web", Runtime: "vm", State: reconcile.StateRunning, Job: "busy", UpToDate: true},
	}
	h.fleet.warnings = []string{"pool web: GitHub is unreachable"}

	resp := h.do("GET", "/api/runners", nil)
	var body struct {
		Runners  []reconcile.RunnerStatus `json:"runners"`
		Warnings []string                 `json:"warnings"`
	}
	h.decode(resp, &body)
	if len(body.Runners) != 1 || body.Runners[0].Job != "busy" {
		t.Fatalf("got %+v", body.Runners)
	}
	// Warnings are shown rather than swallowed: a fleet that cannot reach
	// GitHub still works, and the operator should know.
	if len(body.Warnings) != 1 {
		t.Fatalf("warnings were dropped: %+v", body)
	}
}

// A pool that resized itself has to be able to say why.
func TestRunnersCarryTheScalingDecisions(t *testing.T) {
	h := newHarness(t)
	h.fleet.scaling = map[string]reconcile.Scale{
		"web": {Target: 3, Floor: 1, Ceiling: 5, Reason: "every runner is busy", ScaledUp: true},
	}

	payload := readAll(t, h.do("GET", "/api/runners", nil))
	for _, want := range []string{`"target":3`, `"floor":1`, `"ceiling":5`, "every runner is busy"} {
		if !strings.Contains(payload, want) {
			t.Errorf("the response is missing %q: %s", want, payload)
		}
	}
}

func TestReconcileNow(t *testing.T) {
	h := newHarness(t)
	resp := h.do("POST", "/api/reconcile", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || h.fleet.passes != 1 {
		t.Fatalf("status %d, passes %d", resp.StatusCode, h.fleet.passes)
	}
}

func TestChangingThePassword(t *testing.T) {
	h := newHarness(t)
	resp := h.do("PUT", "/api/settings/auth", map[string]string{
		"user": "clement", "password": "a-much-better-password",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("got %d", resp.StatusCode)
	}

	// The old credentials stop working immediately.
	resp = h.do("GET", "/api/pools", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the old password still works: %d", resp.StatusCode)
	}

	req, _ := http.NewRequest("GET", h.server.URL+"/api/pools", nil)
	req.SetBasicAuth("clement", "a-much-better-password")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the new password does not work: %d", resp.StatusCode)
	}
}

func TestShortPasswordsAreRefused(t *testing.T) {
	h := newHarness(t)
	resp := h.do("PUT", "/api/settings/auth", map[string]string{"user": "admin", "password": "short"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestTheUIIsServed(t *testing.T) {
	h := newHarness(t)
	resp := h.do("GET", "/", nil)
	body := readAll(t, resp)
	if !strings.Contains(body, "fleet") {
		t.Fatalf("got %q", body)
	}

	// A deep link the browser asks for directly has to load the app, not 404.
	resp = h.do("GET", "/pools/3", nil)
	body = readAll(t, resp)
	if !strings.Contains(body, "fleet") {
		t.Fatalf("a deep link did not serve the app: %q", body)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t)
	resp := h.do("GET", "/api/pools", nil)
	resp.Body.Close()
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("headers are %v", resp.Header)
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }

func TestActivity(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	if err := h.store.RecordSamples(context.Background(), now.Add(-time.Minute),
		[]model.Sample{{Pool: "web", Running: 3, Busy: 2, Target: 3}}); err != nil {
		t.Fatal(err)
	}

	payload := readAll(t, h.do("GET", "/api/activity?hours=1", nil))
	for _, want := range []string{`"running":3`, `"busy":2`, `"since"`, `"until"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("the response is missing %q: %s", want, payload)
		}
	}
}

func TestActivityRejectsAnAbsurdWindow(t *testing.T) {
	h := newHarness(t)
	for _, query := range []string{"?hours=0", "?hours=1000", "?hours=nonsense"} {
		resp := h.do("GET", "/api/activity"+query, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400", query, resp.StatusCode)
		}
	}
}

func TestActivityCanBeNarrowedToOnePool(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	if err := h.store.RecordSamples(context.Background(), now.Add(-time.Minute), []model.Sample{
		{Pool: "web", Running: 3, Busy: 2},
		{Pool: "api", Running: 7, Busy: 5},
	}); err != nil {
		t.Fatal(err)
	}

	payload := readAll(t, h.do("GET", "/api/activity?hours=1&pool=api", nil))
	if !strings.Contains(payload, `"running":7`) {
		t.Fatalf("want only the api pool: %s", payload)
	}
	if strings.Contains(payload, `"running":10`) {
		t.Fatalf("the pools were added together despite the filter: %s", payload)
	}
}

func TestActivityDefaultsToARecentWindow(t *testing.T) {
	h := newHarness(t)
	payload := readAll(t, h.do("GET", "/api/activity", nil))
	if !strings.Contains(payload, `"points"`) {
		t.Fatalf("got %s", payload)
	}
}
