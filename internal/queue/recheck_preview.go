package queue

import (
	"context"
	"database/sql"
	"fmt"
)

// recheckRetiredWhere is the population RecheckRetired revives, and therefore
// the one RecheckRetiredPreview predicts. It is the single source of that
// predicate: RecheckRetired (its id SELECT and its UPDATE), CountRecheckRetired
// and the preview all concatenate this const, with the sentinel bound as its
// one placeholder, so a change here moves the preview, the count and the action
// together. TestRecheckRetiredPreviewMatchesRevive is a behavioral check on top:
// it pins the counts for the rows its fixture exercises, not the predicate text.
const recheckRetiredWhere = `status = 'unavailable' AND last_error = ?`

// RetiredLibraryCount is one library's share of the revivable population.
type RetiredLibraryCount struct {
	LibraryID int64
	Name      string
	// Count is the number of distinct retired work_queue rows linked to this
	// library through work_queue_scan_results: exactly what
	// RecheckRetired(&LibraryID) revives.
	Count int64
	// Shared is how many of those Count rows also link to a scan_result in at
	// least one OTHER library. Reviving this library alone still revives those
	// rows for the other libraries (the work_queue row is shared; only the
	// scan_results writeback is library-scoped).
	Shared int64
}

// RecheckRetiredPreview is the read-only blast radius of RecheckRetired.
type RecheckRetiredPreview struct {
	// Total is the number of retired rows RecheckRetired(nil) revives, linked or
	// not. It is NOT the sum of Libraries: a row shared by two libraries counts
	// once here and once per library there, and an unlinked row counts only here.
	Total int64
	// Libraries lists every library with at least one retired row, by id.
	Libraries []RetiredLibraryCount
	// Shared is the number of retired rows linked to more than one library,
	// across all libraries. For one library's share see RetiredLibraryCount.Shared.
	// Reviving for one of those libraries also revives the row for the others
	// (the work_queue row is deduped on artist/title; only the scan_results
	// writeback is library-scoped).
	Shared int64
	// Unlinked is the number of retired rows with no scan_results link. Only the
	// all-libraries revive reaches them.
	Unlinked int64
}

// RecheckRetiredPreview reports what RecheckRetired would revive, without
// writing. It reads the same predicate and the same library link
// (work_queue_scan_results -> scan_results.library_id) as recheckLibraryClause,
// inside one read transaction so the counts agree with each other.
func (q *DBQueue) RecheckRetiredPreview(ctx context.Context) (RecheckRetiredPreview, error) {
	var p RecheckRetiredPreview
	tx, err := q.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return p, fmt.Errorf("queue: begin recheck retired preview tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_queue WHERE `+recheckRetiredWhere, missLimitReachedError,
	).Scan(&p.Total); err != nil {
		return p, fmt.Errorf("queue: preview recheck retired total: %w", err)
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT l.id, l.name, COUNT(DISTINCT wq.id),
		        COUNT(DISTINCT CASE WHEN EXISTS (
		            SELECT 1 FROM work_queue_scan_results x
		              JOIN scan_results s2 ON s2.id = x.scan_result_id
		             WHERE x.work_queue_id = wq.id AND s2.library_id <> l.id)
		          THEN wq.id END)
		   FROM work_queue wq
		   JOIN work_queue_scan_results wqsr ON wqsr.work_queue_id = wq.id
		   JOIN scan_results sr ON sr.id = wqsr.scan_result_id
		   JOIN libraries l ON l.id = sr.library_id
		  WHERE wq.`+recheckRetiredWhere+`
		  GROUP BY l.id, l.name
		  ORDER BY l.id`, missLimitReachedError)
	if err != nil {
		return p, fmt.Errorf("queue: preview recheck retired per library: %w", err)
	}
	for rows.Next() {
		var c RetiredLibraryCount
		if err := rows.Scan(&c.LibraryID, &c.Name, &c.Count, &c.Shared); err != nil {
			_ = rows.Close()
			return p, fmt.Errorf("queue: preview recheck retired scan library: %w", err)
		}
		p.Libraries = append(p.Libraries, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return p, fmt.Errorf("queue: preview recheck retired library rows: %w", err)
	}
	_ = rows.Close()

	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (
		   SELECT wq.id
		     FROM work_queue wq
		     JOIN work_queue_scan_results wqsr ON wqsr.work_queue_id = wq.id
		     JOIN scan_results sr ON sr.id = wqsr.scan_result_id
		    WHERE wq.`+recheckRetiredWhere+`
		    GROUP BY wq.id
		   HAVING COUNT(DISTINCT sr.library_id) > 1)`, missLimitReachedError,
	).Scan(&p.Shared); err != nil {
		return p, fmt.Errorf("queue: preview recheck retired shared: %w", err)
	}

	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_queue wq
		  WHERE wq.`+recheckRetiredWhere+`
		    AND NOT EXISTS (SELECT 1 FROM work_queue_scan_results x WHERE x.work_queue_id = wq.id)`,
		missLimitReachedError,
	).Scan(&p.Unlinked); err != nil {
		return p, fmt.Errorf("queue: preview recheck retired unlinked: %w", err)
	}
	return p, nil
}
