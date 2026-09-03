package store

import (
	"context"
	"errors"
	"testing"

	"github.com/clems4ever/github-runner/internal/model"
)

func seen(t *testing.T, s *Store, digest string, trusted bool) model.RepoLayer {
	t.Helper()
	layer, err := s.SeeRepoLayer(context.Background(), model.RepoLayer{
		Pool: "web", Repo: "clems4ever/runyard", Digest: digest,
		Packages: []string{"jq", "ripgrep"}, Recipe: "echo hello\n",
	}, trusted)
	if err != nil {
		t.Fatal(err)
	}
	return layer
}

// The default is that a repository asking for something gets nothing until a
// person says so. Anything else would let a pull request merged to the default
// branch install packages on the operator's host.
func TestASeenLayerWaitsForADecision(t *testing.T) {
	s := newStore(t)
	layer := seen(t, s, "aaaa", false)

	if layer.Approval != model.LayerPending {
		t.Fatalf("approval %q, want pending", layer.Approval)
	}
	if layer.Buildable() {
		t.Fatal("a layer nobody has looked at is buildable")
	}
	if layer.FirstSeen.IsZero() || layer.LastSeen.IsZero() {
		t.Fatal("the sighting was not timestamped")
	}
	if !layer.DecidedAt.IsZero() {
		t.Fatal("undecided, and yet decided at a time")
	}
	if len(layer.Packages) != 2 || layer.Packages[0] != "jq" {
		t.Fatalf("packages %v did not survive the round trip", layer.Packages)
	}
}

// Every reconciliation pass sees the same layer again. Re-inserting, or
// re-deciding, or losing the first sighting would each be a bug — the pass
// runs every thirty seconds.
func TestSeeingALayerAgainOnlyMovesLastSeen(t *testing.T) {
	s := newStore(t)
	first := seen(t, s, "aaaa", false)

	if _, err := s.DecideRepoLayer(context.Background(), first.ID, model.LayerApproved, "alice"); err != nil {
		t.Fatal(err)
	}
	again := seen(t, s, "aaaa", false)

	if again.ID != first.ID {
		t.Fatalf("id %d then %d: the same ask became two rows", first.ID, again.ID)
	}
	if again.Approval != model.LayerApproved {
		t.Fatal("a second sighting un-approved a layer somebody had approved")
	}
	if !again.FirstSeen.Equal(first.FirstSeen) {
		t.Fatal("the first sighting moved")
	}
	if again.LastSeen.Before(first.LastSeen) {
		t.Fatal("last seen went backwards")
	}
}

// The digest is the key, so editing the file in the repository is a new ask
// rather than a change to one already approved. This is the property the whole
// approval workflow rests on.
func TestEditingTheFileIsANewAsk(t *testing.T) {
	s := newStore(t)
	first := seen(t, s, "aaaa", false)
	if _, err := s.DecideRepoLayer(context.Background(), first.ID, model.LayerApproved, "alice"); err != nil {
		t.Fatal(err)
	}

	edited := seen(t, s, "bbbb", false)
	if edited.ID == first.ID {
		t.Fatal("a different digest reused the approved row")
	}
	if edited.Approval != model.LayerPending {
		t.Fatalf("approval %q: an edit inherited yesterday's decision", edited.Approval)
	}
}

// A pool on "trust" is an operator saying in advance that this repository may
// do this. It has to actually mean something, or the setting is decoration.
func TestATrustedPoolNeedsNobodyToClick(t *testing.T) {
	s := newStore(t)
	layer := seen(t, s, "aaaa", true)

	if !layer.Buildable() {
		t.Fatalf("approval %q on a trusted pool", layer.Approval)
	}
	if layer.DecidedBy != "policy" {
		t.Fatalf("decided by %q, want the policy on the record rather than a person who never clicked", layer.DecidedBy)
	}
}

