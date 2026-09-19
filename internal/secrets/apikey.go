package secrets

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// APIKeyPrefix begins every key the daemon issues.
//
// It is there to be recognised: by a secret scanner watching a repository, and
// by a person who has found a long string in a log file and needs to know what
// they are holding.
const APIKeyPrefix = "rf_"

// apiKeyBytes is how much randomness a key carries. 256 bits, which is the
// number that makes the rest of the design simple: there is nothing to rate
// limit, nothing to lock out, and no reason to hash slowly, because no amount of
// guessing gets anywhere near it.
const apiKeyBytes = 32

// apiKeyHintLength is how much of a key is kept in clear to identify it.
//
// Six characters is thirty-six bits of the two hundred and fifty-six, which
// leaves the key exactly as unguessable as it was and gives an operator
// something to match against what their CI system is configured with.
const apiKeyHintLength = 6

// NewAPIKey mints a key, returning it, the hash to store, and the hint to show.
//
// The key is returned once, to be handed to whoever asked for it, and then it is
// the caller's business to forget. Nothing in the daemon can recover it
// afterwards, which is the point: see HashAPIKey.
func NewAPIKey() (key, hash, hint string, err error) {
	raw := make([]byte, apiKeyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", "", err
	}
	// Unpadded base64url, so a key is one word: safe in a URL, in a header, in
	// a shell without quoting, and in a YAML file without a reader wondering
	// whether the trailing = is part of it.
	body := base64.RawURLEncoding.EncodeToString(raw)
	key = APIKeyPrefix + body
	return key, HashAPIKey(key), body[:apiKeyHintLength], nil
}

// HashAPIKey is how a key is stored and how a presented one is checked.
//
// SHA-256, and deliberately not bcrypt — which is what the web password uses a
// few files away, for the opposite reason. bcrypt is slow on purpose because a
// password somebody chose has perhaps thirty bits of entropy and has to survive
// an attacker grinding a stolen hash. A key from NewAPIKey has two hundred and
// fifty-six bits from crypto/rand: there is no dictionary, no shorter guess, and
// nothing a slow hash would protect. Paying bcrypt's hundred milliseconds on
// every API call would only tax the honest client polling once a second.
//
// Unkeyed, so verifying a key does not depend on the master key. A key is
// checked by hashing what arrived and looking for it, never by decrypting
// anything, so the database holding these is worth nothing to whoever steals it
// — there is no key material in it to take, only the fact that a key existed.
func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:])
}
