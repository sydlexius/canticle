package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
)

// goneSourceMove is a planned repair of one work_queue row whose recorded audio
// file was replaced in place by a same-stem sibling (#1262: an album re-ripped
// from .mp3 to .flac). oldPath is the optimistic-concurrency guard for the
// write, gonePath the vanished file whose scan_results row is removed ("" for
// none), and keepLink leaves work_queue.scan_result_id as it is (the row is
// already linked to another library's row for the incoming file).
type goneSourceMove struct {
	id                int64
	oldPath, gonePath string
	keepLink          bool
}

// SameStemSibling reports whether a and b name two different files in one
// directory that share a stem and differ only by extension. That is the one
// shape where the row's sidecar (stem.lrc beside the audio) still describes
// the incoming file, so outdir, filename and output_paths need no rewrite and
// a settled row stays truthfully settled.
func SameStemSibling(a, b string) bool {
	if a == "" || b == "" || a == b || filepath.Dir(a) != filepath.Dir(b) {
		return false
	}
	return strings.TrimSuffix(filepath.Base(a), filepath.Ext(a)) == strings.TrimSuffix(filepath.Base(b), filepath.Ext(b))
}

// planGoneSourceMove decides whether the row inputs collides with should be
// moved to inputs.SourcePath. It runs BEFORE the enqueue transaction on
// purpose: a stat can block for seconds on a spun-down disk, and a write
// transaction must not be held across it. The write is guarded on what was
// read here (moveGoneSourceTx), so a row that changed in between is left alone.
//
// The move is planned only when the existing row is not 'processing', the two
// paths are same-stem siblings, the recorded file is DEFINITELY gone
// (fs.ErrNotExist) and the incoming one is present. Any other stat error
// (permission, an unavailable mount) reads as present, so an unreachable
// library never re-points rows; and while both copies exist the row stays
// where it is, so two copies of one track cannot flip-flop it.
//
// A row ALREADY at the incoming path is planned as a relink when its scan_result
// link is missing or names a definitely-gone same-stem sibling (a path-only
// caller moved it before a scan indexed the replacement). A row whose link is
// already right is handed to planLibraryRelink, for the overlapping library
// that scans the replacement second; else no stat is made.
func (q *DBQueue) planGoneSourceMove(ctx context.Context, inputs models.Inputs) (*goneSourceMove, error) {
	if inputs.SourcePath == "" {
		return nil, nil
	}
	var m goneSourceMove
	var linked string
	err := q.db.QueryRowContext(ctx,
		`SELECT id, source_path,
                COALESCE((SELECT file_path FROM scan_results WHERE id = work_queue.scan_result_id), '')
         FROM work_queue
         WHERE artist_key = ? AND title_key = ? AND status != 'processing'`,
		normalize.NormalizeKey(inputs.Track.ArtistName), normalize.NormalizeKey(inputs.Track.TrackName),
	).Scan(&m.id, &m.oldPath, &linked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("queue: read row for gone-source check: %w", err)
	}
	if m.oldPath == inputs.SourcePath {
		switch {
		case linked == "":
			return &m, nil
		case SameStemSibling(linked, inputs.SourcePath):
			if _, err := q.stat(linked); errors.Is(err, fs.ErrNotExist) {
				m.gonePath = linked
				return &m, nil
			}
			return nil, nil
		}
		return q.planLibraryRelink(ctx, &m, inputs)
	}
	if !SameStemSibling(m.oldPath, inputs.SourcePath) {
		return nil, nil
	}
	m.gonePath = m.oldPath
	if _, err := q.stat(m.oldPath); !errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if _, err := q.stat(inputs.SourcePath); err != nil {
		return nil, nil
	}
	return &m, nil
}

