package paths

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
)

// Owner is the account the runners' processes run as.
//
// The daemon runs as root — it writes unit files and drives systemctl — and the
// runners deliberately do not: a machine agent runs as an unprivileged service
// user. So every file the daemon leaves for an agent to read has to be handed
// over, and a file written 0600 by root is not.
//
// That is not a hypothetical. The credential a machine mints its registration
// token from was written root-owned on tmpfs and read by the service user,
// which meant every VM runner on a packaged install died on startup with
// "permission denied" and restarted for ever. Containers were unaffected, being
// handed a token instead, so the fleet looked half-healthy for a week.
type Owner struct {
	UID int
	GID int
	// Name is what was asked for, kept for the message when it cannot be found.
	Name string
	// Known says whether a real account was resolved. When it is false nothing
	// is chowned, which is the right answer for a developer running the daemon
	// and the agents as themselves.
	Known bool
}

// LookupOwner resolves the service user by name.
//
// A missing account is not an error. The daemon still serves, container pools
// still work, and machine pools will say what is wrong when they try to read a
// credential they cannot.
func LookupOwner(name string) (Owner, error) {
	owner := Owner{Name: name}
	if name == "" {
		return owner, nil
	}
	account, err := user.Lookup(name)
	if err != nil {
		return owner, fmt.Errorf("look up the service user %q: %w", name, err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return owner, fmt.Errorf("the service user %q has a uid that is not a number: %q", name, account.Uid)
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return owner, fmt.Errorf("the service user %q has a gid that is not a number: %q", name, account.Gid)
	}
	return Owner{UID: uid, GID: gid, Name: name, Known: true}, nil
}

// CurrentOwner is the account this process runs as. It is what a development
// run uses: the daemon and the agents are the same person, and nothing needs
// handing over.
func CurrentOwner() Owner {
	return Owner{UID: os.Getuid(), GID: os.Getgid(), Name: "", Known: false}
}

// Give hands a path to the service user, if there is one to hand it to and this
// process is allowed to.
//
// Chowning to yourself is a no-op the kernel allows, and chowning to somebody
// else is refused unless this process is root — which is exactly the case where
// there is nothing to hand over.
func (o Owner) Give(path string) error {
	if !o.Known || (o.UID == os.Getuid() && o.GID == os.Getgid()) {
		return nil
	}
	if err := os.Chown(path, o.UID, o.GID); err != nil {
		return fmt.Errorf("hand %s to %s: %w", path, o.Name, err)
	}
	return nil
}
