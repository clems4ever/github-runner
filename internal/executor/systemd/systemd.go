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
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/clems4ever/github-runner/internal/agent"
	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/paths"
	"github.com/clems4ever/github-runner/internal/qmp"
	"github.com/clems4ever/github-runner/internal/reconcile"
	"github.com/clems4ever/github-runner/internal/resources"
)

// UnitTemplate is the template unit every VM runner is an instance of.
const UnitTemplate = "gh-runner@"

// Slice is the one control group every machine on this host runs inside.
//
// Every runner is already a cgroup of its own, which is how the fleet is
// measured. What was missing was a parent over all of them: per-runner limits
// are per runner, and say nothing about what happens when every pool is busy at
// once. This is that parent, and a limit set on it is a limit on the fleet.
//
// Runners are put in it whether or not a budget has been set, and that is the
// point. A slice is joined when a unit starts, so a fleet that only moved into
// one when somebody set a budget would have to be replaced before the budget
// meant anything — every machine drained, on a host that had just been told it
// was using too much. Grouping unconditionally makes setting a budget a
// property change on a slice that already holds the fleet, which systemd
// applies to the machines that are running.
//
// The daemon itself is deliberately not in here. It is root's, in system.slice,
// and stays there: a fleet pressed against its memory ceiling must not be able
// to take down the thing an operator would use to raise it.
const Slice = "runner-fleet.slice"

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
	layout    paths.Layout
	cmd       Commander
	binary    string // what the unit runs: this daemon's own binary, in agent mode
	user      string
	unitPath  string
	slicePath string
	// cgroupRoot is where the kernel's memory accounting is mounted. Only
	// Usage reads it, and only for the page cache figure systemd will not
	// report itself.
	cgroupRoot string
	// now is the clock, replaced in tests that need one that does not move.
	now func() time.Time
	// cpu remembers each unit's processor counter between samples, which is
	// what makes a percentage out of it.
	cpu *resources.Rate

	// budget is the last one applied to the slice, kept so that Usage can say
	// when a machine is not inside it. Guarded because the reconciler applies
	// budgets on its own loop and the sampler reads usage on another.
	budgetMu sync.Mutex
	budget   model.Budget
}

// Option configures an executor.
type Option func(*Executor)

// WithCommander replaces the process runner, for tests.
func WithCommander(c Commander) Option { return func(e *Executor) { e.cmd = c } }

// WithUnitPath puts the template unit somewhere other than /etc/systemd/system.
func WithUnitPath(path string) Option { return func(e *Executor) { e.unitPath = path } }

// WithSlicePath puts the fleet's slice somewhere other than
// /etc/systemd/system, so a test can read back what was written for it.
func WithSlicePath(path string) Option { return func(e *Executor) { e.slicePath = path } }

// WithClock replaces the clock, so a test can say how long a drain has taken.
func WithClock(now func() time.Time) Option { return func(e *Executor) { e.now = now } }

// WithCgroupRoot puts the kernel's accounting somewhere other than
// /sys/fs/cgroup, so a test can lay out a memory.stat of its own rather than
// depend on what the machine running it happens to have mounted.
func WithCgroupRoot(path string) Option { return func(e *Executor) { e.cgroupRoot = path } }

