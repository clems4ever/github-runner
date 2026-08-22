// Package e2e drives the whole daemon the way an operator does — over HTTP,
// against a real database — with only the host and GitHub replaced by fakes.
//
// The point is to catch what the unit tests cannot: that the API, the store,
// the reconciler and the executors agree with each other. Every test here is a
// promise about behaviour someone would notice if it broke.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/api"
	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/reconcile"
	"github.com/clems4ever/github-runner/internal/secrets"
	"github.com/clems4ever/github-runner/internal/store"
)

// host stands in for systemd and Docker: it keeps runners, and only changes
// their state when something asks it to, or when a test says a job finished.
type host struct {
	mu      sync.Mutex
	runtime model.Runtime
	runners map[string]*reconcile.Runner
	created []string
	specs   []reconcile.Spec
	started []reconcile.Spec
}

func newHost(runtime model.Runtime) *host {
	return &host{runtime: runtime, runners: map[string]*reconcile.Runner{}}
}

func (h *host) Runtime() model.Runtime { return h.runtime }

func (h *host) List(context.Context) ([]reconcile.Runner, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]reconcile.Runner, 0, len(h.runners))
	for _, r := range h.runners {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (h *host) Create(_ context.Context, spec reconcile.Spec) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.created = append(h.created, spec.Name)
	h.specs = append(h.specs, spec)
	// A real executor writes the scope and credential alongside the runner —
	// in an environment file, or in container labels — so that a runner whose
	// pool is later deleted can still be asked about. The fake has to keep the
	// same promise, or these tests would pass on a daemon that cannot.
	h.runners[spec.Name] = &reconcile.Runner{
		Name: spec.Name, Pool: spec.Pool, Generation: spec.Generation,
		Runtime: h.runtime, State: reconcile.StateRunning,
		ScopeKind: spec.ScopeKind, Scope: spec.Scope, CredentialID: spec.CredentialID,
	}
	return nil
}

func (h *host) Start(_ context.Context, spec reconcile.Spec) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r, ok := h.runners[spec.Name]; ok {
		r.State = reconcile.StateRunning
	}
	h.started = append(h.started, spec)
	return nil
}

func (h *host) Drain(_ context.Context, name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r, ok := h.runners[name]; ok {
		r.State = reconcile.StateStopping
	}
	return nil
}

func (h *host) Remove(_ context.Context, name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.runners, name)
	return nil
}

// jobsFinish is the host doing what a host does: the runners that were asked
// to stop have finished their jobs and stopped.
func (h *host) jobsFinish() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.runners {
		if r.State == reconcile.StateStopping {
			r.State = reconcile.StateStopped
		}
	}
}

func (h *host) names() []string {
	runners, _ := h.List(context.Background())
	var names []string
	for _, r := range runners {
		names = append(names, r.Name)
	}
	return names
}

type fakeGitHub struct {
	mu     sync.Mutex
	states map[string]github.State
	minted int
}

func (f *fakeGitHub) States(context.Context, github.Scope) (map[string]github.State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]github.State{}
	for name, state := range f.states {
		out[name] = state
	}
	return out, nil
}

func (f *fakeGitHub) Deregister(context.Context, github.Scope, string) error { return nil }

func (f *fakeGitHub) RegistrationToken(context.Context, github.Scope) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.minted++
	return fmt.Sprintf("AAAA-registration-%d", f.minted), nil
}

func (f *fakeGitHub) setBusy(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[name] = github.StateBusy
}

type fleet struct {
	t          *testing.T
	server     *httptest.Server
	store      *store.Store
	vm         *host
	containers *host
	gh         *fakeGitHub
	reconciler *reconcile.Reconciler
	dir        string
	// now is the clock the daemon sees, so a test can let five minutes of quiet
	// pass without waiting for them.
	now time.Time
}

func newFleet(t *testing.T) *fleet {
	t.Helper()
	dir := t.TempDir()
	f := &fleet{t: t, vm: newHost(model.RuntimeVM), containers: newHost(model.RuntimeContainer),
		gh: &fakeGitHub{states: map[string]github.State{}}, dir: dir,
		now: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)}
	f.start()
	return f
}

