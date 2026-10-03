package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/reports"
)

// seedChipPage inserts done rows ("Done 001".."Done 006") with distinct chip
// traits: 001 line, 002 line+edited, 003 word, 004 word+edited, 005 mis-synced
// word, 006 plain text.
func seedChipPage(t *testing.T, db *sql.DB) {
	t.Helper()
	rows := []struct{ outcome, tier, timing, edited string }{
		{"synced", "line", "", ""}, {"synced", "line", "", "x"}, {"synced", "word", "", ""},
		{"synced", "word", "", "x"}, {"synced", "word", "mis_synced", ""}, {"unsynced", "", "", ""},
	}
	for i, r := range rows {
		title := fmt.Sprintf("Done %03d", i+1)
		_, err := db.ExecContext(context.Background(),
			`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, outcome_type, sync_tier, timing_outcome, lyric_edited_at, updated_at)
             VALUES ('Chip Artist', ?, 'chip artist', ?, 'Album', 'done', ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?)`,
			title, strings.ToLower(title), r.outcome, r.tier, r.timing, r.edited, fmt.Sprintf("2026-01-0%dT00:00:00Z", i+1))
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestQueueChipsFilterRowsAndCombine(t *testing.T) {
	db := openReportsTestDB(t)
	seedChipPage(t, db)
	mux := newReportsUIServer(t, db)
	for target, want := range map[string]string{
		"/queue/settled":                          "[Done 006 Done 005 Done 002 Done 001]",
		"/queue/settled?tier=line":                "[Done 002 Done 001]",
		"/queue/settled?tier=line&edited=1":       "[Done 002]",
		"/queue/settled?missync=1":                "[Done 005]",
		"/queue/settled?edited=1":                 "[Done 002]",
		"/queue/finished":                         "[Done 004 Done 003]",
		"/queue/finished?edited=1":                "[Done 004]",
		"/queue/settled?tier=line&sort=title":     "[Done 001 Done 002]",
		"/queue/settled?tier=line&q=chip&dir=asc": "[Done 001 Done 002]",
		"/queue/settled?tier=line&q=nomatch":      "[]",
		// A URL carrying both exclusive chips resolves to Mis-synced.
		"/queue/settled?tier=line&missync=1": "[Done 005]",
		// Chips the bucket does not offer are ignored: Finished has no Line-synced,
		// Mis-synced or Word-synced chip, and nothing offers a word tier.
		"/queue/finished?tier=line":          "[Done 004 Done 003]",
		"/queue/finished?missync=1":          "[Done 004 Done 003]",
		"/queue/finished?tier=word":          "[Done 004 Done 003]",
		"/queue/finished?tier=line&edited=1": "[Done 004]",
		"/queue/settled?tier=word":           "[Done 006 Done 005 Done 002 Done 001]",
		"/queue/settled?tier=word&edited=1":  "[Done 002]",
		"/queue/settled?tier=word&missync=1": "[Done 005]",
	} {
		if got := fmt.Sprint(orderOf(t, mux, target)); got != want {
			t.Errorf("GET %s = %s, want %s", target, got, want)
		}
	}
}

func TestQueueChipsUnknownValueIgnored(t *testing.T) {
	db := openReportsTestDB(t)
	seedChipPage(t, db)
	mux := newReportsUIServer(t, db)
	all := fmt.Sprint(orderOf(t, mux, "/queue/settled"))
	for _, target := range []string{
		"/queue/settled?tier=bogus", "/queue/settled?tier=", "/queue/settled?edited=yes", "/queue/settled?missync=2",
		"/queue/settled?tier=line%27%3B--&edited=1%27",
	} {
		rec := getQueue(t, mux, target, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", target, rec.Code)
		}
		if got := fmt.Sprint(titlesIn(rec.Body.String())); got != all {
			t.Errorf("GET %s = %s, want unfiltered %s", target, got, all)
		}
	}
	// A bucket with no chips drops them rather than filtering or erroring.
	seedSortRows(t, db, "pending", [][3]string{{"Pend 001", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"}})
	rec := getQueue(t, mux, "/queue/pending?tier=line&missync=1", false)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Pend 001") {
		t.Errorf("chip params on a non-chip bucket must be ignored (status %d)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "mx-queue-chips") {
		t.Error("non-chip bucket rendered the chip row")
	}
}

// A repeated chip param is ambiguous (400) only on a bucket that offers that
// chip; elsewhere it is ignored like any other chip param.
func TestQueueChipsRepeatedParam(t *testing.T) {
	db := openReportsTestDB(t)
	seedChipPage(t, db)
	seedSortRows(t, db, "pending", [][3]string{{"Pend 001", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"}})
	mux := newReportsUIServer(t, db)
	for target, want := range map[string]int{
		"/queue/settled?tier=line&tier=word":  http.StatusBadRequest,
		"/queue/settled?edited=1&edited=1":    http.StatusBadRequest,
		"/queue/settled?missync=1&missync=1":  http.StatusBadRequest,
		"/queue/finished?edited=1&edited=1":   http.StatusBadRequest,
		"/queue/finished?tier=line&tier=word": http.StatusOK,
		"/queue/finished?missync=1&missync=1": http.StatusOK,
		"/queue/pending?tier=line&tier=word":  http.StatusOK,
		"/queue/pending?edited=1&edited=1":    http.StatusOK,
		"/queue/pending?missync=1&missync=1":  http.StatusOK,
		"/queue/pending?q=a&q=b":              http.StatusBadRequest,
	} {
		if rec := getQueue(t, mux, target, false); rec.Code != want {
			t.Errorf("GET %s = %d, want %d", target, rec.Code, want)
		}
	}
}

var chipAnchorRE = regexp.MustCompile(`<a class="mx-queue-chip( is-active)?" href="([^"]*)"( aria-current="true")?>([^<]*)</a>`)

// chipsOf parses the rendered chip row into label -> match groups.
func chipsOf(body string) map[string][]string {
	chips := map[string][]string{}
	for _, m := range chipAnchorRE.FindAllStringSubmatch(body, -1) {
		chips[html2(m[4])] = m
	}
	return chips
}

func chipLabels(body string) []string {
	var out []string
	for _, m := range chipAnchorRE.FindAllStringSubmatch(body, -1) {
		out = append(out, html2(m[4]))
	}
	return out
}

func chipQuery(t *testing.T, m []string) url.Values {
	t.Helper()
	u, err := url.Parse(html2(m[2]))
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

// The chip set is per bucket: Finished offers Hand-edited only, Settled offers
// Line-synced, Hand-edited and Mis-synced, in that order, and no other bucket any.
func TestQueueChipsPerBucketSet(t *testing.T) {
	db := openReportsTestDB(t)
	seedChipPage(t, db)
	seedSortRows(t, db, "pending", [][3]string{{"Pend 001", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z"}})
	mux := newReportsUIServer(t, db)
	for target, want := range map[string]string{
		"/queue/finished": "[Hand-edited]",
		"/queue/settled":  "[Line-synced (editable) Hand-edited Mis-synced]",
		"/queue/pending":  "[]",
	} {
		if got := fmt.Sprint(chipLabels(getQueue(t, mux, target, false).Body.String())); got != want {
			t.Errorf("GET %s chips = %s, want %s", target, got, want)
		}
	}
}

func TestQueueChipsRenderActiveStateAndCarryState(t *testing.T) {
	db := openReportsTestDB(t)
	seedChipPage(t, db)
	mux := newReportsUIServer(t, db)
	body := getQueue(t, mux, "/queue/settled?tier=line&edited=1&q=chip&sort=title&dir=asc", false).Body.String()
	chips := chipsOf(body)
	if len(chips) != 3 {
		t.Fatalf("rendered %d chips, want 3: %v", len(chips), chips)
	}
	active := map[string]bool{"Line-synced (editable)": true, "Hand-edited": true}
	for label, m := range chips {
		if (m[1] != "") != active[label] || (m[3] != "") != active[label] {
			t.Errorf("chip %q active=%q aria=%q, want active=%v", label, m[1], m[3], active[label])
		}
		q := chipQuery(t, m)
		if q.Get("q") != "chip" || q.Get("sort") != "title" || q.Get("dir") != "asc" || q.Get("after") != "" {
			t.Errorf("chip %q link %q lost search/sort or kept a cursor", label, m[2])
		}
	}
	// Toggling: the active Line chip's link clears the tier; Mis-synced adds itself
	// (and turns Line off); Hand-edited clears itself and keeps the tier.
	check := func(label, key, want string) {
		if got := chipQuery(t, chips[label]).Get(key); got != want {
			t.Errorf("chip %q link %s=%q, want %q", label, key, got, want)
		}
	}
	check("Line-synced (editable)", "tier", "")
	check("Line-synced (editable)", "edited", "1")
	check("Mis-synced", "missync", "1")
	check("Mis-synced", "tier", "")
	check("Mis-synced", "edited", "1")
	check("Hand-edited", "edited", "")
	check("Hand-edited", "tier", "line")
	// The sort header links and the search form keep the chips.
	for _, want := range []string{`name="tier" value="line"`, `name="edited" value="1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("search form lost %s", want)
		}
	}
	if strings.Contains(body, `name="missync"`) {
		t.Error("search form carries an inactive missync chip")
	}
	hdrs := regexp.MustCompile(`class="mx-sort-link" href="([^"]*)"`).FindAllStringSubmatch(body, -1)
	if len(hdrs) == 0 {
		t.Fatal("no sortable headers rendered")
	}
	for _, m := range hdrs {
		if h := html2(m[1]); !strings.Contains(h, "tier=line") || !strings.Contains(h, "edited=1") || !strings.Contains(h, "q=chip") {
			t.Errorf("sort header link %q drops the filters", h)
		}
	}
	// The search form re-submits the Mis-synced chip when it is the active one.
	body = getQueue(t, mux, "/queue/settled?missync=1&q=chip", false).Body.String()
	if !strings.Contains(body, `name="missync" value="1"`) {
		t.Error("search form lost name=\"missync\" value=\"1\"")
	}
}

// Line-synced and Mis-synced switch each other off, in the generated links and
// when a URL carries both.
func TestQueueChipsMutuallyExclusive(t *testing.T) {
	db := openReportsTestDB(t)
	seedChipPage(t, db)
	mux := newReportsUIServer(t, db)

	body := getQueue(t, mux, "/queue/settled?missync=1", false).Body.String()
	chips := chipsOf(body)
	if chips["Mis-synced"][1] == "" || chips["Line-synced (editable)"][1] != "" {
		t.Error("only Mis-synced should be active on ?missync=1")
	}
	if q := chipQuery(t, chips["Line-synced (editable)"]); q.Get("tier") != "line" || q.Get("missync") != "" {
		t.Errorf("Line chip link %v must turn Mis-synced off", q)
	}

	body = getQueue(t, mux, "/queue/settled?tier=line", false).Body.String()
	chips = chipsOf(body)
	if q := chipQuery(t, chips["Mis-synced"]); q.Get("missync") != "1" || q.Get("tier") != "" {
		t.Errorf("Mis-synced chip link %v must turn Line-synced off", q)
	}

	// Both in the URL: Mis-synced wins, deterministically, and only it is active.
	body = getQueue(t, mux, "/queue/settled?tier=line&missync=1", false).Body.String()
	chips = chipsOf(body)
	if chips["Mis-synced"][1] == "" || chips["Line-synced (editable)"][1] != "" {
		t.Error("tier=line&missync=1 must resolve to Mis-synced only")
	}
	if strings.Contains(body, `name="tier"`) {
		t.Error("search form carries the dropped tier")
	}
	v, _ := url.ParseQuery("tier=line&missync=1")
	s, err := parseQueueViewState(v, reports.BucketSettled)
	if err != nil || s.Tier != "" || !s.MisSynced {
		t.Errorf("state for tier=line&missync=1 = %+v, %v; want MisSynced only", s, err)
	}
}

// seedChipBulk adds n Settled line-synced rows ("Done 001"...) plus three more
// (numbered past n) that match the "bulk" search but are not line-synced, so only
// the chip excludes them.
func seedChipBulk(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	for i := 1; i <= n+3; i++ {
		title := fmt.Sprintf("Done %03d", i)
		outcome, tier := "synced", "line"
		if i > n {
			outcome, tier = "unsynced", ""
		}
		_, err := db.ExecContext(context.Background(),
			`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, outcome_type, sync_tier, updated_at)
             VALUES ('Bulk Artist', ?, 'bulk artist', ?, 'Album', 'done', ?, NULLIF(?, ''), ?)`,
			title, strings.ToLower(title), outcome, tier, fmt.Sprintf("2026-02-01T%02d:%02d:00Z", i/60, i%60))
		if err != nil {
			t.Fatal(err)
		}
	}
}

var (
	moreHrefRE = regexp.MustCompile(`hx-get="(/queue/settled\?[^"]*)"`)
	clearRE    = regexp.MustCompile(`<a class="mx-queue-link" href="([^"]*)">Clear search</a>`)
	startRE    = regexp.MustCompile(`<a class="mx-queue-link" href="([^"]*)">Back to start of list</a>`)
)

// Driving a second page with chip, search and sort active: every link on it
// keeps the chip, the chip links drop the live cursor, and the walk returns each
// matching row once.
func TestQueueChipsSecondPage(t *testing.T) {
	db := openReportsTestDB(t)
	n := queuePageSize*2 + 3
	seedChipBulk(t, db, n)
	mux := newReportsUIServer(t, db)

	seen := map[string]int{}
	target := "/queue/settled?tier=line&q=bulk&sort=title&dir=asc"
	pages := 0
	var body string
	for ; target != "" && pages < 6; pages++ {
		body = getQueue(t, mux, target, false).Body.String()
		for _, title := range titlesIn(body) {
			seen[title]++
		}
		m := moreHrefRE.FindStringSubmatch(body)
		if pages == 1 { // the second page, reached through a live cursor
			if !strings.Contains(html2(target), "after=") {
				t.Fatalf("page 2 target %q carries no cursor", target)
			}
			for label, c := range chipsOf(body) {
				if q := chipQuery(t, c); q.Get("after") != "" || q.Get("q") != "bulk" || q.Get("sort") != "title" {
					t.Errorf("page 2 chip %q link %v keeps the cursor or loses search/sort", label, q)
				}
			}
			if h := html2(clearRE.FindStringSubmatch(body)[1]); !strings.Contains(h, "tier=line") || strings.Contains(h, "q=") || strings.Contains(h, "after=") {
				t.Errorf("Clear search link %q must keep the chip, drop q and the cursor", h)
			}
			if h := html2(startRE.FindStringSubmatch(body)[1]); !strings.Contains(h, "tier=line") || strings.Contains(h, "after=") {
				t.Errorf("Back to start link %q must keep the chip and drop the cursor", h)
			}
		}
		if m == nil {
			pages++ // the loop's own increment is skipped by this break
			break
		}
		more := html2(m[1])
		for _, want := range []string{"tier=line", "q=bulk", "sort=title", "dir=asc", "after="} {
			if !strings.Contains(more, want) {
				t.Errorf("Show more link %q lacks %s", more, want)
			}
		}
		target = more
	}
	if len(seen) != n {
		t.Fatalf("walk returned %d distinct rows, want %d", len(seen), n)
	}
	for title, c := range seen {
		if num, _ := strconv.Atoi(strings.TrimPrefix(title, "Done ")); c != 1 || num > n {
			t.Errorf("row %q seen %d times", title, c)
		}
	}
	if pages != 3 {
		t.Errorf("walked %d pages, want 3", pages)
	}
}

func TestQueueChipsEmptyStates(t *testing.T) {
	db := openReportsTestDB(t)
	mux := newReportsUIServer(t, db)
	const (
		chipOnly = "No tracks match the selected filters."
		search   = "No tracks match your search."
		combined = "No tracks match your search and the selected filters."
		none     = "No rows in this bucket."
	)
	for target, want := range map[string]string{
		"/queue/settled":                  none,
		"/queue/settled?edited=1":         chipOnly,
		"/queue/settled?q=zzzz":           search,
		"/queue/settled?edited=1&q=zzzz":  combined,
		"/queue/settled?missync=1&q=zzzz": combined,
		"/queue/finished?edited=1&q=zzzz": combined,
	} {
		body := getQueue(t, mux, target, false).Body.String()
		if !strings.Contains(body, want) {
			t.Errorf("GET %s lacks %q", target, want)
		}
		for _, other := range []string{chipOnly, search, combined, none} {
			if other != want && !strings.Contains(want, other) && strings.Contains(body, other) {
				t.Errorf("GET %s also says %q", target, other)
			}
		}
	}
	// The combined message keeps the Clear-the-search affordance, and it keeps the chip.
	body := getQueue(t, mux, "/queue/settled?edited=1&q=zzzz", false).Body.String()
	m := regexp.MustCompile(`<a class="mx-queue-link" href="([^"]*)">Clear the search</a>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("combined empty state lost the Clear the search link")
	}
	if h := html2(m[1]); !strings.Contains(h, "edited=1") || strings.Contains(h, "q=") {
		t.Errorf("Clear the search link %q must keep the chip and drop q", h)
	}
}

func TestQueueChipsStateRoundTrip(t *testing.T) {
	v, _ := url.ParseQuery("tier=line&edited=1&q=x&sort=title&dir=desc")
	s, err := parseQueueViewState(v, reports.BucketSettled)
	if err != nil {
		t.Fatal(err)
	}
	got := s.backLinkState().href("settled", "")
	for _, want := range []string{"tier=line", "edited=1", "q=x", "sort=title", "dir=desc"} {
		if !strings.Contains(got, want) {
			t.Errorf("back link %q lacks %s", got, want)
		}
	}
	row := reports.BucketRow{ID: 7, Previewable: true}
	if h := queuePreviewHref(row, reports.BucketSettled, s); !strings.Contains(h, "tier=line") || !strings.Contains(h, "from=settled") {
		t.Errorf("preview href %q does not carry the chips", h)
	}
	// On Finished the chips the bucket does not offer never enter the state.
	v, _ = url.ParseQuery("tier=line&edited=1&missync=1")
	f, err := parseQueueViewState(v, reports.BucketFinished)
	if err != nil || f.Tier != "" || f.MisSynced || !f.Edited {
		t.Errorf("finished state = %+v, %v; want Edited only", f, err)
	}
	if h := f.href("finished", ""); h != "/queue/finished?edited=1" {
		t.Errorf("finished href = %q", h)
	}
}

// html2 unescapes the few entities templ writes into attribute values.
func html2(s string) string {
	return strings.NewReplacer("&amp;", "&", "&#39;", "'", "&#34;", `"`).Replace(s)
}
