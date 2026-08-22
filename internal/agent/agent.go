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
	dir := filepath.Join(c.StateDir, "vms", c.Runner)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// A machine is rebuilt from scratch every start. Whatever the last job
	// left behind goes with it, which is the entire point of the runtime.
	defer os.RemoveAll(dir)

	publicKey, err := ensureSSHKey(filepath.Join(c.StateDir, "ssh", "id_ed25519"))
	if err != nil {
		return err
	}

	spec := ImageSpec{Variant: c.Image}
	golden, err := EnsureImage(ctx, spec, filepath.Join(c.StateDir, "images"), publicKey, log)
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

	select {
	case err := <-exited:
		// The machine powered itself off. Usually that is the runner finishing
		// — the job did, on an ephemeral runner — and this agent is done.
		//
		// But it is also what a machine does when its runner cannot work at
		// all: the runner exits, the guest's unit powers the machine off, and
		// systemd here starts another one. That loop is indistinguishable from
		// healthy churn from the outside, and it ran for hours on a real host
		// while the fleet showed a runner in perfect health. So the console is
		// kept and asked whether the runner ever got as far as being able to
		// accept a job.
		kept := keepConsole(c.StateDir, c.Runner, options.Console)
		if problem := whyItCouldNotWork(options.Console); problem != "" {
			return fmt.Errorf("the machine powered off without the runner ever listening for jobs: %s"+
				" (its console is at %s)", problem, kept)
		}
		log.Info("the machine powered off", "runner", c.Runner)
		return ignoreCleanExit(err)

	case <-ctx.Done():
		log.Info("stopping: asking the machine to shut down and waiting for the job in flight", "runner", c.Runner)
		if err := powerDown(options.QMPSocket); err != nil {
			log.Warn("could not reach the monitor; the machine will be stopped the hard way", "error", err)
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		select {
		case err := <-exited:
			return ignoreCleanExit(err)
		case <-time.After(shutdownGrace):
			log.Warn("the machine did not stop in time and is being killed", "runner", c.Runner)
			_ = cmd.Process.Kill()
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
