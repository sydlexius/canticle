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
	"errors"
	"fmt"

	"github.com/doxazo-net/canticle/internal/normalize"
)

// IdentityReader re-reads the corrected artist and album-artist for a file. It
// returns a non-nil error when the file cannot be opened or parsed, so the
// repairer skips the row rather than blanking a genuine identity.
type IdentityReader func(path string) (artist, albumArtist string, err error)

// Change describes one scan_results row whose on-disk identity differs from what
// is stored. It is reported to the caller (for a restorable backup / preview)
// once per applied change; the Old* fields capture the pre-repair state.
type Change struct {
	ScanResultID   int64
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
					return res, err
				}
			}
			continue
		}

		outcome, err := r.apply(ctx, ch, rw.titleKey)
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
		if opts.Report != nil {
			if err := opts.Report(ch); err != nil {
				return res, err
			}
		}
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
func (r *Repairer) apply(ctx context.Context, ch Change, titleKey string) (applyOutcome, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return applyOutcome{}, fmt.Errorf("identityrepair: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Locate the queue row that carried the OLD identity (UNIQUE(artist_key,
	// title_key) => at most one). A 'processing' match aborts the whole change.
	oldID, oldStatus, err := queueRowAt(ctx, tx, ch.OldArtistKey, titleKey, 0)
	if err != nil {
		return applyOutcome{}, err
	}
	if oldID != 0 && oldStatus == "processing" {
		return applyOutcome{processingSkip: true}, nil
	}

	keyChanged := ch.NewArtistKey != ch.OldArtistKey

	// When the key changes, a row may already occupy the corrected key. If that
	// row is in-flight, skip rather than disturb it.
	var conflictID int64
	var conflictStatus string
	if keyChanged {
		conflictID, conflictStatus, err = queueRowAt(ctx, tx, ch.NewArtistKey, titleKey, oldID)
		if err != nil {
			return applyOutcome{}, err
		}
		if conflictID != 0 && conflictStatus == "processing" {
			return applyOutcome{processingSkip: true}, nil
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE scan_results SET artist = ?, album_artist = ?, artist_key = ? WHERE id = ?`,
		ch.NewArtist, ch.NewAlbumArtist, ch.NewArtistKey, ch.ScanResultID); err != nil {
		return applyOutcome{}, fmt.Errorf("identityrepair: update scan_results %d: %w", ch.ScanResultID, err)
	}

	var out applyOutcome
	switch {
	case oldID == 0:
		// No coupled queue row (never enqueued, or already completed and pruned).
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
		// Corrected key is free: re-key the queue row in place.
		if _, err := tx.ExecContext(ctx,
			`UPDATE work_queue SET artist = ?, album_artist = ?, artist_key = ? WHERE id = ?`,
			ch.NewArtist, ch.NewAlbumArtist, ch.NewArtistKey, oldID); err != nil {
			return applyOutcome{}, fmt.Errorf("identityrepair: re-key work_queue %d: %w", oldID, err)
		}
		out.queueUpdated = 1
	default:
		// A row already holds the corrected key: merge the old-key row into it.
		if err := mergeQueueRows(ctx, tx, oldID, oldStatus, conflictID, conflictStatus, ch.ScanResultID); err != nil {
			return applyOutcome{}, err
		}
		out.queueMerged = 1
	}

	if err := tx.Commit(); err != nil {
		return applyOutcome{}, fmt.Errorf("identityrepair: commit tx: %w", err)
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
// corrected key (keepID): the more-advanced status is preserved on the keeper,
// the dropped row's scan_result links are re-pointed, and the dropped row is
// deleted. The current scan_result is explicitly linked to the keeper so the
// association survives even if it lived only on the dropped row's scalar link.
func mergeQueueRows(ctx context.Context, tx *sql.Tx, dropID int64, dropStatus string, keepID int64, keepStatus string, scanResultID int64) error {
	if statusRank(dropStatus) > statusRank(keepStatus) {
		if _, err := tx.ExecContext(ctx,
			`UPDATE work_queue SET status = ? WHERE id = ?`, dropStatus, keepID); err != nil {
			return fmt.Errorf("identityrepair: promote work_queue %d status: %w", keepID, err)
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

// statusRank orders work_queue statuses by how much completed work they
// represent, so a merge preserves the most valuable state (a 'done' fetch is
// never downgraded to 'pending'). 'processing' rows never reach a merge (they
// abort the change earlier), so their rank is only a guard.
func statusRank(status string) int {
	switch status {
	case "done":
		return 4
	case "processing":
		return 3
	case "failed", "deferred":
		return 2
	case "pending":
		return 1
	default:
		return 0
	}
}
