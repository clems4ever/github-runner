package model

import "time"

// APIKeyScope is what a key may do.
//
// Two of them, and deliberately not more: a scope nobody can describe in a
// sentence is one nobody grants correctly. Read covers the whole of the
// daemon's reading surface, admin covers changing the fleet, and the things a
// key may never do are not a scope at all — see APIKey.
type APIKeyScope string

const (
	// APIKeyRead may read the fleet and change nothing. This is what a
	// dashboard, an alert or a "how busy are we" script wants, and giving one of
	// those the ability to delete a pool buys nothing.
	APIKeyRead APIKeyScope = "read"
	// APIKeyAdmin may also change the fleet: create pools, rotate a GitHub
	// credential, ask for a reconcile.
	APIKeyAdmin APIKeyScope = "admin"
)

// Valid reports whether a scope is one the daemon knows. An unknown scope is
// refused rather than treated as the safer one: a key created from a typo would
// otherwise work, and its holder would believe it had the access they asked for.
func (s APIKeyScope) Valid() bool {
	return s == APIKeyRead || s == APIKeyAdmin
}

// APIKey is a credential a machine uses to call this daemon's API, as opposed
// to a person typing a password into a browser.
//
// The key itself is not in this struct and cannot be: the database holds a
// SHA-256 of it and nothing else, so it is shown once when it is created and is
// afterwards unrecoverable by anyone, including whoever runs the daemon. That
// is the difference between this and a Credential, which has to be decryptable
// because the daemon needs the plaintext to talk to GitHub. A key is only ever
// compared, so it is hashed one way and a stolen database yields nothing.
//
// No scope lets a key read the web password or another key. That is not a
// permission anyone can grant: it is refused at the routes themselves, so a
// leaked key cannot mint its replacement or lock the operator out of the UI,
// and revoking it is therefore final.
type APIKey struct {
	ID    int64       `json:"id"`
	Name  string      `json:"name"`
	Scope APIKeyScope `json:"scope"`
	// Hint is the front of the key, which is enough to recognise one in a log
	// or tell two apart in a list. The front rather than the tail, unlike a
	// credential's hint: a key's entropy is all of it, so either end gives the
	// same nothing away, and the front is the part somebody has already seen.
	Hint      string    `json:"hint"`
	CreatedAt time.Time `json:"createdAt"`
	// ExpiresAt is when the key stops working, if it ever does. A key that
	// outlives the job it was made for is the one nobody remembers to revoke.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// LastUsedAt is what makes "which of these can I safely revoke" an
	// answerable question. Nil for a key that has never been used, which is
	// itself the answer often enough.
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

// Expired reports whether the key is past its expiry at the given time.
func (k APIKey) Expired(now time.Time) bool {
	return k.ExpiresAt != nil && !now.Before(*k.ExpiresAt)
}
