package reports

import (
	"context"
	"fmt"
	"time"
)

// SourceEventCount is one source_event_daily row (#1301).
type SourceEventCount struct {
	Day   string
	Lane  string
	Event string
	Count int64
}

// sourceEvents returns the per-day, per-source counters for the UTC days from
// through to (inclusive), ordered by day, lane, event; a non-empty lane scopes
// the read to that lane in SQL (the primary key is (day, lane, event)).
// Delivered-type events (word/line/unsynced/instrumental) count landings, not
// distinct tracks: a track delivered line and later rechecked to word counts
// both. hit and miss mirror the provider_outcomes credits. Consumer:
// SourceTrend (#1302).
func (r *Repo) sourceEvents(ctx context.Context, from, to time.Time, lane string) ([]SourceEventCount, error) {
	q := `SELECT day, lane, event, count FROM source_event_daily WHERE day >= ? AND day <= ?`
	args := []any{from.UTC().Format("2006-01-02"), to.UTC().Format("2006-01-02")}
	if lane != "" {
		q += ` AND lane = ?`
		args = append(args, lane)
	}
	rows, err := r.db.QueryContext(ctx, q+` ORDER BY day, lane, event`, args...)
	if err != nil {
		return nil, fmt.Errorf("reports: source events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SourceEventCount
	for rows.Next() {
		var c SourceEventCount
		if err := rows.Scan(&c.Day, &c.Lane, &c.Event, &c.Count); err != nil {
			return nil, fmt.Errorf("reports: scan source event: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: source events rows: %w", err)
	}
	return out, nil
}
