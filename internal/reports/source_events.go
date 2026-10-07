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

// SourceEvents returns the per-day, per-source counters for the UTC days from
// through to (inclusive), ordered by day, lane, event. Consumer: the #1302 chart.
func (r *Repo) SourceEvents(ctx context.Context, from, to time.Time) ([]SourceEventCount, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT day, lane, event, count FROM source_event_daily
         WHERE day >= ? AND day <= ? ORDER BY day, lane, event`,
		from.UTC().Format("2006-01-02"), to.UTC().Format("2006-01-02"))
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
