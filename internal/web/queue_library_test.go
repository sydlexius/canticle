package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/reports"
)

// seedLibraryPage creates libraries "Lib One" (id 1) and "Lib Two" (id 2) and
// done rows linked as: 001 -> one, 002 -> two, 003 -> one and two, 004 -> one
// through TWO files (a join would repeat it), 005 -> no library.
func seedLibraryPage(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	for i, name := range []string{"Lib One", "Lib Two"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO libraries (path, name) VALUES (?, ?)`, fmt.Sprintf("/lib/%d", i+1), name); err != nil {
			t.Fatal(err)
		}
	}
	links := [][]int{{1}, {2}, {1, 2}, {1, 1}, nil}
	file := 0
	for i, libs := range links {
		title := fmt.Sprintf("Done %03d", i+1)
		res, err := db.ExecContext(ctx,
			`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, outcome_type, updated_at)
             VALUES ('Lib Artist', ?, 'lib artist', ?, 'Album', 'done', 'unsynced', ?)`,
			title, strings.ToLower(title), fmt.Sprintf("2026-01-0%dT00:00:00Z", i+1))
		if err != nil {
			t.Fatal(err)
		}
		wq, _ := res.LastInsertId()
		for _, lib := range libs {
			file++
			sr, err := db.ExecContext(ctx, `INSERT INTO scan_results (library_id, file_path) VALUES (?, ?)`, lib, fmt.Sprintf("/f/%d.flac", file))
			if err != nil {
				t.Fatal(err)
			}
			id, _ := sr.LastInsertId()
			if _, err := db.ExecContext(ctx, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wq, id); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestQueueLibraryFilterRowsAndCombine(t *testing.T) {
	db := openReportsTestDB(t)
	seedLibraryPage(t, db)
	mux := newReportsUIServer(t, db)
	for target, want := range map[string]string{
		"/queue/settled":                              "[Done 005 Done 004 Done 003 Done 002 Done 001]",
		"/queue/settled?library=1":                    "[Done 004 Done 003 Done 001]",
		"/queue/settled?library=2":                    "[Done 003 Done 002]",
		"/queue/settled?library=1&sort=title&dir=asc": "[Done 001 Done 003 Done 004]",
		"/queue/settled?library=1&q=done+003":         "[Done 003]",
		"/queue/settled?library=2&q=done+001":         "[]",
		// Unknown, nonexistent and malformed ids are ignored, not errors.
		"/queue/settled?library=99":     "[Done 005 Done 004 Done 003 Done 002 Done 001]",
		"/queue/settled?library=0":      "[Done 005 Done 004 Done 003 Done 002 Done 001]",
		"/queue/settled?library=-1":     "[Done 005 Done 004 Done 003 Done 002 Done 001]",
		"/queue/settled?library=x%27--": "[Done 005 Done 004 Done 003 Done 002 Done 001]",
		"/queue/settled?library=":       "[Done 005 Done 004 Done 003 Done 002 Done 001]",
	} {
		if got := fmt.Sprint(orderOf(t, mux, target)); got != want {
			t.Errorf("GET %s = %s, want %s", target, got, want)
		}
	}
	if rec := getQueue(t, mux, "/queue/settled?library=1&library=2", false); rec.Code != http.StatusBadRequest {
		t.Errorf("repeated library = %d, want 400", rec.Code)
	}
}

var (
	libSelectRE = regexp.MustCompile(`<select class="mx-queue-filter" id="mx-queue-library" name="library" autocomplete="off" data-queue-autosubmit>`)
	libOptionRE = regexp.MustCompile(`<option value="(\d*)"( selected)?>([^<]*)</option>`)
)

func TestQueueLibrarySelectAndLinksCarryState(t *testing.T) {
	db := openReportsTestDB(t)
	seedLibraryPage(t, db)
	mux := newReportsUIServer(t, db)
	body := getQueue(t, mux, "/queue/settled?library=2&q=lib&sort=title&dir=asc", false).Body.String()
	if !libSelectRE.MatchString(body) || !strings.Contains(body, `<label class="mx-queue-search-label" for="mx-queue-library">Library</label>`) {
		t.Fatal("Library select or its label is not rendered inside the search form")
	}
	var sel []string
	libSel := body[strings.Index(body, `id="mx-queue-library"`):]
	libSel = libSel[:strings.Index(libSel, "</select>")]
	for _, m := range libOptionRE.FindAllStringSubmatch(libSel, -1) {
		sel = append(sel, m[1]+":"+m[3]+":"+m[2])
	}
	if got, want := fmt.Sprint(sel), "[:All libraries: 1:Lib One: 2:Lib Two: selected]"; got != want {
		t.Errorf("options = %s, want %s", got, want)
	}
	// Sort headers, the clear-search link and the chips keep the library.
	for _, re := range []*regexp.Regexp{regexp.MustCompile(`class="mx-sort-link" href="([^"]*)"`), clearRE, regexp.MustCompile(`class="mx-queue-chip[^"]*" href="([^"]*)"`)} {
		ms := re.FindAllStringSubmatch(body, -1)
		if len(ms) == 0 {
			t.Fatalf("no links matched %s", re)
		}
		for _, m := range ms {
			if h := html2(m[1]); !strings.Contains(h, "library=2") {
				t.Errorf("link %q drops the library", h)
			}
		}
	}
	// An ignored library id is not carried: no option selected, no link names it
	// (an unknown id, and values that never parse as one).
	for _, bad := range []string{"99", "abc", "-5", "0"} {
		body = getQueue(t, mux, "/queue/settled?q=lib&library="+bad, false).Body.String()
		if strings.Contains(body, "library="+bad) || strings.Contains(body, " selected>") {
			t.Errorf("library=%s was carried into the page", bad)
		}
	}
	// The preview back link returns to the same library view.
	st, err := parseQueueViewState(url.Values{"library": {"2"}}, reports.BucketSettled)
	if err != nil {
		t.Fatal(err)
	}
	href := queuePreviewHref(reports.BucketRow{ID: 7, Previewable: true}, reports.BucketSettled, st)
	if !strings.Contains(href, "library=2") {
		t.Errorf("preview link %q drops the library", href)
	}
	// Without a configured library the control is not rendered.
	empty := newReportsUIServer(t, openReportsTestDB(t))
	if libSelectRE.MatchString(getQueue(t, empty, "/queue/settled", false).Body.String()) {
		t.Error("select rendered with no libraries configured")
	}
}

