package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func renderDashRecent(t *testing.T, rows []RecentOutcomeRow) string {
	t.Helper()
	var b bytes.Buffer
	if err := dashRecentOutcomes(rows).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func renderReportRecent(t *testing.T, rows []RecentOutcomeRow) string {
	t.Helper()
	var b bytes.Buffer
	cols := []SortHeaderView{{Label: "Artist"}, {Label: "Actions"}}
	if err := tableRecentOutcomes(cols, rows).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestRecentOutcomesActionsCell: on both Recent tables every row state shows
// exactly the icons its state allows, in one wrapper inside the hit-area cell,
// and a row with no work item id or no mark routes renders none.
func TestRecentOutcomesActionsCell(t *testing.T) {
	const ret = "/reports/recent-outcomes?ro_sort=title"
	act := func(a RowActions) RowActions { a.ID, a.Return = 9, ret; return a }
	cases := []struct {
		name   string
		a      RowActions
		titles []string
	}{
		{"synced", act(RowActions{HasLyric: true}), []string{"Mark instrumental", "Lyrics are wrong"}},
		{"unsynced", act(RowActions{HasLyric: true}), []string{"Mark instrumental", "Lyrics are wrong"}},
		{"pending", act(RowActions{}), []string{"Mark instrumental"}},
		{"failed", act(RowActions{}), []string{"Mark instrumental"}},
		{"manual instrumental", act(RowActions{Manual: true}), []string{"Marked instrumental by hand. Undo"}},
		{"blocked", act(RowActions{Blocked: true}), []string{"Mark instrumental", "Lyrics blocked. Unblock"}},
		{"blocked and re-fetched", act(RowActions{Blocked: true, HasLyric: true}), []string{"Mark instrumental", "Lyrics blocked. Unblock"}},
		{"in flight", act(RowActions{InFlight: true}), nil},
		{"no work item id", RowActions{Return: ret, HasLyric: true}, nil},
		{"no mark routes", RowActions{}, nil},
	}
	surfaces := map[string]func(*testing.T, []RecentOutcomeRow) string{"dashboard": renderDashRecent, "reports": renderReportRecent}
	for surface, render := range surfaces {
		for _, c := range cases {
			out := render(t, []RecentOutcomeRow{{Artist: "A", Title: "T", Actions: c.a}})
			if n := strings.Count(out, `<td class="mx-row-hit-td"><span class="mx-row-cell">`); n != 1 {
				t.Errorf("%s %s: %d hit-area cells with one wrapper, want 1", surface, c.name, n)
			}
			if n := strings.Count(out, `class="mx-row-icon`); n != len(c.titles) {
				t.Errorf("%s %s: %d icons, want %d", surface, c.name, n, len(c.titles))
			}
			for _, title := range c.titles {
				if !strings.Contains(out, `title="`+title+`"`) || !strings.Contains(out, `<span class="mx-row-sr">`+title+`</span>`) {
					t.Errorf("%s %s: missing %q (tooltip and accessible label)", surface, c.name, title)
				}
			}
			if len(c.titles) > 0 && !strings.Contains(out, "/queue/9/") {
				t.Errorf("%s %s: links do not address work item 9: %s", surface, c.name, out)
			}
			if len(c.titles) > 0 && !strings.Contains(out, "?return=%2Freports%2Frecent-outcomes%3Fro_sort%3Dtitle") {
				t.Errorf("%s %s: return target not encoded into the links", surface, c.name)
			}
			if len(c.titles) == 0 && strings.Contains(out, "/queue/") {
				t.Errorf("%s %s: rendered a mark link", surface, c.name)
			}
		}
	}
}

func TestDashboardRecentHasActionsHeader(t *testing.T) {
	out := renderDashRecent(t, []RecentOutcomeRow{{Artist: "A"}})
	if !strings.Contains(out, "<th>Completed</th>\n\t\t\t\t\t\t\t<th>Actions</th>") && !strings.Contains(out, "<th>Actions</th>") {
		t.Errorf("no Actions header: %s", out)
	}
}
