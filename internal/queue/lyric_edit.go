package queue

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// notHandProtected keeps automatic replacement paths away from a row a person
// has acted on: a file adjusted by ear (lyric_edited_at, #481 Stage 2) or a row
// an operator marked instrumental by hand (manual_instrumental_at, #1218).
// Shared by the upgrade sweep, word recheck, categorical reopen and
// gone-source move predicates. Leading AND.
const notHandProtected = ` AND lyric_edited_at IS NULL AND manual_instrumental_at IS NULL`

// NotHandProtected is notHandProtected for packages that write work_queue
// themselves (prune), so they share this one fragment instead of a literal.
const NotHandProtected = notHandProtected

// HandProtectedPredicate is the positive form: true for a hand-edited or
// manually marked row. No leading AND.
const HandProtectedPredicate = `(lyric_edited_at IS NOT NULL OR manual_instrumental_at IS NOT NULL)`

// SetLyricEdit records a hand edit of the row's .lrc: the net offset from the
// original and the edit time. Non-fatal for the caller only in the sense that
// the file is already written; the next save re-derives from .lrc.orig.
func (q *DBQueue) SetLyricEdit(ctx context.Context, id int64, offsetMS int) error {
	if _, err := q.db.ExecContext(ctx,
		`UPDATE work_queue SET lyric_offset_ms = ?, lyric_edited_at = ? WHERE id = ?`,
		offsetMS, formatTime(q.now()), id); err != nil {
		return fmt.Errorf("queue: set lyric edit %d: %w", id, err)
	}
	return nil
}

// SetLyricRetime records an accepted generated retiming of the row's .lrc
// (#1008): the edit time, with lyric_offset_ms left NULL because the change is
// per line, not one offset. The mark keeps the automatic sweeps away exactly
// as a hand edit's does (notHandProtected).
func (q *DBQueue) SetLyricRetime(ctx context.Context, id int64) error {
	if _, err := q.db.ExecContext(ctx,
		`UPDATE work_queue SET lyric_offset_ms = NULL, lyric_edited_at = ? WHERE id = ?`,
		formatTime(q.now()), id); err != nil {
		return fmt.Errorf("queue: set lyric retime %d: %w", id, err)
	}
	return nil
}

// LyricRetimed reports whether the row's mark is a retime: edited, no offset.
func (q *DBQueue) LyricRetimed(ctx context.Context, id int64) (bool, error) {
	var retimed bool
	if err := q.db.QueryRowContext(ctx,
		`SELECT lyric_edited_at IS NOT NULL AND lyric_offset_ms IS NULL FROM work_queue WHERE id = ?`, id).Scan(&retimed); err != nil {
		return false, fmt.Errorf("queue: lyric retimed %d: %w", id, err)
	}
	return retimed, nil
}

// ClearLyricEdit forgets a hand edit (a revert, or an explicit re-fetch).
func (q *DBQueue) ClearLyricEdit(ctx context.Context, id int64) error {
	if _, err := q.db.ExecContext(ctx,
		`UPDATE work_queue SET lyric_offset_ms = NULL, lyric_edited_at = NULL WHERE id = ?`, id); err != nil {
		return fmt.Errorf("queue: clear lyric edit %d: %w", id, err)
	}
	return nil
}

// LyricEditedAmong returns which of ids carry a hand-edit mark. The timing
// sweep (#1226) stamps such a row's verdict but never remediates its file, so
// it asks after planning, immediately before applying.
func (q *DBQueue) LyricEditedAmong(ctx context.Context, ids []int64) (_ map[int64]bool, retErr error) {
	out := make(map[int64]bool)
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	query := `SELECT id FROM work_queue WHERE lyric_edited_at IS NOT NULL AND id IN (?` + strings.Repeat(",?", len(ids)-1) + `)` //nolint:gosec // reason: G202: only "?" placeholders are concatenated; every id is a bound parameter
	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("queue: lyric edited among: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("queue: close lyric edited rows: %w", cerr)
		}
	}()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("queue: lyric edited scan: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: lyric edited rows: %w", err)
	}
	return out, nil
}

// LyricEdit reports the row's recorded hand edit.
func (q *DBQueue) LyricEdit(ctx context.Context, id int64) (int, bool, error) {
	var off sql.NullInt64
	var at sql.NullString
	if err := q.db.QueryRowContext(ctx,
		`SELECT lyric_offset_ms, lyric_edited_at FROM work_queue WHERE id = ?`, id).Scan(&off, &at); err != nil {
		return 0, false, fmt.Errorf("queue: lyric edit %d: %w", id, err)
	}
	return int(off.Int64), at.Valid, nil
}
