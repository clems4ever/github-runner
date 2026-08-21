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
	"strings"

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
	fmt.Fprintf(&b, "FLEET_STATE_DIR=%s\n", layout.State)
	return b.String()
}

// Start brings a runner back that exists but is not running.
func (e *Executor) Start(ctx context.Context, name string) error {
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
		runners = append(runners, reconcile.Runner{
			Name:       name,
			Pool:       env["FLEET_POOL"],
			Generation: env["FLEET_GENERATION"],
			Runtime:    model.RuntimeVM,
			State:      reconcile.StateStopped,
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
		if state, ok := states[unitName(runners[i].Name)]; ok {
			runners[i].State = state
		}
	}
	sort.Slice(runners, func(i, j int) bool { return runners[i].Name < runners[j].Name })
	return runners, nil
}

// unitStates asks about every unit in one call rather than one call each: a
// full fleet is one process, not sixty.
func (e *Executor) unitStates(ctx context.Context, units []string) (map[string]reconcile.RunnerState, error) {
	args := append([]string{"show", "--property=Id,ActiveState"}, units...)
	out, err := e.cmd.Run(ctx, "systemctl", args...)
	if err != nil {
		return nil, err
	}

	states := map[string]reconcile.RunnerState{}
	var id, active string
	flush := func() {
		if id != "" {
			states[id] = mapActiveState(active)
		}
		id, active = "", ""
	}
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "Id="):
			id = strings.TrimPrefix(line, "Id=")
		case strings.HasPrefix(line, "ActiveState="):
			active = strings.TrimPrefix(line, "ActiveState=")
		}
	}
	flush()
	return states, scanner.Err()
}

// mapActiveState turns systemd's vocabulary into the three states the
// reconciler reasons about.
func mapActiveState(active string) reconcile.RunnerState {
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
