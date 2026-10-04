package reports

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/providers"
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
	// Tier, Edited and MisSynced are the #1235 chips; they AND together with each
	// other and the search. Tier is the TierLine key mapped through
	// tierPredicates; any other value applies no filter. Each chip reuses an
	// existing shared predicate. The repo applies whatever it is given: which
	// chips a bucket OFFERS (BucketChips) is decided by the caller, which drops
	// the rest before they reach a filter.
	Tier      string
	Edited    bool
	MisSynced bool
	// LibraryID, when positive, keeps only rows linked to that library (a
	// work_queue row dedupes several files, so it can belong to several
	// libraries; a CLI-enqueued row belongs to none and never matches). The id
	// is bound as a parameter. Whether the id names a real library is the
	// caller's check; an id that matches nothing simply lists nothing.
	LibraryID int64
	// Lane keeps only rows whose provider_lane equals it; a value outside Lanes
	// applies no filter. Every bucket offers it: a row keeps the lane that last
	// served or was rejected for it through a retire, an upgrade trip and a
	// verify failure, so the column is not confined to done rows. A row that
	// never reached a lane (NULL) or was settled by the detector never matches.
	Lane string
	// Reason keeps only rows in that failure-reason category (a ReasonCategory
	// key); a value outside the keys applies no filter. The
	// repo applies a valid key as given: which buckets OFFER it (HasReason) is the
	// caller's decision.
	Reason string
}

// Lanes are the provider lanes the Lane filter offers, in display order:
// providers.Known, the one list of built-in lanes (the values the worker
// stamps into provider_lane), so a provider added there is offered here and
// admitted by the request-log allowlist with no second list to update. The
// detector's instrumental lane is not a provider and is deliberately not
// offered.
func Lanes() []string {
	return providers.Known()
}

// ValidLane reports whether l is one of Lanes.
func ValidLane(l string) bool { return slices.Contains(Lanes(), l) }

// libraryPredicate is the Library filter: an EXISTS over the junction, not a
// JOIN, so a row linked to several files of one library cannot repeat and
// break the keyset cursor. The unary plus on wqsr.scan_result_id is
// load-bearing: without it SQLite drives the junction primary key with BOTH
// columns (work_queue_id=? AND scan_result_id=?) and walks the library's whole
// IN list once per candidate row, which is quadratic (seconds, then the write
// timeout, at ten thousand files; production never runs ANALYZE). With it the
// junction is searched by work_queue_id alone and the IN list is a bloom
// filter built once per query. A plain JOIN inside the EXISTS is as slow as
// the unadorned IN. TestLibraryPredicateUsesPrefixProbe pins the plan.
// Constant text; the library id is the one bound "?".
const libraryPredicate = `EXISTS (SELECT 1 FROM work_queue_scan_results wqsr
        WHERE wqsr.work_queue_id = work_queue.id
          AND +wqsr.scan_result_id IN (SELECT id FROM scan_results WHERE library_id = ?))`

