package web

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/reports"
)

// seedSearchRow inserts a pending row with app-style normalized keys.
func seedSearchRow(t *testing.T, db *sql.DB, artist, title string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status)
         VALUES (?, ?, ?, ?, 'Invented Album', 'pending')`,
		artist, title, normalize.NormalizeKey(artist), normalize.NormalizeKey(title))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestQueueSearchNarrowsRows(t *testing.T) {
	db := openReportsTestDB(t)
	seedSearchRow(t, db, "Zebra Stand-in", "Alpha Tune")
	seedSearchRow(t, db, "Other Act", "Zebra Song")
	seedSearchRow(t, db, "Other Act", "Beta Tune")
	mux := newReportsUIServer(t, db)

	rec := getQueue(t, mux, "/queue/pending?q=ZEBRA", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Alpha Tune") || !strings.Contains(body, "Zebra Song") {
		t.Errorf("matching rows missing from body")
	}
	if strings.Contains(body, "Beta Tune") {
		t.Errorf("non-matching row rendered under a search")
	}
	if !strings.Contains(body, `value="ZEBRA"`) {
		t.Errorf("search box does not echo the query")
	}
	if !strings.Contains(body, "Clear search") {
		t.Errorf("active search offers no clear link")
	}
}

func TestQueueSearchSurvivesPaging(t *testing.T) {
	db := openReportsTestDB(t)
	total := queuePageSize + 5
	for i := 1; i <= total; i++ {
		seedSearchRow(t, db, "Needle Act", fmt.Sprintf("Pend %03d", i))
		seedSearchRow(t, db, "Haystack", fmt.Sprintf("Other %03d", i))
	}
	mux := newReportsUIServer(t, db)

	rec := getQueue(t, mux, "/queue/pending?q=needle+act", false)
	body := rec.Body.String()
	if strings.Contains(body, "Other 0") {
		t.Fatalf("non-matching rows on page 1")
	}
	m := regexp.MustCompile(`hx-get="(/queue/pending\?[^"]*)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no Show more on a full searched page")
	}
	raw := strings.ReplaceAll(m[1], "&amp;", "&")
	next, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if next.Query().Get("q") != "needle act" || next.Query().Get("after") == "" {
		t.Fatalf("next link = %q, want q and after carried", m[1])
	}
	// href mirrors hx-get so the no-JS fallback keeps the search too.
	if !strings.Contains(body, `href="`+m[1]+`"`) {
		t.Errorf("plain href differs from hx-get %q", m[1])
	}

	frag := getQueue(t, mux, raw, true)
	fb := frag.Body.String()
	if strings.Contains(fb, "<html") {
		t.Errorf("fragment got a full page")
	}
	if got := len(titlesIn(fb)); got != 5 {
		t.Errorf("page 2 rows = %d, want 5 (the matches left over)", got)
	}
	if strings.Contains(fb, "Other 0") || strings.Contains(fb, "Show more") {
		t.Errorf("page 2 leaked non-matches or offered a further page")
	}

	// A full page beyond the first links back to the start with the search kept.
	full := getQueue(t, mux, raw, false).Body.String()
	if !strings.Contains(full, `href="/queue/pending?q=needle+act"`) {
		t.Errorf("Back to start of list lost the search")
	}
}

func TestQueueSearchEmptyState(t *testing.T) {
	db := openReportsTestDB(t)
	seedSearchRow(t, db, "Some Act", "Some Song")
	mux := newReportsUIServer(t, db)

	body := getQueue(t, mux, "/queue/pending?q=nothing-like-this", false).Body.String()
	if !strings.Contains(body, "No tracks match your search") {
		t.Errorf("empty search result lacks the plain empty state")
	}
	if !strings.Contains(body, `href="/queue/pending"`) {
		t.Errorf("empty state offers no clear link to the bare bucket")
	}
	if strings.Contains(body, "No rows in this bucket") {
		t.Errorf("searched empty state used the bucket-empty text")
	}
	// The search box stays so the query can be edited.
	if !strings.Contains(body, `name="q"`) {
		t.Errorf("search box missing on empty result")
	}
}

func TestQueueSearchQueryNeverLogged(t *testing.T) {
	db := openReportsTestDB(t)
	seedSearchRow(t, db, "Some Act", "Some Song")
	mux := newReportsUIServer(t, db)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const secret = "zq-secret-needle-7731"
	for _, target := range []string{
		"/queue/pending?q=" + secret,
		"/queue/pending?q=" + secret + "&after=bad",
		"/queue/pending?q=" + secret + "&q=again",
		"/queue/pending?q=" + strings.Repeat("x", maxQueueQueryRunes+1) + secret,
	} {
		getQueue(t, mux, target, false)
		getQueue(t, mux, target, true)
	}
	if buf.Len() == 0 {
		t.Fatalf("no log output captured; the test would pass vacuously")
	}
	if strings.Contains(buf.String(), secret) {
		t.Errorf("query text reached the logs:\n%s", buf.String())
	}
}