// A paged walk under the Library filter (rows linked through two files each, so
// a join would repeat them) lists every matching row exactly once.
func TestQueueLibraryFilterPagingWalk(t *testing.T) {
	db := openReportsTestDB(t)
	ctx := context.Background()
	for _, p := range []string{"/a", "/b"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO libraries (path, name) VALUES (?, ?)`, p, "Lib "+p); err != nil {
			t.Fatal(err)
		}
	}
	const n = 130
	want := map[string]bool{}
	for i := 1; i <= n; i++ {
		title := fmt.Sprintf("Done %03d", i)
		res, err := db.ExecContext(ctx,
			`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, outcome_type, updated_at)
             VALUES ('Walk Artist', ?, 'walk artist', ?, 'Album', 'done', 'unsynced', ?)`,
			title, strings.ToLower(title), fmt.Sprintf("2026-02-01T%02d:%02d:00Z", i/60, i%60))
		if err != nil {
			t.Fatal(err)
		}
		wq, _ := res.LastInsertId()
		lib := 1 + i%2 // odd rows library 2, even rows library 1
		if lib == 1 {
			want[title] = true
		}
		for f := 0; f < 2; f++ {
			sr, err := db.ExecContext(ctx, `INSERT INTO scan_results (library_id, file_path) VALUES (?, ?)`, lib, fmt.Sprintf("/f/%d/%d", i, f))
			if err != nil {
				t.Fatal(err)
			}
			id, _ := sr.LastInsertId()
			if _, err := db.ExecContext(ctx, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wq, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	mux := newReportsUIServer(t, db)
	seen := map[string]int{}
	target := "/queue/settled?library=1&sort=title&q=walk"
	for pages := 0; target != "" && pages < 10; pages++ {
		body := getQueue(t, mux, target, false).Body.String()
		for _, title := range titlesIn(body) {
			seen[title]++
		}
		target = ""
		if m := moreHrefRE.FindStringSubmatch(body); m != nil {
			target = html2(m[1])
			if !strings.Contains(target, "library=1") {
				t.Fatalf("pager link %q drops the library", target)
			}
		}
	}
	if len(seen) != len(want) {
		t.Errorf("walk returned %d distinct rows, want %d", len(seen), len(want))
	}
	for title, c := range seen {
		if !want[title] || c != 1 {
			t.Errorf("row %q seen %d times (expected=%v), want exactly once", title, c, want[title])
		}
	}
}

// The preview back link returns to the same Library view, and an id that is not
// a positive integer is not reflected into it.
func TestPreviewBackLinkCarriesLibrary(t *testing.T) {
	f := newPreviewFixture(t)
	id := itoa(f.row(t, f.writeFile(t, f.root, "song.flac")))
	f.put(t, "song.lrc", pageLRC)
	for query, href := range map[string]string{
		"?from=settled&library=2":   "/queue/settled?library=2",
		"?from=settled&library=abc": "/queue/settled",
		"?from=settled&library=0":   "/queue/settled",
	} {
		body := getPath(t, f.mux, "/preview/"+id+query).Body.String()
		if want := `id="mx-preview-back" href="` + href + `">`; !strings.Contains(body, want) {
			t.Errorf("query %q: back link missing %s", query, want)
		}
	}
}

// A Library filter that matches no row says so, even with no search and no chip.
func TestQueueLibraryFilterEmptyState(t *testing.T) {
	db := openReportsTestDB(t)
	seedLibraryPage(t, db)
	mux := newReportsUIServer(t, db)
	body := getQueue(t, mux, "/queue/finished?library=1", false).Body.String()
	if !strings.Contains(body, "No tracks match the selected filters.") {
		t.Error("an empty library-filtered bucket does not say the filters matched nothing")
	}
}

// setLanes stamps provider_lane on the seeded done rows: 001 musixmatch, 002
// petitlyrics, 003 innertube, 004 musixmatch, 005 unstamped; plus a row whose
// lane differs only in case (006) and a detector-settled row (007), neither of
// which any lane option may match. 001 and 003 are the only rows with a library
// besides 002/004.
func setLanes(t *testing.T, db *sql.DB) {
	t.Helper()
	seedSortRows(t, db, "done", [][3]string{
		{"Done 006", "2026-01-06T00:00:00Z", "2026-01-06T00:00:00Z"},
		{"Done 007", "2026-01-07T00:00:00Z", "2026-01-07T00:00:00Z"},
	})
	for title, lane := range map[string]string{"Done 001": "musixmatch", "Done 002": "petitlyrics", "Done 003": "innertube",
		"Done 004": "musixmatch", "Done 006": "Musixmatch", "Done 007": "detector"} {
		if _, err := db.ExecContext(context.Background(), `UPDATE work_queue SET provider_lane = ? WHERE title = ?`, lane, title); err != nil {
			t.Fatal(err)
		}
	}
}

// The Source (provider lane) filter: exact rows, combination with library,
// search and sort, ignored when not one of the three lanes (a differently cased
// or detector lane included), offered and applied on every bucket.
func TestQueueLaneFilter(t *testing.T) {
	db := openReportsTestDB(t)
	seedLibraryPage(t, db)
	setLanes(t, db)
	// A retired or queued row keeps the lane that last served it.
	seedSortRows(t, db, "pending", [][3]string{{"Pend 001", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"}, {"Pend 002", "2026-01-02T00:00:00Z", "2026-01-02T00:00:00Z"}})
	if _, err := db.ExecContext(context.Background(), `UPDATE work_queue SET provider_lane = 'musixmatch' WHERE title = 'Pend 001'`); err != nil {
		t.Fatal(err)
	}
	mux := newReportsUIServer(t, db)
	// Stamping a lane bumps updated_at (a trigger), so the unfiltered order is read, not typed.
	all := fmt.Sprint(orderOf(t, mux, "/queue/settled"))
	if !strings.Contains(all, "Done 006") || !strings.Contains(all, "Done 007") {
		t.Fatalf("unfiltered settled list = %s, want all seven rows", all)
	}
	pendAll := fmt.Sprint(orderOf(t, mux, "/queue/pending"))
	for target, want := range map[string]string{
		"/queue/settled?lane=musixmatch":                               "[Done 004 Done 001]",
		"/queue/settled?lane=innertube":                                "[Done 003]",
		"/queue/settled?lane=musixmatch&library=2":                     "[]",
		"/queue/settled?lane=musixmatch&library=1&sort=title&dir=desc": "[Done 004 Done 001]",
		"/queue/settled?lane=musixmatch&q=done+004":                    "[Done 004]",
		"/queue/settled?lane=bogus":                                    all,
		"/queue/settled?lane=":                                         all,
		"/queue/settled?lane=musixmatch%27--":                          all,
		// Not one of the three lanes: ignored, never matched case-insensitively.
		"/queue/settled?lane=Musixmatch": all,
		"/queue/settled?lane=detector":   all,
		"/queue/pending?lane=musixmatch": "[Pend 001]",
		"/queue/pending?lane=bogus":      pendAll,
	} {
		if got := fmt.Sprint(orderOf(t, mux, target)); got != want {
			t.Errorf("GET %s = %s, want %s", target, got, want)
		}
	}
	for target, want := range map[string]int{
		"/queue/settled?lane=musixmatch&lane=innertube": http.StatusBadRequest,
		"/queue/pending?lane=musixmatch&lane=innertube": http.StatusBadRequest,
	} {
		if rec := getQueue(t, mux, target, false); rec.Code != want {
			t.Errorf("GET %s = %d, want %d", target, rec.Code, want)
		}
	}
}

var laneOptionRE = regexp.MustCompile(`<option value="([a-z]*)"( selected)?>([^<]*)</option>`)

func TestQueueLaneSelectAndLinksCarryState(t *testing.T) {
	db := openReportsTestDB(t)
	seedLibraryPage(t, db)
	setLanes(t, db)
	mux := newReportsUIServer(t, db)
	body := getQueue(t, mux, "/queue/settled?lane=innertube&q=done&sort=title&dir=asc", false).Body.String()
	if !strings.Contains(body, `<label class="mx-queue-search-label" for="mx-queue-lane">Source</label>`) ||
		!strings.Contains(body, `<select class="mx-queue-filter" id="mx-queue-lane" name="lane" autocomplete="off" data-queue-autosubmit>`) {
		t.Fatal("Source select, its label or its auto-submit marker is not rendered")
	}
	laneSel := body[strings.Index(body, `id="mx-queue-lane"`):]
	laneSel = laneSel[:strings.Index(laneSel, "</select>")]
	var opts []string
	for _, m := range laneOptionRE.FindAllStringSubmatch(laneSel, -1) {
		opts = append(opts, m[1]+":"+m[3]+":"+m[2])
	}
	// Labeled by laneLabel, as the dashboard and reports label the lanes (lanes
	// without a case render as their name).
	if got, want := fmt.Sprint(opts), "[:All sources: musixmatch:musixmatch: petitlyrics:petitlyrics: innertube:YouTube Music: selected]"; got != want {
		t.Errorf("options = %s, want %s", got, want)
	}
	for _, re := range []*regexp.Regexp{regexp.MustCompile(`class="mx-sort-link" href="([^"]*)"`), clearRE, regexp.MustCompile(`class="mx-queue-chip[^"]*" href="([^"]*)"`)} {
		ms := re.FindAllStringSubmatch(body, -1)
		if len(ms) == 0 {
			t.Fatalf("no links matched %s", re)
		}
		for _, m := range ms {
			if h := html2(m[1]); !strings.Contains(h, "lane=innertube") {
				t.Errorf("link %q drops the lane", h)
			}
		}
	}
	// Every bucket offers the select.
	for _, b := range []string{"pending", "processing", "deferred", "failed", "finished", "settled", "unavailable"} {
		if !strings.Contains(getQueue(t, mux, "/queue/"+b, false).Body.String(), `id="mx-queue-lane"`) {
			t.Errorf("bucket %s does not offer the Source select", b)
		}
	}
	// An ignored lane value is never reflected: no option selected, no link, no
	// preview href carries it.
	for _, bad := range []string{"bogus", "Musixmatch", "detector", "x%22%3E%3Cscript%3E"} {
		for _, b := range []string{"settled", "pending"} {
			body = getQueue(t, mux, "/queue/"+b+"?q=done&lane="+bad, false).Body.String()
			if strings.Contains(body, "lane="+bad) || strings.Contains(body, " selected>") || strings.Contains(body, "<script>") {
				t.Errorf("bucket %s: lane=%s was carried into the page", b, bad)
			}
		}
	}
	st, err := parseQueueViewState(url.Values{"lane": {"bogus"}, "library": {"2"}}, reports.BucketSettled)
	if err != nil {
		t.Fatal(err)
	}
	if h := queuePreviewHref(reports.BucketRow{ID: 7, Previewable: true}, reports.BucketSettled, st); strings.Contains(h, "lane") {
		t.Errorf("preview link %q carries an invalid lane", h)
	}
	st, _ = parseQueueViewState(url.Values{"lane": {"innertube"}}, reports.BucketSettled)
	if h := queuePreviewHref(reports.BucketRow{ID: 7, Previewable: true}, reports.BucketSettled, st); !strings.Contains(h, "lane=innertube") {
		t.Errorf("preview link %q drops the lane", h)
	}
}

