package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/clems4ever/github-runner/internal/model"
)

// A pool that is allowed to sleep has a problem the autoscaler cannot solve on
// its own: with nothing running there is nothing to observe, so "every runner
// is busy" — the signal the fleet grows on — never fires. Something has to
// notice a job waiting for a runner that does not exist yet.
//
// GitHub does not offer "how many jobs are queued for these labels". What it
// offers is the runs, and the jobs within them, so that is what this asks:
// which runs are queued or in progress, and of their jobs, which are still
// waiting and ask only for labels this pool has.
//
// The cost is bounded where it matters. A quiet repository answers in two
// requests — both listings come back with total_count zero — and a quiet
// repository is exactly when a pool is asleep. A busy one costs a request per
// unfinished run, and a busy one has runners up, which is when this is not
// asked at all.

// ErrNoQueueForOrganisations is why an organisation-scoped pool cannot sleep.
//
// GitHub lists runs per repository. An organisation's queue would mean
// enumerating every repository in it on every pass, which is not a poll — it
// is a crawl, and it would spend somebody's whole rate limit to find out that
// nothing is waiting.
var ErrNoQueueForOrganisations = errors.New(
	"GitHub lists queued jobs per repository, so an organisation's queue cannot be read")

// runsPerPage is how many unfinished runs are looked at. A repository with
// more than fifty runs in flight is not one that needs waking up.
const runsPerPage = 50

// QueuedJobs is how many jobs are waiting for a runner this pool could take.
//
// labels is what the pool's runners register with. limit stops the count
// early: the caller only ever needs "how many, up to the most it could start",
// and a repository with two hundred queued jobs must not cost two hundred
// requests to say so.
func (c *Client) QueuedJobs(ctx context.Context, scope Scope, labels []string, limit int) (int, error) {
	if scope.Kind != model.ScopeRepository {
		return 0, ErrNoQueueForOrganisations
	}
	owner, repo, ok := strings.Cut(scope.Path, "/")
	if !ok {
		return 0, fmt.Errorf("%q is not owner/repository", scope.Path)
	}
	if limit <= 0 {
		return 0, nil
	}
	base := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/actions/runs"

	// Both, because a queued job is not only found in a queued run: a matrix
	// leg waiting for a runner sits inside a run that is already in progress,
	// and a pool that only looked at queued runs would sleep through it.
	var unfinished []int64
	for _, status := range []string{"queued", "in_progress"} {
		var runs struct {
			TotalCount int `json:"total_count"`
			Runs       []struct {
				ID int64 `json:"id"`
			} `json:"workflow_runs"`
		}
		query := fmt.Sprintf("?status=%s&per_page=%d&exclude_pull_requests=true", status, runsPerPage)
		if err := c.do(ctx, http.MethodGet, base+query, &runs, scope); err != nil {
			return 0, err
		}
		for _, run := range runs.Runs {
			unfinished = append(unfinished, run.ID)
		}
	}

	queued := 0
	for _, id := range unfinished {
		jobs, err := c.jobsOf(ctx, scope, base, id)
		if err != nil {
			return 0, err
		}
		for _, job := range jobs {
			if job.Status != "queued" || !Serves(labels, job.Labels) {
				continue
			}
			queued++
			if queued >= limit {
				return queued, nil
			}
		}
	}
	return queued, nil
}

// jobRow is the part of a job this cares about: whether it is still waiting,
// and what its runs-on asked for.
type jobRow struct {
	Status string   `json:"status"`
	Labels []string `json:"labels"`
}

// jobsOf reads one run's jobs.
//
// filter=latest: a job that was re-run has its earlier attempts in this
// listing too, and an attempt that finished days ago is not waiting for
// anything.
func (c *Client) jobsOf(ctx context.Context, scope Scope, base string, run int64) ([]jobRow, error) {
	var out struct {
		Jobs []jobRow `json:"jobs"`
	}
	path := fmt.Sprintf("%s/%d/jobs?filter=latest&per_page=100", base, run)
	if err := c.do(ctx, http.MethodGet, path, &out, scope); err != nil {
		return nil, err
	}
	return out.Jobs, nil
}

// implicit are the labels every self-hosted Linux runner has whether or not
// anybody configured them. A workflow that says runs-on: [self-hosted, vm] is
// asking for one of them, and a pool that matched literally would decide the
// job was not for it and go back to sleep.
var implicit = map[string]bool{
	"self-hosted": true,
	"linux":       true,
	"x64":         true,
}

// Serves reports whether a runner with these labels would be given a job that
// asked for those.
//
// GitHub's own rule: every label the job asks for has to be on the runner. The
// runner having more is what makes a pool usable by several workflows.
func Serves(runner, job []string) bool {
	if len(job) == 0 {
		// runs-on with nothing in it is not a job any runner is offered.
		return false
	}
	has := make(map[string]bool, len(runner))
	for _, label := range runner {
		has[strings.ToLower(label)] = true
	}
	for _, want := range job {
		want = strings.ToLower(strings.TrimSpace(want))
		if has[want] || implicit[want] {
			continue
		}
		return false
	}
	return true
}
