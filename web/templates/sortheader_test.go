package templates

import (
	"context"
	"strings"
	"testing"
)

func renderHeader(t *testing.T, h SortHeaderView) string {
	t.Helper()
	var b strings.Builder
	if err := SortHeader(h).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestSortHeaderRender(t *testing.T) {
	cases := []struct {
		name    string
		h       SortHeaderView
		want    []string
		notWant []string
	}{
		{"plain", SortHeaderView{Label: "Reason"}, []string{"<th>Reason</th>"}, []string{"<a ", "aria-sort"}},
		{"inactive link", SortHeaderView{Label: "Title", Href: "/t?dir=asc&sort=title"},
			[]string{`href="/t?dir=asc&amp;sort=title"`, ">Title"}, []string{"aria-sort", "mx-sort-arrow"}},
		{"ascending", SortHeaderView{Label: "Title", Href: "/t?dir=desc&sort=title", Aria: "ascending"},
			[]string{`aria-sort="ascending"`, "&#9650;", `aria-hidden="true"`}, []string{"&#9660;"}},
		{"descending", SortHeaderView{Label: "Title", Href: "/t?dir=asc&sort=title", Aria: "descending"},
			[]string{`aria-sort="descending"`, "&#9660;"}, []string{"&#9650;"}},
		{"label is escaped", SortHeaderView{Label: "<b>x</b>", Href: "/t"}, []string{"&lt;b&gt;"}, []string{"<b>x"}},
	}
	for _, c := range cases {
		got := renderHeader(t, c.h)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: missing %q in %s", c.name, w, got)
			}
		}
		for _, n := range c.notWant {
			if strings.Contains(got, n) {
				t.Errorf("%s: unexpected %q in %s", c.name, n, got)
			}
		}
	}
}
