package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/model"
)

// shutdownGrace is how long the agent waits for a machine to stop on its own
// after asking. It is longer than any job worth waiting for, and it is what
// the unit's TimeoutStopSec has to exceed — systemd must not lose patience
// before the agent does, or the job dies to a SIGKILL after all.
const shutdownGrace = 60 * time.Minute

// Run is one runner, from registration to shutdown. It returns when the runner
// has stopped, and it is the whole of what a systemd unit or a container
// entrypoint executes.
func Run(ctx context.Context, c Config, log *slog.Logger) error {
	// The agent must react to SIGTERM itself rather than dying to it: that
	// signal is the fleet asking a runner to finish its job and stop, and
	// passing it straight through would be a power cut.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	if c.Runtime == model.RuntimeContainer {
		return runContainer(ctx, c, log)
	}
	return runVM(ctx, c, log)
}

// registrationToken mints the short-lived token the runner registers with.
// One per boot: it expires an hour after it is issued, so it cannot be stored
// in the machine's configuration and reused.
func registrationToken(ctx context.Context, c Config) (string, error) {
	// A token the daemon minted for this runner, which is how a container
	// registers without ever being given the credential itself.
	if minted := c.RegistrationToken(); minted != "" {
		return minted, nil
	}

	secret, err := c.Token()
	if err != nil {
		return "", err
	}
	client, err := github.NewFromSecret(github.Secret{
		IsAppCredential: c.CredentialKind == model.CredentialApp,
		Token:           secret,
		AppID:           c.AppID,
		InstallationID:  c.InstallationID,
	})
	if err != nil {
		return "", err
	}
	return client.RegistrationToken(ctx, github.Scope{Kind: c.ScopeKind, Path: c.Scope})
}