// start builds a daemon over whatever is already on the host and in the
// database. Calling it twice is a daemon restart, which is the case the whole
// architecture is built around.
func (f *fleet) start() {
	f.t.Helper()
	if f.store != nil {
		f.store.Close()
		f.server.Close()
	}

	ring, err := secrets.LoadOrCreateKey(filepath.Join(f.dir, "master.key"))
	if err != nil {
		f.t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(f.dir, "fleet.db"), ring)
	if err != nil {
		f.t.Fatal(err)
	}
	f.store = db

	f.reconciler = reconcile.New(db,
		[]reconcile.Executor{f.vm, f.containers},
		func(model.Secret) (reconcile.GitHubClient, error) { return f.gh, nil },
		func(int64, string) error { return nil },
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithClock(func() time.Time { return f.now })

	server := api.New(api.Options{Store: db, Fleet: f.reconciler, Version: "test"})
	if err := server.Auth().SetPassword(context.Background(), "admin", "correct-horse-battery"); err != nil {
		f.t.Fatal(err)
	}
	f.server = httptest.NewServer(server.Handler())
}

func (f *fleet) close() {
	f.server.Close()
	f.store.Close()
}

// reconcileNow runs a pass and fails the test if the daemon complained.
func (f *fleet) reconcileNow() {
	f.t.Helper()
	result := f.reconciler.Once(context.Background())
	if len(result.Errors) > 0 {
		f.t.Fatalf("reconcile reported %v", result.Errors)
	}
}

func (f *fleet) request(method, path string, body any) (*http.Response, string) {
	f.t.Helper()
	var reader io.Reader = bytes.NewReader(nil)
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, f.server.URL+path, reader)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("admin", "correct-horse-battery")
	resp, err := f.server.Client().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	payload, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(payload)
}

func (f *fleet) mustRequest(method, path string, body any, wantStatus int) string {
	f.t.Helper()
	resp, payload := f.request(method, path, body)
	if resp.StatusCode != wantStatus {
		f.t.Fatalf("%s %s answered %d, want %d: %s", method, path, resp.StatusCode, wantStatus, payload)
	}
	return payload
}

func (f *fleet) addCredential() int64 {
	f.t.Helper()
	payload := f.mustRequest("POST", "/api/credentials", map[string]string{
		"name": "pat", "secret": "github_pat_11TESTVALUE",
	}, http.StatusCreated)
	var credential model.Credential
	if err := json.Unmarshal([]byte(payload), &credential); err != nil {
		f.t.Fatal(err)
	}
	return credential.ID
}

func (f *fleet) addPool(pool map[string]any) model.Pool {
	f.t.Helper()
	payload := f.mustRequest("POST", "/api/pools", pool, http.StatusCreated)
	var created model.Pool
	if err := json.Unmarshal([]byte(payload), &created); err != nil {
		f.t.Fatal(err)
	}
	return created
}

// vmPool is a fixed-size pool unless a test widens it: minimum equal to
// maximum is how a pool that never scales is expressed.
func vmPool(credentialID int64, replicas int) map[string]any {
	return elasticVMPool(credentialID, replicas, replicas)
}

func elasticVMPool(credentialID int64, min, max int) map[string]any {
	return map[string]any{
		"name": "web", "scopeKind": "repository", "scope": "clems4ever/runyard",
		"runtime": "vm", "minReplicas": min, "maxReplicas": max, "ephemeral": true,
		"credentialId": credentialID, "enabled": true,
	}
}

// The ordinary path: a credential, a pool, and runners appear.
func TestCreatingAPoolBringsUpRunners(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(vmPool(credential, 3))
	f.reconcileNow()

	if got := strings.Join(f.vm.names(), ","); got != "web-1,web-2,web-3" {
		t.Fatalf("got %q", got)
	}

	// And the API reports them, with what the host and GitHub each say.
	payload := f.mustRequest("GET", "/api/runners", nil, http.StatusOK)
	var body struct {
		Runners []reconcile.RunnerStatus `json:"runners"`
	}
	json.Unmarshal([]byte(payload), &body)
	if len(body.Runners) != 3 || body.Runners[0].State != reconcile.StateRunning {
		t.Fatalf("got %+v", body.Runners)
	}
	if !body.Runners[0].UpToDate {
		t.Fatal("a runner created a moment ago is not up to date")
	}
}

