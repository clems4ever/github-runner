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
	"errors"
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
//
// The credential is either a personal access token, used as it is, or a GitHub
// App, whose key is exchanged for a short-lived installation token behind this
// interface. Nothing above here has to know which.
type Client struct {
	token   string
	app     *appAuth
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

// CheckAccess asks whether this credential can do what a pool will need it to,
// before a pool is created that quietly fails a minute later.
//
// Listing runners is the cheapest call that exercises the same permission the
// daemon actually uses, so a pass here means the pool will work rather than
// only that the credential exists.
func (c *Client) CheckAccess(ctx context.Context, scope Scope) error {
	_, err := c.Runners(ctx, scope)
	if err == nil {
		return nil
	}

	var apiErr *Error
	if errors.As(err, &apiErr) && c.app != nil && apiErr.Status == http.StatusNotFound {
		apiErr.GrantURL = c.app.grantURL(ctx, c)
	}
	return err
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

// do makes a call with whatever credential this client has.
func (c *Client) do(ctx context.Context, method, path string, out any, scope Scope) error {
	bearer := c.token
	if c.app != nil {
		// An app's key cannot call these endpoints directly; it buys an
		// installation token first, which is cached until it is nearly spent.
		minted, err := c.app.bearer(ctx, c, scope)
		if err != nil {
			return err
		}
		bearer = minted
	}
	return c.doAs(ctx, method, path, bearer, out, scope)
}

// doAs makes a call with an explicit bearer, which is how the app flow signs
// its own requests with a JWT rather than recursing into do.
func (c *Client) doAs(ctx context.Context, method, path, bearer string, out any, scope Scope) error {
	isApp := c.app != nil
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+bearer)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apiError(resp.StatusCode, body, scope, isApp)
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
	// GrantURL is where a person goes to fix this, when there is such a place.
	// An app cannot widen its own access — GitHub does not allow it, which is
	// the point of installing on selected repositories — so the most this can
	// do is put the right page one click away instead of somewhere to hunt
	// for.
	GrantURL string
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
func apiError(status int, body []byte, scope Scope, isApp bool) error {
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
		e.Advice = "the credential itself is wrong, not its permissions: check the value, not the scopes. For an app, check the private key belongs to the app id, and that this host's clock is right — a JWT from the future is refused"
	case http.StatusForbidden:
		if scope.Kind == model.ScopeOrganization {
			e.Advice = "the credential is valid but not allowed to do this: an organisation needs Self-hosted runners: Read and write, or the classic admin:org scope"
		} else {
			e.Advice = "the credential is valid but not allowed to do this: a repository needs Administration: Read and write, or the classic repo scope"
		}
	case http.StatusNotFound:
		switch {
		case isApp:
			// The app authenticated — a wrong key is a 401, not this — so the
			// app simply cannot see this scope. On a repository that is
			// almost always an installation that does not cover it.
			e.Advice = fmt.Sprintf("the app is authenticated but has no installation covering %s. "+
				"Install it there, or add %s to its repository access: "+
				"https://github.com/settings/installations", scope.Path, scope.Path)
		case scope.Kind == model.ScopeOrganization:
			e.Advice = "either the credential cannot see this organisation, or it is a personal account — GitHub has no account-level runners, so a personal account needs one pool per repository"
		default:
			e.Advice = "a 404 here usually means the credential cannot see the repository at all rather than that it is missing: check the token's resource owner and repository access"
		}
	}
	return e
}
