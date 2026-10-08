package lyricblock

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sydlexius/canticle/internal/normalize"
)

// ErrNoFingerprint is returned by Add for an empty fingerprint: a result with
// no words (an instrumental marker) can never be blocked.
var ErrNoFingerprint = errors.New("lyricblock: empty fingerprint cannot be blocked")

// Execer is the subset of *sql.DB / *sql.Tx the mutating methods need, so a
// caller already inside a transaction (mark, identity repair) can compose them
// into it.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Block is one blocked result: the fingerprint of a lyric body that must not
// be accepted for the (ArtistKey, TitleKey) identity. WorkQueueID, Lane and
// Upstream are informational only and never enforced.
type Block struct {
	ID          int64
	ArtistKey   string
	TitleKey    string
	Fingerprint string
	WorkQueueID int64 // 0 when unknown
	Lane        string
	Upstream    string
	CreatedAt   string
}

// ListFilter narrows List. Zero fields do not filter.
type ListFilter struct {
	WorkItemID int64  // blocks recorded from this work_queue row
	ArtistKey  string // with TitleKey: one identity
	TitleKey   string
	Orphans    bool // only identities with no work_queue row
}

// Store is the lyric_blocks repository.
type Store struct {
	db  *sql.DB
	log *slog.Logger
}

// NewStore returns a Store over db. A nil logger uses slog.Default().
func NewStore(db *sql.DB, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{db: db, log: log}
}

