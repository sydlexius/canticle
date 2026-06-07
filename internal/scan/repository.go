package scan

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	dbpkg "github.com/sydlexius/mxlrcgo-svc/internal/db"
	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/normalize"
)

const (
	// StatusPending marks a discovered file that has not been queued yet.
	StatusPending = "pending"
	// StatusProcessing marks a discovered file currently being processed.
	StatusProcessing = "processing"
	// StatusDone marks a discovered file whose lyrics work completed.
	StatusDone = "done"
	// StatusFailed marks a discovered file whose lyrics work failed.
	StatusFailed = "failed"
)

// Repo provides persistence for library scan results.
type Repo struct {
	db *sql.DB
}

// New creates a scan result repository backed by db.
func New(db *sql.DB) *Repo {
	return &Repo{db: db}
}

// UpsertOptions controls how Upsert handles existing rows on conflict.
type UpsertOptions struct {
	// ForceStatus, when true, replaces the existing row's status with the
	// incoming value. Used by forced rescans (--update / --upgrade) to
	// re-eligible already-completed rows for re-fetching. Default false
	// preserves the existing status so periodic scans cannot clobber
	// terminal states recorded by the worker.
	ForceStatus bool
}

// UpsertBatchSize bounds how many scan results are written per transaction.
// Small batches keep SQLite's write lock held only briefly, so a concurrent
// writer (the serve worker, or another scan process) is not starved past
// busy_timeout. This replaces the previous whole-library single transaction,
// which held the write lock for seconds and aborted the entire scan on a single
// SQLITE_BUSY.
const UpsertBatchSize = 500

// upsertMaxAttempts bounds the per-batch SQLITE_BUSY retries. busy_timeout
// already waits per attempt, so a handful of retries is ample.
const upsertMaxAttempts = 5

const baseUpsert = `INSERT INTO scan_results (library_id, file_path, artist, title, artist_key, title_key, outdir, filename, status)
             VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
             ON CONFLICT(library_id, file_path) DO UPDATE SET
                 artist = excluded.artist,
                 title = excluded.title,
                 artist_key = excluded.artist_key,
                 title_key = excluded.title_key,
                 outdir = excluded.outdir,
                 filename = excluded.filename`

