package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/imagebuild"
	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/reconcile"
	"github.com/clems4ever/github-runner/internal/resources"
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

// stubResources stands in for the sampler, which reads the host on a timer.
type stubResources struct {
	report resources.Report
	ready  bool
}

func (r *stubResources) Latest() (resources.Report, bool) { return r.report, r.ready }

// stubImages stands in for the builder, which only exists on a host that
// builds machines.
type stubImages struct {
	state   map[string]imagebuild.State
	history []imagebuild.Build
	log     string
	// asked is the pools somebody pressed the button for, and forgotten the
	// ones that were deleted.
	asked     []string
	forgotten []string
	busy      bool
	err       error
}

func (i *stubImages) Status(pool model.Pool) imagebuild.Status {
	state := i.state[pool.Name]
	if state == "" {
		state = imagebuild.StateUnbuilt
	}
	return imagebuild.Status{
		Pool: pool.Name, Image: "runner-noble-default-abc123", State: state,
		Ready: state == imagebuild.StateReady, Summary: "as it stands",
	}
}

func (i *stubImages) History(ctx context.Context, pool string, limit int) ([]imagebuild.Build, error) {
	return i.history, i.err
}

func (i *stubImages) Log(ctx context.Context, id int64, maxBytes int64) (string, error) {
	return i.log, i.err
}

func (i *stubImages) Forget(ctx context.Context, pool string) error {
	i.forgotten = append(i.forgotten, pool)
	return nil
}

func (i *stubImages) Rebuild(ctx context.Context, pool model.Pool) (imagebuild.Build, error) {
	if i.busy {
		return imagebuild.Build{}, imagebuild.ErrBusy
	}
	i.asked = append(i.asked, pool.Name)
	return imagebuild.Build{ImageBuild: model.ImageBuild{ID: 7, Pool: pool.Name, Phase: model.ImageQueued}}, nil
}

type harness struct {
	t           *testing.T
	server      *httptest.Server
	store       *store.Store
	fleet       *stubFleet
	nudges      int
	resources   *stubResources
	images      *stubImages
	credID      int64
	checkAccess func(context.Context, int64, github.Scope) error
	// forgot is the pools whose cached layer answer the server dropped, which
	// is how a decision made in the UI takes effect now rather than at the
	// resolver's next reading.
	forgot []string
}

