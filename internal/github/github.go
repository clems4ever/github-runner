// Package github is the part of GitHub's API the fleet needs: minting
// registration tokens, and asking what each runner is doing.
//
// Whether a job is on a runner is only knowable from here. The host can see
// that a VM or a container is running; it cannot see that anything is
// happening inside it, and that is exactly the fact the reconciler needs
// before it removes anything.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
)

// DefaultBaseURL is github.com's API. Enterprise Server installations put
// theirs elsewhere, which is why this is not hard-coded further down.
const DefaultBaseURL = "https://api.github.com"

// Client talks to one GitHub with one credential.
type Client struct {
	token   string
	baseURL string
	http    *http.Client
}

// Option configures a client.
type Option func(*Client)

// WithBaseURL points the client at another GitHub, or at a test server.
func WithBaseURL(base string) Option {
	return func(c *Client) { c.baseURL = strings.TrimSuffix(base, "/") }
}

// WithHTTPClient replaces the HTTP client.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// New builds a client.
func New(token string, opts ...Option) *Client {
	c := &Client{
		token:   token,
		baseURL: DefaultBaseURL,
		// A timeout rather than none: the reconciler runs on a ticker, and a
		// call that never returns would stall every pool behind it.
		http: &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Scope is a repository or an organisation.
type Scope struct {
	Kind model.ScopeKind
	Path string
}

// ScopeOf reads the scope out of a pool.
func ScopeOf(p model.Pool) Scope { return Scope{Kind: p.ScopeKind, Path: p.Scope} }

func (s Scope) String() string { return s.Path }

// prefix is the API path for the scope's runners.
func (s Scope) prefix() string {
	if s.Kind == model.ScopeOrganization {
		return "/orgs/" + url.PathEscape(s.Path)
	}
	owner, repo, _ := strings.Cut(s.Path, "/")
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

// State is what a runner is doing, as GitHub sees it.
type State string

const (
	// StateBusy means a job is on it right now. Removing it fails that job.
	StateBusy State = "busy"
	// StateIdle means registered and waiting for work.
	StateIdle State = "idle"
	// StateOffline means registered but not connected — a runner that has not
	// booted, or has died. Deliberately not folded into idle: idle reads as
	// "safe to remove", and this is the one case where that is misleading in
	// the other direction, since there is nothing to wait for.
	StateOffline State = "offline"
)

// Runner is one registered runner.
type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

// State maps GitHub's two fields onto one.
func (r Runner) State() State {
	if r.Status != "online" {
		return StateOffline
	}
	if r.Busy {
		return StateBusy
	}
	return StateIdle
}

// RegistrationToken mints the short-lived token a runner registers with. One
// is needed per start, because a runner keeps nothing between boots and the
// token expires an hour after it is issued.
func (c *Client) RegistrationToken(ctx context.Context, scope Scope) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	if err := c.do(ctx, http.MethodPost, scope.prefix()+"/actions/runners/registration-token", &out, scope); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("%s: GitHub returned no registration token", scope)
	}
	return out.Token, nil
}

// Runners lists what is registered for a scope, with what each one is doing.
func (c *Client) Runners(ctx context.Context, scope Scope) ([]Runner, error) {
	var out struct {
		TotalCount int      `json:"total_count"`
		Runners    []Runner `json:"runners"`
	}
	// One page of a hundred covers any fleet this daemon can run on one host,
	// and asking for more pages on every reconcile would cost more than it is
	// worth.
	if err := c.do(ctx, http.MethodGet, scope.prefix()+"/actions/runners?per_page=100", &out, scope); err != nil {
		return nil, err
	}
	return out.Runners, nil
}

// States is Runners keyed by name, which is how the reconciler asks about one.
func (c *Client) States(ctx context.Context, scope Scope) (map[string]State, error) {
	runners, err := c.Runners(ctx, scope)
	if err != nil {
		return nil, err
	}
	states := make(map[string]State, len(runners))
	for _, r := range runners {
		states[r.Name] = r.State()
	}
	return states, nil
}

// Deregister removes a runner's entry, so a fleet that has scaled down does
// not leave a list of offline runners behind on the repository.
func (c *Client) Deregister(ctx context.Context, scope Scope, name string) error {
	runners, err := c.Runners(ctx, scope)
	if err != nil {
		return err
	}
	for _, r := range runners {
		if r.Name == name {
			return c.do(ctx, http.MethodDelete, fmt.Sprintf("%s/actions/runners/%d", scope.prefix(), r.ID), nil, scope)
		}
	}
	// Already gone is the outcome that was wanted.
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, out any, scope Scope) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(resp.StatusCode, body, scope)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s %s: GitHub returned something that is not the expected JSON: %w", method, path, err)
	}
	return nil
}

// Error is a refusal from GitHub, carrying the status so callers can tell a
// permissions problem from an outage.
type Error struct {
	Status  int
	Scope   string
	Message string
	Advice  string
}

func (e *Error) Error() string {
	if e.Advice == "" {
		return fmt.Sprintf("GitHub returned %d for %s: %s", e.Status, e.Scope, e.Message)
	}
	return fmt.Sprintf("GitHub returned %d for %s: %s — %s", e.Status, e.Scope, e.Message, e.Advice)
}

// apiError turns a refusal into the specific thing to go and check. The three
// statuses have genuinely different causes, and the advice for one is useless
// for the others.
func apiError(status int, body []byte, scope Scope) error {
	var payload struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &payload)
	message := payload.Message
	if message == "" {
		message = strings.TrimSpace(string(body))
		if len(message) > 200 {
			message = message[:200]
		}
	}

	e := &Error{Status: status, Scope: scope.Path, Message: message}
	switch status {
	case http.StatusUnauthorized:
		e.Advice = "the token itself is wrong, not its permissions: check the value, not the scopes"
	case http.StatusForbidden:
		if scope.Kind == model.ScopeOrganization {
			e.Advice = "the credential is valid but not allowed to do this: an organisation needs Self-hosted runners: Read and write, or the classic admin:org scope"
		} else {
			e.Advice = "the credential is valid but not allowed to do this: a repository needs Administration: Read and write, or the classic repo scope"
		}
	case http.StatusNotFound:
		if scope.Kind == model.ScopeOrganization {
			e.Advice = "either the credential cannot see this organisation, or it is a personal account — GitHub has no account-level runners, so a personal account needs one pool per repository"
		} else {
			e.Advice = "a 404 here usually means the credential cannot see the repository at all rather than that it is missing: check the token's resource owner and repository access"
		}
	}
	return e
}
