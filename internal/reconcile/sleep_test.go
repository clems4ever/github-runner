package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/model"
)

// sleepingPool is a repository pool allowed to reach zero.
func sleepingPool(min, max int) model.Pool {
	p := elasticPool(min, max)
	p.Sleeps = true
	p.MinReplicas = min
	p.Defaults()
	return p
}

var noon = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

// The state a sleeping pool is in nearly all the time, and the one the whole
// feature is for: nothing running, nothing waiting, nothing started.
func TestASleepingPoolStaysDownWhenNothingIsQueued(t *testing.T) {
	scale := Autoscale(sleepingPool(0, 4), nil, nil, 0, time.Time{}, noon)

	if scale.Target != 0 {
		t.Fatalf("wants %d runners with nothing queued", scale.Target)
	}
	// The fleet page shows this. "Nothing is running" reads like a fault; a
	// pool that is asleep because there is nothing to do should say so.
	if !strings.Contains(scale.Reason, "asleep") {
		t.Fatalf("says %q, which does not say the pool is asleep", scale.Reason)
	}
}

// The job that arrives at three in the morning.
func TestASleepingPoolWakesForAQueuedJob(t *testing.T) {
	scale := Autoscale(sleepingPool(0, 4), nil, nil, 1, time.Time{}, noon)

	if scale.Target != 1 {
		t.Fatalf("wants %d runners for one queued job", scale.Target)
	}
	if !scale.ScaledUp {
		t.Fatal("did not report this as scaling up")
	}
	if !strings.Contains(scale.Reason, "a job is waiting") {
		t.Fatalf("says %q", scale.Reason)
	}
}

// Waking straight to the depth of the queue rather than one runner a pass. A
// pass is thirty seconds; ten jobs would otherwise take five minutes to be
// met, most of it with the host idle.
func TestASleepingPoolWakesToTheWholeQueue(t *testing.T) {
	scale := Autoscale(sleepingPool(0, 4), nil, nil, 3, time.Time{}, noon)

	if scale.Target != 3 {
		t.Fatalf("wants %d runners for three queued jobs", scale.Target)
	}
	if !strings.Contains(scale.Reason, "3 jobs are waiting") {
		t.Fatalf("says %q", scale.Reason)
	}
}

// The ceiling is still the ceiling. A repository that pushes twenty jobs at
// once must not take the whole host because it was allowed to sleep.
func TestAWokenPoolStopsAtItsCeiling(t *testing.T) {
	scale := Autoscale(sleepingPool(0, 2), nil, nil, 20, time.Time{}, noon)

	if scale.Target != 2 {
		t.Fatalf("wants %d runners, which is past the ceiling", scale.Target)
	}
}

// A queue known to be deep, with every runner busy, is met at once rather than
// climbed one a pass. This is the same information used the other way round:
// the pool is awake, and what is behind the jobs it is running is now a number
// rather than a guess.
func TestAKnownQueueIsMetAtOnce(t *testing.T) {
	runners, states := fleetOf("web", "bb")
	scale := Autoscale(sleepingPool(0, 8), runners, states, 4, noon, noon)

	if scale.Target != 6 {
		t.Fatalf("wants %d runners for two busy and four waiting", scale.Target)
	}
	if !strings.Contains(scale.Reason, "4 jobs are waiting") {
		t.Fatalf("says %q", scale.Reason)
	}
}

// A pool with spare capacity is not asked about its queue, so the depth is
// unknown and the old rules apply unchanged. Without this the feature would
// change how every pool scales, not only the ones that sleep.
func TestAnUnaskedQueueChangesNothing(t *testing.T) {
	runners, states := fleetOf("web", "bb")

	asked := Autoscale(elasticPool(1, 8), runners, states, QueueUnknown, noon, noon)
	if asked.Target != 3 || asked.Reason != "every runner is busy" {
		t.Fatalf("wants %d because %q", asked.Target, asked.Reason)
	}
}

// A pool that has finished its work goes back down, which is the same rule as
// before with a floor of zero underneath it.
func TestASleepingPoolGoesBackToSleep(t *testing.T) {
	runners, states := fleetOf("web", "ii")
	pool := sleepingPool(0, 4)

	// Still inside the quiet period: the gap between two jobs is not a reason
	// to tear the machines down and build them again.
	soon := Autoscale(pool, runners, states, 0, noon, noon.Add(ScaleDownAfter-time.Minute))
	if soon.Target != 2 {
		t.Fatalf("wants %d runners a minute after the last job", soon.Target)
	}

	later := Autoscale(pool, runners, states, 0, noon, noon.Add(ScaleDownAfter))
	if later.Target != 0 {
		t.Fatalf("wants %d runners after the quiet lasted", later.Target)
	}
}

