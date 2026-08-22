// Package systemd runs each VM runner as a systemd unit.
//
// The daemon deliberately does not supervise these itself. A runner is an
// instance of one template unit, with its configuration in an environment file
// beside it, so the daemon can be stopped, upgraded or crash while every
// runner carries on — and when it comes back it finds them by reading the same
// two places, rather than by remembering anything.
package systemd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/clems4ever/github-runner/internal/agent"
	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/paths"
	"github.com/clems4ever/github-runner/internal/reconcile"
)

// UnitTemplate is the template unit every VM runner is an instance of.
const UnitTemplate = "gh-runner@"

// Commander runs systemctl. It is an interface so the executor can be tested
// without systemd, which no CI runner has in a usable state.
type Commander interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

type execCommander struct{}

func (execCommander) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Executor creates, drains and removes VM runners.
type Executor struct {
	layout   paths.Layout
	cmd      Commander
	binary   string // what the unit runs: this daemon's own binary, in agent mode
	user     string
	unitPath string
}

// Option configures an executor.
type Option func(*Executor)

// WithCommander replaces the process runner, for tests.
func WithCommander(c Commander) Option { return func(e *Executor) { e.cmd = c } }

// WithUnitPath puts the template unit somewhere other than /etc/systemd/system.
func WithUnitPath(path string) Option { return func(e *Executor) { e.unitPath = path } }

