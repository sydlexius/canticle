package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func renderAttention(t *testing.T, rows []AttentionRow) string {
	t.Helper()
	var b bytes.Buffer
	if err := attentionTable(rows, "none").Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func renderUpNext(t *testing.T, inFlight []InFlightRow, rows []UpNextRow) string {
	t.Helper()
	var b bytes.Buffer
	if err := dashUpNext(inFlight, rows, "h", "e").Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestAttentionAndUpNextActionsCell: on both tables every row state shows only
// the instrumental icon its state allows, in one wrapper inside the hit-area
// cell, and the flag never renders (these rows carry no lyric).
func TestAttentionAndUpNextActionsCell(t *testing.T) {
	const ret = "/dashboard"
	act := func(a RowActions) RowActions { a.ID, a.Return = 9, ret; return a }
	cases := []struct {
		name   string
		a      RowActions
		titles []string
	}{
		{"plain", act(RowActions{}), []string{"Mark instrumental"}},
		{"manual instrumental", act(RowActions{Manual: true}), []string{"Marked instrumental by hand. Undo"}},
		{"in flight", act(RowActions{InFlight: true}), nil},
		{"no work item id", RowActions{Return: ret}, nil},
		{"no mark routes", RowActions{}, nil},
	}
	surfaces := map[string]func(*testing.T, RowActions) string{
		"attention": func(t *testing.T, a RowActions) string {
			return renderAttention(t, []AttentionRow{{Artist: "A", Title: "T", Actions: a}})
		},
		"up next": func(t *testing.T, a RowActions) string {
			return renderUpNext(t, nil, []UpNextRow{{Position: "1", Artist: "A", Title: "T", Actions: a}})
		},
	}
	for surface, render := range surfaces {
		for _, c := range cases {
			out := render(t, c.a)
			if n := strings.Count(out, `<td class="mx-row-hit-td"><span class="mx-row-cell">`); n != 1 {
				t.Errorf("%s %s: %d hit-area cells, want 1", surface, c.name, n)
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
				t.Errorf("%s %s: links do not address work item 9", surface, c.name)
			}
			if len(c.titles) > 0 && !strings.Contains(out, "?return=%2Fdashboard") {
				t.Errorf("%s %s: return target not encoded", surface, c.name)
			}
			if len(c.titles) == 0 && strings.Contains(out, "/queue/") {
				t.Errorf("%s %s: rendered a mark link", surface, c.name)
			}
			for _, flag := range []string{"Lyrics are wrong", "Unblock"} {
				if strings.Contains(out, flag) {
					t.Errorf("%s %s: rendered the flag (%q)", surface, c.name, flag)
				}
			}
		}
	}
}

// TestActionsHeadersAndInFlightRows: both tables end in an Actions header, and a
// claimed Up Next row keeps the column (an empty reserved cell) with no link.
func TestActionsHeadersAndInFlightRows(t *testing.T) {
	att := renderAttention(t, []AttentionRow{{Artist: "A"}})
	if !strings.Contains(att, `<th scope="col">Last attempt</th><th scope="col">Actions</th>`) {
		t.Errorf("attention: Actions is not the last header: %s", att)
	}
	up := renderUpNext(t, []InFlightRow{{Artist: "C", Title: "Claimed"}}, []UpNextRow{{Position: "1", Artist: "A", Actions: RowActions{ID: 3, Return: "/dashboard"}}})
	if !strings.Contains(up, `<th class="mx-upnext-waited">Waited</th><th>Actions</th>`) {
		t.Errorf("up next: Actions is not the last header: %s", up)
	}
	if n := strings.Count(up, `class="mx-row-hit-td"`); n != 2 {
		t.Errorf("up next: %d action cells, want one per row (claimed + buffered)", n)
	}
	if n := strings.Count(up, "/queue/3/instrumental"); n != 1 {
		t.Errorf("up next: %d instrumental links, want 1 (the claimed row has none)", n)
	}
	// Claimed rows alone still carry the column.
	only := renderUpNext(t, []InFlightRow{{Artist: "C"}}, nil)
	if !strings.Contains(only, "<th>Actions</th>") || strings.Contains(only, "/queue/") {
		t.Errorf("claimed-only table: %s", only)
	}
}
