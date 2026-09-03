// Package imagebuild builds the golden images the machine pools boot, in the
// daemon, one at a time, and keeps an account of every attempt.
//
// It used to happen somewhere else. A runner's own unit built the image it was
// about to boot, which put the slowest thing this product does inside the
// process least able to report it: the build had no page of its own, its log
// was thrown away by the next attempt, and a recipe that could not work was
// rebuilt every two seconds for ever because that is what a systemd unit does
// with a command that keeps failing. Worse, the pool went on taking jobs while
// it happened, on whatever image it had before.
//
// So it is here instead, and the rules are the ones an operator would state:
//
//   - A pool's runners are not created until the image they would boot exists,
//     so a pool never takes a job on an image that was not built.
//   - A build is attempted once. If it fails, it stays failed until somebody
//     asks for another or changes the recipe — a broken recipe should fail
//     once, loudly, not a thousand times quietly.
//   - Every attempt is kept, with its whole log, against the pool it was for.
package imagebuild

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/clems4ever/github-runner/internal/agent"
	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/paths"
)

// Store is where the builds are kept.
type Store interface {
	StartImageBuild(ctx context.Context, build model.ImageBuild) (model.ImageBuild, error)
	UpdateImageBuild(ctx context.Context, build model.ImageBuild) error
	ImageBuild(ctx context.Context, id int64) (model.ImageBuild, error)
	ImageBuilds(ctx context.Context, pool string, limit int) ([]model.ImageBuild, error)
	LatestImageBuilds(ctx context.Context) (map[string]model.ImageBuild, error)
	AbandonImageBuilds(ctx context.Context, at time.Time, reason string) ([]model.ImageBuild, error)
	PruneImageBuilds(ctx context.Context, keepPerPool int) ([]string, error)
	ForgetImageBuilds(ctx context.Context, pool string) ([]string, error)
}

// State is where a pool's image stands on this host.
type State string

const (
	// StateReady is an image that is built and on disk. This is the only state
	// in which a machine pool may have runners.
	StateReady State = "ready"
	// StateUnbuilt is an image this host has never made and is not making.
	StateUnbuilt State = "unbuilt"
	// StateQueued is a build that has been asked for and is waiting its turn.
	StateQueued State = "queued"
	// StateBuilding is one that is happening now.
	StateBuilding State = "building"
	// StateFailed is a build that did not work and will not be tried again on
	// its own.
	StateFailed State = "failed"
	// StateNone is a pool with no image to build here: a container pool runs
	// an image somebody else published.
	StateNone State = "none"
)

// Status is a pool's image, as the fleet and the page both see it.
type Status struct {
	Pool string `json:"pool"`
	// Image is the file name of the image this pool asks for, which is a hash
	// of everything it is built from.
	Image string `json:"image"`
	State State  `json:"state"`
	// Ready says whether this pool may have runners. It is the one field the
	// reconciler reads, and the reason the rest of this exists.
	Ready bool `json:"ready"`
	// Summary is the state in a sentence, so the page and the fleet's log say
	// the same thing about it.
	Summary string `json:"summary"`
	// Build is the attempt this state comes from: the one happening, or the
	// last one that did. Absent for an image nobody has ever tried to build.
	Build *Build `json:"build,omitempty"`
}

// Build is one attempt, as the page receives it: what was recorded, plus what
// can be seen about it right now.
type Build struct {
	model.ImageBuild
	// Seconds is how long it has been going, or how long it took.
	Seconds int `json:"seconds"`
	// Detail is the last thing its log said, which for a build in progress is
	// the answer to "it has been six minutes and I cannot tell if it is
	// working".
	Detail string `json:"detail,omitempty"`
	// Silent says a running build has printed nothing for a long time. It does
	// not say the build is dead — only that it has stopped saying otherwise,
	// which is what can honestly be reported.
	Silent bool `json:"silent,omitempty"`
	// HasLog says there is something to read. A build that failed before it
	// wrote anything has nothing to open.
	HasLog bool `json:"hasLog"`
}

// Silence is how long a running build may say nothing before that is worth
// remarking on.
//
// Not a timeout and not a diagnosis: the build has its own deadline and gives
// up long before this matters. It is the difference between "this is taking a
// while" and "nothing has happened for a quarter of an hour", which is what
// somebody watching is actually asking. A recipe downloading a large toolchain
// is quiet for minutes at a time, so this is generous on purpose.
const Silence = 15 * time.Minute

