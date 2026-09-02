package paths_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/clems4ever/github-runner/internal/paths"
)

// unit is the packaged daemon unit, read from the repository rather than
// described here: a test that quoted it would agree with itself and with
// nothing that ships.
func unit(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("../../packaging/runner-fleetd.service")
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// directive returns the value of a unit setting, or "" when it is absent.
func directive(t *testing.T, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `=(.*)$`)
	m := re.FindStringSubmatch(unit(t))
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// The runtime directory is systemd's to create, because the daemon cannot.
//
// /run is a tmpfs, so Layout.Run is gone after every boot. EnsureDirs creates
// it — but it never gets that far: ReadWritePaths= needs every path it names to
// exist when the mount namespace is built, and that is before ExecStart. So the
// unit failed at step NAMESPACE with status 226 and restarted for ever, naming
// a binary that was perfectly present. The whole fleet was down after one
// reboot, and the daemon had never survived one.
func TestSystemdMakesTheRuntimeDirectoryTheDaemonCannot(t *testing.T) {
	want := filepath.Base(paths.Default().Run)
	if got := directive(t, "RuntimeDirectory"); got != want {
		t.Errorf("RuntimeDirectory is %q, want %q — without it /run/%s does not "+
			"exist at boot and the unit cannot start at all", got, want, want)
	}
}

// Stopping the daemon must not take the runners' credentials with it.
//
// The unit opens by promising that restarting it is safe at any time, because
// it supervises nothing and every runner carries on through it. The decrypted
// credential an agent reads lives under the runtime directory, and systemd's
// default is to delete that directory on stop — which would break the promise
// for every job in flight.
func TestTheRuntimeDirectorySurvivesTheDaemon(t *testing.T) {
	if got := directive(t, "RuntimeDirectoryPreserve"); got != "yes" {
		t.Errorf("RuntimeDirectoryPreserve is %q, want \"yes\" — a daemon restart "+
			"would otherwise delete the credentials the running agents read", got)
	}
}

// The mode matches what the daemon would have used.
//
// EnsureDirs makes these 0700 because they hold credentials, but MkdirAll
// leaves an existing directory's mode alone — so whatever systemd creates is
// what it stays. The default is 0755.
func TestTheRuntimeDirectoryIsAsNarrowAsTheDaemonWouldMakeIt(t *testing.T) {
	if got := directive(t, "RuntimeDirectoryMode"); got != "0700" {
		t.Errorf("RuntimeDirectoryMode is %q, want \"0700\" — the daemon does not "+
			"narrow a directory that already exists", got)
	}
}

// A path named in both places is one the fix does not cover.
//
// RuntimeDirectory= already makes its directory writable. Naming it in
// ReadWritePaths= as well re-states the requirement that broke this unit — that
// the path exist before the namespace is built — for a reader who has no way to
// tell the two apart.
func TestTheRuntimeDirectoryIsNotAlsoAWritablePath(t *testing.T) {
	run := paths.Default().Run
	for _, p := range strings.Fields(directive(t, "ReadWritePaths")) {
		if p == run {
			t.Errorf("%s is named by both RuntimeDirectory= and ReadWritePaths=", run)
		}
	}
}
