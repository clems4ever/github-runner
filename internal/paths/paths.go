// Package paths is the one place that decides where things live on the host,
// so the daemon, the agent and the packaging cannot disagree about it.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// Layout is the set of directories the daemon uses.
type Layout struct {
	// Etc holds configuration that must survive a reboot: the database, the
	// master key, the per-runner environment files.
	Etc string
	// State holds what can be rebuilt: golden images, VM disks, ssh keys.
	State string
	// Run holds what must not survive a reboot. It is a tmpfs on any systemd
	// host, which is why decrypted credentials go here and nowhere else: the
	// database keeps them encrypted, and the clear copy the runners need never
	// touches a disk.
	Run string
}

// Default is where a packaged install puts everything.
func Default() Layout {
	return Layout{
		Etc:   "/etc/runner-fleet",
		State: "/var/lib/runner-fleet",
		Run:   "/run/runner-fleet",
	}
}

// Under puts the whole layout inside one directory, which is what tests and a
// development run use so neither touches the host.
func Under(root string) Layout {
	return Layout{
		Etc:   filepath.Join(root, "etc"),
		State: filepath.Join(root, "state"),
		Run:   filepath.Join(root, "run"),
	}
}

// FromEnv reads RUNNER_FLEET_ROOT, so a developer can run the daemon without
// root and without colliding with a real install.
func FromEnv() Layout {
	if root := os.Getenv("RUNNER_FLEET_ROOT"); root != "" {
		return Under(root)
	}
	return Default()
}

// Database is the desired-state store.
func (l Layout) Database() string { return filepath.Join(l.Etc, "fleet.db") }

// MasterKey encrypts the credentials in the database.
func (l Layout) MasterKey() string { return filepath.Join(l.Etc, "master.key") }

// RunnersDir holds one environment file per runner, which is how a systemd
// template unit learns what it is running.
func (l Layout) RunnersDir() string { return filepath.Join(l.Etc, "runners") }

// RunnerEnv is one runner's environment file.
func (l Layout) RunnerEnv(name string) string {
	return filepath.Join(l.RunnersDir(), name+".env")
}

// CredentialsDir holds the decrypted tokens, on tmpfs.
func (l Layout) CredentialsDir() string { return filepath.Join(l.Run, "credentials") }

// Credential is the decrypted token for one credential id.
func (l Layout) Credential(id int64) string {
	return filepath.Join(l.CredentialsDir(), fmt.Sprintf("%d", id))
}

// ImagesDir holds the golden images the VMs boot from.
func (l Layout) ImagesDir() string { return filepath.Join(l.State, "images") }

// VMDir is one VM's working directory.
func (l Layout) VMDir(name string) string { return filepath.Join(l.State, "vms", name) }

// SSHKey is the key that reaches every VM this host runs.
func (l Layout) SSHKey() string { return filepath.Join(l.State, "ssh", "id_ed25519") }

// EnsureDirs creates what must exist before anything else runs, and hands the
// ones the runners use to the account they run as.
//
// Which directories those are is the part worth being explicit about. The
// daemon is root and the agents are not, so anything an agent reads or writes
// has to belong to the service user: the state directory, where a machine
// builds its images and disks, and the credentials directory, where it finds
// the token it registers with. The rest stays root's.
func (l Layout) EnsureDirs(owner Owner) error {
	// 0700 throughout: these hold credentials, configuration naming private
	// repositories, and disk images a job has written to. Ownership, not the
	// mode, is what decides who that one user is.
	for _, dir := range []string{l.Etc, l.RunnersDir(), l.State, l.ImagesDir(), l.Run, l.CredentialsDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	for _, dir := range l.AgentDirs() {
		if err := owner.Give(dir); err != nil {
			return err
		}
	}
	return nil
}

// AgentDirs are the directories an agent needs of its own. The daemon creates
// them; the runners live in them.
//
// Run is in the list as well as the credentials directory inside it. Handing
// over a directory nobody can walk into is handing over nothing, and a 0700
// root-owned parent is exactly that — which is the second half of the same bug,
// found by the test that reads the file as the other user rather than looking
// at its mode.
func (l Layout) AgentDirs() []string {
	return []string{l.State, l.ImagesDir(), l.Run, l.CredentialsDir()}
}