// Upsert stores scan results for a library, keyed by library_id and file_path.
// On conflict, status is preserved by default; pass ForceStatus to overwrite.
//
// Results are written in batches of UpsertBatchSize, each in its own
// transaction retried on SQLITE_BUSY, so a concurrent writer cannot abort the
// whole scan. A batch that still fails after retries is logged and skipped while
// the remaining batches continue; this is safe because scan_results is an
// idempotent cache (a later scan re-upserts). If any batch failed, Upsert
// returns an aggregate error after processing them all.
func (r *Repo) Upsert(ctx context.Context, libraryID int64, results []models.ScanResult, opts UpsertOptions) error {
	if len(results) == 0 {
		return nil
	}
	totalBatches := (len(results) + UpsertBatchSize - 1) / UpsertBatchSize
	failed := 0
	for start := 0; start < len(results); start += UpsertBatchSize {
		end := start + UpsertBatchSize
		if end > len(results) {
			end = len(results)
		}
		batch := results[start:end]
		batchNum := start/UpsertBatchSize + 1

		err := dbpkg.RetryOnBusy(ctx, upsertMaxAttempts, func() error {
			tx, err := r.db.BeginTx(ctx, nil)
			if err != nil {
				return fmt.Errorf("scan: begin upsert tx: %w", err)
			}
			defer func() { _ = tx.Rollback() }()
			if err := upsertBatch(ctx, tx, libraryID, batch, opts); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("scan: commit upsert tx: %w", err)
			}
			return nil
		})
		if err != nil {
			failed++
			slog.Error("scan: upsert batch failed; skipping",
				"library_id", libraryID, "batch", batchNum, "of", totalBatches,
				"rows", len(batch), "error", err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("scan: %d of %d upsert batches failed", failed, totalBatches)
	}
	return nil
}

// upsertBatch writes one batch of scan results within tx.
func upsertBatch(ctx context.Context, tx *sql.Tx, libraryID int64, results []models.ScanResult, opts UpsertOptions) error {
	stmt := baseUpsert
	if opts.ForceStatus {
		stmt += `,
                 status = excluded.status`
	}
	for _, res := range results {
		insertStatus := res.Status
		if insertStatus == "" {
			insertStatus = StatusPending
		}
		if _, err := tx.ExecContext(ctx, stmt,
			libraryID,
			res.FilePath,
			res.Track.ArtistName,
			res.Track.TrackName,
			normalize.NormalizeKey(res.Track.ArtistName),
			normalize.NormalizeKey(res.Track.TrackName),
			res.Outdir,
			res.Filename,
			insertStatus,
		); err != nil {
			return fmt.Errorf("scan: upsert %s: %w", res.FilePath, err)
		}
	}
	return nil
}

// ListByLibrary returns persisted scan results for a library in stable ID order.
func (r *Repo) ListByLibrary(ctx context.Context, libraryID int64) (results []models.ScanResult, retErr error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, library_id, file_path, artist, title, outdir, filename, status, created_at
         FROM scan_results
         WHERE library_id = ?
         ORDER BY id ASC`,
		libraryID,
	)
	if err != nil {
		return nil, fmt.Errorf("scan: list by library: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("scan: close list rows: %w", err)
		}
	}()

	results, err = scanResultRows(rows)
	if err != nil {
		return nil, fmt.Errorf("scan: list rows: %w", err)
	}
	return results, nil
}

// ListPendingByLibrary returns pending scan results for a library in stable ID order.
func (r *Repo) ListPendingByLibrary(ctx context.Context, libraryID int64) (results []models.ScanResult, retErr error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, library_id, file_path, artist, title, outdir, filename, status, created_at
         FROM scan_results
         WHERE library_id = ?
           AND status = ?
         ORDER BY id ASC`,
		libraryID,
		StatusPending,
	)
	if err != nil {
		return nil, fmt.Errorf("scan: list pending by library: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("scan: close pending rows: %w", err)
		}
	}()

	results, err = scanResultRows(rows)
	if err != nil {
		return nil, fmt.Errorf("scan: list pending rows: %w", err)
	}
	return results, nil
}

func scanResultRows(rows *sql.Rows) ([]models.ScanResult, error) {
	var results []models.ScanResult
	for rows.Next() {
		var res models.ScanResult
		if err := rows.Scan(
			&res.ID,
			&res.LibraryID,
			&res.FilePath,
			&res.Track.ArtistName,
			&res.Track.TrackName,
			&res.Outdir,
			&res.Filename,
			&res.Status,
			&res.CreatedAt,
		); err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// Filter narrows the rows returned by List.
type Filter struct {
	// LibraryID, when non-nil, restricts results to a specific library row.
	LibraryID *int64
	// Status optionally restricts results to a single status value (e.g.
	// "pending", "processing", "done", "failed"). Empty means no filter.
	Status string
	// Limit caps the number of returned rows. Zero or negative means no
	// limit. Applied as a SQL LIMIT so the database does not materialize
	// the full result set when the caller only wants a slice.
	Limit int
}

// List returns persisted scan results matching filter in stable ID order.
func (r *Repo) List(ctx context.Context, filter Filter) (results []models.ScanResult, retErr error) {
	const baseQuery = `SELECT id, library_id, file_path, artist, title, outdir, filename, status, created_at
                       FROM scan_results`
	const orderClause = ` ORDER BY id ASC`
	var args []any
	var query string
	switch {
	case filter.LibraryID != nil && filter.Status != "":
		query = baseQuery + ` WHERE library_id = ? AND status = ?` + orderClause
		args = append(args, *filter.LibraryID, filter.Status)
	case filter.LibraryID != nil:
		query = baseQuery + ` WHERE library_id = ?` + orderClause
		args = append(args, *filter.LibraryID)
	case filter.Status != "":
		query = baseQuery + ` WHERE status = ?` + orderClause
		args = append(args, filter.Status)
	default:
		query = baseQuery + orderClause
	}
	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)
	}

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("scan: list: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("scan: close list rows: %w", err)
		}
	}()

	results, err = scanResultRows(rows)
	if err != nil {
		return nil, fmt.Errorf("scan: list rows: %w", err)
	}
	return results, nil
}

