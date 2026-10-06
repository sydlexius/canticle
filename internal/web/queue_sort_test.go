package web

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/reports"
)

// seedSortRows inserts rows with the given (title, updated, next) so sort order
// is observable from the rendered page. Titles are "Pend NNN" so titlesIn finds them.
func seedSortRows(t *testing.T, db *sql.DB, status string, rows [][3]string) {
	t.Helper()
	for i, r := range rows {
		_, err := db.ExecContext(context.Background(),
			`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, updated_at, next_attempt_at)
             VALUES ('Invented Artist', ?, ?, ?, 'Invented Album', ?, ?, ?)`,
			r[0], status, strings.ToLower(r[0]), status, r[1], r[2])
		if err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
}

func orderOf(t *testing.T, mux http.Handler, target string) []string {
	t.Helper()
	rec := getQueue(t, mux, target, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", target, rec.Code)
	}
	return titlesIn(rec.Body.String())
}

func TestQueueDefaultOrderPerBucket(t *testing.T) {
	db := openReportsTestDB(t)
	// Title order 001..003 differs from both the updated and the next-attempt order.
	rows := [][3]string{
		{"Pend 001", "2026-01-02T00:00:00Z", "2026-03-01T00:00:00Z"},
		{"Pend 002", "2026-01-03T00:00:00Z", "2026-03-03T00:00:00Z"},
		{"Pend 003", "2026-01-01T00:00:00Z", "2026-03-02T00:00:00Z"},
	}
	for _, st := range []string{"pending", "deferred", "failed", "unavailable", "done"} {
		seedSortRows(t, db, st, rows)
	}
	if _, err := db.Exec(`UPDATE work_queue SET outcome_type='synced', sync_tier='word' WHERE status='done' AND title='Pend 002'`); err != nil {
		t.Fatal(err)
	}
	mux := newReportsUIServer(t, db)
	byUpdated := "[Pend 002 Pend 001 Pend 003]" // newest first
	byNext := "[Pend 001 Pend 003 Pend 002]"    // soonest first
	for bucket, want := range map[string]string{
		"pending": byNext, "deferred": byNext,
		"failed": byUpdated, "unavailable": byUpdated,
		"settled":  "[Pend 001 Pend 003]",
		"finished": "[Pend 002]",
	} {
		if got := fmt.Sprint(orderOf(t, mux, "/queue/"+bucket)); got != want {
			t.Errorf("%s default order = %s, want %s", bucket, got, want)
		}
	}
}

func TestQueueSortParamsOrderBothWaysAndFallBack(t *testing.T) {
	db := openReportsTestDB(t)
	seedSortRows(t, db, "failed", [][3]string{
		{"Pend 002", "2026-01-02T00:00:00Z", "2026-03-01T00:00:00Z"},
		{"Pend 001", "2026-01-03T00:00:00Z", "2026-03-01T00:00:00Z"},
		{"Pend 003", "2026-01-01T00:00:00Z", "2026-03-01T00:00:00Z"},
	})
	mux := newReportsUIServer(t, db)
	for target, want := range map[string]string{
		"/queue/failed?sort=title&dir=asc":   "[Pend 001 Pend 002 Pend 003]",
		"/queue/failed?sort=title&dir=desc":  "[Pend 003 Pend 002 Pend 001]",
		"/queue/failed?sort=title":           "[Pend 001 Pend 002 Pend 003]",
		"/queue/failed?sort=updated&dir=asc": "[Pend 003 Pend 002 Pend 001]",
		// Duplicate key (identical next_attempt_at): ties break on id, in the sort direction.
		"/queue/failed?sort=next_attempt&dir=asc":  "[Pend 002 Pend 001 Pend 003]",
		"/queue/failed?sort=next_attempt&dir=desc": "[Pend 003 Pend 001 Pend 002]",
		// Unknown or malicious values fall back to the bucket default (updated, newest first).
		"/queue/failed?sort=id%3Bdrop":                  "[Pend 001 Pend 002 Pend 003]",
		"/queue/failed?sort=title%3Bdrop%20table&dir=x": "[Pend 001 Pend 002 Pend 003]",
		"/queue/failed?dir=sideways":                    "[Pend 001 Pend 002 Pend 003]",
		"/queue/failed?sort=reason":                     "[Pend 001 Pend 002 Pend 003]",
	} {
		if got := fmt.Sprint(orderOf(t, mux, target)); got != want {
			t.Errorf("%s = %s, want %s", target, got, want)
		}
	}
}

func TestQueuePagingUnderEverySortShowsEachRowOnce(t *testing.T) {
	db := openReportsTestDB(t)
	total := queuePageSize*2 + 9
	for i := 1; i <= total; i++ {
		// Heavy duplication: three distinct values per column, .
		next := fmt.Sprintf("2026-03-0%dT00:00:00Z", i%3+1)
		updated := fmt.Sprintf("2026-01-0%dT00:00:00Z", i%3+1)
		if _, err := db.Exec(`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, updated_at, next_attempt_at, miss_count, attempts)
            VALUES (?, ?, ?, ?, ?, 'failed', ?, ?, ?, ?)`,
			fmt.Sprintf("Artist %d", i%4), fmt.Sprintf("Pend %03d", i), fmt.Sprintf("artist %d", i%4),
			fmt.Sprintf("pend %03d", i), fmt.Sprintf("Album %d", i%5), updated, next, i%3, i%2); err != nil {
			t.Fatal(err)
		}
	}
	mux := newReportsUIServer(t, db)
	next := regexp.MustCompile(`hx-get="(/queue/failed\?[^"]+)"`)
	for _, key := range []string{"artist", "album", "title", "next_attempt", "misses", "attempts", "updated"} {
		for _, dir := range []string{"asc", "desc"} {
			seen := map[string]int{}
			target := fmt.Sprintf("/queue/failed?sort=%s&dir=%s", key, dir)
			for page := 0; page < 10 && target != ""; page++ {
				rec := getQueue(t, mux, target, page > 0)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s: status %d", target, rec.Code)
				}
				for _, ti := range titlesIn(rec.Body.String()) {
					seen[ti]++
				}
				target = ""
				if m := next.FindStringSubmatch(rec.Body.String()); m != nil {
					target = html.UnescapeString(m[1])
					if !strings.Contains(target, "sort="+key) || !strings.Contains(target, "dir="+dir) {
						t.Fatalf("pager link lost the sort: %s", target)
					}
				}
			}
			if len(seen) != total {
				t.Errorf("sort=%s dir=%s: saw %d distinct rows, want %d", key, dir, len(seen), total)
			}
			for ti, n := range seen {
				if n != 1 {
					t.Errorf("sort=%s dir=%s: %s shown %d times", key, dir, ti, n)
				}
			}
		}
	}
}

