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
	for _, m := range libOptionRE.FindAllStringSubmatch(body, -1) {
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