// The cost rule. Reading a queue is requests against somebody's rate limit, so
// it is only spent where it changes the answer.
func TestNeedsQueue(t *testing.T) {
	busy, busyStates := fleetOf("web", "bb")
	spare, spareStates := fleetOf("web", "bi")
	booting, bootingStates := fleetOf("web", "?")

	for _, c := range []struct {
		name    string
		pool    model.Pool
		runners []Runner
		states  map[string]github.State
		want    bool
	}{
		{"asleep, with nothing to infer from", sleepingPool(0, 4), nil, nil, true},
		{"awake and full, where the depth is worth knowing", sleepingPool(0, 4), busy, busyStates, true},
		{"awake with somewhere to put the next job", sleepingPool(0, 4), spare, spareStates, false},
		// A runner that is booting is capacity on its way, and the pool is
		// already growing. Asking would spend a request to be told what it is
		// already doing.
		{"already starting one", sleepingPool(0, 4), booting, bootingStates, false},
		{"a pool that does not sleep", elasticPool(1, 4), busy, busyStates, false},
		{"a pool that is switched off", disabled(sleepingPool(0, 4)), nil, nil, false},
	} {
		if got := NeedsQueue(c.pool, c.runners, c.states); got != c.want {
			t.Errorf("%s: NeedsQueue = %v, want %v", c.name, got, c.want)
		}
	}
}

func disabled(p model.Pool) model.Pool { p.Enabled = false; return p }

// The pass, end to end: a pool at zero with a job waiting starts a machine.
func TestAPassWakesASleepingPool(t *testing.T) {
	pool := sleepingPool(0, 4)
	pool.ID, pool.Name, pool.Scope = 1, "web", "o/web"
	h := newHarness(pool)
	ctx := context.Background()

	// Nothing waiting: nothing is created, and the host stays empty.
	result := h.rec.Once(ctx)
	if len(result.Errors) != 0 {
		t.Fatalf("errors: %v", result.Errors)
	}
	if len(h.vm.calls) != 0 {
		t.Fatalf("started %v for a pool with nothing queued", h.vm.calls)
	}

	// Somebody pushes.
	h.gh.queued = 2
	result = h.rec.Once(ctx)
	if len(result.Errors) != 0 {
		t.Fatalf("errors: %v", result.Errors)
	}
	if got := strings.Join(h.vm.calls, "; "); got != "create web-1; create web-2" {
		t.Fatalf("started %q for two queued jobs", got)
	}
}

// A pool that is not allowed to sleep must not cost a request. This is the
// difference between a feature two pools pay for and one every pool pays for.
func TestAnOrdinaryPoolIsNeverAskedAboutItsQueue(t *testing.T) {
	h := newHarness(testPool("web", 2))

	h.rec.Once(context.Background())
	if h.gh.queueCalls != 0 {
		t.Fatalf("asked GitHub about the queue %d times for a pool that never sleeps", h.gh.queueCalls)
	}
}

// One question per pass, not one per runner it might start.
func TestASleepingPoolIsAskedOncePerPass(t *testing.T) {
	pool := sleepingPool(0, 8)
	pool.ID, pool.Name, pool.Scope = 1, "web", "o/web"
	h := newHarness(pool)
	h.gh.queued = 5

	h.rec.Once(context.Background())
	if h.gh.queueCalls != 1 {
		t.Fatalf("asked %d times in one pass", h.gh.queueCalls)
	}
}

// A queue that could not be read leaves the pool where it was — asleep — so
// the jobs wait. That is not something to swallow: it is the one failure that
// looks exactly like a fleet nobody configured.
func TestAQueueThatCannotBeReadIsReported(t *testing.T) {
	pool := sleepingPool(0, 4)
	pool.ID, pool.Name, pool.Scope = 1, "web", "o/web"
	h := newHarness(pool)
	h.gh.queueErr = errors.New("403 rate limited")

	result := h.rec.Once(context.Background())
	if len(h.vm.calls) != 0 {
		t.Fatalf("started %v without knowing whether anything was waiting", h.vm.calls)
	}
	if len(result.Errors) == 0 {
		t.Fatal("said nothing about a queue it could not read")
	}
	if !strings.Contains(strings.Join(result.Errors, "; "), "what is waiting for it") {
		t.Fatalf("errors were %v", result.Errors)
	}
}
