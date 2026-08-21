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
	token, err := c.Token()
	if err != nil {
		return "", err
	}
	client := github.New(token)
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
		// The machine powered itself off: the runner finished, or the job did
		// on an ephemeral runner. Either way this agent is done, and systemd
		// will start a fresh one.
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