// A row whose sort value is far past the old 600-rune cursor cap must still
// page: the pager's cursor round-trips through the decoder, so every row shows
// exactly once instead of "Show more" looping back to page 1.
func TestQueuePagingSurvivesLongSortValue(t *testing.T) {
	db := openReportsTestDB(t)
	const total = 60
	longAlbum := strings.Repeat("Invented Album \u00e9 ", 37) // 629 runes
	if n := len([]rune(longAlbum)); n < 620 {
		t.Fatalf("test album is %d runes, want >= 620", n)
	}
	for i := 1; i <= total; i++ {
		if _, err := db.Exec(`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status)
            VALUES ('Invented Artist', ?, 'invented artist', ?, ?, 'failed')`,
			fmt.Sprintf("Pend %03d", i), fmt.Sprintf("pend %03d", i), longAlbum); err != nil {
			t.Fatal(err)
		}
	}
	mux := newReportsUIServer(t, db)
	next := regexp.MustCompile(`hx-get="(/queue/failed\?[^"]+)"`)
	seen := map[string]int{}
	target := "/queue/failed?sort=album&dir=asc"
	for page := 0; page < 10 && target != ""; page++ {
		rec := getQueue(t, mux, target, page > 0)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", target, rec.Code)
		}
		for _, ti := range titlesIn(rec.Body.String()) {
			seen[ti]++
		}
		target = ""
		if m := next.FindStringSubmatch(rec.Body.String()); m != nil {
			target = html.UnescapeString(m[1])
		}
	}
	if len(seen) != total {
		t.Errorf("saw %d distinct rows, want %d", len(seen), total)
	}
	for ti, n := range seen {
		if n != 1 {
			t.Errorf("%s shown %d times", ti, n)
		}
	}
}

