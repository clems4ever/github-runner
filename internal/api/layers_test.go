package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/clems4ever/github-runner/internal/model"
)

// seen puts an ask in front of the operator, the way a reconciliation pass
// would have.
func (h *harness) seen(pool, repo, digest string, trusted bool) model.RepoLayer {
	h.t.Helper()
	layer, err := h.store.SeeRepoLayer(context.Background(), model.RepoLayer{
		Pool: pool, Repo: repo, Digest: digest,
		Packages: []string{"ffmpeg"}, Recipe: "make deps",
	}, trusted)
	if err != nil {
		h.t.Fatal(err)
	}
	return layer
}

func TestTheLayersAPIListsWhatRepositoriesHaveAskedFor(t *testing.T) {
	h := newHarness(t)
	h.seen("web", "clems4ever/runyard", "aaaa", false)

	var layers []model.RepoLayer
	h.decode(h.do("GET", "/api/layers", nil), &layers)

	if len(layers) != 1 {
		t.Fatalf("got %d layers, want the one that was seen", len(layers))
	}
	if layers[0].Repo != "clems4ever/runyard" || layers[0].Approval != model.LayerPending {
		t.Fatalf("got %+v, want a pending ask from the repository", layers[0])
	}
	// The packages are the thing being approved, so they have to be in what the
	// page is drawn from. An approval button next to a digest is not a decision
	// anybody can make.
	if len(layers[0].Packages) != 1 || layers[0].Recipe == "" {
		t.Fatalf("got %+v, want what it is asking to run", layers[0])
	}
}

// A pool's own page asks about its own pool. Without the filter it would draw
// every repository on the host under one pool's heading.
func TestTheLayersAPINarrowsToOnePool(t *testing.T) {
	h := newHarness(t)
	h.seen("web", "clems4ever/runyard", "aaaa", false)
	h.seen("api", "clems4ever/other", "bbbb", false)

	var layers []model.RepoLayer
	h.decode(h.do("GET", "/api/layers?pool=api", nil), &layers)

	if len(layers) != 1 || layers[0].Pool != "api" {
		t.Fatalf("got %+v, want only the api pool's asks", layers)
	}
}

func TestApprovingALayerRecordsWhoDidIt(t *testing.T) {
	h := newHarness(t)
	layer := h.seen("web", "clems4ever/runyard", "aaaa", false)

	var decided model.RepoLayer
	resp := h.do("POST", "/api/layers/1/decision",
		map[string]any{"approval": "approved", "digest": layer.Digest})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want the decision taken", resp.StatusCode)
	}
	h.decode(resp, &decided)

	if decided.Approval != model.LayerApproved {
		t.Fatalf("approval %q, want approved", decided.Approval)
	}
	// Months later, "who allowed this repository to install a compiler on my
	// host" is a real question with a real answer.
	if decided.DecidedBy != "admin" {
		t.Fatalf("decided by %q, want the operator who was logged in", decided.DecidedBy)
	}
}

// The resolver reads a repository a few times an hour. A decision that took
// effect at its own leisure would read as the button not having worked.
func TestADecisionTakesEffectImmediately(t *testing.T) {
	h := newHarness(t)
	layer := h.seen("web", "clems4ever/runyard", "aaaa", false)

	h.do("POST", "/api/layers/1/decision",
		map[string]any{"approval": "approved", "digest": layer.Digest})

	if len(h.forgot) != 1 || h.forgot[0] != "web" {
		t.Fatalf("forgot %v, want the pool whose layer was just decided", h.forgot)
	}
	if h.nudges == 0 {
		t.Fatal("the fleet was never told, so the runners wait for the next tick")
	}
}

func TestRefusingALayerIsRemembered(t *testing.T) {
	h := newHarness(t)
	layer := h.seen("web", "clems4ever/runyard", "aaaa", false)

	var decided model.RepoLayer
	h.decode(h.do("POST", "/api/layers/1/decision",
		map[string]any{"approval": "refused", "digest": layer.Digest}), &decided)

	if decided.Approval != model.LayerRefused {
		t.Fatalf("approval %q, want refused", decided.Approval)
	}
}

// The whole safety property of the approval workflow: an operator approves the
// script they read. If the repository edits its file while the page is open,
// the id still resolves — to a different definition — and approving it would
// attach a decision to something nobody has seen.
func TestApprovingSomethingThatHasChangedIsRefused(t *testing.T) {
	h := newHarness(t)
	h.seen("web", "clems4ever/runyard", "aaaa", false)

	resp := h.do("POST", "/api/layers/1/decision",
		map[string]any{"approval": "approved", "digest": "what-the-page-was-showing"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want a refusal to approve an unread definition", resp.StatusCode)
	}

	layer, err := h.store.RepoLayerByID(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if layer.Approval != model.LayerPending {
		t.Fatalf("approval %q, want it left undecided", layer.Approval)
	}
}

// A decision has two values and neither of them is "pending". Accepting
// anything else would write a state the resolver does not know how to read.
func TestOnlyApproveOrRefuseIsADecision(t *testing.T) {
	h := newHarness(t)
	layer := h.seen("web", "clems4ever/runyard", "aaaa", false)

	for _, approval := range []string{"pending", "maybe", ""} {
		resp := h.do("POST", "/api/layers/1/decision",
			map[string]any{"approval": approval, "digest": layer.Digest})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%q: status %d, want it rejected", approval, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestDecidingALayerThatIsNotThereIsNotFound(t *testing.T) {
	h := newHarness(t)
	resp := h.do("POST", "/api/layers/404/decision",
		map[string]any{"approval": "approved", "digest": "aaaa"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want not found", resp.StatusCode)
	}
}

// A pool on "trust" has already decided, by policy, and the row says so. The
// list is still where an operator goes to find out what their host has been
// told to build — a trusted pool is not an invisible one.
func TestATrustedPoolsLayersAreStillListed(t *testing.T) {
	h := newHarness(t)
	h.seen("web", "clems4ever/runyard", "aaaa", true)

	var layers []model.RepoLayer
	h.decode(h.do("GET", "/api/layers", nil), &layers)

	if len(layers) != 1 || layers[0].Approval != model.LayerApproved {
		t.Fatalf("got %+v, want an approved row", layers)
	}
	if layers[0].DecidedBy != "policy" {
		t.Fatalf("decided by %q, want it clear that no person read this", layers[0].DecidedBy)
	}
}
