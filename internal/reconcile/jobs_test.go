package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/github"
)

// clockedHarness is a fleet whose time the test controls, which is what the job
// accounting is: a sum of intervals between passes.
func clockedHarness(t *testing.T, pool string, replicas int) (*harness, *time.Time) {
	t.Helper()
	h := newHarness(testPool(pool, replicas))
	now := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	h.rec.WithClock(func() time.Time { return now })
	return h, &now
}

func TestJobsAreCountedAsRunnersPickThemUp(t *testing.T) {
	h, clock := clockedHarness(t, "web", 2)
	ctx := context.Background()

	// The first pass builds the fleet. Nothing has run, and there is no
	// previous pass for any time to have been spent between.
	h.rec.Once(ctx)
	if got := h.store.tallyOf("web"); got.Started != 0 || got.BusySeconds != 0 {
		t.Fatalf("a fleet that has only just been created has run nothing: %+v", got)
	}

	// A job lands on one of the two runners.
	h.gh.states["web-1"] = github.StateBusy
	*clock = clock.Add(30 * time.Second)
	h.rec.Once(ctx)
	if got := h.store.tallyOf("web"); got.Started != 1 || got.BusySeconds != 30 {
		t.Fatalf("want one job and thirty runner-seconds, got %+v", got)
	}

	// The same job on the next pass: more time spent on it, but not a second
	// job. A tally that counted a running job every time it looked would report
	// a fleet's passes rather than its work.
	*clock = clock.Add(30 * time.Second)
	h.rec.Once(ctx)
	if got := h.store.tallyOf("web"); got.Started != 0 || got.BusySeconds != 30 {
		t.Fatalf("the job in flight was counted again: %+v", got)
	}

	// A second job, on the other runner. The time is runner-time, so two
	// runners busy across one thirty-second interval is a minute.
	h.gh.states["web-2"] = github.StateBusy
	*clock = clock.Add(30 * time.Second)
	h.rec.Once(ctx)
	if got := h.store.tallyOf("web"); got.Started != 1 || got.BusySeconds != 60 {
		t.Fatalf("want a second job and a minute of runner-time, got %+v", got)
	}

	// Both finish.
	h.gh.states["web-1"] = github.StateIdle
	h.gh.states["web-2"] = github.StateIdle
	*clock = clock.Add(30 * time.Second)
	h.rec.Once(ctx)
	if got := h.store.tallyOf("web"); got.Started != 0 || got.BusySeconds != 0 {
		t.Fatalf("an idle pool is still being charged for work: %+v", got)
	}
}

// A daemon that was stopped for an hour did not watch an hour of work, and must
// not say it did. Under-reporting a fleet nobody was watching is the honest
// direction to be wrong in.
func TestATallyDoesNotInventTimeTheDaemonWasNotThereFor(t *testing.T) {
	h, clock := clockedHarness(t, "web", 1)
	ctx := context.Background()

	h.rec.Once(ctx)
	h.gh.states["web-1"] = github.StateBusy

	*clock = clock.Add(time.Hour)
	h.rec.Once(ctx)

	if got := h.store.tallyOf("web"); got.BusySeconds != MaxGap.Seconds() {
		t.Fatalf("want the gap capped at %v, got %+v", MaxGap, got)
	}
}

// GitHub is the only thing that knows whether a job is on a runner, so losing
// it means the daemon stops learning — not that every job ended.
func TestGitHubGoingQuietDoesNotEndAndRestartEveryJob(t *testing.T) {
	h, clock := clockedHarness(t, "web", 1)
	ctx := context.Background()

	h.rec.Once(ctx)
	h.gh.states["web-1"] = github.StateBusy
	*clock = clock.Add(30 * time.Second)
	h.rec.Once(ctx)
	if got := h.store.tallyOf("web"); got.Started != 1 {
		t.Fatalf("the job was not seen at all: %+v", got)
	}

	h.gh.err = errors.New("dial tcp: no route to host")
	*clock = clock.Add(30 * time.Second)
	h.rec.Once(ctx)
	got := h.store.tallyOf("web")
	if got.Started != 0 {
		t.Fatalf("silence was read as a new job: %+v", got)
	}
	if got.BusySeconds != 30 {
		t.Fatalf("a job does not stop because the daemon could not ask about it: %+v", got)
	}

	// And when GitHub answers again, it is still the same job.
	h.gh.err = nil
	*clock = clock.Add(30 * time.Second)
	h.rec.Once(ctx)
	if got := h.store.tallyOf("web"); got.Started != 0 {
		t.Fatalf("the outage clearing started the job over again: %+v", got)
	}
}

// A runner that is replaced between jobs — every ephemeral runner, after every
// job — is a different name doing different work, and the tally has to say so.
func TestWorkOnAFreshRunnerIsANewJob(t *testing.T) {
	h, clock := clockedHarness(t, "web", 1)
	ctx := context.Background()

	h.rec.Once(ctx)
	h.gh.states["web-1"] = github.StateBusy
	*clock = clock.Add(30 * time.Second)
	h.rec.Once(ctx)

	// The runner finishes, goes away, and the pool builds its replacement.
	delete(h.gh.states, "web-1")
	h.vm.Remove(ctx, "web-1")
	*clock = clock.Add(30 * time.Second)
	h.rec.Once(ctx)

	h.gh.states["web-1"] = github.StateBusy
	*clock = clock.Add(30 * time.Second)
	h.rec.Once(ctx)
	if got := h.store.tallyOf("web"); got.Started != 1 {
		t.Fatalf("the replacement's job was not counted: %+v", got)
	}
}

// The tally is per pool: two pools on one host are two different sizing
// decisions, and adding them together would answer neither.
func TestEachPoolIsAccountedSeparately(t *testing.T) {
	h := newHarness(testPool("web", 1), testPool("api", 1))
	now := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	h.rec.WithClock(func() time.Time { return now })
	ctx := context.Background()

	h.rec.Once(ctx)
	h.gh.states["web-1"] = github.StateBusy
	now = now.Add(30 * time.Second)
	h.rec.Once(ctx)

	if got := h.store.tallyOf("web"); got.Started != 1 || got.BusySeconds != 30 {
		t.Fatalf("web: %+v", got)
	}
	if got := h.store.tallyOf("api"); got.Started != 0 || got.BusySeconds != 0 {
		t.Fatalf("api ran nothing and was charged anyway: %+v", got)
	}
}