// New builds the executor.
func New(layout paths.Layout, binary, user string, opts ...Option) *Executor {
	e := &Executor{
		layout:   layout,
		cmd:      execCommander{},
		binary:   binary,
		user:     user,
		unitPath: "/etc/systemd/system/" + UnitTemplate + ".service",
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Runtime is what this executor runs.
func (e *Executor) Runtime() model.Runtime { return model.RuntimeVM }

// Recipe is the golden image a machine in this pool would boot from.
//
// Its name is a hash of everything the image is built from, so this changes
// whenever the daemon changes how it builds machines — a new runner version,
// a different package list, a different provisioning script. Runners built from
// the old image are then no longer what the pool asked for, and are replaced
// gracefully, which is the whole reason the reconciler asks.
func (e *Executor) Recipe(pool model.Pool) string {
	return agent.ImageSpec{Variant: pool.Image}.Name()
}

// EnsureUnit writes the template unit and reloads systemd if it changed.
//
// Rewriting it on every start is what keeps an upgraded daemon's unit in step
// with the binary that wrote it; skipping the reload when nothing changed is
// what keeps that from being noisy.
func (e *Executor) EnsureUnit(ctx context.Context) error {
	want := e.renderUnit()
	if current, err := os.ReadFile(e.unitPath); err == nil && string(current) == want {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(e.unitPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(e.unitPath, []byte(want), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", e.unitPath, err)
	}
	// A reload re-reads unit files. It does not restart anything, which is the
	// point: an upgraded daemon must not bounce the fleet.
	_, err := e.cmd.Run(ctx, "systemctl", "daemon-reload")
	return err
}

func (e *Executor) renderUnit() string {
	return fmt.Sprintf(`# Installed by runner-fleet. Do not edit: it is rewritten on every daemon start.
#
# One template unit serves every VM runner on this host. What a runner is —
# which repository, which labels, how big — lives in %s/%%i.env,
# so adding a runner is writing a file and starting an instance.
#
# Nothing here ties a runner to runner-fleetd. That is deliberate: the daemon
# is a reconciler, not a supervisor, and restarting or upgrading it must leave
# every job on this host running.

[Unit]
Description=GitHub Actions runner %%i (runner-fleet)
Documentation=https://github.com/clems4ever/github-runner
# The runner polls GitHub, so wait for a usable network. There is deliberately
# no dependency on dev-kvm.device: systemd only creates device units for
# devices udev tags with "systemd", which never includes misc, where kvm lives.
After=network-online.target
Wants=network-online.target

# A runner that cannot register would otherwise restart every fifteen seconds
# for ever. Ten starts in five minutes is clear of normal ephemeral churn.
StartLimitIntervalSec=300
StartLimitBurst=10

[Service]
Type=simple
User=%s
SupplementaryGroups=kvm

EnvironmentFile=%s/%%i.env
ExecStart=%s agent --name %%i

Restart=always
RestartSec=15

# SIGTERM must reach the agent alone. QEMU daemonises itself but stays in the
# service cgroup, so the default would signal it directly — the equivalent of
# pulling the machine's power cord — instead of letting the agent shut the
# guest down.
KillMode=mixed
KillSignal=SIGTERM

# Stopping is not instant by design: the runner finishes the job it is on
# first. This is the whole reason a scale-down or a reconfiguration cannot fail
# a job, and it has to stay in step with the agent's own shutdown timeout.
TimeoutStopSec=3660

[Install]
WantedBy=multi-user.target
`, e.layout.RunnersDir(), e.user, e.layout.RunnersDir(), e.binary)
}

func unitName(runner string) string { return UnitTemplate + runner + ".service" }

// Create writes a runner's configuration and starts it.
func (e *Executor) Create(ctx context.Context, spec reconcile.Spec) error {
	if err := os.MkdirAll(e.layout.RunnersDir(), 0o700); err != nil {
		return err
	}
	// 0600: it names a private repository and points at a credential file.
	if err := os.WriteFile(e.layout.RunnerEnv(spec.Name), []byte(RenderEnv(spec, e.layout)), 0o600); err != nil {
		return fmt.Errorf("write the runner's configuration: %w", err)
	}
	// enable as well as start, so the fleet comes back after a reboot without
	// waiting for the daemon to notice.
	_, err := e.cmd.Run(ctx, "systemctl", "enable", "--now", unitName(spec.Name))
	return err
}

// RenderEnv is the environment file a runner's unit reads. It is also the
// record that this runner exists and what it was built from, which is how the
// daemon rediscovers the fleet after a restart.
func RenderEnv(spec reconcile.Spec, layout paths.Layout) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by runner-fleet for %s. Do not edit: the daemon rewrites it.\n", spec.Name)
	fmt.Fprintf(&b, "FLEET_RUNNER=%s\n", spec.Name)
	fmt.Fprintf(&b, "FLEET_POOL=%s\n", spec.Pool)
	// The generation is what tells a later reconcile that this runner was
	// built from a configuration that has since changed.
	fmt.Fprintf(&b, "FLEET_GENERATION=%s\n", spec.Generation)
	fmt.Fprintf(&b, "FLEET_URL=%s\n", spec.URL)
	fmt.Fprintf(&b, "FLEET_SCOPE_KIND=%s\n", spec.ScopeKind)
	fmt.Fprintf(&b, "FLEET_SCOPE=%s\n", spec.Scope)
	fmt.Fprintf(&b, "FLEET_LABELS=%s\n", strings.Join(spec.Labels, ","))
	fmt.Fprintf(&b, "FLEET_EPHEMERAL=%t\n", spec.Ephemeral)
	fmt.Fprintf(&b, "FLEET_NESTED=%t\n", spec.Nested)
	fmt.Fprintf(&b, "FLEET_CPUS=%d\n", spec.CPUs)
	fmt.Fprintf(&b, "FLEET_MEMORY_MB=%d\n", spec.MemoryMB)
	fmt.Fprintf(&b, "FLEET_DISK_GB=%d\n", spec.DiskGB)
	fmt.Fprintf(&b, "FLEET_IMAGE=%s\n", spec.Image)
	// The token itself is never in here. This points at a file on tmpfs that
	// the daemon rewrites, so the credential never reaches a disk and a runner
	// can still mint a registration token on its own after a reboot.
	fmt.Fprintf(&b, "FLEET_CREDENTIAL_FILE=%s\n", layout.Credential(spec.CredentialID))
	// The id as well as the path: a runner whose pool has been deleted is still
	// registered somewhere, and this is what lets the daemon find the
	// credential that can ask GitHub about it.
	fmt.Fprintf(&b, "FLEET_CREDENTIAL_ID=%d\n", spec.CredentialID)
	// A GitHub App's agent does its own JWT exchange, so it needs to know which
	// app the key belongs to. None of this is secret; the key beside it is.
	if spec.CredentialKind != "" {
		fmt.Fprintf(&b, "FLEET_CREDENTIAL_KIND=%s\n", spec.CredentialKind)
	}
	if spec.AppID != 0 {
		fmt.Fprintf(&b, "FLEET_APP_ID=%d\n", spec.AppID)
	}
	if spec.InstallationID != 0 {
		fmt.Fprintf(&b, "FLEET_INSTALLATION_ID=%d\n", spec.InstallationID)
	}
	fmt.Fprintf(&b, "FLEET_STATE_DIR=%s\n", layout.State)
	return b.String()
}

// Start brings a runner back that exists but is not running.
//
// The spec is not needed: a machine keeps its own credential and mints what it
// needs at boot, so starting the unit again is enough.
func (e *Executor) Start(ctx context.Context, spec reconcile.Spec) error {
	name := spec.Name
	// A unit that failed keeps its restart counter, and would refuse to start
	// with "start request repeated too quickly".
	_, _ = e.cmd.Run(ctx, "systemctl", "reset-failed", unitName(name))
	_, err := e.cmd.Run(ctx, "systemctl", "start", unitName(name))
	return err
}

// Drain asks a runner to stop once its job is done, and returns immediately.
//
// --no-block is what makes that possible: the stop itself waits for the runner
// to finish, which can take an hour, and a reconcile loop that blocked on it
// would stall every other pool on the host. The unit shows as deactivating in
// the meantime, which List reports as stopping.
func (e *Executor) Drain(ctx context.Context, name string) error {
	_, err := e.cmd.Run(ctx, "systemctl", "stop", "--no-block", unitName(name))
	return err
}

// Remove deletes a runner that has stopped.
func (e *Executor) Remove(ctx context.Context, name string) error {
	if _, err := e.cmd.Run(ctx, "systemctl", "disable", unitName(name)); err != nil {
		// A unit that was never enabled is not an error worth stopping for.
		_ = err
	}
	_, _ = e.cmd.Run(ctx, "systemctl", "reset-failed", unitName(name))
	if err := os.Remove(e.layout.RunnerEnv(name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove the runner's configuration: %w", err)
	}
	return nil
}

// List reports every VM runner on this host.
//
// The environment files are the enumeration, not systemctl: a file is written
// before the unit is started and removed after it is gone, so it is the record
// of what this daemon is responsible for. systemctl is then asked what state
// each one is in.
func (e *Executor) List(ctx context.Context) ([]reconcile.Runner, error) {
	entries, err := os.ReadDir(e.layout.RunnersDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var (
		runners []reconcile.Runner
		units   []string
	)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".env") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".env")
		env, err := readEnv(filepath.Join(e.layout.RunnersDir(), entry.Name()))
		if err != nil {
			continue
		}
		credentialID, _ := strconv.ParseInt(env["FLEET_CREDENTIAL_ID"], 10, 64)
		runners = append(runners, reconcile.Runner{
			Name:         name,
			Pool:         env["FLEET_POOL"],
			Generation:   env["FLEET_GENERATION"],
			Runtime:      model.RuntimeVM,
			State:        reconcile.StateStopped,
			ScopeKind:    model.ScopeKind(env["FLEET_SCOPE_KIND"]),
			Scope:        env["FLEET_SCOPE"],
			CredentialID: credentialID,
		})
		units = append(units, unitName(name))
	}
	if len(runners) == 0 {
		return nil, nil
	}

	states, err := e.unitStates(ctx, units)
	if err != nil {
		return nil, err
	}
	for i := range runners {
		if unit, ok := states[unitName(runners[i].Name)]; ok {
			runners[i].State = unit.state
			runners[i].Trouble = unit.trouble(runners[i].Name)
		}
	}
	sort.Slice(runners, func(i, j int) bool { return runners[i].Name < runners[j].Name })
	return runners, nil
}

// unit is what systemd says about one runner.
type unit struct {
	state    reconcile.RunnerState
	result   string
	restarts int
}

// trouble is what to tell a person, and nothing when there is nothing to tell.
//
// The count is the useful part: one failure is a runner that finished a job and
// is coming back, and nine is a runner that has never worked. The pointer to
// the journal is there because the reason is always there and never here.
func (u unit) trouble(runner string) string {
	if u.result == "" || u.result == "success" {
		return ""
	}
	if u.restarts > 1 {
		return fmt.Sprintf("failing to start (%s), %d times over; journalctl -u %s",
			u.result, u.restarts, unitName(runner))
	}
	return fmt.Sprintf("failed to start (%s); journalctl -u %s", u.result, unitName(runner))
}

// unitStates asks about every unit in one call rather than one call each: a
// full fleet is one process, not sixty.
func (e *Executor) unitStates(ctx context.Context, units []string) (map[string]unit, error) {
	args := append([]string{"show", "--property=Id,ActiveState,SubState,Result,NRestarts"}, units...)
	out, err := e.cmd.Run(ctx, "systemctl", args...)
	if err != nil {
		return nil, err
	}

	states := map[string]unit{}
	var id, active, sub, result, restarts string
	flush := func() {
		if id != "" {
			count, _ := strconv.Atoi(restarts)
			states[id] = unit{state: mapActiveState(active, sub), result: result, restarts: count}
		}
		id, active, sub, result, restarts = "", "", "", "", ""
	}
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		key, value, _ := strings.Cut(line, "=")
		switch {
		case line == "":
			flush()
		case key == "Id":
			id = value
		case key == "ActiveState":
			active = value
		case key == "SubState":
			sub = value
		case key == "Result":
			result = value
		case key == "NRestarts":
			restarts = value
		}
	}
	flush()
	return states, scanner.Err()
}

// mapActiveState turns systemd's vocabulary into the three states the
// reconciler reasons about.
//
// The sub-state matters for exactly one case, and it is the case that hid a bug
// for a week. A unit that fails and is waiting out its RestartSec sits in
// ActiveState=activating, SubState=auto-restart. Reading only the first, a
// runner that had crashed on startup nine times in a row reported as running,
// on the dashboard and to the reconciler both — which then left it alone,
// because as far as it could tell there was nothing wrong.
func mapActiveState(active, sub string) reconcile.RunnerState {
	if active == "activating" && sub == "auto-restart" {
		// Not running, and not going to be: it is dead and waiting to be tried
		// again. Saying so lets the reconciler start it properly, which also
		// clears the restart counter that would otherwise refuse with "start
		// request repeated too quickly".
		return reconcile.StateStopped
	}
	switch active {
	case "active", "activating", "reloading":
		return reconcile.StateRunning
	case "deactivating":
		// Draining: the runner is finishing its job. Reporting this as stopped
		// would let the reconciler delete the machine out from under it.
		return reconcile.StateStopping
	default: // inactive, failed, or a unit systemd has never heard of
		return reconcile.StateStopped
	}
}

func readEnv(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	env := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			env[key] = value
		}
	}
	return env, scanner.Err()
}
