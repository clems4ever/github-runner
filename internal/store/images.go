package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/clems4ever/github-runner/internal/model"
)

// ImageBuildsKept is how many attempts at one pool's image are remembered.
//
// Enough that the failure a recipe was fixed after is still there next to the
// build that fixed it, and few enough that a pool nobody has looked at in a
// month is not carrying a hundred logs. The logs go with the rows they belong
// to, so this is a bound on the disk as well as on the table.
const ImageBuildsKept = 20

// StartImageBuild records a build about to happen and returns it with the id
// the log will be named after.
//
// Written when the build starts, not when it ends. A record that appeared at
// the end would say nothing for the whole time somebody is waiting, which is
// the only time anybody looks.
func (s *Store) StartImageBuild(ctx context.Context, build model.ImageBuild) (model.ImageBuild, error) {
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO image_builds (pool, image, phase, trigger, error, log, started_at, ended_at)
		 VALUES (?, ?, ?, ?, '', '', ?, '')`,
		build.Pool, build.Image, string(build.Phase), build.Trigger,
		build.StartedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return model.ImageBuild{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return model.ImageBuild{}, err
	}
	build.ID = id
	return build, nil
}

// UpdateImageBuild stores what has happened to a build since it started: which
// phase it reached, where its log is, and how it ended.
func (s *Store) UpdateImageBuild(ctx context.Context, build model.ImageBuild) error {
	ended := ""
	if build.EndedAt != nil {
		ended = build.EndedAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE image_builds SET phase = ?, error = ?, log = ?, ended_at = ? WHERE id = ?`,
		string(build.Phase), build.Error, build.Log, ended, build.ID)
	return err
}

// ImageBuild reads one build.
func (s *Store) ImageBuild(ctx context.Context, id int64) (model.ImageBuild, error) {
	row := s.db.QueryRowContext(ctx, imageBuildColumns+` WHERE id = ?`, id)
	build, err := scanImageBuild(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ImageBuild{}, fmt.Errorf("image build %d: %w", id, ErrNotFound)
	}
	return build, err
}

// ImageBuilds is what has been tried for one pool's image, newest first.
func (s *Store) ImageBuilds(ctx context.Context, pool string, limit int) ([]model.ImageBuild, error) {
	if limit <= 0 {
		limit = ImageBuildsKept
	}
	rows, err := s.db.QueryContext(ctx,
		imageBuildColumns+` WHERE pool = ? ORDER BY id DESC LIMIT ?`, pool, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.ImageBuild
	for rows.Next() {
		build, err := scanImageBuild(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, build)
	}
	return out, rows.Err()
}

// LatestImageBuilds is the most recent attempt at each image, which is what
// decides whether the daemon may start another one.
//
// By image and not by pool, because the image's name is a hash of everything
// it is built from: two pools asking for the same thing share one image, and a
// pool that changed its recipe is asking for a different one and has no
// history against it yet.
func (s *Store) LatestImageBuilds(ctx context.Context) (map[string]model.ImageBuild, error) {
	rows, err := s.db.QueryContext(ctx,
		imageBuildColumns+` WHERE id IN (SELECT MAX(id) FROM image_builds GROUP BY image)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]model.ImageBuild{}
	for rows.Next() {
		build, err := scanImageBuild(rows)
		if err != nil {
			return nil, err
		}
		out[build.Image] = build
	}
	return out, rows.Err()
}

// AbandonImageBuilds marks every unfinished build as failed, and returns them.
//
// Called when the daemon starts, because a build is a process this daemon was
// running: one that was going when it stopped is not going now, however the
// row was left. Without this a pool would sit for ever behind a build that
// ended when somebody upgraded the daemon, with the page saying it was still
// happening.
func (s *Store) AbandonImageBuilds(ctx context.Context, at time.Time, reason string) ([]model.ImageBuild, error) {
	rows, err := s.db.QueryContext(ctx, imageBuildColumns+` WHERE ended_at = ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stale []model.ImageBuild
	for rows.Next() {
		build, err := scanImageBuild(rows)
		if err != nil {
			return nil, err
		}
		stale = append(stale, build)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	ended := at.UTC()
	for i := range stale {
		stale[i].Phase = model.ImageFailed
		stale[i].Error = reason
		stale[i].EndedAt = &ended
		if err := s.UpdateImageBuild(ctx, stale[i]); err != nil {
			return nil, err
		}
	}
	return stale, nil
}

// PruneImageBuilds forgets the oldest builds of each pool and returns the logs
// that are no longer referred to, so whoever owns them can delete them.
//
// The rows are deleted here and the files are not, because a store that
// reached into the filesystem would be two things at once — and because
// deleting a log is the one part of this that cannot be undone by a
// transaction.
func (s *Store) PruneImageBuilds(ctx context.Context, keepPerPool int) ([]string, error) {
	if keepPerPool <= 0 {
		keepPerPool = ImageBuildsKept
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT log FROM image_builds WHERE id NOT IN (
			SELECT id FROM image_builds AS keep
			WHERE (SELECT COUNT(*) FROM image_builds AS newer
			       WHERE newer.pool = keep.pool AND newer.id > keep.id) < ?
		 )`, keepPerPool)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []string
	for rows.Next() {
		var log string
		if err := rows.Scan(&log); err != nil {
			return nil, err
		}
		if log != "" {
			logs = append(logs, log)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	_, err = s.db.ExecContext(ctx,
		`DELETE FROM image_builds WHERE id NOT IN (
			SELECT id FROM image_builds AS keep
			WHERE (SELECT COUNT(*) FROM image_builds AS newer
			       WHERE newer.pool = keep.pool AND newer.id > keep.id) < ?
		 )`, keepPerPool)
	return logs, err
}

const imageBuildColumns = `SELECT id, pool, image, phase, trigger, error, log, started_at, ended_at
	FROM image_builds`

func scanImageBuild(row scanner) (model.ImageBuild, error) {
	var (
		b              model.ImageBuild
		phase          string
		started, ended string
	)
	if err := row.Scan(&b.ID, &b.Pool, &b.Image, &phase, &b.Trigger, &b.Error, &b.Log,
		&started, &ended); err != nil {
		return model.ImageBuild{}, err
	}
	b.Phase = model.ImagePhase(phase)
	b.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
	if ended != "" {
		at, err := time.Parse(time.RFC3339Nano, ended)
		if err == nil {
			b.EndedAt = &at
		}
	}
	return b, nil
}

// ForgetImageBuilds drops everything remembered about one pool's image and
// returns the logs nobody refers to any more.
//
// Called when a pool is deleted. The history is filed under the pool it was
// for, so once the pool is gone there is nowhere to read it from — and a
// console is megabytes, which is not a thing to leave on a host for ever with
// no way to see it.
func (s *Store) ForgetImageBuilds(ctx context.Context, pool string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT log FROM image_builds WHERE pool = ?`, pool)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []string
	for rows.Next() {
		var log string
		if err := rows.Scan(&log); err != nil {
			return nil, err
		}
		if log != "" {
			logs = append(logs, log)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM image_builds WHERE pool = ?`, pool)
	return logs, err
}