// Forget makes the harness itself the layer resolver.
func (h *harness) Forget(pool string) { h.forgot = append(h.forgot, pool) }

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

	h := &harness{t: t, store: db, fleet: &stubFleet{}, resources: &stubResources{},
		images: &stubImages{state: map[string]imagebuild.State{}}}
	srv := New(Options{
		Store: db, Fleet: h.fleet, Resources: h.resources, Images: h.images, Layers: h,
		Version: "test",
		UI:      fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>fleet</html>")}},
		Nudge:   func() { h.nudges++ },
		CheckAccess: func(ctx context.Context, id int64, scope github.Scope) error {
			if h.checkAccess == nil {
				return nil
			}
			return h.checkAccess(ctx, id, scope)
		},
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

// createPool makes one and hands back what the daemon stored, which is where
// its id comes from.
func (h *harness) createPool(name string) model.Pool {
	h.t.Helper()
	body := h.samplePool()
	body["name"] = name
	var created model.Pool
	h.decode(h.do("POST", "/api/pools", body), &created)
	return created
}

// Nothing but health may be reachable without credentials: this daemon can
// create machines and holds a token that administers repositories.
func TestEverythingNeedsCredentials(t *testing.T) {
	h := newHarness(t)

	for _, path := range []string{"/api/pools", "/api/pools/export", "/api/runners", "/api/credentials",
		"/api/resources", "/api/resources/history", "/api/settings", "/"} {
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

// The pools list can resize a pool without opening its definition, which sends
// the pool back with new bounds and nothing else touched. Two things have to
// hold for that to be safe: the rest of the pool survives the round trip, and
// the daemon acts on it now rather than at whatever the next tick happens to
// be — a row that sits unchanged after a click reads as a broken button.
func TestScalingAPoolLeavesTheRestOfItAlone(t *testing.T) {
	h := newHarness(t)

	var created model.Pool
	h.decode(h.do("POST", "/api/pools", h.samplePool()), &created)

	nudgesBefore := h.nudges
	update := h.samplePool()
	update["minReplicas"] = 3
	update["maxReplicas"] = 3

	var updated model.Pool
	h.decode(h.do("PUT", "/api/pools/"+itoa(created.ID), update), &updated)

	if updated.MinReplicas != 3 || updated.MaxReplicas != 3 {
		t.Fatalf("got %d–%d, want a pool of a fixed three", updated.MinReplicas, updated.MaxReplicas)
	}
	if updated.Runtime != created.Runtime || updated.Scope != created.Scope ||
		updated.CPUs != created.CPUs || updated.MemoryMB != created.MemoryMB {
		t.Fatalf("scaling changed something it was not asked to: %+v", updated)
	}
	if h.nudges == nudgesBefore {
		t.Error("scaling did not ask for a reconcile, so nothing would happen until the next tick")
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

func TestActivityCanBeNarrowedToOneScope(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	if err := h.store.RecordSamples(context.Background(), now.Add(-time.Minute), []model.Sample{
		{Pool: "web", Scope: "acme/site", Running: 3, Busy: 2},
		{Pool: "web-arm", Scope: "acme/site", Running: 1, Busy: 1},
		{Pool: "api", Scope: "acme/api", Running: 7, Busy: 5},
	}); err != nil {
		t.Fatal(err)
	}

	payload := readAll(t, h.do("GET", "/api/activity?hours=1&scope=acme%2Fsite", nil))
	if !strings.Contains(payload, `"running":4`) {
		t.Fatalf("want both pools on acme/site added together: %s", payload)
	}
	if !strings.Contains(payload, `"scope":"acme/site"`) {
		t.Fatalf("the response does not say what it was narrowed to: %s", payload)
	}

	// The scopes on offer come back whatever the filter is, or a reader who
	// narrowed to one repository could not leave it again.
	for _, want := range []string{`"acme/api"`, `"acme/site"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("the response is missing the scope %s: %s", want, payload)
		}
	}
}

// A scope nothing was ever recorded against is empty, not an error.
func TestActivityForAnUnknownScope(t *testing.T) {
	h := newHarness(t)
	resp := h.do("GET", "/api/activity?hours=1&scope=nobody%2Fnothing", nil)
	payload := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("answered %d: %s", resp.StatusCode, payload)
	}
	if !strings.Contains(payload, `"points":[]`) {
		t.Fatalf("got %s", payload)
	}
}

func TestActivityDefaultsToARecentWindow(t *testing.T) {
	h := newHarness(t)
	payload := readAll(t, h.do("GET", "/api/activity", nil))
	if !strings.Contains(payload, `"points"`) {
		t.Fatalf("got %s", payload)
	}
}

// A pool whose credential cannot serve it is refused while someone is looking
// at the form, rather than created and left failing in a log a minute later.
func TestAPoolTheCredentialCannotServeIsRefused(t *testing.T) {
	h := newHarness(t)
	h.checkAccess = func(ctx context.Context, credentialID int64, scope github.Scope) error {
		return &github.Error{
			Status: http.StatusNotFound, Scope: scope.Path, Message: "Not Found",
			Advice:   "the app is authenticated but has no installation covering " + scope.Path,
			GrantURL: "https://github.com/settings/installations/42",
		}
	}

	resp := h.do("POST", "/api/pools", h.samplePool())
	payload := readAll(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("answered %d: %s", resp.StatusCode, payload)
	}
	// The page that fixes it travels with the refusal: an app cannot widen its
	// own access, so the most the daemon can do is put that one click away.
	if !strings.Contains(payload, "settings/installations/42") {
		t.Fatalf("no way to fix it was offered: %s", payload)
	}
	if !strings.Contains(payload, "no installation covering") {
		t.Fatalf("the reason was lost: %s", payload)
	}

	// And nothing was stored, so there is no half-working pool to clean up.
	pools := readAll(t, h.do("GET", "/api/pools", nil))
	if strings.Contains(pools, `"name":"web"`) {
		t.Fatalf("the pool was created anyway: %s", pools)
	}
}

// A daemon that cannot reach GitHub must not stop someone configuring their
// fleet: the pool will work when the network does.
func TestAPoolIsAllowedWhenGitHubCannotBeReached(t *testing.T) {
	h := newHarness(t)
	h.checkAccess = func(context.Context, int64, github.Scope) error {
		return errors.New("dial tcp: no route to host")
	}
	resp := h.do("POST", "/api/pools", h.samplePool())
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("a network problem blocked configuration: %d", resp.StatusCode)
	}
}

// A pool that is switched off is not going to register anything, so there is
// nothing to check and no reason to refuse it.
func TestADisabledPoolIsNotChecked(t *testing.T) {
	h := newHarness(t)
	var asked bool
	h.checkAccess = func(context.Context, int64, github.Scope) error {
		asked = true
		return &github.Error{Status: http.StatusNotFound, Message: "Not Found"}
	}
	pool := h.samplePool()
	pool["enabled"] = false

	resp := h.do("POST", "/api/pools", pool)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("answered %d", resp.StatusCode)
	}
	if asked {
		t.Error("it asked GitHub about a pool that is switched off")
	}
}

// The lockout took the operator out of their own dashboard: "too many failed
// attempts; try again shortly", in a browser, with the right password.
//
// Behind a reverse proxy every request arrives from the same address, so one
// script guessing at a public name locks out everybody — a denial of service
// delivered by the defence. And the right password being refused protects
// nobody: whoever sent it can already have everything behind this.
func TestTheRightPasswordWorksEvenAfterAFloodOfWrongOnes(t *testing.T) {
	h := newHarness(t)

	for i := 0; i < maxAttempts*2; i++ {
		req, _ := http.NewRequest("GET", h.server.URL+"/api/pools", nil)
		req.SetBasicAuth("admin", "not-the-password")
		resp, err := h.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	// Guessing is still refused, which is what the lockout is for.
	req, _ := http.NewRequest("GET", h.server.URL+"/api/pools", nil)
	req.SetBasicAuth("admin", "still-not-the-password")
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a client that has guessed %d times got %d, and should be refused",
			maxAttempts*2, resp.StatusCode)
	}

	// The operator is not.
	resp = h.do("GET", "/api/pools", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the right password was refused with %d; the lockout is locking out the wrong person",
			resp.StatusCode)
	}
}

// A browser's first request to a protected page carries no credentials at all.
// That is how Basic auth starts, not an attempt at guessing, and counting it
// meant ten page loads could lock somebody out.
func TestAskingWithoutCredentialsIsNotAGuess(t *testing.T) {
	h := newHarness(t)

	for i := 0; i < maxAttempts*2; i++ {
		resp, err := h.server.Client().Get(h.server.URL + "/api/pools")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("request %d answered %d; a browser that has not logged in yet must be"+
				" challenged, not locked out", i+1, resp.StatusCode)
		}
		if resp.Header.Get("WWW-Authenticate") == "" {
			t.Fatal("no login box was offered")
		}
	}

	// And the fleet is still reachable afterwards.
	resp := h.do("GET", "/api/pools", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d after a browser knocked without credentials", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Resources
// ---------------------------------------------------------------------------

func percent(v float64) *float64 { return &v }

// The daemon has been up for less than one sample. That is not an error and it
// is not a host with no memory, so it says which of the two it is.
func TestResourcesSaysWhenNothingHasBeenMeasuredYet(t *testing.T) {
	h := newHarness(t)

	resp := h.do(http.MethodGet, "/api/resources", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var body struct {
		Ready bool `json:"ready"`
		Host  *struct {
			CPUs int `json:"cpus"`
		} `json:"host"`
	}
	h.decode(resp, &body)
	if body.Ready {
		t.Fatal("a daemon that has not measured anything says it has")
	}
	if body.Host != nil {
		t.Fatalf("a host was drawn from no measurement: %+v", body.Host)
	}
}

func TestResourcesReportsTheHostAndItsRunners(t *testing.T) {
	h := newHarness(t)
	h.resources.ready = true
	h.resources.report = resources.Report{
		At: time.Now().UTC(),
		Host: resources.Host{
			CPUs: 8, CPUPercent: 42.5,
			MemoryUsedBytes: 4 << 30, MemoryTotalBytes: 16 << 30,
			DiskPath: "/var/lib/runner-fleet", DiskUsedBytes: 100 << 30, DiskTotalBytes: 400 << 30,
			Load1: 1.5,
		},
		Runners: []resources.RunnerUsage{
			{Name: "web-1", Pool: "web", Runtime: "vm", CPUPercent: percent(12.5), MemoryBytes: 2 << 30},
			// Seen once, so there is no rate for it yet.
			{Name: "api-1", Pool: "api", Runtime: "container", MemoryBytes: 512 << 20},
		},
		Warnings: []string{"is dockerd running?"},
	}

	var body struct {
		Ready    bool                    `json:"ready"`
		Host     resources.Host          `json:"host"`
		Runners  []resources.RunnerUsage `json:"runners"`
		Warnings []string                `json:"warnings"`
	}
	h.decode(h.do(http.MethodGet, "/api/resources", nil), &body)

	if !body.Ready || body.Host.CPUs != 8 || body.Host.CPUPercent != 42.5 {
		t.Fatalf("host: %+v", body.Host)
	}
	if len(body.Runners) != 2 {
		t.Fatalf("runners: %+v", body.Runners)
	}
	if body.Runners[0].CPUPercent == nil || *body.Runners[0].CPUPercent != 12.5 {
		t.Fatalf("the measured runner lost its figure: %+v", body.Runners[0])
	}
	// A rate needs two readings. Zero would show a machine mid-boot as idle,
	// so the field is absent rather than false.
	if body.Runners[1].CPUPercent != nil {
		t.Fatalf("an unmeasured runner was given a figure: %v", *body.Runners[1].CPUPercent)
	}
	if len(body.Warnings) != 1 {
		t.Fatalf("the runtime that could not answer was swallowed: %v", body.Warnings)
	}
}

// What the pools would take if every one of them grew at once. It is a
// different question from what is being used, and the answer is the one nobody
// is watching for.
func TestResourcesSaysWhatThePoolsHavePromised(t *testing.T) {
	h := newHarness(t)
	h.resources.ready = true

	pool := h.samplePool()
	pool["cpus"] = 4
	pool["memoryMb"] = 8192
	pool["diskGb"] = 40
	pool["minReplicas"] = 1
	pool["maxReplicas"] = 3
	if resp := h.do(http.MethodPost, "/api/pools", pool); resp.StatusCode != http.StatusCreated {
		t.Fatalf("could not create the pool: %d", resp.StatusCode)
	}

	var body struct {
		Committed model.Commitment `json:"committed"`
	}
	h.decode(h.do(http.MethodGet, "/api/resources", nil), &body)

	if body.Committed.Runners != 3 || body.Committed.CPUs != 12 {
		t.Fatalf("committed: %+v", body.Committed)
	}
	if body.Committed.MemoryBytes != 3*8192*1024*1024 {
		t.Fatalf("committed memory: %d", body.Committed.MemoryBytes)
	}
}

func TestResourceHistoryIsBucketedOverTheWindowAsked(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	for i := range 3 {
		if err := h.store.RecordHostSample(context.Background(), now.Add(-time.Duration(i)*time.Minute),
			model.HostSample{CPUPercent: float64(10 * (i + 1)), MemoryUsedBytes: 1, MemoryTotalBytes: 4}); err != nil {
			t.Fatal(err)
		}
	}

	var body struct {
		Points []model.HostPoint `json:"points"`
	}
	h.decode(h.do(http.MethodGet, "/api/resources/history?hours=1", nil), &body)

	if len(body.Points) == 0 {
		t.Fatal("no history came back")
	}
	for _, point := range body.Points {
		if point.MemoryPercent != 25 {
			t.Fatalf("memory should be a share of the total: %+v", point)
		}
	}
}

func TestResourceHistoryRefusesAWindowItCannotServe(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodGet, "/api/resources/history?hours=999", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// The signal the pools table shows against every row: whether this pool's
// image is built, and so whether it can take a job at all.
func TestEveryPoolSaysWhereItsImageStands(t *testing.T) {
	h := newHarness(t)
	pool := h.samplePool()
	pool["name"] = "web"
	h.do("POST", "/api/pools", pool).Body.Close()
	h.images.state["web"] = imagebuild.StateBuilding

	var got []imagebuild.Status
	h.decode(h.do("GET", "/api/pool-images", nil), &got)
	if len(got) != 1 {
		t.Fatalf("got %d statuses", len(got))
	}
	if got[0].Pool != "web" || got[0].State != imagebuild.StateBuilding || got[0].Ready {
		t.Fatalf("got %+v", got[0])
	}
}

// The history is the part that was missing. A failed build used to be replaced
// by the next attempt at the same thing, so what a recipe did survived only
// until something tried again.
func TestAPoolsImageCarriesEveryAttemptAtIt(t *testing.T) {
	h := newHarness(t)
	created := h.createPool("web")
	h.images.history = []imagebuild.Build{
		{ImageBuild: model.ImageBuild{ID: 2, Pool: "web", Phase: model.ImageSucceeded}},
		{ImageBuild: model.ImageBuild{ID: 1, Pool: "web", Phase: model.ImageFailed,
			Error: "the recipe exited 1"}, HasLog: true},
	}

	var got struct {
		Status imagebuild.Status  `json:"status"`
		Builds []imagebuild.Build `json:"builds"`
	}
	h.decode(h.do("GET", fmt.Sprintf("/api/pools/%d/image", created.ID), nil), &got)
	if len(got.Builds) != 2 {
		t.Fatalf("got %d builds", len(got.Builds))
	}
	if got.Builds[1].Error != "the recipe exited 1" {
		t.Fatalf("the failure was not kept: %+v", got.Builds[1])
	}
}

// A build that failed is not retried on its own, so there has to be a way to
// ask for another one.
func TestABuildCanBeAskedFor(t *testing.T) {
	h := newHarness(t)
	created := h.createPool("web")

	resp := h.do("POST", fmt.Sprintf("/api/pools/%d/image/builds", created.ID), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(h.images.asked) != 1 || h.images.asked[0] != "web" {
		t.Fatalf("the builder was asked for %v", h.images.asked)
	}
	// The fleet is told, so the pool picks its runners up as soon as the image
	// is there rather than at the next tick.
	if h.nudges == 0 {
		t.Error("nothing asked the fleet to reconcile after a build was requested")
	}
}

// Two of the same build is a working directory two processes are fighting
// over. Asking again while one is running is refused, not queued.
func TestABuildAlreadyHappeningIsRefused(t *testing.T) {
	h := newHarness(t)
	created := h.createPool("web")
	h.images.busy = true

	resp := h.do("POST", fmt.Sprintf("/api/pools/%d/image/builds", created.ID), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// A deleted pool's builds are unreachable — the history is filed under the
// pool — and each of their logs is a console worth megabytes.
func TestDeletingAPoolForgetsItsBuilds(t *testing.T) {
	h := newHarness(t)
	created := h.createPool("web")

	h.do("DELETE", fmt.Sprintf("/api/pools/%d", created.ID), nil).Body.Close()
	if len(h.images.forgotten) != 1 || h.images.forgotten[0] != "web" {
		t.Fatalf("the builder was told to forget %v", h.images.forgotten)
	}
}

// The log is a console. It is read, not parsed.
func TestABuildLogIsServedAsText(t *testing.T) {
	h := newHarness(t)
	h.images.log = "==> building runner-noble-default-abc123\ncloud-init running\n"

	resp := h.do("GET", "/api/image-builds/3/log", nil)
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("content type %q", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "cloud-init running") {
		t.Fatalf("got %q", body)
	}
}

// A container-only fleet builds nothing, and every pool on it says so rather
// than the page having to handle an error.
func TestADaemonThatBuildsNothing(t *testing.T) {
	h := newHarness(t)
	h.createPool("web")
	// Rebuilt without a builder at all, which is how a daemon that cannot
	// build machines is configured.
	srv := New(Options{Store: h.store, Fleet: h.fleet})
	if err := srv.Auth().SetPassword(context.Background(), "admin", "correct-horse"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	req, _ := http.NewRequest("GET", server.URL+"/api/pool-images", nil)
	req.SetBasicAuth("admin", "correct-horse")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("got %d", resp.StatusCode)
	}
	var got []imagebuild.Status
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].State != imagebuild.StateNone || !got[0].Ready {
		t.Fatalf("got %+v", got)
	}
}
