// Package layers answers one question, once per pool per reconcile pass:
// which image should this pool's runners boot?
//
// It is where the repository's own opinion enters the fleet, so it is also
// where that opinion is bounded. The path is:
//
//	the repository's default branch  ->  a spec, or a refusal
//	                                 ->  a digest of what would execute
//	                                 ->  a row an operator has decided about
//	                                 ->  an image, built once, shared
//
// Every step of that is a narrowing. What comes back is a file name and
// nothing else: no packages, no script, nothing the caller has to make a
// judgement about. The judgement is here.
package layers

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/clems4ever/github-runner/internal/github"
	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/repospec"
)

// Store is the fleet's memory of what repositories have asked for.
type Store interface {
	SeeRepoLayer(ctx context.Context, layer model.RepoLayer, trusted bool) (model.RepoLayer, error)
	SetRepoLayerImage(ctx context.Context, id int64, image string) error
	Secret(ctx context.Context, id int64) (model.Secret, error)
}

// Reader reads a file from a repository's default branch — and only from
// there. See github.Client.DefaultBranchFile for why that is a property and
// not an omission.
type Reader interface {
	DefaultBranchFile(ctx context.Context, scope github.Scope, path string) ([]byte, error)
}

// ReaderFor builds a reader for one credential.
type ReaderFor func(secret model.Secret) (Reader, error)

// Builder builds layers and says whether one is built.
type Builder interface {
	EnsureLayer(ctx context.Context, pool model.Pool, layer model.RepoLayer) (image string, ready bool, err error)
}

// Interval is how often a repository's file is re-read.
//
// Not every pass. A pass is every thirty seconds and this is a request to
// GitHub per pool; a repository's runner definition changes on the order of
// weeks. The cost of being a few minutes late to a change is a few minutes of
// runners built from the previous one — and the change needs approving anyway,
// which takes longer than this.
const Interval = 5 * time.Minute

// Resolver answers which image a pool's runners should boot.
type Resolver struct {
	store   Store
	reader  ReaderFor
	builder Builder
	log     *slog.Logger
	now     func() time.Time

	mu   sync.Mutex
	seen map[string]answer
}

// answer is what was decided for a pool, and when.
type answer struct {
	image string
	note  string
	at    time.Time
}

// New builds a resolver.
func New(store Store, reader ReaderFor, builder Builder, log *slog.Logger) *Resolver {
	if log == nil {
		log = slog.Default()
	}
	return &Resolver{
		store: store, reader: reader, builder: builder, log: log,
		now: time.Now, seen: map[string]answer{},
	}
}

// WithClock replaces the clock, for tests.
func (r *Resolver) WithClock(now func() time.Time) *Resolver { r.now = now; return r }

// For is the reconciler's LayerFor: the image this pool's runners should boot,
// and a note for the operator when there is something worth saying.
//
// Never an error. A pool whose repository cannot be read, or whose file does
// not parse, keeps running on the pool's own image — which is what it did
// before the file existed. The note says what happened; taking a fleet down
// over a repository's yaml would be the worse failure by a distance.
func (r *Resolver) For(ctx context.Context, pool model.Pool) (string, string) {
	if !pool.LayersAllowed() || pool.Layers == model.LayersOff {
		return "", ""
	}

	// Cached between reads, so a pass costs nothing. The image already chosen
	// keeps being returned in the meantime: a pool must not flip between two
	// images because a request was slow.
	r.mu.Lock()
	last, known := r.seen[pool.Name]
	r.mu.Unlock()
	if known && r.now().Sub(last.at) < Interval {
		return last.image, last.note
	}

	image, note := r.resolve(ctx, pool)

	r.mu.Lock()
	r.seen[pool.Name] = answer{image: image, note: note, at: r.now()}
	r.mu.Unlock()
	return image, note
}

// Forget drops what was cached for a pool, so the next pass reads the
// repository again. This is what a "check now" button is.
func (r *Resolver) Forget(pool string) {
	r.mu.Lock()
	delete(r.seen, pool)
	r.mu.Unlock()
}

func (r *Resolver) resolve(ctx context.Context, pool model.Pool) (image, note string) {
	secret, err := r.store.Secret(ctx, pool.CredentialID)
	if err != nil {
		return "", fmt.Sprintf("could not read the credential to look at %s: %v", pool.Scope, err)
	}
	reader, err := r.reader(secret)
	if err != nil {
		return "", fmt.Sprintf("could not reach GitHub to look at %s: %v", pool.Scope, err)
	}

	data, err := reader.DefaultBranchFile(ctx,
		github.Scope{Kind: pool.ScopeKind, Path: pool.Scope}, repospec.Path)
	if err != nil {
		return "", fmt.Sprintf("could not read %s from %s: %v", repospec.Path, pool.Scope, err)
	}
	if data == nil {
		// No file. Not a problem and not worth a note on every pass: this is
		// the ordinary state of a repository that has not asked for anything.
		return "", ""
	}

	spec, err := repospec.Parse(data)
	if err != nil {
		// The repository's file is wrong, and the repository is who can fix
		// it. Said plainly, with the path, because the operator reading this
		// is not the person who wrote the file.
		return "", fmt.Sprintf("%s in %s: %v", repospec.Path, pool.Scope, err)
	}
	if spec.Empty() {
		return "", ""
	}

	layer, err := r.store.SeeRepoLayer(ctx, model.RepoLayer{
		Pool: pool.Name, Repo: pool.Scope, Digest: spec.Digest(),
		Packages: spec.EffectivePackages(), Recipe: spec.Recipe,
	}, pool.Layers == model.LayersTrust)
	if err != nil {
		return "", fmt.Sprintf("could not record what %s asked for: %v", pool.Scope, err)
	}

	switch layer.Approval {
	case model.LayerPending:
		return "", fmt.Sprintf("%s is asking to add %s to its runners, and is waiting to be allowed to (%s)",
			pool.Scope, summarise(layer), repospec.Short(layer.Digest))
	case model.LayerRefused:
		// Said once and then not again: the operator has decided, and a pass
		// that repeated this every thirty seconds would bury everything else.
		return "", ""
	}

	built, ready, err := r.builder.EnsureLayer(ctx, pool, layer)
	if err != nil {
		return "", fmt.Sprintf("could not build what %s asked for: %v", pool.Scope, err)
	}
	if !ready {
		return "", fmt.Sprintf("building what %s asked for; its runners use the pool's own image until it is done",
			pool.Scope)
	}

	// Remembered against the row so the collector knows the image is wanted,
	// and so the UI can say which image a repository is running on. Recorded
	// after the build rather than when it was asked for: an image that is not
	// there is not one to keep.
	if layer.Image != built {
		if err := r.store.SetRepoLayerImage(ctx, layer.ID, built); err != nil {
			r.log.Warn("could not record which image a repository's layer built as",
				"pool", pool.Name, "repo", pool.Scope, "image", built, "error", err)
		}
	}
	return built, ""
}

// summarise is what a repository asked for, in a sentence an operator can
// decide about without opening anything.
func summarise(layer model.RepoLayer) string {
	switch {
	case len(layer.Packages) > 0 && layer.Recipe != "":
		return fmt.Sprintf("%d packages and a script", len(layer.Packages))
	case len(layer.Packages) > 0:
		return fmt.Sprintf("%d packages", len(layer.Packages))
	default:
		return "a script"
	}
}