// ErrBusy is what asking for a build of something already being built gets.
var ErrBusy = errors.New("that image is already being built")

// Builder builds this host's golden images.
type Builder struct {
	imagesDir string
	sshKey    string
	store     Store
	owner     paths.Owner
	log       *slog.Logger
	now       func() time.Time
	// build is the build itself, replaced in tests by something that does not
	// need QEMU, a network, or forty minutes. buildLayer is the same for a
	// repository's layer.
	build      func(ctx context.Context, o agent.BuildOptions) (string, error)
	buildLayer func(ctx context.Context, o agent.LayerOptions) (string, error)

	mu sync.Mutex
	// latest is the most recent attempt at each image, which is what says
	// whether another may start. Held in memory as well as in the store so
	// that asking is free: the fleet asks once per pool per pass, and the page
	// asks every few seconds.
	latest  map[string]model.ImageBuild
	pending []queued
	wake    chan struct{}
}

// queued is a build waiting its turn: which record it is, and what to build.
//
// The spec travels with it rather than being looked up when the build starts,
// because it is a snapshot of what the pool asked for at the moment it asked.
// A pool edited while its build waits gets a different image, asked for by the
// next pass; this one still builds the thing it was asked for.
type queued struct {
	id   int64
	spec agent.ImageSpec
	// layer is set when this is a repository's layer rather than a pool's own
	// image. It goes through the same queue for the same reasons the queue
	// exists: one QEMU building at a time on a host that is also running jobs,
	// one console log per attempt, and one history somebody can read.
	layer *agent.LayerSpec
	repo  string
}

// Options are what a builder needs.
type Options struct {
	// ImagesDir is where the golden images and their logs live.
	ImagesDir string
	// SSHKey is the host key baked into every image, so a machine built from
	// one can be looked inside.
	SSHKey string
	Store  Store
	// Owner is the account the runners run as. The daemon is root and they are
	// not, so an image it builds has to be handed over or no machine can boot
	// it.
	Owner paths.Owner
	Log   *slog.Logger
}

// New builds a builder. It does not touch the host until Run is called.
func New(o Options) *Builder {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	return &Builder{
		imagesDir:  o.ImagesDir,
		sshKey:     o.SSHKey,
		store:      o.Store,
		owner:      o.Owner,
		log:        log,
		now:        time.Now,
		build:      agent.BuildImage,
		buildLayer: agent.BuildLayer,
		latest:     map[string]model.ImageBuild{},
		wake:       make(chan struct{}, 1),
	}
}

// WithClock replaces the clock, for tests.
func (b *Builder) WithClock(now func() time.Time) *Builder {
	b.now = now
	return b
}

// WithBuild replaces what a build actually does, for tests.
func (b *Builder) WithBuild(build func(ctx context.Context, o agent.BuildOptions) (string, error)) *Builder {
	b.build = build
	return b
}

// LogsDir is where the account of each build is kept, beside the images they
// were attempts at.
func (b *Builder) LogsDir() string { return filepath.Join(b.imagesDir, "logs") }

// Run builds what is asked for, one image at a time, until the context is
// cancelled. Adopt is called first, once, before anything can ask.
func (b *Builder) Run(ctx context.Context) {
	for {
		next, ok := b.take()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-b.wake:
			}
			continue
		}
		b.attempt(ctx, next)
	}
}

// Adopt settles the builds that were happening when this daemon stopped, and
// reads back what this host has already tried.
//
// It is called once, at startup, before anything can ask for a build — a build
// is a process this daemon was running, so one that was going when it went
// down is not going now, whatever its row still says, and settling those after
// new ones had been queued would mark the new ones failed too.
//
// Nothing is retried here. An image whose build was cut short by an upgrade is
// in exactly the state a failed one is, and gets the same treatment: it says
// what happened and waits to be asked again.
func (b *Builder) Adopt(ctx context.Context) error {
	stale, err := b.store.AbandonImageBuilds(ctx, b.now(),
		"the daemon stopped while this build was running, so the build stopped with it")
	if err != nil {
		return fmt.Errorf("settle the image builds that were interrupted: %w", err)
	}
	for _, build := range stale {
		b.log.Warn("an image build did not survive the daemon restarting",
			"pool", build.Pool, "image", build.Image)
	}

	latest, err := b.store.LatestImageBuilds(ctx)
	if err != nil {
		return fmt.Errorf("read what has been built on this host: %w", err)
	}
	b.mu.Lock()
	b.latest = latest
	// Whatever was queued is in the same position: the rows now say those
	// builds ended, so the queue must not go on to run them.
	b.pending = nil
	b.mu.Unlock()

	b.forgetTheOldArrangement()
	return nil
}

