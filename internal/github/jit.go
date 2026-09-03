package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/clems4ever/github-runner/internal/model"
)

// A just-in-time configuration is a runner's whole identity, minted by the
// daemon and handed to it once.
//
// It replaces the two-step registration every runner used to do: mint a
// registration token, hand it over, and let the runner call config.sh with it
// to trade the token for credentials of its own. That step is the reason a
// registration token had to exist inside a guest at all, it is the reason
// registration can fail an hour after a machine was created, and it is a
// minute of a machine's life spent doing something the daemon could have done
// before the machine booted.
//
// What comes back here is base64 of the same files config.sh would have
// written, and `run.sh --jitconfig` unpacks them and starts. So:
//
//   - Nothing that can administer a repository is ever inside a guest. The
//     credential that mints this stays on the host, and what the guest gets
//     runs one job and is worthless afterwards.
//   - There is no registration step to fail, and no config.sh to run.
//   - A runner that never boots leaves nothing behind: GitHub only creates the
//     entry when the runner connects, where a registration is created the
//     moment config.sh succeeds and has to be deleted afterwards.
//
// The cost, and it is the reason this does not replace registration
// everywhere: a just-in-time runner is always ephemeral, and its configuration
// is spent by the one job it takes. A pool of long-lived runners cannot be
// built out of these, so that pool still registers the old way.

// JIT is what one runner should be.
type JIT struct {
	// Name is the runner's name, which is also its identity in the fleet.
	Name string
	// Labels are what a workflow's runs-on has to match. GitHub requires at
	// least one and rejects an empty list, so a pool that named none gets the
	// self-hosted label every runner has anyway.
	Labels []string
	// Group is the runner group by name, which is what a pool stores. It is
	// resolved to the id this endpoint wants.
	Group string
	// WorkFolder is where the runner puts a job's checkout. Empty leaves
	// GitHub's default of _work.
	WorkFolder string
}

// JITConfig mints one runner's whole configuration.
//
// The returned string is opaque and is a credential: it is what the runner
// authenticates with, so it is handed to exactly one machine and never
// written anywhere it outlives that machine.
func (c *Client) JITConfig(ctx context.Context, scope Scope, want JIT) (string, error) {
	labels := want.Labels
	if len(labels) == 0 {
		// GitHub refuses an empty list. Every runner carries this label
		// regardless, so it is the one that changes nothing.
		labels = []string{"self-hosted"}
	}

	group, err := c.runnerGroupID(ctx, scope, want.Group)
	if err != nil {
		return "", err
	}

	body := map[string]any{
		"name":            want.Name,
		"runner_group_id": group,
		"labels":          labels,
	}
	if want.WorkFolder != "" {
		body["work_folder"] = want.WorkFolder
	}

	var out struct {
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	path := scope.prefix() + "/actions/runners/generate-jitconfig"
	err = c.post(ctx, path, body, &out, scope)
	if isNameTaken(err) {
		// A runner's name is its identity in the fleet and is reused on
		// purpose — the same machine comes back after every job under the same
		// name. config.sh had --replace for this; this endpoint has nothing,
		// and refuses outright.
		//
		// What is being replaced is an entry GitHub should have removed
		// itself: a just-in-time runner is deleted when its job ends, so one
		// left over means the machine holding it was killed rather than
		// stopped. Deleting it costs nothing — there is no job on it, because
		// the machine that would have been running it is gone.
		if removed := c.Deregister(ctx, scope, want.Name); removed != nil {
			return "", fmt.Errorf("a runner called %s already exists and could not be removed: %w",
				want.Name, removed)
		}
		err = c.post(ctx, path, body, &out, scope)
	}
	if err != nil {
		return "", err
	}
	if out.EncodedJITConfig == "" {
		return "", fmt.Errorf("%s: GitHub returned no runner configuration", scope)
	}
	return out.EncodedJITConfig, nil
}

// isNameTaken reports whether GitHub refused because a runner of that name is
// already registered.
//
// Matched on the message as well as the status: 409 is the only status this
// endpoint uses for it, but the same status could arrive from something else
// entirely, and deregistering a runner in response to the wrong conflict would
// be the expensive kind of mistake.
func isNameTaken(err error) bool {
	var refused *Error
	if !errors.As(err, &refused) || refused.Status != http.StatusConflict {
		return false
	}
	return strings.Contains(strings.ToLower(refused.Message), "already exists")
}

// DefaultRunnerGroup is the group every scope has and cannot delete. It is
// also the only group a repository-scoped runner can be in: runner groups are
// an organisation and enterprise idea, and a repository's own runners are
// always in group 1.
const DefaultRunnerGroup int64 = 1

// runnerGroupID turns the group name a pool stores into the id this endpoint
// wants.
//
// Cached for the life of the client, which is one reconcile pass: groups are
// created by hand and almost never change, and asking once per runner would
// double the calls a pool makes to scale up.
func (c *Client) runnerGroupID(ctx context.Context, scope Scope, name string) (int64, error) {
	if name == "" || name == "Default" || scope.Kind != model.ScopeOrganization {
		return DefaultRunnerGroup, nil
	}

	key := scope.Path + "\x00" + name
	c.groupsMu.Lock()
	if id, ok := c.groups[key]; ok {
		c.groupsMu.Unlock()
		return id, nil
	}
	c.groupsMu.Unlock()

	var out struct {
		RunnerGroups []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"runner_groups"`
	}
	path := "/orgs/" + url.PathEscape(scope.Path) + "/actions/runner-groups?per_page=100"
	if err := c.do(ctx, http.MethodGet, path, &out, scope); err != nil {
		return 0, fmt.Errorf("find the runner group %q: %w", name, err)
	}
	for _, group := range out.RunnerGroups {
		if group.Name == name {
			c.groupsMu.Lock()
			if c.groups == nil {
				c.groups = map[string]int64{}
			}
			c.groups[key] = group.ID
			c.groupsMu.Unlock()
			return group.ID, nil
		}
	}
	// Named rather than silently placed in the default group: a runner in the
	// wrong group is invisible to the workflows that asked for it, which reads
	// as a fleet that is not scaling up rather than as a configuration
	// mistake.
	return 0, fmt.Errorf("%s has no runner group called %q", scope.Path, name)
}

// post is do with a JSON body. It exists here rather than in the client's own
// file because this is the only call that sends one.
func (c *Client) post(ctx context.Context, path string, body, out any, scope Scope) error {
	bearer := c.token
	if c.app != nil {
		minted, err := c.app.bearer(ctx, c, scope)
		if err != nil {
			return err
		}
		bearer = minted
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(resp.StatusCode, payload, scope, c.app != nil)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("POST %s: GitHub returned something that is not the expected JSON: %w", path, err)
	}
	return nil
}

// groups caches the runner-group lookup above.
type groupCache struct {
	groupsMu sync.Mutex
	groups   map[string]int64
}
