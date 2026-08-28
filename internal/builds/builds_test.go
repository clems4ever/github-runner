package builds

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/agent"
)

func newReader(t *testing.T, now time.Time) (*Reader, string) {
	t.Helper()
	dir := t.TempDir()
	r := New(dir)
	r.now = func() time.Time { return now }
	return r, dir
}

func write(t *testing.T, imagesDir string, record agent.BuildRecord) {
	t.Helper()
	if err := os.MkdirAll(agent.BuildsDir(imagesDir), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(agent.BuildsDir(imagesDir), record.Image+".json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func console(t *testing.T, imagesDir, text string, at time.Time) {
	t.Helper()
	dir := filepath.Join(imagesDir, "build")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "console.log")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// A host that has never built anything is not a host with a problem, and
// nothing here may fail on the ordinary case of an empty directory.
func TestAHostThatHasBuiltNothing(t *testing.T) {
	r, _ := newReader(t, time.Now())
	found, err := r.List()
	if err != nil {
		t.Fatalf("a fresh host reported an error: %v", err)
	}
	if len(found) != 0 {
		t.Fatalf("got %d builds from an empty host", len(found))
	}
}

// The panel exists to answer "what is it doing", so a running build has to
// come back with the answer rather than with a spinner and a hash.
func TestARunningBuildSaysWhatItIsDoing(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	r, dir := newReader(t, now)

	write(t, dir, agent.BuildRecord{
		Image: "runner-fleet-noble-abc123", Pool: "web", Runner: "web-1",
		Phase: agent.BuildRunning, StartedAt: now.Add(-4*time.Minute - 12*time.Second),
	})
	console(t, dir, "cloud-init running\r\nrunning this pool's recipe\r\n", now.Add(-3*time.Second))

	found, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("got %d builds", len(found))
	}
	build := found[0]
	if build.Detail != "running this pool's recipe" {
		t.Errorf("the build says %q, which is not the last thing its console said", build.Detail)
	}
	if build.Seconds != 252 {
		t.Errorf("it has been running for %d seconds, want 252", build.Seconds)
	}
	if build.Silent {
		t.Error("a build that printed three seconds ago was reported as having gone quiet")
	}
	if build.Pool != "web" || build.Runner != "web-1" {
		t.Errorf("got %+v", build)
	}
}

// A build whose agent was killed leaves a record saying "running" for ever.
// The daemon cannot know it is dead — but it can say that nothing has happened
// for a quarter of an hour, which is the honest version and is enough to stop
// somebody waiting on it.
func TestABuildThatHasGoneQuiet(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	r, dir := newReader(t, now)

	write(t, dir, agent.BuildRecord{
		Image: "img", Pool: "web", Runner: "web-1",
		Phase: agent.BuildRunning, StartedAt: now.Add(-40 * time.Minute),
	})
	console(t, dir, "the last thing it ever said\n", now.Add(-Silence-time.Minute))

	found, _ := r.List()
	if len(found) != 1 || !found[0].Silent {
		t.Fatalf("a build silent for %s was not reported as such: %+v", Silence, found)
	}
	// And it still says what the last thing was, which is the most useful
	// sentence there is about a build that stopped.
	if found[0].Detail != "the last thing it ever said" {
		t.Errorf("detail is %q", found[0].Detail)
	}
}

// The first build on a host spends minutes downloading with no machine booted
// and so no console at all. Reporting nothing there is reporting nothing for
// the longest part of the longest build anybody will ever wait through.
func TestADownloadReportsItsSize(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	r, dir := newReader(t, now)

	write(t, dir, agent.BuildRecord{
		Image: "img", Pool: "web", Runner: "web-1",
		Phase: agent.BuildFetching, StartedAt: now.Add(-90 * time.Second),
	})
	partial := filepath.Join(dir, "cloud-"+agent.UbuntuRelease+".img")
	if err := os.WriteFile(partial, make([]byte, 3<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(partial, now, now); err != nil {
		t.Fatal(err)
	}

	found, _ := r.List()
	if len(found) != 1 {
		t.Fatalf("got %d builds", len(found))
	}
	if !strings.Contains(found[0].Detail, "3 MB") {
		t.Errorf("a download in progress says %q", found[0].Detail)
	}
}

// The retention rule, and the whole of it: the newest build per pool. A
// failure stays on the page until the build that fixes it replaces it, and a
// pool that has been fixed stops being reported as broken without anybody
// clearing anything.
func TestAPoolShowsWhatHappenedToItLast(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	r, dir := newReader(t, now)

	failedAt := now.Add(-30 * time.Minute)
	write(t, dir, agent.BuildRecord{
		Image: "old-recipe", Pool: "web", Runner: "web-1", Phase: agent.BuildFailed,
		Error: "the recipe exited 1", StartedAt: now.Add(-40 * time.Minute), EndedAt: &failedAt,
	})

	found, _ := r.List()
	if len(found) != 1 || found[0].Phase != agent.BuildFailed {
		t.Fatalf("the failure is not being reported: %+v", found)
	}

	// The recipe is fixed, which is a different image and so a different
	// record. The failure must not survive it.
	fixedAt := now.Add(-2 * time.Minute)
	write(t, dir, agent.BuildRecord{
		Image: "new-recipe", Pool: "web", Runner: "web-2", Phase: agent.BuildDone,
		StartedAt: now.Add(-8 * time.Minute), EndedAt: &fixedAt,
	})

	found, _ = r.List()
	if len(found) != 1 {
		t.Fatalf("a pool reported %d builds; it has one image at a time", len(found))
	}
	if found[0].Phase != agent.BuildDone || found[0].Image != "new-recipe" {
		t.Fatalf("the fixed pool still reports %+v", found[0])
	}
	if found[0].Seconds != 360 {
		t.Errorf("the build took %d seconds, want 360", found[0].Seconds)
	}
}

// A build that worked is worth seeing while whoever changed the recipe is
// still watching, and is noise the next morning.
func TestASuccessAgesOut(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	r, dir := newReader(t, now)

	yesterday := now.Add(-24 * time.Hour)
	write(t, dir, agent.BuildRecord{
		Image: "img", Pool: "web", Runner: "web-1", Phase: agent.BuildDone,
		StartedAt: yesterday.Add(-6 * time.Minute), EndedAt: &yesterday,
	})

	found, _ := r.List()
	if len(found) != 0 {
		t.Fatalf("yesterday's successful build is still on the page: %+v", found)
	}
}

// A failure never ages out, because it is still true. This is the case the
// panel exists for: the pool has been at zero runners since last night and the
// reason has to still be there in the morning.
func TestAFailureDoesNotAgeOut(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	r, dir := newReader(t, now)

	yesterday := now.Add(-24 * time.Hour)
	write(t, dir, agent.BuildRecord{
		Image: "img", Pool: "web", Runner: "web-1", Phase: agent.BuildFailed,
		Error: "the recipe exited 1", StartedAt: yesterday.Add(-6 * time.Minute), EndedAt: &yesterday,
	})

	found, _ := r.List()
	if len(found) != 1 {
		t.Fatalf("last night's failure is gone, and the pool is still empty because of it")
	}
}

// Whatever is unfinished is what somebody is waiting on, so it goes first.
func TestWhatIsStillRunningComesFirst(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	r, dir := newReader(t, now)

	ended := now.Add(-time.Minute)
	write(t, dir, agent.BuildRecord{
		Image: "a", Pool: "alpha", Runner: "alpha-1", Phase: agent.BuildDone,
		StartedAt: now.Add(-5 * time.Minute), EndedAt: &ended,
	})
	write(t, dir, agent.BuildRecord{
		Image: "b", Pool: "beta", Runner: "beta-1", Phase: agent.BuildRunning,
		StartedAt: now.Add(-2 * time.Minute),
	})

	found, _ := r.List()
	if len(found) != 2 {
		t.Fatalf("got %d builds", len(found))
	}
	if found[0].Pool != "beta" {
		t.Errorf("the finished build is above the one still running: %v, %v", found[0].Pool, found[1].Pool)
	}
}

// A record that is not a record must cost one line of the page rather than the
// page. Half a file is what a reader sees when a writer is interrupted, and
// this directory is written by processes that are killed by design.
func TestAnUnreadableRecordDoesNotTakeTheRestWithIt(t *testing.T) {
	now := time.Now()
	r, dir := newReader(t, now)

	write(t, dir, agent.BuildRecord{
		Image: "good", Pool: "web", Runner: "web-1", Phase: agent.BuildRunning, StartedAt: now,
	})
	if err := os.WriteFile(filepath.Join(agent.BuildsDir(dir), "half.json"), []byte(`{"pool":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agent.BuildsDir(dir), "notjson.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	found, err := r.List()
	if err != nil {
		t.Fatalf("half a file failed the whole read: %v", err)
	}
	if len(found) != 1 || found[0].Pool != "web" {
		t.Fatalf("got %+v", found)
	}
}

// A serial console is CRLF and carries whatever escape sequences the thing
// drawing on it used. Put on the page raw, a progress bar's worth of escapes
// is what the panel would show.
func TestTheConsoleLineIsMadePrintable(t *testing.T) {
	now := time.Now()
	r, dir := newReader(t, now)
	write(t, dir, agent.BuildRecord{
		Image: "img", Pool: "web", Runner: "web-1", Phase: agent.BuildRunning, StartedAt: now,
	})

	console(t, dir, "earlier\r\n\x1b[32mSetting up nftables (1.0.9-1build1) ...\x1b[0m\r\n\r\n", now)

	found, _ := r.List()
	if found[0].Detail != "Setting up nftables (1.0.9-1build1) ..." {
		t.Fatalf("the console line came back as %q", found[0].Detail)
	}
}

// The last line of a console that is megabytes long, without reading megabytes.
func TestOnlyTheEndOfALongConsoleIsRead(t *testing.T) {
	now := time.Now()
	r, dir := newReader(t, now)
	write(t, dir, agent.BuildRecord{
		Image: "img", Pool: "web", Runner: "web-1", Phase: agent.BuildRunning, StartedAt: now,
	})

	console(t, dir, strings.Repeat("boot message\n", 20000)+"the last word\n", now)

	found, _ := r.List()
	if found[0].Detail != "the last word" {
		t.Fatalf("got %q", found[0].Detail)
	}
}

// A finished build has nothing happening, so nothing to ask the host about —
// and the console it names is the one that was kept, not the live one.
func TestAFinishedBuildIsNotDescribedFromALiveConsole(t *testing.T) {
	now := time.Now()
	r, dir := newReader(t, now)
	ended := now.Add(-time.Minute)
	write(t, dir, agent.BuildRecord{
		Image: "img", Pool: "web", Runner: "web-1", Phase: agent.BuildFailed,
		Error: "the recipe exited 1", Console: "/var/lib/runner-fleet/images/last-build-console.log",
		StartedAt: now.Add(-6 * time.Minute), EndedAt: &ended,
	})
	// Left behind by some other build entirely.
	console(t, dir, "something else is building\n", now)

	found, _ := r.List()
	if found[0].Detail != "" {
		t.Errorf("a finished build borrowed a live console: %q", found[0].Detail)
	}
	if found[0].Console != "/var/lib/runner-fleet/images/last-build-console.log" {
		t.Errorf("the kept console is %q", found[0].Console)
	}
}
