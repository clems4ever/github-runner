package imagebuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/agent"
	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/secrets"
	"github.com/clems4ever/github-runner/internal/store"
)

// fixture is a builder over a real database, with the build itself replaced:
// what is being tested is the rules around a build, not QEMU.
type fixture struct {
	t         *testing.T
	builder   *Builder
	imagesDir string
	// attempts counts how many times a build was actually run, which is the
	// number most of these tests are about.
	attempts int
	// outcome decides what the next build does.
	outcome error
	// says is written to the build's log, standing in for a console.
	says string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	ring, err := secrets.LoadOrCreateKey(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dir, "fleet.db"), ring)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	f := &fixture{t: t, imagesDir: filepath.Join(dir, "images"), says: "cloud-init running\n"}
	f.builder = New(Options{
		ImagesDir: f.imagesDir,
		SSHKey:    filepath.Join(dir, "ssh", "id_ed25519"),
		Store:     db,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).WithBuild(func(ctx context.Context, o agent.BuildOptions) (string, error) {
		f.attempts++
		fmt.Fprint(o.Journal, f.says)
		if f.outcome != nil {
			return "", f.outcome
		}
		path := o.Spec.Path(o.ImagesDir)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", err
		}
		return path, os.WriteFile(path, []byte("a golden image"), 0o600)
	})
	return f
}

// run does what the worker would, without a goroutine: it is the same queue,
// taken one build at a time.
func (f *fixture) run() {
	f.t.Helper()
	for {
		next, ok := f.builder.take()
		if !ok {
			return
		}
		f.builder.attempt(context.Background(), next)
	}
}

func (f *fixture) pool() model.Pool {
	return model.Pool{Name: "web", Runtime: model.RuntimeVM, Image: "default", Recipe: "echo hello\n"}
}

// The rule the whole package exists for: a pool is not ready until the image
// its runners would boot has been built, and it is ready once it has.
func TestAPoolIsNotReadyUntilItsImageIsBuilt(t *testing.T) {
	f := newFixture(t)
	pool := f.pool()

	if status := f.builder.Status(pool); status.Ready || status.State != StateUnbuilt {
		t.Fatalf("a host that has built nothing reports %+v", status)
	}

	// Asking is what starts the first build. Until it finishes the pool is
	// still not ready — this is the window in which the old fleet handed
	// GitHub a runner and let it take a job.
	status := f.builder.Ensure(context.Background(), pool)
	if status.Ready || status.State != StateQueued {
		t.Fatalf("a pool whose build has just been queued reports %+v", status)
	}

	f.run()
	if f.attempts != 1 {
		t.Fatalf("the image was built %d times", f.attempts)
	}
	if status := f.builder.Status(pool); !status.Ready || status.State != StateReady {
		t.Fatalf("after a build that worked, the pool reports %+v", status)
	}
}

// A recipe that cannot work should fail once, loudly. It used to fail every
// two seconds for ever, because a systemd unit is what was retrying it.
func TestAFailedBuildIsNotTriedAgainOnItsOwn(t *testing.T) {
	f := newFixture(t)
	f.outcome = errors.New("the recipe exited 1")
	pool := f.pool()

	f.builder.Ensure(context.Background(), pool)
	f.run()

	status := f.builder.Status(pool)
	if status.Ready || status.State != StateFailed {
		t.Fatalf("after a build that failed, the pool reports %+v", status)
	}
	if status.Build == nil || status.Build.Error != "the recipe exited 1" {
		t.Fatalf("the failure does not say why: %+v", status.Build)
	}

	// The fleet goes on asking, once a pass, for as long as the pool exists.
	for range 5 {
		f.builder.Ensure(context.Background(), pool)
		f.run()
	}
	if f.attempts != 1 {
		t.Fatalf("a failed build was retried on its own: %d attempts", f.attempts)
	}
}

