package systemd

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/paths"
	"github.com/clems4ever/github-runner/internal/qmp"
	"github.com/clems4ever/github-runner/internal/reconcile"
)

// fakeMonitor answers on a machine's QMP socket with one fixed run state.
//
// The layout root is a short temp dir rather than t.TempDir(): a unix socket
// path is capped near 108 bytes, and <tmpdir>/state/vms/<runner>/qmp.sock under
// a directory named after the test overruns it.
func fakeMonitor(t *testing.T, runner, status string) (*Executor, paths.Layout) {
	t.Helper()
	root, err := os.MkdirTemp("", "fleet")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	layout := paths.Under(root)
	if err := layout.EnsureDirs(paths.CurrentOwner()); err != nil {
		t.Fatal(err)
	}
	e := New(layout, "/usr/local/bin/runner-fleet", "runner-fleet",
		WithCommander(&fakeCommander{output: map[string]string{}}),
		WithUnitPath(layout.Etc+"/gh-runner@.service"))
	if status == "" {
		return e, layout // a machine with no monitor at all
	}

	if err := os.MkdirAll(layout.VMDir(runner), 0o755); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", layout.QMPSocket(runner))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = conn.Write([]byte(`{"QMP":{"version":{},"capabilities":[]}}` + "\n"))
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('}')
					if err != nil {
						return
					}
					if strings.Contains(line, "query-status") {
						_, _ = conn.Write([]byte(`{"return":{"status":"` + status + `"}}` + "\n"))
						continue
					}
					_, _ = conn.Write([]byte(`{"return":{}}` + "\n"))
				}
			}()
		}
	}()
	return e, layout
}

func vmRunner(name string) reconcile.Runner {
	return reconcile.Runner{Name: name, Runtime: model.RuntimeVM, State: reconcile.StateRunning}
}

// The fault this exists for. systemd reports the unit as perfectly active,
// because the QEMU process is alive and doing nothing at all.
func TestAMachineStoppedOnAWriteErrorIsReported(t *testing.T) {
	e, _ := fakeMonitor(t, "vm-1", qmp.StatusIOError)

	trouble := e.machineTrouble(vmRunner("vm-1"))

	if trouble == "" {
		t.Fatal("a machine QEMU stopped reported no trouble at all")
	}
	for _, want := range []string{"write error", "will not resume on its own", "machine resume vm-1"} {
		if !strings.Contains(trouble, want) {
			t.Errorf("trouble does not mention %q: %s", want, trouble)
		}
	}
}

func TestARunningMachineIsNotTrouble(t *testing.T) {
	e, _ := fakeMonitor(t, "vm-1", qmp.StatusRunning)

	if trouble := e.machineTrouble(vmRunner("vm-1")); trouble != "" {
		t.Fatalf("a running machine reported trouble: %s", trouble)
	}
}

// Any other stopped state is worth saying too, and is not guessed at: the run
// state QEMU gave is quoted rather than translated into a story about why.
func TestAnyOtherStoppedStateIsNamed(t *testing.T) {
	e, _ := fakeMonitor(t, "vm-1", "guest-panicked")

	trouble := e.machineTrouble(vmRunner("vm-1"))

	if !strings.Contains(trouble, "guest-panicked") {
		t.Errorf("trouble does not carry the run state QEMU reported: %s", trouble)
	}
}

// Best effort, and this is the case that decides it. A machine still booting,
// one shutting down, and one this host has never had all answer the same way:
// no monitor. Reporting that as trouble would put a warning on every runner
// during the minute it takes to come up.
func TestNoMonitorIsNotTrouble(t *testing.T) {
	e, _ := fakeMonitor(t, "vm-1", "")

	if trouble := e.machineTrouble(vmRunner("vm-1")); trouble != "" {
		t.Fatalf("a machine with no monitor reported trouble: %s", trouble)
	}
}

// A container has no QEMU, so it must not be asked. Without the guard the
// socket path would be a VM directory that never existed, which answers "no
// monitor" by accident rather than by decision — and would start costing a
// dial per container per reconcile the day that changed.
func TestAContainerIsNeverAskedAboutItsMachine(t *testing.T) {
	e, layout := fakeMonitor(t, "container-1", qmp.StatusIOError)

	runner := reconcile.Runner{
		Name: "container-1", Runtime: model.RuntimeContainer, State: reconcile.StateRunning,
	}
	if trouble := e.machineTrouble(runner); trouble != "" {
		t.Fatalf("a container was asked what QEMU thinks: %s", trouble)
	}
	// The socket really is there, so the empty answer above is the guard and
	// not a missing file.
	if _, err := os.Stat(layout.QMPSocket("container-1")); err != nil {
		t.Fatalf("the test did not create the socket it is proving is ignored: %v", err)
	}
}

// A stopped runner has nothing to ask and no monitor to ask it on.
func TestAStoppedRunnerIsNotAsked(t *testing.T) {
	e, _ := fakeMonitor(t, "vm-1", qmp.StatusIOError)

	runner := vmRunner("vm-1")
	runner.State = reconcile.StateStopped
	if trouble := e.machineTrouble(runner); trouble != "" {
		t.Fatalf("a stopped runner reported machine trouble: %s", trouble)
	}
}

// The socket path is the agent's, not one this test invented: a helper that
// agreed with the executor and disagreed with the agent would pass here and
// find nothing in production.
func TestTheSocketIsWhereTheAgentPutsIt(t *testing.T) {
	layout := paths.Under("/tmp/x")
	if got, want := layout.QMPSocket("vm-1"), filepath.Join(layout.VMDir("vm-1"), "qmp.sock"); got != want {
		t.Fatalf("QMPSocket = %q, want %q", got, want)
	}
}
