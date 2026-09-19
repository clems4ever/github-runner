package model

import (
	"testing"
	"time"
)

func TestAPIKeyScopeValid(t *testing.T) {
	for _, scope := range []APIKeyScope{APIKeyRead, APIKeyAdmin} {
		if !scope.Valid() {
			t.Errorf("%q should be a scope", scope)
		}
	}
	// A typo must not pass for the scope it nearly is: the operator would
	// believe they had granted what they typed.
	for _, scope := range []APIKeyScope{"", "write", "Read", "admin ", "*"} {
		if APIKeyScope(scope).Valid() {
			t.Errorf("%q should not be a scope", scope)
		}
	}
}

func TestAPIKeyWithoutAnExpiryNeverExpires(t *testing.T) {
	key := APIKey{Name: "ci"}
	if key.Expired(time.Now().AddDate(10, 0, 0)) {
		t.Error("a key with no expiry expired anyway")
	}
}

func TestAPIKeyExpiry(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	key := APIKey{Name: "ci", ExpiresAt: &at}

	if key.Expired(at.Add(-time.Second)) {
		t.Error("expired a second early")
	}
	// The instant itself counts as expired: an expiry of "now" that still worked
	// would be a key with no expiry for one tick.
	if !key.Expired(at) {
		t.Error("still valid at the very moment it expires")
	}
	if !key.Expired(at.Add(time.Second)) {
		t.Error("still valid after it expired")
	}
}