func TestQueueForgedCursorFallsBackToFirstPage(t *testing.T) {
	db := openReportsTestDB(t)
	seedQueueRows(t, db, "pending", "Pend", 3)
	mux := newReportsUIServer(t, db)
	for _, after := range []string{"abc", "-4", "99%3Ai5", "1%3Anope", "1%3At%27%3BDROP%20TABLE%20work_queue%3B--x%FF"} {
		rec := getQueue(t, mux, "/queue/pending?after="+after, false)
		if rec.Code != http.StatusOK || len(titlesIn(rec.Body.String())) != 3 {
			t.Errorf("after=%s: status %d rows %d, want the first page of 3", after, rec.Code, len(titlesIn(rec.Body.String())))
		}
	}
}

// TestQueueHeadersSortableAndAria pins that the queue headers are sort links with a single aria-sort on the active column, toggling direction and keeping the search.
func TestQueueHeadersSortableAndAria(t *testing.T) {
	db := openReportsTestDB(t)
	seedQueueRows(t, db, "pending", "Pend", 2)
	mux := newReportsUIServer(t, db)
	body := getQueue(t, mux, "/queue/pending?q=pend", false).Body.String()
	// Default (pending): Next attempt ascending, the only aria-sort on the page.
	if n := strings.Count(body, "aria-sort="); n != 1 || !strings.Contains(body, `aria-sort="ascending"`) {
		t.Fatalf("aria-sort count = %d, want exactly one ascending", n)
	}
	// Clicking the active column asks for the opposite direction and keeps the search.
	if !strings.Contains(body, `href="/queue/pending?dir=desc&amp;q=pend&amp;sort=next_attempt"`) {
		t.Error("active header does not toggle to desc with q kept")
	}
	// Another column starts in its natural direction.
	if !strings.Contains(body, `href="/queue/pending?dir=desc&amp;q=pend&amp;sort=misses"`) ||
		!strings.Contains(body, `href="/queue/pending?dir=asc&amp;q=pend&amp;sort=artist"`) {
		t.Error("inactive headers lack their natural-direction links")
	}
	for _, plain := range []string{"Status", "Reason", "Libraries", "Lyrics"} {
		if !strings.Contains(body, `<th scope="col">`+plain+"</th>") {
			t.Errorf("%s header should be plain and not sortable", plain)
		}
	}
	// Artist, Album, Title order.
	if !regexp.MustCompile(`(?s)>Artist.*>Album.*>Title`).MatchString(body) {
		t.Error("header order is not Artist, Album, Title")
	}
	desc := getQueue(t, mux, "/queue/pending?sort=title&dir=desc", false).Body.String()
	if !strings.Contains(desc, `aria-sort="descending"`) || strings.Contains(desc, `aria-sort="ascending"`) {
		t.Error("descending title sort not reflected in aria-sort")
	}
	if !strings.Contains(desc, `<input type="hidden" name="sort" value="title">`) ||
		!strings.Contains(desc, `<input type="hidden" name="dir" value="desc">`) {
		t.Error("search form drops the sort")
	}
	// A forged sort is never reflected anywhere in the page.
	evil := getQueue(t, mux, "/queue/pending?sort=%22%3E%3Cscript%3E&dir=x%22", false).Body.String()
	if strings.Contains(evil, "script%3E") || strings.Contains(evil, `value="&#34;`) {
		t.Error("invalid sort/dir reflected into the page")
	}
}

