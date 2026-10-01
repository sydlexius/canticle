package reports

import (
	"context"
	"fmt"
	"time"

	"github.com/sydlexius/canticle/internal/queue"
)

// InFlightItem is one work_queue row a worker has claimed and not yet settled.
type InFlightItem struct {
	ID     int64
	Artist string
	Title  string
	Album  string
	// ClaimedAt is when the row entered processing. Zero when the stored value
	// is absent or unparsable, so a caller can render "unknown" rather than a
	// bogus elapsed time.
	ClaimedAt time.Time
}

// InFlight returns every row with status='processing', oldest claim first (id
// breaks ties). It never assumes one row: a worker pool claims several at once,
// and a crash leaves an orphan that stays 'processing' until startup recovery,
// so a caller tells a live claim from an orphan by ClaimedAt age, not by count.
//
// ClaimedAt reads work_queue.claimed_at (migration 057, stamped by the claim
// statements), falling back to updated_at for a row claimed before that
// migration. updated_at alone is not a claim time: the updated_at trigger fires
// on every write, including the completion stamps that land while a row is
// still processing. Read-only; the result feeds the session-gated UI, which is
// why it carries artist/title/album.
func (r *Repo) InFlight(ctx context.Context) ([]InFlightItem, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, artist, title, album, COALESCE(claimed_at, updated_at, '')
         FROM work_queue
         WHERE status = ?
         ORDER BY COALESCE(claimed_at, updated_at) ASC, id ASC`,
		queue.StatusProcessing)
	if err != nil {
		return nil, fmt.Errorf("reports: in flight: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []InFlightItem
	for rows.Next() {
		var it InFlightItem
		var claimed string
		if err := rows.Scan(&it.ID, &it.Artist, &it.Title, &it.Album, &claimed); err != nil {
			return nil, fmt.Errorf("reports: scan in flight: %w", err)
		}
		// Stored as ISO 'T...Z' (strftime in the claim and the trigger);
		// RFC3339 parses exactly that. A bad value leaves ClaimedAt zero.
		if t, perr := time.Parse(time.RFC3339, claimed); perr == nil {
			it.ClaimedAt = t
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: in flight rows: %w", err)
	}
	return out, nil
}