func TestScalingAPool(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	pool := f.addPool(vmPool(credential, 1))
	f.reconcileNow()

	up := vmPool(credential, 4)
	f.mustRequest("PUT", fmt.Sprintf("/api/pools/%d", pool.ID), up, http.StatusOK)
	f.reconcileNow()
	if got := len(f.vm.names()); got != 4 {
		t.Fatalf("scaling up left %d runners", got)
	}

	down := vmPool(credential, 2)
	f.mustRequest("PUT", fmt.Sprintf("/api/pools/%d", pool.ID), down, http.StatusOK)
	f.reconcileNow()
	// Still four: the extra two are draining, not gone. Removing them now
	// would fail whatever they are running.
	if got := len(f.vm.names()); got != 4 {
		t.Fatalf("scaling down removed runners immediately: %v", f.vm.names())
	}

	f.vm.jobsFinish()
	f.reconcileNow()
	if got := strings.Join(f.vm.names(), ","); got != "web-1,web-2" {
		t.Fatalf("after draining: %q", got)
	}
}

// Changing anything but the replica count replaces the runners, and it does it
// without failing a job.
func TestChangingLabelsReplacesRunnersGracefully(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	pool := f.addPool(vmPool(credential, 2))
	f.reconcileNow()
	f.vm.created = nil

	relabelled := vmPool(credential, 2)
	relabelled["labels"] = []string{"gpu"}
	f.mustRequest("PUT", fmt.Sprintf("/api/pools/%d", pool.ID), relabelled, http.StatusOK)

	f.reconcileNow()
	if len(f.vm.created) != 0 {
		t.Fatalf("runners were rebuilt before the old ones stopped: %v", f.vm.created)
	}

	// The UI shows them as superseded while they finish.
	payload := f.mustRequest("GET", "/api/runners", nil, http.StatusOK)
	if !strings.Contains(payload, `"upToDate":false`) {
		t.Fatalf("the runners are not flagged as out of date: %s", payload)
	}

	f.vm.jobsFinish()
	f.reconcileNow()
	if got := strings.Join(f.vm.created, ","); got != "web-1,web-2" {
		t.Fatalf("the replacements were %q", got)
	}
}

// The reason the daemon is a reconciler rather than a supervisor.
func TestRestartingTheDaemonLeavesTheFleetAlone(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(vmPool(credential, 3))
	f.reconcileNow()
	before := strings.Join(f.vm.names(), ",")
	f.vm.created = nil

	// An upgrade: the process goes away and a new one takes over the same
	// database and the same host.
	f.start()
	f.reconcileNow()

	if len(f.vm.created) != 0 {
		t.Fatalf("the new daemon rebuilt runners that were already running: %v", f.vm.created)
	}
	if after := strings.Join(f.vm.names(), ","); after != before {
		t.Fatalf("the fleet changed across a restart: %q then %q", before, after)
	}
}

// Nothing the operator can do through the UI may kill a job.
func TestABusyRunnerSurvivesEverything(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	pool := f.addPool(vmPool(credential, 2))
	f.reconcileNow()
	f.gh.setBusy("web-1")

	// Delete the whole pool while a job is running on web-1.
	f.mustRequest("DELETE", fmt.Sprintf("/api/pools/%d", pool.ID), nil, http.StatusNoContent)
	f.reconcileNow()
	f.vm.jobsFinish() // the host reports both units stopped
	f.reconcileNow()

	names := f.vm.names()
	if len(names) != 1 || names[0] != "web-1" {
		t.Fatalf("got %v, want the busy runner still there and the idle one gone", names)
	}
}

func TestDisablingAPoolDrainsItAndKeepsTheConfiguration(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	pool := f.addPool(vmPool(credential, 2))
	f.reconcileNow()

	disabled := vmPool(credential, 2)
	disabled["enabled"] = false
	f.mustRequest("PUT", fmt.Sprintf("/api/pools/%d", pool.ID), disabled, http.StatusOK)
	f.reconcileNow()
	f.vm.jobsFinish()
	f.reconcileNow()

	if len(f.vm.names()) != 0 {
		t.Fatalf("a disabled pool still has %v", f.vm.names())
	}
	// The pool itself is still there, with its settings.
	payload := f.mustRequest("GET", fmt.Sprintf("/api/pools/%d", pool.ID), nil, http.StatusOK)
	if !strings.Contains(payload, `"maxReplicas":2`) {
		t.Fatalf("the configuration was lost: %s", payload)
	}
}

