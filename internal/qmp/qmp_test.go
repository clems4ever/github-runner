package qmp_test

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/clems4ever/github-runner/internal/qmp"
)

// monitor is a stand-in QEMU monitor: it greets, negotiates, and answers the
// commands this package sends. A real QEMU is not something a unit test can
// have, and the protocol — a greeting to read before anything, then one reply
// per command, with asynchronous events allowed in between — is exactly the
// part worth pinning.
type monitor struct {
	t        *testing.T
	status   string
	events   bool // emit an unsolicited event before the reply
	mu       sync.Mutex
	executed []string
}

func (m *monitor) commands() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.executed...)
}

// listen starts the monitor on a socket in a temp dir and returns its path.
//
// A short directory, because a unix socket path is capped near 100 bytes and a
// t.TempDir() under a long test name has overflowed it before.
func (m *monitor) listen() string {
	m.t.Helper()
	dir, err := os.MkdirTemp("", "qmp")
	if err != nil {
		m.t.Fatal(err)
	}
	m.t.Cleanup(func() { _ = os.RemoveAll(dir) })

	socket := filepath.Join(dir, "q.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		m.t.Fatal(err)
	}
	m.t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go m.serve(conn)
		}
	}()
	return socket
}

func (m *monitor) serve(conn net.Conn) {
	defer conn.Close()
	// QEMU speaks first.
	_, _ = conn.Write([]byte(`{"QMP":{"version":{},"capabilities":[]}}` + "\n"))

	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('}')
		if err != nil {
			return
		}
		m.mu.Lock()
		m.executed = append(m.executed, line)
		m.mu.Unlock()

		if m.events {
			// An asynchronous event, arriving where a naive reader would take
			// it for the reply it is waiting on.
			_, _ = conn.Write([]byte(`{"event":"RESUME","timestamp":{"seconds":1,"microseconds":0}}` + "\n"))
		}
		switch {
		case strings.Contains(line, "query-status"):
			reply, _ := json.Marshal(map[string]any{
				"return": map[string]any{"status": m.status, "running": m.status == "running"},
			})
			_, _ = conn.Write(append(reply, '\n'))
		default:
			_, _ = conn.Write([]byte(`{"return":{}}` + "\n"))
		}
	}
}

func TestStatusReportsWhatTheMachineIsDoing(t *testing.T) {
	for _, want := range []string{"running", "io-error", "paused"} {
		m := &monitor{t: t, status: want}
		got, err := qmp.Status(m.listen())
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if got != want {
			t.Errorf("status = %q, want %q", got, want)
		}
	}
}

// The state the fleet exists to notice. A machine QEMU stopped on a write error
// has a live process and an active unit; only the monitor knows.
func TestStatusSeesAMachineStoppedOnAWriteError(t *testing.T) {
	m := &monitor{t: t, status: qmp.StatusIOError}

	got, err := qmp.Status(m.listen())
	if err != nil {
		t.Fatal(err)
	}
	if got != qmp.StatusIOError || got == qmp.StatusRunning {
		t.Fatalf("status = %q, want %q", got, qmp.StatusIOError)
	}
}

// Events arrive whenever QEMU has something to say, including between a command
// and its reply. Treating the next message as the reply reads an event as a
// result and then answers every later command with the previous one's.
func TestAnEventBeforeTheReplyIsNotMistakenForIt(t *testing.T) {
	m := &monitor{t: t, status: qmp.StatusIOError, events: true}

	got, err := qmp.Status(m.listen())
	if err != nil {
		t.Fatalf("status with an event in the way: %v", err)
	}
	if got != qmp.StatusIOError {
		t.Fatalf("status = %q, want %q", got, qmp.StatusIOError)
	}
}

// Capabilities are not optional: QEMU refuses every command until they are
// negotiated, so a connection that skipped them would fail against a real
// monitor and pass against a lenient fake.
func TestEveryCommandNegotiatesCapabilitiesFirst(t *testing.T) {
	m := &monitor{t: t, status: "running"}
	socket := m.listen()

	if err := qmp.Cont(socket); err != nil {
		t.Fatal(err)
	}
	sent := m.commands()
	if len(sent) < 2 {
		t.Fatalf("sent %d commands, want capabilities then the command: %v", len(sent), sent)
	}
	if !strings.Contains(sent[0], "qmp_capabilities") {
		t.Errorf("first command = %q, want qmp_capabilities", sent[0])
	}
	if !strings.Contains(sent[1], "cont") {
		t.Errorf("second command = %q, want cont", sent[1])
	}
}

func TestPowerDownPressesThePowerButton(t *testing.T) {
	m := &monitor{t: t, status: "running"}

	if err := qmp.PowerDown(m.listen()); err != nil {
		t.Fatal(err)
	}
	sent := m.commands()
	if !strings.Contains(sent[len(sent)-1], "system_powerdown") {
		t.Errorf("last command = %q, want system_powerdown", sent[len(sent)-1])
	}
}

// A machine that is gone, or one whose monitor never answers, must be an error
// and never an empty state that a caller could read as "fine".
func TestAMonitorThatIsNotThereIsAnError(t *testing.T) {
	status, err := qmp.Status(filepath.Join(t.TempDir(), "absent.sock"))
	if err == nil {
		t.Fatal("status of a machine with no monitor reported success")
	}
	if status != "" {
		t.Errorf("status = %q, want empty beside the error", status)
	}
}
