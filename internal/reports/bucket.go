package reports

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/tablesort"
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
	// SortVal is the row's value under the listing's sort, encoded for a
	// tablesort.Cursor ("n" when NULL or unsorted); the next page's cursor reads it.
	SortVal string
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
	return r.ListBucketFiltered(ctx, bucket, BucketFilter{}, tablesort.Order{}, tablesort.Cursor{ID: afterID}, limit)
}

// BucketFilter narrows a bucket listing (#1234). The zero value is no filter.
// Later discoverability slices add their fields here.
type BucketFilter struct {
	// Query is a case-insensitive substring searched in artist and title. It is
	// normalized with normalize.NormalizeKey, the function that stamps the
	// stored artist_key/title_key, so the two sides agree on case and accents.
	// A query that normalizes to empty applies no filter.
	Query string
}

// bucketColumns is the Work Queue's sortable columns (#1242), over the shared
// tablesort vocabulary. Artist and title sort on the normalized keys (case and
// accent folded, the same keys the search uses); album has no key column.
// Reason, Libraries and Lyrics are deliberately not sortable, and neither is
// Status: every bucket is a single status, so sorting on it would only be id order.
var bucketColumns = map[string]tablesort.Column{
	tablesort.KeyArtist:      {Expr: "artist_key"},
	tablesort.KeyAlbum:       {Expr: "album COLLATE NOCASE"},
	tablesort.KeyTitle:       {Expr: "title_key"},
	tablesort.KeyNextAttempt: {Expr: "next_attempt_at"},
	tablesort.KeyMisses:      {Expr: "miss_count", Integer: true, DescFirst: true},
	tablesort.KeyAttempts:    {Expr: "attempts", Integer: true, DescFirst: true},
	tablesort.KeyUpdated:     {Expr: "updated_at", DescFirst: true},
}

// BucketSpec is the bucket's sort spec: the shared columns with the bucket's
// own default. Queued and Retrying list what the worker will try soonest;
// every other bucket lists what changed most recently.
func BucketSpec(b Bucket) tablesort.Spec {
	def := tablesort.Order{Key: tablesort.KeyUpdated, Desc: true}
	if b == BucketPending || b == BucketDeferred {
		def = tablesort.Order{Key: tablesort.KeyNextAttempt}
	}
	return tablesort.Spec{Columns: bucketColumns, ID: "id", Default: def}
}

// ListBucketFiltered is ListBucket with the filter applied as an extra
// predicate, ordered by o with a keyset cursor on (sort value, id) (#1242). The
// cursor must have passed BucketSpec(bucket).DecodeCursor for o.
//
// The search is instr(artist_key, ?) / instr(title_key, ?) on the normalized
// keys: a literal substring test, so '%', '_' and backslash in the query match
// themselves (LIKE would need escaping). The query is bound as a parameter,
// never concatenated, and never logged by this package.
func (r *Repo) ListBucketFiltered(ctx context.Context, bucket Bucket, f BucketFilter, o tablesort.Order, after tablesort.Cursor, limit int) ([]BucketRow, error) {
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
	spec := BucketSpec(bucket)
	if _, ok := spec.Columns[o.Key]; o.Key != "" && !ok {
		return nil, fmt.Errorf("reports: unknown sort %q", o.Key)
	}
	keyset, keyArgs := spec.Keyset(o, after)
	args := append([]any{queue.NoReasonRecorded}, keyArgs...)
	search := ""
	if q := normalize.NormalizeKey(f.Query); q != "" {
		search = ` AND (instr(artist_key, ?) > 0 OR instr(title_key, ?) > 0)`
		args = append(args, q, q)
	}
	args = append(args, limit)
	// pred, the sort expressions and the keyset text come from constant maps,
	// never from caller input; every caller value is a bound parameter.
	//nolint:gosec // reason: every concatenated fragment is a constant (bucket predicate, tablesort Spec expression); caller values are bound parameters.
	query := `SELECT id, artist, title, album, status,
                COALESCE(NULLIF(last_error, ''), ?),
                COALESCE(next_attempt_at, ''), miss_count, attempts, COALESCE(updated_at, ''),
                COALESCE(status = 'done' AND outcome_type = 'synced'
                 AND ((` + wordTierPredicate + `) OR (` + lineTierPredicate + `)), 0),
                ` + spec.SelectExpr(o) + `
         FROM work_queue
         WHERE (` + pred + `)` + keyset + search + `
         ORDER BY ` + spec.OrderBy(o) + `
         LIMIT ?`
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reports: list bucket %s: %w", bucket, err)
	}
	defer func() { _ = rows.Close() }()

	var out []BucketRow
	for rows.Next() {
		var (
			it BucketRow
			sv any
		)
		if err := rows.Scan(&it.ID, &it.Artist, &it.Title, &it.Album, &it.Status, &it.Reason,
			&it.NextAttemptAt, &it.MissCount, &it.Attempts, &it.UpdatedAt, &it.Previewable, &sv); err != nil {
			return nil, fmt.Errorf("reports: scan bucket row: %w", err)
		}
		it.SortVal = tablesort.EncodeValue(sv)
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
