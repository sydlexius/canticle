package web

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	unattributedTileRE = regexp.MustCompile(`(?s)<a class="mx-dash-tile-link" href="` + regexp.QuoteMeta(unattributedPath) +
		`"[^>]*>\s*<span class="mx-dash-tile-label"><span class="mx-lane-cell"><span>Unattributed</span></span></span>\s*<span class="mx-dash-tile-value">(\d+)</span>`)
	sourceTotalRE = regexp.MustCompile(`Total (\d+) completed tracks`)
	tileValueRE   = regexp.MustCompile(`<span class="mx-dash-tile-value">(\d+)</span>`)
)

// TestUnattributedTileEqualsItsPage pins #1422: the Lyrics Sources row carries
// a tile for results with no recorded source, linking to the unattributed page,
// whose count equals both that page's own total (and the sum of its per-type
// tiles) and the done rows holding no provider_lane, on real SQLite.
func TestUnattributedTileEqualsItsPage(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	seedSources(t, sqlDB)
	seedSourceRow(t, sqlDB, "n2", "", "", "synced", "line")
	seedSourceRow(t, sqlDB, "n3", "", "", "instrumental", "")
	// A blocked track keeps no source, so it joins the group (#1395).
	seedSourceRow(t, sqlDB, "n4", "", "", "blocked", "")
	// A non-done row with no lane is not a result and must not count.
	seedLinkRow(t, sqlDB, "pend", "pending", "", "", "", "", "", "")
	mux := newReportsUIServer(t, sqlDB)

	var want int
	if err := sqlDB.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM work_queue WHERE status = 'done' AND provider_lane IS NULL`).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if want != 4 {
		t.Fatalf("seed has %d unattributed done rows, want 4", want)
	}

	_, dash := getSource(t, mux, "/dashboard")
	m := unattributedTileRE.FindStringSubmatch(dash)
	if m == nil {
		t.Fatal("dashboard has no linked Unattributed tile")
	}
	if n, _ := strconv.Atoi(m[1]); n != want {
		t.Errorf("tile count %d, want %d", n, want)
	}
	if strings.Count(dash, `href="`+unattributedPath+`"`) != 1 {
		t.Errorf("want exactly one link to %s on the dashboard", unattributedPath)
	}
	if strings.Contains(dash, "Results with no recorded source") {
		t.Error("the text link the tile replaces is still rendered")
	}

	code, page := getSource(t, mux, unattributedPath)
	if code != 200 {
		t.Fatalf("GET %s = %d", unattributedPath, code)
	}
	tm := sourceTotalRE.FindStringSubmatch(page)
	if tm == nil {
		t.Fatal("page has no total")
	}
	if n, _ := strconv.Atoi(tm[1]); n != want {
		t.Errorf("page total %d, tile/SQL %d", n, want)
	}
	sum := 0
	for _, v := range tileValueRE.FindAllStringSubmatch(page, -1) {
		n, _ := strconv.Atoi(v[1])
		sum += n
	}
	if sum != want {
		t.Errorf("page type tiles sum to %d, want %d", sum, want)
	}
}

// TestUnattributedTileHiddenAtZero: with every done row attributed there is no
// group to link to, so no tile and no stray link, while lane tiles still render.
func TestUnattributedTileHiddenAtZero(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	seedSourceRow(t, sqlDB, "m1", "musixmatch", "", "synced", "word")
	mux := newReportsUIServer(t, sqlDB)

	_, dash := getSource(t, mux, "/dashboard")
	if strings.Contains(dash, "Unattributed") || strings.Contains(dash, unattributedPath) {
		t.Error("zero unattributed results still render a tile or link")
	}
	if !strings.Contains(dash, `href="/sources/musixmatch"`) {
		t.Error("lane tile missing")
	}
}
