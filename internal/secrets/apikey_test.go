package secrets

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestNewAPIKeyIsPrefixedAndLongEnough(t *testing.T) {
	key, _, _, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, APIKeyPrefix) {
		t.Errorf("key %q does not begin with %q, so nothing can recognise it", key, APIKeyPrefix)
	}

	// The prefix is decoration; the entropy is what makes the rest of the design
	// safe, so it is the thing worth asserting.
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(key, APIKeyPrefix))
	if err != nil {
		t.Fatalf("the body of the key is not base64url: %v", err)
	}
	if len(raw) != apiKeyBytes {
		t.Errorf("key carries %d bytes of randomness, want %d", len(raw), apiKeyBytes)
	}
}

// A key that is one word survives a shell, a header, a URL and a YAML file
// without quoting. Padding and slashes are what would break that.
func TestNewAPIKeyNeedsNoQuoting(t *testing.T) {
	for range 200 {
		key, _, _, err := NewAPIKey()
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(key, "+/= \t\n\"'\\") {
			t.Fatalf("key %q contains something that needs quoting", key)
		}
	}
}

func TestNewAPIKeyNeverRepeats(t *testing.T) {
	seen := map[string]bool{}
	for range 500 {
		key, hash, _, err := NewAPIKey()
		if err != nil {
			t.Fatal(err)
		}
		if seen[key] {
			t.Fatalf("NewAPIKey returned %q twice", key)
		}
		if seen[hash] {
			t.Fatalf("NewAPIKey returned the hash %q twice", hash)
		}
		seen[key], seen[hash] = true, true
	}
}

// The hint identifies a key without being usable as one.
func TestAPIKeyHintIsAShortPrefixOfTheBody(t *testing.T) {
	key, _, hint, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(hint) != apiKeyHintLength {
		t.Errorf("hint %q is %d characters, want %d", hint, len(hint), apiKeyHintLength)
	}
	if !strings.HasPrefix(strings.TrimPrefix(key, APIKeyPrefix), hint) {
		t.Errorf("hint %q is not the front of key %q, so it identifies nothing", hint, key)
	}
	// Whatever is stored in clear has to be far too little to work with.
	if len(hint) >= len(key)/2 {
		t.Errorf("hint %q gives away too much of %q", hint, key)
	}
}

// What NewAPIKey stores must be what verification computes, or every key is
// refused the moment it is used.
func TestHashAPIKeyMatchesWhatNewAPIKeyReturned(t *testing.T) {
	key, hash, _, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if got := HashAPIKey(key); got != hash {
		t.Errorf("HashAPIKey gave %q, but the key was stored as %q", got, hash)
	}
}

func TestHashAPIKeyIsNotReversible(t *testing.T) {
	key, hash, _, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	// The whole reason a stolen database is worth nothing: the key is not in the
	// hash in any form, not even its recognisable front.
	if strings.Contains(hash, key) || strings.Contains(hash, strings.TrimPrefix(key, APIKeyPrefix)) {
		t.Errorf("the hash %q contains the key %q", hash, key)
	}
	if strings.Contains(hash, APIKeyPrefix) {
		t.Errorf("the hash %q carries the key's prefix", hash)
	}
}

func TestHashAPIKeyDiffersPerKey(t *testing.T) {
	first, _, _, _ := NewAPIKey()
	second, _, _, _ := NewAPIKey()
	if HashAPIKey(first) == HashAPIKey(second) {
		t.Error("two different keys hash the same")
	}
}

// A key pasted into a form or an environment file arrives with whitespace around
// it often enough that refusing it would be a support question rather than a
// defence.
func TestHashAPIKeyIgnoresSurroundingWhitespace(t *testing.T) {
	key, hash, _, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, padded := range []string{" " + key, key + "\n", "\t" + key + " \r\n"} {
		if HashAPIKey(padded) != hash {
			t.Errorf("a key padded as %q was not recognised", padded)
		}
	}
}
