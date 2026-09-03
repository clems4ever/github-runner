package systemd

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/paths"
	"github.com/clems4ever/github-runner/internal/reconcile"
)

// fakeCommander stands in for systemctl, which no CI runner has in a state
// these tests could use.
type fakeCommander struct {
	calls  []string
	output map[string]string
	err    error
}

func (f *fakeCommander) Run(ctx context.Context, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, call)
	if f.err != nil {
		return "", f.err
	}
	for prefix, out := range f.output {
		if strings.HasPrefix(call, prefix) {
			return out, nil
		}
	}
	return "", nil
}

func (f *fakeCommander) called(substr string) bool {
	for _, call := range f.calls {
		if strings.Contains(call, substr) {
			return true
		}
	}
	return false
}

func newExecutor(t *testing.T) (*Executor, *fakeCommander, paths.Layout) {
	t.Helper()
	layout := paths.Under(t.TempDir())
	if err := layout.EnsureDirs(paths.CurrentOwner()); err != nil {
		t.Fatal(err)
	}
	cmd := &fakeCommander{output: map[string]string{}}
	e := New(layout, "/usr/local/bin/runner-fleet", "runner-fleet",
		WithCommander(cmd), WithUnitPath(layout.Etc+"/gh-runner@.service"))
	return e, cmd, layout
}

// newExecutorWithCgroupRoot is newExecutor with the kernel's memory accounting
// pointed at a directory the test owns, so that what Usage reads is what the
// test wrote and not whatever the machine running it has mounted.
func newExecutorWithCgroupRoot(t *testing.T) (*Executor, *fakeCommander, string) {
	t.Helper()
	layout := paths.Under(t.TempDir())
	if err := layout.EnsureDirs(paths.CurrentOwner()); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cmd := &fakeCommander{output: map[string]string{}}
	e := New(layout, "/usr/local/bin/runner-fleet", "runner-fleet",
		WithCommander(cmd), WithUnitPath(layout.Etc+"/gh-runner@.service"),
		WithCgroupRoot(root))
	return e, cmd, root
}

