package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/secrets"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	ring, err := secrets.LoadOrCreateKey(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(dir, "fleet.db"), ring)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func credential(t *testing.T, s *Store) model.Credential {
	t.Helper()
	c, err := s.CreateCredential(context.Background(), "default", "github_pat_secret_value")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func samplePool(credentialID int64) model.Pool {
	return model.Pool{
		Name:         "web",
		ScopeKind:    model.ScopeRepository,
		Scope:        "clems4ever/runyard",
		Runtime:      model.RuntimeVM,
		Replicas:     2,
		Labels:       []string{"fast"},
		CredentialID: credentialID,
		Enabled:      true,
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	dir := t.TempDir()
	ring, err := secrets.LoadOrCreateKey(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "fleet.db")

	first, err := Open(path, ring)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	cred := credential(t, first)
	if _, err := first.CreatePool(context.Background(), samplePool(cred.ID)); err != nil {
		t.Fatal(err)
	}
	first.Close()

	// Reopening is what happens on every daemon restart, and it must not lose
	// anything or try to create the schema twice.
	second, err := Open(path, ring)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	pools, err := second.ListPools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 1 || pools[0].Name != "web" {
		t.Fatalf("the pool did not survive a restart: %+v", pools)
	}
}

func TestCredentialTokenIsNotStoredInClear(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	const token = "github_pat_11ONLY_IN_MEMORY"
	c, err := s.CreateCredential(ctx, "pat", token)
	if err != nil {
		t.Fatal(err)
	}

	// Straight at the row: whatever is on disk must not contain the token.
	var sealed string
	if err := s.db.QueryRowContext(ctx, `SELECT sealed FROM credentials WHERE id = ?`, c.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, token) {
		t.Fatalf("the token is in the database in clear: %q", sealed)
	}

	got, err := s.Token(ctx, c.ID)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if got != token {
		t.Fatalf("got %q, want the original token", got)
	}
}

func TestCredentialHintIdentifiesWithoutRevealing(t *testing.T) {
	s := newStore(t)
	c, err := s.CreateCredential(context.Background(), "pat", "github_pat_ABCD1234")
	if err != nil {
		t.Fatal(err)
	}
	if c.Hint != "…1234" {
		t.Fatalf("hint is %q", c.Hint)
	}
}

func TestCredentialNamesAreUnique(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.CreateCredential(ctx, "pat", "one"); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateCredential(ctx, "pat", "two")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

// Deleting a credential a pool depends on would leave that pool unable to
// register anything, so it is refused rather than cascaded.
func TestDeleteCredentialRefusesWhileInUse(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	cred := credential(t, s)
	if _, err := s.CreatePool(ctx, samplePool(cred.ID)); err != nil {
		t.Fatal(err)
	}

	err := s.DeleteCredential(ctx, cred.ID)
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("want ErrInUse, got %v", err)
	}

	pools, _ := s.ListPools(ctx)
	if err := s.DeletePool(ctx, pools[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteCredential(ctx, cred.ID); err != nil {
		t.Fatalf("deleting an unused credential failed: %v", err)
	}
}

func TestReplaceCredentialTokenChangesTheFingerprint(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	cred := credential(t, s)

	before, err := s.CredentialFingerprint(ctx, cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceCredentialToken(ctx, cred.ID, "github_pat_rotated"); err != nil {
		t.Fatal(err)
	}
	after, err := s.CredentialFingerprint(ctx, cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("rotating the token left the fingerprint alone, so runners would keep the old one")
	}

	token, err := s.Token(ctx, cred.ID)
	if err != nil || token != "github_pat_rotated" {
		t.Fatalf("got %q, %v", token, err)
	}
}

func TestPoolRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	cred := credential(t, s)

	created, err := s.CreatePool(ctx, samplePool(cred.ID))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("no id assigned")
	}

	got, err := s.Pool(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "web" || got.Scope != "clems4ever/runyard" || got.Replicas != 2 {
		t.Fatalf("round trip changed the pool: %+v", got)
	}
	if len(got.Labels) != 1 || got.Labels[0] != "fast" {
		t.Fatalf("labels came back as %v", got.Labels)
	}
	// Defaults must be persisted, not reapplied differently on the way out.
	if got.CPUs != 2 || got.MemoryMB != 4096 || got.DiskGB != 40 {
		t.Fatalf("sizing came back as %d/%d/%d", got.CPUs, got.MemoryMB, got.DiskGB)
	}
}

func TestPoolWithoutLabelsComesBackAsAnEmptyList(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	cred := credential(t, s)

	p := samplePool(cred.ID)
	p.Labels = nil
	created, err := s.CreatePool(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Pool(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Not nil: the API serialises this straight to JSON, and null where the UI
	// expects a list is a crash in the browser rather than an empty table.
	if got.Labels == nil {
		t.Fatal("labels came back as nil rather than an empty list")
	}
}

func TestCreatePoolRejectsInvalid(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	cred := credential(t, s)

	p := samplePool(cred.ID)
	p.Name = "Not Valid"
	if _, err := s.CreatePool(ctx, p); err == nil {
		t.Fatal("an invalid pool was stored")
	}
}

func TestCreatePoolRejectsAnUnknownCredential(t *testing.T) {
	s := newStore(t)
	p := samplePool(999)
	_, err := s.CreatePool(context.Background(), p)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestPoolNamesAreUnique(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	cred := credential(t, s)
	if _, err := s.CreatePool(ctx, samplePool(cred.ID)); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreatePool(ctx, samplePool(cred.ID))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want ErrConflict, got %v", err)
	}
}

func TestUpdatePool(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	cred := credential(t, s)
	created, err := s.CreatePool(ctx, samplePool(cred.ID))
	if err != nil {
		t.Fatal(err)
	}

	created.Replicas = 5
	created.Nested = true
	created.Labels = []string{"gpu"}
	updated, err := s.UpdatePool(ctx, created)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Replicas != 5 || !updated.Nested || updated.Labels[0] != "gpu" {
		t.Fatalf("update did not stick: %+v", updated)
	}
	if !updated.UpdatedAt.After(created.CreatedAt) && !updated.UpdatedAt.Equal(created.CreatedAt) {
		t.Fatal("updated_at went backwards")
	}
}

func TestUpdateUnknownPool(t *testing.T) {
	s := newStore(t)
	cred := credential(t, s)
	p := samplePool(cred.ID)
	p.ID = 999
	_, err := s.UpdatePool(context.Background(), p)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestDeleteUnknownPool(t *testing.T) {
	s := newStore(t)
	if err := s.DeletePool(context.Background(), 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSettings(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	got, err := s.Setting(ctx, "auth.user")
	if err != nil || got != "" {
		t.Fatalf("an unset setting should be empty, got %q, %v", got, err)
	}
	if err := s.SetSetting(ctx, "auth.user", "admin"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(ctx, "auth.user", "clement"); err != nil {
		t.Fatal(err)
	}
	got, err = s.Setting(ctx, "auth.user")
	if err != nil || got != "clement" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestListPoolsIsOrdered(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	cred := credential(t, s)

	for _, name := range []string{"web", "api", "docs"} {
		p := samplePool(cred.ID)
		p.Name = name
		if _, err := s.CreatePool(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	pools, err := s.ListPools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range pools {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "api,docs,web" {
		t.Fatalf("got %v, want them sorted so the UI does not shuffle", names)
	}
}
