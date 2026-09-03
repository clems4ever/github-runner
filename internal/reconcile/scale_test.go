package reconcile

import (
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/model"
)

// What resizing a pool does to the machines already on the host.
//
// Nothing writes a runner count: an operator moves a pool's bounds — from the
// editor, or from the plus and minus on the pools list — and everything below
// follows from that. These tests are the promises that control is making. The
// one that matters most is that shrinking never takes a job down with it.

// resize is the whole path from new bounds to actions on the host, which is
// three pure functions and no host at all.
func resize(p model.Pool, runners []Runner, states map[string]github.State) (Scale, []Action) {
	now := time.Now()
	// Busy a moment ago, so the quiet timer is not what is being measured here.
	scale := Autoscale(p, runners, states, now.Add(-time.Second), now)
	names := DesiredNames(p, runners, states, scale.Target)
	return scale, Plan(SpecsFor(p, "fp", "image", names), runners, states)
}

// fleetForPool is fleetOf with the runners built from the pool they belong to,
// so a plan is not full of replacements for a configuration nobody changed.
func fleetForPool(p model.Pool, description string) ([]Runner, map[string]github.State) {
	runners, states := fleetOf(p.Name, description)
	generation := p.Generation("fp", "image", "")
	for i := range runners {
		runners[i].Generation = generation
	}
	return runners, states
}

// fixedPool is a pool of exactly n runners: the shape the list view keeps when
// its stepper moves both bounds together.
func fixedPool(n int) model.Pool { return elasticPool(n, n) }

func TestScalingAFixedPoolDown(t *testing.T) {
	pool := fixedPool(3)
	runners, states := fleetForPool(pool, "bii")

	if _, actions := resize(pool, runners, states); render(actions) != "" {
		t.Fatalf("a pool at the size it was asked for still had work to do: %s", render(actions))
	}

	// What the minus button writes: both bounds, so a fixed pool stays fixed.
	pool.MinReplicas, pool.MaxReplicas = 2, 2
	scale, actions := resize(pool, runners, states)

	if scale.Target != 2 {
		t.Fatalf("target %d, want the new size", scale.Target)
	}
	// Drained, not removed. The surplus runner is asked to stop when its job is
	// done; nothing here can end one that is under way.
	if render(actions) != "drain web-3" {
		t.Fatalf("got %q, want the spare runner drained", render(actions))
	}
	// And the two that stay are left alone: the bounds are not part of a
	// runner's generation, so scaling never replaces a healthy machine.
	for _, a := range actions {
		if a.Reason == "configuration changed" {
			t.Fatalf("scaling replaced %s, which would rebuild a working fleet", a.Runner)
		}
	}
}

// The rule the whole design is for: a pool can be scaled down to one runner
// while a job is in flight, and the job still finishes.
func TestScalingDownKeepsTheRunnersWithJobsOnThem(t *testing.T) {
	pool := fixedPool(3)
	runners, states := fleetForPool(pool, "ibi")

	pool.MinReplicas, pool.MaxReplicas = 1, 1
	_, actions := resize(pool, runners, states)

	if render(actions) != "drain web-1; drain web-3" {
		t.Fatalf("got %q, want the two idle runners drained and the busy one kept", render(actions))
	}
	for _, a := range actions {
		if a.Runner == "web-2" {
			t.Fatalf("the busy runner was told to %s", a.Op)
		}
		if a.Op == OpRemove {
			t.Fatalf("%s was removed rather than drained, which can fail a job", a.Runner)
		}
	}
}

// An autoscaling pool answers the same click differently, because the number
// being moved is a ceiling rather than a size: the autoscaler still decides how
// many runners live under it.
func TestScalingAnAutoscalingPoolMovesItsCeiling(t *testing.T) {
	t.Run("lowering it to where the pool already is changes nothing", func(t *testing.T) {
		pool := elasticPool(1, 4)
		runners, states := fleetForPool(pool, "bbb")

		// Every runner is busy and there is room, so the pool wants a fourth.
		if scale, _ := resize(pool, runners, states); scale.Target != 4 || !scale.ScaledUp {
			t.Fatalf("target %d, want the pool climbing", scale.Target)
		}

		pool.MaxReplicas = 3
		scale, actions := resize(pool, runners, states)
		if scale.Target != 3 {
			t.Fatalf("target %d, want the pool held where it is", scale.Target)
		}
		if render(actions) != "" {
			t.Fatalf("lowering the ceiling onto the pool disturbed it: %s", render(actions))
		}
		if !strings.Contains(scale.Reason, "maximum") {
			t.Fatalf("the reason does not say the pool is capped: %q", scale.Reason)
		}
	})

	t.Run("lowering it below the pool drains, even a busy runner", func(t *testing.T) {
		// Three runners, all with jobs on them, and a ceiling of two. Something
		// has to go, and the only safe way to take a busy runner away is to ask
		// it to stop once its job is over.
		pool := elasticPool(1, 2)
		runners, states := fleetForPool(pool, "bbb")

		scale, actions := resize(pool, runners, states)
		if scale.Target != 2 {
			t.Fatalf("target %d, want the new ceiling", scale.Target)
		}
		if render(actions) != "drain web-3" {
			t.Fatalf("got %q, want the odd runner drained rather than killed", render(actions))
		}
	})

	t.Run("raising it lets a pool that was pinned grow again", func(t *testing.T) {
		pool := elasticPool(1, 3)
		runners, states := fleetForPool(pool, "bbb")

		if scale, actions := resize(pool, runners, states); scale.Target != 3 || render(actions) != "" {
			t.Fatalf("a pool at its ceiling did something: %d, %q", scale.Target, render(actions))
		}

		pool.MaxReplicas = 4
		scale, actions := resize(pool, runners, states)
		if scale.Target != 4 || !scale.ScaledUp {
			t.Fatalf("target %d, want room for one more", scale.Target)
		}
		if render(actions) != "create web-4" {
			t.Fatalf("got %q, want a fourth runner", render(actions))
		}
	})

	t.Run("raising it adds nothing while the pool is idle", func(t *testing.T) {
		// The ceiling is permission, not a request. A quiet pool sits on its
		// floor whatever the ceiling says, which is why the pools list shows
		// the live count next to the bounds rather than instead of them.
		pool := elasticPool(1, 4)
		runners, states := fleetForPool(pool, "i")

		scale, actions := resize(pool, runners, states)
		if scale.Target != 1 {
			t.Fatalf("target %d, want an idle pool left on its floor", scale.Target)
		}
		if render(actions) != "" {
			t.Fatalf("raising the ceiling of an idle pool did something: %s", render(actions))
		}
	})
}

// Scaling a switched-off pool is editing numbers, not machines: it has none.
func TestScalingASwitchedOffPool(t *testing.T) {
	pool := fixedPool(2)
	pool.Enabled = false

	scale, actions := resize(pool, nil, nil)
	if scale.Target != 0 {
		t.Fatalf("target %d, want nothing running", scale.Target)
	}
	if render(actions) != "" {
		t.Fatalf("a switched-off pool built something: %s", render(actions))
	}
}
