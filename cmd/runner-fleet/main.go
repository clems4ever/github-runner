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
