package store

import (
	"context"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
)

func startBuild(t *testing.T, s *Store, pool, image string) model.ImageBuild {
	t.Helper()
	build, err := s.StartImageBuild(context.Background(), model.ImageBuild{
		Pool: pool, Image: image, Phase: model.ImageQueued,
		Trigger: model.ImageAutomatic, StartedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return build
}

func finishBuild(t *testing.T, s *Store, build model.ImageBuild, phase model.ImagePhase, log string) {
	t.Helper()
	ended := time.Now()
	build.Phase, build.EndedAt, build.Log = phase, &ended, log
	if err := s.UpdateImageBuild(context.Background(), build); err != nil {
		t.Fatal(err)
	}
}

// The history of a pool's image is per attempt, newest first. It used to be
// one file per image, replaced by whatever tried next.
func TestImageBuildsAreKeptPerAttempt(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	first := startBuild(t, s, "web", "image-a")
	finishBuild(t, s, first, model.ImageFailed, "/var/log/1.log")
	second := startBuild(t, s, "web", "image-b")
	finishBuild(t, s, second, model.ImageSucceeded, "/var/log/2.log")
	other := startBuild(t, s, "api", "image-c")
	finishBuild(t, s, other, model.ImageSucceeded, "/var/log/3.log")

	history, err := s.ImageBuilds(ctx, "web", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("web has %d builds", len(history))
	}
	if history[0].ID != second.ID || history[1].ID != first.ID {
		t.Fatalf("the history is in the wrong order: %v, %v", history[0].ID, history[1].ID)
	}
	if history[1].Phase != model.ImageFailed || history[1].Log != "/var/log/1.log" {
		t.Fatalf("the failure was not kept whole: %+v", history[1])
	}
}

// What decides whether another build may start is the newest attempt at that
// image — by image, because the name is a hash of what it is built from and
// two pools asking for the same thing are asking about the same history.
func TestTheLatestAttemptAtEachImageIsFound(t *testing.T) {
	s := newStore(t)

	first := startBuild(t, s, "web", "image-a")
	finishBuild(t, s, first, model.ImageFailed, "")
	second := startBuild(t, s, "web", "image-a")
	finishBuild(t, s, second, model.ImageSucceeded, "")

	latest, err := s.LatestImageBuilds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(latest) != 1 {
		t.Fatalf("got %d images", len(latest))
	}
	if got := latest["image-a"]; got.ID != second.ID || got.Phase != model.ImageSucceeded {
		t.Fatalf("got %+v", got)
	}
}

// A build that was running when the daemon stopped is not running now.
func TestUnfinishedBuildsAreSettled(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	done := startBuild(t, s, "web", "image-a")
	finishBuild(t, s, done, model.ImageSucceeded, "")
	interrupted := startBuild(t, s, "web", "image-b")

	stale, err := s.AbandonImageBuilds(ctx, time.Now(), "the daemon stopped")
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].ID != interrupted.ID {
		t.Fatalf("settled %+v", stale)
	}

	read, err := s.ImageBuild(ctx, interrupted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if read.Phase != model.ImageFailed || read.Error != "the daemon stopped" || read.EndedAt == nil {
		t.Fatalf("got %+v", read)
	}
}

// Kept per pool, so a pool that has been rebuilt fifty times does not push
// another pool's only build out of the history.
func TestPruningKeepsTheNewestOfEachPool(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	var web []model.ImageBuild
	for i := range 4 {
		build := startBuild(t, s, "web", "image-a")
		finishBuild(t, s, build, model.ImageSucceeded, "/var/log/web-"+string(rune('0'+i))+".log")
		web = append(web, build)
	}
	api := startBuild(t, s, "api", "image-b")
	finishBuild(t, s, api, model.ImageSucceeded, "/var/log/api.log")

	logs, err := s.PruneImageBuilds(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 2 {
		t.Fatalf("pruning offered %v to delete", logs)
	}

	kept, err := s.ImageBuilds(ctx, "web", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 2 || kept[0].ID != web[3].ID || kept[1].ID != web[2].ID {
		t.Fatalf("web kept %+v", kept)
	}
	// The other pool's single build is untouched, which is the whole reason
	// this is per pool.
	if others, err := s.ImageBuilds(ctx, "api", 0); err != nil || len(others) != 1 {
		t.Fatalf("api kept %+v (%v)", others, err)
	}
}
