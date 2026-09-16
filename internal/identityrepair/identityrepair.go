// Package identityrepair re-derives the stored artist identity of existing
// scan_results (and their coupled work_queue rows) by re-reading each file's
// tags, correcting rows ingested before the multi-value ID3v2.4 artist fix
// (issue #466).
//
// The mangled run-together artist ("A", "B", "C" stored as "ABC") cannot be
// un-joined from the database alone -- the value boundaries were destroyed at
// tag-read time -- so the only source of truth is the file on disk. A caller
// supplies an IdentityReader (scanner.ReadArtistIdentity in production) that
// re-reads a path and returns the corrected artist / album-artist; this package
// owns the database reconciliation.
//
// scan_results is keyed on (library_id, file_path), so its identity columns
// update in place with no conflict risk. work_queue carries a UNIQUE
// (artist_key, title_key): a scan row's OLD (artist_key, title_key) therefore
// identifies at most one queue row, which is re-keyed to the corrected identity
// -- or, when a row already occupies the corrected key, merged into it (the
// more-advanced status is preserved and junction links are re-pointed). A queue
// row that is mid-flight ('processing') is left untouched, and the whole change
// for that file is skipped so scan_results and work_queue never drift apart.
package identityrepair

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/queue"
)

// IdentityReader re-reads the corrected artist and album-artist for a file. It
// returns a non-nil error when the file cannot be opened or parsed, so the
// repairer skips the row rather than blanking a genuine identity.
type IdentityReader func(path string) (artist, albumArtist string, err error)

// Op names which table -- and which row's prior state -- a Change's Old*
// fields describe (#963's CodeRabbit finding: a divergence Change's Old*
// values are the WORK_QUEUE row's prior identity, not scan_results', so a
// backup consumer needs to know which to restore into). OpScanCorrection is
// Run's tag-re-read correction, where Old* describes the scan_results row
// itself. Every OpQueue* value is a RepairDivergence outcome, where Old*
// describes the work_queue row being touched (or, for OpQueueUnlink/
// OpQueueDelete, the queue row a member was dropped from) -- scan_results, if
// it changes at all in that path, is a status reset, not an identity write,
// and is not what Old*/New* describe.
type Op string

const (
	// OpScanCorrection is Run's per-file tag re-read correction: Old*/New*
	// describe the scan_results row identified by ScanResultID.
	OpScanCorrection Op = "scan_correction"
	// OpQueueRekey is RepairDivergence's unanimous-group re-key-in-place:
	// Old*/New* describe the work_queue row WorkQueueID.
	OpQueueRekey Op = "queue_rekey"
	// OpQueueMerge is RepairDivergence's unanimous-group merge into an
	// existing correct-key row: Old*/New* describe the OLD (dropped)
	// work_queue row's identity; WorkQueueID is that dropped row's id.
	OpQueueMerge Op = "queue_merge"
	// OpQueueDisplaySync is RepairDivergence's key-agrees/display-columns-only
	// sync: Old*/New* describe the work_queue row WorkQueueID.
	OpQueueDisplaySync Op = "queue_display_sync"
	// OpQueueUnlink is RepairDivergence's disagreement-branch unlink of one
	// divergent scan_results member from its queue row: Old* is the queue
	// row's (still-unchanged) identity, New* is the unlinked member's own;
	// WorkQueueID is the queue row the member was unlinked from.
	OpQueueUnlink Op = "queue_unlink"
	// OpQueueDelete is RepairDivergence deleting a work_queue row left with no
	// linked member after every member was unlinked: Old* is the deleted
	// row's identity, New* is unset; WorkQueueID is the deleted row's id.
	OpQueueDelete Op = "queue_delete"
)

// Change describes one corrected row's before/after identity, reported to the
// caller (for a restorable backup / preview) once per applied change. Op
// names the operation and which table Old*/New* describe (see the Op
// constants); WorkQueueID is set for every RepairDivergence Op and zero for
// OpScanCorrection, which has no single work_queue row of its own -- Run's
// caller (apply) reconciles a scan_results row's coupled queue row as a
// separate, unreported side effect of the same Change.
type Change struct {
	Op             Op
	ScanResultID   int64
	WorkQueueID    int64
	LibraryID      int64
	FilePath       string
	OldArtist      string
	NewArtist      string
	OldAlbumArtist string
	NewAlbumArtist string
	OldArtistKey   string
	NewArtistKey   string
}