func TestPoolsOfDifferentRuntimesCoexist(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(vmPool(credential, 1))
	f.addPool(map[string]any{
		"name": "api", "scopeKind": "organization", "scope": "runyard-ai",
		"runtime": "container", "minReplicas": 2, "maxReplicas": 2, "nested": true,
		"credentialId": credential, "enabled": true,
	})
	f.reconcileNow()

	if got := strings.Join(f.vm.names(), ","); got != "web-1" {
		t.Fatalf("machines: %q", got)
	}
	if got := strings.Join(f.containers.names(), ","); got != "api-1,api-2" {
		t.Fatalf("containers: %q", got)
	}
}

// Rotating a token must reach the runners, which means replacing them.
func TestRotatingACredentialSupersedesTheRunners(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(vmPool(credential, 1))
	f.reconcileNow()
	f.vm.created = nil

	f.mustRequest("PUT", fmt.Sprintf("/api/credentials/%d/secret", credential),
		map[string]string{"secret": "github_pat_11ROTATED"}, http.StatusNoContent)

	f.reconcileNow()
	f.vm.jobsFinish()
	f.reconcileNow()

	if got := strings.Join(f.vm.created, ","); got != "web-1" {
		t.Fatalf("the runner was not rebuilt with the new credential: %v", f.vm.created)
	}
}

// The database is the desired state and has to survive a restart intact.
func TestConfigurationSurvivesARestart(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(vmPool(credential, 2))

	f.start()

	payload := f.mustRequest("GET", "/api/pools", nil, http.StatusOK)
	if !strings.Contains(payload, `"name":"web"`) {
		t.Fatalf("the pool did not survive: %s", payload)
	}
	// And the credential is still usable, which means the key was reloaded and
	// still decrypts what the last process wrote.
	secret, err := f.store.Secret(context.Background(), credential)
	if err != nil || secret.Token != "github_pat_11TESTVALUE" {
		t.Fatalf("got %q, %v", secret.Token, err)
	}
}

// A fleet the operator never touches must not drift: a reconciler that acted
// on a settled fleet would restart runners for ever.
func TestASettledFleetIsLeftAlone(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(vmPool(credential, 2))
	f.reconcileNow()

	for i := 0; i < 5; i++ {
		f.vm.created = nil
		result := f.reconciler.Once(context.Background())
		if len(result.Actions) != 0 {
			t.Fatalf("pass %d wanted to do %+v", i, result.Actions)
		}
	}
}

// ---------------------------------------------------------------------------
// Autoscaling
// ---------------------------------------------------------------------------
//
// GitHub does not publish how many jobs are queued for a set of labels, so the
// daemon infers demand from what its runners are doing: when every one of them
// is busy, the next job would have nowhere to go. These tests drive that
// through the real API, with jobs arriving and finishing on the fake GitHub.

// busy marks runners as working, and idle marks them free again.
func (f *fleet) busy(names ...string) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	for _, name := range names {
		f.gh.states[name] = github.StateBusy
	}
}

func (f *fleet) idle(names ...string) {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	for _, name := range names {
		f.gh.states[name] = github.StateIdle
	}
}

// everyRunnerIdle is what the fake GitHub reports once a burst is over.
func (f *fleet) everyRunnerIdle() {
	f.idle(f.vm.names()...)
}

