package web

import (
	"database/sql"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/reports"
)

// seedActionRows seeds one row per state the Actions column distinguishes and
// returns each row's id by title.
func seedActionRows(t *testing.T, db *sql.DB) map[string]int64 {
	t.Helper()
	rows := []struct {
		title, status, outcome, extra string
	}{
		{"Syn", "done", "synced", ""},
		{"Uns", "done", "unsynced", ""},
		{"Pen", "pending", "", ""},
		{"Fai", "failed", "", ""},
		{"Man", "done", "instrumental", ", manual_instrumental_at = '2026-01-01T00:00:00Z'"},
		{"Blo", "done", "blocked", ""},
		{"Ref", "done", "synced", ""},
		{"Edi", "done", "synced", ", lyric_edited_at = '2026-01-01T00:00:00Z', lyric_offset_ms = 600"},
		{"Fly", "processing", "", ""},
		{"Wip", "failed", "synced", ""},
	}
	ids := map[string]int64{}
	for _, r := range rows {
		var id int64
		if err := db.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, outcome_type, sync_tier)
			VALUES ('Act Artist', ?, 'act artist', lower(?), 'Al', ?, NULLIF(?, ''), CASE WHEN ? = 'synced' THEN 'line' END) RETURNING id`,
			r.title, r.title, r.status, r.outcome, r.outcome).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if r.extra != "" {
			if _, err := db.Exec(`UPDATE work_queue SET id = id`+r.extra+` WHERE id = ?`, id); err != nil {
				t.Fatal(err)
			}
		}
		ids[r.title] = id
	}
	for _, k := range []string{"blo", "ref"} {
		if _, err := db.Exec(`INSERT INTO lyric_blocks (artist_key, title_key, fingerprint) VALUES ('act artist', ?, 'fp')`, k); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func newActionsMux(t *testing.T, db *sql.DB, dbPath string) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	ui := NewUI(config.Config{}, "v-test", WithReports(reports.New(db)))
	ui.AttachMarkActions(MarkDeps{DB: db, Instrumental: &fakeMarks{}, Blocks: &fakeBlocks{}, DBPath: dbPath})
	ui.Register(mux)
	return mux
}

// TestQueueBucketActionsColumn: every bucket page has the Actions header, and each
// row state shows exactly the icons its RECORDED state allows, linking back to the
// current page.
func TestQueueBucketActionsColumn(t *testing.T) {
	db := openReportsTestDB(t)
	ids := seedActionRows(t, db)
	mux := newActionsMux(t, db, "/data/x.db")
	id := func(title string) string { return itoa(ids[title]) }
	const (
		mark, undo, wrong, unblock = "Mark instrumental", "Marked instrumental by hand. Undo", "Lyrics are wrong", "Lyrics blocked. Unblock"
	)
	cases := []struct {
		page, title string
		want        []string
	}{
		{"/queue/settled", "Syn", []string{mark, wrong}},
		{"/queue/settled", "Uns", []string{mark, wrong}},
		{"/queue/settled", "Edi", []string{mark, wrong}},
		{"/queue/settled", "Ref", []string{mark, unblock}}, // blocked and re-fetched: the flag is on
		{"/queue/pending", "Pen", []string{mark}},
		{"/queue/failed", "Fai", []string{mark}},
		{"/queue/failed", "Wip", []string{mark}}, // a failed row never offers "wrong", whatever it once recorded
		{"/queue/finished", "Man", []string{undo}},
		{"/queue/blocked", "Blo", []string{mark, unblock}},
		{"/queue/processing", "Fly", nil},
	}
	for _, c := range cases {
		body := getQueue(t, mux, c.page+"?sort=title&dir=asc", false).Body.String()
		if !strings.Contains(body, `<th scope="col">Actions</th>`) || strings.Contains(body, `<th scope="col">Lyrics</th>`) {
			t.Errorf("%s: no Actions header", c.page)
		}
		row := rowHTML(t, body, c.title)
		for _, name := range []string{mark, undo, wrong, unblock} {
			got := strings.Contains(row, `title="`+name+`"`)
			want := false
			for _, w := range c.want {
				want = want || w == name
			}
			if got != want {
				t.Errorf("%s %s: %q rendered=%v, want %v", c.page, c.title, name, got, want)
			}
		}
		if len(c.want) > 0 {
			ret := url.QueryEscape(c.page + "?dir=asc&sort=title")
			if !strings.Contains(row, "/queue/"+id(c.title)+"/") || !strings.Contains(row, "?return="+ret) {
				t.Errorf("%s %s: links do not return to the page (%s): %s", c.page, c.title, ret, row)
			}
		}
	}
	// The one-shot status never rides into a return target.
	body := getQueue(t, mux, "/queue/settled?mark=wrong_marked&files=2", false).Body.String()
	if strings.Contains(body, "return=%2Fqueue%2Fsettled%3Fmark") || strings.Contains(body, "files%3D") {
		t.Error("return target carries the one-shot status")
	}
}

var pendingMoreRE = regexp.MustCompile(`hx-get="(/queue/pending\?[^"]*)"`)

// The "Show more" fragment carries the same cell as the full page, with the
// cursor of the batch it belongs to in its return target.
func TestQueueFragmentRowsCarryActions(t *testing.T) {
	db := openReportsTestDB(t)
	seedQueueRows(t, db, "pending", "Pend", queuePageSize+3)
	mux := newActionsMux(t, db, "/data/x.db")
	page := getQueue(t, mux, "/queue/pending", false).Body.String()
	if n := strings.Count(page, "mx-row-icon"); n != queuePageSize {
		t.Fatalf("page icons = %d, want %d", n, queuePageSize)
	}
	m := pendingMoreRE.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no Show more link")
	}
	frag := getQueue(t, mux, strings.ReplaceAll(m[1], "&amp;", "&"), true).Body.String()
	if n := strings.Count(frag, "mx-row-icon"); n != 3 {
		t.Errorf("fragment icons = %d, want 3", n)
	}
	if !strings.Contains(frag, "?return=%2Fqueue%2Fpending%3Fafter%3D") {
		t.Errorf("fragment return target lacks its cursor: %s", frag)
	}
	if strings.Contains(frag, "<th") {
		t.Error("fragment rendered a header")
	}
}

// With no backup location the routes 404, so the icons are not rendered, and the
// header and the Preview cell stay.
func TestQueueBucketNoActionsWithoutMarkDeps(t *testing.T) {
	db := openReportsTestDB(t)
	seedActionRows(t, db)
	for name, mux := range map[string]http.Handler{
		"never attached": newReportsUIServer(t, db),
		"empty db path":  newActionsMux(t, db, ""),
	} {
		body := getQueue(t, mux, "/queue/settled", false).Body.String()
		if row := rowHTML(t, body, "Syn"); strings.Contains(row, "mx-row-icon") || strings.Contains(row, "?return=") {
			t.Errorf("%s: icons rendered without mark routes", name)
		}
		if !strings.Contains(body, "Actions") || !strings.Contains(rowHTML(t, body, "Edi"), "mx-queue-edited") {
			t.Errorf("%s: header or Edited badge lost", name)
		}
	}
}