// Libraries lists every configured library, by name then id, for the Library
// filter's options.
func (r *Repo) Libraries(ctx context.Context) ([]BucketLibrary, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id, name FROM libraries ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, fmt.Errorf("reports: list libraries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []BucketLibrary
	for rows.Next() {
		var l BucketLibrary
		if err := rows.Scan(&l.ID, &l.Name); err != nil {
			return nil, fmt.Errorf("reports: scan library: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: library rows: %w", err)
	}
	return out, nil
}

// TierLine is the one Tier chip key (the URL value tier=line). A word-synced
// chip is deliberately absent: Finished IS the word tier and Settled excludes it,
// so on either bucket that chip would be a no-op or always empty.
const TierLine = "line"

// tierPredicates maps a Tier key to its SQL: the dashboard's own shared tier
// predicate, restricted to synced outcomes exactly as ResultsBreakdown does.
// The ONE place a Tier value becomes SQL; callers never supply the text.
var tierPredicates = map[string]string{
	TierLine: `outcome_type = 'synced' AND ` + lineTierPredicate,
}

// Hand-edited (#1213) and mis-synced chips: constant fragments, no caller text.
const (
	editedPredicate = `lyric_edited_at IS NOT NULL`
	// timingWrongPredicate means exactly timing_outcome = 'mis_synced' on done rows
	// (composed with a done bucket), NOT the review queue's wider
	// ('mis_synced', 'categorical') set over any status.
	timingWrongPredicate = `timing_outcome = 'mis_synced'`
)

// Chip names one filter chip. The string is also its URL parameter name
// ("tier" for the line chip, which carries the value TierLine).
type Chip string

// The chips a bucket page can offer.
const (
	ChipLineSynced Chip = "tier"
	ChipEdited     Chip = "edited"
	ChipMissynced  Chip = "missync"
)

// bucketChips is the ONE place the chip set per bucket is decided, in display
// order. Finished is status done AND synced AND word tier (which excludes
// mis_synced) and Settled is its complement within done, so only Hand-edited
// can match on Finished, while Line-synced and Mis-synced only ever match on
// Settled. A chip a bucket does not list is never offered and never honored.
var bucketChips = map[Bucket][]Chip{
	BucketFinished: {ChipEdited},
	BucketSettled:  {ChipLineSynced, ChipEdited, ChipMissynced},
}

// BucketChips returns the chips b offers, in display order (nil for none). The
// result is a copy, so a caller cannot change the set other requests see.
func BucketChips(b Bucket) []Chip { return slices.Clone(bucketChips[b]) }

// HasChip reports whether bucket b offers chip c.
func HasChip(b Bucket, c Chip) bool {
	for _, have := range bucketChips[b] {
		if have == c {
			return true
		}
	}
	return false
}

// ValidTier reports whether t is a Tier chip key.
func ValidTier(t string) bool { _, ok := tierPredicates[t]; return ok }

// ChipBucket reports whether b offers any chip.
func ChipBucket(b Bucket) bool { return len(bucketChips[b]) > 0 }

// chipSQL renders the chip predicates as AND-joined constant fragments.
func (f BucketFilter) chipSQL() string {
	out := ""
	if p, ok := tierPredicates[f.Tier]; ok {
		out += ` AND (` + p + `)`
	}
	if f.Edited {
		out += ` AND ` + editedPredicate
	}
	if f.MisSynced {
		out += ` AND ` + timingWrongPredicate
	}
	return out
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

// bucketQuery builds the page query and its arguments; split from the listing
// so the plan test can EXPLAIN the real text.
func bucketQuery(bucket Bucket, f BucketFilter, o tablesort.Order, after tablesort.Cursor, limit int) (string, []any, error) {
	pred, ok := bucketPredicates[bucket]
	if !ok {
		return "", nil, fmt.Errorf("reports: unknown bucket %q", string(bucket))
	}
	if limit < 1 {
		limit = 1
	}
	if limit > MaxBucketLimit {
		limit = MaxBucketLimit
	}
	spec := BucketSpec(bucket)
	if _, ok := spec.Columns[o.Key]; o.Key != "" && !ok {
		return "", nil, fmt.Errorf("reports: unknown sort %q", o.Key)
	}
	keyset, keyArgs := spec.Keyset(o, after)
	args := append([]any{queue.NoReasonRecorded}, keyArgs...)
	search := ""
	if q := normalize.NormalizeKey(f.Query); q != "" {
		search = ` AND (instr(artist_key, ?) > 0 OR instr(title_key, ?) > 0)`
		args = append(args, q, q)
	}
	search += f.chipSQL()
	if ValidLane(f.Lane) {
		search += ` AND provider_lane = ?`
		args = append(args, f.Lane)
	}
	if ValidReason(f.Reason) {
		search += ` AND (` + reasonCaseSQL + `) = ?`
		args = append(args, f.Reason)
	}
	if f.LibraryID > 0 {
		search += ` AND ` + libraryPredicate
		args = append(args, f.LibraryID)
	}
	args = append(args, limit)
	// pred, the sort expressions and the keyset text come from constant maps,
	// never from caller input; every caller value is a bound parameter.
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
	return query, args, nil
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
	query, args, err := bucketQuery(bucket, f, o, after, limit)
	if err != nil {
		return nil, err
	}
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

// The failure-reason filter (#1235) on the Retrying and Errored buckets. last_error is free text and the displayed Reason is its failsig
// signature, computed in Go; a filter has to run in SQL so keyset paging sees
// every matching row exactly once. failsig.Classify cannot be expressed there
// (it reads segment starts and a status regex), so the categories are a small
// fixed set of case-insensitive substring tests over the RAW last_error, tried
// in order by one CASE expression. A CASE yields exactly one key per row, so the
// categories partition a bucket by construction. A marker can only misfile a
// row into another category; it can never hide one, because "other" is the ELSE.
// Accepted limit: path or response-body text holding a marker can misfile a
// row. A value of only whitespace is "none" for every Unicode space, because
// the SQL trim set is derived from unicode.IsSpace, the same predicate
// strings.TrimSpace (the displayed Reason) uses.
//
// Not indexable (a scan of one status's rows). These buckets hold the retry
// backlog, not the done rows. Cost scales with message bytes, since
// lower(last_error) is evaluated per marker: about 60 to 125 ms over 14,000
// deferred rows of short messages, about 3 s when every message is near 4 KiB
// (title sort).

// Reason category keys: the URL values of the reason parameter.
const (
	ReasonNone     = "none"
	ReasonWrite    = "write"
	ReasonThrottle = "throttle"
	ReasonNetwork  = "network"
	ReasonMiss     = "miss"
	ReasonOther    = "other"
)

// ReasonCategory is one selectable failure-reason category.
type ReasonCategory struct {
	Key   string
	Label string
}

// reasonDef is a category and its lower-case markers. Order is CASE order, so
// an earlier category wins a row that matches several. A marker holding "[" is
// a GLOB pattern; any other is a plain substring. "none" has no markers
// (an empty or blank last_error) and "other" is the ELSE.
type reasonDef struct {
	ReasonCategory
	markers []string
}

// httpStatusMarkers are the status shapes the providers print ("status 503",
// "status_code 502", "HTTP 500"). Three digits, never "status 5": an ffmpeg
// "exit status 5" must not read as a server error.
func httpStatusMarkers(codes ...string) []string {
	var out []string
	for _, p := range []string{"status ", "status_code ", "http "} {
		for _, c := range codes {
			out = append(out, p+c)
		}
	}
	return out
}

var reasonDefs = []reasonDef{
	{ReasonCategory{ReasonNone, "No reason recorded"}, nil},
	{ReasonCategory{ReasonWrite, "Write or file error"}, []string{
		"write item", "refusing to write", "permission denied", "no space left",
		"read-only file system", "nothing to save for"}},
	{ReasonCategory{ReasonThrottle, "Rate limited or refused"}, append([]string{
		"rate limited", "unauthorized", "forbidden", "token renewal", "throttled",
		"circuit open", "lane unavailable", "lane not ready",
		// petitlyrics' confirmed and latched outage: both wrap "no results found".
		"application id revoked"},
		httpStatusMarkers("429", "403")...)},
	{ReasonCategory{ReasonNetwork, "Server or network error"}, append([]string{
		"transport error", "connection refused", "connection reset", "dial tcp",
		"timeout", "timed out", "deadline exceeded", "unexpected eof", ": eof",
		"tls handshake", "no such host", "lane outage", "context canceled",
		"broken pipe", "goaway", "stream error", "network is unreachable"},
		httpStatusMarkers("408", "5[0-9][0-9]")...)},
	{ReasonCategory{ReasonMiss, "Not found or no lyrics"}, []string{
		"no results found", "no songs in response", "no lyrics", "does not match the requested track",
		"benign miss", "truncated or empty", "unrecognized subtitle_body", "no title or alternate",
		"no timings", "miss limit reached", "matcher rejected"}},
	{ReasonCategory{ReasonOther, "Other"}, nil},
}

// reasonOffered is the ONE place the categories per bucket are decided, in
// display order: only those the worker and queue can write there. Retrying holds
// benign misses, parked lanes (Defer, DeferRefused) and a word recheck's failed
// write; Errored holds hard failures (Fail), which can still carry a miss
// message: a dispatch where one lane missed and another failed in a shape no
// marker names is failed with both texts. Given up is not listed: RetireMiss
// is its only writer, so every row there reads "miss limit reached" and a filter
// would be vacuous. A category a bucket does not list is never offered or
// honored.
var reasonOffered = map[Bucket][]string{
	BucketDeferred: {ReasonNone, ReasonMiss, ReasonWrite, ReasonThrottle, ReasonNetwork, ReasonOther},
	BucketFailed:   {ReasonNone, ReasonMiss, ReasonWrite, ReasonThrottle, ReasonNetwork, ReasonOther},
}

// reasonCaseSQL is the CASE expression yielding a row's category key. Built once
// from reasonDefs; every literal is a constant marker (checked at init), never
// caller input.
var reasonCaseSQL = buildReasonCase()

// spaceCharSQL is a SQL expression yielding exactly the code points
// unicode.IsSpace accepts, as char(...) of integer constants, so the "none"
// trim agrees with strings.TrimSpace in normalizedReason. Built once at init
// from the rune table; nothing from a request reaches it.
var spaceCharSQL = buildSpaceCharSQL()

func buildSpaceCharSQL() string {
	var cps []string
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.IsSpace(r) {
			cps = append(cps, strconv.Itoa(int(r)))
		}
	}
	return "char(" + strings.Join(cps, ", ") + ")"
}

func buildReasonCase() string {
	var b strings.Builder
	b.WriteString("CASE")
	for _, d := range reasonDefs {
		switch {
		case d.Key == ReasonNone:
			// Blank counts as none, as normalizedReason does for the display.
			b.WriteString(` WHEN TRIM(COALESCE(last_error, ''), ` + spaceCharSQL + `) = '' THEN '` + d.Key + `'`)
		case len(d.markers) > 0:
			conds := make([]string, 0, len(d.markers))
			for _, m := range d.markers {
				if m != strings.ToLower(m) || strings.ContainsAny(m, "'%\\") {
					panic("reports: bad reason marker " + m)
				}
				if strings.Contains(m, "[") {
					conds = append(conds, "lower(last_error) GLOB '*"+m+"*'")
					continue
				}
				conds = append(conds, "instr(lower(last_error), '"+m+"') > 0")
			}
			b.WriteString(" WHEN " + strings.Join(conds, " OR ") + " THEN '" + d.Key + "'")
		}
	}
	b.WriteString(" ELSE '" + ReasonOther + "' END")
	return b.String()
}

// ValidReason reports whether key is a reason category key.
func ValidReason(key string) bool {
	for _, d := range reasonDefs {
		if d.Key == key {
			return true
		}
	}
	return false
}

// ReasonCategories returns the categories b offers, in display order (nil for a
// bucket with no reason filter).
func ReasonCategories(b Bucket) []ReasonCategory {
	var out []ReasonCategory
	for _, key := range reasonOffered[b] {
		for _, d := range reasonDefs {
			if d.Key == key {
				out = append(out, d.ReasonCategory)
			}
		}
	}
	return out
}

// HasReason reports whether bucket b offers category key.
func HasReason(b Bucket, key string) bool { return slices.Contains(reasonOffered[b], key) }
