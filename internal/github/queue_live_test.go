package github

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
)

// The queue reader, against real GitHub. What a fake cannot answer here is
// whether the two listings and the jobs endpoint are shaped the way this
// believes — and that answer decides whether a sleeping pool ever wakes up.
//
// It cannot assert a number: what is queued on somebody's repository at the
// moment this runs is not a fixture. What it can assert is that the call
// completes against the real endpoints, comes back with something sane, and
// costs what a sleeping pool is allowed to cost.
func TestLiveQueuedJobsReadsTheRealQueue(t *testing.T) {
	client, scope := live(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	labels := []string{"self-hosted", "linux", "x64", "vm", "ephemeral"}
	got, err := client.QueuedJobs(ctx, scope, labels, 8)
	if err != nil {
		t.Fatal(err)
	}
	if got < 0 || got > 8 {
		t.Fatalf("counted %d, which is outside the limit it was given", got)
	}

	// A pool that could take nothing is a pool that never wakes, and the
	// arithmetic that produces the label set is somewhere else entirely. Asked
	// with labels no job would ever request, the answer has to be zero rather
	// than everything.
	none, err := client.QueuedJobs(ctx, scope, []string{"no-such-label-ndj28f"}, 8)
	if err != nil {
		t.Fatal(err)
	}
	if none != 0 {
		t.Fatalf("counted %d jobs for a pool whose labels nothing asks for", none)
	}
}

// The refusal an organisation pool gets, from the real client rather than from
// a fake that was told to refuse. Nothing is sent: the reason is structural.
func TestLiveAnOrganisationQueueIsRefusedNotEmpty(t *testing.T) {
	client, _ := live(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := client.QueuedJobs(ctx,
		Scope{Kind: model.ScopeOrganization, Path: "github"}, []string{"vm"}, 8)
	if !errors.Is(err, ErrNoQueueForOrganisations) {
		t.Fatalf("refused with %v", err)
	}
}

// The field names the whole feature turns on, read off a real run.
//
// QueuedJobs on a quiet repository never reaches the jobs endpoint — it
// answers from two empty listings — so the test above can pass while
// "status" and "labels" are both wrong. This reads the jobs of a run that
// really happened, which is the only way to see those two fields arrive
// without dispatching a workflow on somebody's repository.
func TestLiveAJobSaysItsStatusAndItsLabels(t *testing.T) {
	client, scope := live(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	owner, repo, _ := strings.Cut(scope.Path, "/")
	base := "/repos/" + owner + "/" + repo + "/actions/runs"

	var runs struct {
		Runs []struct {
			ID int64 `json:"id"`
		} `json:"workflow_runs"`
	}
	if err := client.do(ctx, http.MethodGet, base+"?per_page=5", &runs, scope); err != nil {
		t.Fatal(err)
	}
	if len(runs.Runs) == 0 {
		t.Skip("this repository has never run a workflow, so there are no jobs to read")
	}

	for _, run := range runs.Runs {
		jobs, err := client.jobsOf(ctx, scope, base, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) == 0 {
			continue
		}
		for _, job := range jobs {
			// The value the count is compared against. If GitHub called this
			// anything else, every job would read as queued and a sleeping
			// pool would wake for ever.
			switch job.Status {
			case "queued", "in_progress", "completed", "waiting", "pending":
			default:
				t.Fatalf("a job's status is %q, which is not a status this knows", job.Status)
			}
			if len(job.Labels) == 0 {
				t.Fatal("a job named no labels; nothing could ever be matched to a pool")
			}
		}
		return
	}
	t.Skip("none of the recent runs has any jobs left to read")
}
