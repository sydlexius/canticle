package templates

import (
	"bytes"
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func renderCell(t *testing.T, a RowActions) string {
	t.Helper()
	var b bytes.Buffer
	if err := RowActionsCell(a).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestRowActionsStates(t *testing.T) {
	const (
		mark    = "Mark instrumental"
		undo    = "Marked instrumental by hand. Undo"
		wrong   = "Lyrics are wrong"
		unblock = "Lyrics blocked. Unblock"
	)
	cases := []struct {
		name string
		a    RowActions
		want []string
		on   int
	}{
		{"lyric written", RowActions{ID: 7, HasLyric: true}, []string{mark, wrong}, 0},
		{"no lyric", RowActions{ID: 7}, []string{mark}, 0},
		{"manual instrumental", RowActions{ID: 7, Manual: true}, []string{undo}, 1},
		{"blocked", RowActions{ID: 7, Blocked: true}, []string{mark, unblock}, 1},
		{"blocked and re-fetched", RowActions{ID: 7, Blocked: true, HasLyric: true}, []string{mark, unblock}, 1},
		{"in flight", RowActions{ID: 7, HasLyric: true, InFlight: true}, nil, 0},
		{"manual with a lyric file", RowActions{ID: 7, Manual: true, HasLyric: true}, []string{undo}, 1},
		{"blocked and in flight", RowActions{ID: 7, Blocked: true, InFlight: true}, nil, 0},
		{"marking unavailable", RowActions{HasLyric: true}, nil, 0},
	}
	all := []string{mark, undo, wrong, unblock}
	for _, tc := range cases {
		out := renderCell(t, tc.a)
		for _, name := range all {
			want := false
			for _, w := range tc.want {
				want = want || w == name
			}
			got := strings.Contains(out, `<span class="mx-row-sr">`+name+`</span>`) && strings.Contains(out, `title="`+name+`"`)
			if got != want {
				t.Errorf("%s: %q rendered=%v, want %v", tc.name, name, got, want)
			}
		}
		if n := strings.Count(out, "mx-row-icon-on"); n != tc.on {
			t.Errorf("%s: on-chip count = %d, want %d", tc.name, n, tc.on)
		}
		for _, banned := range []string{"<details", "mx-row-pill", "aria-pressed", "<script"} {
			if strings.Contains(out, banned) {
				t.Errorf("%s: emitted %q", tc.name, banned)
			}
		}
		if len(tc.want) == 0 && strings.TrimSpace(out) != "" {
			t.Errorf("%s: expected no markup, got %q", tc.name, out)
		}
		if strings.Contains(out, "<svg") && !strings.Contains(out, `aria-hidden="true"`) {
			t.Errorf("%s: svg not aria-hidden", tc.name)
		}
	}
}

func TestRowActionsHrefs(t *testing.T) {
	a := RowActions{ID: 12, Return: "/queue/finished?sort=x", HasLyric: true}
	out := renderCell(t, a)
	for _, p := range []string{"instrumental", "wrong"} {
		if !strings.Contains(out, `href="/queue/12/`+p+`?return=%2Fqueue%2Ffinished%3Fsort%3Dx"`) {
			t.Errorf("missing href for %s in %s", p, out)
		}
	}
	if !strings.Contains(renderCell(t, RowActions{ID: 12, Manual: true}), `href="/queue/12/instrumental/undo"`) {
		t.Error("undo href missing")
	}
	if !strings.Contains(renderCell(t, RowActions{ID: 12, Blocked: true}), `href="/queue/12/unblock"`) {
		t.Error("unblock href missing")
	}
}

func TestMarkStatus(t *testing.T) {
	if got := MarkStatusFromQuery(url.Values{"mark": {MarkWrongDone}, "files": {"1"}}); !strings.Contains(got, "1 lyric file removed") {
		t.Errorf("status = %q", got)
	}
	for _, bad := range []string{"-5", "1001", "99999999999999999999", "x"} {
		if got := MarkStatusFromQuery(url.Values{"mark": {MarkWrongDone}, "files": {bad}}); got != "Lyrics marked wrong. The track will be looked up again." {
			t.Errorf("files=%s: status = %q, want the count treated as absent", bad, got)
		}
	}
	if got := MarkStatusFromQuery(url.Values{"mark": {MarkInstrumentalDone}, "files": {"1000"}}); !strings.Contains(got, "1000 lyric files") {
		t.Errorf("files=1000 (the bound) = %q", got)
	}
	if got := MarkStatusFromQuery(url.Values{"mark": {MarkUnblocked}, "files": {"7"}}); strings.Contains(got, "7") {
		t.Errorf("a code that reports no files honored files=7: %q", got)
	}
	if MarkStatusFromQuery(url.Values{"mark": {"<script>"}}) != "" {
		t.Error("an unknown code produced text")
	}
	for _, c := range []string{MarkNothingToDo, MarkNotFound, MarkInFlight, MarkNoLyricFile, MarkIsInstrumental, MarkNotMarkable, MarkFailed} {
		if txt := MarkStatusText(c, 0); txt == "" || strings.Contains(txt, "/") {
			t.Errorf("code %s text %q", c, txt)
		}
	}
	var b bytes.Buffer
	if err := MarkStatusLine("").Render(context.Background(), &b); err != nil || b.Len() != 0 {
		t.Error("empty status rendered markup")
	}
}

func TestMarkStatusTextCounts(t *testing.T) {
	cases := []struct {
		code  string
		files int
		want  string
	}{
		{MarkInstrumentalDone, 0, "Marked instrumental."},
		{MarkInstrumentalDone, 1, "Marked instrumental. 1 lyric file removed and saved to a backup file."},
		{MarkInstrumentalDone, 2, "Marked instrumental. 2 lyric files removed and saved to a backup file."},
		{MarkInstrumentalDone, -1, "Marked instrumental."},
		{MarkInstrumentalDone, maxMarkFiles + 1, "Marked instrumental."},
		{MarkInstrumentalDone, maxMarkFiles, "Marked instrumental. 1000 lyric files removed and saved to a backup file."},
		{MarkWrongDone, 0, "Lyrics marked wrong. The track will be looked up again."},
		{MarkWrongDone, 1, "Lyrics marked wrong. 1 lyric file removed and saved to a backup file; the track will be looked up again."},
		{MarkWrongDone, 2, "Lyrics marked wrong. 2 lyric files removed and saved to a backup file; the track will be looked up again."},
		{MarkWrongDone, -7, "Lyrics marked wrong. The track will be looked up again."},
		{MarkWrongDone, 1 << 30, "Lyrics marked wrong. The track will be looked up again."},
	}
	for _, c := range cases {
		if got := MarkStatusText(c.code, c.files); got != c.want {
			t.Errorf("MarkStatusText(%s, %d) = %q, want %q", c.code, c.files, got, c.want)
		}
	}
}

func TestMarkConfirmPageReturnIsLocal(t *testing.T) {
	render := func(ret string, alert string) string {
		var b bytes.Buffer
		v := MarkConfirmView{Title: "T", Track: "A - B", Confirm: "Go", Return: ret, Action: "/queue/1/wrong", CSRFToken: "tok", Alert: alert}
		if err := MarkConfirmPage("v", v, nil, false, false).Render(context.Background(), &b); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	if out := render("/queue?page=2", "refused"); !strings.Contains(out, `href="/queue?page=2">Back</a>`) {
		t.Errorf("alert Back link did not render the local return: %q", out)
	}
	if out := render("/library/3?x=1", ""); !strings.Contains(out, `name="return" value="/library/3?x=1"`) || !strings.Contains(out, `href="/library/3?x=1">Cancel</a>`) {
		t.Errorf("form did not render the local return: %q", out)
	}
	for _, bad := range []string{"https://evil.example/x", "//evil.example", `/\evil`, "javascript:alert(1)", "", "/a\nb", "/a\x00b"} {
		out := render(bad, "refused")
		if !strings.Contains(out, `href="/queue">Back</a>`) {
			t.Errorf("return %q: Back link = %q, want the fallback", bad, out)
		}
		if strings.Contains(out, "evil") {
			t.Errorf("return %q leaked into the alert page", bad)
		}
		out = render(bad, "")
		if !strings.Contains(out, `name="return" value="/queue"`) || !strings.Contains(out, `href="/queue">Cancel</a>`) {
			t.Errorf("return %q: form = %q, want the fallback", bad, out)
		}
		if strings.Contains(out, "evil") || strings.Contains(out, "javascript") {
			t.Errorf("return %q leaked into the form page", bad)
		}
	}
}

func TestMarkConfirmPage(t *testing.T) {
	n := 4
	var b bytes.Buffer
	v := MarkConfirmView{Title: "T", Track: "A - B", Confirm: "Go", Return: "/queue", Action: "/queue/1/wrong", CSRFToken: "tok", Files: &n, Notes: []string{"note"}}
	if err := MarkConfirmPage("v", v, nil, false, false).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, w := range []string{`action="/queue/1/wrong"`, `value="tok"`, "would be removed: 4", "A - B"} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q", w)
		}
	}
	b.Reset()
	v.Alert = "refused"
	_ = MarkConfirmPage("v", v, nil, false, false).Render(context.Background(), &b)
	if strings.Contains(b.String(), `name="csrf_token"`) || !strings.Contains(b.String(), "refused") {
		t.Error("alert page must have no form")
	}
}

func TestRowActionsCellWrapKeepsChildrenOnOneLine(t *testing.T) {
	var b bytes.Buffer
	ctx := templ.WithChildren(context.Background(), templ.Raw(`<a class="mx-run-link">Preview</a>`))
	if err := RowActionsCellWrap().Render(ctx, &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.HasPrefix(out, `<span class="mx-row-cell">`) || !strings.HasSuffix(strings.TrimSpace(out), `</span>`) {
		t.Errorf("wrapper markup = %q", out)
	}
	if !strings.Contains(out, `Preview`) {
		t.Errorf("wrapper dropped its children: %q", out)
	}
	for _, banned := range []string{"<br", "<div", "<details"} {
		if strings.Contains(out, banned) {
			t.Errorf("wrapper emitted %q", banned)
		}
	}
}
