package reconcile

import (
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/model"
)

// These are the autoscaler's rules. GitHub does not publish how many jobs are
// queued for a set of labels, so demand is inferred from what the runners are
// doing — which is why a pool keeps at least one runner even when idle, and
// why every case below is about what "busy" implies.

func elasticPool(min, max int) model.Pool {
	p := model.Pool{
		Name: "web", ScopeKind: model.ScopeRepository, Scope: "o/r",
		Runtime: model.RuntimeVM, MinReplicas: min, MaxReplicas: max,
		CredentialID: 1, Enabled: true,
	}
	p.Defaults()
	return p
}

// pool builds runners and their GitHub states from a compact description:
// "b" is busy, "i" is idle, "?" is registered-but-unknown, "d" is draining.
func fleetOf(pool string, description string) ([]Runner, map[string]github.State) {
	var runners []Runner
	states := map[string]github.State{}
	for i, letter := range strings.Split(description, "") {
		name := pool + "-" + string(rune('1'+i))
		runner := Runner{Name: name, Pool: pool, Generation: "g1", Runtime: model.RuntimeVM, State: StateRunning}
		switch letter {
		case "b":
			states[name] = github.StateBusy
		case "i":
			states[name] = github.StateIdle
		case "?":
			// Registered nowhere yet: a runner that is still booting.
		case "d":
			runner.State = StateStopping
			states[name] = github.StateIdle
		case "x":
			runner.State = StateStopped
			states[name] = github.StateOffline
		}
		runners = append(runners, runner)
	}
	return runners, states
}

func TestAutoscale(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	longAgo := now.Add(-time.Hour)
	justNow := now.Add(-time.Second)

	tests := []struct {
		name     string
		pool     model.Pool
		fleet    string
		lastBusy time.Time
		want     int
		wantUp   bool
		reason   string
	}{
		{
			name: "an empty pool starts at its minimum",
			pool: elasticPool(1, 5), fleet: "", lastBusy: now,
			want: 1, reason: "below the minimum",
		},
		{
			name: "a pool below its minimum climbs to it in one step",
			pool: elasticPool(3, 5), fleet: "i", lastBusy: now,
			want: 3, reason: "below the minimum",
		},
		{
			// The case the whole feature exists for: the one idle runner took a
			// job, so the next job has nowhere to go.
			name: "the last idle runner going busy adds one",
			pool: elasticPool(1, 5), fleet: "b", lastBusy: now,
			want: 2, wantUp: true, reason: "every runner is busy",
		},
		{
			name: "one at a time, not a jump to the ceiling",
			pool: elasticPool(1, 10), fleet: "bbb", lastBusy: now,
			want: 4, wantUp: true,
		},
		{
			name: "spare capacity means no growth",
			pool: elasticPool(1, 5), fleet: "bi", lastBusy: now,
			want: 2, reason: "spare capacity available",
		},
		{
			// A runner that is still booting is capacity. Counting it as
			// missing would add another every pass until it registered.
			name: "a runner that is still starting counts as capacity",
			pool: elasticPool(1, 5), fleet: "b?", lastBusy: now,
			want: 2,
		},
		{
			name: "the maximum is a ceiling",
			pool: elasticPool(1, 3), fleet: "bbb", lastBusy: now,
			want: 3, wantUp: false, reason: "at its maximum",
		},
		{
			// A pool whose minimum equals its maximum never moves, which is how
			// a fixed size is expressed.
			name: "a fixed pool does not grow, however busy",
			pool: elasticPool(2, 2), fleet: "bb", lastBusy: now,
			want: 2, reason: "fixed size",
		},
		{
			name: "quiet for long enough returns to the minimum",
			pool: elasticPool(1, 5), fleet: "iiii", lastBusy: longAgo,
			want: 1, reason: "quiet for",
		},
		{
			// Shrinking is the slow direction: a gap between two jobs must not
			// cost the fleet that is about to be needed again.
			name: "a moment of quiet is not enough",
			pool: elasticPool(1, 5), fleet: "iiii", lastBusy: justNow,
			want: 4, reason: "waiting to see if the quiet lasts",
		},
		{
			name: "one job still running holds the pool up",
			pool: elasticPool(1, 5), fleet: "biii", lastBusy: now,
			want: 4, reason: "spare capacity available",
		},
		{
			name: "draining runners are not capacity",
			// Two draining and one busy: the pool is effectively full.
			pool: elasticPool(1, 5), fleet: "bdd", lastBusy: now,
			want: 2, wantUp: true, reason: "every runner is busy",
		},
		{
			name: "a stopped runner still counts as a slot to be restarted",
			pool: elasticPool(2, 5), fleet: "bx", lastBusy: now,
			want: 2, reason: "spare capacity",
		},
		{
			name:  "a switched-off pool wants nothing",
			pool:  func() model.Pool { p := elasticPool(1, 5); p.Enabled = false; return p }(),
			fleet: "bb", lastBusy: now,
			want: 0, reason: "switched off",
		},
		{
			name: "a pool that has never seen a job does not shrink on that basis",
			// lastBusy is zero: the daemon has just started and knows nothing.
			pool: elasticPool(1, 5), fleet: "iii", lastBusy: time.Time{},
			want: 3, reason: "waiting to see if the quiet lasts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runners, states := fleetOf("web", tt.fleet)
			got := Autoscale(tt.pool, runners, states, QueueUnknown, tt.lastBusy, now)

			if got.Target != tt.want {
				t.Errorf("target is %d, want %d (%s)", got.Target, tt.want, got.Reason)
			}
			if got.ScaledUp != tt.wantUp {
				t.Errorf("scaledUp is %t, want %t", got.ScaledUp, tt.wantUp)
			}
			if tt.reason != "" && !strings.Contains(got.Reason, tt.reason) {
				t.Errorf("reason is %q, want it to mention %q", got.Reason, tt.reason)
			}
			if got.Reason == "" {
				t.Error("no reason given, so the UI would show a size with no explanation")
			}
		})
	}
}

