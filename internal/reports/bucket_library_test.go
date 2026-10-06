package reports

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/petitlyrics"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/tablesort"
)

// seedLibraryRows opens a migrated temp-file SQLite holding libraries "North
// Shelf" (id 1) and "South Shelf" (id 2) and done rows linked as: only-north ->
// 1, only-south -> 2, both -> 1 and 2, north-twice -> 1 through TWO files (a
// join would repeat it), unlinked -> no library.
func seedLibraryRows(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	for i, name := range []string{"North Shelf", "South Shelf"} {
		if _, err := d.ExecContext(ctx, `INSERT INTO libraries (path, name) VALUES (?, ?)`, fmt.Sprintf("/shelf/%d", i+1), name); err != nil {
			t.Fatal(err)
		}
	}
	file := 0
	for _, row := range []struct {
		title string
		libs  []int
	}{{"only-north", []int{1}}, {"only-south", []int{2}}, {"both", []int{1, 2}}, {"north-twice", []int{1, 1}}, {"unlinked", nil}} {
		res, err := d.ExecContext(ctx,
			`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, outcome_type)
             VALUES ('Shelf Band', ?, 'shelf band', ?, 'Album', 'done', 'unsynced')`, row.title, row.title)
		if err != nil {
			t.Fatal(err)
		}
		wq, _ := res.LastInsertId()
		for _, lib := range row.libs {
			file++
			sr, err := d.ExecContext(ctx, `INSERT INTO scan_results (library_id, file_path) VALUES (?, ?)`, lib, fmt.Sprintf("/shelf/%d.flac", file))
			if err != nil {
				t.Fatal(err)
			}
			id, _ := sr.LastInsertId()
			if _, err := d.ExecContext(ctx, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wq, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	return d
}

// filteredTitles lists the titles f returns on bucket b, sorted and joined. A
// row returned twice shows up twice.
func filteredTitles(t *testing.T, d *sql.DB, b Bucket, f BucketFilter) string {
	t.Helper()
	rows, err := New(d).ListBucketFiltered(context.Background(), b, f, BucketSpec(b).Default, tablesort.Cursor{}, MaxBucketLimit)
	if err != nil {
		t.Fatalf("ListBucketFiltered(%s, %+v): %v", b, f, err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Title)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// The Library filter keeps exactly the rows linked to that library: a row in
// two libraries matches both, a row linked through two files of one library is
// listed once, and a row with no scan link matches no library.
func TestListBucketFilteredByLibrary(t *testing.T) {
	d := seedLibraryRows(t)
	const all = "both,north-twice,only-north,only-south,unlinked"
	for _, tc := range []struct {
		name string
		f    BucketFilter
		want string
	}{
		{"no filter", BucketFilter{}, all},
		{"library 1", BucketFilter{LibraryID: 1}, "both,north-twice,only-north"},
		{"library 2", BucketFilter{LibraryID: 2}, "both,only-south"},
		{"library with no rows", BucketFilter{LibraryID: 99}, ""},
		{"non-positive id applies no filter", BucketFilter{LibraryID: -1}, all},
		{"library and search", BucketFilter{LibraryID: 1, Query: "NORTH"}, "north-twice,only-north"},
		{"library and a search outside it", BucketFilter{LibraryID: 2, Query: "north"}, ""},
	} {
		if got := filteredTitles(t, d, BucketSettled, tc.f); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	// The filter is composed with the bucket, not a replacement for it.
	if got := filteredTitles(t, d, BucketPending, BucketFilter{LibraryID: 1}); got != "" {
		t.Errorf("pending bucket under library 1 = %q, want no rows", got)
	}
}

// Libraries feeds the Library select: every library, by name ignoring case and
// then by id; none configured is an empty list, and a database failure is an
// error rather than an empty list.
func TestLibrariesOrderAndFailure(t *testing.T) {
	d := seedLibraryRows(t)
	ctx := context.Background()
	for i, name := range []string{"attic", "North Shelf", "Zeta"} {
		if _, err := d.ExecContext(ctx, `INSERT INTO libraries (path, name) VALUES (?, ?)`, fmt.Sprintf("/extra/%d", i), name); err != nil {
			t.Fatal(err)
		}
	}
	libs, err := New(d).Libraries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range libs {
		got = append(got, fmt.Sprintf("%d:%s", l.ID, l.Name))
	}
	if want := "[3:attic 1:North Shelf 4:North Shelf 2:South Shelf 5:Zeta]"; fmt.Sprint(got) != want {
		t.Errorf("Libraries = %v, want %s", got, want)
	}

	if _, err := d.ExecContext(ctx, `DELETE FROM work_queue_scan_results; DELETE FROM scan_results; DELETE FROM libraries`); err != nil {
		t.Fatal(err)
	}
	if libs, err = New(d).Libraries(ctx); err != nil || len(libs) != 0 {
		t.Errorf("Libraries with none configured = %v, %v; want empty, nil", libs, err)
	}

	_ = d.Close()
	if _, err := New(d).Libraries(ctx); err == nil || !strings.Contains(err.Error(), "reports: list libraries") {
		t.Errorf("Libraries on a closed database = %v, want a wrapped list error", err)
	}
}

// The Library filter must probe the junction by work_queue_id alone. When the
// planner may also use scan_result_id as an index constraint it walks the
// library's whole IN list for every candidate row (quadratic: seconds at ten
// thousand files, then the write timeout). This pins the plan of the real query
// text on a database that was never ANALYZEd (as in production), for the
// default and a text sort on a done and a failed bucket.
func TestLibraryPredicateUsesPrefixProbe(t *testing.T) {
	d := seedLibraryRows(t)
	for _, bucket := range []Bucket{BucketSettled, BucketFailed} {
		for _, o := range []tablesort.Order{BucketSpec(bucket).Default, {Key: tablesort.KeyTitle}} {
			name := string(bucket) + "/" + o.Key
			q, args, err := bucketQuery(bucket, BucketFilter{LibraryID: 1}, o, tablesort.Cursor{}, 50)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := d.Query("EXPLAIN QUERY PLAN "+q, args...)
			if err != nil {
				t.Fatalf("%s: explain: %v", name, err)
			}
			var junction []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(detail, "work_queue_scan_results") {
					junction = append(junction, detail)
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			_ = rows.Close()
			if len(junction) == 0 {
				t.Fatalf("%s: the plan never searches the junction", name)
			}
			for _, detail := range junction {
				if !strings.Contains(detail, "(work_queue_id=?)") || strings.Contains(detail, "scan_result_id=?") {
					t.Errorf("%s: junction searched as %q, want the work_queue_id=? prefix probe only", name, detail)
				}
			}
		}
	}
}

// The Lane filter is an exact match on provider_lane for one of Lanes: a lane
// differing only in case, the detector lane and a row no lane served are never
// selectable, a value outside Lanes applies no filter (it is not bound as a
// lane that matches nothing), it composes with the Library filter, and a row
// outside done keeps its lane.
func TestListBucketFilteredByLane(t *testing.T) {
	d := seedLibraryRows(t)
	ctx := context.Background()
	for title, lane := range map[string]string{"only-north": "musixmatch", "only-south": "petitlyrics", "both": "innertube",
		"north-twice": "Musixmatch"} {
		if _, err := d.ExecContext(ctx, `UPDATE work_queue SET provider_lane = ? WHERE title = ?`, lane, title); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range [][3]string{{"detector-settled", "done", "detector"}, {"second-musixmatch", "done", "musixmatch"}, {"queued-musixmatch", "pending", "musixmatch"}} {
		if _, err := d.ExecContext(ctx,
			`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, outcome_type, provider_lane)
             VALUES ('Shelf Band', ?, 'shelf band', ?, 'Album', ?, 'unsynced', ?)`, row[0], row[0], row[1], row[2]); err != nil {
			t.Fatal(err)
		}
	}
	const all = "both,detector-settled,north-twice,only-north,only-south,second-musixmatch,unlinked"
	for _, tc := range []struct {
		name   string
		bucket Bucket
		f      BucketFilter
		want   string
	}{
		{"musixmatch", BucketSettled, BucketFilter{Lane: "musixmatch"}, "only-north,second-musixmatch"},
		{"petitlyrics", BucketSettled, BucketFilter{Lane: "petitlyrics"}, "only-south"},
		{"innertube", BucketSettled, BucketFilter{Lane: "innertube"}, "both"},
		{"case variant is not a lane", BucketSettled, BucketFilter{Lane: "Musixmatch"}, all},
		{"detector is not offered", BucketSettled, BucketFilter{Lane: "detector"}, all},
		{"unknown value", BucketSettled, BucketFilter{Lane: "x' OR 1=1 --"}, all},
		{"lane and library", BucketSettled, BucketFilter{Lane: "musixmatch", LibraryID: 1}, "only-north"},
		{"lane and a library without it", BucketSettled, BucketFilter{Lane: "musixmatch", LibraryID: 2}, ""},
		{"lane on a queued row", BucketPending, BucketFilter{Lane: "musixmatch"}, "queued-musixmatch"},
		{"another lane on the queued bucket", BucketPending, BucketFilter{Lane: "innertube"}, ""},
	} {
		if got := filteredTitles(t, d, tc.bucket, tc.f); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
	// The offered lanes are providers.Known, not a second list that could
	// drift from it when a provider is added.
	if got, want := fmt.Sprint(Lanes()), fmt.Sprint(providers.Known()); got != want {
		t.Errorf("Lanes() = %s, want providers.Known() %s", got, want)
	}
}

// reasonShapes are last_error values modeled on what the worker, providers and
// queue write (invented names), each with the category it must land in. The
// near misses (an ffmpeg exit status that looks like an HTTP status, a path whose
// name holds a marker word inside a write error) pin the ordering and the
// explicit status list. Every marker has a row carrying it and no other marker
// of its category, so dropping one moves that row to another category; the rows
// carrying markers of two categories pin the CASE order.
var reasonShapes = []struct{ err, want string }{
	{"", ReasonNone},
	{"  \t\n", ReasonNone},
	{"\u00a0", ReasonNone},               // only a no-break space: the display reads "no reason recorded"
	{"\u3000\t\u2003\u0085", ReasonNone}, // ideographic, tab, em and next-line spaces
	{`worker: write item 5 output /Share/Music/Timeout Band/Album/01. Song.lrc: no space left on device`, ReasonWrite},
	{`lyrics: refusing to write: output dir "/mnt/a" does not exist`, ReasonWrite},
	{`open /mnt/a/b.lrc.tmp: read-only file system`, ReasonWrite},
	{`nothing to save for "Some Band" - "Some Song"`, ReasonWrite},
	{`nothing to save`, ReasonWrite},
	{`worker: write item 5 output x: disk quota exceeded`, ReasonWrite},
	{`open /mnt/a/b.lrc: permission denied`, ReasonWrite},
	{`write /mnt/a/b.lrc: no space left on device`, ReasonWrite},
	{queue.UnresolvableGoneError, ReasonOther}, // a done row's text: no category here
	{"lane musixmatch: musixmatch: unauthorized: HTTP 401", ReasonThrottle},
	{"musixmatch: rate limited", ReasonThrottle},
	{"orchestrator: lane unavailable (circuit open)", ReasonThrottle},
	{"innertube: HTTP 429: too many requests", ReasonThrottle},
	{"innertube: forbidden", ReasonThrottle},
	{"musixmatch: token renewal required", ReasonThrottle},
	{"lane petitlyrics throttled", ReasonThrottle},
	{"orchestrator: circuit open", ReasonThrottle},
	{"orchestrator: lane unavailable", ReasonThrottle},
	{"orchestrator: lane not ready", ReasonThrottle},
	{"musixmatch API error: status 429, body: slow down", ReasonThrottle},
	{"musixmatch: unexpected matcher status_code 429", ReasonThrottle},
	{"musixmatch API error: status 403, body: access denied", ReasonThrottle},
	{"musixmatch API error: status 403, body: gateway timeout", ReasonThrottle},
	{"musixmatch: unexpected matcher status_code 403", ReasonThrottle},
	{"innertube: HTTP 403: blocked", ReasonThrottle},
	// Both petitlyrics outage sentinels wrap "no results found" (a miss marker).
	{petitlyrics.ErrOutageLatched.Error(), ReasonThrottle},
	{petitlyrics.ErrProviderUnavailable.Error(), ReasonThrottle},
	{"lane a: rate limited: context deadline exceeded", ReasonThrottle}, // throttle before network
	{"lane a: transport error", ReasonNetwork},
	{"dial x.example: connection refused", ReasonNetwork},
	{"read: connection reset by peer", ReasonNetwork},
	{"dial tcp 192.0.2.1:443: unknown", ReasonNetwork},
	{"net/http: timeout awaiting response headers", ReasonNetwork},
	{"read: operation timed out", ReasonNetwork},
	{"read body: unexpected EOF", ReasonNetwork},
	{"net/http: TLS handshake failure", ReasonNetwork},
	{"lookup x.example: no such host", ReasonNetwork},
	{"fetch: context canceled", ReasonNetwork},
	{"write tcp 192.0.2.1:443: write: broken pipe", ReasonNetwork},
	{"http2: server sent GOAWAY and closed the connection", ReasonNetwork},
	{"stream error: stream ID 3; INTERNAL_ERROR", ReasonNetwork},
	{"connect: network is unreachable", ReasonNetwork},
	{"musixmatch API error: status 408, body:", ReasonNetwork},
	{"musixmatch: unexpected matcher status_code 408", ReasonNetwork},
	{"innertube: HTTP 408: slow", ReasonNetwork},
	{"musixmatch API error: status 520, body: x", ReasonNetwork},
	{"petitlyrics: unexpected HTTP status 501", ReasonNetwork},
	{"musixmatch: unexpected matcher status_code 502", ReasonNetwork},
	{"innertube: HTTP 500: x", ReasonNetwork},
	{"petitlyrics: no results found: connection reset by peer", ReasonNetwork}, // network before miss
	{"lane musixmatch: musixmatch: transport error: proxyconnect tcp: connection refused", ReasonNetwork},
	{`Get "https://x.example/y": context deadline exceeded`, ReasonNetwork},
	{`petitlyrics: request: Post "https://x.example/y": EOF`, ReasonNetwork},
	{"musixmatch API error: status 503, body: upstream down", ReasonNetwork},
	{"petitlyrics: unexpected HTTP status 502", ReasonNetwork},
	{"orchestrator: lane outage", ReasonNetwork},
	{"lane musixmatch: musixmatch: no results found", ReasonMiss},
	{"petitlyrics: no songs in response: petitlyrics: no results found", ReasonMiss},
	{"musixmatch: no lyrics available", ReasonMiss},
	{"innertube: no lyrics tab for video", ReasonMiss},
	{"musixmatch: response does not match the requested track", ReasonMiss},
	{"petitlyrics: no songs in response", ReasonMiss},
	{"lane a: benign miss", ReasonMiss},
	{"petitlyrics: truncated or empty lyrics data", ReasonMiss},
	{"musixmatch: unrecognized subtitle_body shape", ReasonMiss},
	{"innertube: no title or alternate", ReasonMiss},
	{"petitlyrics: no timings in payload", ReasonMiss},
	{"miss limit reached", ReasonMiss},
	{musixmatch.ErrMatcherClientError.Error() + ": inner status_code 400", ReasonMiss},
	{queue.NoReasonRecorded + " is only a display label", ReasonOther},
	{"detector: ffmpeg: exit status 5", ReasonOther},
	{"detector: sample audio with ffmpeg: exit status 69", ReasonOther},
	{"some brand new failure nobody has seen", ReasonOther},
	{"verification: rejected lyrics for another recording", ReasonOther},
}

func openReasonDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func insertReasonRow(t *testing.T, d *sql.DB, status, title, lastErr string) {
	t.Helper()
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, last_error)
         VALUES ('Reason Band', ?, 'reason band', ?, 'Album', ?, ?)`, title, strings.ToLower(title), status, lastErr); err != nil {
		t.Fatal(err)
	}
}

func titlesOf(t *testing.T, d *sql.DB, b Bucket, f BucketFilter) []string {
	t.Helper()
	rows, err := New(d).ListBucketFiltered(context.Background(), b, f, BucketSpec(b).Default, tablesort.Cursor{}, MaxBucketLimit)
	if err != nil {
		t.Fatalf("ListBucketFiltered(%s, %+v): %v", b, f, err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Title)
	}
	sort.Strings(out)
	return out
}

// Every category returns exactly the rows whose shape belongs to it, and the
// categories together return every row of the bucket once (a partition).
func TestReasonCategoriesPartitionTheBucket(t *testing.T) {
	d := openReasonDB(t)
	want := map[string][]string{}
	for i, s := range reasonShapes {
		title := fmt.Sprintf("t%02d", i)
		insertReasonRow(t, d, "failed", title, s.err)
		want[s.want] = append(want[s.want], title)
	}
	insertReasonRow(t, d, "deferred", "elsewhere", "no results found") // another bucket never leaks in

	var union []string
	for _, def := range reasonDefs {
		got := titlesOf(t, d, BucketFailed, BucketFilter{Reason: def.Key})
		w := slices.Clone(want[def.Key])
		sort.Strings(w)
		if !slices.Equal(got, w) {
			t.Errorf("reason %s: got %v, want %v", def.Key, got, w)
		}
		union = append(union, got...)
	}
	all := titlesOf(t, d, BucketFailed, BucketFilter{})
	sort.Strings(union)
	if !slices.Equal(union, all) {
		t.Errorf("categories do not partition the bucket: union %v, bucket %v", union, all)
	}
}

// An unknown value applies no filter; every offered key is a real category.
func TestReasonUnknownValueAppliesNoFilter(t *testing.T) {
	d := openReasonDB(t)
	insertReasonRow(t, d, "failed", "a", "")
	insertReasonRow(t, d, "failed", "b", "worker: write item 1 output x: no space left")
	for _, v := range []string{"bogus", "NONE", "Write", " write", "1=1"} {
		if got := titlesOf(t, d, BucketFailed, BucketFilter{Reason: v}); len(got) != 2 {
			t.Errorf("reason %q listed %v, want both rows (no filter)", v, got)
		}
	}
	for b, keys := range reasonOffered {
		for _, k := range keys {
			if !ValidReason(k) || !HasReason(b, k) {
				t.Errorf("%s offers %q which is not a valid category", b, k)
			}
		}
	}
	if HasReason(BucketFinished, ReasonNone) || ReasonCategories(BucketSettled) != nil || !HasReason(BucketFailed, ReasonMiss) ||
		ReasonCategories(BucketUnavailable) != nil || ValidReason("gone") {
		t.Error("a bucket offers a reason it cannot hold, or Errored lacks miss")
	}
	if got := ReasonCategories(BucketDeferred); len(got) != 6 || got[2].Label != "Write or file error" {
		t.Errorf("deferred categories = %+v", got)
	}
}

// The reason filter combines with search and sort, and a keyset walk under all
// three returns every matching row exactly once.
func TestReasonFilterPagingWalkReturnsEachRowOnce(t *testing.T) {
	d := openReasonDB(t)
	var want []string
	for i := 0; i < 23; i++ {
		title := fmt.Sprintf("walk %02d", i)
		switch i % 3 {
		case 0:
			insertReasonRow(t, d, "deferred", title, "lane a: musixmatch: no results found")
			want = append(want, title)
		case 1:
			insertReasonRow(t, d, "deferred", title, "lane a: rate limited")
		default:
			insertReasonRow(t, d, "deferred", strings.Replace(title, "walk", "stray", 1), "lane a: musixmatch: no results found")
		}
	}
	spec := BucketSpec(BucketDeferred)
	order := tablesort.Order{Key: tablesort.KeyTitle}
	f := BucketFilter{Reason: ReasonMiss, Query: "walk"}
	var got []string
	var cur tablesort.Cursor
	for pages := 0; pages < 20; pages++ {
		rows, err := New(d).ListBucketFiltered(context.Background(), BucketDeferred, f, order, cur, 3)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			got = append(got, r.Title)
		}
		if len(rows) < 3 {
			break
		}
		last := rows[len(rows)-1]
		var ok bool
		cur, ok = spec.DecodeCursor(order, tablesort.Cursor{ID: last.ID, Val: last.SortVal}.Encode())
		if !ok {
			t.Fatal("cursor refused")
		}
	}
	sort.Strings(want)
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("walk returned %v, want each of %v once", got, want)
	}
}