// writeCgroupStats lays out one unit's memory.stat under a cgroup root.
func writeCgroupStats(t *testing.T, root, controlGroup, body string) {
	t.Helper()
	dir := filepath.Join(root, controlGroup)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.stat"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newExecutorAt is newExecutor with a clock that does not move, for the tests
// that ask how long something has been happening.
func newExecutorAt(t *testing.T, now time.Time) (*Executor, *fakeCommander, paths.Layout) {
	t.Helper()
	layout := paths.Under(t.TempDir())
	if err := layout.EnsureDirs(paths.CurrentOwner()); err != nil {
		t.Fatal(err)
	}
	cmd := &fakeCommander{output: map[string]string{}}
	e := New(layout, "/usr/local/bin/runner-fleet", "runner-fleet",
		WithCommander(cmd), WithUnitPath(layout.Etc+"/gh-runner@.service"),
		WithClock(func() time.Time { return now }))
	return e, cmd, layout
}

func testSpec(name string) reconcile.Spec {
	return reconcile.Spec{
		Name: name, Pool: "web", PoolID: 1, Generation: "abc123def456",
		Runtime: model.RuntimeVM, URL: "https://github.com/o/r",
		ScopeKind: model.ScopeRepository, Scope: "o/r",
		Labels: []string{"vm", "nestedvirt"}, Ephemeral: true, Nested: true,
		CPUs: 4, MemoryMB: 8192, DiskGB: 60, Image: "default", CredentialID: 7,
	}
}

func TestCreateWritesTheConfigurationThenStarts(t *testing.T) {
	e, cmd, layout := newExecutor(t)

	if err := e.Create(context.Background(), testSpec("web-1")); err != nil {
		t.Fatal(err)
	}

	env, err := os.ReadFile(layout.RunnerEnv("web-1"))
	if err != nil {
		t.Fatalf("no configuration was written: %v", err)
	}
	for _, want := range []string{
		"FLEET_RUNNER=web-1",
		"FLEET_POOL=web",
		"FLEET_GENERATION=abc123def456",
		"FLEET_URL=https://github.com/o/r",
		"FLEET_LABELS=vm,nestedvirt",
		"FLEET_EPHEMERAL=true",
		"FLEET_NESTED=true",
		"FLEET_CPUS=4",
		"FLEET_MEMORY_MB=8192",
		"FLEET_DISK_GB=60",
	} {
		if !strings.Contains(string(env), want) {
			t.Errorf("the configuration is missing %q:\n%s", want, env)
		}
	}

	// Enabled as well as started, or the fleet would not come back after a
	// reboot until the daemon happened to notice.
	if !cmd.called("systemctl enable --now gh-runner@web-1.service") {
		t.Fatalf("calls were %v", cmd.calls)
	}
}

// The token must never be written to a disk. The environment file points at a
// file on tmpfs instead, which the daemon rewrites.
func TestTheEnvironmentFileHoldsNoSecret(t *testing.T) {
	_, _, layout := newExecutor(t)
	env := RenderEnv(testSpec("web-1"), layout)

	if strings.Contains(env, "TOKEN=") && !strings.Contains(env, "CREDENTIAL_FILE") {
		t.Fatal("a token was written into the unit's environment")
	}
	if !strings.Contains(env, "FLEET_CREDENTIAL_FILE="+layout.Credential(7)) {
		t.Fatalf("the credential is not referenced by path:\n%s", env)
	}
	if !strings.Contains(layout.Credential(7), layout.Run) {
		t.Fatalf("the credential path is not under the runtime directory: %s", layout.Credential(7))
	}
}

func TestTheEnvironmentFileIsOwnerOnly(t *testing.T) {
	e, _, layout := newExecutor(t)
	if err := e.Create(context.Background(), testSpec("web-1")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(layout.RunnerEnv("web-1"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("the configuration is %#o, want 0600", mode)
	}
}

func TestDrainDoesNotBlock(t *testing.T) {
	e, cmd, _ := newExecutor(t)
	if err := e.Drain(context.Background(), "web-1"); err != nil {
		t.Fatal(err)
	}
	// Without --no-block this call waits for the job in flight — up to an hour
	// — and stalls every other pool behind it.
	if !cmd.called("systemctl stop --no-block gh-runner@web-1.service") {
		t.Fatalf("calls were %v", cmd.calls)
	}
}

func TestRemoveTakesTheConfigurationWithIt(t *testing.T) {
	e, cmd, layout := newExecutor(t)
	ctx := context.Background()
	if err := e.Create(ctx, testSpec("web-1")); err != nil {
		t.Fatal(err)
	}

	if err := e.Remove(ctx, "web-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(layout.RunnerEnv("web-1")); !os.IsNotExist(err) {
		t.Fatal("the configuration outlived the runner, so it would be listed for ever")
	}
	if !cmd.called("systemctl disable gh-runner@web-1.service") {
		t.Fatalf("calls were %v", cmd.calls)
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	e, _, _ := newExecutor(t)
	if err := e.Remove(context.Background(), "never-existed"); err != nil {
		t.Fatalf("removing something that is not there should be quiet: %v", err)
	}
}

func TestStartClearsAFailedUnitFirst(t *testing.T) {
	e, cmd, _ := newExecutor(t)
	if err := e.Start(context.Background(), testSpec("web-1")); err != nil {
		t.Fatal(err)
	}
	// A failed unit refuses to start again with "start request repeated too
	// quickly" until its counter is cleared.
	if !cmd.called("reset-failed") || !cmd.called("systemctl start gh-runner@web-1.service") {
		t.Fatalf("calls were %v", cmd.calls)
	}
}

func TestListReadsTheFleetBackFromDisk(t *testing.T) {
	e, cmd, _ := newExecutor(t)
	ctx := context.Background()

	for _, name := range []string{"web-1", "web-2", "web-3"} {
		if err := e.Create(ctx, testSpec(name)); err != nil {
			t.Fatal(err)
		}
	}

	cmd.output["systemctl show"] = strings.Join([]string{
		"Id=gh-runner@web-1.service", "ActiveState=active", "",
		"Id=gh-runner@web-2.service", "ActiveState=deactivating", "",
		"Id=gh-runner@web-3.service", "ActiveState=failed", "",
	}, "\n")

	runners, err := e.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) != 3 {
		t.Fatalf("got %d runners", len(runners))
	}

	want := map[string]reconcile.RunnerState{
		"web-1": reconcile.StateRunning,
		// Deactivating is a runner finishing its job. Calling it stopped would
		// let the reconciler delete the machine out from under it.
		"web-2": reconcile.StateStopping,
		"web-3": reconcile.StateStopped,
	}
	for _, r := range runners {
		if r.State != want[r.Name] {
			t.Errorf("%s is %q, want %q", r.Name, r.State, want[r.Name])
		}
		if r.Pool != "web" || r.Generation != "abc123def456" {
			t.Errorf("%s came back as pool %q generation %q", r.Name, r.Pool, r.Generation)
		}
		if r.Runtime != model.RuntimeVM {
			t.Errorf("%s has runtime %q", r.Name, r.Runtime)
		}
	}
}

// This is what a restarted daemon does: it has no memory, and has to find the
// fleet by reading the host.
func TestListIsHowAFreshDaemonFindsTheFleet(t *testing.T) {
	e, cmd, layout := newExecutor(t)
	ctx := context.Background()
	if err := e.Create(ctx, testSpec("web-1")); err != nil {
		t.Fatal(err)
	}

	fresh := New(layout, "/usr/local/bin/runner-fleet", "runner-fleet",
		WithCommander(cmd), WithUnitPath(layout.Etc+"/gh-runner@.service"))
	cmd.output["systemctl show"] = "Id=gh-runner@web-1.service\nActiveState=active\n"

	runners, err := fresh.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) != 1 || runners[0].Generation != "abc123def456" {
		t.Fatalf("a new daemon did not rediscover the runner: %+v", runners)
	}
}

func TestListOnAnEmptyHost(t *testing.T) {
	e, _, _ := newExecutor(t)
	runners, err := e.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) != 0 {
		t.Fatalf("got %+v", runners)
	}
}

func TestEnsureUnit(t *testing.T) {
	e, cmd, layout := newExecutor(t)
	ctx := context.Background()

	if err := e.EnsureUnit(ctx); err != nil {
		t.Fatal(err)
	}
	unit, err := os.ReadFile(layout.Etc + "/gh-runner@.service")
	if err != nil {
		t.Fatal(err)
	}
	text := string(unit)

	for _, want := range []string{
		"ExecStart=/usr/local/bin/runner-fleet agent --name %i",
		"EnvironmentFile=" + layout.RunnersDir() + "/%i.env",
		"User=runner-fleet",
		"KillMode=mixed",
		// Stopping waits for the job in flight. This is what makes a
		// scale-down or a reconfiguration safe.
		"TimeoutStopSec=3660",
		"Restart=always",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the unit is missing %q", want)
		}
	}

	// The runners must not be tied to the daemon's lifetime in any way, or
	// upgrading the daemon would take the fleet down with it.
	for _, forbidden := range []string{"PartOf=", "BindsTo=", "Requires=runner-fleetd", "After=runner-fleetd"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("the unit contains %q, which would couple the runners to the daemon", forbidden)
		}
	}

	if !cmd.called("systemctl daemon-reload") {
		t.Error("systemd was not told to re-read the unit")
	}

	// Writing it again when nothing changed must not reload: an upgraded
	// daemon rewrites this on every start.
	cmd.calls = nil
	if err := e.EnsureUnit(ctx); err != nil {
		t.Fatal(err)
	}
	if len(cmd.calls) != 0 {
		t.Errorf("an unchanged unit was reloaded anyway: %v", cmd.calls)
	}
}

