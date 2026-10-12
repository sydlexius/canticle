package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

// AdoptCandidate is a settled ('done') scan_results row that no work_queue row
// links to (#1409): an audio file with a lyric file beside it that the scan
// indexed but the queue never saw.
type AdoptCandidate struct {
	ScanResultID int64
	FilePath     string
	Outdir       string
	Filename     string
	Track        models.Track
	// ArtistKey and TitleKey are the row's identity, taken from scan_results
	// (falling back to IdentityKeys of Track when a legacy row stored none).
	ArtistKey, TitleKey string
}

// AdoptClass is what the sidecar on disk says about a candidate. SyncTier is ""
// for an outcome that is not a synced .lrc.
type AdoptClass struct {
	OutcomeType string
	SyncTier    string
}

// ErrAdoptBackup wraps every error that came from the backup callback, so a
// caller can tell "the backup cannot be written" (stop the pass) from a
// database failure on one row.
var ErrAdoptBackup = errors.New("adopt backup failed")

// AdoptRecord is the restorable record written for every adoption BEFORE its
// transaction commits. Restoring an inserted row (Linked false) is deleting
// WorkQueueID; restoring a link-only adoption is deleting the junction row.
//
// ORPHANS: the record is written before COMMIT, so a record whose commit then
// failed names a WorkQueueID that never existed and that a later insert may
// reuse for a different song. A restorer MUST match on ScanResultID and
// SourcePath (and check the row's source_path) and never act on WorkQueueID
// alone.
type AdoptRecord struct {
	WorkQueueID  int64  `json:"work_queue_id"`
	ScanResultID int64  `json:"scan_result_id"`
	Linked       bool   `json:"link_only"`
	OutcomeType  string `json:"outcome_type,omitempty"`
	SyncTier     string `json:"sync_tier,omitempty"`
	SourcePath   string `json:"source_path"`
	AdoptedAt    string `json:"adopted_at"`
}

// AdoptResult says what one adoption attempt did.
type AdoptResult int

const (
	// AdoptNoRow means no work_queue row has the candidate's identity.
	AdoptNoRow AdoptResult = iota
	// AdoptInserted means a new 'done' row was inserted and linked.
	AdoptInserted
	// AdoptLinked means only the junction link was written to an existing row.
	AdoptLinked
	// AdoptAlreadyLinked means the identity's row is 'done' and the junction
	// link already existed (another adopter, or Enqueue, linked it first):
	// nothing was written and no backup record was produced.
	AdoptAlreadyLinked
	// AdoptNotSettled means the identity's row exists but is not 'done'
	// (processing, pending, failed, deferred, unavailable...): nothing was
	// written. Linking it would let RecheckRetired revive the settled file.
	AdoptNotSettled
)

