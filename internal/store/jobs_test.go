package store

import (
	"context"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
)

func TestJobsAreAddedUpPerPoolPerDay(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	day := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)

	// Three passes over one morning, the way the reconciler writes them.
	for _, pass := range []struct {
		at      time.Time
		samples []model.JobSample
	}{
		{day, []model.JobSample{{Pool: "web", Started: 1, BusySeconds: 30}}},
		{day.Add(time.Minute), []model.JobSample{
			{Pool: "web", Started: 0, BusySeconds: 30},
			{Pool: "api", Started: 2, BusySeconds: 60},
		}},
		{day.Add(2 * time.Minute), []model.JobSample{{Pool: "web", Started: 1, BusySeconds: 15}}},
	} {
		if err := s.RecordJobs(ctx, pass.at, pass.samples); err != nil {
			t.Fatal(err)
		}
	}

	days, err := s.JobHistory(ctx, day.AddDate(0, 0, -1), day)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("want one row per pool for the one day, got %+v", days)
	}

	byPool := map[string]model.JobDay{}
	for _, entry := range days {
		byPool[entry.Pool] = entry
	}
	if got := byPool["web"]; got.Jobs != 2 || got.Seconds != 75 || got.Day != "2026-03-04" {
		t.Fatalf("web: %+v", got)
	}
	if got := byPool["api"]; got.Jobs != 2 || got.Seconds != 60 {
		t.Fatalf("api: %+v", got)
	}
}

// A day is a day. Two passes either side of midnight belong to different days
// even though they are a minute apart, because a tally read per day that put
// them together would be reporting a day that never happened.
func TestJobsAreKeptPerUTCDay(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	midnight := time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)

	if err := s.RecordJobs(ctx, midnight.Add(-time.Minute),
		[]model.JobSample{{Pool: "web", Started: 1, BusySeconds: 30}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordJobs(ctx, midnight,
		[]model.JobSample{{Pool: "web", Started: 1, BusySeconds: 30}}); err != nil {
		t.Fatal(err)
	}

	days, err := s.JobHistory(ctx, midnight.AddDate(0, 0, -1), midnight)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("want the two days kept apart, got %+v", days)
	}
	if days[0].Day != "2026-03-04" || days[1].Day != "2026-03-05" {
		t.Fatalf("want them in order, got %+v", days)
	}
}

// An idle fleet is passed over every thirty seconds for ever. Writing a row of
// zeroes each time would be a great deal of writing to record that nothing
// happened, and a history full of days a pool did nothing to read back.
func TestAPoolThatDidNothingIsNotWrittenDown(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	day := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)

	if err := s.RecordJobs(ctx, day, []model.JobSample{
		{Pool: "web", Started: 0, BusySeconds: 0},
		{Pool: "api", Started: 0, BusySeconds: 45},
	}); err != nil {
		t.Fatal(err)
	}

	days, err := s.JobHistory(ctx, day, day)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 1 || days[0].Pool != "api" {
		t.Fatalf("want only the pool that did something, got %+v", days)
	}
}

// A pool can be busy without a job starting: the reconciler sees a job in
// flight on the pass after the one that counted it, and every pass after that.
func TestTimeIsRecordedWithoutAJobStarting(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	day := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)

	if err := s.RecordJobs(ctx, day, []model.JobSample{{Pool: "web", BusySeconds: 30}}); err != nil {
		t.Fatal(err)
	}
	days, err := s.JobHistory(ctx, day, day)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 1 || days[0].Jobs != 0 || days[0].Seconds != 30 {
		t.Fatalf("want time with no job started, got %+v", days)
	}
}

func TestJobsOlderThanTheRetentionAreDropped(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-JobRetention - 48*time.Hour)

	if err := s.RecordJobs(ctx, old, []model.JobSample{{Pool: "web", Started: 9, BusySeconds: 900}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordJobs(ctx, now, []model.JobSample{{Pool: "web", Started: 1, BusySeconds: 30}}); err != nil {
		t.Fatal(err)
	}

	days, err := s.JobHistory(ctx, old.AddDate(0, 0, -1), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 1 || days[0].Seconds != 30 {
		t.Fatalf("the old tally survived its retention: %+v", days)
	}
}

// The prune runs whether or not anything was written, so a fleet that goes
// quiet for a quarter still stops carrying the quarter before it.
func TestAQuietFleetStillLetsGoOfWhatHasAgedOut(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	if err := s.RecordJobs(ctx, now.Add(-JobRetention-48*time.Hour),
		[]model.JobSample{{Pool: "web", Started: 9, BusySeconds: 900}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordJobs(ctx, now, []model.JobSample{{Pool: "web"}}); err != nil {
		t.Fatal(err)
	}

	days, err := s.JobHistory(ctx, now.AddDate(0, 0, -365), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 0 {
		t.Fatalf("nothing was written, and nothing was let go of either: %+v", days)
	}
}

func TestJobHistoryIsBoundedByItsWindow(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

	for _, back := range []int{0, 3, 10} {
		if err := s.RecordJobs(ctx, now.AddDate(0, 0, -back),
			[]model.JobSample{{Pool: "web", Started: 1, BusySeconds: 60}}); err != nil {
			t.Fatal(err)
		}
	}

	days, err := s.JobHistory(ctx, now.AddDate(0, 0, -6), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("want the two days inside the window, got %+v", days)
	}
}
