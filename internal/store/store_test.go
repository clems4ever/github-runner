package store

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clems4ever/github-runner/internal/github"
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
	c, err := s.CreateCredential(context.Background(), model.Credential{Name: "default"}, "github_pat_secret_value")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func samplePool(credentialID int64) model.Pool {
	return model.Pool{
		Name:        "web",
		ScopeKind:   model.ScopeRepository,
		Scope:       "clems4ever/runyard",
		Runtime:     model.RuntimeVM,
		MinReplicas: 2, MaxReplicas: 2,
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
	c, err := s.CreateCredential(ctx, model.Credential{Name: "pat"}, token)
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

	got, err := s.Secret(ctx, c.ID)
	if err != nil {
		t.Fatalf("secret: %v", err)
	}
	if got.Token != token {
		t.Fatalf("got %q, want the original token", got.Token)
	}
	if got.Kind != model.CredentialPAT {
		t.Fatalf("kind came back as %q", got.Kind)
	}
}

func TestCredentialHintIdentifiesWithoutRevealing(t *testing.T) {
	s := newStore(t)
	c, err := s.CreateCredential(context.Background(), model.Credential{Name: "pat"}, "github_pat_ABCD1234")
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
	if _, err := s.CreateCredential(ctx, model.Credential{Name: "pat"}, "one"); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateCredential(ctx, model.Credential{Name: "pat"}, "two")
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
	if err := s.ReplaceCredentialSecret(ctx, cred.ID, "github_pat_rotated"); err != nil {
		t.Fatal(err)
	}
	after, err := s.CredentialFingerprint(ctx, cred.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("rotating the token left the fingerprint alone, so runners would keep the old one")
	}

	secret, err := s.Secret(ctx, cred.ID)
	if err != nil || secret.Token != "github_pat_rotated" {
		t.Fatalf("got %q, %v", secret.Token, err)
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
	if got.Name != "web" || got.Scope != "clems4ever/runyard" || got.MinReplicas != 2 || got.MaxReplicas != 2 {
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

	created.MinReplicas, created.MaxReplicas = 5, 5
	created.Nested = true
	created.Labels = []string{"gpu"}
	updated, err := s.UpdatePool(ctx, created)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.MaxReplicas != 5 || !updated.Nested || updated.Labels[0] != "gpu" {
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

// An existing installation must survive the move to autoscaling. A pool that
// was a fixed three runners becomes a pool whose minimum and maximum are both
// three — exactly what it was — so an upgrade changes nothing until someone
// raises the maximum.
func TestUpgradingADatabaseFromBeforeAutoscaling(t *testing.T) {
	dir := t.TempDir()
	ring, err := secrets.LoadOrCreateKey(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "fleet.db")

	// Build the schema as it stood before autoscaling: the first three
	// migrations, and a pool with a fixed replica count.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range append([]string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		`INSERT INTO schema_version (version) VALUES (3)`,
	}, migrations[:3]...) {
		if _, err := old.Exec(statement); err != nil {
			t.Fatalf("building the old schema: %v", err)
		}
	}
	if _, err := old.Exec(
		`INSERT INTO credentials (name, sealed, hint, created_at) VALUES ('pat', 'x', '…1234', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(
		`INSERT INTO pools (name, scope_kind, scope, runtime, nested, ephemeral, replicas, labels,
			cpus, memory_mb, disk_gb, image, credential_id, enabled, created_at, updated_at)
		 VALUES ('web','repository','o/r','vm',0,1,3,'fast',2,4096,40,'default',1,1,
			'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatal(err)
	}
	old.Close()

	// Now open it with the current code, which is what an upgrade does.
	s, err := Open(path, ring)
	if err != nil {
		t.Fatalf("upgrading: %v", err)
	}
	defer s.Close()

	pools, err := s.ListPools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 1 {
		t.Fatalf("got %d pools", len(pools))
	}
	pool := pools[0]
	if pool.MinReplicas != 3 || pool.MaxReplicas != 3 {
		t.Fatalf("the pool became %d..%d, want the fixed 3 it already was", pool.MinReplicas, pool.MaxReplicas)
	}
	if pool.Elastic() {
		t.Fatal("an upgraded pool started scaling on its own, which nobody asked for")
	}
	// The rest of it must come through untouched.
	if pool.Name != "web" || pool.Scope != "o/r" || pool.CPUs != 2 || pool.Labels[0] != "fast" {
		t.Fatalf("the upgrade changed the pool: %+v", pool)
	}
}

// History recorded before scopes were kept is backfilled from the pools that
// are still there, so an upgraded installation can be read by scope straight
// away rather than after the old samples have aged out.
func TestUpgradingADatabaseFromBeforeScopedHistory(t *testing.T) {
	dir := t.TempDir()
	ring, err := secrets.LoadOrCreateKey(filepath.Join(dir, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "fleet.db")
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	// Every migration up to but not including the scope column: the schema as
	// it stood when samples knew only which pool they came from.
	before := len(migrations) - 2
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range append([]string{
		`CREATE TABLE schema_version (version INTEGER NOT NULL)`,
		fmt.Sprintf(`INSERT INTO schema_version (version) VALUES (%d)`, before),
	}, migrations[:before]...) {
		if _, err := old.Exec(statement); err != nil {
			t.Fatalf("building the old schema: %v", err)
		}
	}
	if _, err := old.Exec(
		`INSERT INTO credentials (name, sealed, hint, created_at) VALUES ('pat', 'x', '…1234', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(
		`INSERT INTO pools (name, scope_kind, scope, runtime, nested, ephemeral, replicas, labels,
			cpus, memory_mb, disk_gb, image, credential_id, enabled, created_at, updated_at,
			min_replicas, max_replicas)
		 VALUES ('web','repository','acme/site','vm',0,1,3,'fast',2,4096,40,'default',1,1,
			'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z',3,3)`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(
		`INSERT INTO samples (at, pool, running, busy, target) VALUES (?, 'web', 3, 2, 3)`,
		base.Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := Open(path, ring)
	if err != nil {
		t.Fatalf("upgrading: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	points, err := s.Activity(ctx, base.Add(-time.Hour), base.Add(time.Hour), 60,
		ActivityFilter{Scope: "acme/site"})
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].Busy != 2 {
		t.Fatalf("got %+v, want the old sample attributed to its pool's scope", points)
	}
}

func TestActivityBucketsThePeaks(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	// Two pools, sampled every thirty seconds for ten minutes. One of them has
	// a brief burst: every runner busy for a single sample.
	for i := 0; i < 20; i++ {
		at := base.Add(time.Duration(i) * 30 * time.Second)
		busy := 1
		if i == 7 {
			busy = 4 // the burst
		}
		if err := s.RecordSamples(ctx, at, []model.Sample{
			{Pool: "web", Running: 4, Busy: busy, Target: 4},
			{Pool: "api", Running: 2, Busy: 0, Target: 2},
		}); err != nil {
			t.Fatal(err)
		}
	}

	points, err := s.Activity(ctx, base, base.Add(10*time.Minute), 10, ActivityFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(points) == 0 {
		t.Fatal("no history came back")
	}

	// Each point is the whole fleet, both pools added together.
	for _, point := range points {
		if point.Running != 6 {
			t.Fatalf("a point reports %d runners, want both pools counted: %+v", point.Running, point)
		}
	}

	// The burst has to survive bucketing. A mean over a minute would flatten
	// four busy runners into one and a bit, which is the opposite of what the
	// chart is for.
	var peak int
	for _, point := range points {
		if point.Busy > peak {
			peak = point.Busy
		}
	}
	if peak != 4 {
		t.Fatalf("the peak came back as %d, want the burst preserved", peak)
	}
}

// A daemon that was not running should read as a gap, not as a fleet that was
// switched off.
func TestActivityLeavesGapsWhereThereIsNoHistory(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	if err := s.RecordSamples(ctx, base, []model.Sample{{Pool: "web", Running: 2, Busy: 1}}); err != nil {
		t.Fatal(err)
	}
	points, err := s.Activity(ctx, base, base.Add(time.Hour), 60, ActivityFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 {
		t.Fatalf("got %d points, want only the one that was recorded", len(points))
	}
}

func TestActivityForgetsOldHistory(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	old := now.Add(-SampleRetention - time.Hour)
	if err := s.RecordSamples(ctx, old, []model.Sample{{Pool: "web", Running: 9, Busy: 9}}); err != nil {
		t.Fatal(err)
	}
	// Recording again is what prunes: the daemon does it every pass, so nothing
	// has to remember to tidy up.
	if err := s.RecordSamples(ctx, now, []model.Sample{{Pool: "web", Running: 1, Busy: 0}}); err != nil {
		t.Fatal(err)
	}

	points, err := s.Activity(ctx, old.Add(-time.Hour), now, 100, ActivityFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, point := range points {
		if point.Running == 9 {
			t.Fatal("history older than the retention window was kept")
		}
	}
}

func TestActivityOnAFreshInstall(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	points, err := s.Activity(context.Background(), now.Add(-time.Hour), now, 60, ActivityFilter{})
	if err != nil {
		t.Fatal(err)
	}
	// Empty, not nil: this is serialised straight to JSON, and null where the
	// chart expects a list is a crash in the browser.
	if points == nil {
		t.Fatal("no history came back as nil rather than an empty list")
	}
}

// The UI can narrow the history to one pool, which is how someone looks at a
// single repository without a chart per pool crowding the page.
func TestActivityCanBeNarrowedToOnePool(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 4; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		if err := s.RecordSamples(ctx, at, []model.Sample{
			{Pool: "web", Running: 4, Busy: 3},
			{Pool: "api", Running: 2, Busy: 1},
		}); err != nil {
			t.Fatal(err)
		}
	}

	whole, err := s.Activity(ctx, base, base.Add(time.Hour), 60, ActivityFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if whole[0].Running != 6 || whole[0].Busy != 4 {
		t.Fatalf("the fleet-wide view is %+v, want both pools added together", whole[0])
	}

	just, err := s.Activity(ctx, base, base.Add(time.Hour), 60, ActivityFilter{Pool: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if just[0].Running != 2 || just[0].Busy != 1 {
		t.Fatalf("the api view is %+v, want only that pool", just[0])
	}

	// A pool with no history is empty, not an error: a pool created a moment
	// ago honestly has nothing to show.
	none, err := s.Activity(ctx, base, base.Add(time.Hour), 60, ActivityFilter{Pool: "never-existed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("got %+v", none)
	}
}

// Several pools can point at the same repository or organisation, and asking
// by scope is asking about the repository, not about whichever pool happens to
// be serving it.
func TestActivityCanBeNarrowedToOneScope(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 4; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		if err := s.RecordSamples(ctx, at, []model.Sample{
			{Pool: "web", Scope: "acme/site", Running: 4, Busy: 3},
			{Pool: "web-arm", Scope: "acme/site", Running: 1, Busy: 1},
			{Pool: "api", Scope: "acme/api", Running: 2, Busy: 1},
		}); err != nil {
			t.Fatal(err)
		}
	}

	site, err := s.Activity(ctx, base, base.Add(time.Hour), 60, ActivityFilter{Scope: "acme/site"})
	if err != nil {
		t.Fatal(err)
	}
	if site[0].Running != 5 || site[0].Busy != 4 {
		t.Fatalf("the acme/site view is %+v, want both of its pools added together", site[0])
	}

	// Both together is one pool checked against its own scope, which is what
	// the two filters in the UI compose to.
	both, err := s.Activity(ctx, base, base.Add(time.Hour), 60,
		ActivityFilter{Pool: "web", Scope: "acme/site"})
	if err != nil {
		t.Fatal(err)
	}
	if both[0].Running != 4 {
		t.Fatalf("pool and scope together gave %+v, want just the one pool", both[0])
	}

	scopes, err := s.ActivityScopes(ctx, base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(scopes, ",") != "acme/api,acme/site" {
		t.Fatalf("got %v, want both scopes, sorted so the UI does not shuffle", scopes)
	}
}

// A pool can be deleted; the hours it worked still happened. The history keeps
// the scope it was recorded against, so the work stays attributed to the
// repository until it ages out of the retention window.
func TestActivityKeepsTheScopeOfADeletedPool(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	cred := credential(t, s)
	pool, err := s.CreatePool(ctx, samplePool(cred.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSamples(ctx, base, []model.Sample{
		{Pool: pool.Name, Scope: pool.Scope, Running: 3, Busy: 2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePool(ctx, pool.ID); err != nil {
		t.Fatal(err)
	}

	points, err := s.Activity(ctx, base, base.Add(time.Hour), 60, ActivityFilter{Scope: pool.Scope})
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].Busy != 2 {
		t.Fatalf("got %+v, want the deleted pool's work still counted against its scope", points)
	}

	// And the scope stays on offer for as long as there is something to show.
	scopes, err := s.ActivityScopes(ctx, base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 1 || scopes[0] != pool.Scope {
		t.Fatalf("got %v, want %q", scopes, pool.Scope)
	}
}

// Samples recorded before scopes were kept have none. They still count towards
// the fleet, and they are not offered as a scope with no name.
func TestActivityScopesIgnoresHistoryFromBeforeScopesWereRecorded(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	if err := s.RecordSamples(ctx, base, []model.Sample{{Pool: "web", Running: 2, Busy: 1}}); err != nil {
		t.Fatal(err)
	}
	scopes, err := s.ActivityScopes(ctx, base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Empty, not nil: this is serialised straight to JSON, and null where the
	// UI expects a list is a crash in the browser.
	if scopes == nil {
		t.Fatal("no scopes came back as nil rather than an empty list")
	}
	if len(scopes) != 0 {
		t.Fatalf("got %v, want nothing offered for history with no scope", scopes)
	}

	fleet, err := s.Activity(ctx, base, base.Add(time.Hour), 60, ActivityFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fleet) != 1 || fleet[0].Running != 2 {
		t.Fatalf("got %+v, want the unscoped history still counted", fleet)
	}
}

// ---------------------------------------------------------------------------
// GitHub App credentials
// ---------------------------------------------------------------------------

// testAppKey is a real RSA key in the format GitHub issues.
func testAppKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

func TestAppCredentialRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	key := testAppKey(t)

	created, err := s.CreateCredential(ctx, model.Credential{
		Name: "runyard app", Kind: model.CredentialApp, AppID: 123456, InstallationID: 42,
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	// The hint says which app it is, since there is no token tail to show.
	if created.Hint != "app 123456" {
		t.Fatalf("hint is %q", created.Hint)
	}

	secret, err := s.Secret(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !secret.IsApp() || secret.AppID != 123456 || secret.InstallationID != 42 {
		t.Fatalf("got %+v", secret)
	}
	// Trimmed of the whitespace a paste picks up, but the same key: it still
	// parses, which is what the agent will do with it.
	if secret.Token != strings.TrimSpace(key) {
		t.Fatal("the private key did not survive the round trip")
	}
	if _, err := github.ParsePrivateKey([]byte(secret.Token)); err != nil {
		t.Fatalf("what came back is no longer a usable key: %v", err)
	}

	// And the list still says nothing secret.
	credentials, err := s.ListCredentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if credentials[0].Kind != model.CredentialApp || credentials[0].AppID != 123456 {
		t.Fatalf("got %+v", credentials[0])
	}
}

func TestTheAppKeyIsNotStoredInClear(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	key := testAppKey(t)

	created, err := s.CreateCredential(ctx,
		model.Credential{Name: "app", Kind: model.CredentialApp, AppID: 1}, key)
	if err != nil {
		t.Fatal(err)
	}

	var sealed string
	if err := s.db.QueryRowContext(ctx, `SELECT sealed FROM credentials WHERE id = ?`, created.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "PRIVATE KEY") {
		t.Fatalf("the key is on disk in clear: %q", sealed[:60])
	}
}

// A key that went wrong in the paste has to be caught here, while whoever
// pasted it is still looking — not at the first runner boot an hour later.
func TestAppCredentialsAreCheckedBeforeTheyAreStored(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	tests := []struct {
		name       string
		credential model.Credential
		secret     string
		want       string
	}{
		{
			name:       "no app id",
			credential: model.Credential{Name: "a", Kind: model.CredentialApp},
			secret:     testAppKey(t),
			want:       "app id",
		},
		{
			name:       "a token pasted where the key goes",
			credential: model.Credential{Name: "b", Kind: model.CredentialApp, AppID: 1},
			secret:     "github_pat_11ABCDEF",
			want:       "not a PEM file",
		},
		{
			name:       "a public key",
			credential: model.Credential{Name: "c", Kind: model.CredentialApp, AppID: 1},
			secret:     "-----BEGIN PUBLIC KEY-----\nMIIBIjAN\n-----END PUBLIC KEY-----\n",
			want:       "private key",
		},
		{
			name:       "a kind nobody has heard of",
			credential: model.Credential{Name: "d", Kind: "oauth"},
			secret:     "x",
			want:       "credential kind",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.CreateCredential(ctx, tt.credential, tt.secret)
			if err == nil {
				t.Fatal("stored a credential that cannot work")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("the message does not say what is wrong: %v", err)
			}
		})
	}
}

// Rotating must not be a side door around the checks.
func TestRotatingAnAppKeyIsCheckedToo(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	created, err := s.CreateCredential(ctx,
		model.Credential{Name: "app", Kind: model.CredentialApp, AppID: 7}, testAppKey(t))
	if err != nil {
		t.Fatal(err)
	}

	if err := s.ReplaceCredentialSecret(ctx, created.ID, "github_pat_not_a_key"); err == nil {
		t.Fatal("a token was accepted as an app's private key")
	}

	replacement := testAppKey(t)
	if err := s.ReplaceCredentialSecret(ctx, created.ID, replacement); err != nil {
		t.Fatal(err)
	}
	secret, err := s.Secret(ctx, created.ID)
	if err != nil || secret.Token != strings.TrimSpace(replacement) {
		t.Fatalf("the new key did not stick: %v", err)
	}
	// The app it belongs to is unchanged, so the hint is too.
	if secret.AppID != 7 {
		t.Fatalf("the app id changed to %d", secret.AppID)
	}
}

// The fingerprint drives the generation, so anything that changes which
// credential a runner is using has to move it.
func TestAppFingerprintCoversMoreThanTheKey(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	key := testAppKey(t)

	first, err := s.CreateCredential(ctx,
		model.Credential{Name: "one", Kind: model.CredentialApp, AppID: 1}, key)
	if err != nil {
		t.Fatal(err)
	}
	// The same key, a different app: a different credential in every way that
	// matters, and the runners have to be rebuilt.
	second, err := s.CreateCredential(ctx,
		model.Credential{Name: "two", Kind: model.CredentialApp, AppID: 2}, key)
	if err != nil {
		t.Fatal(err)
	}

	a, err := s.CredentialFingerprint(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CredentialFingerprint(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two apps sharing a key share a fingerprint")
	}

	// And a token credential is never confused with an app one.
	pat, err := s.CreateCredential(ctx, model.Credential{Name: "pat"}, key)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.CredentialFingerprint(ctx, pat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if c == a {
		t.Fatal("a token and an app with the same secret share a fingerprint")
	}
}

// An installation that existed before GitHub Apps did must keep working
// untouched.
func TestUpgradingADatabaseFromBeforeApps(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	created, err := s.CreateCredential(ctx, model.Credential{Name: "old"}, "github_pat_11EXISTING")
	if err != nil {
		t.Fatal(err)
	}
	// A row written before the app columns existed defaults to what it was.
	if created.Kind != model.CredentialPAT {
		t.Fatalf("kind is %q", created.Kind)
	}
	secret, err := s.Secret(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if secret.IsApp() || secret.Token != "github_pat_11EXISTING" {
		t.Fatalf("got %+v", secret)
	}
}

func TestHostHistoryKeepsThePeakOfEachBucket(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	start := time.Now().UTC().Add(-time.Hour)

	// Three readings inside one bucket. The middle one is a build storm, and a
	// mean over the bucket would leave no trace of it at all.
	for i, cpu := range []float64{5, 92, 7} {
		if err := s.RecordHostSample(ctx, start.Add(time.Duration(i)*time.Second), model.HostSample{
			CPUPercent:       cpu,
			MemoryUsedBytes:  int64(i+1) * 1024,
			MemoryTotalBytes: 4096,
			DiskUsedBytes:    1024,
			DiskTotalBytes:   4096,
		}); err != nil {
			t.Fatal(err)
		}
	}

	points, err := s.HostHistory(ctx, start.Add(-time.Minute), time.Now().UTC(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 {
		t.Fatalf("want the readings in one bucket, got %d: %+v", len(points), points)
	}
	if points[0].CPUPercent != 92 {
		t.Fatalf("want the peak, got %v", points[0].CPUPercent)
	}
	if points[0].MemoryPercent != 75 {
		t.Fatalf("want the peak memory as a share of the total, got %v", points[0].MemoryPercent)
	}
	if points[0].DiskPercent != 25 {
		t.Fatalf("disk: got %v", points[0].DiskPercent)
	}
}

// A daemon that was not running should read as a gap in the line rather than
// as a host that was switched off and using nothing.
func TestHostHistoryLeavesEmptyBucketsOut(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	until := time.Now().UTC()

	if err := s.RecordHostSample(ctx, until.Add(-time.Minute), model.HostSample{
		CPUPercent: 10, MemoryUsedBytes: 1, MemoryTotalBytes: 2,
	}); err != nil {
		t.Fatal(err)
	}

	points, err := s.HostHistory(ctx, until.Add(-6*time.Hour), until, 180)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 {
		t.Fatalf("want only the bucket that has a reading, got %d", len(points))
	}
}

func TestHostHistoryIsPrunedToTheRetentionWindow(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.RecordHostSample(ctx, now.Add(-SampleRetention-time.Hour), model.HostSample{CPUPercent: 50}); err != nil {
		t.Fatal(err)
	}
	// Writing the next one is what prunes: nobody has to think about the size
	// of this table.
	if err := s.RecordHostSample(ctx, now, model.HostSample{CPUPercent: 10}); err != nil {
		t.Fatal(err)
	}

	points, err := s.HostHistory(ctx, now.Add(-SampleRetention-2*time.Hour), now.Add(time.Minute), 180)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].CPUPercent != 10 {
		t.Fatalf("the aged-out reading is still here: %+v", points)
	}
}

// A host whose totals are zero — a disk that could not be measured — must not
// divide by them.
func TestHostHistorySurvivesAMeasurementThatFailed(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.RecordHostSample(ctx, now, model.HostSample{CPUPercent: 3}); err != nil {
		t.Fatal(err)
	}
	points, err := s.HostHistory(ctx, now.Add(-time.Hour), now.Add(time.Minute), 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].DiskPercent != 0 {
		t.Fatalf("got %+v", points)
	}
}