// FindByTrack returns scan results whose normalized artist and title keys match
// the given artist and title. The inputs are normalized with the same function
// used when rows are stored, so callers pass raw artist/title strings. Results
// are ordered so rows not yet done (pending, processing, failed) come first,
// then by id, so a caller that wants a single match can prefer work that still
// needs doing.
//
// Matching is by normalized key only and is NOT scoped to a library: if the same
// artist/title exists under multiple configured libraries, rows from all of them
// are returned and the cross-library order is just the id tiebreak. This is low
// impact in practice (the caller picks one via pickByAlbum) but means a match is
// not guaranteed to come from any particular library.
func (r *Repo) FindByTrack(ctx context.Context, artist, title string) (results []models.ScanResult, retErr error) {
	artistKey := normalize.NormalizeKey(artist)
	titleKey := normalize.NormalizeKey(title)
	if artistKey == "" || titleKey == "" {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, library_id, file_path, artist, title, outdir, filename, status, created_at
                       FROM scan_results
                       WHERE artist_key = ? AND title_key = ?
                       ORDER BY CASE status WHEN 'done' THEN 1 ELSE 0 END ASC, id ASC`,
		artistKey, titleKey,
	)
	if err != nil {
		return nil, fmt.Errorf("scan: find by track: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("scan: close find rows: %w", err)
		}
	}()

	results, err = scanResultRows(rows)
	if err != nil {
		return nil, fmt.Errorf("scan: find by track rows: %w", err)
	}
	return results, nil
}

// ClearByLibrary deletes every scan_results row belonging to libraryID and
// returns the number of rows deleted. The library row itself is left intact.
func (r *Repo) ClearByLibrary(ctx context.Context, libraryID int64) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM scan_results WHERE library_id = ?`,
		libraryID,
	)
	if err != nil {
		return 0, fmt.Errorf("scan: clear by library: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("scan: clear by library rows affected: %w", err)
	}
	return n, nil
}

// ClearByLibraryTx runs the same DELETE as ClearByLibrary inside a caller-
// supplied transaction so the scan_results delete can be committed atomically
// with other writes (e.g. work_queue cancellation). The caller owns Begin and
// Commit.
func (r *Repo) ClearByLibraryTx(ctx context.Context, tx *sql.Tx, libraryID int64) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM scan_results WHERE library_id = ?`,
		libraryID,
	)
	if err != nil {
		return 0, fmt.Errorf("scan: clear by library tx: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("scan: clear by library tx rows affected: %w", err)
	}
	return n, nil
}

// CountByLibrary returns the number of scan_results rows belonging to
// libraryID. It is useful for reporting what ClearByLibrary would delete
// without actually deleting anything.
func (r *Repo) CountByLibrary(ctx context.Context, libraryID int64) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM scan_results WHERE library_id = ?`,
		libraryID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("scan: count by library: %w", err)
	}
	return n, nil
}

// SetStatus updates scan result status for each id.
func (r *Repo) SetStatus(ctx context.Context, ids []int64, status string) error {
	if len(ids) == 0 {
		return nil
	}
	switch status {
	case StatusPending, StatusProcessing, StatusDone, StatusFailed:
	default:
		return fmt.Errorf("scan: unsupported status %q", status)
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("scan: begin set status tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, id := range ids {
		res, err := tx.ExecContext(ctx,
			`UPDATE scan_results SET status = ? WHERE id = ?`,
			status,
			id,
		)
		if err != nil {
			return fmt.Errorf("scan: set status %d: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("scan: set status rows affected: %w", err)
		}
		if n == 0 {
			return sql.ErrNoRows
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("scan: commit set status tx: %w", err)
	}
	return nil
}
