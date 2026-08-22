package agent

import (
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
