package reports

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sydlexius/canticle/internal/failsig"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/tablesort"
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
	limit = clampFailureLimit(limit)
	// where comes from the constant failureRowFilters map, never caller input.
	rows, err := r.db.QueryContext(ctx,
		failureItemSelect+where+`
         ORDER BY updated_at DESC, id DESC`,
		queue.NoReasonRecorded, status)
	if err != nil {
		return nil, fmt.Errorf("reports: failure group items (%s): %w", status, err)
	}
	defer func() { _ = rows.Close() }()

	var out []FailureItem
	for len(out) < limit && rows.Next() {
		it, err := scanFailureItem(rows)
		if err != nil {
			return nil, err
		}
		if it.Reason != signature {
			continue
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: failure item rows: %w", err)
	}
	return out, nil
}

// clampFailureLimit bounds a per-row listing's limit to [1, MaxFailureItemsLimit].
func clampFailureLimit(limit int) int {
	return min(max(limit, 1), MaxFailureItemsLimit)
}

// failureItemSelect is the column list scanFailureItem reads, ending in WHERE;
// it binds one argument, the no-reason sentinel. Shared with NeedsAttention.
const failureItemSelect = `SELECT id, artist, title, album, status,
                COALESCE(NULLIF(last_error, ''), ?),
                COALESCE(next_attempt_at, ''), miss_count, attempts, COALESCE(updated_at, '')
         FROM work_queue
         WHERE `

// scanFailureItem scans one failureItemSelect row, normalizing its reason and
// classifying it when (and only when) the row is failed.
func scanFailureItem(rows *sql.Rows) (FailureItem, error) {
	var it FailureItem
	if err := rows.Scan(&it.ID, &it.Artist, &it.Title, &it.Album, &it.Status, &it.Reason,
		&it.NextAttemptAt, &it.MissCount, &it.Attempts, &it.UpdatedAt); err != nil {
		return FailureItem{}, fmt.Errorf("reports: scan failure item: %w", err)
	}
	it.Reason = normalizedReason(it.Reason)
	if it.Status == "failed" {
		it.Class = failsig.Classify(it.Reason)
	}
	return it, nil
}

// NeedsAttention returns up to limit failed and deferred rows, the work that has
// NOT produced a lyric outcome (#654 AC2). They are a separate axis from Recent
// Outcomes, which lists lyric classifications only, so they get their own list
// rather than a pill in the outcome column.
//
// ORDER: every failed row before any deferred row, then newest updated_at first
// (id DESC breaks ties). Failed first because a hard error needs a person and a
// deferred miss is already on a retry schedule. updated_at is the right column
// HERE and only here: the work_queue trigger restamps it on every write, so it
// reads "last attempt", which is what this list shows (and labels). Recent
// Outcomes keeps completed_at for the same reason in reverse: a retry restamp
// must never pose as a newer outcome.
//
// Membership is failureRowFilters, the same filters Failure Analysis and
// Deferred misses count, so a row is listed here exactly when those reports
// count it (the #789 never-attempted guard, and the parked/refused exclusions
// for deferred). Reason is the failsig-normalized signature, never raw
// last_error; Class is set for failed rows only. Read-only; per-row
// artist/title feeds the session-gated UI, as for FailureGroupItems. limit is
// clamped to [1, MaxFailureItemsLimit], as for FailureGroupItems: a negative
// LIMIT means "no limit" to SQLite, which would list every failed and deferred
// row.
func (r *Repo) NeedsAttention(ctx context.Context, limit int) ([]FailureItem, error) {
	limit = clampFailureLimit(limit)
	rows, err := r.db.QueryContext(ctx,
		failureItemSelect+`(`+failureRowFilters["failed"]+`) OR (`+failureRowFilters["deferred"]+`)
         ORDER BY status = 'deferred', updated_at DESC, id DESC
         LIMIT ?`,
		queue.NoReasonRecorded, "failed", "deferred", limit)
	if err != nil {
		return nil, fmt.Errorf("reports: needs attention: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []FailureItem
	for rows.Next() {
		it, err := scanFailureItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: needs attention rows: %w", err)
	}
	return out, nil
}

// FailureItemSpec is the sort of one failure group's rows (#1260): the Work
// Queue's columns, defaulting to the newest-updated-first order
// FailureGroupItems returns. It is a spec over the work_queue columns, so the
// keys are the queue bucket vocabulary.
var FailureItemSpec = tablesort.Spec{
	Columns: bucketColumns,
	ID:      "id",
	Default: tablesort.Order{Key: tablesort.KeyUpdated, Desc: true},
}

// SortFailureItems reorders items, which the caller has already bounded to
// the rows it shows (FailureGroupItems in its default order, truncated), by o.
// The row set is the caller's: only ids already in items are selected, so a
// sort can never change WHICH rows a truncated group shows. The ORDER BY is a
// fixed expression from FailureItemSpec. The id list is bounded by the caller's
// page size (at most MaxFailureItemsLimit), far under SQLite's variable limit.
func (r *Repo) SortFailureItems(ctx context.Context, items []FailureItem, o tablesort.Order) ([]FailureItem, error) {
	if len(items) < 2 {
		return items, nil
	}
	byID := make(map[int64]FailureItem, len(items))
	args := make([]any, 0, len(items))
	marks := make([]byte, 0, 2*len(items))
	for i, it := range items {
		byID[it.ID] = it
		args = append(args, it.ID)
		if i > 0 {
			marks = append(marks, ',')
		}
		marks = append(marks, '?')
	}
	// marks is only "?" and ","; the ORDER BY is a fixed spec expression.
	//nolint:gosec // reason: G202: marks is only '?' and ',' and the ORDER BY a fixed spec expression, never request text
	query := `SELECT id FROM work_queue WHERE id IN (` + string(marks) + `) ORDER BY ` + FailureItemSpec.OrderBy(o)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reports: sort failure items: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]FailureItem, 0, len(items))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("reports: scan sorted failure item: %w", err)
		}
		if it, ok := byID[id]; ok {
			out = append(out, it)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: sorted failure item rows: %w", err)
	}
	// A row that vanished between the two reads is simply absent: the survivors
	// stay in the requested order, so the header's sort state still matches.
	return out, nil
}