// Refusing is not the same as never having looked, and the difference is what
// stops the UI asking the same question every thirty seconds.
func TestARefusedLayerStaysRefusedAndIsNotBuilt(t *testing.T) {
	s := newStore(t)
	layer := seen(t, s, "aaaa", false)

	refused, err := s.DecideRepoLayer(context.Background(), layer.ID, model.LayerRefused, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if refused.Buildable() {
		t.Fatal("a refused layer is buildable")
	}
	if refused.DecidedBy != "bob" {
		t.Fatalf("decided by %q, want the person who decided", refused.DecidedBy)
	}
	if refused.DecidedAt.IsZero() {
		t.Fatal("no record of when it was refused")
	}

	if again := seen(t, s, "aaaa", false); again.Approval != model.LayerRefused {
		t.Fatal("seeing it again asked the question a second time")
	}
}

// "Pending" is not a decision, so it must not be reachable through the call
// that records one — an API that accepted it would let somebody un-decide a
// layer and lose who had refused it and why.
func TestDecidingAcceptsOnlyADecision(t *testing.T) {
	s := newStore(t)
	layer := seen(t, s, "aaaa", false)

	if _, err := s.DecideRepoLayer(context.Background(), layer.ID, model.LayerPending, "alice"); err == nil {
		t.Fatal("un-decided a layer")
	}
}

func TestDecidingALayerThatIsNotThere(t *testing.T) {
	s := newStore(t)
	if _, err := s.DecideRepoLayer(context.Background(), 404, model.LayerApproved, "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v, want not found", err)
	}
}

// The image is recorded when the build finishes, which is minutes after the
// approval and is what makes the build happen once rather than every pass.
func TestTheBuiltImageIsRememberedAgainstTheLayer(t *testing.T) {
	s := newStore(t)
	layer := seen(t, s, "aaaa", true)

	if err := s.SetRepoLayerImage(context.Background(), layer.ID, "runner-noble-layer-abc123.qcow2"); err != nil {
		t.Fatal(err)
	}
	got, err := s.RepoLayerByID(context.Background(), layer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Image != "runner-noble-layer-abc123.qcow2" {
		t.Fatalf("image %q", got.Image)
	}
}

// The collector deletes images no pool asks for. A layer's image is asked for
// by a row in this table and by nothing else, so without this it is collected
// as soon as its grace runs out — every time, for ever.
func TestTheCollectorIsToldWhichLayerImagesAreWanted(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	approved := seen(t, s, "aaaa", true)
	if err := s.SetRepoLayerImage(ctx, approved.ID, "runner-noble-layer-aaa.qcow2"); err != nil {
		t.Fatal(err)
	}

	// Built once, then refused after the fact. The image is no longer wanted,
	// and leaving it in the set would keep it on the disk for ever.
	stale := seen(t, s, "bbbb", true)
	if err := s.SetRepoLayerImage(ctx, stale.ID, "runner-noble-layer-bbb.qcow2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DecideRepoLayer(ctx, stale.ID, model.LayerRefused, "bob"); err != nil {
		t.Fatal(err)
	}

	// Approved but not built yet: there is no image to keep.
	seen(t, s, "cccc", true)

	wanted, err := s.WantedLayerImages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !wanted["runner-noble-layer-aaa.qcow2"] {
		t.Fatal("the live layer's image is not wanted")
	}
	if wanted["runner-noble-layer-bbb.qcow2"] {
		t.Fatal("a refused layer's image is still wanted")
	}
	if len(wanted) != 1 {
		t.Fatalf("wanted %v", wanted)
	}
}

// Most recently seen first, because the row asking for a decision now is the
// one the operator opened the page for.
func TestLayersAreListedMostRecentlySeenFirst(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	seen(t, s, "aaaa", false)
	seen(t, s, "bbbb", false)
	seen(t, s, "aaaa", false) // touched again, so it goes back to the top

	layers, err := s.ListRepoLayers(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 2 {
		t.Fatalf("listed %d layers, want 2", len(layers))
	}
	if layers[0].Digest != "aaaa" {
		t.Fatalf("first is %q, want the one just seen", layers[0].Digest)
	}

	if narrowed, err := s.ListRepoLayers(ctx, "api"); err != nil || len(narrowed) != 0 {
		t.Fatalf("narrowing to another pool gave %v, %v", narrowed, err)
	}
}

// A pool name is not a stable identity — pools are deleted and recreated. An
// approval outliving the pool it was made for would arm a new pool with a
// decision nobody made about it.
func TestDeletingAPoolForgetsWhatItsRepositoriesAskedFor(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	cred := credential(t, s)
	pool, err := s.CreatePool(ctx, samplePool(cred.ID))
	if err != nil {
		t.Fatal(err)
	}
	seen(t, s, "aaaa", true)

	if err := s.DeletePool(ctx, pool.ID); err != nil {
		t.Fatal(err)
	}
	layers, err := s.ListRepoLayers(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 0 {
		t.Fatalf("%d approvals outlived the pool", len(layers))
	}
}
