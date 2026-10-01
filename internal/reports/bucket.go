package reports

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/sydlexius/canticle/internal/queue"
)

// Bucket names one drill-down population of work_queue rows (#598). A bucket
// is not always a bare status: Finished and Settled are predicates over
// status='done', and together they partition it exactly (the same split
// QueueSummary reports as Finished / SettledUpgradable).
type Bucket string

// The drill-down buckets. Retired rows are status='unavailable' since #477
// (migration 049), so Unavailable is a plain status bucket, not a
// (status, last_error) pair.
const (
	BucketPending     Bucket = "pending"
	BucketProcessing  Bucket = "processing"
	BucketDeferred    Bucket = "deferred"
	BucketFailed      Bucket = "failed"
	BucketFinished    Bucket = "finished"
	BucketSettled     Bucket = "settled"
	BucketUnavailable Bucket = "unavailable"
)

// bucketPredicates is the ONE place a bucket becomes SQL. Each value is a
// constant fragment (never built from caller input) with no leading AND/WHERE.
// finished reuses finishedPredicate and settled is its exact complement within
// done, so the two can never overlap or leave a gap.
var bucketPredicates = map[Bucket]string{
	BucketPending:     `status = 'pending'`,
	BucketProcessing:  `status = 'processing'`,
	BucketDeferred:    `status = 'deferred'`,
	BucketFailed:      `status = 'failed'`,
	BucketFinished:    finishedPredicate,
	BucketSettled:     `status = 'done' AND NOT COALESCE((` + finishedPredicate + `), 0)`,
	BucketUnavailable: `status = 'unavailable'`,
}

// Buckets returns every valid bucket in display order.
func Buckets() []Bucket {
	return []Bucket{
		BucketPending, BucketProcessing, BucketDeferred, BucketFailed,
		BucketFinished, BucketSettled, BucketUnavailable,
	}
}

// ParseBucket validates a caller-supplied key (e.g. a URL path segment).
func ParseBucket(key string) (Bucket, error) {
	b := Bucket(key)
	if _, ok := bucketPredicates[b]; !ok {
		return "", fmt.Errorf("reports: unknown bucket %q", key)
	}
	return b, nil
}

// BucketLibrary is one library a bucket row is linked to.
type BucketLibrary struct {
	ID   int64
	Name string
}

// BucketRow is one work_queue row in a drill-down. Reason is the shared
// failsig-normalized last_error (queue.NoReasonRecorded when none). Libraries
// lists EVERY library the row is linked to through work_queue_scan_results; it
// is empty for a CLI-enqueued row with no scan link.
type BucketRow struct {
	ID            int64
	Artist        string
	Title         string
	Album         string
	Status        string
	Reason        string
	NextAttemptAt string
	MissCount     int64
	Attempts      int64
	UpdatedAt     string
	Libraries     []BucketLibrary
	// Previewable reports a settled synced row whose tier is current: the same
	// wordTierPredicate/lineTierPredicate the dashboard counts, so a
	// timing-remediated, recheck-queued or prune-retired row (whose stamped
	// tier may be stale) is not offered to the player. Decided in SQL from the
	// row's recorded state, never by touching the disk.
	Previewable bool
}

// MaxBucketLimit caps one page so a caller cannot ask for the whole table.
const MaxBucketLimit = 500

// ListBucket returns up to limit rows of the bucket with id > afterID, ordered
// by id ASC. Keyset pagination on id is stable for rows that stay in the
// bucket under concurrent worker writes (offset would drift); a row that moves
// bucket mid-walk may be dropped or missed. The next page's cursor is the last returned row's ID;
// fewer than limit rows means the bucket is exhausted. limit is clamped to
// [1, MaxBucketLimit]. An unknown bucket is an error.
//
// Per-row artist/title is deliberate, as for ReviewQueue: this feeds the
// authenticated, session-gated UI.
func (r *Repo) ListBucket(ctx context.Context, bucket Bucket, afterID int64, limit int) ([]BucketRow, error) {
	pred, ok := bucketPredicates[bucket]
	if !ok {
		return nil, fmt.Errorf("reports: unknown bucket %q", string(bucket))
	}
	if limit < 1 {
		limit = 1
	}
	if limit > MaxBucketLimit {
		limit = MaxBucketLimit
	}
	// pred is selected from the constant map above, never from caller input.
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, artist, title, album, status,
                COALESCE(NULLIF(last_error, ''), ?),
                COALESCE(next_attempt_at, ''), miss_count, attempts, COALESCE(updated_at, ''),
                COALESCE(status = 'done' AND outcome_type = 'synced'
                 AND ((`+wordTierPredicate+`) OR (`+lineTierPredicate+`)), 0)
         FROM work_queue
         WHERE id > ? AND (`+pred+`)
         ORDER BY id ASC
         LIMIT ?`, queue.NoReasonRecorded, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("reports: list bucket %s: %w", bucket, err)
	}
	defer func() { _ = rows.Close() }()

	var out []BucketRow
	for rows.Next() {
		var it BucketRow
		if err := rows.Scan(&it.ID, &it.Artist, &it.Title, &it.Album, &it.Status, &it.Reason,
			&it.NextAttemptAt, &it.MissCount, &it.Attempts, &it.UpdatedAt, &it.Previewable); err != nil {
			return nil, fmt.Errorf("reports: scan bucket row: %w", err)
		}
		it.Reason = normalizedReason(it.Reason)
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: bucket rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("reports: close bucket rows: %w", err)
	}
	if err := r.attachLibraries(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// attachLibraries fills Libraries for one page in a single query through the
// work_queue_scan_results junction, so a row linked to several libraries lists
// all of them (a JOIN in the page query would duplicate the row and break the
// keyset cursor).
func (r *Repo) attachLibraries(ctx context.Context, items []BucketRow) error {
	if len(items) == 0 {
		return nil
	}
	idx := make(map[int64]int, len(items))
	ids := make([]string, 0, len(items))
	for i, it := range items {
		idx[it.ID] = i
		ids = append(ids, strconv.FormatInt(it.ID, 10))
	}
	// The ids travel as ONE JSON-array parameter read through json_each, so the
	// statement text is constant whatever the page size.
	rows, err := r.db.QueryContext(ctx,
		`SELECT DISTINCT wqsr.work_queue_id, l.id, l.name
         FROM work_queue_scan_results wqsr
         JOIN scan_results sr ON sr.id = wqsr.scan_result_id
         JOIN libraries l ON l.id = sr.library_id
         WHERE wqsr.work_queue_id IN (SELECT value FROM json_each(?))
         ORDER BY wqsr.work_queue_id, l.id`, "["+strings.Join(ids, ",")+"]")
	if err != nil {
		return fmt.Errorf("reports: bucket libraries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			wqID int64
			lib  BucketLibrary
		)
		if err := rows.Scan(&wqID, &lib.ID, &lib.Name); err != nil {
			return fmt.Errorf("reports: scan bucket library: %w", err)
		}
		i := idx[wqID]
		items[i].Libraries = append(items[i].Libraries, lib)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reports: bucket libraries rows: %w", err)
	}
	return nil
}
