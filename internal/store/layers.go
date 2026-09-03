package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
)

// A repository layer is a fact the daemon observed, not a thing an operator
// configured: some repository's default branch asked for a set of packages and
// a recipe, and this row is the fleet's memory of having seen that ask.
//
// The row is keyed by the digest of what would execute, so an edit to the
// repository's file is a *different* row rather than a mutation of this one.
// That is the whole point of the approval workflow: an operator approves a
// specific set of packages and a specific script, and a repository that changes
// either one is back in front of them rather than silently running the change
// with yesterday's decision attached.

// SeeRepoLayer records that a repository is asking for a layer, and returns
// what the fleet knows about that ask.
//
// Called on every reconciliation pass for every repository-scoped pool whose
// policy is not off, so it has to be cheap and it has to be idempotent. First
// sighting inserts; every sighting after that only moves last_seen. It never
// changes an approval — deciding is a separate call with a person behind it.
//
// The approval a first sighting gets depends on the pool's policy, which the
// caller passes because the policy lives on the pool and this table does not
// know about pools. A pool on "trust" gets an approved row immediately, which
// is what makes that policy mean anything.
func (s *Store) SeeRepoLayer(ctx context.Context, layer model.RepoLayer, trusted bool) (model.RepoLayer, error) {
	now := time.Now().UTC()
	approval := model.LayerPending
	decidedAt := ""
	decidedBy := ""
	if trusted {
		approval = model.LayerApproved
		decidedAt = now.Format(time.RFC3339Nano)
		decidedBy = "policy"
	}

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO repo_layers
		   (pool, repo, digest, packages, recipe, approval, decided_by, first_seen, last_seen, decided_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(pool, repo, digest) DO UPDATE SET last_seen = excluded.last_seen`,
		layer.Pool, layer.Repo, layer.Digest,
		strings.Join(layer.Packages, "\n"), layer.Recipe,
		string(approval), decidedBy,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), decidedAt,
	); err != nil {
		return model.RepoLayer{}, err
	}
	return s.RepoLayer(ctx, layer.Pool, layer.Repo, layer.Digest)
}

// RepoLayer reads one layer by the key it is stored under.
func (s *Store) RepoLayer(ctx context.Context, pool, repo, digest string) (model.RepoLayer, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+layerColumns+` FROM repo_layers WHERE pool = ? AND repo = ? AND digest = ?`,
		pool, repo, digest)
	layer, err := scanLayer(row)
	if err == sql.ErrNoRows {
		return model.RepoLayer{}, ErrNotFound
	}
	return layer, err
}

// RepoLayerByID reads one layer by its row id, which is what the API and the
// UI hold: an operator approving something clicked a row, and the digest is a
// hash they should never have to type.
func (s *Store) RepoLayerByID(ctx context.Context, id int64) (model.RepoLayer, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+layerColumns+` FROM repo_layers WHERE id = ?`, id)
	layer, err := scanLayer(row)
	if err == sql.ErrNoRows {
		return model.RepoLayer{}, ErrNotFound
	}
	return layer, err
}

// ListRepoLayers returns every layer the fleet has seen, most recently seen
// first — which puts what is asking for a decision right now at the top,
// rather than whatever was inserted first.
//
// A pool name narrows it; empty is the whole fleet.
func (s *Store) ListRepoLayers(ctx context.Context, pool string) ([]model.RepoLayer, error) {
	query := `SELECT ` + layerColumns + ` FROM repo_layers`
	var args []any
	if pool != "" {
		query += ` WHERE pool = ?`
		args = append(args, pool)
	}
	query += ` ORDER BY last_seen DESC, id DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.RepoLayer{}
	for rows.Next() {
		layer, err := scanLayer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, layer)
	}
	return out, rows.Err()
}

// DecideRepoLayer approves or refuses a layer.
//
// by is who decided, kept because "why is this repository allowed to install a
// compiler on my host" is a question asked months later.
func (s *Store) DecideRepoLayer(ctx context.Context, id int64, approval model.LayerApproval, by string) (model.RepoLayer, error) {
	if approval != model.LayerApproved && approval != model.LayerRefused {
		return model.RepoLayer{}, fmt.Errorf("a layer is approved or refused, not %q", approval)
	}
	result, err := s.db.ExecContext(ctx,
		`UPDATE repo_layers SET approval = ?, decided_by = ?, decided_at = ? WHERE id = ?`,
		string(approval), by, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return model.RepoLayer{}, err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return model.RepoLayer{}, ErrNotFound
	}
	return s.RepoLayerByID(ctx, id)
}

// SetRepoLayerImage records which image was built for a layer.
//
// Separate from the approval because they happen at different times and for
// different reasons: a person approves, and minutes later a build finishes. It
// is also what makes the build idempotent — a layer with an image already
// named is not built again.
func (s *Store) SetRepoLayerImage(ctx context.Context, id int64, image string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE repo_layers SET image = ? WHERE id = ?`, image, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ForgetRepoLayers deletes the layers of a pool that no longer exists.
//
// Called when a pool is deleted. The rows are keyed by pool name and would
// otherwise come back to life, already approved, under a new pool that
// happened to be given the same name — an approval nobody made.
func (s *Store) ForgetRepoLayers(ctx context.Context, pool string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM repo_layers WHERE pool = ?`, pool)
	return err
}

// WantedLayerImages is every image name the approved layers point at.
//
// The image collector deletes what no pool asks for, and a layer's image is
// asked for by a row in this table rather than by a pool's specification. It
// would otherwise be collected the moment it was built.
func (s *Store) WantedLayerImages(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT image FROM repo_layers WHERE image <> '' AND approval = ?`, string(model.LayerApproved))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var image string
		if err := rows.Scan(&image); err != nil {
			return nil, err
		}
		out[image] = true
	}
	return out, rows.Err()
}

const layerColumns = `id, pool, repo, digest, packages, recipe, image, approval, decided_by, first_seen, last_seen, decided_at`

func scanLayer(row scanner) (model.RepoLayer, error) {
	var (
		l                              model.RepoLayer
		packages, approval             string
		firstSeen, lastSeen, decidedAt string
	)
	if err := row.Scan(&l.ID, &l.Pool, &l.Repo, &l.Digest, &packages, &l.Recipe, &l.Image,
		&approval, &l.DecidedBy, &firstSeen, &lastSeen, &decidedAt); err != nil {
		return model.RepoLayer{}, err
	}
	if packages != "" {
		l.Packages = strings.Split(packages, "\n")
	}
	l.Approval = model.LayerApproval(approval)
	l.FirstSeen, _ = time.Parse(time.RFC3339Nano, firstSeen)
	l.LastSeen, _ = time.Parse(time.RFC3339Nano, lastSeen)
	l.DecidedAt, _ = time.Parse(time.RFC3339Nano, decidedAt)
	return l, nil
}
