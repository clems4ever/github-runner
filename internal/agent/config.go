// Package agent is what a runner actually is: one process, started by systemd
// or as a container's entrypoint, that registers a GitHub runner and keeps it
// alive until it is told to stop.
//
// It is deliberately independent of the daemon. Everything it needs arrives in
// its environment and in one credential file, so a runner survives the daemon
// being stopped, upgraded or crashed — it can even come back after a reboot
// with the daemon still down.
package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/clems4ever/github-runner/internal/model"
)

// Config is what the daemon told this runner to be. The names match the
// environment file the systemd executor writes and the variables the Docker
// executor sets, and nothing else reads them.
type Config struct {
	Runner         string
	Pool           string
	Generation     string
	URL            string
	ScopeKind      model.ScopeKind
	Scope          string
	Labels         []string
	Group          string
	Ephemeral      bool
	Nested         bool
	Runtime        model.Runtime
	CPUs           int
	MemoryMB       int
	DiskGB         int
	Image          string
	CredentialFile string
	StateDir       string
	// How to authenticate. A GitHub App's agent signs its own assertion with
	// the key beside it and buys an installation token, so a runner can come
	// back after a reboot with the daemon still starting.
	CredentialKind model.CredentialKind
	AppID          int64
	InstallationID int64
}

// ConfigFromEnv reads the runner's configuration out of its environment.
func ConfigFromEnv(name string) (Config, error) {
	c := Config{
		Runner:         first(name, os.Getenv("FLEET_RUNNER")),
		Pool:           os.Getenv("FLEET_POOL"),
		Generation:     os.Getenv("FLEET_GENERATION"),
		URL:            os.Getenv("FLEET_URL"),
		ScopeKind:      model.ScopeKind(first(os.Getenv("FLEET_SCOPE_KIND"), string(model.ScopeRepository))),
		Scope:          os.Getenv("FLEET_SCOPE"),
		Group:          first(os.Getenv("FLEET_GROUP"), "Default"),
		Ephemeral:      boolEnv("FLEET_EPHEMERAL"),
		Nested:         boolEnv("FLEET_NESTED"),
		Runtime:        model.Runtime(first(os.Getenv("FLEET_RUNTIME"), string(model.RuntimeVM))),
		CPUs:           intEnv("FLEET_CPUS", 2),
		MemoryMB:       intEnv("FLEET_MEMORY_MB", 4096),
		DiskGB:         intEnv("FLEET_DISK_GB", 40),
		Image:          first(os.Getenv("FLEET_IMAGE"), "default"),
		CredentialFile: os.Getenv("FLEET_CREDENTIAL_FILE"),
		StateDir:       first(os.Getenv("FLEET_STATE_DIR"), "/var/lib/runner-fleet"),
		CredentialKind: model.CredentialKind(first(os.Getenv("FLEET_CREDENTIAL_KIND"), string(model.CredentialPAT))),
		AppID:          int64(intEnv("FLEET_APP_ID", 0)),
		InstallationID: int64(intEnv("FLEET_INSTALLATION_ID", 0)),
	}
	if labels := os.Getenv("FLEET_LABELS"); labels != "" {
		c.Labels = strings.Split(labels, ",")
	}

	if c.Runner == "" {
		return c, fmt.Errorf("no runner name: pass --name, or set FLEET_RUNNER")
	}
	if c.URL == "" || c.Scope == "" {
		return c, fmt.Errorf("%s: no repository or organisation to register on", c.Runner)
	}
	if c.CredentialFile == "" {
		return c, fmt.Errorf("%s: no credential file. A runner registers afresh on every start, so it needs a credential that can mint registration tokens", c.Runner)
	}
	if c.CredentialKind == model.CredentialApp && c.AppID == 0 {
		return c, fmt.Errorf("%s: an app credential without an app id", c.Runner)
	}
	return c, nil
}

// RegistrationToken is a token the daemon minted for this runner, used instead
// of a credential of its own.
//
// This is how a container runner registers. A container shares everything with
// the job it runs, so handing it the credential that mints tokens would hand
// the job a key that administers repositories; a registration token is
// short-lived and can do one thing. A virtual machine has no such problem —
// the job is inside the guest and the credential never is — so a VM keeps the
// credential and mints for itself, which is what lets it come back after a
// reboot with the daemon still down.
func (c Config) RegistrationToken() string {
	token := os.Getenv("FLEET_REGISTRATION_TOKEN")
	if token != "" {
		// Taken out of the environment as it is read: everything this process
		// starts inherits that environment, and the job is one of them.
		_ = os.Unsetenv("FLEET_REGISTRATION_TOKEN")
	}
	return token
}

// Token reads the credential the daemon left for this runner.
//
// It is read on every registration rather than kept, so a rotated token is
// picked up by the next boot without the runner having to be rebuilt.
func (c Config) Token() (string, error) {
	raw, err := os.ReadFile(c.CredentialFile)
	if err != nil {
		return "", fmt.Errorf("read the credential: %w (the daemon writes this on tmpfs; is runner-fleetd running?)", err)
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", fmt.Errorf("the credential file %s is empty", c.CredentialFile)
	}
	return token, nil
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func boolEnv(key string) bool {
	value, _ := strconv.ParseBool(os.Getenv(key))
	return value
}

func intEnv(key string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(key))
	if err != nil || value < 0 {
		return fallback
	}
	if value == 0 {
		return fallback
	}
	return value
}