func TestRuntime(t *testing.T) {
	e, _, _ := newExecutor(t)
	if e.Runtime() != model.RuntimeVM {
		t.Fatalf("got %q", e.Runtime())
	}
}

func TestMapActiveState(t *testing.T) {
	for active, want := range map[string]reconcile.RunnerState{
		"active":       reconcile.StateRunning,
		"activating":   reconcile.StateRunning,
		"reloading":    reconcile.StateRunning,
		"deactivating": reconcile.StateStopping,
		"inactive":     reconcile.StateStopped,
		"failed":       reconcile.StateStopped,
		"":             reconcile.StateStopped,
	} {
		if got := mapActiveState(active, ""); got != want {
			t.Errorf("%q maps to %q, want %q", active, got, want)
		}
	}
}

// The state that lied. A unit that crashes on startup and is waiting out its
// RestartSec is "activating", and reading only that made a runner which had
// never once registered report as running — to the dashboard and to the
// reconciler, which then had no reason to touch it.
func TestAUnitWaitingToBeRetriedIsNotRunning(t *testing.T) {
	if got := mapActiveState("activating", "auto-restart"); got != reconcile.StateStopped {
		t.Fatalf("a crash-looping runner reports as %q", got)
	}
	// A unit on its way up for the first time still is.
	if got := mapActiveState("activating", "start"); got != reconcile.StateRunning {
		t.Fatalf("a starting runner reports as %q", got)
	}
}

// What a person is told, and what they can do about it.
func TestTroubleSaysHowBadItIsAndWhereToLook(t *testing.T) {
	if got := (unit{result: "success"}).trouble("web-1"); got != "" {
		t.Errorf("a healthy runner reports trouble: %q", got)
	}
	if got := (unit{}).trouble("web-1"); got != "" {
		t.Errorf("a unit systemd said nothing about reports trouble: %q", got)
	}

	once := unit{result: "exit-code", restarts: 1}.trouble("web-1")
	if !strings.Contains(once, "exit-code") || !strings.Contains(once, "journalctl -u gh-runner@web-1.service") {
		t.Errorf("got %q", once)
	}

	// The count is what separates a runner coming back from one that has never
	// worked.
	many := unit{result: "exit-code", restarts: 9}.trouble("web-1")
	if !strings.Contains(many, "9 times") {
		t.Errorf("got %q", many)
	}
}