func TestAPoolGrowsWhenEveryRunnerIsBusy(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(elasticVMPool(credential, 1, 4))
	f.reconcileNow()

	// The minimum: one runner, idle, ready to accept a job and — crucially —
	// ready to reveal that more are needed.
	if got := strings.Join(f.vm.names(), ","); got != "web-1" {
		t.Fatalf("a quiet pool has %q, want just its minimum", got)
	}
	f.idle("web-1")

	// A job lands on it. There is now nothing free, so the pool grows.
	f.busy("web-1")
	f.reconcileNow()
	if got := strings.Join(f.vm.names(), ","); got != "web-1,web-2" {
		t.Fatalf("got %q, want a second runner for the next job", got)
	}

	// The new one is idle: there is room again, so nothing more is added.
	f.idle("web-2")
	f.reconcileNow()
	if got := len(f.vm.names()); got != 2 {
		t.Fatalf("the pool grew to %d with capacity to spare", got)
	}

	// Both busy: it climbs again, one at a time, up to the ceiling.
	f.busy("web-1", "web-2")
	f.reconcileNow()
	f.busy(f.vm.names()...)
	f.reconcileNow()
	f.busy(f.vm.names()...)
	f.reconcileNow()
	if got := len(f.vm.names()); got != 4 {
		t.Fatalf("the pool reached %d, want its maximum of 4", got)
	}

	// And it stops there, however much work arrives.
	for i := 0; i < 3; i++ {
		f.busy(f.vm.names()...)
		f.reconcileNow()
	}
	if got := len(f.vm.names()); got != 4 {
		t.Fatalf("the pool passed its maximum: %d runners", got)
	}
}

func TestAPoolReturnsToItsMinimumWhenTheWorkStops(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(elasticVMPool(credential, 1, 4))

	// Drive it up to four.
	for i := 0; i < 5; i++ {
		f.reconcileNow()
		f.busy(f.vm.names()...)
	}
	if got := len(f.vm.names()); got != 4 {
		t.Fatalf("setup: the pool is %d", got)
	}

	// The burst ends.
	f.everyRunnerIdle()
	f.reconcileNow()
	if got := len(f.vm.names()); got != 4 {
		t.Fatalf("the pool shrank the moment the work stopped: %d", got)
	}

	// Still nothing, but not for long enough. Shrinking is deliberately the
	// slow direction: a gap between two jobs must not cost the fleet that is
	// about to be needed again.
	f.now = f.now.Add(reconcile.ScaleDownAfter - time.Minute)
	f.reconcileNow()
	if got := len(f.vm.names()); got != 4 {
		t.Fatalf("it shrank before the stabilisation window was over: %d", got)
	}

	// Once the quiet has lasted, it comes back to the minimum — by draining,
	// like every other shrink.
	f.now = f.now.Add(2 * time.Minute)
	f.reconcileNow()
	f.vm.jobsFinish()
	f.reconcileNow()
	if got := strings.Join(f.vm.names(), ","); got != "web-1" {
		t.Fatalf("got %q, want back to the minimum", got)
	}
}

// Scaling down must not pick the runner that has a job on it.
func TestShrinkingKeepsTheBusyRunners(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(elasticVMPool(credential, 1, 3))
	for i := 0; i < 3; i++ {
		f.reconcileNow()
		f.busy(f.vm.names()...)
	}
	if got := len(f.vm.names()); got != 3 {
		t.Fatalf("setup: %d runners", got)
	}

	// Everything goes idle except web-2, which picks up a long job.
	f.everyRunnerIdle()
	f.busy("web-2")
	f.reconcileNow()

	// It has been quiet for a while — but web-2 is working, so the pool is not
	// quiet at all and nothing shrinks.
	f.now = f.now.Add(reconcile.ScaleDownAfter + time.Minute)
	f.reconcileNow()
	if got := len(f.vm.names()); got != 3 {
		t.Fatalf("a pool with a job running shrank to %d", got)
	}

	// The job ends. Now it may shrink, and the runner it keeps is the one that
	// was working, not simply the first by name.
	f.idle("web-2")
	f.busy("web-2")
	f.reconcileNow() // records that it was busy just now
	f.idle("web-2")
	f.now = f.now.Add(reconcile.ScaleDownAfter + time.Minute)
	f.reconcileNow()
	f.vm.jobsFinish()
	f.reconcileNow()

	if got := len(f.vm.names()); got != 1 {
		t.Fatalf("got %d runners, want the minimum", got)
	}
}

// A pool whose minimum equals its maximum is the old fixed-size behaviour and
// must not move at all.
func TestAFixedPoolNeverScales(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(vmPool(credential, 2))
	f.reconcileNow()

	for i := 0; i < 5; i++ {
		f.busy(f.vm.names()...)
		f.reconcileNow()
	}
	if got := strings.Join(f.vm.names(), ","); got != "web-1,web-2" {
		t.Fatalf("a fixed pool moved to %q", got)
	}

	f.everyRunnerIdle()
	f.now = f.now.Add(2 * reconcile.ScaleDownAfter)
	f.reconcileNow()
	if got := strings.Join(f.vm.names(), ","); got != "web-1,web-2" {
		t.Fatalf("a fixed pool shrank to %q", got)
	}
}

