package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// repository is a fake with runs in it, answering the two listings and the
// jobs of each run.
type repository struct {
	// queued and inProgress are run IDs, by the status GitHub would list them
	// under.
	queued, inProgress []int64
	// jobs is what each run holds: a status and the labels its runs-on asked
	// for.
	jobs map[int64][]job
	// asked counts requests, which is the cost this is supposed to bound.
	asked int
}

type job struct {
	status string
	labels []string
}

func (rep *repository) serve(w http.ResponseWriter, r *http.Request) {
	rep.asked++
	switch {
	case strings.HasSuffix(r.URL.Path, "/actions/runs"):
		ids := rep.queued
		if r.URL.Query().Get("status") == "in_progress" {
			ids = rep.inProgress
		}
		var runs []string
		for _, id := range ids {
			runs = append(runs, fmt.Sprintf(`{"id":%d}`, id))
		}
		fmt.Fprintf(w, `{"total_count":%d,"workflow_runs":[%s]}`, len(runs), strings.Join(runs, ","))
	case strings.HasSuffix(r.URL.Path, "/jobs"):
		var id int64
		fmt.Sscanf(r.URL.Path[strings.LastIndex(r.URL.Path, "/runs/")+len("/runs/"):], "%d", &id)
		var out []string
		for _, j := range rep.jobs[id] {
			labels := make([]string, 0, len(j.labels))
			for _, label := range j.labels {
				labels = append(labels, `"`+label+`"`)
			}
			out = append(out, fmt.Sprintf(`{"status":%q,"labels":[%s]}`, j.status, strings.Join(labels, ",")))
		}
		fmt.Fprintf(w, `{"total_count":%d,"jobs":[%s]}`, len(out), strings.Join(out, ","))
	default:
		http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
	}
}

func vmLabels() []string { return []string{"self-hosted", "linux", "x64", "vm", "ephemeral"} }

