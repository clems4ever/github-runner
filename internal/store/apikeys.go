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
)

// apiKeyColumns is the whole row except the hash, which never leaves this file:
// it is what a presented key is looked up by, and nothing above the store has
// any use for it.
const apiKeyColumns = `SELECT id, name, scope, hint, created_at, expires_at, last_used_at FROM api_keys`

// CreateAPIKey mints a key and stores its hash.
//
// The key comes back as a second return value rather than on the struct, and
// that is deliberate: a model.APIKey has nowhere to put it, so it cannot be
// logged by something that prints the struct, returned by a handler that
// marshals one, or written to the database by a later change to this function.
// The one copy that exists is the string handed to the caller, and when they
// drop it the key is gone for good.
func (s *Store) CreateAPIKey(ctx context.Context, key model.APIKey) (model.APIKey, string, error) {
	key.Name = strings.TrimSpace(key.Name)
	if key.Scope == "" {
		key.Scope = model.APIKeyRead
	}
	if err := validateAPIKey(key); err != nil {
		return model.APIKey{}, "", err
	}

	plaintext, hash, hint, err := secrets.NewAPIKey()
	if err != nil {
		return model.APIKey{}, "", err
	}

	key.Hint = hint
	key.CreatedAt = time.Now().UTC()
	expires := ""
	if key.ExpiresAt != nil {
		utc := key.ExpiresAt.UTC()
		key.ExpiresAt = &utc
		expires = utc.Format(time.RFC3339Nano)
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO api_keys (name, hash, hint, scope, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		key.Name, hash, key.Hint, string(key.Scope),
		key.CreatedAt.Format(time.RFC3339Nano), expires)
	if err != nil {
		if isUnique(err) {
			return model.APIKey{}, "", fmt.Errorf("api key %q: %w", key.Name, ErrConflict)
		}
		return model.APIKey{}, "", err
	}
	key.ID, _ = res.LastInsertId()
	return key, plaintext, nil
}

// validateAPIKey refuses what would be confusing later rather than at the point
// a request arrives with it.
func validateAPIKey(key model.APIKey) error {
	if key.Name == "" {
		return errors.New("an api key needs a name to tell it from the others")
	}
	if !key.Scope.Valid() {
		return fmt.Errorf("api key scope %q: want %q or %q", key.Scope, model.APIKeyRead, model.APIKeyAdmin)
	}
	// A key that has already expired would be accepted here and refused on
	// first use, which reads as the daemon being broken rather than as the date
	// being wrong.
	if key.ExpiresAt != nil && !key.ExpiresAt.After(time.Now()) {
		return errors.New("an api key cannot expire in the past")
	}
	return nil
}

// ListAPIKeys returns every key, without anything that could be used as one.
func (s *Store) ListAPIKeys(ctx context.Context) ([]model.APIKey, error) {
	rows, err := s.db.QueryContext(ctx, apiKeyColumns+` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.APIKey{}
	for rows.Next() {
		key, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

// APIKeyByHash finds the key a request presented.
//
// By hash and not by id, so there is nothing to parse out of what arrived and
// no branch taken on a partial match: the hash either names a row or it does
// not. One indexed lookup, whatever the key turns out to be.
func (s *Store) APIKeyByHash(ctx context.Context, hash string) (model.APIKey, error) {
	key, err := scanAPIKey(s.db.QueryRowContext(ctx, apiKeyColumns+` WHERE hash = ?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		// Without the hash in the message: it is a valid key if the guess was
		// right, and the logs are not where it should end up.
		return model.APIKey{}, fmt.Errorf("api key: %w", ErrNotFound)
	}
	if err != nil {
		return model.APIKey{}, err
	}
	return key, nil
}

// TouchAPIKey records that a key was used.
//
// The caller decides how often this is worth doing — see the throttle in the
// api package. A write per request would be a write per request, and this is
// bookkeeping, not state the fleet depends on.
func (s *Store) TouchAPIKey(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ?`,
		at.UTC().Format(time.RFC3339Nano), id)
	return err
}

// DeleteAPIKey revokes a key. There is no disabled state to move it into: a key
// that is still in the table is one somebody has to reason about, and the whole
// value of hashing is that deleting the row ends the matter.
func (s *Store) DeleteAPIKey(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("api key %d: %w", id, ErrNotFound)
	}
	return nil
}

func scanAPIKey(row scanner) (model.APIKey, error) {
	var (
		key                        model.APIKey
		scope                      string
		created, expires, lastUsed string
	)
	if err := row.Scan(&key.ID, &key.Name, &scope, &key.Hint, &created, &expires, &lastUsed); err != nil {
		return model.APIKey{}, err
	}
	key.Scope = model.APIKeyScope(scope)
	key.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	key.ExpiresAt = parseOptionalTime(expires)
	key.LastUsedAt = parseOptionalTime(lastUsed)
	return key, nil
}

// parseOptionalTime turns the empty string the schema uses for "has not
// happened" into a nil, and anything unparseable into the same thing: a corrupt
// timestamp on a key is not a reason to refuse the request it arrived with.
func parseOptionalTime(value string) *time.Time {
	if value == "" {
		return nil
	}
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	return &at
}