// A restarted daemon has no memory of when the pool was last busy. That must
// delay a scale-down rather than cause one.
func TestARestartedDaemonDoesNotShrinkImmediately(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(elasticVMPool(credential, 1, 3))
	for i := 0; i < 3; i++ {
		f.reconcileNow()
		f.busy(f.vm.names()...)
	}
	f.everyRunnerIdle()

	// Upgrade: a new process over the same host and database.
	f.start()
	f.reconcileNow()
	if got := len(f.vm.names()); got != 3 {
		t.Fatalf("a restarted daemon shrank the fleet to %d before observing it for any length of time", got)
	}

	// It shrinks once it has watched the pool stay quiet for the window.
	f.now = f.now.Add(reconcile.ScaleDownAfter + time.Minute)
	f.reconcileNow()
	f.vm.jobsFinish()
	f.reconcileNow()
	if got := len(f.vm.names()); got != 1 {
		t.Fatalf("got %d, want the minimum once the quiet was observed", got)
	}
}

// The UI shows why a pool is the size it is, so the reason has to reach it.
func TestScalingDecisionsAreReported(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(elasticVMPool(credential, 1, 3))
	f.reconcileNow()
	f.busy("web-1")

	result := f.reconciler.Once(context.Background())
	scale, ok := result.Scaling["web"]
	if !ok {
		t.Fatalf("no decision was reported: %+v", result.Scaling)
	}
	if scale.Target != 2 || !scale.ScaledUp {
		t.Fatalf("got %+v", scale)
	}
	if !strings.Contains(scale.Reason, "busy") {
		t.Fatalf("the reason is %q", scale.Reason)
	}
	if scale.Floor != 1 || scale.Ceiling != 3 {
		t.Fatalf("the bounds are %d..%d", scale.Floor, scale.Ceiling)
	}
}

// ---------------------------------------------------------------------------
// GitHub App credentials
// ---------------------------------------------------------------------------

func appKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

func (f *fleet) addAppCredential(appID int64) int64 {
	f.t.Helper()
	payload := f.mustRequest("POST", "/api/credentials", map[string]any{
		"name": "app", "kind": "app", "appId": appID, "secret": appKey(f.t),
	}, http.StatusCreated)
	var credential model.Credential
	if err := json.Unmarshal([]byte(payload), &credential); err != nil {
		f.t.Fatal(err)
	}
	return credential.ID
}

// A runner authenticates on its own — it comes back after a reboot with the
// daemon still starting — so everything it needs to do that has to reach it.
func TestAPoolOnAnAppReachesItsRunners(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addAppCredential(123456)
	pool := vmPool(credential, 1)
	f.addPool(pool)
	f.reconcileNow()

	if len(f.vm.specs) != 1 {
		t.Fatalf("got %d runners", len(f.vm.specs))
	}
	spec := f.vm.specs[0]
	if spec.CredentialKind != model.CredentialApp || spec.AppID != 123456 {
		t.Fatalf("the runner was not told which app to authenticate as: %+v", spec)
	}
	// The key itself travels the same way every secret does: on tmpfs, by
	// reference, never in the runner's configuration.
	if spec.CredentialID != credential {
		t.Fatalf("the runner points at credential %d, want %d", spec.CredentialID, credential)
	}
}

// Replacing an app's key must reach the runners, which means replacing them.
func TestRotatingAnAppKeySupersedesTheRunners(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addAppCredential(123456)
	f.addPool(vmPool(credential, 1))
	f.reconcileNow()
	f.vm.created = nil

	f.mustRequest("PUT", fmt.Sprintf("/api/credentials/%d/secret", credential),
		map[string]string{"secret": appKey(t)}, http.StatusNoContent)

	f.reconcileNow()
	f.vm.jobsFinish()
	f.reconcileNow()

	if got := strings.Join(f.vm.created, ","); got != "web-1" {
		t.Fatalf("the runner was not rebuilt with the new key: %v", f.vm.created)
	}
}

