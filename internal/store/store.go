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

	"github.com/clems4ever/github-runner/internal/github"
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
	// Autoscaling. A pool used to be a fixed number of runners; it is now a
	// range. Existing pools become a fixed size — minimum equal to maximum —
	// which is exactly what they were, so an upgrade changes nothing until
	// someone raises the maximum.
	`ALTER TABLE pools ADD COLUMN min_replicas INTEGER NOT NULL DEFAULT 1`,
	`ALTER TABLE pools ADD COLUMN max_replicas INTEGER NOT NULL DEFAULT 1`,
	`UPDATE pools SET min_replicas = MAX(replicas, 1), max_replicas = MAX(replicas, 1)`,
	// What the fleet was doing, over time. One row per pool per reconcile
	// pass, pruned to a couple of days: enough to see yesterday's build storm,
	// small enough that nobody has to think about it.
	`CREATE TABLE samples (
		at      TEXT    NOT NULL,
		pool    TEXT    NOT NULL,
		running INTEGER NOT NULL,
		busy    INTEGER NOT NULL,
		target  INTEGER NOT NULL
	)`,
	`CREATE INDEX samples_at ON samples(at)`,
	// GitHub Apps. A credential used to be a token and nothing else; it is now
	// either that or an app, which is an id and a private key. Existing rows
	// become what they already were.
	`ALTER TABLE credentials ADD COLUMN kind TEXT NOT NULL DEFAULT 'pat'`,
	`ALTER TABLE credentials ADD COLUMN app_id INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE credentials ADD COLUMN installation_id INTEGER NOT NULL DEFAULT 0`,
	// What the host itself was using, over time. A table of its own rather than
	// columns on samples: that one has a row per pool per pass, and the host is
	// one thing however many pools are on it.
	`CREATE TABLE host_samples (
		at           TEXT    NOT NULL,
		cpu_percent  REAL    NOT NULL,
		memory_used  INTEGER NOT NULL,
		memory_total INTEGER NOT NULL,
		disk_used    INTEGER NOT NULL,
		disk_total   INTEGER NOT NULL
	)`,
	`CREATE INDEX host_samples_at ON host_samples(at)`,
	// Which repository or organisation each observation was for. Written on
	// the sample rather than looked up from the pool at query time, because a
	// pool can be deleted and the hours it worked still happened: the history
	// keeps the scope it was recorded against. Existing rows are backfilled
	// from the pools that are still there, which is the best that can be said
	// about them.
	`ALTER TABLE samples ADD COLUMN scope TEXT NOT NULL DEFAULT ''`,
	`UPDATE samples SET scope = COALESCE((SELECT scope FROM pools WHERE pools.name = samples.pool), '')`,
	// How much work each pool has actually done, a day at a time.
	//
	// Added to in place rather than appended to: a pass every thirty seconds
	// for a quarter would be a quarter of a million rows per pool, all to
	// answer a question about days. One row per pool per day is small enough
	// to keep for months, which is what the question needs — nobody decides a
	// pool is too small on last night's evidence.
	`CREATE TABLE pool_jobs (
		day     TEXT NOT NULL,
		pool    TEXT NOT NULL,
		jobs    INTEGER NOT NULL,
		seconds REAL NOT NULL,
		PRIMARY KEY (day, pool)
	)`,
	// What a pool bakes into its image, on top of what every runner gets.
	// Empty for every existing pool, which is what they were: the image field
	// alone named a variant and the variant was always built the same way.
	`ALTER TABLE pools ADD COLUMN packages TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pools ADD COLUMN recipe TEXT NOT NULL DEFAULT ''`,
	// Every attempt at building a pool's golden image, and where the account
	// of it was kept.
	//
	// In the database rather than in a file beside the image, which is where
	// this used to live: a file per image held the latest attempt and nothing
	// else, so the failure somebody was reading was replaced by the next
	// attempt at the same thing — and a build that worked erased the one that
	// had explained why the pool was empty all morning. A row per attempt
	// keeps the history that question is answered from.
	`CREATE TABLE image_builds (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		pool       TEXT NOT NULL,
		image      TEXT NOT NULL,
		phase      TEXT NOT NULL,
		trigger    TEXT NOT NULL DEFAULT '',
		error      TEXT NOT NULL DEFAULT '',
		log        TEXT NOT NULL DEFAULT '',
		started_at TEXT NOT NULL,
		ended_at   TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE INDEX image_builds_pool ON image_builds(pool, id)`,
	`CREATE INDEX image_builds_image ON image_builds(image, id)`,
	`ALTER TABLE pools ADD COLUMN layers TEXT NOT NULL DEFAULT 'off'`,
	// What a repository asked for, and what an operator said about it.
	//
	// Keyed by the digest as well as by the repository, so that the answer
	// belongs to a definition rather than to a repository: editing the file
	// asks again, and putting it back finds the old answer still there rather
	// than asking a second time about something already decided.
	`CREATE TABLE repo_layers (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		pool       TEXT NOT NULL,
		repo       TEXT NOT NULL,
		digest     TEXT NOT NULL,
		packages   TEXT NOT NULL DEFAULT '',
		recipe     TEXT NOT NULL DEFAULT '',
		image      TEXT NOT NULL DEFAULT '',
		approval   TEXT NOT NULL,
		decided_by TEXT NOT NULL DEFAULT '',
		first_seen TEXT NOT NULL,
		last_seen  TEXT NOT NULL,
		decided_at TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE UNIQUE INDEX repo_layers_key ON repo_layers(pool, repo, digest)`,
}

