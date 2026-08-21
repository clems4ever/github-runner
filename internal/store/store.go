// Package store keeps the fleet's desired state.
//
// Only desired state. What is running on the host is read back from systemd
// and Docker every time it is needed, because a row claiming a runner is up
// survives a reboot that the runner did not.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
	"github.com/clems4ever/github-runner/internal/secrets"

	_ "modernc.org/sqlite" // pure Go, so the daemon cross-compiles and needs no cgo
)

// Errors the API layer turns into status codes.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
	ErrInUse    = errors.New("still in use")
)

// Store is the daemon's database.
type Store struct {
	db   *sql.DB
	ring *secrets.Keyring
}

// Open opens or creates the database and brings the schema up to date.
func Open(path string, ring *secrets.Keyring) (*Store, error) {
	// Foreign keys are off by default in SQLite, and they are what stops a
	// credential being deleted out from under the pools using it. WAL keeps
	// the UI's reads from blocking the reconciler's writes.
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s := &Store{db: db, ring: ring}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// migrations are applied in order and never edited once released; a new one is
// appended instead, so a database from any earlier version arrives at the same
// schema as a fresh one.
var migrations = []string{
	`CREATE TABLE credentials (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		name       TEXT NOT NULL UNIQUE,
		sealed     TEXT NOT NULL,
		hint       TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE pools (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		name          TEXT NOT NULL UNIQUE,
		scope_kind    TEXT NOT NULL,
		scope         TEXT NOT NULL,
		runtime       TEXT NOT NULL,
		nested        INTEGER NOT NULL,
		ephemeral     INTEGER NOT NULL,
		replicas      INTEGER NOT NULL,
		labels        TEXT NOT NULL,
		cpus          INTEGER NOT NULL,
		memory_mb     INTEGER NOT NULL,
		disk_gb       INTEGER NOT NULL,
		image         TEXT NOT NULL,
		credential_id INTEGER NOT NULL REFERENCES credentials(id),
		enabled       INTEGER NOT NULL,
		created_at    TEXT NOT NULL,
		updated_at    TEXT NOT NULL
	)`,
	`CREATE TABLE settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	var version int
	err := s.db.QueryRowContext(ctx, `SELECT version FROM schema_version`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (0)`); err != nil {
			return err
		}
		version = 0
	} else if err != nil {
		return err
	}

	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = ?`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

// CreateCredential seals a token and stores it. The token is never written in
// clear, and never returned again — only used.
func (s *Store) CreateCredential(ctx context.Context, name, token string) (model.Credential, error) {
	name = strings.TrimSpace(name)
	token = strings.TrimSpace(token)
	if name == "" {
		return model.Credential{}, errors.New("a credential needs a name to tell it from the others")
	}
	if token == "" {
		return model.Credential{}, errors.New("the token is empty")
	}

	sealed, err := s.ring.Seal(token)
	if err != nil {
		return model.Credential{}, err
	}

	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO credentials (name, sealed, hint, created_at) VALUES (?, ?, ?, ?)`,
		name, sealed, hintOf(token), now.Format(time.RFC3339Nano))
	if err != nil {
		if isUnique(err) {
			return model.Credential{}, fmt.Errorf("credential %q: %w", name, ErrConflict)
		}
		return model.Credential{}, err
	}
	id, _ := res.LastInsertId()
	return model.Credential{ID: id, Name: name, Hint: hintOf(token), CreatedAt: now}, nil
}

// hintOf is the tail of a token, which is enough to tell two apart in a list
// without showing anything usable.
func hintOf(token string) string {
	if len(token) <= 4 {
		return "****"
	}
	return "…" + token[len(token)-4:]
}