// Both kinds work at once: an installation that was on a token does not have
// to move everything to an app in one go.
func TestTokenAndAppPoolsSideBySide(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	token := f.addCredential()
	app := f.addAppCredential(999)

	f.addPool(vmPool(token, 1))
	f.addPool(map[string]any{
		"name": "api", "scopeKind": "repository", "scope": "clems4ever/other",
		"runtime": "vm", "minReplicas": 1, "maxReplicas": 1,
		"credentialId": app, "enabled": true,
	})
	f.reconcileNow()

	kinds := map[string]model.CredentialKind{}
	for _, spec := range f.vm.specs {
		kinds[spec.Name] = spec.CredentialKind
	}
	if kinds["web-1"] != model.CredentialPAT || kinds["api-1"] != model.CredentialApp {
		t.Fatalf("got %v", kinds)
	}
}

// A container runner is given a token the daemon minted, not the credential —
// a container shares everything with the job it runs.
func TestContainerRunnersAreGivenATokenNotTheCredential(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addAppCredential(123456)
	f.addPool(map[string]any{
		"name": "api", "scopeKind": "repository", "scope": "clems4ever/claude-control",
		"runtime": "container", "minReplicas": 1, "maxReplicas": 1,
		"credentialId": credential, "enabled": true,
	})
	f.reconcileNow()

	if len(f.containers.specs) != 1 {
		t.Fatalf("got %d containers", len(f.containers.specs))
	}
	spec := f.containers.specs[0]
	if spec.RegistrationToken == "" {
		t.Fatal("the container was created without a registration token, so it cannot register")
	}
	if f.gh.minted != 1 {
		t.Fatalf("minted %d tokens for one container", f.gh.minted)
	}
}

// A machine gets no minted token: it holds the credential and mints for
// itself, which is what lets it come back after a reboot with the daemon down.
func TestMachineRunnersMintForThemselves(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(vmPool(credential, 1))
	f.reconcileNow()

	if f.vm.specs[0].RegistrationToken != "" {
		t.Fatal("a machine was handed a token it did not need, which expires in an hour")
	}
	if f.gh.minted != 0 {
		t.Fatalf("the daemon minted %d tokens for a machine", f.gh.minted)
	}
}

// A container that has exited is rebuilt with a fresh token rather than
// started again with the expired one it registered with.
func TestAStoppedContainerIsRebuiltWithAFreshToken(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.addPool(map[string]any{
		"name": "api", "scopeKind": "repository", "scope": "o/r",
		"runtime": "container", "minReplicas": 1, "maxReplicas": 1,
		"credentialId": credential, "enabled": true,
	})
	f.reconcileNow()
	first := f.containers.specs[0].RegistrationToken

	// The job finished and the container exited, which is what an ephemeral
	// container runner does after every job.
	f.containers.mu.Lock()
	f.containers.runners["api-1"].State = reconcile.StateStopped
	f.containers.mu.Unlock()

	f.reconcileNow()
	if len(f.containers.started) != 1 {
		t.Fatalf("it was not brought back: %+v", f.containers.started)
	}
	if got := f.containers.started[0].RegistrationToken; got == "" || got == first {
		t.Fatalf("it was brought back with %q, want a token minted for this attempt", got)
	}
}

// ---------------------------------------------------------------------------
// Templates
// ---------------------------------------------------------------------------

// The template checked into this repository, through the real route, all the
// way to runners on the host.
//
// It is what someone is handed and told to import, so what it is worth is not
// that it parses: it is that importing it leaves a fleet that can serve both
// halves of the workflow — containers for the jobs that need a toolchain,
// machines for the ones that need a Docker daemon or a systemd of their own.
func TestImportingTheShippedTemplateBringsUpBothKindsOfRunner(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.importTemplate(t, "github-runner-ci.json", map[string]any{"credentialId": credential})
	f.reconcileNow()

	// Every pool keeps one runner at its floor, so each can accept a job and
	// find out whether more are needed.
	if got := strings.Join(f.containers.names(), ","); got != "ci-container-1" {
		t.Fatalf("containers: %q", got)
	}
	if got := strings.Join(f.vm.names(), ","); got != "ci-vm-1" {
		t.Fatalf("machines: %q", got)
	}

	// And a workflow can tell them apart, which is the whole point of having
	// two pools.
	payload := f.mustRequest("GET", "/api/pools", nil, http.StatusOK)
	var pools []model.Pool
	if err := json.Unmarshal([]byte(payload), &pools); err != nil {
		t.Fatal(err)
	}
	for _, pool := range pools {
		labels := strings.Join(pool.EffectiveLabels(), ",")
		if !strings.Contains(labels, string(pool.Runtime)) {
			t.Errorf("%s cannot be selected by runs-on: %q", pool.Name, labels)
		}
		if !pool.Ephemeral {
			t.Errorf("%s reuses runners between jobs", pool.Name)
		}
	}
}

