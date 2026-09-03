package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// runnerHomes are where the actions runner is found in the images people use.
//
// The official image (ghcr.io/actions/actions-runner) puts it straight in the
// home directory; others put it in a subdirectory. Looking for config.sh
// rather than assuming a path is what makes a custom image work without
// anything else changing — which is where per-repository images are going.
var runnerHomes = []string{
	"/home/runner",
	"/home/runner/actions-runner",
	"/actions-runner",
	"/runner",
}

// findRunnerHome returns the directory holding the runner, or an error naming
// everywhere it looked.
func findRunnerHome() (string, error) {
	if override := os.Getenv("FLEET_RUNNER_HOME"); override != "" {
		return override, nil
	}
	for _, candidate := range runnerHomes {
		if _, err := os.Stat(filepath.Join(candidate, "config.sh")); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no GitHub Actions runner in this image: looked for config.sh in %s. "+
		"Set FLEET_RUNNER_HOME if it is somewhere else",
		strings.Join(runnerHomes, ", "))
}

// runContainer is the agent inside a container: it registers the runner that
// the image already carries, then runs it.
//
// The same shape as the VM path — register, run, forward the stop — but with
// no machine in between. Everything a job does happens in this container,
// which is a weaker boundary than a VM and is why the runtime is a per-pool
// choice rather than a default.
func runContainer(ctx context.Context, c Config, log *slog.Logger) error {
	runnerHome, err := findRunnerHome()
	if err != nil {
		return err
	}

	reg, err := register(ctx, c)
	if err != nil {
		return err
	}

	// A just-in-time configuration is everything config.sh would have written,
	// minted by the daemon before this container was created. There is nothing
	// to register: unpack it and run.
	runArgs := []string{}
	if reg.JIT != "" {
		log.Info("starting from a just-in-time configuration",
			"runner", c.Runner, "url", c.URL, "labels", strings.Join(c.Labels, ","))
		runArgs = append(runArgs, "--jitconfig", reg.JIT)
	} else {
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
			"--token", reg.Token,
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
	}

	runner := exec.Command("./run.sh", runArgs...)
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
