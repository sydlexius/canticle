package reports

import (
	"context"
	"fmt"

	"github.com/sydlexius/canticle/internal/failsig"
	"github.com/sydlexius/canticle/internal/queue"
)

// MaxFailureItemsLimit caps one FailureGroupItems call so a caller cannot ask
// for an entire (possibly tens-of-thousands-row) group.
const MaxFailureItemsLimit = 500

// FailureItem is one work_queue row inside a Failure Analysis / Deferred misses
// group. Reason is the group's normalized signature (the same value as
// FailureGroup.Reason), Class its failsig verdict. Class is set only for
// failed rows; it is empty for deferred rows, which are provider misses the
// worker already retries on a schedule (failsig.Classify reads failed-row error
// signatures and would label them persistent).
type FailureItem struct {
	ID            int64
	Artist        string
	Title         string
	Album         string
	Status        string
	Reason        string
	Class         failsig.Class
	NextAttemptAt string
	MissCount     int64
	Attempts      int64
	UpdatedAt     string
}

// FailureGroupItems returns up to limit rows of the failure group
// (status, signature), newest first (updated_at DESC, id DESC). status is
// 'failed' or 'deferred'; signature is a FailureGroup.Reason as returned by
// FailureAnalysis / DeferredMisses.
//
// Membership is defined by the grouped report itself: the row filter is the
// shared failureRowFilters and the signature is normalizedReason, so a group's
// count and its expanded rows cannot disagree. SQLite has no regex, so rows are
// normalized in Go; they stream newest first and the scan stops at limit
// matches. limit is clamped to [1, MaxFailureItemsLimit]. An unknown signature
// yields an empty result, not an error. Read-only.
//
// Per-row artist/title is deliberate, as for ListBucket: this feeds the
// authenticated, session-gated UI.
func (r *Repo) FailureGroupItems(ctx context.Context, status, signature string, limit int) ([]FailureItem, error) {
	where, ok := failureRowFilters[status]
	if !ok {
		return nil, fmt.Errorf("reports: failure group items: unsupported status %q", status)
	}
	if limit < 1 {
		limit = 1
	}
	if limit > MaxFailureItemsLimit {
		limit = MaxFailureItemsLimit
	}
	// where comes from the constant failureRowFilters map, never caller input.
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, artist, title, album, status,
                COALESCE(NULLIF(last_error, ''), ?),
                COALESCE(next_attempt_at, ''), miss_count, attempts, COALESCE(updated_at, '')
         FROM work_queue
         WHERE `+where+`
         ORDER BY updated_at DESC, id DESC`,
		queue.NoReasonRecorded, status)
	if err != nil {
		return nil, fmt.Errorf("reports: failure group items (%s): %w", status, err)
	}
	defer func() { _ = rows.Close() }()

	var out []FailureItem
	for len(out) < limit && rows.Next() {
		var it FailureItem
		if err := rows.Scan(&it.ID, &it.Artist, &it.Title, &it.Album, &it.Status, &it.Reason,
			&it.NextAttemptAt, &it.MissCount, &it.Attempts, &it.UpdatedAt); err != nil {
			return nil, fmt.Errorf("reports: scan failure item: %w", err)
		}
		it.Reason = normalizedReason(it.Reason)
		if it.Reason != signature {
			continue
		}
		if it.Status == "failed" {
			it.Class = failsig.Classify(it.Reason)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: failure item rows: %w", err)
	}
	return out, nil
}
