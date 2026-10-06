package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/web/static"
)

// distinctSummary gives every bucket a different count, so a bucket wired to
// another bucket's field shows up as a value mismatch.
var distinctSummary = reports.QueueSummary{
	Pending: 1, Processing: 2, Finished: 3, SettledUpgradable: 4, Failed: 5,
	Deferred: 6, Unavailable: 7, Done: 7, Total: 28,
}

// TestQueueBucketSurfacesAgree is the #599 one-name-per-bucket guard: the
// dashboard tiles and the doughnut's labels carry the same labels in the same
// order, with the same value per label. (The Queue page's agreement with the
// tiles is TestQueueIndexCountsMatchDashboardTiles.)
func TestQueueBucketSurfacesAgree(t *testing.T) {
	tiles := buildQueueTiles(distinctSummary)
	chart := buildQueueChart(distinctSummary)

	var tileLabels, tileValues []string
	for _, tl := range tiles {
		tileLabels = append(tileLabels, tl.Label)
		tileValues = append(tileValues, tl.Value)
	}
	if !slices.Equal(tileLabels, chart.Labels) {
		t.Errorf("tile labels %q != chart labels %q", tileLabels, chart.Labels)
	}
	for i, v := range chart.Values {
		if i < len(tileValues) && tileValues[i] != strconv.FormatFloat(v, 'f', -1, 64) {
			t.Errorf("%q: tile value %s != chart value %v", tileLabels[i], tileValues[i], v)
		}
	}
}

// TestQueueBucketPageHeadingsMatchLabels keeps the /queue/{bucket} drill-down
// on the same name as the tile that links to it (#599: one name per bucket).
func TestQueueBucketPageHeadingsMatchLabels(t *testing.T) {
	for _, b := range queueBuckets {
		info, ok := queueBucketInfo[b.Key]
		if !ok {
			t.Errorf("bucket %q has no drill-down page info", b.Label)
			continue
		}
		if info[0] != b.Label {
			t.Errorf("bucket %q: drill-down heading is %q", b.Label, info[0])
		}
	}
}

// TestQueueBucketTooltips asserts every bucket has its own non-empty tooltip,
// and keeps the #477 Given up guard that used to sit on the templ switch:
// it must not reuse Errored's copy and must name the manual revival command,
// since nothing revives these rows on its own.
func TestQueueBucketTooltips(t *testing.T) {
	seen := map[string]string{}
	byLabel := map[string]string{}
	for _, b := range queueBuckets {
		if strings.TrimSpace(b.Tooltip) == "" {
			t.Errorf("bucket %q has an empty tooltip", b.Label)
		}
		if prev, dup := seen[b.Tooltip]; dup {
			t.Errorf("buckets %q and %q share a tooltip", prev, b.Label)
		}
		seen[b.Tooltip] = b.Label
		byLabel[b.Label] = b.Tooltip
	}
	if !strings.Contains(byLabel["Given up"], "queue recheck --retired") {
		t.Errorf("Given up tooltip %q does not name the manual revival command", byLabel["Given up"])
	}
}

// TestDashboardTileTooltipsRendered asserts the rendered dashboard carries each
// bucket's tooltip on its tile (both the tile and its drill-down link), so the
// templ reads the Go field rather than a label-keyed switch that can drift.
func TestDashboardTileTooltipsRendered(t *testing.T) {
	mux := newReportsUIServer(t, openReportsTestDB(t))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, b := range queueBuckets {
		title := regexp.QuoteMeta(templAttr(b.Tooltip))
		re := regexp.MustCompile(`<div class="mx-dash-tile" role="listitem" title="` + title +
			`"><a class="mx-dash-tile-link" href="` + regexp.QuoteMeta(queueBucketHref(b.Key)) +
			`" title="` + title + `"><span class="mx-dash-tile-label">` + regexp.QuoteMeta(b.Label) + `</span>`)
		if !re.MatchString(body) {
			t.Errorf("dashboard tile %q does not render its tooltip on the tile and link", b.Label)
		}
	}
}

// templAttr mirrors templ's attribute escaping for the characters the
// tooltips use.
func templAttr(s string) string {
	return strings.NewReplacer("&", "&amp;", `"`, "&#34;", "'", "&#39;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// queueColorKeys extracts the keys of the QUEUE_COLOR_VARS object literal from
// chart-init.js: bare identifiers or single/double-quoted strings.
func queueColorKeys(t *testing.T) map[string]bool {
	t.Helper()
	src, err := fs.ReadFile(static.FS, "js/chart-init.js")
	if err != nil {
		t.Fatalf("read chart-init.js: %v", err)
	}
	block := regexp.MustCompile(`(?s)var QUEUE_COLOR_VARS = \{(.*?)\};`).FindSubmatch(src)
	if block == nil {
		t.Fatal("chart-init.js: QUEUE_COLOR_VARS object literal not found")
	}
	entry := regexp.MustCompile(`(?m)^\s*(?:'([^']*)'|"([^"]*)"|([A-Za-z_$][\w$]*))\s*:\s*'--[\w-]+'`)
	keys := map[string]bool{}
	for _, m := range entry.FindAllSubmatch(block[1], -1) {
		keys[string(m[1])+string(m[2])+string(m[3])] = true
	}
	if len(keys) == 0 {
		t.Fatal("chart-init.js: QUEUE_COLOR_VARS parsed to no entries")
	}
	return keys
}

// TestQueueBucketsHaveChartColors asserts every chart label has a
// QUEUE_COLOR_VARS entry. The map is keyed by label, so a rename on the Go side
// alone would otherwise fall back to the accent color silently.
func TestQueueBucketsHaveChartColors(t *testing.T) {
	keys := queueColorKeys(t)
	for _, label := range buildQueueChart(distinctSummary).Labels {
		if !keys[label] {
			t.Errorf("chart label %q has no QUEUE_COLOR_VARS entry in chart-init.js", label)
		}
	}
}

// TestQueueBucketsOrderAndNoProcessing pins the #599 vocabulary: activity first,
// the two settled halves, then Given up, and no Processing bucket on any
// surface (an in-flight row is shown in Up Next instead).
func TestQueueBucketsOrderAndNoProcessing(t *testing.T) {
	want := []string{"Retrying", "Errored", "Queued", "Finished", "Settled (upgradable)", "Given up"}
	var got []string
	for _, b := range queueBuckets {
		got = append(got, b.Label)
		if b.Key == reports.BucketProcessing {
			t.Errorf("bucket %q reads the processing status; in-flight rows belong to Up Next", b.Label)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("bucket labels = %q, want %q", got, want)
	}
	keys := queueColorKeys(t)
	for _, old := range []string{"Processing", "Pending", "Failed", "Deferred", "Unavailable"} {
		if keys[old] {
			t.Errorf("chart-init.js QUEUE_COLOR_VARS still carries retired label %q", old)
		}
	}
}