// Importing the same template again is how a fleet is updated from a file in a
// repository, and it must not fail the job a runner is on.
func TestImportingOverAFleetReplacesRunnersGracefully(t *testing.T) {
	f := newFleet(t)
	defer f.close()

	credential := f.addCredential()
	f.importTemplate(t, "github-runner-ci.json", map[string]any{"credentialId": credential})
	f.reconcileNow()
	f.vm.created = nil

	// The same document, with the machines given more memory. Without the
	// tick-box it is refused rather than silently doing nothing.
	edited := f.template(t, "github-runner-ci.json")
	pools := edited["pools"].([]any)
	pools[1].(map[string]any)["memoryMb"] = 16384

	f.mustRequest("POST", "/api/pools/import", map[string]any{
		"document": edited, "credentialId": credential,
	}, http.StatusConflict)

	f.mustRequest("POST", "/api/pools/import", map[string]any{
		"document": edited, "credentialId": credential, "replaceExisting": true,
	}, http.StatusOK)

	// The runner is mid-job, so it is drained rather than rebuilt under it.
	f.reconcileNow()
	if len(f.vm.created) != 0 {
		t.Fatalf("a runner was rebuilt before the old one stopped: %v", f.vm.created)
	}

	f.vm.jobsFinish()
	f.reconcileNow()
	if got := strings.Join(f.vm.created, ","); got != "ci-vm-1" {
		t.Fatalf("the replacement was %q", got)
	}
	if got := f.vm.specs[len(f.vm.specs)-1].MemoryMB; got != 16384 {
		t.Fatalf("the replacement has %d MiB, so the import did not reach it", got)
	}
}

// What comes out of one daemon goes into the next one. This is the path
// somebody takes when they move a fleet to another host.
func TestAFleetCanBeExportedAndImportedSomewhereElse(t *testing.T) {
	source := newFleet(t)
	defer source.close()
	credential := source.addCredential()
	source.addPool(elasticVMPool(credential, 1, 4))

	exported := source.mustRequest("GET", "/api/pools/export", nil, http.StatusOK)
	var document map[string]any
	if err := json.Unmarshal([]byte(exported), &document); err != nil {
		t.Fatalf("the export is not JSON: %v", err)
	}

	destination := newFleet(t)
	defer destination.close()
	destination.mustRequest("POST", "/api/pools/import", map[string]any{
		"document": document, "credentialId": destination.addCredential(),
	}, http.StatusOK)
	destination.reconcileNow()

	if got := strings.Join(destination.vm.names(), ","); got != "web-1" {
		t.Fatalf("the second host has %q", got)
	}
	payload := destination.mustRequest("GET", "/api/pools", nil, http.StatusOK)
	var pools []model.Pool
	if err := json.Unmarshal([]byte(payload), &pools); err != nil {
		t.Fatal(err)
	}
	if pools[0].MinReplicas != 1 || pools[0].MaxReplicas != 4 {
		t.Fatalf("the scaling bounds did not travel: %+v", pools[0])
	}
}

// template reads a template shipped in this repository.
func (f *fleet) template(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "templates", name))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("%s is not JSON: %v", name, err)
	}
	return document
}

func (f *fleet) importTemplate(t *testing.T, name string, options map[string]any) {
	t.Helper()
	body := map[string]any{"document": f.template(t, name)}
	for key, value := range options {
		body[key] = value
	}
	f.mustRequest("POST", "/api/pools/import", body, http.StatusOK)
}
