package reconcile

import (
	"context"
	"strings"
	"testing"

	"github.com/clems4ever/github-runner/internal/model"
)

// layered gives the harness a resolver that answers with whatever is set, and
// counts how often it was asked.
type layered struct {
	image string
	note  string
	asked int
}

func (l *layered) For(context.Context, model.Pool) (string, string) {
	l.asked++
	return l.image, l.note
}

func layerPool(policy model.LayerPolicy) model.Pool {
	p := testPool("web", 2)
	p.Layers = policy
	return p
}

// The runner has to actually be told, or the whole chain ends at the daemon.
func TestARunnerIsToldWhichLayerToBoot(t *testing.T) {
	h := newHarness(layerPool(model.LayersApprove))
	resolver := &layered{image: "runner-noble-layer-abc123def456.qcow2"}
	h.rec.WithLayers(resolver.For)

	h.rec.Once(context.Background())

	if len(h.vm.created) == 0 {
		t.Fatal("created nothing")
	}
	for _, spec := range h.vm.created {
		if spec.Layer != resolver.image {
			t.Fatalf("%s boots %q, want the layer", spec.Name, spec.Layer)
		}
	}
}

// A pool with no policy must not cost a request to GitHub, and its runners
// must be byte-identical to what they were before layers existed.
func TestAPoolWithNoPolicyIsNeverAsked(t *testing.T) {
	h := newHarness(layerPool(model.LayersOff))
	resolver := &layered{image: "runner-noble-layer-abc123def456.qcow2"}
	h.rec.WithLayers(resolver.For)

	h.rec.Once(context.Background())

	if resolver.asked != 0 {
		t.Fatalf("asked %d times for a pool that wants no layers", resolver.asked)
	}
	for _, spec := range h.vm.created {
		if spec.Layer != "" {
			t.Fatalf("%s was given layer %q", spec.Name, spec.Layer)
		}
	}
}

// Once per pass, not once per runner. It is a request to GitHub behind there.
func TestTheLayerIsResolvedOncePerPassAndNotPerRunner(t *testing.T) {
	h := newHarness(layerPool(model.LayersApprove))
	resolver := &layered{image: "runner-noble-layer-abc123def456.qcow2"}
	h.rec.WithLayers(resolver.For)

	h.rec.Once(context.Background())

	if resolver.asked != 1 {
		t.Fatalf("asked %d times to create 2 runners", resolver.asked)
	}
}

// The reason approving a layer takes effect: the runners built without it are
// running the wrong configuration, and a generation that ignored the layer
// would leave them alone until something else replaced them.
func TestApprovingALayerReplacesTheRunnersBuiltWithoutIt(t *testing.T) {
	h := newHarness(layerPool(model.LayersApprove))
	resolver := &layered{note: "waiting to be allowed to"}
	h.rec.WithLayers(resolver.For)
	ctx := context.Background()

	h.rec.Once(ctx)
	h.vm.calls = nil
	if result := h.rec.Once(ctx); len(result.Actions) != 0 {
		t.Fatalf("a settled fleet was touched: %+v", result.Actions)
	}

	// Somebody approves it, and the layer is built.
	resolver.image, resolver.note = "runner-noble-layer-abc123def456.qcow2", ""
	h.vm.calls = nil

	result := h.rec.Once(ctx)
	if len(result.Actions) == 0 {
		t.Fatal("approving a layer changed nothing")
	}
	// Drained, not killed: a runner on the old image may be mid-job, and the
	// job did not ask for any of this.
	for _, action := range result.Actions {
		if action.Op != OpDrain {
			t.Fatalf("%+v: a runner was not given the chance to finish its job", action)
		}
	}
}

// A repository waiting for approval is the fleet working exactly as
// configured, and it still has to be visible or nobody ever approves it.
func TestWhatARepositoryIsWaitingForIsOnThePass(t *testing.T) {
	h := newHarness(layerPool(model.LayersApprove))
	h.rec.WithLayers((&layered{note: "o/web is asking to add 2 packages"}).For)

	result := h.rec.Once(context.Background())

	if len(result.Notes) != 1 {
		t.Fatalf("notes %v", result.Notes)
	}
	if !strings.Contains(result.Notes[0], "web:") || !strings.Contains(result.Notes[0], "2 packages") {
		t.Fatalf("note %q does not say which pool, or what for", result.Notes[0])
	}
	// A note is not an error. A fleet that reported this as a failure would
	// have an operator looking for a fault that is not there.
	if len(result.Errors) != 0 {
		t.Fatalf("errors: %v", result.Errors)
	}
}

// The runners keep being created while the layer is not ready. The repository
// asked for additions and has not been given them; it has not asked to lose
// its runners.
func TestAPoolStillGetsRunnersWhileItsLayerIsNotReady(t *testing.T) {
	h := newHarness(layerPool(model.LayersApprove))
	h.rec.WithLayers((&layered{note: "building"}).For)

	h.rec.Once(context.Background())

	if len(h.vm.created) != 2 {
		t.Fatalf("created %d runners while a layer was building", len(h.vm.created))
	}
	for _, spec := range h.vm.created {
		if spec.Layer != "" {
			t.Fatalf("%s was pointed at %q before it was built", spec.Name, spec.Layer)
		}
	}
}
