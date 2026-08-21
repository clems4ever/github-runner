package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	ring := testKeyring(t)

	const token = "github_pat_11ABCDEFG0123456789"
	sealed, err := ring.Seal(token)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if strings.Contains(sealed, token) {
		t.Fatalf("the token appears in its own ciphertext: %q", sealed)
	}
	if !strings.HasPrefix(sealed, "v1:") {
		t.Fatalf("want a version prefix, got %q", sealed)
	}

	got, err := ring.Open(sealed)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got != token {
		t.Fatalf("round trip changed the token: got %q, want %q", got, token)
	}
}

// Two rows holding the same token must not look the same, or the database
// leaks which pools share a credential.
func TestSealIsNotDeterministic(t *testing.T) {
	ring := testKeyring(t)

	first, err := ring.Seal("same-token")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ring.Seal("same-token")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("sealing one token twice produced identical ciphertext")
	}
}

func TestOpenRejectsTamperedCiphertext(t *testing.T) {
	ring := testKeyring(t)
	sealed, err := ring.Seal("github_pat_original")
	if err != nil {
		t.Fatal(err)
	}

	// Flip a character in the body. An AEAD has to notice.
	body := []byte(sealed)
	i := len(body) - 3
	if body[i] == 'A' {
		body[i] = 'B'
	} else {
		body[i] = 'A'
	}

	if _, err := ring.Open(string(body)); err == nil {
		t.Fatal("a tampered ciphertext decrypted without complaint")
	}
}

func TestOpenRejectsAnotherKeyring(t *testing.T) {
	sealed, err := testKeyring(t).Seal("github_pat_original")
	if err != nil {
		t.Fatal(err)
	}
	other, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "other.key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("a different key opened the ciphertext")
	}
}

func TestOpenRejectsMalformedInput(t *testing.T) {
	ring := testKeyring(t)
	for _, in := range []string{
		"",
		"no-version-prefix",
		"v2:" + strings.Repeat("A", 40),
		"v1:not-base64!!",
		"v1:QQ==", // shorter than a nonce
	} {
		if _, err := ring.Open(in); err == nil {
			t.Errorf("%q decrypted without complaint", in)
		}
	}
}

func TestLoadOrCreateKeyPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "master.key")

	first, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	sealed, err := first.Seal("github_pat_persisted")
	if err != nil {
		t.Fatal(err)
	}

	// A daemon restart must be able to read what the last one wrote.
	second, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	got, err := second.Open(sealed)
	if err != nil {
		t.Fatalf("reloaded key cannot open its own ciphertext: %v", err)
	}
	if got != "github_pat_persisted" {
		t.Fatalf("got %q", got)
	}
}

func TestNewKeyIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	if _, err := LoadOrCreateKey(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("new key is %#o, want 0600", mode)
	}
}

// A key anyone can read makes the encryption theatre, so loading it is a
// refusal rather than a warning.
func TestLoadRejectsLoosePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	if _, err := LoadOrCreateKey(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadOrCreateKey(path)
	if !errors.Is(err, ErrKeyPermissions) {
		t.Fatalf("want ErrKeyPermissions, got %v", err)
	}
}

func TestLoadRejectsUnusableKeyFile(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"short.key":      "QUJD\n", // valid base64, too few bytes
		"not-base64.key": "this is not base64 at all!\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateKey(path); err == nil {
			t.Errorf("%s loaded without complaint", name)
		}
	}
}

// The reconciler compares fingerprints to notice that a pool's credential was
// replaced; sealed values cannot be compared, since they differ every time.
func TestFingerprintIsStableAndDistinct(t *testing.T) {
	ring := testKeyring(t)

	a := ring.Fingerprint("token-one")
	if a != ring.Fingerprint("token-one") {
		t.Fatal("the same token fingerprinted differently twice")
	}
	if a == ring.Fingerprint("token-two") {
		t.Fatal("two tokens share a fingerprint")
	}
	if strings.Contains(a, "token-one") {
		t.Fatalf("fingerprint leaks the token: %q", a)
	}

	// Every GitHub token starts the same way. A fingerprint that only reflects
	// the first few characters would report a rotated credential as unchanged,
	// and the runners would keep using the one that was just revoked.
	first := ring.Fingerprint("github_pat_11ABCDEFGHIJKLMNOP_first")
	second := ring.Fingerprint("github_pat_11ABCDEFGHIJKLMNOP_second")
	if first == second {
		t.Fatal("two tokens sharing a long prefix have the same fingerprint")
	}
}

// A fingerprint is bound to the key: two hosts must not be able to compare
// theirs and learn they hold the same credential.
func TestFingerprintDependsOnTheKey(t *testing.T) {
	one := testKeyring(t)
	two := testKeyring(t)
	if one.Fingerprint("same-token") == two.Fingerprint("same-token") {
		t.Fatal("the fingerprint does not depend on the key")
	}
}

func testKeyring(t *testing.T) *Keyring {
	t.Helper()
	ring, err := LoadOrCreateKey(filepath.Join(t.TempDir(), "master.key"))
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	return ring
}