// Result tallies the outcome of a Run.
type Result struct {
	Scanned         int // scan_results rows examined
	ReadFailures    int // files that could not be re-read (skipped, identity untouched)
	Changed         int // scan_results rows whose identity was corrected
	QueueUpdated    int // work_queue rows re-keyed/synced in place
	QueueMerged     int // work_queue rows merged into an existing correct-key row
	ProcessingSkips int // changes skipped because a linked work_queue row was in-flight
}

// Options controls a Run.
type Options struct {
	// LibraryID, when non-nil, limits the repair to one library's rows.
	LibraryID *int64
	// DryRun computes and reports the changes without mutating the database.
	DryRun bool
	// Report is invoked once per change: in a dry run before returning (a
	// preview), under apply after the change commits (a durable record). Nil
	// disables it. A Report error aborts the Run.
	Report func(Change) error
	// Progress, when set, is called periodically with the number of rows scanned
	// so far, so a long backfill over a large library can surface liveness.
	Progress func(scanned int)
	// PathPrefix, when non-empty, scopes RepairDivergence's candidate join to
	// scan_results rows whose file_path is AT or UNDER this directory (equal,
	// or prefix + a path separator -- never a lexical/LIKE prefix, so a
	// sibling directory sharing a name prefix, e.g. "/a/b" vs "/a/bc", is not
	// matched). It is ignored by Run, which always re-reads every in-scope
	// row's file regardless of path. commands.scheduler's OnScanComplete sets
	// it to the rescanned subtree on a TriggerWatcher callback (#963 follow-up
	// findings 3/4): the watcher only just touched files under that path, and
	// a watcher-driven subtree rescan should not pay for a library-wide join
	// on every keystroke-sized change. A TriggerScheduler (full-library) pass
	// leaves this unset and keeps library scope.
	PathPrefix string
}

// progressEvery bounds how often Progress fires (every N rows scanned).
const progressEvery = 500

// Repairer reconciles stored artist identity against the files on disk.
type Repairer struct {
	db   *sql.DB
	read IdentityReader
}

// New builds a Repairer over db using read to re-derive identity from disk.
func New(db *sql.DB, read IdentityReader) *Repairer {
	return &Repairer{db: db, read: read}
}

// row is a scan_results row's stored identity, loaded up front so the file
// re-reads (slow I/O) do not hold a database cursor open.
type row struct {
	id          int64
	libraryID   int64
	filePath    string
	artist      string
	albumArtist string
	artistKey   string
	titleKey    string
}

// Run re-reads every in-scope scan_results row's file and corrects the stored
// identity where it differs. It returns the tally of what changed (or would
// change, under DryRun).
func (r *Repairer) Run(ctx context.Context, opts Options) (Result, error) {
	rows, err := r.load(ctx, opts.LibraryID)
	if err != nil {
		return Result{}, err
	}

	var res Result
	for _, rw := range rows {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Scanned++
		if opts.Progress != nil && res.Scanned%progressEvery == 0 {
			opts.Progress(res.Scanned)
		}

		newArtist, newAlbumArtist, rerr := r.read(rw.filePath)
		if rerr != nil {
			res.ReadFailures++
			continue
		}
		newKey := normalize.NormalizeKey(newArtist)
		if newArtist == rw.artist && newAlbumArtist == rw.albumArtist && newKey == rw.artistKey {
			continue // already correct
		}

		ch := Change{
			Op:             OpScanCorrection,
			ScanResultID:   rw.id,
			LibraryID:      rw.libraryID,
			FilePath:       rw.filePath,
			OldArtist:      rw.artist,
			NewArtist:      newArtist,
			OldAlbumArtist: rw.albumArtist,
			NewAlbumArtist: newAlbumArtist,
			OldArtistKey:   rw.artistKey,
			NewArtistKey:   newKey,
		}

		if opts.DryRun {
			res.Changed++
			if opts.Report != nil {
				if err := opts.Report(ch); err != nil {
					return res, fmt.Errorf("identityrepair: report change for scan_result %d: %w", ch.ScanResultID, err)
				}
			}
			continue
		}

		// opts.Report runs INSIDE apply's transaction, before commit, so the
		// restorable backup record is durable before the correction it protects
		// commits (backup-first) and a report failure rolls the change back.
		outcome, err := r.apply(ctx, ch, rw.titleKey, opts.Report)
		if err != nil {
			return res, err
		}
		if outcome.processingSkip {
			res.ProcessingSkips++
			continue
		}
		res.Changed++
		res.QueueUpdated += outcome.queueUpdated
		res.QueueMerged += outcome.queueMerged
	}
	return res, nil
}

