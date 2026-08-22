package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
)

func TestJobsReportsWhatEachPoolHasRun(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	if err := h.store.RecordJobs(context.Background(), now, []model.JobSample{
		{Pool: "web", Started: 3, BusySeconds: 900},
		{Pool: "api", Started: 1, BusySeconds: 120},
	}); err != nil {
		t.Fatal(err)
	}

	payload := readAll(t, h.do("GET", "/api/jobs?days=7", nil))
	for _, want := range []string{`"pool":"web"`, `"jobs":3`, `"seconds":900`, `"days"`, `"since"`, `"until"`} {
		if !strings.Contains(payload, want) {
			t.Errorf("the response is missing %q: %s", want, payload)
		}
	}
}

// The hungriest pool first: the order somebody scanning for what to resize is
// reading in, rather than the order the pools happen to be named in.
func TestJobsPutsTheBiggestConsumerFirst(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	if err := h.store.RecordJobs(context.Background(), now, []model.JobSample{
		{Pool: "api", Started: 1, BusySeconds: 120},
		{Pool: "web", Started: 3, BusySeconds: 900},
	}); err != nil {
		t.Fatal(err)
	}

	payload := readAll(t, h.do("GET", "/api/jobs", nil))
	pools, _, _ := strings.Cut(payload, `"days"`)
	if strings.Index(pools, `"web"`) > strings.Index(pools, `"api"`) {
		t.Fatalf("the pool that cost the most is not first: %s", payload)
	}
}

// The tally is added up across the window, not reported a pass at a time.
func TestJobsAddsTheWindowUpPerPool(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, back := range []int{0, 1, 2} {
		if err := h.store.RecordJobs(ctx, now.AddDate(0, 0, -back),
			[]model.JobSample{{Pool: "web", Started: 2, BusySeconds: 60}}); err != nil {
			t.Fatal(err)
		}
	}

	payload := readAll(t, h.do("GET", "/api/jobs?days=7", nil))
	if !strings.Contains(payload, `"jobs":6`) || !strings.Contains(payload, `"seconds":180`) {
		t.Fatalf("want the three days added up: %s", payload)
	}
}

func TestJobsRejectsAWindowItCannotAnswer(t *testing.T) {
	h := newHarness(t)
	for _, query := range []string{"?days=0", "?days=91", "?days=nonsense"} {
		resp := h.do("GET", "/api/jobs"+query, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400", query, resp.StatusCode)
		}
	}
}

// A window may reach exactly as far back as the daemon still keeps, and no
// further: the two are the same number so they cannot drift apart.
func TestJobsReachesAsFarBackAsTheDaemonKeeps(t *testing.T) {
	h := newHarness(t)
	resp := h.do("GET", "/api/jobs?days=90", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the full retention was refused: %d", resp.StatusCode)
	}
}

func TestJobsDefaultsToAWindowWorthSizingFrom(t *testing.T) {
	h := newHarness(t)
	payload := readAll(t, h.do("GET", "/api/jobs", nil))
	if !strings.Contains(payload, `"pools"`) {
		t.Fatalf("got %s", payload)
	}
}

// A fleet that has run nothing answers with empty lists rather than nulls: the
// UI draws "nothing yet", and a null would be an error it has to guess about.
func TestJobsOnAFleetThatHasRunNothing(t *testing.T) {
	h := newHarness(t)
	payload := readAll(t, h.do("GET", "/api/jobs", nil))
	if !strings.Contains(payload, `"pools":[]`) || !strings.Contains(payload, `"days":[]`) {
		t.Fatalf("want empty lists, got %s", payload)
	}
}
