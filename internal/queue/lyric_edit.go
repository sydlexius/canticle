package queue

import (
	"context"
	"database/sql"
	"fmt"
)

// notLyricEdited keeps automatic replacement paths away from a file a person
// adjusted by ear (#481 Stage 2). Shared by the upgrade sweep and the word
// recheck predicates. Leading AND.
const notLyricEdited = ` AND lyric_edited_at IS NULL`

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

// ClearLyricEdit forgets a hand edit (a revert, or an explicit re-fetch).
func (q *DBQueue) ClearLyricEdit(ctx context.Context, id int64) error {
	if _, err := q.db.ExecContext(ctx,
		`UPDATE work_queue SET lyric_offset_ms = NULL, lyric_edited_at = NULL WHERE id = ?`, id); err != nil {
		return fmt.Errorf("queue: clear lyric edit %d: %w", id, err)
	}
	return nil
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
