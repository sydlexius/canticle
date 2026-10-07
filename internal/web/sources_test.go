package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/detectorbackfill"
	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/internal/trustnet"
)

// seedSourceRow seeds a done row; an empty lane or upstream is NULL.
func seedSourceRow(t *testing.T, db *sql.DB, title, lane, upstream, outcome, tier string) {
	t.Helper()
	seedLinkRow(t, db, title, "done", outcome, tier, "", "", "", "")
	if _, err := db.ExecContext(context.Background(),
		`UPDATE work_queue SET provider_lane = NULLIF(?, ''), upstream = NULLIF(?, '') WHERE title = ?`,
		lane, upstream, title); err != nil {
		t.Fatal(err)
	}
	if lane == "" {
		return
	}
	// One attempt, so the lane has a dashboard tile.
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO lane_attempts (queue_id, lane, hit, attempted_at)
         SELECT id, ?, 1, '2026-06-18T00:00:00Z' FROM work_queue WHERE title = ?`, lane, title); err != nil {
		t.Fatal(err)
	}
}

func seedSources(t *testing.T, db *sql.DB) {
	t.Helper()
	seedSourceRow(t, db, "m1", "musixmatch", "", "synced", "word")
	seedSourceRow(t, db, "m2", "musixmatch", "", "synced", "word")
	seedSourceRow(t, db, "m3", "musixmatch", "", "synced", "line")
	seedSourceRow(t, db, "m4", "musixmatch", "", "unsynced", "")
	seedSourceRow(t, db, "i1", "innertube", "lyricfind", "synced", "line")
	seedSourceRow(t, db, "i2", "innertube", "lyricfind", "unsynced", "")
	seedSourceRow(t, db, "i3", "innertube", "musixmatch", "synced", "line")
	seedSourceRow(t, db, "i4", "innertube", "", "synced", "line")
	seedSourceRow(t, db, "d1", detectorbackfill.LaneName, "", "instrumental", "")
	seedSourceRow(t, db, "n1", "", "", "unsynced", "")
}

func getSource(t *testing.T, mux http.Handler, path string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func TestSourcePages(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	seedSources(t, sqlDB)
	mux := newReportsUIServer(t, sqlDB)

	t.Run("known source: table, chart, row links", func(t *testing.T) {
		code, body := getSource(t, mux, "/sources/musixmatch")
		if code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		for _, want := range []string{
			`href="/queue/finished?lane=musixmatch"`,
			`href="/queue/settled?lane=musixmatch&amp;tier=line"`,
			`data-chart-labels="[&#34;Word-synced&#34;,&#34;Line-synced&#34;,&#34;Unsynced&#34;]"`,
			`data-chart-values="[2,1,1]"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %q", want)
			}
		}
		if strings.Contains(body, "By upstream") {
			t.Error("a single-source lane must not render the upstream block")
		}
	})

	t.Run("unknown lane is 404", func(t *testing.T) {
		for _, p := range []string{"/sources/nosuchlane", "/sources/unattributed", "/sources/-"} {
			if code, _ := getSource(t, mux, p); code != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404", p, code)
			}
		}
	})

	t.Run("known lane with no rows is an empty state", func(t *testing.T) {
		if code, body := getSource(t, mux, "/sources/petitlyrics"); code != 200 || !strings.Contains(body, "Data accrues") {
			t.Errorf("status %d, empty state missing", code)
		}
	})

	t.Run("unattributed", func(t *testing.T) {
		code, body := getSource(t, mux, unattributedPath)
		if code != http.StatusOK || !strings.Contains(body, "no recorded source") || strings.Contains(body, "/queue/") {
			t.Errorf("status %d; want 200, blurb, and no queue links", code)
		}
	})

	t.Run("detector rows are plain text", func(t *testing.T) {
		code, body := getSource(t, mux, "/sources/"+detectorbackfill.LaneName)
		if code != http.StatusOK || !strings.Contains(body, "Instrumental Detector") || strings.Contains(body, "/queue/") {
			t.Errorf("status %d; want 200, label, and no queue links", code)
		}
	})

	t.Run("multiplexing source adds the upstream table", func(t *testing.T) {
		code, body := getSource(t, mux, "/sources/innertube")
		if code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		for _, want := range []string{
			"By upstream",
			`data-chart-labels="[&#34;lyricfind&#34;,&#34;musixmatch&#34;,&#34;Not recorded&#34;]"`,
			`data-chart-values="[2,1,1]"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %q", want)
			}
		}
	})

	t.Run("dashboard links tiles and the unattributed page", func(t *testing.T) {
		_, body := getSource(t, mux, "/dashboard")
		for _, want := range []string{`href="/sources/musixmatch"`, `href="/sources/` + detectorbackfill.LaneName + `"`, `href="` + unattributedPath + `"`} {
			if !strings.Contains(body, want) {
				t.Errorf("dashboard missing %q", want)
			}
		}
	})
}

func TestSourcePagesRequireSession(t *testing.T) {
	a, svc := newTestAuth(t, trustnet.LoopbackOnly())
	sqlDB := openReportsTestDB(t)
	seedSources(t, sqlDB)
	mux := http.NewServeMux()
	NewUI(config.Config{}, "vtest", WithAuth(a), WithReports(reports.New(sqlDB))).Register(mux)
	for _, p := range []string{"/sources/musixmatch", unattributedPath} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.RemoteAddr = "198.51.100.40:1"
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login") {
			t.Errorf("GET %s unauthenticated = %d -> %q, want 303 to /login", p, rec.Code, rec.Header().Get("Location"))
		}
		req = httptest.NewRequest(http.MethodGet, p, nil)
		req.RemoteAddr = "198.51.100.41:1"
		req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: loginToken(t, svc)})
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s with a session = %d, want 200", p, rec.Code)
		}
	}
}

// TestSourceRowLinksEqualPopulations: each linked row lists exactly its count, under both rungs.
func TestSourceRowLinksEqualPopulations(t *testing.T) {
	for _, top := range []reports.TopRung{reports.TopRungWord, reports.TopRungLine} {
		sqlDB := openReportsTestDB(t)
		seedSources(t, sqlDB)
		repo := reports.New(sqlDB, reports.WithLineTopRung(top == reports.TopRungLine))
		all, err := repo.SourceBreakdown(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		linked := 0
		for _, sb := range all {
			labels, vals := typeCellsFor(sb.Counts, top)
			for i, l := range labels {
				href := sourceTypeLink(sb, l, top)
				if href == "" {
					continue
				}
				linked++
				if n := listFromHref(t, repo, top, href); int64(n) != vals[i] {
					t.Errorf("rung %v %s/%s: count %d but %s lists %d", top, sb.Lane, l, vals[i], href, n)
				}
			}
		}
		if want := map[reports.TopRung]int{reports.TopRungWord: 4, reports.TopRungLine: 2}[top]; linked != want {
			t.Errorf("rung %v: %d linked rows, want %d", top, linked, want)
		}
	}
}