// SampleRetention is how much history the daemon keeps. Two days covers "what
// happened overnight" without turning the database into something that needs
// managing.
const SampleRetention = 48 * time.Hour

// JobRetention is how long the per-pool job tally is kept. Far longer than the
// samples above, and for a different reason: those are for looking at what the
// fleet just did, this is the evidence somebody brings to an argument about
// whether a pool needs to be bigger, and a quarter is about the span such an
// argument covers.
const JobRetention = 90 * 24 * time.Hour

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

// CreateCredential seals a secret and stores it. The secret is never written in
// clear, and never returned again — only used.
//
// The secret is a personal access token, or a GitHub App's PEM private key.
// Which one it is decides how it is checked: a key that does not parse is
// refused here, while whoever pasted it is still looking, rather than at the
// first runner boot an hour later.
func (s *Store) CreateCredential(ctx context.Context, credential model.Credential, secret string) (model.Credential, error) {
	credential.Name = strings.TrimSpace(credential.Name)
	// Trimmed, because a pasted token picks up whitespace and a pasted PEM
	// picks up a blank line, and neither is meant to be part of the secret. A
	// PEM parses with or without its final newline.
	secret = strings.TrimSpace(secret)
	if credential.Kind == "" {
		credential.Kind = model.CredentialPAT
	}
	if err := validateCredential(credential, secret); err != nil {
		return model.Credential{}, err
	}

	sealed, err := s.ring.Seal(secret)
	if err != nil {
		return model.Credential{}, err
	}

	credential.Hint = hintOf(credential, secret)
	credential.CreatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO credentials (name, kind, app_id, installation_id, sealed, hint, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		credential.Name, string(credential.Kind), credential.AppID, credential.InstallationID,
		sealed, credential.Hint, credential.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		if isUnique(err) {
			return model.Credential{}, fmt.Errorf("credential %q: %w", credential.Name, ErrConflict)
		}
		return model.Credential{}, err
	}
	credential.ID, _ = res.LastInsertId()
	return credential, nil
}