// load reads the stored identity of every in-scope scan_results row into memory
// so the per-row file re-reads do not hold a long-lived database cursor open.
func (r *Repairer) load(ctx context.Context, libraryID *int64) ([]row, error) {
	q := `SELECT id, library_id, file_path, artist, album_artist, artist_key, title_key
	      FROM scan_results WHERE file_path != ''`
	var args []any
	if libraryID != nil {
		q += ` AND library_id = ?`
		args = append(args, *libraryID)
	}
	q += ` ORDER BY id`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("identityrepair: query scan_results: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []row
	for rows.Next() {
		var rw row
		if err := rows.Scan(&rw.id, &rw.libraryID, &rw.filePath, &rw.artist, &rw.albumArtist, &rw.artistKey, &rw.titleKey); err != nil {
			return nil, fmt.Errorf("identityrepair: scan row: %w", err)
		}
		out = append(out, rw)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identityrepair: iterate scan_results: %w", err)
	}
	return out, nil
}

// applyOutcome reports what a single apply did to the coupled work_queue row.
type applyOutcome struct {
	queueUpdated   int
	queueMerged    int
	processingSkip bool
}

// apply corrects one scan_results row and reconciles its coupled work_queue row
// inside a single transaction. When the linked queue row (at either the old or
// the corrected key) is mid-flight ('processing'), the entire change is skipped
// so scan_results and work_queue never drift apart.
func (r *Repairer) apply(ctx context.Context, ch Change, titleKey string, report func(Change) error) (applyOutcome, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return applyOutcome{}, fmt.Errorf("identityrepair: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	lookup, err := probeQueueConflict(ctx, tx, ch, titleKey)
	if err != nil {
		return applyOutcome{}, err
	}
	if lookup.skip {
		return applyOutcome{processingSkip: true}, nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE scan_results SET artist = ?, album_artist = ?, artist_key = ? WHERE id = ?`,
		ch.NewArtist, ch.NewAlbumArtist, ch.NewArtistKey, ch.ScanResultID); err != nil {
		return applyOutcome{}, fmt.Errorf("identityrepair: update scan_results %d: %w", ch.ScanResultID, err)
	}

	out, err := reconcileQueue(ctx, tx, ch, titleKey, lookup)
	if err != nil {
		return applyOutcome{}, err
	}

	// Write the restorable backup record (report) before committing, so a report
	// failure aborts via the deferred rollback -- the correction is never applied
	// without its record (backup-first), and the report never over-records a row a
	// commit failure later rolls back.
	if report != nil {
		if err := report(ch); err != nil {
			return applyOutcome{}, fmt.Errorf("identityrepair: report change for scan_result %d: %w", ch.ScanResultID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return applyOutcome{}, fmt.Errorf("identityrepair: commit tx: %w", err)
	}
	return out, nil
}

// queueLookup bundles what probeQueueConflict found about the work_queue row(s)
// touching ch: the row (if any) carrying the OLD identity, and -- when the key
// is changing -- the row (if any) already occupying the corrected key. skip is
// true when either match is mid-flight ('processing'), meaning the whole
// change must be abandoned so scan_results and work_queue never drift apart.
type queueLookup struct {
	oldID, conflictID         int64
	oldStatus, conflictStatus string
	skip                      bool
}

// probeQueueConflict locates the work_queue row(s) relevant to ch without
// mutating anything, so both apply (which also corrects scan_results) and
// repairOneDivergentRow (RepairDivergence's queue-only path, #963) can share
// one conflict-detection reading.
func probeQueueConflict(ctx context.Context, tx *sql.Tx, ch Change, titleKey string) (queueLookup, error) {
	// Locate the queue row that carried the OLD identity (UNIQUE(artist_key,
	// title_key) => at most one). A 'processing' match aborts the whole change.
	oldID, oldStatus, err := queueRowAt(ctx, tx, ch.OldArtistKey, titleKey, 0)
	if err != nil {
		return queueLookup{}, err
	}
	if oldID != 0 && oldStatus == "processing" {
		return queueLookup{skip: true}, nil
	}

	keyChanged := ch.NewArtistKey != ch.OldArtistKey

	// When the key changes AND there is an old-key queue row to re-key, a row may
	// already occupy the corrected key. If that row is in-flight, skip rather than
	// disturb it. With no old-key row (oldID == 0) nothing touches the queue, so a
	// conflict at the corrected key is irrelevant and must not gate the harmless
	// scan_results correction.
	var conflictID int64
	var conflictStatus string
	if keyChanged && oldID != 0 {
		conflictID, conflictStatus, err = queueRowAt(ctx, tx, ch.NewArtistKey, titleKey, oldID)
		if err != nil {
			return queueLookup{}, err
		}
		if conflictID != 0 && conflictStatus == "processing" {
			return queueLookup{skip: true}, nil
		}
	}
	return queueLookup{oldID: oldID, oldStatus: oldStatus, conflictID: conflictID, conflictStatus: conflictStatus}, nil
}

// reconcileQueue applies ch's queue-side reconciliation using an
// already-probed lookup: re-key in place, sync display columns, merge on
// conflict, or drop a stale junction link -- the same #960/#961-constrained
// switch apply has always run. It performs NO scan_results write and does NOT
// commit or report -- the caller (apply, or RepairDivergence's
// repairOneDivergentRow, #963) owns the transaction lifecycle so a
// scan_results write (when present) and the queue reconciliation commit or
// roll back together.
func reconcileQueue(ctx context.Context, tx *sql.Tx, ch Change, titleKey string, lookup queueLookup) (applyOutcome, error) {
	keyChanged := ch.NewArtistKey != ch.OldArtistKey
	oldID, oldStatus := lookup.oldID, lookup.oldStatus
	conflictID, conflictStatus := lookup.conflictID, lookup.conflictStatus

	var out applyOutcome
	switch {
	case oldID == 0:
		// No queue row at the OLD key. Usually the row was never enqueued, or a
		// sibling scan sharing this identity already re-keyed the shared queue row
		// to the SAME corrected key (consistent -- keep the link). The exception is
		// the divergent case: a queue row shared by scans whose corrected identities
		// differ, where a sibling re-keyed it to a DIFFERENT key, leaving this
		// scan_result linked to a queue row of the wrong identity. Drop any junction
		// link to a queue row that is NOT at this scan's corrected (artist_key,
		// title_key) so the row re-enqueues cleanly on the next scan instead of
		// carrying a mismatched link.
		if keyChanged {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM work_queue_scan_results
				 WHERE scan_result_id = ?
				   AND work_queue_id IN (
				       SELECT id FROM work_queue WHERE NOT (artist_key = ? AND title_key = ?))`,
				ch.ScanResultID, ch.NewArtistKey, titleKey); err != nil {
				return applyOutcome{}, fmt.Errorf("identityrepair: drop stale junction links for scan_result %d: %w", ch.ScanResultID, err)
			}
		}
	case !keyChanged:
		// Artist unchanged but album-artist differs: sync the display columns; the
		// key is stable so no conflict is possible.
		if _, err := tx.ExecContext(ctx,
			`UPDATE work_queue SET artist = ?, album_artist = ? WHERE id = ?`,
			ch.NewArtist, ch.NewAlbumArtist, oldID); err != nil {
			return applyOutcome{}, fmt.Errorf("identityrepair: sync work_queue %d: %w", oldID, err)
		}
		out.queueUpdated = 1
	case conflictID == 0:
		// Corrected key is free: re-key the queue row in place. This is the
		// COMMON path for a genuine identity correction -- the corrected key
		// usually collides with nothing -- so it is where issue #960 lived: a
		// 'done' row's identity was corrected here but the row itself stayed
		// settled forever, since nothing re-queried it under the corrected key.
		if _, err := tx.ExecContext(ctx,
			`UPDATE work_queue SET artist = ?, album_artist = ?, artist_key = ? WHERE id = ?`,
			ch.NewArtist, ch.NewAlbumArtist, ch.NewArtistKey, oldID); err != nil {
			return applyOutcome{}, fmt.Errorf("identityrepair: re-key work_queue %d: %w", oldID, err)
		}
		out.queueUpdated = 1
		// Reopen a 'done' row so the worker re-fetches under the corrected
		// identity (#960). queue.ReopenDoneRowTx is a no-op (and correctly so)
		// on any other status: 'pending'/'deferred' have nothing settled to
		// reopen, and 'unavailable' is deliberately left retired -- that key
		// already exhausted its miss budget, and queue.RecheckRetired is the
		// designed revival path (see mergeQueueRows' doc comment for the same
		// reasoning on the merge side).
		if oldStatus == queue.StatusDone {
			if _, err := queue.ReopenDoneRowTx(ctx, tx, oldID, time.Now().UTC()); err != nil {
				return applyOutcome{}, fmt.Errorf("identityrepair: reopen re-keyed work_queue %d: %w", oldID, err)
			}
		}
	default:
		// A row already holds the corrected key: merge the old-key row into it.
		if err := mergeQueueRows(ctx, tx, oldID, conflictID, conflictStatus, ch.ScanResultID); err != nil {
			return applyOutcome{}, err
		}
		out.queueMerged = 1
	}
	return out, nil
}

