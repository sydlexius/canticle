package queue

import (
	"context"
	"fmt"
	"strings"
)

// UpstreamCandidate is one row `scan reconcile-upstream` may fill (#1298).
type UpstreamCandidate struct {
	ID                int64
	AudioPath, Status string
	Lane              string
}

// ListUpstreamCandidates returns the done or processing rows with a
// provider_lane in lanes and a NULL upstream. The caller passes the lanes that
// can report an upstream (providers.ReportsUpstream), so queue holds no second
// list. processing rows are returned so the caller can count them as skipped.
func (q *DBQueue) ListUpstreamCandidates(ctx context.Context, lanes []string) ([]UpstreamCandidate, error) {
	if len(lanes) == 0 {
		return nil, nil
	}
	args := make([]any, len(lanes))
	for i, l := range lanes {
		args[i] = l
	}
	const head = `SELECT id, COALESCE(source_path,''), status, provider_lane FROM work_queue
          WHERE status IN ('done','processing') AND upstream IS NULL AND provider_lane IN (`
	marks := strings.TrimSuffix(strings.Repeat("?,", len(lanes)), ",")
	query := head + marks + `) ORDER BY id` //nolint:gosec // reason: G202 -- only "?" placeholders are concatenated; the lane values are bound
	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("queue: list upstream candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []UpstreamCandidate
	for rows.Next() {
		var c UpstreamCandidate
		if err := rows.Scan(&c.ID, &c.AudioPath, &c.Status, &c.Lane); err != nil {
			return nil, fmt.Errorf("queue: scan upstream candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetUpstreamIfPending writes upstream onto a row only if it is still done,
// still on c.Lane and still has a NULL upstream, so a row that changed since
// listing (re-fetched, re-laned, filled by a completion) is never overwritten.
// It never touches provider_lane. backup runs inside the transaction once the
// row is confirmed, before commit (write-ahead). A raced row returns false, nil.
func (q *DBQueue) SetUpstreamIfPending(ctx context.Context, c UpstreamCandidate, upstream string, backup func() error) (bool, error) {
	if upstream == "" {
		return false, fmt.Errorf("queue: set upstream for id %d: empty upstream", c.ID)
	}
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("queue: set upstream for id %d: begin tx: %w", c.ID, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	res, err := tx.ExecContext(ctx,
		`UPDATE work_queue SET upstream = ? WHERE id = ? AND status = 'done' AND provider_lane = ? AND upstream IS NULL`,
		upstream, c.ID, c.Lane)
	if err != nil {
		return false, fmt.Errorf("queue: set upstream for id %d: %w", c.ID, err)
	}
	if n, rerr := res.RowsAffected(); rerr != nil || n == 0 {
		return false, rerr
	}
	if backup != nil {
		if berr := backup(); berr != nil {
			return false, fmt.Errorf("queue: set upstream for id %d: backup failed, rolled back: %w", c.ID, berr)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("queue: set upstream for id %d: commit: %w", c.ID, err)
	}
	return true, nil
}
