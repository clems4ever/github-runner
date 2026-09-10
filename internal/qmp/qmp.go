// Package qmp speaks QEMU's machine protocol over a machine's monitor socket.
//
// Two callers need it and they sit on opposite sides of the fleet: the agent,
// which presses a machine's power button when it is asked to drain, and the
// daemon, which asks a machine whether it is actually running. Neither may
// import the other, and the handshake is fiddly enough — a greeting to read,
// capabilities to negotiate, then one command per reply — that a second
// implementation of it would be a second thing to get subtly wrong.
package qmp

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// dialTimeout and deadline bound everything here, because the whole point of
// asking is that the machine may be in trouble. A monitor that does not answer
// must not become a reconcile loop that does not return.
const (
	dialTimeout = 5 * time.Second
	deadline    = 10 * time.Second
)

// conn is one negotiated monitor session.
type conn struct {
	net     net.Conn
	decoder *json.Decoder
}

// dial opens the monitor and gets past its greeting.
//
// QMP announces itself and refuses every command until capabilities are
// negotiated, so there is no such thing as a connection that is ready to use
// the moment it is open.
func dial(socket string) (*conn, error) {
	netConn, err := net.DialTimeout("unix", socket, dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("reach the machine's monitor: %w", err)
	}
	_ = netConn.SetDeadline(time.Now().Add(deadline))

	c := &conn{net: netConn, decoder: json.NewDecoder(netConn)}
	var greeting map[string]any
	if err := c.decoder.Decode(&greeting); err != nil {
		c.net.Close()
		return nil, fmt.Errorf("read the monitor greeting: %w", err)
	}
	if _, err := c.execute(`{"execute":"qmp_capabilities"}`); err != nil {
		c.net.Close()
		return nil, err
	}
	return c, nil
}

func (c *conn) close() { _ = c.net.Close() }

// execute sends one command and returns its "return" value.
func (c *conn) execute(command string) (json.RawMessage, error) {
	if _, err := c.net.Write([]byte(command)); err != nil {
		return nil, err
	}
	// Events are asynchronous and can arrive between a command and its reply,
	// so the reply is the next message carrying "return" or "error" — not
	// simply the next message.
	for {
		var reply struct {
			Return json.RawMessage `json:"return"`
			Error  json.RawMessage `json:"error"`
			Event  string          `json:"event"`
		}
		if err := c.decoder.Decode(&reply); err != nil {
			return nil, err
		}
		switch {
		case reply.Error != nil:
			return nil, fmt.Errorf("monitor refused %s: %s", command, reply.Error)
		case reply.Event != "":
			continue
		default:
			return reply.Return, nil
		}
	}
}

// PowerDown presses the machine's ACPI power button.
//
// This is not a power cut: inside the machine systemd stops the runner unit,
// and that unit waits for the job in flight. It is the mechanism behind every
// drain in the fleet.
func PowerDown(socket string) error {
	c, err := dial(socket)
	if err != nil {
		return err
	}
	defer c.close()
	_, err = c.execute(`{"execute":"system_powerdown"}`)
	return err
}

// Status is QEMU's own run state for a machine: "running", "paused",
// "io-error", "shutdown", and the rest of the set in QEMU's RunState enum.
//
// It is not the same question as "is the QEMU process alive", and the
// difference is the whole reason this exists. A machine QEMU has stopped still
// has a process, still has a systemd unit that is perfectly active, and does
// not execute a single instruction.
func Status(socket string) (string, error) {
	c, err := dial(socket)
	if err != nil {
		return "", err
	}
	defer c.close()

	raw, err := c.execute(`{"execute":"query-status"}`)
	if err != nil {
		return "", err
	}
	var status struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return "", fmt.Errorf("read the machine's run state: %w", err)
	}
	if status.Status == "" {
		return "", fmt.Errorf("the monitor reported no run state")
	}
	return status.Status, nil
}

// Cont resumes a machine QEMU stopped.
//
// The one it is for is StatusIOError: QEMU's default write-error policy stops
// a machine when the host has no space left rather than passing the error into
// the guest, which keeps the guest's filesystem intact and leaves the machine
// frozen until somebody says to carry on.
func Cont(socket string) error {
	c, err := dial(socket)
	if err != nil {
		return err
	}
	defer c.close()
	_, err = c.execute(`{"execute":"cont"}`)
	return err
}

// StatusRunning and StatusIOError are the two run states the fleet reasons
// about by name.
const (
	StatusRunning = "running"
	StatusIOError = "io-error"
)