// Not retried on its own is not the same as stuck. Somebody who has read the
// log can ask for another.
func TestAFailedBuildCanBeAskedForAgain(t *testing.T) {
	f := newFixture(t)
	f.outcome = errors.New("the recipe exited 1")
	pool := f.pool()

	f.builder.Ensure(context.Background(), pool)
	f.run()

	f.outcome = nil
	if _, err := f.builder.Rebuild(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	f.run()

	if f.attempts != 2 {
		t.Fatalf("asking for another build ran %d in total", f.attempts)
	}
	if status := f.builder.Status(pool); !status.Ready {
		t.Fatalf("the pool is still not ready after a build that worked: %+v", status)
	}
}

// The other way a failure is retried: fixing the recipe. The image's name is a
// hash of what it is built from, so an edited recipe is a different image and
// has no failure against it.
func TestFixingTheRecipeBuildsAgainByItself(t *testing.T) {
	f := newFixture(t)
	f.outcome = errors.New("the recipe exited 1")
	pool := f.pool()

	f.builder.Ensure(context.Background(), pool)
	f.run()

	f.outcome = nil
	pool.Recipe = "echo hello, and this time it works\n"
	f.builder.Ensure(context.Background(), pool)
	f.run()

	if f.attempts != 2 {
		t.Fatalf("a fixed recipe ran %d builds in total", f.attempts)
	}
	if status := f.builder.Status(pool); !status.Ready {
		t.Fatalf("the pool is not ready after its new image was built: %+v", status)
	}
}

// Two pools that want the same thing want the same image, and one build serves
// both. The second must not start one of its own on top of it.
func TestTwoPoolsWantingTheSameImageShareOneBuild(t *testing.T) {
	f := newFixture(t)
	one := f.pool()
	two := f.pool()
	two.Name = "api"

	f.builder.Ensure(context.Background(), one)
	f.builder.Ensure(context.Background(), two)
	f.run()

	if f.attempts != 1 {
		t.Fatalf("two pools wanting one image built it %d times", f.attempts)
	}
	if status := f.builder.Status(two); !status.Ready {
		t.Fatalf("the pool that waited is not ready: %+v", status)
	}
}

// Every attempt is kept, with everything it printed. The old arrangement held
// one record per image and threw away the previous one, so the failure
// somebody was reading vanished the moment anything tried again.
func TestEveryAttemptIsKeptWithItsLog(t *testing.T) {
	f := newFixture(t)
	pool := f.pool()

	f.says = "the recipe could not find the toolchain\n"
	f.outcome = errors.New("the recipe exited 1")
	f.builder.Ensure(context.Background(), pool)
	f.run()

	f.says = "the recipe installed the toolchain\n"
	f.outcome = nil
	if _, err := f.builder.Rebuild(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	f.run()

	history, err := f.builder.History(context.Background(), "web", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("the history holds %d builds", len(history))
	}
	if history[0].Phase != model.ImageSucceeded || history[1].Phase != model.ImageFailed {
		t.Fatalf("the history reads %v, %v", history[0].Phase, history[1].Phase)
	}
	// Newest first, and the one underneath is the failure with its own log —
	// which is the whole point of keeping it.
	failed, err := f.builder.Log(context.Background(), history[1].ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(failed, "could not find the toolchain") {
		t.Fatalf("the failed build's log reads %q", failed)
	}
	if !strings.Contains(failed, "the build failed after") {
		t.Fatalf("the log does not say how it ended: %q", failed)
	}
	worked, err := f.builder.Log(context.Background(), history[0].ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(worked, "installed the toolchain") {
		t.Fatalf("the second build's log reads %q", worked)
	}
}

// Kept is not kept for ever: the logs are consoles, and a pool nobody has
// looked at in a month should not be carrying a hundred of them.
func TestOldBuildsAndTheirLogsAreForgotten(t *testing.T) {
	f := newFixture(t)
	pool := f.pool()

	f.builder.Ensure(context.Background(), pool)
	f.run()
	first, err := f.builder.History(context.Background(), "web", 0)
	if err != nil {
		t.Fatal(err)
	}
	oldest := first[0]

	for range store.ImageBuildsKept {
		if _, err := f.builder.Rebuild(context.Background(), pool); err != nil {
			t.Fatal(err)
		}
		f.run()
	}

	history, err := f.builder.History(context.Background(), "web", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != store.ImageBuildsKept {
		t.Fatalf("the history holds %d builds", len(history))
	}
	if _, err := os.Stat(oldest.Log); !os.IsNotExist(err) {
		t.Fatalf("the log of a build nobody remembers is still on the disk: %v", err)
	}
}

// A build is a process this daemon was running. One that was going when the
// daemon stopped is not going now, and a pool waiting on it would wait for
// ever.
func TestABuildInterruptedByARestartIsSettled(t *testing.T) {
	f := newFixture(t)
	pool := f.pool()

	// Queued and never run, which is what a build cut off by an upgrade looks
	// like from the database.
	f.builder.Ensure(context.Background(), pool)

	if err := f.builder.Adopt(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := f.builder.Status(pool)
	if status.State != StateFailed {
		t.Fatalf("after a restart the interrupted build reports %+v", status)
	}
	if status.Build == nil || !strings.Contains(status.Build.Error, "the daemon stopped") {
		t.Fatalf("it does not say what happened to it: %+v", status.Build)
	}
	// And it is not retried on its own either: an upgrade must not start a
	// forty-minute build nobody asked for.
	f.builder.Ensure(context.Background(), pool)
	f.run()
	if f.attempts != 0 {
		t.Fatalf("a build interrupted by a restart was retried: %d attempts", f.attempts)
	}
}

// A container pool runs an image somebody else published. There is nothing to
// build, and nothing to wait for.
func TestAContainerPoolHasNothingToBuild(t *testing.T) {
	f := newFixture(t)
	pool := model.Pool{Name: "ci", Runtime: model.RuntimeContainer, Image: "ghcr.io/x/y"}

	status := f.builder.Ensure(context.Background(), pool)
	if !status.Ready || status.State != StateNone {
		t.Fatalf("a container pool reports %+v", status)
	}
	f.run()
	if f.attempts != 0 {
		t.Fatalf("a container pool started %d image builds", f.attempts)
	}
	if _, err := f.builder.Rebuild(context.Background(), pool); err == nil {
		t.Fatal("a container pool was allowed to ask for an image build")
	}
}

// What a page shows beside the spinner: the last thing the build said, and
// whether it has said anything lately. Not "stuck" — the daemon cannot know
// that — only that nothing has been printed.
func TestABuildInProgressSaysWhatItIsDoing(t *testing.T) {
	f := newFixture(t)
	pool := f.pool()
	f.builder.Ensure(context.Background(), pool)

	next, ok := f.builder.take()
	if !ok {
		t.Fatal("nothing was queued")
	}
	// Its log, written by the build that is happening, read as a page would
	// read it while it happens.
	journal, err := f.builder.journal(next.id)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	fmt.Fprint(journal, "Get:14 http://archive.ubuntu.com noble/main amd64 gcc\n")

	build, err := f.builder.store.ImageBuild(context.Background(), next.id)
	if err != nil {
		t.Fatal(err)
	}
	build.Log = journal.Path()
	build.Phase = model.ImageRunning
	f.builder.record(context.Background(), build)

	status := f.builder.Status(pool)
	if status.State != StateBuilding {
		t.Fatalf("a build in progress reports %q", status.State)
	}
	if status.Build.Detail != "Get:14 http://archive.ubuntu.com noble/main amd64 gcc" {
		t.Fatalf("it says it is doing %q", status.Build.Detail)
	}
	if status.Build.Silent {
		t.Error("a build that has just printed something is reported as silent")
	}

	// A quarter of an hour later, with nothing more printed.
	f.builder.WithClock(func() time.Time { return time.Now().Add(Silence + time.Minute) })
	if !f.builder.Status(pool).Build.Silent {
		t.Error("a build that has printed nothing for a quarter of an hour is not reported as silent")
	}
}

// A console is CRLF and full of escape sequences, and half of what it prints
// is a progress bar redrawing itself.
func TestConsoleLinesAreMadeReadable(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain\r", "plain"},
		{"\x1b[0;32mgreen\x1b[0m text", "green text"},
		{"tabs\tbecome\tspaces", "tabs become spaces"},
	} {
		if got := clean(tc.in); got != tc.want {
			t.Errorf("clean(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A pool that is deleted takes its builds with it. Nothing can reach them
// afterwards — the history is filed under the pool — and each log is a console
// worth megabytes.
func TestADeletedPoolsBuildsAreForgotten(t *testing.T) {
	f := newFixture(t)
	pool := f.pool()

	f.builder.Ensure(context.Background(), pool)
	f.run()
	history, err := f.builder.History(context.Background(), "web", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("the history holds %d builds", len(history))
	}

	if err := f.builder.Forget(context.Background(), "web"); err != nil {
		t.Fatal(err)
	}
	if left, err := f.builder.History(context.Background(), "web", 0); err != nil || len(left) != 0 {
		t.Fatalf("the pool's builds are still there: %+v (%v)", left, err)
	}
	if _, err := os.Stat(history[0].Log); !os.IsNotExist(err) {
		t.Fatalf("its log is still on the disk: %v", err)
	}
}

// What the runners left behind when they built their own images: a file per
// image holding only the latest attempt, and one console shared by every build
// on the host. Nothing writes or reads either any more.
func TestWhatTheOldArrangementLeftBehindIsCleanedUp(t *testing.T) {
	f := newFixture(t)
	stale := filepath.Join(f.imagesDir, "builds")
	console := filepath.Join(f.imagesDir, "last-build-console.log")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "img.json"), []byte(`{"pool":"web"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(console, []byte("a console from a previous version"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := f.builder.Adopt(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{stale, console} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v", path, err)
		}
	}
}
