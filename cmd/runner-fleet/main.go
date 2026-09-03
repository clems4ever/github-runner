// Command runner-fleet is the whole product: the daemon that maintains a fleet
// of GitHub Actions runners, and the agent that each runner is.
//
// One binary in three modes. The agent is the same executable the daemon
// installs into its units, so an upgrade replaces both at once — and because
// running processes keep the file they started from, the runners already going
// carry on with the version they were started with until they are next
// replaced.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/clems4ever/github-runner/internal/agent"
	"github.com/clems4ever/github-runner/internal/paths"
	"github.com/clems4ever/github-runner/internal/qmp"
	"github.com/clems4ever/github-runner/internal/secrets"
	"github.com/clems4ever/github-runner/internal/store"
)

// version is stamped in at build time by goreleaser.
var version = "dev"

func main() {
	err := run(os.Args[1:])
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "runner-fleet: "+err.Error())
	// A runner whose image is not built is not a runner that failed at
	// something it could retry. It exits with a code of its own so that its
	// unit knows not to start it again — see agent.ExitImageNotBuilt.
	if errors.Is(err, agent.ErrImageNotBuilt) {
		os.Exit(agent.ExitImageNotBuilt)
	}
	os.Exit(1)
}

func run(args []string) error {
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}

	switch command {
	case "serve", "daemon", "fleetd":
		return serveCommand(args)
	case "agent":
		return agentCommand(args)
	case "passwd":
		return passwdCommand(args)
	case "machine":
		return machineCommand(args)
	case "version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func usage() {
	fmt.Print(`runner-fleet — self-hosted GitHub Actions runners, managed from one daemon

Usage:
  runner-fleet serve [flags]     run the daemon and its web UI (the default)
  runner-fleet agent --name NAME be one runner; this is what a unit or a
                                 container runs, not something to run by hand
  runner-fleet passwd [flags]    set the web UI's user and password
  runner-fleet machine status NAME   what QEMU says one machine is doing
  runner-fleet machine resume NAME   carry on a machine QEMU stopped
  runner-fleet version

Flags for serve:
  --addr HOST:PORT   where to listen (default 127.0.0.1:8080). It holds a
                     credential that administers repositories, so it binds to
                     the loopback address unless told otherwise
  --root DIR         put everything under DIR instead of /etc, /var and /run,
                     which is how to run it without root while developing
  --interval D       how often to reconcile the fleet (default 30s)
  --user NAME        the service user the runners' units run as

Flags for passwd:
  --user NAME        the user name (default admin)
  --password VALUE   the password; read from stdin when not given, so it does
                     not reach the process list or the shell history
  --root DIR         as above

Flags for machine:
  --root DIR         as above

Environment:
  RUNNER_FLEET_ROOT  same as --root
`)
}

func logger() *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("RUNNER_FLEET_DEBUG") != "" {
		level = slog.LevelDebug
	}
	// Text rather than JSON: this goes to the journal, where a person reads it.
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func agentCommand(args []string) error {
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	name := flags.String("name", "", "the runner's name")
	if err := flags.Parse(args); err != nil {
		return err
	}

	config, err := agent.ConfigFromEnv(*name)
	if err != nil {
		return err
	}
	return agent.Run(context.Background(), config, logger())
}

func passwdCommand(args []string) error {
	flags := flag.NewFlagSet("passwd", flag.ContinueOnError)
	user := flags.String("user", "admin", "the user name")
	password := flags.String("password", "", "the password; read from stdin when empty")
	root := flags.String("root", "", "put everything under this directory")
	if err := flags.Parse(args); err != nil {
		return err
	}

	layout := layoutFor(*root)
	// Setting a password creates nothing the runners read, so it hands nothing
	// over: the daemon does that when it starts.
	if err := layout.EnsureDirs(paths.CurrentOwner()); err != nil {
		return err
	}
	ring, err := secrets.LoadOrCreateKey(layout.MasterKey())
	if err != nil {
		return err
	}
	db, err := store.Open(layout.Database(), ring)
	if err != nil {
		return err
	}
	defer db.Close()

	value := *password
	if value == "" {
		// From stdin, so it is not in the process list for every user on the
		// machine, nor in the shell history.
		read, err := readSecretLine()
		if err != nil {
			return err
		}
		value = read
	}

	auth := newAuthenticator(db)
	if err := auth.SetPassword(context.Background(), *user, value); err != nil {
		return err
	}
	fmt.Printf("the web UI now accepts %s\n", *user)
	return nil
}

func layoutFor(root string) paths.Layout {
	if root != "" {
		return paths.Under(root)
	}
	return paths.FromEnv()
}

func readSecretLine() (string, error) {
	var line string
	if _, err := fmt.Fscanln(os.Stdin, &line); err != nil && !errors.Is(err, os.ErrClosed) {
		if line == "" {
			return "", fmt.Errorf("no password on stdin: pass --password, or pipe one in")
		}
	}
	return strings.TrimSpace(line), nil
}

// machineCommand asks QEMU about one machine, or tells it to carry on.
//
// It exists because of one state: QEMU stops a machine when a write fails for
// want of space on the host, rather than passing the error into the guest, and
// nothing resumes it afterwards. The fleet reports such a machine (see the
// systemd executor's machineTrouble) and this is the other half — the thing the
// report tells somebody to run.
//
// Deliberately not automatic. Resuming a machine while the host is still full
// stops it again on its next write, and a loop that keeps doing that turns one
// legible fault into a machine that flaps. Somebody makes space, then says to
// carry on.
func machineCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("machine: say what to do: status NAME, or resume NAME")
	}
	action, rest := args[0], args[1:]

	flags := flag.NewFlagSet("machine "+action, flag.ContinueOnError)
	root := flags.String("root", os.Getenv("RUNNER_FLEET_ROOT"), "put everything under this directory")
	if err := flags.Parse(rest); err != nil {
		return err
	}
	name := flags.Arg(0)
	if name == "" {
		return fmt.Errorf("machine %s: name a machine", action)
	}
	socket := layoutFor(*root).QMPSocket(name)

	switch action {
	case "status":
		status, err := qmp.Status(socket)
		if err != nil {
			return fmt.Errorf("machine status %s: %w", name, err)
		}
		fmt.Println(status)
		return nil
	case "resume":
		if err := qmp.Cont(socket); err != nil {
			return fmt.Errorf("machine resume %s: %w", name, err)
		}
		status, err := qmp.Status(socket)
		if err != nil {
			// It was told to carry on and that succeeded; not being able to
			// read the state back afterwards is worth saying and is not a
			// failure of the resume.
			fmt.Printf("%s: resumed\n", name)
			return nil
		}
		fmt.Printf("%s: %s\n", name, status)
		return nil
	default:
		return fmt.Errorf("machine: unknown action %q; say status or resume", action)
	}
}
