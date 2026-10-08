package web

import (
	"database/sql"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/web/templates"
)

// seedAttentionUpNextRows seeds Needs attention rows (failed, deferred, and a
// failed row carrying a manual mark) and Up Next rows (buffered, one carrying a
// mark, plus a claimed row that appears only in the in-flight group).
func seedAttentionUpNextRows(t *testing.T, db *sql.DB) map[string]int64 {
	t.Helper()
	rows := []struct {
		title, status, extra string
	}{
		{"Att Failed", "failed", ", last_error = 'boom', attempts = 1"},
		{"Att Deferred", "deferred", ", last_error = 'orchestrator: lane benign miss (no result)', miss_count = 1"},
		{"Att Marked", "failed", ", last_error = 'boom', attempts = 1, manual_instrumental_at = '2026-01-01T00:00:00Z'"},
		{"Up Plain", "pending", ", batch_seq = 1"},
		{"Up Marked", "pending", ", batch_seq = 2, manual_instrumental_at = '2026-01-01T00:00:00Z'"},
		{"Up Claimed", "processing", ""},
	}
	ids := map[string]int64{}
	for _, r := range rows {
		var id int64
		if err := db.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status)
			VALUES ('Act Artist', ?, 'act artist', lower(?), 'Al', ?) RETURNING id`, r.title, r.title, r.status).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE work_queue SET id = id`+r.extra+` WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
		ids[r.title] = id
	}
	return ids
}

// markLinkRE matches a per-row mark link (/queue/<id>/...), not the bucket links.
var markLinkRE = regexp.MustCompile(`/queue/\d+/`)

type attentionSurface struct {
	name, page, wantReturn string
	titles                 []string
}

var attentionSurfaces = []attentionSurface{
	{"dashboard", "/dashboard", "/dashboard", []string{"Att Failed", "Att Deferred", "Att Marked", "Up Plain", "Up Marked"}},
	{"reports", "/reports/needs-attention?mark=wrong_marked&files=2&junk=1", "/reports/needs-attention", []string{"Att Failed", "Att Deferred", "Att Marked"}},
}

// TestAttentionAndUpNextActionsColumn: every render surface of the two tables
// shows the Actions header and, per row, the one icon its RECORDED state allows
// (never the flag), addressing the row and returning to the page the user is on
// without the one-shot status or any unvalidated parameter. A claimed row has
// no link.
func TestAttentionAndUpNextActionsColumn(t *testing.T) {
	db := openReportsTestDB(t)
	ids := seedAttentionUpNextRows(t, db)
	mux := newActionsMux(t, db, "/data/x.db")
	for _, s := range attentionSurfaces {
		body := getQueue(t, mux, s.page, false).Body.String()
		if !strings.Contains(body, "Actions</th>") {
			t.Errorf("%s: no Actions header", s.name)
		}
		for _, title := range s.titles {
			row := rowHTML(t, body, title)
			wantUndo := strings.HasSuffix(title, "Marked")
			if got := strings.Contains(row, `title="`+recUndo+`"`); got != wantUndo {
				t.Errorf("%s %s: undo icon rendered=%v, want %v", s.name, title, got, wantUndo)
			}
			if got := strings.Contains(row, `title="`+recMark+`"`); got == wantUndo {
				t.Errorf("%s %s: mark icon rendered=%v, want %v", s.name, title, got, !wantUndo)
			}
			if n := strings.Count(row, `class="mx-row-icon`); n != 1 {
				t.Errorf("%s %s: %d icons, want exactly 1", s.name, title, n)
			}
			for _, flag := range []string{recWrong, recUnblock} {
				if strings.Contains(row, flag) {
					t.Errorf("%s %s: rendered the flag %q", s.name, title, flag)
				}
			}
			if !strings.Contains(row, "/queue/"+itoa(ids[title])+"/") || !strings.Contains(row, "?return="+url.QueryEscape(s.wantReturn)+`"`) {
				t.Errorf("%s %s: links do not address the row and return to %s: %s", s.name, title, s.wantReturn, row)
			}
		}
		if strings.Contains(body, "mark%3D") || strings.Contains(body, "files%3D") || strings.Contains(body, "junk") {
			t.Errorf("%s: a return target carries the status or an unvalidated parameter", s.name)
		}
	}
	// The claimed row sits in the Up Next table with no link.
	dash := getQueue(t, mux, "/dashboard", false).Body.String()
	if row := rowHTML(t, dash, "Up Claimed"); strings.Contains(row, "/queue/") || strings.Contains(row, "mx-row-icon") {
		t.Errorf("claimed row offers an action: %s", row)
	}
}

// With no backup location the routes 404, so no icon is rendered on either
// table; the header stays so the columns line up either way.
func TestAttentionAndUpNextNoActionsWithoutMarkDeps(t *testing.T) {
	db := openReportsTestDB(t)
	seedAttentionUpNextRows(t, db)
	for name, mux := range map[string]http.Handler{
		"never attached": newReportsUIServer(t, db),
		"empty db path":  newActionsMux(t, db, ""),
	} {
		for _, s := range attentionSurfaces {
			body := getQueue(t, mux, s.page, false).Body.String()
			if strings.Contains(body, "mx-row-icon") || markLinkRE.MatchString(body) {
				t.Errorf("%s %s: icons rendered without mark routes", name, s.name)
			}
			if !strings.Contains(body, "Actions</th>") {
				t.Errorf("%s %s: header lost", name, s.name)
			}
		}
	}
}

// TestNeedsAttentionReportStatusLine: the Needs attention report renders the
// fixed sentence for a known code, and nothing for a missing, unknown or crafted
// one (the page the icons return to must show what happened).
func TestNeedsAttentionReportStatusLine(t *testing.T) {
	db := openReportsTestDB(t)
	seedAttentionUpNextRows(t, db)
	mux := newActionsMux(t, db, "/data/x.db")
	known := templates.MarkStatusText(templates.MarkInstrumentalDone, 2)
	cases := []struct{ name, query, want string }{
		{"known", "?mark=instrumental_marked&files=2", known},
		{"undo", "?mark=instrumental_undone", templates.MarkStatusText(templates.MarkInstrumentalUndone, 0)},
		{"missing", "", ""},
		{"unknown", "?mark=nope", ""},
		{"crafted", "?mark=%3Cb%3Eowned%3C%2Fb%3E&files=%3Cb%3E7331%3C%2Fb%3E", ""},
	}
	for _, c := range cases {
		body := getQueue(t, mux, "/reports/needs-attention"+c.query, false).Body.String()
		got := strings.Contains(body, `class="mx-row-status"`)
		if c.want == "" {
			if got || strings.Contains(body, "owned") || strings.Contains(body, "7331") {
				t.Errorf("%s: rendered a status line or echoed input", c.name)
			}
			continue
		}
		if !got || !strings.Contains(body, c.want) {
			t.Errorf("%s: status %q not rendered", c.name, c.want)
		}
		// The htmx fragment (Refresh) carries it too.
		frag := getQueue(t, mux, "/reports/needs-attention"+c.query, true).Body.String()
		if !strings.Contains(frag, c.want) {
			t.Errorf("%s: fragment lacks the status", c.name)
		}
	}
}