// The preview back link returns to the same Source view, and an invalid value
// is not reflected into it.
func TestPreviewBackLinkCarriesBothFilters(t *testing.T) {
	f := newPreviewFixture(t)
	id := itoa(f.row(t, f.writeFile(t, f.root, "song.flac")))
	f.put(t, "song.lrc", pageLRC)
	for query, href := range map[string]string{
		"?from=settled&lane=innertube&library=2": "/queue/settled?lane=innertube&amp;library=2",
		"?from=settled&lane=innertube":           "/queue/settled?lane=innertube",
		"?from=pending&lane=musixmatch":          "/queue/pending?lane=musixmatch",
		"?from=settled&lane=bogus":               "/queue/settled",
		"?from=settled&lane=Musixmatch":          "/queue/settled",
	} {
		body := getPath(t, f.mux, "/preview/"+id+query).Body.String()
		if want := `id="mx-preview-back" href="` + href + `">`; !strings.Contains(body, want) {
			t.Errorf("query %q: back link missing %s", query, want)
		}
	}
}

// A Source filter that matches no row says so, with no search and no chip.
func TestQueueLaneFilterEmptyState(t *testing.T) {
	db := openReportsTestDB(t)
	seedLibraryPage(t, db)
	mux := newReportsUIServer(t, db)
	for _, target := range []string{"/queue/finished?lane=innertube", "/queue/settled?lane=musixmatch"} {
		if !strings.Contains(getQueue(t, mux, target, false).Body.String(), "No tracks match the selected filters.") {
			t.Errorf("GET %s does not say the filters matched nothing", target)
		}
	}
}

