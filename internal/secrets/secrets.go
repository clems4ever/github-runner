// Package secrets keeps the GitHub credentials the daemon needs at rest.
//
// The daemon has to be able to decrypt: it mints registration tokens on every
// runner boot, with nobody at the keyboard. So this is not protection against
// someone who is already root on the host — it protects the database wherever
// it goes that the key does not: backups, snapshots, a disk pulled out of a
// machine, a copied file left in /tmp while debugging.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// KeySize is the AES-256 key length.
const KeySize = 32

// envelopeV1 prefixes every ciphertext. Versioning the format from the first
// release is what makes it possible to re-key later without guessing at what
// an old value was encrypted with.
const envelopeV1 = "v1"

// ErrKeyPermissions is returned for a key file others can read. It is a
// refusal rather than a warning: silently using a world-readable key would
// make the encryption theatre.
var ErrKeyPermissions = errors.New("key file is readable by more than its owner")

// Keyring seals and opens secrets with one key.
type Keyring struct {
	aead cipher.AEAD
}

// NewKeyring builds a keyring from raw key material.
func NewKeyring(key []byte) (*Keyring, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Keyring{aead: aead}, nil
}

// LoadOrCreateKey reads the master key, generating one the first time.
//
// Creating it here rather than in an install script means a daemon started by
// hand, by systemd or by a test all arrive at the same place, and that there is
// no window in which the daemon runs without a key.
func LoadOrCreateKey(path string) (*Keyring, error) {
	key, err := os.ReadFile(path)
	switch {
	case err == nil:
		if perr := checkPermissions(path); perr != nil {
			return nil, perr
		}
		key, err = decodeKey(key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return NewKeyring(key)

	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		key = make([]byte, KeySize)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		// Written 0600 from the start: creating it readable and narrowing it
		// afterwards leaves the key exposed for as long as that takes.
		encoded := []byte(base64.StdEncoding.EncodeToString(key) + "\n")
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			return nil, err
		}
		return NewKeyring(key)

	default:
		return nil, err
	}
}

func decodeKey(raw []byte) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("not valid base64: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("key must be %d bytes, got %d", KeySize, len(key))
	}
	return key, nil
}

func checkPermissions(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("%w: %s is %#o, want 0600", ErrKeyPermissions, path, mode)
	}
	return nil
}

// Seal encrypts a secret into a string safe to store in the database.
//
// The nonce travels with the ciphertext because it has to be unique per
// message, not secret; generating it randomly per call is what keeps two
// identical tokens from producing identical rows.
func (k *Keyring) Seal(plaintext string) (string, error) {
	nonce := make([]byte, k.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := k.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return envelopeV1 + ":" + base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts what Seal produced. A modified ciphertext is an error rather
// than garbage, which is the point of using an AEAD.
func (k *Keyring) Open(sealed string) (string, error) {
	version, encoded, ok := strings.Cut(sealed, ":")
	if !ok {
		return "", errors.New("malformed ciphertext: no version prefix")
	}
	if version != envelopeV1 {
		return "", fmt.Errorf("unknown ciphertext version %q", version)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("malformed ciphertext: %w", err)
	}
	if len(raw) < k.aead.NonceSize() {
		return "", errors.New("malformed ciphertext: shorter than a nonce")
	}
	nonce, body := raw[:k.aead.NonceSize()], raw[k.aead.NonceSize():]
	plaintext, err := k.aead.Open(nil, nonce, body, nil)
	if err != nil {
		return "", fmt.Errorf("cannot decrypt: %w", err)
	}
	return string(plaintext), nil
}

// Fingerprint identifies which credential a runner was configured with,
// without revealing it: the reconciler needs to notice that a token changed,
// and comparing sealed values cannot tell it anything, since sealing the same
// token twice gives two different strings.
func (k *Keyring) Fingerprint(plaintext string) string {
	// Deterministic by construction: a fixed nonce over a value that is never
	// stored, only compared. The output is truncated so it cannot be attacked
	// as a ciphertext.
	nonce := make([]byte, k.aead.NonceSize())
	sealed := k.aead.Seal(nil, nonce, []byte(plaintext), nil)
	return base64.RawURLEncoding.EncodeToString(sealed)[:16]
}
