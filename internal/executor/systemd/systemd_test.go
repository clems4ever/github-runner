package systemd

import (
	"context"
	"os"
	"strings"
	"testing"

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

func testSpec(name string) reconcile.Spec {
	return reconcile.Spec{
		Name: name, Pool: "web", PoolID: 1, Generation: "abc123def456",
		Runtime: model.RuntimeVM, URL: "https://github.com/o/r",
		ScopeKind: model.ScopeRepository, Scope: "o/r",
		Labels: []string{"vm", "nested"}, Ephemeral: true, Nested: true,
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
		"FLEET_LABELS=vm,nested",
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