func TestQueueSearchLengthCap(t *testing.T) {
	db := openReportsTestDB(t)
	seedSearchRow(t, db, "Some Act", "Some Song")
	mux := newReportsUIServer(t, db)

	ok := strings.Repeat("a", maxQueueQueryRunes)
	if rec := getQueue(t, mux, "/queue/pending?q="+ok, false); rec.Code != http.StatusOK {
		t.Errorf("query at the cap: status %d, want 200", rec.Code)
	}
	// Counted in runes, not bytes: 200 two-byte runes is still within the cap.
	multi := strings.Repeat("é", maxQueueQueryRunes)
	if rec := getQueue(t, mux, "/queue/pending?q="+multi, false); rec.Code != http.StatusOK {
		t.Errorf("200 multibyte runes: status %d, want 200", rec.Code)
	}
	if rec := getQueue(t, mux, "/queue/pending?q="+ok+"a", false); rec.Code != http.StatusBadRequest {
		t.Errorf("query over the cap: status %d, want 400", rec.Code)
	}
}

func TestQueueViewStateRoundTrip(t *testing.T) {
	cases := []struct {
		raw   string
		want  queueViewState
		href  string
		after string
	}{
		{"", queueViewState{}, "/queue/pending", ""},
		{"q=a+b%26c", queueViewState{Query: "a b&c"}, "/queue/pending?q=a+b%26c", ""},
		{"q=x&after=7:n", queueViewState{Query: "x", After: "7:n"}, "/queue/pending?after=9%3Atab&q=x", "9:tab"},
		{"sort=artist&dir=desc&sort2=x", queueViewState{Sort: "artist", Dir: "desc"}, "/queue/pending?dir=desc&sort=artist", ""},
		{"sort=id%3Bdrop&dir=sideways", queueViewState{}, "/queue/pending", ""},
		{"sort=status&dir=desc", queueViewState{Dir: "desc"}, "/queue/pending?dir=desc", ""}, // in the vocabulary, not in the spec
	}
	for _, c := range cases {
		v, _ := url.ParseQuery(c.raw)
		got, err := parseQueueViewState(v, reports.BucketSpec(reports.BucketPending))
		if err != nil || got != c.want {
			t.Errorf("parse(%q) = %+v, %v; want %+v", c.raw, got, err, c.want)
		}
		if h := got.href("pending", c.after); h != c.href {
			t.Errorf("href(%q) = %q, want %q", c.raw, h, c.href)
		}
	}
	for _, bad := range []string{"q=a&q=b", "after=1&after=2", "sort=a&sort=b", "dir=asc&dir=desc"} {
		v, _ := url.ParseQuery(bad)
		if _, err := parseQueueViewState(v, reports.BucketSpec(reports.BucketPending)); err == nil {
			t.Errorf("parse(%q) accepted, want error", bad)
		}
	}
}

// A query that normalizes to nothing is no search: the page shows the ordinary
// bucket with no active-search state (#1234 review).
func TestQueueWhitespaceQueryIsNoSearch(t *testing.T) {
	db := openReportsTestDB(t)
	seedSearchRow(t, db, "Some Act", "Some Song")
	mux := newReportsUIServer(t, db)

	body := getQueue(t, mux, "/queue/pending?q=%20", false).Body.String()
	if !strings.Contains(body, "Some Song") {
		t.Errorf("whitespace query filtered the bucket")
	}
	if strings.Contains(body, "Clear search") {
		t.Errorf("whitespace query shows the active-search clear link")
	}
	if strings.Contains(body, `value=" "`) {
		t.Errorf("search box echoes the whitespace query")
	}
}

func TestQueueWhitespaceQueryEmptyBucketIsOrdinary(t *testing.T) {
	db := openReportsTestDB(t)
	mux := newReportsUIServer(t, db)

	body := getQueue(t, mux, "/queue/pending?q=%20%20", false).Body.String()
	if strings.Contains(body, "No tracks match your search") {
		t.Errorf("whitespace query on an empty bucket claims a search")
	}
	if !strings.Contains(body, "No rows in this bucket") {
		t.Errorf("ordinary empty state missing")
	}
}

// The HTML maxlength counts UTF-16 units and would block queries the server's
// rune cap accepts; the server is the single authority.
func TestQueueSearchInputHasNoMaxlength(t *testing.T) {
	db := openReportsTestDB(t)
	mux := newReportsUIServer(t, db)
	body := getQueue(t, mux, "/queue/pending", false).Body.String()
	if strings.Contains(body, "maxlength") {
		t.Errorf("search input carries a maxlength that disagrees with the server's rune cap")
	}
}
