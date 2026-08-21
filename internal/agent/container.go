package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// runnerHome is where the actions runner lives in the runner image.
const runnerHome = "/home/runner/actions-runner"

// runContainer is the agent inside a container: it registers the runner that
// the image already carries, then runs it.
//
// The same shape as the VM path — register, run, forward the stop — but with
// no machine in between. Everything a job does happens in this container,
// which is a weaker boundary than a VM and is why the runtime is a per-pool
// choice rather than a default.
func runContainer(ctx context.Context, c Config, log *slog.Logger) error {
	if _, err := os.Stat(runnerHome); err != nil {
		return fmt.Errorf("no runner found at %s: the image must carry the GitHub Actions runner: %w", runnerHome, err)
	}

	token, err := registrationToken(ctx, c)
	if err != nil {
		return err
	}

	args := []string{
		"--url", c.URL,
		"--name", c.Runner,
		"--runnergroup", c.Group,
		"--work", "/home/runner/_work",
		"--unattended",
		"--disableupdate",
		// This runner's name is its identity in the fleet and is reused on
		// purpose, so it takes over the entry a previous container left.
		"--replace",
		"--token", token,
	}
	if len(c.Labels) > 0 {
		args = append(args, "--labels", strings.Join(c.Labels, ","))
	}
	if c.Ephemeral {
		args = append(args, "--ephemeral")
	}

	log.Info("registering", "runner", c.Runner, "url", c.URL, "labels", strings.Join(c.Labels, ","))
	config := exec.CommandContext(ctx, "./config.sh", args...)
	config.Dir = runnerHome
	config.Stdout, config.Stderr = os.Stdout, os.Stderr
	if err := config.Run(); err != nil {
		return fmt.Errorf("registration failed: %w", err)
	}

	runner := exec.Command("./run.sh")
	runner.Dir = runnerHome
	runner.Stdout, runner.Stderr = os.Stdout, os.Stderr
	if err := runner.Start(); err != nil {
		return err
	}

	exited := make(chan error, 1)
	go func() { exited <- runner.Wait() }()

	select {
	case err := <-exited:
		return ignoreCleanExit(err)
	case <-ctx.Done():
		// The runner treats SIGTERM as "finish this job, then stop". Passing
		// it on is the whole of draining a container runner.
		log.Info("stopping: letting the runner finish the job in flight", "runner", c.Runner)
		_ = runner.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-exited:
			return ignoreCleanExit(err)
		case <-time.After(shutdownGrace):
			_ = runner.Process.Kill()
			<-exited
			return fmt.Errorf("the runner did not stop within the grace period")
		}
	}
}