// ListUnlinkedSettled returns up to limit 'done' scan_results rows of
// libraryID with no work_queue_scan_results link, id > afterID, in id order.
// A non-empty pathPrefix (a directory or file path) narrows it to files at or
// under that path, so a scoped watcher rescan never reaches elsewhere.
// Keyset-paged so a caller can walk the backlog without a LIMIT that rows it
// will never adopt (no lyric file) could fill. DB-only: no file I/O.
func (q *DBQueue) ListUnlinkedSettled(ctx context.Context, libraryID int64, pathPrefix string, afterID int64, limit int) ([]AdoptCandidate, error) {
	// Normalize so a trailing separator or a root prefix cannot match nothing,
	// and match on a whole path component so /x/a never reaches /x/ab.
	prefix, under := "", ""
	if pathPrefix != "" {
		prefix = filepath.Clean(pathPrefix)
		under = prefix
		if !strings.HasSuffix(under, string(filepath.Separator)) {
			under += string(filepath.Separator)
		}
	}
	rows, err := q.db.QueryContext(ctx,
		`SELECT sr.id, sr.file_path, sr.outdir, sr.filename, sr.artist, sr.title, sr.album, sr.album_artist, sr.artist_key, sr.title_key
         FROM scan_results sr
         WHERE sr.library_id = ? AND sr.status = 'done' AND sr.id > ?
           AND (? = '' OR sr.file_path = ? OR substr(sr.file_path, 1, length(?)) = ?)
           AND NOT EXISTS (SELECT 1 FROM work_queue_scan_results l WHERE l.scan_result_id = sr.id)
         ORDER BY sr.id ASC LIMIT ?`,
		libraryID, afterID, prefix, prefix, under, under, limit)
	if err != nil {
		return nil, fmt.Errorf("queue: list unlinked settled: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AdoptCandidate
	for rows.Next() {
		var c AdoptCandidate
		if err := rows.Scan(&c.ScanResultID, &c.FilePath, &c.Outdir, &c.Filename, &c.Track.ArtistName, &c.Track.TrackName,
			&c.Track.AlbumName, &c.Track.AlbumArtist, &c.ArtistKey, &c.TitleKey); err != nil {
			return nil, fmt.Errorf("queue: scan unlinked settled: %w", err)
		}
		if c.ArtistKey == "" || c.TitleKey == "" {
			c.ArtistKey, c.TitleKey = IdentityKeys(c.Track)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: unlinked settled rows: %w", err)
	}
	return out, nil
}

// LinkSettled links c to the work_queue row that already has its identity,
// writing only the junction row (no file read, no new row). Only a 'done' row
// is linked: RecheckRetired flips every linked scan row from done to pending
// when it revives a row, so a settled file linked to a retired, pending,
// failed, deferred or processing row could be re-enqueued. Any other status
// is left alone (AdoptNotSettled). With no such row it writes nothing
// (AdoptNoRow).
// backup runs inside the transaction, before commit; its failure rolls the
// link back. A link that already existed writes nothing and records nothing
// (AdoptAlreadyLinked); the transaction rolls back unchanged.
func (q *DBQueue) LinkSettled(ctx context.Context, c AdoptCandidate, now time.Time, backup func(AdoptRecord) error) (AdoptResult, error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return AdoptNoRow, fmt.Errorf("queue: link settled: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	res, err := q.linkExistingTx(ctx, tx, c, now, backup)
	if err != nil || res != AdoptLinked {
		return res, err
	}
	if err := tx.Commit(); err != nil {
		return AdoptNoRow, fmt.Errorf("queue: link settled: commit: %w", err)
	}
	return AdoptLinked, nil
}

// linkExistingTx links c to the work_queue row with its identity, inside tx.
func (q *DBQueue) linkExistingTx(ctx context.Context, tx *sql.Tx, c AdoptCandidate, now time.Time, backup func(AdoptRecord) error) (AdoptResult, error) {
	var id int64
	var status string
	err := tx.QueryRowContext(ctx, `SELECT id, status FROM work_queue WHERE artist_key = ? AND title_key = ?`, c.ArtistKey, c.TitleKey).Scan(&id, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return AdoptNoRow, nil
	}
	if err != nil {
		return AdoptNoRow, fmt.Errorf("queue: look up settled identity: %w", err)
	}
	if status != StatusDone {
		return AdoptNotSettled, nil
	}
	ins, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, id, c.ScanResultID)
	if err != nil {
		return AdoptNoRow, fmt.Errorf("queue: link settled: %w", err)
	}
	n, err := ins.RowsAffected()
	if err != nil {
		return AdoptNoRow, fmt.Errorf("queue: link settled: rows affected: %w", err)
	}
	if n == 0 {
		// The link pre-existed. A backup record here would let a restorer delete
		// a junction row this call did not create.
		return AdoptAlreadyLinked, nil
	}
	if backup != nil {
		rec := AdoptRecord{WorkQueueID: id, ScanResultID: c.ScanResultID, Linked: true, SourcePath: c.FilePath, AdoptedAt: formatTime(now)}
		if berr := backup(rec); berr != nil {
			return AdoptNoRow, fmt.Errorf("queue: link settled: %w, link rolled back: %w", ErrAdoptBackup, berr)
		}
	}
	return AdoptLinked, nil
}

// InsertSettledDone inserts a 'done' row for c carrying cls and links it, in
// ONE transaction, adopting a file that has no queue row. lane and upstream stay
// NULL (a source is never invented), attempts 0, and completed_at is now so the
// upgrade sweep's standard hold applies before the first attempt.
//
// The word-recheck sweep has no completed_at hold (it passes no
// CompletedBefore); its own cooldown is word_timing_checked_at on a row whose
// word_timing_state is NULL ("the sweep last touched this row at", see
// word_timing.go), which UnexaminedCheckedBefore reads. A synced row is stamped
// with now there, so it waits the sweep's normal readmit interval before it is a
// candidate. word_timing_state stays NULL: no word-timing verdict is recorded
// because none was reached.
//
// ON CONFLICT DO NOTHING on the identity key: a racing adoption (or a worker
// enqueue) that won the key makes this fall back to the link-only path inside
// the same transaction, so two adoptions collapse to one row. If that
// fallback finds the link already present it returns AdoptAlreadyLinked: the
// transaction rolls back with nothing written and no backup record.
//
// TODO(#1559): the identity here is (artist_key, title_key). When the queue
// identity gains an edition column this insert and the lookup in
// linkExistingTx must become edition-aware, or two editions collapse.
func (q *DBQueue) InsertSettledDone(ctx context.Context, c AdoptCandidate, cls AdoptClass, now time.Time, backup func(AdoptRecord) error) (AdoptResult, error) {
	outputPaths, err := marshalOutputPaths(models.Inputs{Outdir: c.Outdir, Filename: c.Filename})
	if err != nil {
		return AdoptNoRow, err
	}
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return AdoptNoRow, fmt.Errorf("queue: insert settled: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	stamp := formatTime(now)
	var wordChecked any
	if cls.OutcomeType == "synced" {
		wordChecked = stamp
	}
	var id int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO work_queue (artist, title, album, album_artist, artist_key, title_key, outdir, filename, source_path, output_paths,
             scan_result_id, status, attempts, outcome_type, sync_tier, completed_at, word_timing_checked_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'done', 0, ?, ?, ?, ?)
         ON CONFLICT(artist_key, title_key) DO NOTHING RETURNING id`,
		c.Track.ArtistName, c.Track.TrackName, c.Track.AlbumName, c.Track.AlbumArtist, c.ArtistKey, c.TitleKey,
		c.Outdir, c.Filename, c.FilePath, outputPaths, c.ScanResultID, cls.OutcomeType, nullIfEmpty(cls.SyncTier), stamp, wordChecked).Scan(&id)
	switch {
	case err == nil:
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, id, c.ScanResultID); err != nil {
			return AdoptNoRow, fmt.Errorf("queue: insert settled: link: %w", err)
		}
		rec := AdoptRecord{WorkQueueID: id, ScanResultID: c.ScanResultID, OutcomeType: cls.OutcomeType, SyncTier: cls.SyncTier, SourcePath: c.FilePath, AdoptedAt: stamp}
		if backup != nil {
			if berr := backup(rec); berr != nil {
				return AdoptNoRow, fmt.Errorf("queue: insert settled: %w, adoption rolled back: %w", ErrAdoptBackup, berr)
			}
		}
		if err := tx.Commit(); err != nil {
			return AdoptNoRow, fmt.Errorf("queue: insert settled: commit: %w", err)
		}
		return AdoptInserted, nil
	case errors.Is(err, sql.ErrNoRows):
		res, lerr := q.linkExistingTx(ctx, tx, c, now, backup)
		if lerr != nil || res != AdoptLinked {
			return res, lerr
		}
		if err := tx.Commit(); err != nil {
			return AdoptNoRow, fmt.Errorf("queue: insert settled: commit: %w", err)
		}
		return AdoptLinked, nil
	default:
		return AdoptNoRow, fmt.Errorf("queue: insert settled: %w", err)
	}
}