// A burst arrives, is served, and the pool comes back down on its own.
func TestAutoscaleOverABurst(t *testing.T) {
	pool := elasticPool(1, 4)
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	// One idle runner, then work arrives and every pass takes another job.
	steps := []struct {
		fleet string
		want  int
	}{
		{"i", 1},    // quiet
		{"b", 2},    // the one runner is working: make room for the next job
		{"bb", 3},   // both working
		{"bbb", 4},  // still climbing
		{"bbbb", 4}, // at the ceiling; it stays there
	}
	for _, step := range steps {
		runners, states := fleetOf("web", step.fleet)
		got := Autoscale(pool, runners, states, QueueUnknown, now, now)
		if got.Target != step.want {
			t.Fatalf("with %q the target is %d, want %d (%s)", step.fleet, got.Target, step.want, got.Reason)
		}
	}

	// The burst ends. Nothing shrinks until the quiet has lasted.
	runners, states := fleetOf("web", "iiii")
	if got := Autoscale(pool, runners, states, QueueUnknown, now, now.Add(ScaleDownAfter-time.Second)); got.Target != 4 {
		t.Fatalf("it shrank after %s, before the stabilisation window was over", ScaleDownAfter-time.Second)
	}
	if got := Autoscale(pool, runners, states, QueueUnknown, now, now.Add(ScaleDownAfter)); got.Target != 1 {
		t.Fatalf("target is %d after the window, want back to the minimum", got.Target)
	}
}

// Scaling up and down repeatedly must not leave a pool oscillating.
func TestAutoscaleSettles(t *testing.T) {
	pool := elasticPool(2, 6)
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	// Two runners, one busy one idle: there is room, so nothing should change,
	// however many times it is asked.
	runners, states := fleetOf("web", "bi")
	for i := 0; i < 10; i++ {
		if got := Autoscale(pool, runners, states, QueueUnknown, now, now.Add(time.Duration(i)*time.Minute)); got.Target != 2 {
			t.Fatalf("pass %d moved a settled pool to %d (%s)", i, got.Target, got.Reason)
		}
	}
}