// seedReasons adds rows with a recorded failure: failed 001 write, 002 network,
// 003 no reason, 004 write; deferred 005 miss, 006 network (lane musixmatch),
// 008 write (lane petitlyrics); unavailable 007 miss. Titles are invented.
func seedReasons(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, r := range []struct{ title, status, lastErr string }{
		{"Other 008", "deferred", "worker: write item 8 output: disk full"},
		{"Other 001", "failed", "worker: write item 1 output x: no space left on device"},
		{"Other 002", "failed", "lane a: transport error: connection refused"},
		{"Other 003", "failed", ""},
		{"Other 004", "failed", "worker: write item 4 output y: permission denied"},
		{"Other 005", "deferred", "lane a: musixmatch: no results found"},
		{"Other 006", "deferred", "lane a: transport error"},
		{"Other 007", "unavailable", "miss limit reached"},
	} {
		if _, err := db.ExecContext(context.Background(),
			`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, last_error)
             VALUES ('Invented Artist', ?, 'invented artist', ?, 'Invented Album', ?, ?)`,
			r.title, strings.ToLower(r.title), r.status, r.lastErr); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE work_queue SET provider_lane =
         CASE title WHEN 'Other 006' THEN 'musixmatch' ELSE 'petitlyrics' END WHERE title IN ('Other 006', 'Other 008')`); err != nil {
		t.Fatal(err)
	}
}

// The reason filter: exact rows per category, combination with search and sort,
// ignored when unknown or not offered by the bucket, repeated value rejected.
func TestQueueReasonFilter(t *testing.T) {
	db := openReportsTestDB(t)
	seedReasons(t, db)
	mux := newReportsUIServer(t, db)
	failedAll := fmt.Sprint(orderOf(t, mux, "/queue/failed"))
	pendAll := fmt.Sprint(orderOf(t, mux, "/queue/pending"))
	for target, want := range map[string]string{
		"/queue/failed?reason=write":                         "[Other 004 Other 001]",
		"/queue/failed?reason=write&sort=title&dir=asc":      "[Other 001 Other 004]",
		"/queue/failed?reason=write&q=other+004":             "[Other 004]",
		"/queue/failed?reason=network":                       "[Other 002]",
		"/queue/failed?reason=none":                          "[Other 003]",
		"/queue/deferred?reason=miss":                        "[Other 005]",
		"/queue/deferred?reason=write":                       "[Other 008]",
		"/queue/unavailable?reason=none":                     "[Other 007]", // Given up offers no reason filter
		"/queue/failed?reason=bogus":                         failedAll,
		"/queue/failed?reason=":                              failedAll,
		"/queue/failed?reason=write%27--":                    failedAll,
		"/queue/failed?reason=WRITE":                         failedAll,
		"/queue/failed?reason=miss":                          failedAll, // a category this bucket does not offer
		"/queue/pending?reason=write":                        pendAll,
		"/queue/settled?reason=none":                         fmt.Sprint(orderOf(t, mux, "/queue/settled")),
		"/queue/deferred?reason=network&lane=musixmatch":     "[Other 006]",
		"/queue/deferred?reason=network&lane=petitlyrics":    "[]",
		"/queue/failed?reason=write&reason=network&bad=skip": "400",
	} {
		if want == "400" {
			if rec := getQueue(t, mux, target, false); rec.Code != http.StatusBadRequest {
				t.Errorf("GET %s = %d, want 400", target, rec.Code)
			}
			continue
		}
		if got := fmt.Sprint(orderOf(t, mux, target)); got != want {
			t.Errorf("GET %s = %s, want %s", target, got, want)
		}
	}
}

var reasonOptionRE = regexp.MustCompile(`<option value="([a-z]*)"( selected)?>([^<]*)</option>`)

// The select is offered only on Retrying and Errored, lists exactly what the
// bucket offers, marks the active value, and every link carries it.
func TestQueueReasonSelectAndLinksCarryState(t *testing.T) {
	db := openReportsTestDB(t)
	seedReasons(t, db)
	mux := newReportsUIServer(t, db)
	body := getQueue(t, mux, "/queue/failed?reason=write&q=other&sort=title&dir=asc", false).Body.String()
	if !strings.Contains(body, `<label class="mx-queue-search-label" for="mx-queue-reason">Reason</label>`) ||
		!strings.Contains(body, `<select class="mx-queue-filter" id="mx-queue-reason" name="reason" autocomplete="off" data-queue-autosubmit>`) {
		t.Fatal("Reason select, its label or its auto-submit marker is not rendered")
	}
	sel := body[strings.Index(body, `id="mx-queue-reason"`):]
	sel = sel[:strings.Index(sel, "</select>")]
	var opts []string
	for _, m := range reasonOptionRE.FindAllStringSubmatch(sel, -1) {
		opts = append(opts, m[1]+":"+m[3]+":"+m[2])
	}
	want := "[:All reasons: none:No reason recorded: write:Write or file error: selected throttle:Rate limited or refused: network:Server or network error: other:Other:]"
	if got := fmt.Sprint(opts); got != want {
		t.Errorf("options = %s, want %s", got, want)
	}
	for _, re := range []*regexp.Regexp{regexp.MustCompile(`class="mx-sort-link" href="([^"]*)"`), clearRE} {
		ms := re.FindAllStringSubmatch(body, -1)
		if len(ms) == 0 {
			t.Fatalf("no links matched %s", re)
		}
		for _, m := range ms {
			if h := html2(m[1]); !strings.Contains(h, "reason=write") {
				t.Errorf("link %q drops the reason", h)
			}
		}
	}
	for _, b := range []string{"pending", "processing", "finished", "settled", "unavailable"} {
		if strings.Contains(getQueue(t, mux, "/queue/"+b+"?reason=miss", false).Body.String(), "mx-queue-reason") {
			t.Errorf("bucket %s offers the Reason select", b)
		}
	}
	for _, b := range []string{"deferred", "failed"} {
		if !strings.Contains(getQueue(t, mux, "/queue/"+b, false).Body.String(), `id="mx-queue-reason"`) {
			t.Errorf("bucket %s does not offer the Reason select", b)
		}
	}
	// A value the bucket does not offer is dropped before any link or preview href.
	st, err := parseQueueViewState(url.Values{"reason": {"miss"}}, reports.BucketUnavailable)
	if err != nil || st.Reason != "" {
		t.Errorf("reason=miss on unavailable parsed to %q (err %v), want ignored", st.Reason, err)
	}
	st, _ = parseQueueViewState(url.Values{"reason": {"miss"}, "library": {"2"}}, reports.BucketDeferred)
	if h := queuePreviewHref(reports.BucketRow{ID: 7, Previewable: true}, reports.BucketDeferred, st); !strings.Contains(h, "reason=miss") {
		t.Errorf("preview link %q drops the reason", h)
	}
}

// A reason filter that matches nothing says so, with no search and no chip.
func TestQueueReasonFilterEmptyState(t *testing.T) {
	db := openReportsTestDB(t)
	seedReasons(t, db)
	mux := newReportsUIServer(t, db)
	if !strings.Contains(getQueue(t, mux, "/queue/deferred?reason=none", false).Body.String(), "No tracks match the selected filters.") {
		t.Error("an empty reason filter does not say the filters matched nothing")
	}
}
