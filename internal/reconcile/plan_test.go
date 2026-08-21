package reconcile

import (
	"fmt"
	"strings"
	"testing"

	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/model"
)

// These are the fleet's rules. Every case here is a promise about what the
// daemon will and will not do to a runner, and the one that matters most is
// that nothing ever removes a runner with a job on it.

func spec(name, generation string) Spec {
	return Spec{Name: name, Pool: "web", Generation: generation, Runtime: model.RuntimeVM}
}

func runner(name, generation string, state RunnerState) Runner {
	return Runner{Name: name, Pool: "web", Generation: generation, Runtime: model.RuntimeVM, State: state}
}

// render makes a plan comparable as text, which reads far better in a failure
// than a slice of structs.
func render(actions []Action) string {
	var lines []string
	for _, a := range actions {
		lines = append(lines, fmt.Sprintf("%s %s", a.Op, a.Runner))
	}
	return strings.Join(lines, "; ")
}

func TestPlan(t *testing.T) {
	tests := []struct {
		name    string
		desired []Spec
		actual  []Runner
		states  map[string]github.State
		want    string
	}{
		{
			name:    "an empty host gets the whole pool",
			desired: []Spec{spec("web-1", "g1"), spec("web-2", "g1")},
			want:    "create web-1; create web-2",
		},
		{
			name:    "a fleet that is already right is left alone",
			desired: []Spec{spec("web-1", "g1"), spec("web-2", "g1")},
			actual:  []Runner{runner("web-1", "g1", StateRunning), runner("web-2", "g1", StateRunning)},
			want:    "",
		},
		{
			// The property the whole design exists for: after a daemon restart
			// the runners are still there, and the plan is empty.
			name:    "a daemon restart adopts rather than rebuilds",
			desired: []Spec{spec("web-1", "g1"), spec("web-2", "g1"), spec("web-3", "g1")},
			actual: []Runner{
				runner("web-1", "g1", StateRunning),
				runner("web-2", "g1", StateRunning),
				runner("web-3", "g1", StateRunning),
			},
			states: map[string]github.State{"web-1": github.StateBusy, "web-2": github.StateIdle},
			want:   "",
		},
		{
			name:    "scaling up adds only what is missing",
			desired: []Spec{spec("web-1", "g1"), spec("web-2", "g1"), spec("web-3", "g1")},
			actual:  []Runner{runner("web-1", "g1", StateRunning), runner("web-2", "g1", StateRunning)},
			want:    "create web-3",
		},
		{
			name:    "scaling down drains the extra runners",
			desired: []Spec{spec("web-1", "g1")},
			actual:  []Runner{runner("web-1", "g1", StateRunning), runner("web-2", "g1", StateRunning)},
			want:    "drain web-2",
		},
		{
			name:    "a drained runner is removed once it has stopped",
			desired: []Spec{spec("web-1", "g1")},
			actual:  []Runner{runner("web-1", "g1", StateRunning), runner("web-2", "g1", StateStopped)},
			want:    "remove web-2",
		},
		{
			name:    "a runner that is still stopping is left to finish",
			desired: []Spec{spec("web-1", "g1")},
			actual:  []Runner{runner("web-1", "g1", StateRunning), runner("web-2", "g1", StateStopping)},
			want:    "",
		},
		{
			name:    "a reconfigured runner is drained, not killed",
			desired: []Spec{spec("web-1", "g2")},
			actual:  []Runner{runner("web-1", "g1", StateRunning)},
			want:    "drain web-1",
		},
		{
			name:    "and rebuilt once it has stopped",
			desired: []Spec{spec("web-1", "g2")},
			actual:  []Runner{runner("web-1", "g1", StateStopped)},
			want:    "remove web-1; create web-1",
		},
		{
			name:    "a wanted runner that died is started again",
			desired: []Spec{spec("web-1", "g1")},
			actual:  []Runner{runner("web-1", "g1", StateStopped)},
			want:    "start web-1",
		},
		{
			name:    "a disabled or deleted pool drains everything",
			desired: nil,
			actual:  []Runner{runner("web-1", "g1", StateRunning), runner("web-2", "g1", StateRunning)},
			want:    "drain web-1; drain web-2",
		},
		{
			// GitHub says a job is on it. The unit says it has stopped. That
			// cannot both be true, and the safe reading is to wait rather than
			// delete the machine a job might still be on.
			name:    "a busy runner is never removed",
			desired: nil,
			actual:  []Runner{runner("web-1", "g1", StateStopped)},
			states:  map[string]github.State{"web-1": github.StateBusy},
			want:    "",
		},
		{
			name:    "nor rebuilt underneath a job",
			desired: []Spec{spec("web-1", "g2")},
			actual:  []Runner{runner("web-1", "g1", StateStopped)},
			states:  map[string]github.State{"web-1": github.StateBusy},
			want:    "",
		},
		{
			// Offline is not busy: a runner GitHub has lost is exactly the one
			// that has to be replaceable, or a dead VM would block its slot
			// for ever.
			name:    "an offline runner is not treated as busy",
			desired: nil,
			actual:  []Runner{runner("web-1", "g1", StateStopped)},
			states:  map[string]github.State{"web-1": github.StateOffline},
			want:    "remove web-1",
		},
		{
			name:    "a runner from a pool that no longer exists is cleaned up",
			desired: []Spec{spec("web-1", "g1")},
			actual: []Runner{
				runner("web-1", "g1", StateRunning),
				{Name: "old-1", Pool: "old", Generation: "gx", Runtime: model.RuntimeVM, State: StateStopped},
			},
			want: "remove old-1",
		},
		{
			name: "pools are independent",
			desired: []Spec{
				spec("web-1", "g1"),
				{Name: "api-1", Pool: "api", Generation: "h1", Runtime: model.RuntimeContainer},
			},
			actual: []Runner{runner("web-1", "g1", StateRunning)},
			want:   "create api-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := render(Plan(tt.desired, tt.actual, tt.states))
			if got != tt.want {
				t.Fatalf("\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

// A plan that shuffles between runs is a plan nobody can review, and it would
// make every test here flaky.
func TestPlanIsDeterministic(t *testing.T) {
	desired := []Spec{spec("web-3", "g1"), spec("web-1", "g1"), spec("web-2", "g1")}
	actual := []Runner{runner("web-9", "g1", StateStopped), runner("web-8", "g1", StateStopped)}

	first := render(Plan(desired, actual, nil))
	for i := 0; i < 20; i++ {
		if got := render(Plan(desired, actual, nil)); got != first {
			t.Fatalf("plan %d differs:\n%q\n%q", i, got, first)
		}
	}
	want := "create web-1; create web-2; create web-3; remove web-8; remove web-9"
	if first != want {
		t.Fatalf("got %q, want %q", first, want)
	}
}

func TestPlanCarriesTheSpecForCreates(t *testing.T) {
	actions := Plan([]Spec{spec("web-1", "g1")}, nil, nil)
	if len(actions) != 1 || actions[0].Spec == nil {
		t.Fatalf("a create must carry the spec the executor builds from: %+v", actions)
	}
	if actions[0].Spec.Name != "web-1" {
		t.Fatalf("got %+v", actions[0].Spec)
	}
	if actions[0].Reason == "" {
		t.Error("every action needs a reason: they are what the daemon logs and the UI shows")
	}
}

// A container pool and a VM pool are dispatched to different executors, so the
// runtime has to survive into the action.
func TestActionsCarryTheRuntime(t *testing.T) {
	actions := Plan(
		[]Spec{{Name: "api-1", Pool: "api", Generation: "g", Runtime: model.RuntimeContainer}},
		nil, nil)
	if actions[0].Runtime != model.RuntimeContainer {
		t.Fatalf("got %q", actions[0].Runtime)
	}

	// And when removing, it comes from what is on the host rather than from a
	// pool that may no longer exist.
	actions = Plan(nil, []Runner{{Name: "api-1", Runtime: model.RuntimeContainer, State: StateStopped}}, nil)
	if actions[0].Runtime != model.RuntimeContainer {
		t.Fatalf("got %q", actions[0].Runtime)
	}
}

func TestSpecsForAPool(t *testing.T) {
	p := model.Pool{
		ID: 7, Name: "web", ScopeKind: model.ScopeRepository, Scope: "o/r",
		Runtime: model.RuntimeVM, Replicas: 2, Nested: true, Ephemeral: true,
		Labels: []string{"gpu"}, CredentialID: 3, Enabled: true,
	}
	p.Defaults()

	specs := SpecsFor(p, "fp")
	if len(specs) != 2 {
		t.Fatalf("got %d specs, want 2", len(specs))
	}
	if specs[0].Name != "web-1" || specs[1].Name != "web-2" {
		t.Fatalf("names are %q and %q", specs[0].Name, specs[1].Name)
	}
	if specs[0].Generation != specs[1].Generation {
		t.Fatal("replicas of one pool must share a generation, or they would replace each other for ever")
	}
	if specs[0].Generation != p.Generation("fp") {
		t.Fatal("the spec's generation must be the pool's, or a restart would see every runner as stale")
	}
	if specs[0].URL != "https://github.com/o/r" || specs[0].CredentialID != 3 {
		t.Fatalf("got %+v", specs[0])
	}
	if strings.Join(specs[0].Labels, ",") != "vm,nested,ephemeral,gpu" {
		t.Fatalf("labels are %v", specs[0].Labels)
	}
}

func TestSpecsForADisabledPool(t *testing.T) {
	p := model.Pool{Name: "web", Replicas: 3, Enabled: false}
	p.Defaults()
	if specs := SpecsFor(p, "fp"); len(specs) != 0 {
		t.Fatalf("a disabled pool asked for %d runners", len(specs))
	}
}