// planLibraryRelink covers overlapping libraries: scan_results is unique per
// (library_id, file_path), so each library holds its own row for the vanished
// file and its own for the replacement, and the move made for the first library
// to scan touches only that library's rows. When a later library offers the
// replacement the row already names it and is linked to the first library's
// scan_result, so nothing above fires. This plans the rest: remove THIS
// library's row for a definitely-gone same-stem sibling, link and settle its
// row for the incoming file, and leave work_queue.scan_result_id alone.
//
// The library is inputs.LibraryID, else that of inputs.ScanResultID; with
// neither, nothing is planned. The sibling is found by an index range over
// (library_id, file_path), not a directory read, and only if no OTHER queue
// row is linked to it. The stat is made only when such a row exists.
func (q *DBQueue) planLibraryRelink(ctx context.Context, m *goneSourceMove, inputs models.Inputs) (*goneSourceMove, error) {
	if inputs.LibraryID <= 0 && inputs.ScanResultID <= 0 {
		return nil, nil
	}
	prefix := strings.TrimSuffix(inputs.SourcePath, filepath.Ext(inputs.SourcePath)) + "."
	rows, err := q.db.QueryContext(ctx,
		`SELECT sr.file_path FROM scan_results sr
         WHERE sr.library_id = COALESCE(NULLIF(?, 0), (SELECT library_id FROM scan_results WHERE id = ?))
           AND sr.file_path >= ? AND sr.file_path < ? AND sr.file_path != ?
           AND NOT EXISTS (SELECT 1 FROM work_queue_scan_results j
                           WHERE j.scan_result_id = sr.id AND j.work_queue_id != ?)
         ORDER BY sr.id`,
		inputs.LibraryID, inputs.ScanResultID, prefix, prefix[:len(prefix)-1]+"/", inputs.SourcePath, m.id)
	if err != nil {
		return nil, fmt.Errorf("queue: read sibling scan_results for row %d: %w", m.id, err)
	}
	var siblings []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("queue: scan sibling scan_result for row %d: %w", m.id, err)
		}
		siblings = append(siblings, p)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("queue: read sibling scan_results for row %d: %w", m.id, err)
	}
	for _, p := range siblings { // stat only after the rows are closed
		if !SameStemSibling(p, inputs.SourcePath) {
			continue
		}
		if _, err := q.stat(p); errors.Is(err, fs.ErrNotExist) {
			m.gonePath, m.keepLink = p, true
			return m, nil
		}
	}
	return nil, nil
}

// moveResurrectSQL moves a row prune RETIRED as unresolvable (its file was
// gone and nothing could relink it) and reopens it, exactly as prune's own
// relink does (prune.relinkResurrectSQL): that 'done' never described a fetch,
// so the row is re-fetched at its new path. A hand-edited row is excluded and
// falls through to movePathSQL, as it does there (#1228).
//
// movePathSQL moves every other row and touches nothing else: status, outcome,
// provenance, timing, tier, attempts and timestamps all describe the track and
// its sidecar, which the swap did not change. A word-recheck or upgrade-armed
// row is moved the same way; what protects those rows is their output paths and
// mode, neither of which this writes, and their trip needs the audio that exists.
const (
	moveResurrectSQL = `UPDATE work_queue SET source_path = ?, scan_result_id = COALESCE(?, scan_result_id),
             status = 'pending', attempts = 0, next_attempt_at = ?,
             completed_at = NULL, last_error = '',
             outcome_type = NULL, outcome_detail = NULL, timing_outcome = NULL,
             ` + ClearWordRecheckQueued + `
         WHERE id = ? AND source_path = ? AND status = 'done' AND last_error = ?` + notLyricEdited
	movePathSQL = `UPDATE work_queue SET source_path = ?, scan_result_id = COALESCE(?, scan_result_id)
         WHERE id = ? AND source_path = ? AND status != 'processing'`
)