func validateCredential(credential model.Credential, secret string) error {
	if credential.Name == "" {
		return errors.New("a credential needs a name to tell it from the others")
	}
	if secret == "" {
		return errors.New("the secret is empty")
	}

	switch credential.Kind {
	case model.CredentialPAT:
		return nil
	case model.CredentialApp:
		if credential.AppID <= 0 {
			return errors.New("an app needs its app id, which is on the app's settings page")
		}
		if _, err := github.ParsePrivateKey([]byte(secret)); err != nil {
			return fmt.Errorf("the app's private key: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("credential kind %q: want %q or %q", credential.Kind, model.CredentialPAT, model.CredentialApp)
	}
}

// hintOf is enough to tell two credentials apart in a list, and no more: the
// tail of a token, or which app it is.
func hintOf(credential model.Credential, secret string) string {
	if credential.Kind == model.CredentialApp {
		return fmt.Sprintf("app %d", credential.AppID)
	}
	if len(secret) <= 4 {
		return "****"
	}
	return "…" + secret[len(secret)-4:]
}

// ListCredentials returns every credential, without their secrets.
func (s *Store) ListCredentials(ctx context.Context) ([]model.Credential, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, kind, app_id, installation_id, hint, created_at FROM credentials ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.Credential{}
	for rows.Next() {
		var c model.Credential
		var kind, created string
		if err := rows.Scan(&c.ID, &c.Name, &kind, &c.AppID, &c.InstallationID, &c.Hint, &created); err != nil {
			return nil, err
		}
		c.Kind = model.CredentialKind(kind)
		c.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Secret opens one credential, for the daemon's own use.
func (s *Store) Secret(ctx context.Context, id int64) (model.Secret, error) {
	var (
		sealed, kind string
		secret       model.Secret
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT kind, app_id, installation_id, sealed FROM credentials WHERE id = ?`, id).
		Scan(&kind, &secret.AppID, &secret.InstallationID, &sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Secret{}, fmt.Errorf("credential %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return model.Secret{}, err
	}

	opened, err := s.ring.Open(sealed)
	if err != nil {
		return model.Secret{}, err
	}
	secret.Kind = model.CredentialKind(kind)
	secret.Token = opened
	return secret, nil
}

// CredentialFingerprint identifies the secret behind a credential without
// revealing it, so the reconciler can notice that it was replaced.
//
// The app id and installation are part of it: pointing a credential at a
// different app is as much a change as replacing its key, and either has to
// reach the runners.
func (s *Store) CredentialFingerprint(ctx context.Context, id int64) (string, error) {
	secret, err := s.Secret(ctx, id)
	if err != nil {
		return "", err
	}
	return s.ring.Fingerprint(fmt.Sprintf("%s\x00%d\x00%d\x00%s",
		secret.Kind, secret.AppID, secret.InstallationID, secret.Token)), nil
}

// ReplaceCredentialSecret rotates a secret in place, keeping the pools pointed
// at it. The generation changes with the fingerprint, so runners are replaced
// gracefully rather than left holding a credential that no longer works.
func (s *Store) ReplaceCredentialSecret(ctx context.Context, id int64, secret string) error {
	secret = strings.TrimSpace(secret)

	var (
		kind     string
		existing model.Credential
	)
	err := s.db.QueryRowContext(ctx, `SELECT name, kind, app_id FROM credentials WHERE id = ?`, id).
		Scan(&existing.Name, &kind, &existing.AppID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("credential %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return err
	}
	existing.Kind = model.CredentialKind(kind)

	// The same checks as creating one: a key that does not parse must not be
	// able to get in by the side door.
	if err := validateCredential(existing, secret); err != nil {
		return err
	}

	sealed, err := s.ring.Seal(secret)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE credentials SET sealed = ?, hint = ? WHERE id = ?`,
		sealed, hintOf(existing, secret), id)
	return err
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

const poolColumns = `id, name, scope_kind, scope, runtime, nested, ephemeral, min_replicas, max_replicas,
	labels, cpus, memory_mb, disk_gb, image, packages, recipe, layers, credential_id, enabled, created_at, updated_at`

// execer is the part of the database both a connection and a transaction offer,
// so the statements below can be run either way. Importing several pools has to
// be all-or-nothing, and that means a transaction.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// CreatePool validates and stores a pool.
func (s *Store) CreatePool(ctx context.Context, p model.Pool) (model.Pool, error) {
	p.Defaults()
	if err := p.Validate(); err != nil {
		return model.Pool{}, err
	}
	if err := credentialExists(ctx, s.db, p.CredentialID); err != nil {
		return model.Pool{}, err
	}
	return insertPool(ctx, s.db, p)
}

func insertPool(ctx context.Context, db execer, p model.Pool) (model.Pool, error) {
	now := time.Now().UTC()
	p.CreatedAt, p.UpdatedAt = now, now
	res, err := db.ExecContext(ctx,
		`INSERT INTO pools (name, scope_kind, scope, runtime, nested, ephemeral, min_replicas, max_replicas,
			labels, cpus, memory_mb, disk_gb, image, packages, recipe, layers, credential_id, enabled, created_at, updated_at, replicas)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.Name, string(p.ScopeKind), p.Scope, string(p.Runtime), p.Nested, p.Ephemeral,
		p.MinReplicas, p.MaxReplicas,
		strings.Join(p.Labels, ","), p.CPUs, p.MemoryMB, p.DiskGB, p.Image,
		strings.Join(p.Packages, ","), p.Recipe, string(p.Layers), p.CredentialID, p.Enabled,
		p.CreatedAt.Format(time.RFC3339Nano), p.UpdatedAt.Format(time.RFC3339Nano),
		// The old column is not dropped: SQLite makes that awkward, and a
		// database that can still be read by the previous release is worth more
		// than a tidy schema. It is kept in step rather than left to rot.
		p.MaxReplicas)
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
	if err := credentialExists(ctx, s.db, p.CredentialID); err != nil {
		return model.Pool{}, err
	}
	if err := updatePool(ctx, s.db, p); err != nil {
		return model.Pool{}, err
	}
	return s.Pool(ctx, p.ID)
}

func updatePool(ctx context.Context, db execer, p model.Pool) error {
	p.UpdatedAt = time.Now().UTC()
	res, err := db.ExecContext(ctx,
		`UPDATE pools SET name=?, scope_kind=?, scope=?, runtime=?, nested=?, ephemeral=?,
			min_replicas=?, max_replicas=?, replicas=?,
			labels=?, cpus=?, memory_mb=?, disk_gb=?, image=?, packages=?, recipe=?, layers=?,
			credential_id=?, enabled=?, updated_at=?
		 WHERE id = ?`,
		p.Name, string(p.ScopeKind), p.Scope, string(p.Runtime), p.Nested, p.Ephemeral,
		p.MinReplicas, p.MaxReplicas, p.MaxReplicas,
		strings.Join(p.Labels, ","), p.CPUs, p.MemoryMB, p.DiskGB, p.Image,
		strings.Join(p.Packages, ","), p.Recipe, string(p.Layers), p.CredentialID, p.Enabled,
		p.UpdatedAt.Format(time.RFC3339Nano), p.ID)
	if err != nil {
		if isUnique(err) {
			return fmt.Errorf("pool %q: %w", p.Name, ErrConflict)
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("pool %d: %w", p.ID, ErrNotFound)
	}
	return nil
}

// DeletePool forgets a pool. Its runners are not touched here: the reconciler
// notices they are no longer wanted and drains them, which is what keeps a
// delete from failing a job that is in flight.
func (s *Store) DeletePool(ctx context.Context, id int64) error {
	// Read the name before deleting the row: the layers a repository asked
	// this pool for are keyed by that name, and a pool later given the same
	// name would inherit approvals nobody made for it.
	pool, err := poolByID(ctx, s.db, id)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM pools WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("pool %d: %w", id, ErrNotFound)
	}
	return s.ForgetRepoLayers(ctx, pool.Name)
}

// Pool returns one pool.
func (s *Store) Pool(ctx context.Context, id int64) (model.Pool, error) {
	return poolByID(ctx, s.db, id)
}

func poolByID(ctx context.Context, db execer, id int64) (model.Pool, error) {
	row := db.QueryRowContext(ctx, `SELECT `+poolColumns+` FROM pools WHERE id = ?`, id)
	p, err := scanPool(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Pool{}, fmt.Errorf("pool %d: %w", id, ErrNotFound)
	}
	return p, err
}

func poolByName(ctx context.Context, db execer, name string) (model.Pool, error) {
	row := db.QueryRowContext(ctx, `SELECT `+poolColumns+` FROM pools WHERE name = ?`, name)
	p, err := scanPool(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Pool{}, fmt.Errorf("pool %q: %w", name, ErrNotFound)
	}
	return p, err
}

// ImportAction is what an import did to one pool, or would do.
type ImportAction string

const (
	// ImportCreate means the name was free.
	ImportCreate ImportAction = "create"
	// ImportUpdate means a pool of that name existed and was written over. Its
	// runners are replaced gracefully by the next pass, as each finishes its
	// job, exactly as if it had been edited in the UI.
	ImportUpdate ImportAction = "update"
)

// ImportOutcome is one line of an import's report.
type ImportOutcome struct {
	Name   string       `json:"name"`
	Action ImportAction `json:"action"`
	Pool   model.Pool   `json:"pool"`
}

// ImportPools writes a set of pools in one transaction.
//
// All of them or none. A document is usually a fleet that only makes sense
// whole — two pools that between them cover a repository's jobs — so importing
// the first and failing on the second would leave someone with half a fleet and
// an error message.
//
// A dry run does the entire thing and rolls back. That is what makes the
// preview worth reading: it is not a second implementation guessing at what
// would happen, it is what happened, undone.
func (s *Store) ImportPools(ctx context.Context, pools []model.Pool, replaceExisting, dryRun bool) ([]ImportOutcome, error) {
	if len(pools) == 0 {
		return nil, fmt.Errorf("there is nothing to import")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	// Rolls back unless the commit below happened first, which covers both the
	// dry run and every error path.
	defer func() { _ = tx.Rollback() }()

	outcomes := make([]ImportOutcome, 0, len(pools))
	for _, pool := range pools {
		pool.Defaults()
		if err := pool.Validate(); err != nil {
			return nil, err
		}
		if err := credentialExists(ctx, tx, pool.CredentialID); err != nil {
			return nil, err
		}

		existing, err := poolByName(ctx, tx, pool.Name)
		switch {
		case errors.Is(err, ErrNotFound):
			created, err := insertPool(ctx, tx, pool)
			if err != nil {
				return nil, err
			}
			outcomes = append(outcomes, ImportOutcome{Name: pool.Name, Action: ImportCreate, Pool: created})

		case err != nil:
			return nil, err

		case !replaceExisting:
			return nil, fmt.Errorf("pool %q: %w. Import over the pools that are already here, or rename them in the template", pool.Name, ErrConflict)

		default:
			pool.ID = existing.ID
			if err := updatePool(ctx, tx, pool); err != nil {
				return nil, err
			}
			updated, err := poolByID(ctx, tx, pool.ID)
			if err != nil {
				return nil, err
			}
			outcomes = append(outcomes, ImportOutcome{Name: pool.Name, Action: ImportUpdate, Pool: updated})
		}
	}

	if dryRun {
		return outcomes, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return outcomes, nil
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
		labels, packages   string
		layers             string
		created, updated   string
	)
	err := row.Scan(&p.ID, &p.Name, &scopeKind, &p.Scope, &runtime, &p.Nested, &p.Ephemeral,
		&p.MinReplicas, &p.MaxReplicas, &labels, &p.CPUs, &p.MemoryMB, &p.DiskGB, &p.Image,
		&packages, &p.Recipe, &layers, &p.CredentialID, &p.Enabled, &created, &updated)
	if err != nil {
		return model.Pool{}, err
	}
	p.ScopeKind = model.ScopeKind(scopeKind)
	p.Runtime = model.Runtime(runtime)
	p.Layers = model.LayerPolicy(layers)
	if labels != "" {
		p.Labels = strings.Split(labels, ",")
	} else {
		p.Labels = []string{}
	}
	if packages != "" {
		p.Packages = strings.Split(packages, ",")
	} else {
		p.Packages = []string{}
	}
	p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return p, nil
}

func credentialExists(ctx context.Context, db execer, id int64) error {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM credentials WHERE id = ?`, id).Scan(&n); err != nil {
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

// ---------------------------------------------------------------------------
// Activity
// ---------------------------------------------------------------------------

// RecordSamples stores one observation per pool and prunes what has aged out.
func (s *Store) RecordSamples(ctx context.Context, at time.Time, samples []model.Sample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stamp := at.UTC().Format(time.RFC3339Nano)
	for _, sample := range samples {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO samples (at, pool, scope, running, busy, target) VALUES (?,?,?,?,?,?)`,
			stamp, sample.Pool, sample.Scope, sample.Running, sample.Busy, sample.Target); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM samples WHERE at < ?`,
		at.Add(-SampleRetention).UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

// ActivityFilter narrows a history query. Both fields empty is the whole
// fleet; either one on its own is the usual case, and both together is a
// single pool checked against the scope it was targeting.
type ActivityFilter struct {
	Pool  string
	Scope string
}

// Activity returns a history over a window, in a fixed number of buckets.
//
// Bucketed here rather than in the browser, because a day at one pass every
// thirty seconds is a few thousand rows and a chart wants a couple of hundred
// points. Each bucket reports the *peak* it saw, not the average: a burst that
// filled the fleet for two minutes is the thing worth seeing, and a mean over
// a ten-minute bucket would flatten it into nothing.
// A zero filter is the whole fleet; narrowing it is how the UI shows one
// repository's history without a chart per pool crowding the page.
func (s *Store) Activity(ctx context.Context, since, until time.Time, buckets int, filter ActivityFilter) ([]model.ActivityPoint, error) {
	if buckets < 1 {
		buckets = 1
	}

	query := `SELECT at, SUM(running), SUM(busy) FROM samples WHERE at >= ? AND at <= ?`
	args := []any{since.UTC().Format(time.RFC3339Nano), until.UTC().Format(time.RFC3339Nano)}
	if filter.Pool != "" {
		query += ` AND pool = ?`
		args = append(args, filter.Pool)
	}
	if filter.Scope != "" {
		// Summing across every pool that targets the scope is the point: two
		// pools on one repository are one repository's worth of work.
		query += ` AND scope = ?`
		args = append(args, filter.Scope)
	}
	query += ` GROUP BY at ORDER BY at`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	width := until.Sub(since) / time.Duration(buckets)
	if width <= 0 {
		width = time.Second
	}
	peaks := make([]*model.ActivityPoint, buckets)

	for rows.Next() {
		var stamp string
		var running, busy int
		if err := rows.Scan(&stamp, &running, &busy); err != nil {
			return nil, err
		}
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			continue
		}
		index := int(at.Sub(since) / width)
		if index < 0 || index >= buckets {
			continue
		}
		if peaks[index] == nil {
			peaks[index] = &model.ActivityPoint{At: since.Add(time.Duration(index) * width)}
		}
		if running > peaks[index].Running {
			peaks[index].Running = running
		}
		if busy > peaks[index].Busy {
			peaks[index].Busy = busy
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Empty buckets are left out rather than drawn as zero: a daemon that was
	// not running should read as a gap in the line, not as a fleet that was
	// switched off.
	out := []model.ActivityPoint{}
	for _, point := range peaks {
		if point != nil {
			out = append(out, *point)
		}
	}
	return out, nil
}

// ActivityScopes returns the repositories and organisations that appear in a
// window, in the order a list of them should be read.
//
// Taken from the history rather than from the pools that exist now, so a scope
// stays offered for as long as there is something to show for it — including
// after the pool that was working on it has been deleted. Samples recorded
// before scopes were kept have none, and are left out rather than shown as a
// scope with no name.
func (s *Store) ActivityScopes(ctx context.Context, since, until time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT scope FROM samples WHERE at >= ? AND at <= ? AND scope != '' ORDER BY scope`,
		since.UTC().Format(time.RFC3339Nano), until.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	scopes := []string{}
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			return nil, err
		}
		scopes = append(scopes, scope)
	}
	return scopes, rows.Err()
}

// ---------------------------------------------------------------------------
// Job accounting
// ---------------------------------------------------------------------------

// dayFormat is the key the job tally is kept under: a UTC date, which sorts
// and compares as a string, so a window is a plain pair of comparisons and no
// date arithmetic has to happen in SQLite.
const dayFormat = "2006-01-02"

// RecordJobs adds one pass's observations to the running per-pool tally and
// prunes what has aged out.
func (s *Store) RecordJobs(ctx context.Context, at time.Time, samples []model.JobSample) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	day := at.UTC().Format(dayFormat)
	for _, sample := range samples {
		// A pool that did nothing this pass adds nothing. That keeps an idle
		// fleet from touching every pool's row twice a minute, and keeps a day
		// out of the history entirely unless something happened on it — which
		// is what lets a reader tell a pool that was quiet from a pool that
		// was not there.
		if sample.Started == 0 && sample.BusySeconds == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO pool_jobs (day, pool, jobs, seconds) VALUES (?,?,?,?)
			 ON CONFLICT(day, pool) DO UPDATE SET
				jobs = jobs + excluded.jobs, seconds = seconds + excluded.seconds`,
			day, sample.Pool, sample.Started, sample.BusySeconds); err != nil {
			return err
		}
	}
	// Pruned whether or not anything was written, so a fleet that goes quiet
	// for a quarter still stops carrying the quarter before it.
	if _, err := tx.ExecContext(ctx, `DELETE FROM pool_jobs WHERE day < ?`,
		at.Add(-JobRetention).UTC().Format(dayFormat)); err != nil {
		return err
	}
	return tx.Commit()
}

// JobHistory returns the tally over a window, one row per pool per day it did
// something.
//
// Days a pool was idle are absent rather than zero, for the same reason an
// empty bucket is left out of the activity chart: a pool nobody used and a
// daemon nobody was running should not read the same, and the caller knows
// which pools exist.
func (s *Store) JobHistory(ctx context.Context, since, until time.Time) ([]model.JobDay, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT day, pool, jobs, seconds FROM pool_jobs
		 WHERE day >= ? AND day <= ? ORDER BY day, pool`,
		since.UTC().Format(dayFormat), until.UTC().Format(dayFormat))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.JobDay{}
	for rows.Next() {
		var day model.JobDay
		if err := rows.Scan(&day.Day, &day.Pool, &day.Jobs, &day.Seconds); err != nil {
			return nil, err
		}
		out = append(out, day)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Host resources
// ---------------------------------------------------------------------------

// RecordHostSample stores one observation of the host and prunes what has aged
// out, on the same two-day retention as the fleet's own history.
func (s *Store) RecordHostSample(ctx context.Context, at time.Time, sample model.HostSample) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO host_samples (at, cpu_percent, memory_used, memory_total, disk_used, disk_total)
		 VALUES (?,?,?,?,?,?)`,
		at.UTC().Format(time.RFC3339Nano), sample.CPUPercent,
		sample.MemoryUsedBytes, sample.MemoryTotalBytes,
		sample.DiskUsedBytes, sample.DiskTotalBytes); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM host_samples WHERE at < ?`,
		at.Add(-SampleRetention).UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

// HostHistory returns what the host has been using over a window, bucketed the
// same way the activity chart is.
//
// Each bucket reports the peak, for the same reason: a build storm that pinned
// every core for ninety seconds is the thing worth seeing, and a mean over a
// ten-minute bucket would leave no trace of it at all.
func (s *Store) HostHistory(ctx context.Context, since, until time.Time, buckets int) ([]model.HostPoint, error) {
	if buckets < 1 {
		buckets = 1
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT at, cpu_percent, memory_used, memory_total, disk_used, disk_total
		 FROM host_samples WHERE at >= ? AND at <= ? ORDER BY at`,
		since.UTC().Format(time.RFC3339Nano), until.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	width := until.Sub(since) / time.Duration(buckets)
	if width <= 0 {
		width = time.Second
	}
	peaks := make([]*model.HostPoint, buckets)

	for rows.Next() {
		var stamp string
		var sample model.HostSample
		if err := rows.Scan(&stamp, &sample.CPUPercent, &sample.MemoryUsedBytes,
			&sample.MemoryTotalBytes, &sample.DiskUsedBytes, &sample.DiskTotalBytes); err != nil {
			return nil, err
		}
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			continue
		}
		index := int(at.Sub(since) / width)
		if index < 0 || index >= buckets {
			continue
		}
		if peaks[index] == nil {
			peaks[index] = &model.HostPoint{At: since.Add(time.Duration(index) * width)}
		}
		point := peaks[index]
		point.CPUPercent = max(point.CPUPercent, sample.CPUPercent)
		point.MemoryPercent = max(point.MemoryPercent, share(sample.MemoryUsedBytes, sample.MemoryTotalBytes))
		point.DiskPercent = max(point.DiskPercent, share(sample.DiskUsedBytes, sample.DiskTotalBytes))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Empty buckets are left out rather than drawn as zero, so a daemon that
	// was not running reads as a gap in the line rather than as an idle host.
	out := []model.HostPoint{}
	for _, point := range peaks {
		if point != nil {
			out = append(out, *point)
		}
	}
	return out, nil
}

// share turns a used-of-total pair into a percentage, rounded to the two
// decimals a chart can actually draw.
func share(used, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(int64(float64(used)/float64(total)*10000+0.5)) / 100
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