// queueRowAt returns the id and status of the work_queue row at (artistKey,
// titleKey), excluding excludeID (pass 0 to exclude nothing). It returns
// (0, "", nil) when none exists. UNIQUE(artist_key, title_key) guarantees at
// most one match.
func queueRowAt(ctx context.Context, tx *sql.Tx, artistKey, titleKey string, excludeID int64) (int64, string, error) {
	var id int64
	var status string
	err := tx.QueryRowContext(ctx,
		`SELECT id, status FROM work_queue WHERE artist_key = ? AND title_key = ? AND id != ?`,
		artistKey, titleKey, excludeID).Scan(&id, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("identityrepair: lookup work_queue (%q,%q): %w", artistKey, titleKey, err)
	}
	return id, status, nil
}

// mergeQueueRows folds the old-key queue row (dropID) into the row already at the
// corrected key (keepID) so the two duplicate identities collapse to one queue
// row that will (re)write every linked file's lyrics.
//
// The survivor's output_paths are unioned with the dropped row's, so both files'
// sidecar targets are covered. The survivor's status is NOT fabricated -- doing
// so would break the invariant that a 'done' work_queue row implies its linked
// scan_results are 'done' (queue.Complete). Instead, a survivor that already
// completed is reopened to 'pending' via queue.ReopenDoneRowTx so the worker
// re-fetches and writes the newly-unioned paths (the write is idempotent for
// the already-satisfied one). That reopen clears every settle-state column
// (provider_lane, outcome_type, outcome_detail, timing_outcome, the overrun
// pair, evaluated_at) alongside status/attempts/next_attempt_at/last_error/
// completed_at -- #960: a done-era outcome_type left on a now-pending row is
// the same stale-vs-NULL ambiguity #655/#773 exist to prevent, and the merge
// path reopened the row without clearing it before this fix.
//
// An 'unavailable' survivor (#477: an exhausted benign miss) is deliberately
// NOT reopened: it gains the unioned output_paths and nothing else. Its
// (artist, title) key is the one the merge collapses onto, and that key has
// already exhausted its miss budget, so a reopen would buy one fetch of the
// same lookup before queue.RetireMiss re-retired it. queue.RecheckRetired is
// the designed revival path for 'unavailable' rows, and it will carry the
// unioned paths when it runs. ReopenDoneRowTx's own status = 'done' guard
// enforces this: calling it on an 'unavailable' (or any non-'done') survivor
// is a no-op by construction, so this function does not need its own status
// branch beyond the one that decides whether to call it at all.
//
// Any other survivor keeps its status and picks up the merged paths on its
// next run. The dropped row's scan_result links
// (and the current scan_result, in case it lived only on the dropped row's
// scalar link) are re-pointed to the survivor before the dropped row is
// deleted.
func mergeQueueRows(ctx context.Context, tx *sql.Tx, dropID, keepID int64, keepStatus string, scanResultID int64) error {
	merged, err := unionOutputPaths(ctx, tx, keepID, dropID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE work_queue SET output_paths = ? WHERE id = ?`, merged, keepID); err != nil {
		return fmt.Errorf("identityrepair: union output_paths into work_queue %d: %w", keepID, err)
	}
	if keepStatus == queue.StatusDone {
		if _, err := queue.ReopenDoneRowTx(ctx, tx, keepID, time.Now().UTC()); err != nil {
			return fmt.Errorf("identityrepair: reopen merged work_queue %d: %w", keepID, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO work_queue_scan_results (work_queue_id, scan_result_id)
		 SELECT ?, scan_result_id FROM work_queue_scan_results WHERE work_queue_id = ?`,
		keepID, dropID); err != nil {
		return fmt.Errorf("identityrepair: re-point junction rows to %d: %w", keepID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`,
		keepID, scanResultID); err != nil {
		return fmt.Errorf("identityrepair: link scan_result %d to %d: %w", scanResultID, keepID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM work_queue_scan_results WHERE work_queue_id = ?`, dropID); err != nil {
		return fmt.Errorf("identityrepair: clear junction rows for %d: %w", dropID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM work_queue WHERE id = ?`, dropID); err != nil {
		return fmt.Errorf("identityrepair: delete merged work_queue %d: %w", dropID, err)
	}
	return nil
}

// unionOutputPaths returns the JSON output_paths of keepID and dropID merged and
// de-duplicated by (outdir, filename), keep's entries first. When a row's
// output_paths column is empty or unparsable, its write targets are
// reconstructed from its linked scan_results (outdir, filename) rather than
// dropped -- otherwise deleting the merged row would permanently lose a file's
// rewrite target for legacy or corrupt rows.
func unionOutputPaths(ctx context.Context, tx *sql.Tx, keepID, dropID int64) (string, error) {
	read := func(id int64) ([]models.OutputPath, error) {
		var raw string
		if err := tx.QueryRowContext(ctx, `SELECT output_paths FROM work_queue WHERE id = ?`, id).Scan(&raw); err != nil {
			return nil, fmt.Errorf("identityrepair: read output_paths of work_queue %d: %w", id, err)
		}
		if raw != "" {
			var paths []models.OutputPath
			if err := json.Unmarshal([]byte(raw), &paths); err == nil && len(paths) > 0 {
				return paths, nil
			}
			// Empty array or malformed JSON: fall through to reconstruction so the
			// row's write targets are not silently lost when it is deleted.
		}
		return reconstructOutputPaths(ctx, tx, id)
	}
	keep, err := read(keepID)
	if err != nil {
		return "", err
	}
	drop, err := read(dropID)
	if err != nil {
		return "", err
	}
	seen := make(map[string]bool, len(keep)+len(drop))
	out := make([]models.OutputPath, 0, len(keep)+len(drop))
	for _, p := range append(keep, drop...) {
		k := p.Outdir + "\x00" + p.Filename
		if !seen[k] {
			seen[k] = true
			out = append(out, p)
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("identityrepair: marshal unioned output_paths: %w", err)
	}
	return string(b), nil
}

// reconstructOutputPaths rebuilds a queue row's write targets from the outdir /
// filename of its linked scan_results, used when its output_paths column is
// empty or unparsable so a merge never discards a recoverable rewrite target.
func reconstructOutputPaths(ctx context.Context, tx *sql.Tx, workQueueID int64) ([]models.OutputPath, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT sr.outdir, sr.filename FROM scan_results sr
		 JOIN work_queue_scan_results j ON j.scan_result_id = sr.id
		 WHERE j.work_queue_id = ? AND sr.outdir != '' AND sr.filename != ''`,
		workQueueID)
	if err != nil {
		return nil, fmt.Errorf("identityrepair: reconstruct output_paths for work_queue %d: %w", workQueueID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []models.OutputPath
	for rows.Next() {
		var p models.OutputPath
		if err := rows.Scan(&p.Outdir, &p.Filename); err != nil {
			return nil, fmt.Errorf("identityrepair: scan reconstructed path: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identityrepair: iterate reconstructed paths: %w", err)
	}
	return out, nil
}
