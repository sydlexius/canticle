package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func renderQueueRow(t *testing.T, r QueueRow) string {
	t.Helper()
	var b bytes.Buffer
	if err := QueueRows(QueueView{Rows: []QueueRow{r}}).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestQueueRowsActionsCell: the last cell holds the Preview link, the Edited badge
// and the icons in ONE wrapper, in that order, for every row state.
func TestQueueRowsActionsCell(t *testing.T) {
	const ret = "/queue/settled?sort=title"
	act := func(a RowActions) RowActions { a.ID, a.Return = 9, ret; return a }
	cases := []struct {
		name    string
		row     QueueRow
		preview bool
		edited  bool
		titles  []string
	}{
		{"synced", QueueRow{PreviewHref: "/preview/9", PreviewLabel: "Preview", Actions: act(RowActions{HasLyric: true})}, true, false, []string{"Mark instrumental", "Lyrics are wrong"}},
		{"unsynced", QueueRow{Actions: act(RowActions{HasLyric: true})}, false, false, []string{"Mark instrumental", "Lyrics are wrong"}},
		{"pending", QueueRow{Actions: act(RowActions{})}, false, false, []string{"Mark instrumental"}},
		{"failed", QueueRow{Actions: act(RowActions{})}, false, false, []string{"Mark instrumental"}},
		{"manual instrumental", QueueRow{Actions: act(RowActions{Manual: true})}, false, false, []string{"Marked instrumental by hand. Undo"}},
		{"blocked", QueueRow{Actions: act(RowActions{Blocked: true})}, false, false, []string{"Mark instrumental", "Lyrics blocked. Unblock"}},
		{"blocked and re-fetched", QueueRow{PreviewHref: "/preview/9", PreviewLabel: "Preview", Actions: act(RowActions{Blocked: true, HasLyric: true})}, true, false, []string{"Mark instrumental", "Lyrics blocked. Unblock"}},
		{"edited", QueueRow{PreviewHref: "/preview/9", PreviewLabel: "Preview", Edited: true, EditedTitle: "+0.60 s", Actions: act(RowActions{HasLyric: true})}, true, true, []string{"Mark instrumental", "Lyrics are wrong"}},
		{"in flight", QueueRow{Actions: act(RowActions{InFlight: true})}, false, false, nil},
		{"marking unavailable", QueueRow{PreviewHref: "/preview/9", PreviewLabel: "Preview", Edited: true, EditedTitle: "+0.60 s"}, true, true, nil},
	}
	for _, c := range cases {
		out := renderQueueRow(t, c.row)
		if n := strings.Count(out, `<span class="mx-row-cell">`); n != 1 {
			t.Errorf("%s: %d cell wrappers, want 1", c.name, n)
		}
		// The td is the containing block for the icons' 44px hit areas (input.css).
		if n := strings.Count(out, `<td class="mx-row-hit-td">`); n != 1 {
			t.Errorf("%s: %d actions cells with the hit-area class, want 1", c.name, n)
		}
		if got := strings.Contains(out, "mx-queue-preview"); got != c.preview {
			t.Errorf("%s: preview = %v, want %v", c.name, got, c.preview)
		}
		if got := strings.Contains(out, "mx-queue-edited"); got != c.edited {
			t.Errorf("%s: edited = %v, want %v", c.name, got, c.edited)
		}
		if n := strings.Count(out, `class="mx-row-icon`); n != len(c.titles) {
			t.Errorf("%s: %d icons, want %d", c.name, n, len(c.titles))
		}
		for _, title := range c.titles {
			if !strings.Contains(out, `title="`+title+`"`) || !strings.Contains(out, `<span class="mx-row-sr">`+title+`</span>`) {
				t.Errorf("%s: missing %q (tooltip and accessible label)", c.name, title)
			}
		}
		if len(c.titles) > 0 && !strings.Contains(out, "?return=%2Fqueue%2Fsettled%3Fsort%3Dtitle") {
			t.Errorf("%s: return target not encoded into the links: %s", c.name, out)
		}
		// Order inside the cell: Preview, then Edited, then the icons.
		last := -1
		for _, marker := range []string{"mx-queue-preview", "mx-queue-edited", "mx-row-actions"} {
			if i := strings.Index(out, marker); i >= 0 {
				if i < last {
					t.Errorf("%s: %s out of order", c.name, marker)
				}
				last = i
			}
		}
	}
}
