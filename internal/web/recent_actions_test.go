package web

import (
	"database/sql"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/web/templates"
)

const (
	recMark, recUndo, recWrong, recUnblock = "Mark instrumental", "Marked instrumental by hand. Undo", "Lyrics are wrong", "Lyrics blocked. Unblock"
)

// seedRecentActionRows adds the Recent-only state (an exhausted miss) to the rows
// the Queue test seeds; rows still pending, failed or in flight are never listed.
func seedRecentActionRows(t *testing.T, db *sql.DB) map[string]int64 {
	t.Helper()
	ids := seedActionRows(t, db)
	var id int64
	if err := db.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, last_error)
		VALUES ('Act Artist', 'Exh', 'act artist', 'exh', 'Al', 'unavailable', 'miss limit reached') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	ids["Exh"] = id
	return ids
}

type recentSurface struct{ name, page, wantReturn string }

var recentSurfaces = []recentSurface{
	{"dashboard", "/dashboard", "/dashboard"},
	{"reports", "/reports/recent-outcomes?ro_sort=title&ro_dir=asc&mark=wrong_marked&files=2&junk=1", "/reports/recent-outcomes?ro_dir=asc&ro_sort=title"},
}

// TestRecentOutcomesActionsColumn: both Recent tables show the Actions header and,
// per row, exactly the icons the RECORDED state allows, each addressing the row's
// work item and returning to the page the user is on with its own sort only.
func TestRecentOutcomesActionsColumn(t *testing.T) {
	db := openReportsTestDB(t)
	ids := seedRecentActionRows(t, db)
	mux := newActionsMux(t, db, "/data/x.db")
	cases := []struct {
		title string
		want  []string
	}{
		{"Syn", []string{recMark, recWrong}},
		{"Uns", []string{recMark, recWrong}},
		{"Edi", []string{recMark, recWrong}},
		{"Exh", []string{recMark}},
		{"Man", []string{recUndo}},
		{"Blo", []string{recMark, recUnblock}},
		{"Ref", []string{recMark, recUnblock}}, // blocked and re-fetched
	}
	for _, s := range recentSurfaces {
		body := getQueue(t, mux, s.page, false).Body.String()
		if !strings.Contains(body, "Actions</th>") {
			t.Errorf("%s: no Actions header", s.name)
		}
		for _, c := range cases {
			row := rowHTML(t, body, c.title)
			for _, name := range []string{recMark, recUndo, recWrong, recUnblock} {
				want := false
				for _, w := range c.want {
					want = want || w == name
				}
				if got := strings.Contains(row, `title="`+name+`"`); got != want {
					t.Errorf("%s %s: %q rendered=%v, want %v", s.name, c.title, name, got, want)
				}
			}
			if !strings.Contains(row, "/queue/"+itoa(ids[c.title])+"/") || !strings.Contains(row, "?return="+url.QueryEscape(s.wantReturn)+`"`) {
				t.Errorf("%s %s: links do not address the row and return to %s: %s", s.name, c.title, s.wantReturn, row)
			}
		}
		// Neither the one-shot status nor an unvalidated parameter rides into a return target.
		if strings.Contains(body, "return=%2F"+"dashboard%3F") || strings.Contains(body, "mark%3D") || strings.Contains(body, "files%3D") || strings.Contains(body, "junk") {
			t.Errorf("%s: a return target carries the status or an unvalidated parameter", s.name)
		}
	}
}

// With no backup location the routes 404, so the icons are not rendered; the
// header stays, so the columns line up either way.
func TestRecentOutcomesNoActionsWithoutMarkDeps(t *testing.T) {
	db := openReportsTestDB(t)
	seedRecentActionRows(t, db)
	for name, mux := range map[string]http.Handler{
		"never attached": newReportsUIServer(t, db),
		"empty db path":  newActionsMux(t, db, ""),
	} {
		for _, s := range recentSurfaces {
			body := getQueue(t, mux, s.page, false).Body.String()
			if row := rowHTML(t, body, "Syn"); strings.Contains(row, "mx-row-icon") || strings.Contains(row, "/queue/") {
				t.Errorf("%s %s: icons rendered without mark routes", name, s.name)
			}
			if !strings.Contains(body, "Actions</th>") {
				t.Errorf("%s %s: header lost", name, s.name)
			}
		}
	}
}

// TestRecentOutcomesStatusLine: after an action the page renders the fixed
// sentence for a known code, and nothing for a missing, unknown or crafted one.
func TestRecentOutcomesStatusLine(t *testing.T) {
	db := openReportsTestDB(t)
	seedRecentActionRows(t, db)
	mux := newActionsMux(t, db, "/data/x.db")
	known := templates.MarkStatusText(templates.MarkInstrumentalDone, 2)
	for _, base := range []struct{ name, path string }{{"dashboard", "/dashboard"}, {"reports", "/reports/recent-outcomes"}} {
		cases := []struct {
			name, query string
			want        string
		}{
			{"known", "?mark=instrumental_marked&files=2", known},
			{"known no count", "?mark=unblocked", templates.MarkStatusText(templates.MarkUnblocked, 0)},
			{"missing", "", ""},
			{"unknown", "?mark=nope", ""},
			{"crafted", "?mark=%3Cb%3Eowned%3C%2Fb%3E&files=%3Cb%3E7331%3C%2Fb%3E", ""},
		}
		for _, c := range cases {
			body := getQueue(t, mux, base.path+c.query, false).Body.String()
			got := strings.Contains(body, `class="mx-row-status"`)
			if c.want == "" {
				if got || strings.Contains(body, "owned") || strings.Contains(body, "7331") {
					t.Errorf("%s %s: rendered a status line or echoed input", base.name, c.name)
				}
				continue
			}
			if !got || !strings.Contains(body, c.want) {
				t.Errorf("%s %s: status %q not rendered", base.name, c.name, c.want)
			}
		}
	}
}