// forgetTheOldArrangement deletes what the runners used to leave behind when
// they built their own images: a file per image holding the latest attempt,
// and one console shared by every build on the host.
//
// Nothing writes either any more, and nothing reads them. A host upgraded into
// this version would otherwise keep them for ever — including a console that
// is the only file in the state directory nobody can account for.
func (b *Builder) forgetTheOldArrangement() {
	for _, path := range []string{
		filepath.Join(b.imagesDir, "builds"),
		filepath.Join(b.imagesDir, "last-build-console.log"),
	} {
		if err := os.RemoveAll(path); err != nil {
			b.log.Warn("could not delete what the old image builds left behind",
				"path", path, "error", err)
		}
	}
}

// Status says where a pool's image stands, without starting anything.
func (b *Builder) Status(pool model.Pool) Status {
	if pool.Runtime != model.RuntimeVM {
		return Status{
			Pool: pool.Name, State: StateNone, Ready: true,
			Summary: "a container pool runs an image somebody else published, so there is nothing to build here",
		}
	}

	image := Image(pool)
	status := Status{Pool: pool.Name, Image: image}

	b.mu.Lock()
	last, known := b.latest[image]
	b.mu.Unlock()
	if known {
		build := b.describe(last)
		status.Build = &build
	}

	if _, built := agent.GoldenImage(specFor(pool), b.imagesDir); built {
		status.State, status.Ready = StateReady, true
		status.Summary = "its image is built"
		return status
	}

	switch {
	case known && last.Phase == model.ImageQueued:
		status.State = StateQueued
		status.Summary = "its image is queued to be built; one image is built at a time"
	case known && last.Unfinished():
		status.State = StateBuilding
		status.Summary = fmt.Sprintf("its image has been building for %s", took(status.Build.Seconds))
	case known && last.Phase == model.ImageFailed:
		status.State = StateFailed
		status.Summary = "its image did not build, so the pool has no runners. " +
			"Read the log, then fix the recipe or ask for another build"
	default:
		status.State = StateUnbuilt
		status.Summary = "its image has not been built on this host yet"
	}
	return status
}

// Ensure is Status for a pool that wants runners: the state, and a build asked
// for if this host has never tried to make the image.
//
// Only never. A failure stays a failure until somebody asks for another
// attempt or changes what the image is built from, which is the whole
// difference between a broken recipe that says so once and one that fills a
// journal with the same paragraph every two seconds.
func (b *Builder) Ensure(ctx context.Context, pool model.Pool) Status {
	status := b.Status(pool)
	if status.State != StateUnbuilt {
		return status
	}
	if _, err := b.enqueue(ctx, pool, model.ImageAutomatic); err != nil {
		status.Summary = fmt.Sprintf("its image has not been built, and asking for one failed: %v", err)
		return status
	}
	b.log.Info("building a golden image a pool needs", "pool", pool.Name, "image", status.Image)
	return b.Status(pool)
}

// Rebuild is somebody asking for another attempt, which is how a failed build
// is retried and how an image is made again from scratch.
func (b *Builder) Rebuild(ctx context.Context, pool model.Pool) (Build, error) {
	if pool.Runtime != model.RuntimeVM {
		return Build{}, errors.New("a container pool has no image to build on this host")
	}
	build, err := b.enqueue(ctx, pool, model.ImageRequested)
	if err != nil {
		return Build{}, err
	}
	b.log.Info("an image build was asked for", "pool", pool.Name, "image", build.Image)
	return b.describe(build), nil
}