// The point of the whole file: a pool with nothing running finds out that
// something is waiting for it.
func TestQueuedJobsCountsWhatThisPoolCouldTake(t *testing.T) {
	rep := &repository{
		queued: []int64{1},
		jobs: map[int64][]job{
			1: {
				{status: "queued", labels: []string{"self-hosted", "vm"}},
				{status: "queued", labels: []string{"self-hosted", "vm"}},
			},
		},
	}
	client, _ := newClient(t, rep.serve)

	got, err := client.QueuedJobs(context.Background(), repoScope(), vmLabels(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("counted %d, want the two jobs waiting", got)
	}
}

// A job asking for labels this pool does not have belongs to some other pool,
// or to GitHub's own runners. Waking for it would boot a machine that then sat
// there while the job waited for somebody else.
func TestQueuedJobsIgnoresJobsForSomebodyElse(t *testing.T) {
	rep := &repository{
		queued: []int64{1},
		jobs: map[int64][]job{
			1: {
				{status: "queued", labels: []string{"ubuntu-latest"}},
				{status: "queued", labels: []string{"self-hosted", "container"}},
				{status: "queued", labels: []string{"self-hosted", "vm", "gpu"}},
			},
		},
	}
	client, _ := newClient(t, rep.serve)

	got, err := client.QueuedJobs(context.Background(), repoScope(), vmLabels(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("counted %d jobs that were not for this pool", got)
	}
}

// A job already running is not waiting for anything. Counting it would keep a
// pool awake for as long as its own jobs took.
func TestQueuedJobsCountsOnlyWhatIsStillWaiting(t *testing.T) {
	rep := &repository{
		inProgress: []int64{7},
		jobs: map[int64][]job{
			7: {
				{status: "in_progress", labels: []string{"self-hosted", "vm"}},
				{status: "completed", labels: []string{"self-hosted", "vm"}},
				{status: "queued", labels: []string{"self-hosted", "vm"}},
			},
		},
	}
	client, _ := newClient(t, rep.serve)

	got, err := client.QueuedJobs(context.Background(), repoScope(), vmLabels(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("counted %d, want only the job still waiting", got)
	}
}

// A matrix leg waiting for a runner sits inside a run that is already in
// progress. A pool that only read queued runs would sleep through it — which
// is the failure that would look like the fleet ignoring half a workflow.
func TestQueuedJobsLooksInsideRunsAlreadyStarted(t *testing.T) {
	rep := &repository{
		inProgress: []int64{7},
		jobs:       map[int64][]job{7: {{status: "queued", labels: []string{"self-hosted", "vm"}}}},
	}
	client, _ := newClient(t, rep.serve)

	got, err := client.QueuedJobs(context.Background(), repoScope(), vmLabels(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("counted %d, want the queued leg of a running workflow", got)
	}
}

// A quiet repository is the state a sleeping pool is in nearly all the time,
// and it has to be cheap: the two listings and nothing else.
func TestQueuedJobsCostsTwoRequestsOnAQuietRepository(t *testing.T) {
	rep := &repository{}
	client, _ := newClient(t, rep.serve)

	got, err := client.QueuedJobs(context.Background(), repoScope(), vmLabels(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("counted %d on a repository with nothing running", got)
	}
	if rep.asked != 2 {
		t.Fatalf("made %d requests to find out that nothing is waiting", rep.asked)
	}
}

// The count is only ever compared against how many runners the pool may start,
// so counting past that is requests spent on a number nobody reads.
func TestQueuedJobsStopsAtTheLimit(t *testing.T) {
	rep := &repository{queued: []int64{1, 2, 3, 4, 5}, jobs: map[int64][]job{}}
	for id := int64(1); id <= 5; id++ {
		rep.jobs[id] = []job{{status: "queued", labels: []string{"self-hosted", "vm"}}}
	}
	client, _ := newClient(t, rep.serve)

	got, err := client.QueuedJobs(context.Background(), repoScope(), vmLabels(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("counted %d, want it to stop at the limit", got)
	}
	// Two listings and the first two runs' jobs. Reading all five would have
	// been seven requests for the same answer.
	if rep.asked != 4 {
		t.Fatalf("made %d requests, want it to have stopped once it had its answer", rep.asked)
	}
}

// Why an organisation pool cannot sleep, said as a refusal rather than as a
// zero. A zero would read as "nothing is queued" and the pool would stay down
// through everything the organisation ran.
func TestQueuedJobsRefusesAnOrganisation(t *testing.T) {
	rep := &repository{}
	client, _ := newClient(t, rep.serve)

	_, err := client.QueuedJobs(context.Background(), orgScope(), vmLabels(), 10)
	if !errors.Is(err, ErrNoQueueForOrganisations) {
		t.Fatalf("refused with %v, want the reason an organisation cannot be asked", err)
	}
	if rep.asked != 0 {
		t.Fatal("asked GitHub anyway")
	}
}

func TestServes(t *testing.T) {
	for _, c := range []struct {
		name   string
		runner []string
		job    []string
		want   bool
	}{
		// The labels GitHub gives every self-hosted Linux runner without
		// anybody configuring them. A pool that matched literally would decide
		// that runs-on: [self-hosted, vm] was not for it.
		{"implicit labels count", []string{"vm"}, []string{"self-hosted", "linux", "vm"}, true},
		{"a runner may have more", vmLabels(), []string{"self-hosted"}, true},
		{"but not fewer", vmLabels(), []string{"self-hosted", "vm", "gpu"}, false},
		{"case is not significant", []string{"GPU"}, []string{"gpu"}, true},
		{"a job asking for nothing is nobody's", vmLabels(), nil, false},
	} {
		if got := Serves(c.runner, c.job); got != c.want {
			t.Errorf("%s: Serves(%v, %v) = %v", c.name, c.runner, c.job, got)
		}
	}
}