// moveGoneSourceTx applies m inside the enqueue transaction, before the upsert,
// so the upsert's "keep a settled row's paths" arms then keep the NEW path.
//
// The incoming file's scan_result is inputs.ScanResultID, or, for a caller that
// has none (RepointGoneSource is fed the scanner's results, which carry no id),
// the row naming the path in inputs.LibraryID: scan_results is unique per
// (library_id, file_path), so overlapping libraries each hold one. Only a
// caller with neither (a webhook) falls back to the lowest-id row naming the
// path, in whichever library. With none yet, the vanished file's scan_results
// row is KEPT and stays linked (deleting it would null the link, ON DELETE SET
// NULL); the relink arm of a later RepointGoneSource swaps the link and removes it.
//
// Only the incoming scan_result's OWN library loses its row for the vanished
// file. Another library's row, and its junction link, are left for that
// library's scan of the replacement (planLibraryRelink).
func moveGoneSourceTx(ctx context.Context, tx *sql.Tx, m *goneSourceMove, inputs models.Inputs, now string) (bool, error) {
	srID := inputs.ScanResultID
	if srID <= 0 {
		query, args := `SELECT id FROM scan_results WHERE file_path = ? ORDER BY id LIMIT 1`, []any{inputs.SourcePath}
		if inputs.LibraryID > 0 {
			query, args = `SELECT id FROM scan_results WHERE file_path = ? AND library_id = ?`, append(args, inputs.LibraryID)
		}
		err := tx.QueryRowContext(ctx, query, args...).Scan(&srID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("queue: find scan_result of new source for row %d: %w", m.id, err)
		}
	}
	scanResultID := nullableID(srID)
	if m.keepLink {
		scanResultID = nil
	}
	relink := m.oldPath == inputs.SourcePath // swaps the link only; never reopens a row
	if relink && srID <= 0 {
		return false, nil
	}
	var n int64
	if !relink {
		res, err := tx.ExecContext(ctx, moveResurrectSQL,
			inputs.SourcePath, scanResultID, now, m.id, m.oldPath, UnresolvableGoneError)
		if err != nil {
			return false, fmt.Errorf("queue: reopen retired row %d at its new source: %w", m.id, err)
		}
		n, _ = res.RowsAffected()
	}
	if n == 0 {
		res, err := tx.ExecContext(ctx, movePathSQL, inputs.SourcePath, scanResultID, m.id, m.oldPath)
		if err != nil {
			return false, fmt.Errorf("queue: move row %d to its new source: %w", m.id, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Claimed by the worker, or already re-pointed, since the plan read it.
			return false, nil
		}
	}
	if srID <= 0 { // nothing to link to yet: see the doc comment
		return true, nil
	}
	// The vanished file's scan_results row would otherwise strand: nothing else
	// removes it while its directory survives. Same in-flight guard as prune.
	// The junction cascades with it. Scoped to the incoming row's library.
	if m.gonePath != "" {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM scan_results WHERE file_path = ? AND id != ?
               AND library_id = (SELECT library_id FROM scan_results WHERE id = ?)
               AND NOT EXISTS (
                   SELECT 1 FROM work_queue_scan_results j
                   JOIN work_queue wq ON wq.id = j.work_queue_id
                   WHERE j.scan_result_id = scan_results.id AND wq.status = 'processing')`,
			m.gonePath, srID, srID); err != nil {
			return false, fmt.Errorf("queue: delete scan_result of vanished source for row %d: %w", m.id, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`,
		m.id, srID); err != nil {
		return false, fmt.Errorf("queue: link moved row %d to scan_result %d: %w", m.id, srID, err)
	}
	// A row that stays settled will never write its status back to the incoming
	// scan_result, which the scan enqueuer just reserved as 'processing'. Only a
	// swap repair settles it; linking a row that merely had no link does not.
	if m.gonePath == "" {
		return true, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE scan_results SET status = 'done' WHERE id = ?
           AND EXISTS (SELECT 1 FROM work_queue WHERE id = ? AND status IN ('done', 'unavailable'))`,
		srID, m.id); err != nil {
		return false, fmt.Errorf("queue: settle scan_result %d for moved row %d: %w", srID, m.id, err)
	}
	slog.Debug("queue: moved row to the same-stem file that replaced its vanished source", "id", m.id)
	return true, nil
}

// RepointGoneSource applies the same repair without enqueueing anything, for a
// file the scan indexed as already settled: its sidecar survived the swap, so
// the scan never offers it to Enqueue and the collision above never happens.
// It reports whether a row was moved or relinked; a row already at the incoming
// path with a live link costs two indexed reads, no stat and no write.
//
// LIMIT: it judges only the moment it is called. If the old file still exists
// then (copy, scan, then delete) or its stat fails for any reason but not-exist,
// nothing moves and scan.Enqueuer.RepointSettled never offers the file again:
// the row stays on the vanished file, as before #1262, until the same-stem
// sibling relink in prune (a later #1262 slice). The write retries on SQLITE_BUSY.
func (q *DBQueue) RepointGoneSource(ctx context.Context, inputs models.Inputs) (bool, error) {
	move, err := q.planGoneSourceMove(ctx, inputs)
	if err != nil || move == nil {
		return false, err
	}
	var moved bool
	err = db.RetryOnBusy(ctx, dequeueMaxAttempts, func() error {
		tx, err := q.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("queue: begin repoint tx: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		if moved, err = moveGoneSourceTx(ctx, tx, move, inputs, formatTime(q.now())); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("queue: commit repoint tx: %w", err)
		}
		return nil
	})
	return moved && err == nil, err
}
