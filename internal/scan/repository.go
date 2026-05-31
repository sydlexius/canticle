package scan

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
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

// Upsert stores scan results for a library, keyed by library_id and file_path.
// On conflict, status is preserved by default; pass ForceStatus to overwrite.
func (r *Repo) Upsert(ctx context.Context, libraryID int64, results []models.ScanResult, opts UpsertOptions) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("scan: begin upsert tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const baseUpsert = `INSERT INTO scan_results (library_id, file_path, artist, title, outdir, filename, status)
             VALUES (?, ?, ?, ?, ?, ?, ?)
             ON CONFLICT(library_id, file_path) DO UPDATE SET
                 artist = excluded.artist,
                 title = excluded.title,
                 outdir = excluded.outdir,
                 filename = excluded.filename`
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
		_, err := tx.ExecContext(ctx, stmt,
			libraryID,
			res.FilePath,
			res.Track.ArtistName,
			res.Track.TrackName,
			res.Outdir,
			res.Filename,
			insertStatus,
		)
		if err != nil {
			return fmt.Errorf("scan: upsert %s: %w", res.FilePath, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("scan: commit upsert tx: %w", err)
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