func runVM(ctx context.Context, c Config, log *slog.Logger) error {
	// The image is the daemon's to build, and it builds it before it creates
	// this runner at all. So a machine that finds one missing does not build
	// one: it says which image it wanted and stops, and its unit is told not to
	// restart it. A runner that quietly built its own image was how a broken
	// recipe used to turn into a unit rebuilding a fleet's worth of it, every
	// two seconds, with the account of each attempt thrown away.
	spec := ImageSpec{Variant: c.Image, Packages: c.Packages, Recipe: c.Recipe}
	golden, built := GoldenImage(spec, filepath.Join(c.StateDir, "images"))
	if !built {
		return fmt.Errorf("%w: %s. The daemon builds it; this runner should not have been started yet",
			ErrImageNotBuilt, filepath.Base(golden))
	}

	dir := filepath.Join(c.StateDir, "vms", c.Runner)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// A machine is rebuilt from scratch every start. Whatever the last job
	// left behind goes with it, which is the entire point of the runtime.
	defer os.RemoveAll(dir)

	publicKey, err := EnsureSSHKey(filepath.Join(c.StateDir, "ssh", "id_ed25519"))
	if err != nil {
		return err
	}

	// A copy-on-write overlay, so booting is seconds rather than minutes and
	// the golden image is never written to.
	disk := filepath.Join(dir, "disk.qcow2")
	if err := run(ctx, "qemu-img", "create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", golden, disk,
		fmt.Sprintf("%dG", c.DiskGB)); err != nil {
		return fmt.Errorf("create the machine's disk: %w", err)
	}

	token, err := registrationToken(ctx, c)
	if err != nil {
		return err
	}

	seed := filepath.Join(dir, "seed.iso")
	if err := makeSeed(ctx, runUserData(c, token, publicKey),
		metaData(c.Runner, fmt.Sprintf("%s-%d", c.Runner, time.Now().Unix())), seed); err != nil {
		return err
	}

	port, err := freePort()
	if err != nil {
		return err
	}

	options := VMOptions{
		Name: c.Runner, Dir: dir, Disk: disk, Seed: seed,
		CPUs: c.CPUs, MemoryMB: c.MemoryMB, SSHPort: port,
		CPUModel:  CPUModel(CPUVendor(), c.Nested),
		QMPSocket: filepath.Join(dir, "qmp.sock"),
		Console:   filepath.Join(dir, "console.log"),
	}

	log.Info("booting", "runner", c.Runner, "cpus", c.CPUs, "memory_mb", c.MemoryMB,
		"nested", c.Nested, "ssh_port", port, "image", filepath.Base(golden))

	// The context is not passed to the process: killing QEMU on cancellation
	// is exactly the power cut this is built to avoid. Shutdown is handled
	// below instead.
	cmd, err := bootVM(context.WithoutCancel(ctx), options)
	if err != nil {
		return err
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	press := func() error {
		if err := powerDown(options.QMPSocket); err != nil {
			// The monitor is the polite route. Without it the only thing left
			// is a signal to QEMU, which is a power cut.
			_ = cmd.Process.Signal(syscall.SIGTERM)
			return err
		}
		return nil
	}

	stopped, err := waitForMachine(ctx, exited, watchOptions{
		console: options.Console,
		press:   press,
		kill:    func() { _ = cmd.Process.Kill() },
		runner:  c.Runner,
		log:     log,
	})

	// Stopped on purpose — drained by the fleet, or because it outlived its
	// runner. Either way the reason is already logged and the console is not
	// evidence of anything.
	if stopped {
		keepConsole(c.StateDir, c.Runner, options.Console)
		return err
	}

	// It went by itself. Usually that is the runner finishing — the job did, on
	// an ephemeral runner — and this agent is done. But it is also what a
	// machine does when its runner cannot work at all, and that loop is
	// indistinguishable from healthy churn from the outside. So the console is
	// kept and asked which it was.
	kept := keepConsole(c.StateDir, c.Runner, options.Console)
	if problem := whyItCouldNotWork(options.Console); problem != "" {
		return fmt.Errorf("the machine powered off without the runner ever listening for jobs: %s"+
			" (its console is at %s)", problem, kept)
	}
	log.Info("the machine powered off", "runner", c.Runner)
	return ignoreCleanExit(err)
}

// watchOptions is what watching a running machine needs, injected so the
// watching can be tested without QEMU.
type watchOptions struct {
	console string
	press   func() error
	kill    func()
	runner  string
	log     *slog.Logger
	// check and linger default to consoleCheck and lingerGrace.
	check  time.Duration
	linger time.Duration
	// interval and grace are passed on to the drain.
	interval, grace time.Duration
}

// waitForMachine waits for a machine to go, and stops it when it should have
// gone and has not.
//
// It reports whether the machine had to be stopped, because that changes what
// its console means: a machine that went by itself may have gone for a bad
// reason worth reading, and one that was stopped went because it was told to.
func waitForMachine(ctx context.Context, exited <-chan error, o watchOptions) (stopped bool, err error) {
	check, quiet := o.check, o.linger
	if check == 0 {
		check = consoleCheck
	}
	if quiet == 0 {
		quiet = patience
	}
	stop := func() (bool, error) {
		return true, drain(exited, drainOptions{
			press: o.press, kill: o.kill, runner: o.runner, log: o.log,
			interval: o.interval, grace: o.grace,
		})
	}

	watching := time.NewTicker(check)
	defer watching.Stop()
	var finishedAt time.Time

	for {
		select {
		case err := <-exited:
			return false, err

		case <-watching.C:
			// The guest says when its runner is done, on the console, and its
			// own unit then powers the machine off. When that does not happen —
			// and on a real host it did not, for eighteen minutes — nothing else
			// notices: the runner has deregistered itself so GitHub cannot give
			// it work, this agent is alive so systemd will not replace it, and
			// the fleet shows a healthy runner doing nothing at all.
			if !runnerFinished(o.console) {
				continue
			}
			if finishedAt.IsZero() {
				finishedAt = time.Now()
				o.log.Info("the runner has finished; asking the machine to power off", "runner", o.runner)
				// Asked at once. Its own unit is asking for the same thing from
				// the inside, and whichever arrives first is the one that works.
				if err := o.press(); err != nil {
					o.log.Warn("could not reach the monitor", "runner", o.runner, "error", err)
				}
				continue
			}
			if time.Since(finishedAt) < quiet {
				continue
			}
			// It was asked and did not go. Now it is worth saying so, and worth
			// keeping on at it: drain presses again on a timer and gives up at
			// the end of the grace.
			o.log.Warn("the runner finished but the machine is still running; stopping it",
				"runner", o.runner, "waited", time.Since(finishedAt).Round(time.Second).String())
			return stop()

		case <-ctx.Done():
			o.log.Info("stopping: asking the machine to shut down and waiting for the job in flight",
				"runner", o.runner)
			return stop()
		}
	}
}

// consoleCheck is how often the machine's console is read while it runs.
//
// Short, because it decides how quickly a machine whose runner has finished is
// asked to go, and reading a file that QEMU is appending to costs nothing.
const consoleCheck = 3 * time.Second

// patience is how long the machine is given after being asked before anyone
// says anything about it.
//
// There is no waiting before the asking. The button press is the same request
// the guest's own unit makes when the runner stops — systemctl poweroff, from
// outside instead of inside — so pressing it while the machine is already
// shutting down changes nothing: there is no logind left to hear it, and it is
// a button, not a power cut. Nothing is cut short by asking early, so the
// earlier version's ninety-second wait bought nothing and cost ninety seconds
// of a machine that had nothing left to do.
//
// What this is for is the log. A machine takes a few seconds to stop docker
// and unmount, and saying "it has not gone" before that is finished would be
// noise on every job. The only thing that does cut a machine off — the kill at
// the end of the drain — stays behind its own long grace, because that one is
// a power cut and a job may still be running.
const patience = 20 * time.Second

// finished are the runner's last words, from the guest's console.
//
//	Runner listener exit with 0 return code, stop the service, no retry needed.
//	Exiting runner...
var finished = []string{"Runner listener exit with", "Exiting runner"}

// runnerFinished reports whether the console shows the runner has stopped for
// good, which is the point at which the machine has nothing left to do.
func runnerFinished(console string) bool {
	raw, err := os.ReadFile(console)
	if err != nil {
		return false
	}
	text := string(raw)
	for _, last := range finished {
		if strings.Contains(text, last) {
			return true
		}
	}
	return false
}

// pressInterval is how often the power button is pressed again while a machine
// refuses to go.
//
// An ACPI power button press is an edge-triggered event, and something in the
// guest has to be listening for it. A machine that is still booting has nothing
// listening yet, so the press is dropped — and a drain that arrives nine
// seconds after boot, which is what a daemon replacing a fleet does, is lost
// entirely. The machine then runs until the grace period ends, holding its
// cpus and memory, with the fleet showing it as stopping the whole time.
//
// So the button is pressed again, and again, until the machine goes. Pressing
// it during a shutdown that has already started is ignored by the guest, which
// is why this is safe to repeat.
const pressInterval = 30 * time.Second

// drainOptions is what draining a machine needs, injected so the waiting can be
// tested without QEMU.
type drainOptions struct {
	press  func() error
	kill   func()
	runner string
	log    *slog.Logger
	// interval and grace default to pressInterval and shutdownGrace.
	interval time.Duration
	grace    time.Duration
}

// drain asks a machine to stop, keeps asking, and gives up on it eventually.
func drain(exited <-chan error, o drainOptions) error {
	interval, grace := o.interval, o.grace
	if interval == 0 {
		interval = pressInterval
	}
	if grace == 0 {
		grace = shutdownGrace
	}

	if err := o.press(); err != nil {
		o.log.Warn("could not reach the monitor; the machine will be stopped the hard way",
			"runner", o.runner, "error", err)
	}

	pressing := time.NewTicker(interval)
	defer pressing.Stop()
	giveUp := time.After(grace)
	started := time.Now()

	for {
		select {
		case err := <-exited:
			return ignoreCleanExit(err)

		case <-pressing.C:
			// Said out loud, because a machine that needs asking twice is
			// either finishing a long job or was too young to hear the first
			// one, and the difference matters to whoever is watching.
			o.log.Warn("the machine has not stopped; asking it again",
				"runner", o.runner, "waiting_for", time.Since(started).Round(time.Second).String())
			if err := o.press(); err != nil {
				o.log.Warn("could not reach the monitor", "runner", o.runner, "error", err)
			}

		case <-giveUp:
			o.log.Warn("the machine did not stop in time and is being killed", "runner", o.runner)
			o.kill()
			<-exited
			return errors.New("the machine did not shut down within the grace period")
		}
	}
}

// listening is what the actions runner prints once GitHub can give it work.
const listening = "Listening for Jobs"

// refusals are the runner saying it is not going to work, in its own words.
//
// Reaching "Listening for Jobs" is not proof of health, which is the trap this
// avoided by being written against a real console: a deprecated runner connects,
// announces it is listening, and is then told it cannot receive messages. The
// last word decides, not the first.
//
//	An error occured: ...      the runner's own error line, typo included
//	exit with terminated error the listener saying it will not retry
//	registration failed        the guest script, when config.sh was refused
var refusals = []string{"An error occured", "exit with terminated error", "registration failed"}

// whyItCouldNotWork reads a dead machine's console and returns the reason its
// runner could not do the job it exists for, or "" if it could.
//
// The console is the only account of what happened inside the machine: the
// guest's journal goes with the disk. What comes back is the runner's own last
// words, which is the difference between "deprecated and cannot receive
// messages" and a guess.
func whyItCouldNotWork(console string) string {
	raw, err := os.ReadFile(console)
	if err != nil {
		// No console is not evidence of failure. Saying nothing is right: the
		// alternative is a healthy fleet reporting problems it cannot support.
		return ""
	}

	// The guest's unit tags the runner's output with the script that produced
	// it, so those lines are the runner talking and everything else is the
	// operating system booting around it.
	var said []string
	refused := false
	for _, line := range strings.Split(string(raw), "\n") {
		i := strings.Index(line, "run-runner.sh")
		if i < 0 {
			continue
		}
		_, message, found := strings.Cut(line[i:], ": ")
		message = strings.TrimSpace(message)
		if !found || message == "" {
			continue
		}
		said = append(said, message)
		for _, refusal := range refusals {
			if strings.Contains(message, refusal) {
				refused = true
			}
		}
	}

	switch {
	case len(said) == 0:
		return "its console records nothing from the runner at all"
	case refused:
	case containsAny(said, listening):
		// It listened and nothing complained: it worked, and this is an
		// ephemeral runner finishing or a machine being drained.
		return ""
	}
	if len(said) > 3 {
		said = said[len(said)-3:]
	}
	return strings.Join(said, " / ")
}

func containsAny(lines []string, want string) bool {
	for _, line := range lines {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

// keepConsole copies a machine's console somewhere that outlives it, and
// returns where.
//
// The machine's directory is deleted when this agent exits, which is exactly
// the moment its console becomes worth reading. One file per runner, replaced
// on each boot: the interesting one is always the last.
func keepConsole(stateDir, runner, console string) string {
	kept := filepath.Join(stateDir, "consoles", runner+".log")
	if err := os.MkdirAll(filepath.Dir(kept), 0o700); err != nil {
		return console
	}
	raw, err := os.ReadFile(console)
	if err != nil {
		return console
	}
	if err := os.WriteFile(kept, raw, 0o600); err != nil {
		return console
	}
	return kept
}

// ignoreCleanExit treats a machine that powered itself off as success. QEMU
// exits non-zero in some of those cases, and a unit that reported failure for
// an ordinary shutdown would trip the restart limiter.
func ignoreCleanExit(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}
	return err
}
