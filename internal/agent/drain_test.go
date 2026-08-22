package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// The bug this exists for, from a real host:
//
//	16:10:18  booting runner=runyard-default-1
//	16:10:27  stopping: asking the machine to shut down …
//
// The drain landed nine seconds into the boot. Nothing in the guest was
// listening for the power button yet, the press was dropped, and the machine
// ran for another forty-eight minutes holding four cpus while the fleet showed
// it as stopping.
func TestAMachineThatMissedTheFirstPressIsAskedAgain(t *testing.T) {
	exited := make(chan error, 1)
	var presses atomic.Int32

	go func() {
		// It hears the third one, as a guest does once it has finished booting.
		for presses.Load() < 3 {
			time.Sleep(time.Millisecond)
		}
		exited <- nil
	}()

	var killed atomic.Bool
	err := drain(exited, drainOptions{
		press:    func() error { presses.Add(1); return nil },
		kill:     func() { killed.Store(true) },
		runner:   "web-1",
		log:      discardLog(),
		interval: 5 * time.Millisecond,
		grace:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if presses.Load() < 2 {
		t.Fatalf("the button was pressed %d times: a machine that missed the first press is never asked again", presses.Load())
	}
	if killed.Load() {
		t.Error("a machine that shut down was killed anyway")
	}
}

// One press is enough for a machine that is listening, and it must not be
// nagged while it finishes a job.
func TestAMachineThatGoesAtOnceIsAskedOnce(t *testing.T) {
	exited := make(chan error, 1)
	exited <- nil

	var presses atomic.Int32
	if err := drain(exited, drainOptions{
		press:    func() error { presses.Add(1); return nil },
		kill:     func() {},
		runner:   "web-1",
		log:      discardLog(),
		interval: time.Hour,
		grace:    time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	if presses.Load() != 1 {
		t.Fatalf("pressed %d times", presses.Load())
	}
}

// A machine that will not go is eventually killed, and says so: the grace
// exists for a job in flight, not for ever.
func TestAMachineThatNeverGoesIsKilled(t *testing.T) {
	exited := make(chan error, 1)
	var killed atomic.Bool

	err := drain(exited, drainOptions{
		press: func() error { return nil },
		kill: func() {
			killed.Store(true)
			exited <- errors.New("killed")
		},
		runner:   "web-1",
		log:      discardLog(),
		interval: time.Hour,
		grace:    20 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("a machine that never stopped was reported as a clean shutdown")
	}
	if !killed.Load() {
		t.Error("it was never killed")
	}
}

// A monitor that cannot be reached is reported and does not stop the waiting:
// the fallback signal may still work.
func TestAnUnreachableMonitorKeepsWaiting(t *testing.T) {
	exited := make(chan error, 1)
	go func() { time.Sleep(10 * time.Millisecond); exited <- nil }()

	if err := drain(exited, drainOptions{
		press:    func() error { return errors.New("no such socket") },
		kill:     func() { t.Error("killed a machine that was still going to stop") },
		runner:   "web-1",
		log:      discardLog(),
		interval: 5 * time.Millisecond,
		grace:    time.Second,
	}); err != nil {
		t.Fatalf("drain: %v", err)
	}
}

// discardLog is a logger that says nothing, so a test's output is its own.
func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The eighteen-minute zombie, as a test.
//
// On a real host: the runner finished its job at 15:04:39Z, deregistered
// itself, and exited. The guest should then have powered the machine off. It
// did not, and nothing noticed — GitHub could not give the machine work
// because the runner had gone, systemd would not replace it because the agent
// was alive, and the fleet showed a healthy runner. Twelve jobs queued behind
// it for twenty minutes, and it took an upgrade to clear.
func TestAMachineThatOutlivesItsRunnerIsStopped(t *testing.T) {
	console := writeConsole(t, "[   55.2] run-runner.sh[1339]: Exiting runner...")
	exited := make(chan error, 1)

	// Bounded, because a watchdog that never fires would otherwise leave this
	// test waiting for the go test timeout rather than saying what is wrong.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var pressed atomic.Int32
	stopped, err := waitForMachine(ctx, exited, watchOptions{
		console: console,
		press: func() error {
			pressed.Add(1)
			// The machine goes when it is asked properly.
			exited <- nil
			return nil
		},
		kill:   func() { t.Error("a machine that answered the power button was killed") },
		runner: "ci-vm-1",
		log:    discardLog(),
		check:  time.Millisecond,
		linger: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("waiting: %v", err)
	}
	if !stopped {
		t.Fatal("the machine was reported as having gone by itself")
	}
	if pressed.Load() == 0 {
		t.Fatal("a machine with no runner on it was left running")
	}
	if ctx.Err() != nil {
		t.Fatal("the machine was only stopped because the test gave up waiting")
	}
}

// A machine whose runner is waiting for work must be left alone, however long
// it waits. That is a pool at rest, not a fault.
func TestAnIdleMachineIsLeftAlone(t *testing.T) {
	console := writeConsole(t, "[   16.0] run-runner.sh[1436]: 2026-08-22 13:46:17Z: Listening for Jobs")
	exited := make(chan error, 1)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Long enough that a five-millisecond linger would have fired a dozen
		// times if the machine were going to be touched at all.
		time.Sleep(60 * time.Millisecond)
		cancel()
	}()

	// Asked at the moment of the press, so the drain's own press — which is
	// the fleet asking, and correct — is not counted.
	var uninvited atomic.Int32
	stopped, err := waitForMachine(ctx, exited, watchOptions{
		console: console,
		press: func() error {
			if ctx.Err() == nil {
				uninvited.Add(1)
			}
			exited <- nil
			return nil
		},
		kill:   func() {},
		runner: "ci-vm-1",
		log:    discardLog(),
		check:  time.Millisecond,
		linger: 5 * time.Millisecond,
	})

	if err != nil {
		t.Fatalf("waiting: %v", err)
	}
	if !stopped {
		t.Fatal("the drain did not report the machine as stopped")
	}
	if uninvited.Load() != 0 {
		t.Fatalf("an idle runner's machine was powered off %d times while it was waiting for work",
			uninvited.Load())
	}
}

// And a machine that goes on its own is reported as such, because that is what
// decides whether its console is worth reading.
func TestAMachineThatGoesByItselfIsNotReportedAsStopped(t *testing.T) {
	exited := make(chan error, 1)
	exited <- nil

	stopped, err := waitForMachine(context.Background(), exited, watchOptions{
		console: writeConsole(t, "anything"),
		press:   func() error { t.Error("pressed the button on a machine that had already gone"); return nil },
		kill:    func() {},
		runner:  "ci-vm-1",
		log:     discardLog(),
		check:   time.Hour,
		linger:  time.Hour,
	})
	if err != nil || stopped {
		t.Fatalf("stopped=%t err=%v", stopped, err)
	}
}