// New builds the executor.
func New(layout paths.Layout, binary, user string, opts ...Option) *Executor {
	e := &Executor{
		layout:     layout,
		cmd:        execCommander{},
		binary:     binary,
		user:       user,
		unitPath:   "/etc/systemd/system/" + UnitTemplate + ".service",
		slicePath:  "/etc/systemd/system/" + Slice,
		cgroupRoot: "/sys/fs/cgroup",
		now:        time.Now,
		cpu:        resources.NewRate(),
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
	return agent.ImageSpec{Variant: pool.Image, Packages: pool.Packages, Recipe: pool.Recipe}.Name()
}

// EnsureUnit writes the template unit and reloads systemd if it changed.
//
// Rewriting it on every start is what keeps an upgraded daemon's unit in step
// with the binary that wrote it; skipping the reload when nothing changed is
// what keeps that from being noisy.
func (e *Executor) EnsureUnit(ctx context.Context) error {
	return e.install(ctx, e.unitPath, e.renderUnit())
}

// install writes one unit file and reloads systemd if it changed.
//
// Skipping the reload when nothing changed is what keeps rewriting these on
// every daemon start from being noisy — and a reload re-reads unit files
// without restarting anything, which is the point: an upgraded daemon must not
// bounce the fleet.
func (e *Executor) install(ctx context.Context, path, want string) error {
	if current, err := os.ReadFile(path); err == nil && string(current) == want {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	_, err := e.cmd.Run(ctx, "systemctl", "daemon-reload")
	return err
}

// ApplyBudget holds the whole fleet to what the operator asked for.
//
// The limits go on the slice rather than on the runners, which is what makes
// them a budget rather than sixty separate allowances: the kernel accounts for
// the group, so ten idle machines leave their share to the one that is
// building, and the eleventh cannot take the host down because the group it is
// in has already spent everything.
//
// It is safe to call on every reconcile pass, and is: a budget changed in the
// UI has to reach the fleet without anyone restarting anything. Nothing is
// written when the rendered slice is identical to what is already there, so
// the ordinary pass costs one file read.
//
// The change applies to machines that are already running. A slice's limits are
// properties of a control group that exists and holds them, so lowering a
// ceiling squeezes the fleet that is running under it rather than waiting for
// the next one — which is the behaviour an operator lowering it wants, and the
// reason membership is not conditional on there being a budget at all.
func (e *Executor) ApplyBudget(ctx context.Context, budget model.Budget) error {
	if err := budget.Validate(); err != nil {
		return err
	}
	e.budgetMu.Lock()
	e.budget = budget
	e.budgetMu.Unlock()
	return e.install(ctx, e.slicePath, renderSlice(budget))
}

// renderSlice is the fleet's control group, and whatever the budget asks of it.
//
// A slice with no limits is still written. It is where the fleet lives, and
// having it there with accounting switched on means systemd-cgtop shows the
// whole fleet as one line on a host that has never set a budget — and means
// setting one later changes a property rather than moving anything.
func renderSlice(budget model.Budget) string {
	var b strings.Builder
	fmt.Fprintf(&b, `# Installed by runner-fleet. Do not edit: it is rewritten from the fleet
# budget in the web UI, and by hand it would last until the next reconcile.
#
# Every machine runner on this host is in this group. The daemon is not: it is
# in system.slice, so a fleet against its ceiling cannot take down the only
# thing that can raise it.

[Unit]
Description=GitHub Actions runners (runner-fleet)
Documentation=https://github.com/clems4ever/github-runner

[Slice]
# On unconditionally. The fleet's own figures come from these, and a budget
# that is switched on later needs the accounting to have been there all along.
CPUAccounting=yes
MemoryAccounting=yes
`)

	if budget.CPUs > 0 {
		fmt.Fprintf(&b, `
# %d processors across every machine together, whatever the pools were promised
# individually. This is throughput and not a set of cores: the scheduler still
# puts the work wherever it likes.
CPUQuota=%d%%
`, budget.CPUs, budget.CPUs*100)
	}
	if budget.CPUWeight > 0 {
		fmt.Fprintf(&b, `
# What the fleet gets when something else on this host wants the machine too.
# systemd's default is 100, so below that is a fleet that yields.
CPUWeight=%d
`, budget.CPUWeight)
	}
	if budget.MemoryMB > 0 {
		fmt.Fprintf(&b, `
# Pressure, not a wall: past this the kernel reclaims from the fleet harder,
# and the fleet gets slower. The alternative is the out-of-memory killer, which
# picks the largest machine in the group rather than the one that overspent —
# so the default costs minutes and the alternative costs somebody's job.
MemoryHigh=%dM
`, budget.MemoryMB)
	}
	if hard := budget.HardMemoryBytes(); hard > 0 {
		fmt.Fprintf(&b, `
# And the wall, %d%% above it, because the operator asked for one. Reaching this
# kills a machine mid-job. It sits above MemoryHigh rather than on it so that
# there is room for the reclaim above to work before it comes to that.
MemoryMax=%dM
`, model.HardMemoryHeadroom, hard/(1024*1024))
	}
	return b.String()
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

# A runner that cannot work would otherwise restart for ever. The window is
# sized for the healthy case rather than against it: an ephemeral machine takes
# about thirty seconds to come back between jobs, so a busy pool does twenty
# starts in ten minutes and must not be mistaken for a crash loop. A machine
# that fails immediately does its thirty in a couple of minutes and is stopped.
StartLimitIntervalSec=600
StartLimitBurst=30

[Service]
Type=simple
User=%s
SupplementaryGroups=kvm

# Every machine on this host runs inside one group, so that the fleet can be
# held to a budget as a whole rather than one runner at a time. The limits live
# on the slice and not here; this line is only the membership, and it is
# unconditional so that setting a budget later does not mean replacing the
# fleet before it takes effect.
Slice=%s

EnvironmentFile=%s/%%i.env
ExecStart=%s agent --name %%i

Restart=always
# Except when the image it would boot has not been built. That is the daemon's
# to fix — by building it, or by not asking for this runner at all — and a unit
# that retried it would be back to rebuilding a broken recipe every two seconds
# with nobody reading the result.
RestartPreventExitStatus=%d
# Two seconds, not fifteen. An ephemeral runner is replaced after every single
# job, so this delay is paid by every job on the host — it was a third of the
# gap between one finishing and the next being able to start. The start limiter
# above is what protects the host from a runner that cannot work, and it does
# that job better than a long delay does.
RestartSec=2

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
`, e.layout.RunnersDir(), e.user, Slice, e.layout.RunnersDir(), e.binary, agent.ExitImageNotBuilt)
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
	if len(spec.Packages) > 0 {
		fmt.Fprintf(&b, "FLEET_PACKAGES=%s\n", strings.Join(spec.Packages, ","))
	}
	// Base64, and not because a recipe is secret — it is not, and anyone who
	// can read this file can read the credential path beside it. It is because
	// a recipe is a shell script with newlines in it, and this file is parsed
	// a line at a time by systemd on the way in and by the daemon's own
	// readEnv on the way back out. One encoded line cannot break either.
	if spec.Recipe != "" {
		fmt.Fprintf(&b, "FLEET_RECIPE_BASE64=%s\n", base64.StdEncoding.EncodeToString([]byte(spec.Recipe)))
	}
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
			if unit.state == reconcile.StateRunning {
				runners[i].Up = unit.running
			}
			runners[i].Coming = unit.coming
		}
		if trouble := e.machineTrouble(runners[i]); trouble != "" {
			runners[i].Trouble = trouble
		}
	}
	sort.Slice(runners, func(i, j int) bool { return runners[i].Name < runners[j].Name })
	return runners, nil
}

// machineTrouble asks QEMU whether the machine behind a running unit is
// actually executing instructions, and says so when it is not.
//
// systemd cannot answer this. A machine QEMU has stopped has a live process and
// a perfectly active unit, so every layer above reports it as running while it
// executes nothing: the fleet showed RUNNING, the runner inside stopped
// heartbeating, and GitHub's view of its job went to unknown. Four machines
// once sat like that for hours across a whole host, and restarting the daemon
// could not have helped — the daemon does not own them, and says so on the way
// out ("shutting down; the runners keep running").
//
// The state that matters is io-error. QEMU's default write-error policy stops a
// machine when the host has no space left rather than passing the error into
// the guest — which is the right trade, because it keeps the guest's filesystem
// whole instead of corrupting it. But nothing resumes a machine on its own, so
// the fleet has to be the thing that notices.
//
// Best effort by design: a monitor that cannot be reached says nothing here.
// The unit state above is what the reconciler acts on, and a machine mid-boot
// or mid-shutdown has no monitor to answer.
func (e *Executor) machineTrouble(runner reconcile.Runner) string {
	if runner.Runtime != model.RuntimeVM || runner.State == reconcile.StateStopped {
		return ""
	}
	status, err := qmp.Status(e.layout.QMPSocket(runner.Name))
	if err != nil || status == qmp.StatusRunning {
		return ""
	}
	if status == qmp.StatusIOError {
		return fmt.Sprintf("stopped by QEMU on a write error and will not resume on its own — "+
			"check the host's free space, then: runner-fleet machine resume %s", runner.Name)
	}
	return fmt.Sprintf("QEMU reports the machine is %q, not running", status)
}

// unit is what systemd says about one runner.
type unit struct {
	state    reconcile.RunnerState
	result   string
	restarts int
	// draining is how long the unit has been stopping, when it is.
	draining time.Duration
	// running is how long it has been up, when it is. A machine takes a minute
	// or two to boot and register, and that is not the same thing as a runner
	// GitHub has never heard of.
	running time.Duration
	// coming is systemd bringing it up: starting, or waiting out the restart
	// delay between two machines.
	coming bool
}

// patience is how long a drain can take before it is worth mentioning.
//
// Stopping a runner waits for the job it is on, so minutes are ordinary and a
// clock is not a failure. But a machine that missed the power button waits out
// the agent's whole grace period looking exactly like a machine finishing a
// long job, and half an hour of that is worth saying out loud.
const patience = 15 * time.Minute

// trouble is what to tell a person, and nothing when there is nothing to tell.
//
// The count is the useful part: one failure is a runner that finished a job and
// is coming back, and nine is a runner that has never worked. The pointer to
// the journal is there because the reason is always there and never here.
func (u unit) trouble(runner string) string {
	// A drain that has outlived any plausible job. Not necessarily broken —
	// a three-hour job is allowed — but the fleet should say how long rather
	// than showing "stopping" for an hour with no clock on it.
	if u.state == reconcile.StateStopping && u.draining > patience {
		return fmt.Sprintf("draining for %s; if no job is running, the machine is not shutting down: journalctl -u %s",
			u.draining.Round(time.Minute), unitName(runner))
	}
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
	shown, err := e.show(ctx, []string{"ActiveState", "SubState", "Result", "NRestarts",
		"ActiveExitTimestamp", "ActiveEnterTimestamp"}, units)
	if err != nil {
		return nil, err
	}

	states := make(map[string]unit, len(shown))
	for id, properties := range shown {
		count, _ := strconv.Atoi(properties["NRestarts"])
		states[id] = unit{
			state:    mapActiveState(properties["ActiveState"], properties["SubState"]),
			result:   properties["Result"],
			restarts: count,
			draining: e.since(properties["ActiveExitTimestamp"]),
			running:  e.since(properties["ActiveEnterTimestamp"]),
			// Both sub-states of "activating" are on the way up: "start" is a
			// unit systemd is launching, "auto-restart" is one waiting out
			// RestartSec, which for an ephemeral runner is the gap between two
			// machines.
			coming: properties["ActiveState"] == "activating",
		}
	}
	return states, nil
}

// show reads properties of several units in one systemctl call, keyed by unit
// name.
//
// Id is always asked for and is what keys the result: systemctl prints one
// block per unit separated by a blank line, in the order they were named, and
// reading the name out of the block rather than counting blocks is what keeps
// this correct when a unit systemd has never heard of is in the list.
//
// --timestamp=unix so that any timestamp among the properties comes back as a
// number rather than as a locale's idea of a date.
func (e *Executor) show(ctx context.Context, properties, units []string) (map[string]map[string]string, error) {
	args := append([]string{"show", "--timestamp=unix",
		"--property=Id," + strings.Join(properties, ",")}, units...)
	out, err := e.cmd.Run(ctx, "systemctl", args...)
	if err != nil {
		return nil, err
	}

	shown := map[string]map[string]string{}
	block := map[string]string{}
	flush := func() {
		if id := block["Id"]; id != "" {
			shown[id] = block
		}
		block = map[string]string{}
	}
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			flush()
			continue
		}
		if key, value, ok := strings.Cut(line, "="); ok {
			block[key] = value
		}
	}
	flush()
	return shown, scanner.Err()
}

// Usage reports what each machine on this host is consuming.
//
// The numbers come from systemd rather than from QEMU, and that is the whole
// trick: every runner is a unit, every unit is a cgroup, and the kernel is
// already accounting for it. Nothing has to be read out of the guest, nothing
// has to know where QEMU put its process, and a machine that has wandered off
// into swap is still counted.
//
// A unit that is not running has no cgroup and no figures. systemd says so by
// printing "[not set]", which parses as nothing and is skipped: a stopped
// runner using zero and a stopped runner we cannot measure are the same thing.
//
// MemoryCurrent alone is not the answer, though, which is why ControlGroup is
// asked for beside it. What systemd reports is the cgroup's whole charge, page
// cache included, and QEMU reads its disk through the host's cache — so a good
// part of what a booted machine appears to hold is the qcow2 it booted from,
// which the kernel would drop the moment anything wanted the memory. The
// container executor has always taken that out. This did not, and the two sat
// next to each other in one table looking like a fair comparison. memory.stat
// is where the figure to subtract lives, and the kernel is the only one who
// has it — hence the trip to the filesystem for a number systemd will not
// report.
func (e *Executor) Usage(ctx context.Context) ([]resources.RunnerUsage, error) {
	runners, err := e.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list what the machines are using: %w", err)
	}
	if len(runners) == 0 {
		return nil, nil
	}

	units := make([]string, 0, len(runners))
	for _, runner := range runners {
		units = append(units, unitName(runner.Name))
	}
	shown, err := e.show(ctx, []string{"MemoryCurrent", "CPUUsageNSec", "ControlGroup"}, units)
	if err != nil {
		return nil, fmt.Errorf("ask systemd what the machines are using: %w", err)
	}

	usage := make([]resources.RunnerUsage, 0, len(runners))
	names := make([]string, 0, len(runners))
	var outside []string
	for _, runner := range runners {
		names = append(names, runner.Name)
		properties, ok := shown[unitName(runner.Name)]
		if !ok {
			continue
		}
		row := resources.RunnerUsage{
			Name:    runner.Name,
			Pool:    runner.Pool,
			Runtime: string(model.RuntimeVM),
		}
		controlGroup := properties["ControlGroup"]
		if memory, err := strconv.ParseInt(properties["MemoryCurrent"], 10, 64); err == nil {
			row.MemoryBytes = resources.WorkingSet(memory, e.cgroupStats(controlGroup))
		}
		if consumed, err := strconv.ParseUint(properties["CPUUsageNSec"], 10, 64); err == nil {
			row.CPUPercent = e.cpu.Percent(runner.Name, consumed)
		}
		if e.strayFromTheSlice(controlGroup) {
			outside = append(outside, runner.Name)
		}
		usage = append(usage, row)
	}
	e.cpu.Keep(names)

	if len(outside) > 0 {
		return usage, fmt.Errorf("these machines started before the fleet budget and are not inside"+
			" it, so they are spending on top of it rather than out of it: %s."+
			" They join as each is replaced; an ephemeral pool does that within a job or two,"+
			" and a fixed one when it is next drained", strings.Join(outside, ", "))
	}
	return usage, nil
}

// strayFromTheSlice reports whether a running machine is outside the fleet's
// group, which is only worth mentioning when there is a budget for it to be
// outside of.
//
// It happens for one reason and it is not a fault: a unit joins its slice when
// it starts, so every machine that was already running when this daemon first
// wrote the slice stays where it was until it is replaced. The window is short
// on an ephemeral pool and indefinite on an idle fixed one, which is exactly
// the case where somebody would otherwise be reading a ceiling that half the
// fleet is not subject to.
//
// A unit with no control group has stopped, and one systemd would not name is
// not evidence of anything.
func (e *Executor) strayFromTheSlice(controlGroup string) bool {
	if controlGroup == "" || controlGroup == "[not set]" {
		return false
	}
	e.budgetMu.Lock()
	budget := e.budget
	e.budgetMu.Unlock()
	if !budget.Capped() {
		return false
	}
	return !strings.Contains(controlGroup, Slice)
}

// cgroupStats is the kernel's own accounting for one unit, or nothing.
//
// Two paths, because the two cgroup versions put it in different places: the
// unified hierarchy hangs memory.stat off the cgroup itself, and v1 has a
// controller directory in front of it. Trying both costs one failed stat call
// on the host that does not have it.
//
// Nothing is an ordinary answer, not an error. A unit that has just stopped
// leaves a ControlGroup property behind for a moment after the directory is
// gone, a daemon in a container may not see the host's hierarchy at all, and
// neither is worth a warning on a page about memory. WorkingSet takes an empty
// map and subtracts nothing, which is the same fallback the container path
// already has when Docker omits the key — so both runtimes degrade the same
// way, and both degrade towards the number they used to report.
func (e *Executor) cgroupStats(controlGroup string) map[string]int64 {
	if controlGroup == "" || controlGroup == "[not set]" {
		return nil
	}
	for _, path := range []string{
		filepath.Join(e.cgroupRoot, controlGroup, "memory.stat"),
		filepath.Join(e.cgroupRoot, "memory", controlGroup, "memory.stat"),
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		stats := map[string]int64{}
		scanner := bufio.NewScanner(strings.NewReader(string(body)))
		for scanner.Scan() {
			key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), " ")
			if !ok {
				continue
			}
			if n, err := strconv.ParseInt(value, 10, 64); err == nil {
				stats[key] = n
			}
		}
		return stats
	}
	return nil
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

// since reads systemd's "@1755870627" timestamp and says how long ago that was.
// Anything it cannot read is no time at all, which reports nothing rather than
// a number made up out of a parse failure.
func (e *Executor) since(stamp string) time.Duration {
	seconds, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(stamp), "@"), 10, 64)
	if err != nil || seconds <= 0 {
		return 0
	}
	return e.now().Sub(time.Unix(seconds, 0))
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
