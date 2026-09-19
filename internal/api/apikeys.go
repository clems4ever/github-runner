package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/secrets"
	"github.com/clems4ever/github-runner/internal/store"
)

// KeyStore is the part of the store API key authentication needs.
type KeyStore interface {
	APIKeyByHash(ctx context.Context, hash string) (model.APIKey, error)
	TouchAPIKey(ctx context.Context, id int64, at time.Time) error
}

// touchInterval is how often a key's last-used timestamp is written.
//
// Once a minute per key, because a client polling the fleet every second would
// otherwise turn every read into a write. What the timestamp is for — deciding
// which of six keys is still in use and can be revoked — is not a question that
// needs the last sixty seconds, and losing that much of it over a restart costs
// nobody anything.
const touchInterval = time.Minute

// keyAuth verifies the keys machines call this daemon with.
//
// There is no attempt counting here and no lockout, unlike the password beside
// it. A key is 256 bits from crypto/rand, so guessing is not a threat worth
// defending against — and defending against it would create a real one: the
// failures would arrive from whatever address the reverse proxy uses, which is
// the same address the operator's browser comes from, so one CI job looping on a
// revoked key would lock its owner out of their own fleet.
type keyAuth struct {
	store  KeyStore
	mu     sync.Mutex
	last   map[int64]time.Time
	nowFor func() time.Time
}

func newKeyAuth(store KeyStore) *keyAuth {
	return &keyAuth{store: store, last: map[int64]time.Time{}, nowFor: time.Now}
}

// bearer is the key a request presented, if it presented one this way.
//
// Only the Authorization header, and only the Bearer scheme. Not a query
// parameter: those are logged by every proxy in the way, kept in browser
// history, and pasted into bug reports.
func bearer(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	key := strings.TrimSpace(header[len(prefix):])
	return key, key != ""
}

// verify resolves a presented key, or says why it will not.
//
// Deliberately not consulting whether a web password has been set. That gate
// exists because a daemon with no password would otherwise serve the fleet to
// anyone who asked; a valid key is itself the proof of authorisation that gate
// is looking for. It is what makes a headless install possible — provisioned
// with a key from the CLI, never opened in a browser.
func (k *keyAuth) verify(ctx context.Context, presented string) (model.APIKey, error) {
	key, err := k.store.APIKeyByHash(ctx, secrets.HashAPIKey(presented))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The same answer for a key that never existed and one that was
			// revoked, because they are the same fact: this key does not work.
			return model.APIKey{}, errors.New("unknown api key")
		}
		return model.APIKey{}, err
	}
	now := k.nowFor()
	if key.Expired(now) {
		// Said plainly rather than folded into "unknown". Whoever sent it
		// already has the key, so nothing is revealed, and an expiry is the one
		// failure here with an obvious fix.
		return model.APIKey{}, errors.New("this api key expired on " + key.ExpiresAt.Format(time.RFC3339))
	}
	k.touch(ctx, key.ID, now)
	return key, nil
}

// touch records the use, at most once a minute per key. Inline rather than in a
// goroutine: the request's own context is the right one to cancel it, and a
// background write outliving the request would be cancelled or lost anyway.
func (k *keyAuth) touch(ctx context.Context, id int64, now time.Time) {
	k.mu.Lock()
	if last, ok := k.last[id]; ok && now.Sub(last) < touchInterval {
		k.mu.Unlock()
		return
	}
	k.last[id] = now
	k.mu.Unlock()
	// A failure here is bookkeeping lost, not a request to refuse: the key is
	// valid whether or not the daemon manages to write down that it was used.
	_ = k.store.TouchAPIKey(ctx, id, now)
}

// ---------------------------------------------------------------------------
// What a key may reach
// ---------------------------------------------------------------------------

// scopeAllows decides what a key may do, from its scope and the method.
//
// The rule is the method and nothing else, which is honest here because the
// routes are: no GET on this API changes anything, POST and PUT and DELETE all
// do. Deciding it centrally rather than route by route is the point — a route
// added next year is covered by this without anyone remembering to cover it, and
// it errs closed, since an unrecognised method is not a read.
func scopeAllows(scope model.APIKeyScope, method string) bool {
	if scope == model.APIKeyAdmin {
		return true
	}
	return method == http.MethodGet || method == http.MethodHead
}

// keysRefused reports paths no api key may reach, whatever its scope.
//
// These are not a permission anybody can grant, which is why they are a list
// here and not a third scope:
//
//   - The keys themselves. A key that could read the key table could not read
//     the keys — they are hashed — but it could enumerate what exists, and a key
//     that could write it could mint its own replacement and outlive its own
//     revocation. Revoking a key has to be final, so no key may touch this.
//   - The web password. A key that could change it would lock the operator out
//     of the UI and take ownership of the daemon; the operator's own password is
//     the one credential that only a person may set.
//
// Checked here, in the one place every /api/ request passes through, rather than
// as a wrapper on each route: a wrapper is something a future route can be
// written without, and this cannot.
func keysRefused(path string) bool {
	for _, prefix := range []string{"/api/api-keys", "/api/settings/auth"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Who is asking
// ---------------------------------------------------------------------------

// principal is how a request authenticated: a person at a browser, or a key.
//
// Carried in the context because two handlers need it — one to refuse, one to
// redact — and threading it through every signature to serve two would be worse.
type principal struct {
	// Key is the api key the request came with, or nil for a person who typed
	// the password.
	Key *model.APIKey
}

// Human reports whether a person authenticated this request.
func (p principal) Human() bool { return p.Key == nil }

type principalKey struct{}

func withPrincipal(r *http.Request, p principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, p))
}

// principalOf says who is asking.
//
// A request that somehow reached a handler without passing the middleware is
// reported as a key with no rights rather than as a person: the safe end of a
// mistake that should be impossible.
func principalOf(r *http.Request) principal {
	if p, ok := r.Context().Value(principalKey{}).(principal); ok {
		return p
	}
	return principal{Key: &model.APIKey{}}
}