// History is what this host has tried for one pool, newest first.
func (b *Builder) History(ctx context.Context, pool string, limit int) ([]Build, error) {
	found, err := b.store.ImageBuilds(ctx, pool, limit)
	if err != nil {
		return nil, err
	}
	out := make([]Build, 0, len(found))
	for _, build := range found {
		out = append(out, b.describe(build))
	}
	return out, nil
}

// Build reads one attempt.
func (b *Builder) Build(ctx context.Context, id int64) (Build, error) {
	found, err := b.store.ImageBuild(ctx, id)
	if err != nil {
		return Build{}, err
	}
	return b.describe(found), nil
}

// Log is the account of one build: what the builder did and everything the
// build machine printed, in order.
//
// The end of it, when it is long. A console is megabytes of boot output and
// the part being read is always the end — but a build that has just failed is
// usually short enough to be read whole, so most of the time this is all of
// it.
func (b *Builder) Log(ctx context.Context, id int64, maxBytes int64) (string, error) {
	build, err := b.store.ImageBuild(ctx, id)
	if err != nil {
		return "", err
	}
	if build.Log == "" {
		return "", nil
	}
	return readTail(build.Log, maxBytes)
}

// Forget drops what is remembered about a pool that no longer exists.
//
// The history is filed under the pool it was for, so a deleted pool's builds
// are unreachable — and their logs are consoles, which are not something to
// leave on a host for ever with no way to read them.
func (b *Builder) Forget(ctx context.Context, pool string) error {
	logs, err := b.store.ForgetImageBuilds(ctx, pool)
	if err != nil {
		return err
	}
	for _, path := range logs {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			b.log.Warn("could not delete a build log", "path", path, "error", err)
		}
	}
	return nil
}

// enqueue records a build and puts it in the queue.
func (b *Builder) enqueue(ctx context.Context, pool model.Pool, trigger string) (model.ImageBuild, error) {
	image := Image(pool)

	b.mu.Lock()
	if last, known := b.latest[image]; known && last.Unfinished() {
		b.mu.Unlock()
		return last, fmt.Errorf("%w (asked for by %s)", ErrBusy, last.Pool)
	}
	b.mu.Unlock()

	build, err := b.store.StartImageBuild(ctx, model.ImageBuild{
		Pool: pool.Name, Image: image, Phase: model.ImageQueued,
		Trigger: trigger, StartedAt: b.now(),
	})
	if err != nil {
		return model.ImageBuild{}, err
	}

	b.mu.Lock()
	b.latest[image] = build
	b.pending = append(b.pending, queued{id: build.ID, spec: specFor(pool)})
	b.mu.Unlock()

	select {
	case b.wake <- struct{}{}:
	default:
	}
	return build, nil
}

// take is the next build to run, if there is one.
func (b *Builder) take() (queued, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pending) == 0 {
		return queued{}, false
	}
	next := b.pending[0]
	b.pending = b.pending[1:]
	return next, true
}

// attempt runs one build from start to finish and records how it went.
func (b *Builder) attempt(ctx context.Context, next queued) {
	build, err := b.store.ImageBuild(ctx, next.id)
	if err != nil {
		b.log.Warn("an image build vanished before it started", "id", next.id, "error", err)
		return
	}

	journal, err := b.journal(build.ID)
	if err != nil {
		b.finish(ctx, build, fmt.Errorf("open the build's log: %w", err))
		return
	}
	defer journal.Close()

	build.Log = journal.Path()
	build.Phase = model.ImageFetching
	b.record(ctx, build)

	publicKey, err := agent.EnsureSSHKey(b.sshKey)
	if err != nil {
		b.finish(ctx, build, fmt.Errorf("make the host's ssh key: %w", err))
		return
	}
	b.give(b.sshKey, b.sshKey+".pub", filepath.Dir(b.sshKey))

	if next.layer != nil {
		b.log.Info("building a repository's layer on its pool's image",
			"pool", build.Pool, "repo", next.repo, "image", build.Image)
	} else {
		b.log.Info("building a golden image; this takes a few minutes and happens once per host",
			"pool", build.Pool, "image", build.Image)
	}

	watch := func(phase agent.BuildPhase) {
		build.Phase = model.ImagePhase(phase)
		b.record(ctx, build)
	}

	var image string
	if next.layer != nil {
		image, err = b.buildLayer(ctx, agent.LayerOptions{
			Spec: *next.layer, ImagesDir: b.imagesDir, PublicKey: publicKey,
			Repo: next.repo, Journal: journal, Phase: watch, Log: b.log,
		})
	} else {
		image, err = b.build(ctx, agent.BuildOptions{
			Spec: next.spec, ImagesDir: b.imagesDir, PublicKey: publicKey,
			Journal: journal, Phase: watch, Log: b.log,
		})
	}
	if err == nil {
		// The daemon is root and the runners are not. An image nobody can read
		// is an image nobody can boot, and the machine would fail on a
		// permission error naming a file that plainly exists.
		b.give(image)
		if chmodErr := os.Chmod(image, 0o644); chmodErr != nil {
			b.log.Warn("could not make the golden image readable by the runners",
				"image", image, "error", chmodErr)
		}
	}
	b.finish(ctx, build, err)
}