// status is in the shared sort vocabulary but not in the queue's spec: it must
// behave exactly like an unknown key, not linger in the state that feeds the
// search form, the pager and the header links.
func TestQueueSortKeyOutsideBucketSpecIsDropped(t *testing.T) {
	db := openReportsTestDB(t)
	seedSortRows(t, db, "failed", [][3]string{
		{"Pend 001", "2026-01-02T00:00:00Z", "2026-03-01T00:00:00Z"},
		{"Pend 002", "2026-01-03T00:00:00Z", "2026-03-03T00:00:00Z"},
		{"Pend 003", "2026-01-01T00:00:00Z", "2026-03-02T00:00:00Z"},
	})
	for i := 0; i < queuePageSize; i++ { // force a pager
		if _, err := db.Exec(`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, updated_at)
			VALUES ('Invented Artist', ?, 'invented artist', ?, 'Invented Album', 'failed', '2025-01-01T00:00:00Z')`,
			fmt.Sprintf("Old %03d", i), fmt.Sprintf("old %03d", i)); err != nil {
			t.Fatal(err)
		}
	}
	mux := newReportsUIServer(t, db)
	body := getQueue(t, mux, "/queue/failed?sort=status&q=pend", false).Body.String()
	if got := fmt.Sprint(titlesIn(body)); got != "[Pend 002 Pend 001 Pend 003]" {
		t.Errorf("order = %s, want the default (updated, newest first)", got)
	}
	if n := strings.Count(body, "aria-sort="); n != 1 || !strings.Contains(body, `aria-sort="descending"`) {
		t.Errorf("aria-sort count = %d, want exactly one descending (Updated)", n)
	}
	if strings.Contains(body, "sort=status") || strings.Contains(body, `name="sort"`) {
		t.Error("unsupported sort carried into the search form or a link")
	}
	paged := getQueue(t, mux, "/queue/failed?sort=status", false).Body.String()
	if !strings.Contains(paged, "after=") {
		t.Fatal("expected a pager on the page")
	}
	if strings.Contains(paged, "sort=status") {
		t.Error("pager link carries sort=status")
	}
}

func TestQueuePreviewHrefCarriesView(t *testing.T) {
	row := reports.BucketRow{ID: 7, Previewable: true}
	got := queuePreviewHref(row, reports.BucketFailed, queueViewState{Query: "a&b", Sort: "title", Dir: "desc", After: "5:n"})
	if got != "/preview/7?dir=desc&from=failed&q=a%26b&sort=title" {
		t.Errorf("href = %q (the cursor must not ride along)", got)
	}
}

func TestPreviewBackLinkCarriesAndValidatesView(t *testing.T) {
	f := newPreviewFixture(t)
	id := itoa(f.row(t, f.writeFile(t, f.root, "song.flac")))
	f.put(t, "song.lrc", pageLRC)
	for _, tc := range []struct{ query, href string }{
		{"?from=failed&q=moon&sort=title&dir=desc", "/queue/failed?dir=desc&amp;q=moon&amp;sort=title"},
		{"?from=failed&sort=title&dir=sideways", "/queue/failed?sort=title"},
		{"?from=failed&sort=id%3Bdrop&dir=up", "/queue/failed"},
		{"?from=failed&sort=title&sort=artist", "/queue/failed"},
		{"?from=failed&sort=status&q=moon", "/queue/failed?q=moon"}, // vocabulary key the bucket's spec lacks
		{"?from=failed&after=9%3An&sort=title", "/queue/failed?sort=title"},
		{"?from=failed&sort=title&q=" + strings.Repeat("x", maxQueueQueryRunes+1), "/queue/failed"}, // overlong search drops the whole view
		{"?from=bogus&q=moon&sort=title", "/queue"},
		{"?q=moon&sort=title", "/queue"},
	} {
		body := getPath(t, f.mux, "/preview/"+id+tc.query).Body.String()
		if !strings.Contains(body, `id="mx-preview-back" href="`+tc.href+`">`) {
			t.Errorf("query %q: back link not %q in %s", tc.query, tc.href, backLinkTag(body))
		}
	}
}

func backLinkTag(body string) string {
	i := strings.Index(body, `id="mx-preview-back"`)
	if i < 0 {
		return "(no back link)"
	}
	return body[i : i+120]
}