// Add records b, normalizing its identity keys exactly as the cache and
// work_queue do. It reports whether a new row was written; an identical
// (identity, fingerprint) is a no-op, not an error.
func (s *Store) Add(ctx context.Context, ex Execer, b Block) (bool, error) {
	if b.Fingerprint == "" {
		return false, ErrNoFingerprint
	}
	var wq any
	if b.WorkQueueID != 0 {
		wq = b.WorkQueueID
	}
	res, err := ex.ExecContext(ctx,
		`INSERT INTO lyric_blocks (artist_key, title_key, fingerprint, work_queue_id, lane, upstream)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(artist_key, title_key, fingerprint) DO NOTHING`,
		normalize.NormalizeKey(b.ArtistKey), normalize.NormalizeKey(b.TitleKey), b.Fingerprint, wq, b.Lane, b.Upstream)
	if err != nil {
		return false, fmt.Errorf("lyricblock: add: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("lyricblock: add rows affected: %w", err)
	}
	return n > 0, nil
}

// Remove deletes the block with the given id and reports whether it existed.
func (s *Store) Remove(ctx context.Context, ex Execer, id int64) (bool, error) {
	res, err := ex.ExecContext(ctx, `DELETE FROM lyric_blocks WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("lyricblock: remove: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("lyricblock: remove rows affected: %w", err)
	}
	return n > 0, nil
}

// DeleteByIdentityTx deletes every block whose keys equal (artistKey, titleKey)
// exactly and returns how many it removed. Unlike List, an empty key is a value
// to match, not a wildcard, so it can never reach another identity.
func (s *Store) DeleteByIdentityTx(ctx context.Context, tx *sql.Tx, artistKey, titleKey string) (int, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM lyric_blocks WHERE artist_key = ? AND title_key = ?`,
		normalize.NormalizeKey(artistKey), normalize.NormalizeKey(titleKey))
	if err != nil {
		return 0, fmt.Errorf("lyricblock: delete by identity: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("lyricblock: delete by identity rows affected: %w", err)
	}
	return int(n), nil
}

// List returns blocks matching f, oldest first.
func (s *Store) List(ctx context.Context, f ListFilter) ([]Block, error) {
	q := `SELECT b.id, b.artist_key, b.title_key, b.fingerprint, COALESCE(b.work_queue_id, 0), b.lane, b.upstream, b.created_at
	      FROM lyric_blocks b WHERE 1=1`
	var args []any
	if f.WorkItemID != 0 {
		q += ` AND b.work_queue_id = ?`
		args = append(args, f.WorkItemID)
	}
	if f.ArtistKey != "" {
		q += ` AND b.artist_key = ?`
		args = append(args, normalize.NormalizeKey(f.ArtistKey))
	}
	if f.TitleKey != "" {
		q += ` AND b.title_key = ?`
		args = append(args, normalize.NormalizeKey(f.TitleKey))
	}
	if f.Orphans {
		q += ` AND NOT EXISTS (SELECT 1 FROM work_queue w WHERE w.artist_key = b.artist_key AND w.title_key = b.title_key)`
	}
	q += ` ORDER BY b.id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("lyricblock: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Block
	for rows.Next() {
		var b Block
		if err := rows.Scan(&b.ID, &b.ArtistKey, &b.TitleKey, &b.Fingerprint, &b.WorkQueueID, &b.Lane, &b.Upstream, &b.CreatedAt); err != nil {
			return nil, fmt.Errorf("lyricblock: list scan: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lyricblock: list rows: %w", err)
	}
	return out, nil
}

// Blocked reports whether one fingerprint is blocked for the identity; see
// AnyBlocked.
func (s *Store) Blocked(ctx context.Context, artistKey, titleKey, fingerprint string) bool {
	return s.AnyBlocked(ctx, artistKey, titleKey, []string{fingerprint})
}

// AnyBlocked reports whether ANY of fingerprints (empties ignored) is blocked
// for the identity, in one query. Pass SongFingerprints of a fetched result. It
// FAILS OPEN: a read error reads as not blocked, so a broken store can never
// stop good lyrics from being written. A real read failure is logged at Error;
// a canceled or expired context (shutdown) is logged at Warn only. It reads
// committed state through the store's own handle, so a block inserted in a
// caller's still-uncommitted transaction is not visible to it.
func (s *Store) AnyBlocked(ctx context.Context, artistKey, titleKey string, fingerprints []string) bool {
	args := []any{normalize.NormalizeKey(artistKey), normalize.NormalizeKey(titleKey)}
	for _, fp := range fingerprints {
		if fp != "" {
			args = append(args, fp)
		}
	}
	if len(args) == 2 {
		return false
	}
	q := `SELECT 1 FROM lyric_blocks WHERE artist_key = ? AND title_key = ? AND fingerprint IN (?` + strings.Repeat(",?", len(args)-3) + `) LIMIT 1`
	var one int
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		if ctx.Err() != nil {
			s.log.WarnContext(ctx, "lyricblock: block lookup canceled, failing open", "error", err)
		} else {
			s.log.ErrorContext(ctx, "lyricblock: block lookup failed, failing open", "error", err)
		}
		return false
	}
	return true
}

// Rekey moves every block of the old identity to the new one, for identity
// repair re-keying a work_queue row. Where the destination already holds the
// same fingerprint the old row is the duplicate and is dropped. It returns the
// number of blocks that now sit under the new identity from the old one.
func (s *Store) Rekey(ctx context.Context, ex Execer, oldArtistKey, oldTitleKey, newArtistKey, newTitleKey string) (int, error) {
	oldA, oldT := normalize.NormalizeKey(oldArtistKey), normalize.NormalizeKey(oldTitleKey)
	newA, newT := normalize.NormalizeKey(newArtistKey), normalize.NormalizeKey(newTitleKey)
	if oldA == newA && oldT == newT {
		return 0, nil
	}
	res, err := ex.ExecContext(ctx,
		`UPDATE OR IGNORE lyric_blocks SET artist_key = ?, title_key = ? WHERE artist_key = ? AND title_key = ?`,
		newA, newT, oldA, oldT)
	if err != nil {
		return 0, fmt.Errorf("lyricblock: rekey: %w", err)
	}
	moved, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("lyricblock: rekey rows affected: %w", err)
	}
	// Rows the UPDATE skipped collided with the destination's own block.
	if _, err := ex.ExecContext(ctx,
		`DELETE FROM lyric_blocks WHERE artist_key = ? AND title_key = ?`, oldA, oldT); err != nil {
		return 0, fmt.Errorf("lyricblock: rekey drop duplicates: %w", err)
	}
	return int(moved), nil
}

// CountFor returns, for each work_queue id with at least one block on its
// identity, the number of blocks. Ids with none are absent from the map.
func (s *Store) CountFor(ctx context.Context, ids []int64) (map[int64]int, error) {
	out := make(map[int64]int)
	const chunk = 500
	for start := 0; start < len(ids); start += chunk {
		part := ids[start:min(start+chunk, len(ids))]
		args := make([]any, len(part))
		for i, id := range part {
			args[i] = id
		}
		q := `SELECT w.id, (SELECT COUNT(*) FROM lyric_blocks b WHERE b.artist_key = w.artist_key AND b.title_key = w.title_key) FROM work_queue w WHERE w.id IN (?` + strings.Repeat(",?", len(part)-1) + `)` //nolint:gosec // reason: only "?" placeholders are concatenated; every value is bound
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("lyricblock: count: %w", err)
		}
		for rows.Next() {
			var id int64
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("lyricblock: count scan: %w", err)
			}
			if n > 0 {
				out[id] = n
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, fmt.Errorf("lyricblock: count rows: %w", err)
		}
	}
	return out, nil
}