// finish records how a build ended, in the record and in the log, and then
// forgets the ones nobody will read.
func (b *Builder) finish(ctx context.Context, build model.ImageBuild, err error) {
	ended := b.now()
	build.EndedAt = &ended
	build.Phase = model.ImageSucceeded
	if err != nil {
		build.Phase = model.ImageFailed
		build.Error = err.Error()
		b.log.Warn("an image build failed", "pool", build.Pool, "image", build.Image, "error", err)
	} else {
		b.log.Info("an image build finished", "pool", build.Pool, "image", build.Image,
			"took", build.Took(ended).Round(time.Second).String())
	}
	// Said in the log as well as in the record, because the log is what
	// somebody reads and a log that simply stops is a log that could mean
	// anything.
	b.say(build, err, ended)
	b.record(ctx, build)
	b.prune(ctx)
}

// record stores a build's progress and remembers it as the newest attempt at
// its image.
func (b *Builder) record(ctx context.Context, build model.ImageBuild) {
	b.mu.Lock()
	b.latest[build.Image] = build
	b.mu.Unlock()
	if err := b.store.UpdateImageBuild(ctx, build); err != nil {
		// The build is worth more than the record of it. What this costs is a
		// page that is out of date, not an image that does not get built.
		b.log.Warn("could not record what an image build is doing",
			"pool", build.Pool, "image", build.Image, "error", err)
	}
}

// prune forgets the oldest builds and deletes their logs.
func (b *Builder) prune(ctx context.Context) {
	logs, err := b.store.PruneImageBuilds(ctx, 0)
	if err != nil {
		b.log.Warn("could not prune the image build history", "error", err)
		return
	}
	for _, path := range logs {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			b.log.Warn("could not delete an old build log", "path", path, "error", err)
		}
	}
}

// describe turns a record into what a page receives, which for a build still
// happening means reading the end of its log.
func (b *Builder) describe(build model.ImageBuild) Build {
	now := b.now()
	out := Build{ImageBuild: build, Seconds: int(build.Took(now).Seconds())}
	if build.Log == "" {
		return out
	}
	line, at, ok := lastLine(build.Log)
	out.HasLog = ok
	if !ok {
		return out
	}
	out.Detail = short(line)
	out.Silent = build.Unfinished() && now.Sub(at) > Silence
	return out
}

// give hands paths to the account the runners run as, best effort.
func (b *Builder) give(paths ...string) {
	for _, path := range paths {
		if err := b.owner.Give(path); err != nil {
			b.log.Warn("could not hand a file to the runners", "path", path, "error", err)
		}
	}
}

// Image is the golden image a pool asks for: a hash of everything it is built
// from, so two pools wanting the same thing share one and a pool that edits
// its recipe asks for a new one.
func Image(pool model.Pool) string { return specFor(pool).Name() }

func specFor(pool model.Pool) agent.ImageSpec {
	return agent.ImageSpec{Variant: pool.Image, Packages: pool.Packages, Recipe: pool.Recipe}
}

// took is a number of seconds as somebody would say it.
func took(seconds int) string {
	if seconds < 60 {
		return fmt.Sprintf("%ds", max(seconds, 0))
	}
	if seconds < 3600 {
		return fmt.Sprintf("%dm %ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%dh %dm", seconds/3600, (seconds%3600)/60)
}