// The whole path: what systemctl prints, through to what the fleet says about
// the runner. This is the shape the daemon actually saw while every machine
// runner on the host was failing to read its credential.
func TestACrashLoopingRunnerIsReportedAsStoppedAndTroubled(t *testing.T) {
	e, cmd, layout := newExecutor(t)
	if err := os.WriteFile(layout.RunnerEnv("web-1"), []byte(RenderEnv(testSpec("web-1"), layout)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd.output["systemctl show"] = strings.Join([]string{
		"Id=gh-runner@web-1.service",
		"ActiveState=activating",
		"SubState=auto-restart",
		"Result=exit-code",
		"NRestarts=9",
		"",
	}, "\n")

	runners, err := e.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) != 1 {
		t.Fatalf("got %d runners", len(runners))
	}
	if runners[0].State != reconcile.StateStopped {
		t.Errorf("state is %q, and this runner has never started", runners[0].State)
	}
	if !strings.Contains(runners[0].Trouble, "9 times") {
		t.Errorf("the fleet has nothing to say about it: %q", runners[0].Trouble)
	}
}

// A GitHub App's agent signs its own assertion and buys an installation token,
// so the unit has to carry which app the key belongs to. The key itself stays
// where every other secret does: on tmpfs, referenced by path.
func TestTheEnvironmentFileCarriesTheAppDetails(t *testing.T) {
	_, _, layout := newExecutor(t)

	spec := testSpec("web-1")
	spec.CredentialKind = model.CredentialApp
	spec.AppID = 123456
	spec.InstallationID = 42

	env := RenderEnv(spec, layout)
	for _, want := range []string{
		"FLEET_CREDENTIAL_KIND=app",
		"FLEET_APP_ID=123456",
		"FLEET_INSTALLATION_ID=42",
		"FLEET_CREDENTIAL_FILE=" + layout.Credential(7),
	} {
		if !strings.Contains(env, want) {
			t.Errorf("the configuration is missing %q:\n%s", want, env)
		}
	}
	if strings.Contains(env, "PRIVATE KEY") {
		t.Fatal("the app's key was written into the unit's environment")
	}
}

// The layer is the last link in the chain: the daemon decides a repository may
// have one and builds it, and this is the only thing that tells the machine to
// boot it. Without it the runner comes up on the pool's own image and the job
// fails on a tool the repository was told it had.
func TestTheEnvironmentFileCarriesTheLayer(t *testing.T) {
	_, _, layout := newExecutor(t)

	spec := testSpec("web-1")
	spec.Layer = "runner-noble-web-aaaaaaaaaaaa.qcow2"

	env := RenderEnv(spec, layout)
	if !strings.Contains(env, "FLEET_LAYER=runner-noble-web-aaaaaaaaaaaa.qcow2") {
		t.Fatalf("the layer never reached the machine:\n%s", env)
	}
}

// An empty layer is a pool that has none, and writing it would name a file the
// agent then refused to find — so a pool with layers switched off would never
// start a machine at all.
func TestAPoolWithNoLayerSaysNothingAboutOne(t *testing.T) {
	_, _, layout := newExecutor(t)

	if env := RenderEnv(testSpec("web-1"), layout); strings.Contains(env, "FLEET_LAYER") {
		t.Fatalf("wrote a layer for a pool that has none:\n%s", env)
	}
}

// An installation of zero means "work it out", and writing it would tell the
// agent to use installation zero.
func TestAnUnknownInstallationIsLeftOut(t *testing.T) {
	_, _, layout := newExecutor(t)
	spec := testSpec("web-1")
	spec.CredentialKind = model.CredentialApp
	spec.AppID = 123456

	env := RenderEnv(spec, layout)
	if strings.Contains(env, "FLEET_INSTALLATION_ID") {
		t.Fatalf("an unknown installation was written anyway:\n%s", env)
	}
}

// A machine that missed the power button looks exactly like one finishing a
// long job: the unit says "deactivating" and nothing says for how long. This
// one had been stopping for forty-eight minutes, holding four cpus, with the
// fleet showing STOPPING and no clock on it.
func TestALongDrainSaysHowLongItHasBeen(t *testing.T) {
	now := time.Date(2026, 8, 22, 16, 58, 0, 0, time.UTC)
	e, cmd, layout := newExecutorAt(t, now)
	if err := os.WriteFile(layout.RunnerEnv("web-1"), []byte(RenderEnv(testSpec("web-1"), layout)), 0o600); err != nil {
		t.Fatal(err)
	}

	// It left active at 16:10, which is where this began.
	leftActive := now.Add(-48 * time.Minute).Unix()
	cmd.output["systemctl show"] = strings.Join([]string{
		"Id=gh-runner@web-1.service",
		"ActiveState=deactivating",
		"SubState=stop-sigterm",
		"Result=success",
		"NRestarts=0",
		fmt.Sprintf("ActiveExitTimestamp=@%d", leftActive),
		"",
	}, "\n")

	runners, err := e.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Still stopping, not failed: a drain is not a failure, and the reconciler
	// must not treat it as a runner it can delete.
	if runners[0].State != reconcile.StateStopping {
		t.Fatalf("state is %q", runners[0].State)
	}
	if !strings.Contains(runners[0].Trouble, "48m") {
		t.Errorf("the fleet does not say how long it has been draining: %q", runners[0].Trouble)
	}
	if !strings.Contains(runners[0].Trouble, "journalctl -u gh-runner@web-1.service") {
		t.Errorf("no way to look into it: %q", runners[0].Trouble)
	}
}

// A drain of a few minutes is a job finishing, and says nothing.
func TestAnOrdinaryDrainIsQuiet(t *testing.T) {
	now := time.Date(2026, 8, 22, 16, 58, 0, 0, time.UTC)
	e, cmd, layout := newExecutorAt(t, now)
	if err := os.WriteFile(layout.RunnerEnv("web-1"), []byte(RenderEnv(testSpec("web-1"), layout)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd.output["systemctl show"] = strings.Join([]string{
		"Id=gh-runner@web-1.service",
		"ActiveState=deactivating",
		"SubState=stop-sigterm",
		"Result=success",
		"NRestarts=0",
		fmt.Sprintf("ActiveExitTimestamp=@%d", now.Add(-90*time.Second).Unix()),
		"",
	}, "\n")

	runners, err := e.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if runners[0].Trouble != "" {
		t.Fatalf("a runner finishing its job was reported as a problem: %q", runners[0].Trouble)
	}
}

// The host knows how long a runner has been up, and that is what separates a
// machine still booting from one GitHub will never hear about.
func TestListSaysHowLongARunnerHasBeenUp(t *testing.T) {
	now := time.Date(2026, 8, 22, 17, 0, 0, 0, time.UTC)
	e, cmd, layout := newExecutorAt(t, now)
	if err := os.WriteFile(layout.RunnerEnv("web-1"), []byte(RenderEnv(testSpec("web-1"), layout)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd.output["systemctl show"] = strings.Join([]string{
		"Id=gh-runner@web-1.service",
		"ActiveState=active",
		"SubState=running",
		"Result=success",
		"NRestarts=0",
		fmt.Sprintf("ActiveEnterTimestamp=@%d", now.Add(-40*time.Second).Unix()),
		"",
	}, "\n")

	runners, err := e.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if runners[0].Up != 40*time.Second {
		t.Fatalf("up for %s, want 40s", runners[0].Up)
	}
}

// An ephemeral machine is replaced after every job, so the restart delay is
// paid by every job on the host, and the start limiter has to be sized for a
// pool that is working rather than one that is broken.
func TestTheRestartPolicySuitsAFleetThatIsBusy(t *testing.T) {
	e, _, _ := newExecutor(t)
	unit := e.renderUnit()

	if !strings.Contains(unit, "RestartSec=2") {
		t.Error("the restart delay is long enough to be noticed once per job")
	}

	// Thirty starts in ten minutes: a machine that comes back in ~30s does
	// twenty and lives; one that fails at once does thirty in a couple of
	// minutes and is stopped.
	interval, burst := field(t, unit, "StartLimitIntervalSec="), field(t, unit, "StartLimitBurst=")
	if interval != "600" || burst != "30" {
		t.Fatalf("start limiter is %s starts per %ss", burst, interval)
	}

	// The healthy case has to fit inside the window with room to spare, or a
	// busy pool stops itself.
	const secondsPerJob = 30
	if starts := 600 / secondsPerJob; starts >= 30 {
		t.Fatalf("a pool doing a job every %ds does %d starts per window and would be"+
			" mistaken for a crash loop", secondsPerJob, starts)
	}
}

func field(t *testing.T, unit, key string) string {
	t.Helper()
	for _, line := range strings.Split(unit, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), key); ok {
			return value
		}
	}
	t.Fatalf("the unit has no %s", key)
	return ""
}

// Reproducing what the fleet page shows between two machines.
//
// An ephemeral runner is replaced after every job, and in the seconds between
// them the unit is not running: systemd is waiting out RestartSec, which it
// calls activating/auto-restart. The host says nothing about how long the
// runner has been up, because it is not up, so the fleet calls it unknown —
// the same word it uses for a runner that will never come back.
func TestARunnerWaitingToBeRestartedSaysItIsComingBack(t *testing.T) {
	now := time.Date(2026, 8, 22, 17, 0, 0, 0, time.UTC)
	e, cmd, layout := newExecutorAt(t, now)
	if err := os.WriteFile(layout.RunnerEnv("web-1"), []byte(RenderEnv(testSpec("web-1"), layout)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd.output["systemctl show"] = strings.Join([]string{
		"Id=gh-runner@web-1.service",
		"ActiveState=activating",
		"SubState=auto-restart",
		"Result=success",
		"NRestarts=3",
		fmt.Sprintf("ActiveExitTimestamp=@%d", now.Add(-2*time.Second).Unix()),
		"",
	}, "\n")

	runners, err := e.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Still stopped as far as the reconciler is concerned — it is not running,
	// and pretending otherwise is what hid a crash loop for a week.
	if runners[0].State != reconcile.StateStopped {
		t.Fatalf("state is %q", runners[0].State)
	}
	if !runners[0].Coming {
		t.Fatal("a runner waiting out its restart delay is not reported as coming back," +
			" so the fleet calls it unknown between every pair of jobs")
	}
}

// And the first moment of a fresh unit, before systemd has recorded when it
// became active: running, no uptime to report, and on its way up.
func TestARunnerThatHasJustBeenStartedIsComingBack(t *testing.T) {
	now := time.Date(2026, 8, 22, 17, 0, 0, 0, time.UTC)
	e, cmd, layout := newExecutorAt(t, now)
	if err := os.WriteFile(layout.RunnerEnv("web-1"), []byte(RenderEnv(testSpec("web-1"), layout)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd.output["systemctl show"] = strings.Join([]string{
		"Id=gh-runner@web-1.service",
		"ActiveState=activating",
		"SubState=start",
		"Result=success",
		"NRestarts=0",
		"ActiveEnterTimestamp=",
		"",
	}, "\n")

	runners, err := e.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !runners[0].Coming {
		t.Fatal("a unit systemd is still starting is not reported as coming back")
	}
}

// A settled runner is not coming back, or every runner would look like it was
// on its way up for ever.
func TestASettledRunnerIsNotComingBack(t *testing.T) {
	now := time.Date(2026, 8, 22, 17, 0, 0, 0, time.UTC)
	e, cmd, layout := newExecutorAt(t, now)
	if err := os.WriteFile(layout.RunnerEnv("web-1"), []byte(RenderEnv(testSpec("web-1"), layout)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd.output["systemctl show"] = strings.Join([]string{
		"Id=gh-runner@web-1.service",
		"ActiveState=active",
		"SubState=running",
		"Result=success",
		"NRestarts=0",
		fmt.Sprintf("ActiveEnterTimestamp=@%d", now.Add(-time.Hour).Unix()),
		"",
	}, "\n")

	runners, err := e.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if runners[0].Coming {
		t.Fatal("a runner that has been up for an hour is reported as coming back")
	}
}

// The numbers come from systemd rather than from QEMU: every runner is a unit,
// every unit is a cgroup, and the kernel is already accounting for it.
func TestUsageReadsWhatSystemdAlreadyAccountsFor(t *testing.T) {
	e, cmd, _ := newExecutor(t)
	if err := e.Create(context.Background(), testSpec("web-1")); err != nil {
		t.Fatal(err)
	}
	cmd.output["systemctl show --timestamp=unix --property=Id,ActiveState"] = "Id=gh-runner@web-1.service\nActiveState=active\nSubState=running\nResult=success\nNRestarts=0\n"
	cmd.output["systemctl show --timestamp=unix --property=Id,MemoryCurrent"] = "Id=gh-runner@web-1.service\nMemoryCurrent=4294967296\nCPUUsageNSec=1000000000\n"

	usage, err := e.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 {
		t.Fatalf("got %v", usage)
	}
	if usage[0].Name != "web-1" || usage[0].Pool != "web" || usage[0].Runtime != "vm" {
		t.Fatalf("the row does not say which runner this is: %+v", usage[0])
	}
	if usage[0].MemoryBytes != 4294967296 {
		t.Fatalf("memory: got %d", usage[0].MemoryBytes)
	}
	// One reading, so there is no rate yet. A machine mid-boot reported as
	// zero per cent would be a machine burning a core shown as idle.
	if usage[0].CPUPercent != nil {
		t.Fatalf("want no figure from one reading, got %v", *usage[0].CPUPercent)
	}

	cmd.output["systemctl show --timestamp=unix --property=Id,MemoryCurrent"] = "Id=gh-runner@web-1.service\nMemoryCurrent=4294967296\nCPUUsageNSec=2000000000\n"
	again, err := e.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].CPUPercent == nil {
		t.Fatalf("want a figure once the counter has been read twice: %+v", again)
	}
}

// A unit that is not running has no cgroup and systemd says so by printing
// "[not set]". A runner using zero and a runner that cannot be measured are
// different things, and neither is a parse error.
func TestUsageSkipsWhatSystemdCannotAccountFor(t *testing.T) {
	e, cmd, _ := newExecutor(t)
	if err := e.Create(context.Background(), testSpec("web-1")); err != nil {
		t.Fatal(err)
	}
	cmd.output["systemctl show --timestamp=unix --property=Id,MemoryCurrent"] = "Id=gh-runner@web-1.service\nMemoryCurrent=[not set]\nCPUUsageNSec=[not set]\n"

	usage, err := e.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].MemoryBytes != 0 || usage[0].CPUPercent != nil {
		t.Fatalf("want the runner listed with nothing measured: %+v", usage)
	}
}

// A machine is reported the way a container is: the charge, less the file cache
// the kernel would drop rather than run out.
//
// This matters more for a machine than for a container, and the reason is in
// the QEMU command line. The disk is opened cache=writeback, so every block a
// guest reads to boot is host page cache — charged to the unit's cgroup, and
// counted by MemoryCurrent. A freshly booted machine that has done nothing at
// all therefore reports the better part of a gigabyte, most of which the kernel
// would hand back the moment anything asked.
//
// The container executor has always taken this out; see
// TestUsageSubtractsThePageCacheFromMemory in the docker package, which is the
// same assertion against the same helper. The two runtimes appear side by side
// in one table under one heading, so a difference here is not an inaccuracy in
// one column — it is two columns that cannot be compared, which is worse,
// because the table invites exactly that comparison.
func TestUsageSubtractsThePageCacheFromMemory(t *testing.T) {
	e, cmd, root := newExecutorWithCgroupRoot(t)
	if err := e.Create(context.Background(), testSpec("web-1")); err != nil {
		t.Fatal(err)
	}
	cmd.output["systemctl show --timestamp=unix --property=Id,MemoryCurrent"] =
		"Id=gh-runner@web-1.service\nMemoryCurrent=1073741824\nCPUUsageNSec=1\n" +
			"ControlGroup=/system.slice/system-gh\\x2drunner.slice/gh-runner@web-1.service\n"
	writeCgroupStats(t, root, "/system.slice/system-gh\\x2drunner.slice/gh-runner@web-1.service",
		"anon 402653184\nfile 536870912\ninactive_file 536870912\nslab 1048576\n")

	usage, err := e.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 {
		t.Fatalf("got %v", usage)
	}
	if usage[0].MemoryBytes != 536870912 {
		t.Fatalf("memory: got %d, want 536870912 — a gigabyte charged, half of it "+
			"the qcow2 this machine booted from", usage[0].MemoryBytes)
	}
}

// cgroup v1 puts the same figure behind a controller directory and under a
// different name. Both are read, so a host on either kernel gets the same
// answer rather than one of them quietly reporting its cache as memory.
func TestUsageUnderstandsBothCgroupVersions(t *testing.T) {
	e, cmd, root := newExecutorWithCgroupRoot(t)
	if err := e.Create(context.Background(), testSpec("web-1")); err != nil {
		t.Fatal(err)
	}
	cmd.output["systemctl show --timestamp=unix --property=Id,MemoryCurrent"] =
		"Id=gh-runner@web-1.service\nMemoryCurrent=1000\nCPUUsageNSec=1\n" +
			"ControlGroup=/system.slice/gh-runner@web-1.service\n"
	writeCgroupStats(t, filepath.Join(root, "memory"), "/system.slice/gh-runner@web-1.service",
		"total_inactive_file 400\n")

	usage, err := e.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].MemoryBytes != 600 {
		t.Fatalf("the v1 hierarchy was not read: %+v", usage)
	}
}

// A cgroup that cannot be read is not a runner that cannot be reported.
//
// It happens ordinarily: a unit systemd has just stopped keeps its ControlGroup
// property for a moment after the directory is gone, and a daemon that cannot
// see the host's hierarchy at all never has one. Reporting the raw charge is
// the number this gave before any of this existed — too large, and far better
// than a machine vanishing from the page, or one shown as using nothing.
func TestUsageStillReportsAMachineWhoseCgroupCannotBeRead(t *testing.T) {
	e, cmd, _ := newExecutorWithCgroupRoot(t)
	if err := e.Create(context.Background(), testSpec("web-1")); err != nil {
		t.Fatal(err)
	}
	cmd.output["systemctl show --timestamp=unix --property=Id,MemoryCurrent"] =
		"Id=gh-runner@web-1.service\nMemoryCurrent=4096\nCPUUsageNSec=1\n" +
			"ControlGroup=/system.slice/gh-runner@web-1.service\n"
	// No memory.stat written: the directory is not there.

	usage, err := e.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].MemoryBytes != 4096 {
		t.Fatalf("want the whole charge and a runner still on the page: %+v", usage)
	}
}

func TestUsageOfAnEmptyHostAsksSystemdNothing(t *testing.T) {
	e, cmd, _ := newExecutor(t)

	usage, err := e.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 0 {
		t.Fatalf("got %v", usage)
	}
	if cmd.called("MemoryCurrent") {
		t.Fatal("systemd was asked about a fleet that does not exist")
	}
}

// The unit name is read out of each block rather than counted off against the
// list that was asked for, so a unit systemd has never heard of — one removed
// between the listing and the question — cannot shift every later answer onto
// the wrong runner.
func TestPropertiesAreMatchedByNameNotByPosition(t *testing.T) {
	e, cmd, _ := newExecutor(t)
	for _, name := range []string{"web-1", "web-2"} {
		if err := e.Create(context.Background(), testSpec(name)); err != nil {
			t.Fatal(err)
		}
	}
	// Only the second unit comes back, and it comes back first.
	cmd.output["systemctl show --timestamp=unix --property=Id,MemoryCurrent"] = "Id=gh-runner@web-2.service\nMemoryCurrent=512\nCPUUsageNSec=1\n"

	usage, err := e.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 1 || usage[0].Name != "web-2" || usage[0].MemoryBytes != 512 {
		t.Fatalf("the answer landed on the wrong runner: %+v", usage)
	}
}

// A recipe is a shell script and this file is parsed a line at a time — by
// systemd on the way in, and by the daemon's own readEnv on the way back out.
// Encoded, it is one line whichever way it is read.
func TestTheEnvironmentFileCarriesWhatTheImageBakesIn(t *testing.T) {
	_, _, layout := newExecutor(t)

	spec := testSpec("web-1")
	spec.Packages = []string{"nftables", "conntrack"}
	spec.Recipe = "#!/usr/bin/env bash\nset -euo pipefail\necho hello\n"

	env := RenderEnv(spec, layout)
	if !strings.Contains(env, "FLEET_PACKAGES=nftables,conntrack") {
		t.Errorf("the package list is missing:\n%s", env)
	}

	var encoded string
	for _, line := range strings.Split(env, "\n") {
		if value, ok := strings.CutPrefix(line, "FLEET_RECIPE_BASE64="); ok {
			encoded = value
		}
	}
	if encoded == "" {
		t.Fatalf("the recipe is missing:\n%s", env)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("the recipe did not survive the environment file: %v", err)
	}
	if string(decoded) != spec.Recipe {
		t.Fatalf("the recipe came back as %q", decoded)
	}
	// The point of encoding it: no line of the file is part of the script.
	if strings.Contains(env, "echo hello") {
		t.Fatalf("the recipe was written as itself, so its newlines are now the file's:\n%s", env)
	}
}

// A pool with nothing extra writes the file it wrote before either field
// existed, so upgrading does not change what every runner is built from.
func TestAPoolThatBakesNothingWritesNothingExtra(t *testing.T) {
	_, _, layout := newExecutor(t)

	env := RenderEnv(testSpec("web-1"), layout)
	for _, unwanted := range []string{"FLEET_PACKAGES", "FLEET_RECIPE_BASE64"} {
		if strings.Contains(env, unwanted) {
			t.Errorf("%s was written for a pool that asked for nothing:\n%s", unwanted, env)
		}
	}
}

// What makes an edit reach the fleet. The reconciler replaces runners whose
// recipe no longer matches their pool's, so a recipe that did not change the
// image's name would be a recipe that shipped and did nothing — which is the
// bug the image name was made to cover in the first place.
func TestEditingWhatIsBakedInBuildsADifferentImage(t *testing.T) {
	e, _, _ := newExecutor(t)

	plain := model.Pool{Runtime: model.RuntimeVM, Image: "default"}
	withPackages := plain
	withPackages.Packages = []string{"ffmpeg"}
	withRecipe := plain
	withRecipe.Recipe = "echo hello\n"
	edited := plain
	edited.Recipe = "echo goodbye\n"

	base := e.Recipe(plain)
	for name, pool := range map[string]model.Pool{
		"a package list": withPackages,
		"a recipe":       withRecipe,
	} {
		if e.Recipe(pool) == base {
			t.Errorf("%s left the image called %s, so the host would keep booting the old one", name, base)
		}
	}
	if e.Recipe(withRecipe) == e.Recipe(edited) {
		t.Error("two different recipes named the same image")
	}
	if e.Recipe(plain) != e.Recipe(model.Pool{Runtime: model.RuntimeVM, Image: "default"}) {
		t.Error("the same pool named two images, so every pass would rebuild")
	}
}