func TestDesiredNames(t *testing.T) {
	pool := elasticPool(1, 5)

	t.Run("a fresh pool numbers from one", func(t *testing.T) {
		got := DesiredNames(pool, nil, nil, 3)
		if strings.Join(got, ",") != "web-1,web-2,web-3" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("growing keeps the runners that exist", func(t *testing.T) {
		runners, states := fleetOf("web", "bi")
		got := DesiredNames(pool, runners, states, 4)
		if strings.Join(got, ",") != "web-1,web-2,web-3,web-4" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("shrinking drops an idle runner, never a busy one", func(t *testing.T) {
		// web-1 idle, web-2 busy. Dropping the highest index would pick the one
		// with a job on it and leave an idle machine running instead.
		runners, states := fleetOf("web", "ib")
		got := DesiredNames(pool, runners, states, 1)
		if strings.Join(got, ",") != "web-2" {
			t.Fatalf("got %v, want the busy runner kept", got)
		}
	})

	t.Run("shrinking keeps every busy runner it can", func(t *testing.T) {
		runners, states := fleetOf("web", "ibib")
		got := DesiredNames(pool, runners, states, 2)
		if strings.Join(got, ",") != "web-2,web-4" {
			t.Fatalf("got %v, want both busy runners", got)
		}
	})

	t.Run("a stopped runner is dropped before a running one", func(t *testing.T) {
		runners, states := fleetOf("web", "xi")
		got := DesiredNames(pool, runners, states, 1)
		if strings.Join(got, ",") != "web-2" {
			t.Fatalf("got %v, want the one that is actually up", got)
		}
	})

	t.Run("a gap in the middle is filled rather than grown past", func(t *testing.T) {
		// web-2 was removed at some point. Growing back should reuse its name,
		// so a pool that has scaled for a week still reads web-1 to web-3.
		runners := []Runner{
			{Name: "web-1", Pool: "web", State: StateRunning},
			{Name: "web-3", Pool: "web", State: StateRunning},
		}
		got := DesiredNames(pool, runners, nil, 3)
		if strings.Join(got, ",") != "web-1,web-2,web-3" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("draining runners are not counted", func(t *testing.T) {
		runners, states := fleetOf("web", "dd")
		got := DesiredNames(pool, runners, states, 2)
		// Both existing runners are on their way out, so two fresh names are
		// needed — and they are the ones the draining runners will free.
		if len(got) != 2 {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("nothing wanted, nothing named", func(t *testing.T) {
		runners, states := fleetOf("web", "ii")
		if got := DesiredNames(pool, runners, states, 0); len(got) != 0 {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("the result is sorted, so plans read in order", func(t *testing.T) {
		runners, states := fleetOf("web", "ibib")
		got := DesiredNames(pool, runners, states, 4)
		if strings.Join(got, ",") != "web-1,web-2,web-3,web-4" {
			t.Fatalf("got %v", got)
		}
	})
}

func TestRunnerIndex(t *testing.T) {
	if got := runnerIndex("web", "web-12"); got != 12 {
		t.Errorf("got %d", got)
	}
	// A name from somewhere else sorts last rather than colliding with index 0.
	if got := runnerIndex("web", "something-else"); got <= 64 {
		t.Errorf("an unrelated name sorted early: %d", got)
	}
	if got := runnerIndex("web", "web-notanumber"); got <= 64 {
		t.Errorf("got %d", got)
	}
}

// The bug, from a real host. The pool had grown to three because every runner
// was busy; ten minutes later it threw the third machine away as "no longer
// wanted", with twelve jobs still queued.
//
// An ephemeral runner is invisible between jobs: it deregisters itself the
// moment one ends, its machine powers off, and the next takes twenty seconds
// to come back. Sample the pool in that window — which is most of the window
// for a pool that is working — and nothing is busy and nothing is registered,
// which is indistinguishable from a pool nobody wants.
func TestAPoolWhoseMachinesAreRecyclingIsNotQuiet(t *testing.T) {
	pool := elasticPool(1, 3)
	now := time.Now()

	// Three machines. One is booting back from a job — running, GitHub has not
	// seen it yet, seconds old. The others are registered and idle.
	runners := []Runner{
		{Name: "web-1", Pool: "web", State: StateRunning, Up: 15 * time.Second},
		{Name: "web-2", Pool: "web", State: StateRunning, Up: time.Hour},
		{Name: "web-3", Pool: "web", State: StateRunning, Up: time.Hour},
	}
	states := map[string]github.State{
		"web-2": github.StateIdle,
		"web-3": github.StateIdle,
	}

	// Nothing has been seen busy for half an hour, which used to be enough to
	// shrink the pool to its floor.
	scale := Autoscale(pool, runners, states, QueueUnknown, now.Add(-30*time.Minute), now)

	if scale.Target != 3 {
		t.Fatalf("the pool shrank to %d while a machine was coming back from a job", scale.Target)
	}
	if !strings.Contains(scale.Reason, "coming back") {
		t.Errorf("the reason does not say why it held: %q", scale.Reason)
	}
}

// And the opposite, or the pool would never shrink again: machines that are up,
// registered and doing nothing are exactly what "quiet" means.
func TestAPoolWhoseMachinesAreAllRegisteredAndIdleStillShrinks(t *testing.T) {
	pool := elasticPool(1, 3)
	now := time.Now()

	runners := []Runner{
		{Name: "web-1", Pool: "web", State: StateRunning, Up: time.Hour},
		{Name: "web-2", Pool: "web", State: StateRunning, Up: time.Hour},
	}
	states := map[string]github.State{"web-1": github.StateIdle, "web-2": github.StateIdle}

	scale := Autoscale(pool, runners, states, QueueUnknown, now.Add(-30*time.Minute), now)
	if scale.Target != 1 {
		t.Fatalf("a genuinely quiet pool stayed at %d", scale.Target)
	}
}

// A machine that has been running for an hour without GitHub ever seeing it is
// broken, not booting, and must not hold a pool up for ever.
func TestARunnerThatWillNeverRegisterDoesNotHoldThePoolOpen(t *testing.T) {
	pool := elasticPool(1, 3)
	now := time.Now()

	runners := []Runner{
		{Name: "web-1", Pool: "web", State: StateRunning, Up: time.Hour},
		{Name: "web-2", Pool: "web", State: StateRunning, Up: time.Hour},
	}
	// GitHub has never heard of either.
	scale := Autoscale(pool, runners, map[string]github.State{}, QueueUnknown, now.Add(-30*time.Minute), now)
	if scale.Target != 1 {
		t.Fatalf("a pool of runners that never registered stayed at %d", scale.Target)
	}
}
