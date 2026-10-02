package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
)

func getPath(t *testing.T, mux http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// queueNavCurrent matches the Queue sidebar anchor carrying aria-current.
var queueNavCurrent = regexp.MustCompile(`<a href="/queue" class="mx-nav-link" aria-current="page">`)

func TestSidebarQueueItemActiveOnQueueRoutes(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	seedFinishedSplitRows(t, sqlDB)
	f := &previewFixture{root: t.TempDir(), db: sqlDB}
	if _, err := sqlDB.Exec(`INSERT INTO libraries (path, name) VALUES (?, 'lib')`, f.root); err != nil {
		t.Fatal(err)
	}
	f.mux = newReportsUIServer(t, sqlDB)
	id := f.row(t, f.writeFile(t, f.root, "song.flac"))
	f.put(t, "song.lrc", pageLRC)

	for _, target := range []string{"/queue", "/queue/finished", "/queue/unavailable", "/preview/" + itoa(id)} {
		rec := getPath(t, f.mux, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", target, rec.Code)
		}
		if !queueNavCurrent.MatchString(rec.Body.String()) {
			t.Errorf("GET %s: Queue nav item is not aria-current", target)
		}
		if strings.Contains(rec.Body.String(), `<a href="/dashboard" class="mx-nav-link" aria-current="page">`) {
			t.Errorf("GET %s: Dashboard nav item is also current", target)
		}
	}
	// The Queue item sits between Dashboard and Settings.
	body := getPath(t, f.mux, "/dashboard").Body.String()
	d, q, s := strings.Index(body, `href="/dashboard" class="mx-nav-link"`), strings.Index(body, `href="/queue" class="mx-nav-link"`), strings.Index(body, `href="/settings" class="mx-nav-link"`)
	if d < 0 || q < d || s < q {
		t.Errorf("nav order dashboard=%d queue=%d settings=%d, want ascending", d, q, s)
	}
	if queueNavCurrent.MatchString(body) {
		t.Error("Queue nav item is current on /dashboard")
	}
}

// TestQueueIndexCountsMatchDashboardTiles: the landing page and the dashboard
// tiles read the same QueueSummary, so every label, count and href agrees.
func TestQueueIndexCountsMatchDashboardTiles(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	seedFinishedSplitRows(t, sqlDB)
	seedQueueRows(t, sqlDB, "pending", "Pend", 4)
	seedQueueRows(t, sqlDB, "failed", "Fail", 2)
	mux := newReportsUIServer(t, sqlDB)

	tile := regexp.MustCompile(`<span class="mx-dash-tile-label">([^<]+)</span>\s*<span class="mx-dash-tile-value">(\d+)</span>`)
	var want [][2]string
	for _, m := range tile.FindAllStringSubmatch(getPath(t, mux, "/dashboard").Body.String(), -1) {
		want = append(want, [2]string{m[1], m[2]})
	}
	row := regexp.MustCompile(`<td><a class="mx-queue-link" href="(/queue/[a-z]+)">([^<]+)</a></td>\s*<td class="mx-cell-mono">(\d+)</td>`)
	var got [][2]string
	for _, m := range row.FindAllStringSubmatch(getPath(t, mux, "/queue").Body.String(), -1) {
		got = append(got, [2]string{m[2], m[3]})
	}
	// The dashboard also renders Results tiles; the queue tiles are its first
	// len(got) entries, in the same order.
	if len(got) != 6 || len(want) < len(got) {
		t.Fatalf("got %d index rows, %d dashboard tiles; want 6 rows", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("row %d = %v, dashboard tile = %v", i, got[i], want[i])
		}
	}
}

func TestQueueIndexEditHintOnlyOnCompletedBuckets(t *testing.T) {
	mux := newReportsUIServer(t, openReportsTestDB(t))
	body := getPath(t, mux, "/queue").Body.String()
	if n := strings.Count(body, "can be previewed and their timing edited"); n != 2 {
		t.Errorf("edit hint appears %d times, want 2 (Finished, Settled)", n)
	}
}

func TestPreviewBackLink(t *testing.T) {
	f := newPreviewFixture(t)
	id := itoa(f.row(t, f.writeFile(t, f.root, "song.flac")))
	f.put(t, "song.lrc", pageLRC)
	for _, tc := range []struct{ query, href, label string }{
		{"", "/queue", "Back to queue"},
		{"?from=finished", "/queue/finished", "Back to Finished"},
		{"?from=settled", "/queue/settled", "Back to Settled (upgradable)"},
		{"?from=bogus", "/queue", "Back to queue"},
		{"?from=%22%3E%3Cscript%3E", "/queue", "Back to queue"},
		{"?from=/dashboard", "/queue", "Back to queue"},
	} {
		body := getPath(t, f.mux, "/preview/"+id+tc.query).Body.String()
		want := `id="mx-preview-back" href="` + tc.href + `">` + tc.label + `</a>`
		if !strings.Contains(body, want) {
			t.Errorf("query %q: missing %s", tc.query, want)
		}
	}
}

// TestQueueIndexQueryError: a failing summary source yields a 500 and never a
// partial page, and the response is not cacheable.
func TestQueueIndexQueryError(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	mux := newReportsUIServer(t, sqlDB)
	_ = sqlDB.Close() // force every query to error

	rec := getPath(t, mux, "/queue")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("GET /queue over a closed DB = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "mx-queue-link") {
		t.Error("error response rendered queue rows")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// TestQueueIndexReportsNotWired: a UI built without a reports repo answers 503
// rather than panicking on a nil source.
func TestQueueIndexReportsNotWired(t *testing.T) {
	mux := http.NewServeMux()
	NewUI(config.Config{}, "v-test").Register(mux)

	rec := getPath(t, mux, "/queue")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /queue without reports = %d, want 503", rec.Code)
	}
}