// ListCredentials returns every credential, without their tokens.
func (s *Store) ListCredentials(ctx context.Context) ([]model.Credential, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, hint, created_at FROM credentials ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.Credential{}
	for rows.Next() {
		var c model.Credential
		var created string
		if err := rows.Scan(&c.ID, &c.Name, &c.Hint, &created); err != nil {
			return nil, err
		}
		c.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Token decrypts one credential, for the daemon's own use.
func (s *Store) Token(ctx context.Context, id int64) (string, error) {
	var sealed string
	err := s.db.QueryRowContext(ctx, `SELECT sealed FROM credentials WHERE id = ?`, id).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("credential %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	return s.ring.Open(sealed)
}

// CredentialFingerprint identifies the token behind a credential without
// revealing it, so the reconciler can notice that it was replaced.
func (s *Store) CredentialFingerprint(ctx context.Context, id int64) (string, error) {
	token, err := s.Token(ctx, id)
	if err != nil {
		return "", err
	}
	return s.ring.Fingerprint(token), nil
}

// ReplaceCredentialToken rotates a token in place, keeping the pools pointed at
// it. The generation changes with the fingerprint, so runners are replaced
// gracefully rather than left holding a token that no longer works.
func (s *Store) ReplaceCredentialToken(ctx context.Context, id int64, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("the token is empty")
	}
	sealed, err := s.ring.Seal(token)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE credentials SET sealed = ?, hint = ? WHERE id = ?`, sealed, hintOf(token), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("credential %d: %w", id, ErrNotFound)
	}
	return nil
}

// DeleteCredential refuses while a pool still needs it, since the pools would
// otherwise be left unable to register anything.
func (s *Store) DeleteCredential(ctx context.Context, id int64) error {
	var used int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pools WHERE credential_id = ?`, id).Scan(&used); err != nil {
		return err
	}
	if used > 0 {
		return fmt.Errorf("credential %d is used by %d pool(s): %w", id, used, ErrInUse)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM credentials WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("credential %d: %w", id, ErrNotFound)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Pools
// ---------------------------------------------------------------------------

const poolColumns = `id, name, scope_kind, scope, runtime, nested, ephemeral, replicas, labels,
	cpus, memory_mb, disk_gb, image, credential_id, enabled, created_at, updated_at`

// CreatePool validates and stores a pool.
func (s *Store) CreatePool(ctx context.Context, p model.Pool) (model.Pool, error) {
	p.Defaults()
	if err := p.Validate(); err != nil {
		return model.Pool{}, err
	}
	if err := s.credentialExists(ctx, p.CredentialID); err != nil {
		return model.Pool{}, err
	}

	now := time.Now().UTC()
	p.CreatedAt, p.UpdatedAt = now, now
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO pools (name, scope_kind, scope, runtime, nested, ephemeral, replicas, labels,
			cpus, memory_mb, disk_gb, image, credential_id, enabled, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.Name, string(p.ScopeKind), p.Scope, string(p.Runtime), p.Nested, p.Ephemeral, p.Replicas,
		strings.Join(p.Labels, ","), p.CPUs, p.MemoryMB, p.DiskGB, p.Image, p.CredentialID, p.Enabled,
		p.CreatedAt.Format(time.RFC3339Nano), p.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		if isUnique(err) {
			return model.Pool{}, fmt.Errorf("pool %q: %w", p.Name, ErrConflict)
		}
		return model.Pool{}, err
	}
	p.ID, _ = res.LastInsertId()
	return p, nil
}

// UpdatePool replaces a pool's configuration.
func (s *Store) UpdatePool(ctx context.Context, p model.Pool) (model.Pool, error) {
	p.Defaults()
	if err := p.Validate(); err != nil {
		return model.Pool{}, err
	}
	if err := s.credentialExists(ctx, p.CredentialID); err != nil {
		return model.Pool{}, err
	}

	p.UpdatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE pools SET name=?, scope_kind=?, scope=?, runtime=?, nested=?, ephemeral=?, replicas=?,
			labels=?, cpus=?, memory_mb=?, disk_gb=?, image=?, credential_id=?, enabled=?, updated_at=?
		 WHERE id = ?`,
		p.Name, string(p.ScopeKind), p.Scope, string(p.Runtime), p.Nested, p.Ephemeral, p.Replicas,
		strings.Join(p.Labels, ","), p.CPUs, p.MemoryMB, p.DiskGB, p.Image, p.CredentialID, p.Enabled,
		p.UpdatedAt.Format(time.RFC3339Nano), p.ID)
	if err != nil {
		if isUnique(err) {
			return model.Pool{}, fmt.Errorf("pool %q: %w", p.Name, ErrConflict)
		}
		return model.Pool{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.Pool{}, fmt.Errorf("pool %d: %w", p.ID, ErrNotFound)
	}
	return s.Pool(ctx, p.ID)
}

// DeletePool forgets a pool. Its runners are not touched here: the reconciler
// notices they are no longer wanted and drains them, which is what keeps a
// delete from failing a job that is in flight.
func (s *Store) DeletePool(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM pools WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("pool %d: %w", id, ErrNotFound)
	}
	return nil
}

// Pool returns one pool.
func (s *Store) Pool(ctx context.Context, id int64) (model.Pool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+poolColumns+` FROM pools WHERE id = ?`, id)
	p, err := scanPool(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Pool{}, fmt.Errorf("pool %d: %w", id, ErrNotFound)
	}
	return p, err
}

// ListPools returns every pool, in a stable order so the UI does not shuffle.
func (s *Store) ListPools(ctx context.Context) ([]model.Pool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+poolColumns+` FROM pools ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.Pool{}
	for rows.Next() {
		p, err := scanPool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanPool(row scanner) (model.Pool, error) {
	var (
		p                  model.Pool
		scopeKind, runtime string
		labels             string
		created, updated   string
	)
	err := row.Scan(&p.ID, &p.Name, &scopeKind, &p.Scope, &runtime, &p.Nested, &p.Ephemeral,
		&p.Replicas, &labels, &p.CPUs, &p.MemoryMB, &p.DiskGB, &p.Image, &p.CredentialID,
		&p.Enabled, &created, &updated)
	if err != nil {
		return model.Pool{}, err
	}
	p.ScopeKind = model.ScopeKind(scopeKind)
	p.Runtime = model.Runtime(runtime)
	if labels != "" {
		p.Labels = strings.Split(labels, ",")
	} else {
		p.Labels = []string{}
	}
	p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return p, nil
}

func (s *Store) credentialExists(ctx context.Context, id int64) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM credentials WHERE id = ?`, id).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("credential %d: %w", id, ErrNotFound)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// Setting reads a setting, returning "" when it has never been set.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

// SetSetting writes a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
