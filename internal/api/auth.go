package api

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Settings keys the daemon stores its own configuration under.
const (
	SettingAuthUser = "auth.user"
	SettingAuthHash = "auth.hash"
)

// Authenticator checks HTTP Basic credentials against the stored ones.
//
// Basic auth over a loopback bind is the whole security model here, and it is
// chosen deliberately: this daemon can create machines and read a credential
// that administers repositories, so the interesting question is not who the
// user is but whether anything else on the host can reach it at all.
type Authenticator struct {
	store  SettingsStore
	mu     sync.Mutex
	fails  map[string]*attempts
	nowFor func() time.Time
}

// SettingsStore is the part of the store authentication needs.
type SettingsStore interface {
	Setting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

type attempts struct {
	count int
	until time.Time
}

// lockout is how long a client is refused after too many wrong guesses, and
// how many it gets. Basic auth invites a script; this makes one pointless
// without locking anyone out for long after a typo.
const (
	maxAttempts = 10
	lockout     = 5 * time.Minute
)

// NewAuthenticator builds an authenticator.
func NewAuthenticator(store SettingsStore) *Authenticator {
	return &Authenticator{store: store, fails: map[string]*attempts{}, nowFor: time.Now}
}

// SetPassword stores a new password, hashed. The plaintext is never written
// anywhere, including the logs.
func (a *Authenticator) SetPassword(ctx context.Context, user, password string) error {
	if strings.TrimSpace(user) == "" {
		return errBadRequest("a user name is required")
	}
	if len(password) < 8 {
		return errBadRequest("the password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if err := a.store.SetSetting(ctx, SettingAuthUser, user); err != nil {
		return err
	}
	return a.store.SetSetting(ctx, SettingAuthHash, string(hash))
}

// Configured reports whether a password has been set. Until one is, the daemon
// refuses to serve anything rather than serving it to everyone.
func (a *Authenticator) Configured(ctx context.Context) bool {
	hash, err := a.store.Setting(ctx, SettingAuthHash)
	return err == nil && hash != ""
}

// Middleware refuses anything without valid credentials.
//
// The order here matters, and it is the opposite of the obvious one. The right
// password is checked first and always accepted, even from a client that has
// been failing; only a wrong one meets the lockout. Refusing a correct password
// protects nobody — whoever sent it can already have everything behind this —
// and it took an operator out of their own dashboard for five minutes at a
// time.
//
// It matters most behind a reverse proxy, where every request arrives from the
// same address. One script guessing at a public name would otherwise lock out
// everybody, which is a denial of service delivered by the defence.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client := clientKey(r)
		user, password, offered := r.BasicAuth()

		if offered && a.check(r.Context(), user, password) {
			a.recordSuccess(client)
			next.ServeHTTP(w, r)
			return
		}

		// No credentials at all is not a guess. It is how Basic auth begins:
		// a browser asks once without them, is challenged, and asks again with
		// them. Counting that as an attempt meant ten page loads could lock
		// somebody out of their own fleet.
		if !offered {
			challenge(w)
			return
		}

		a.recordFailure(client)
		if a.lockedOut(client) {
			http.Error(w, "too many failed attempts; try again shortly", http.StatusTooManyRequests)
			return
		}
		challenge(w)
	})
}

// challenge asks the browser for credentials. The realm is what makes it offer
// a login box rather than showing the error.
func challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Basic realm="runner-fleet", charset="UTF-8"`)
	http.Error(w, "unauthorised", http.StatusUnauthorized)
}

func (a *Authenticator) check(ctx context.Context, user, password string) bool {
	wantUser, err := a.store.Setting(ctx, SettingAuthUser)
	if err != nil {
		return false
	}
	hash, err := a.store.Setting(ctx, SettingAuthHash)
	if err != nil || hash == "" {
		// No password configured: refuse everything. A daemon that served the
		// fleet to anyone until someone got round to setting a password would
		// be worse than one that does not start.
		return false
	}

	// Constant time on the user name, and bcrypt on the password, so neither
	// answers "was the user right?" faster than "was the password right?".
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(wantUser)) == 1
	passwordOK := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
	return userOK && passwordOK
}

func clientKey(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}

func (a *Authenticator) lockedOut(client string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	record, ok := a.fails[client]
	return ok && record.count >= maxAttempts && a.nowFor().Before(record.until)
}

func (a *Authenticator) recordFailure(client string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	record, ok := a.fails[client]
	if !ok {
		record = &attempts{}
		a.fails[client] = record
	}
	if record.count >= maxAttempts && a.nowFor().After(record.until) {
		record.count = 0
	}
	record.count++
	record.until = a.nowFor().Add(lockout)
}

func (a *Authenticator) recordSuccess(client string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.fails, client)
}
