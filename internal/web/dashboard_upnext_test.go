package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
)

func TestTierLabel(t *testing.T) {
	// PriorityMiss=-100 -> miss; PriorityUpgrade=-50 -> upgrade; PriorityScan=0 and PriorityWebhook=10 -> fresh.
	tests := []struct {
		priority int
		want     string
	}{
		{-100, "miss"},
		{-50, "upgrade"},
		{-1, "miss"},
		{0, "fresh"},
		{10, "fresh"},
	}
	for _, tc := range tests {
		if got := tierLabel(tc.priority); got != tc.want {
			t.Errorf("tierLabel(%d) = %q, want %q", tc.priority, got, tc.want)
		}
	}
}

func TestFormatWaited(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		since time.Time
		want  string
	}{
		{"zero", time.Time{}, "0s"},
		{"future clamps to 0s", now.Add(time.Hour), "0s"},
		{"seconds", now.Add(-30 * time.Second), "30s"},
		{"minutes", now.Add(-2 * time.Minute), "2m"},
		{"hours", now.Add(-5 * time.Hour), "5h"},
		{"days", now.Add(-6 * 24 * time.Hour), "6d"},
		{"just under a day is hours", now.Add(-23 * time.Hour), "23h"},
	}
	for _, tc := range tests {
		if got := formatWaited(tc.since, now); got != tc.want {
			t.Errorf("%s: formatWaited = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestGroupThousands(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{42, "42"},
		{999, "999"},
		{1000, "1,000"},
		{10102, "10,102"},
		{1234567, "1,234,567"},
		{-15556, "-15,556"},
	}
	for _, tc := range tests {
		if got := groupThousands(tc.n); got != tc.want {
			t.Errorf("groupThousands(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestBuildUpNextRows(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	items := []reports.UpNextItem{
		{Artist: "Test Artist 1", Title: "Track Alpha", Album: "Album One", Priority: -100, CreatedAt: now.Add(-6 * 24 * time.Hour)},
		{Artist: "Test Artist 2", Title: "Track Beta", Priority: 0, CreatedAt: now.Add(-2 * time.Minute)},
	}
	rows := buildUpNextRows(items, now)
	if len(rows) != 2 {
		t.Fatalf("buildUpNextRows returned %d rows, want 2", len(rows))
	}
	if rows[0].Position != "1" || rows[1].Position != "2" {
		t.Errorf("positions = %q, %q; want 1, 2", rows[0].Position, rows[1].Position)
	}
	if rows[0].Artist != "Test Artist 1" || rows[0].Title != "Track Alpha" || rows[0].Album != "Album One" {
		t.Errorf("row 0 identity = %q/%q/%q; want Test Artist 1/Track Alpha/Album One", rows[0].Artist, rows[0].Title, rows[0].Album)
	}
	if rows[0].Tier != "miss" || rows[1].Tier != "fresh" {
		t.Errorf("tiers = %q, %q; want miss, fresh", rows[0].Tier, rows[1].Tier)
	}
	if rows[0].Waited != "6d" || rows[1].Waited != "2m" {
		t.Errorf("waited = %q, %q; want 6d, 2m", rows[0].Waited, rows[1].Waited)
	}
}

// insertBuffered seeds one buffered work_queue row (batch_seq set) with synthetic
// identity, for the Up-next panel handler test. Synthetic values only (#572 AC).
func insertBuffered(t *testing.T, sqlDB *sql.DB, artist, title, album, status string, priority, batchSeq int) {
	t.Helper()
	var seq any // NULL (unbuffered) when batchSeq <= 0; the draw stamps 1..N.
	if batchSeq > 0 {
		seq = batchSeq
	}
	_, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO work_queue
            (artist, title, artist_key, title_key, album, status, last_error,
             priority, batch_seq)
         VALUES (?, ?, ?, ?, ?, ?, '', ?, ?)`,
		artist, title, artist, title, album, status, priority, seq)
	if err != nil {
		t.Fatalf("insert buffered work_queue: %v", err)
	}
}

// TestHandleDashboard_UpNextPanel verifies the panel renders buffered rows in
// batch_seq order, placed between Lyrics Sources and Recent Outcomes.
func TestHandleDashboard_UpNextPanel(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	// Stamped out of insertion order to prove ordering is by batch_seq.
	insertBuffered(t, sqlDB, "Test Artist 3", "Track Gamma", "Album Three", "pending", 0, 3)
	insertBuffered(t, sqlDB, "Test Artist 1", "Track Alpha", "Album One", "failed", -100, 1)
	insertBuffered(t, sqlDB, "Test Artist 2", "Track Beta", "Album Two", "deferred", 0, 2)
	insertBuffered(t, sqlDB, "Test Artist 4", "Track Delta", "Album Four", "pending", -50, 4) // upgrade trip (#1151)

	mux := newReportsUIServer(t, sqlDB)
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "Up Next") {
		t.Error("dashboard missing Up Next heading")
	}
	if !strings.Contains(body, "4 buffered of 4 eligible") {
		t.Errorf("dashboard missing buffered/eligible header; body has: %s", excerptAround(body, "buffered"))
	}
	// Nothing claimed: the table keeps its upcoming-only label and tooltip.
	if !strings.Contains(body, `aria-label="Upcoming queue work"`) || strings.Contains(body, "processing now") {
		t.Error("buffered-only table must keep the upcoming-work label and tooltip")
	}
	// Rows appear in batch_seq order: Alpha (1) < Beta (2) < Gamma (3).
	iAlpha := strings.Index(body, "Track Alpha")
	iBeta := strings.Index(body, "Track Beta")
	iGamma := strings.Index(body, "Track Gamma")
	if iAlpha < 0 || iBeta < 0 || iGamma < 0 {
		t.Fatalf("missing a buffered track: alpha=%d beta=%d gamma=%d", iAlpha, iBeta, iGamma)
	}
	if iAlpha >= iBeta || iBeta >= iGamma {
		t.Errorf("rows out of batch_seq order: alpha=%d beta=%d gamma=%d", iAlpha, iBeta, iGamma)
	}
	// Album renders in its own column.
	if !strings.Contains(body, "Album One") {
		t.Error("dashboard Up-next panel missing album value")
	}
	for _, h := range []string{">Artist<", ">Title<", ">Album<"} {
		if !strings.Contains(body, h) {
			t.Errorf("Up-next table missing distinct column header %q", h)
		}
	}
	// Panel is placed between Lyrics Sources and Recent Outcomes.
	iSources := strings.Index(body, "Lyrics Sources")
	iUpNext := strings.Index(body, "Up Next")
	iRecent := strings.Index(body, "Recent Outcomes")
	if iSources >= iUpNext || iUpNext >= iRecent {
		t.Errorf("Up Next misplaced: sources=%d upnext=%d recent=%d", iSources, iUpNext, iRecent)
	}
	// The miss-tier badge renders for the deferred benign-miss row.
	if !strings.Contains(body, "mx-upnext-tier-miss") {
		t.Error("dashboard missing miss-tier badge class")
	}
	// The upgrade-trip row gets its own badge, not the fresh fallback (#1151).
	if !strings.Contains(body, `mx-upnext-tier mx-upnext-tier-upgrade">upgrade<`) {
		t.Errorf("dashboard missing upgrade-tier badge; body has: %s", excerptAround(body, "Track Delta"))
	}
}

// TestHandleDashboard_UpNextCappedCount verifies the header reports the TRUE
// buffered count even when the buffer exceeds the display cap
// (dashboardUpNextLimit): the count comes from the DB, not the capped row slice
// (#572 CR). Discriminates: a len(rows)-based header would read "50 buffered".
func TestHandleDashboard_UpNextCappedCount(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	total := dashboardUpNextLimit + 1 // one past the display cap
	for i := 1; i <= total; i++ {
		insertBuffered(t, sqlDB, "Test Artist", "Track "+strconv.Itoa(i), "Album", "pending", 0, i)
	}

	mux := newReportsUIServer(t, sqlDB)
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	want := strconv.Itoa(total) + " buffered of " + strconv.Itoa(total) + " eligible"
	if !strings.Contains(body, want) {
		t.Errorf("header must report true buffered count %q; got: %s", want, excerptAround(body, "buffered of"))
	}
	// The displayed table is still capped at the display limit.
	if n := strings.Count(body, `class="mx-cell-mono mx-upnext-pos"`); n != dashboardUpNextLimit {
		t.Errorf("displayed rows = %d, want %d (display cap)", n, dashboardUpNextLimit)
	}
}

// TestHandleDashboard_UpNextEmpty verifies the empty state shows counts with no
// ordering claim when nothing is buffered.
func TestHandleDashboard_UpNextEmpty(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	// An unbuffered pending row: eligible, but not in the lookahead buffer.
	insertBuffered(t, sqlDB, "Test Artist U", "Unbuffered", "Album U", "pending", 0, 0)

	mux := newReportsUIServer(t, sqlDB)
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Up Next") {
		t.Error("dashboard missing Up Next heading in empty state")
	}
	if !strings.Contains(body, "Nothing buffered.") {
		t.Errorf("empty state missing counts-only line; got: %s", excerptAround(body, "Up Next"))
	}
	// No ordered table when the buffer is empty.
	if strings.Contains(body, `class="mx-table" aria-label=`) {
		t.Error("empty state must not render the ordered table")
	}
}

// insertProcessing inserts a processing row with an explicit claimed_at ("" =
// unknown: claimed_at NULL and updated_at blank, which needs the updated_at
// trigger dropped since it refires on any UPDATE).
func insertProcessing(t *testing.T, sqlDB *sql.DB, artist, title, claimedAt string) {
	t.Helper()
	ctx := context.Background()
	insertBuffered(t, sqlDB, artist, title, "Album "+title, "processing", 0, 0)
	stmts := []string{`UPDATE work_queue SET claimed_at = ? WHERE title = ?`}
	args := []any{claimedAt, title}
	if claimedAt == "" {
		if _, err := sqlDB.ExecContext(ctx, `DROP TRIGGER IF EXISTS update_work_queue_updated_at`); err != nil {
			t.Fatalf("drop trigger: %v", err)
		}
		stmts = []string{`UPDATE work_queue SET claimed_at = NULL, updated_at = '' WHERE title = ?`}
		args = []any{title}
	}
	if _, err := sqlDB.ExecContext(ctx, stmts[0], args...); err != nil {
		t.Fatalf("stamp claimed_at: %v", err)
	}
}

func dashboardBody(t *testing.T, sqlDB *sql.DB) string {
	t.Helper()
	rec := httptest.NewRecorder()
	newReportsUIServer(t, sqlDB).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d", rec.Code)
	}
	return rec.Body.String()
}

// TestHandleDashboard_UpNextInFlightClaimed claims a row through the real queue
// and confirms it stays visible in Up Next, above the buffered rows, as a live
// claim with no stuck badge.
func TestHandleDashboard_UpNextInFlightClaimed(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	insertBuffered(t, sqlDB, "Artist Live", "Track Live", "Album Live", "pending", 0, 1)
	insertBuffered(t, sqlDB, "Artist Wait", "Track Wait", "Album Wait", "pending", 0, 2)
	item, err := queue.NewDBQueue(sqlDB).Dequeue(context.Background())
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if item.Inputs.Track.ArtistName != "Artist Live" {
		t.Fatalf("claimed %q, want Artist Live", item.Inputs.Track.ArtistName)
	}

	body := dashboardBody(t, sqlDB)
	iLive, iWait := strings.Index(body, "Track Live"), strings.Index(body, "Track Wait")
	if iLive < 0 || iWait < 0 || iLive >= iWait {
		t.Fatalf("claimed row must render above buffered rows: live=%d wait=%d", iLive, iWait)
	}
	if !strings.Contains(body, `class="mx-upnext-inflight"`) || !strings.Contains(body, ">in flight<") {
		t.Errorf("claimed row missing in-flight highlight/label: %s", excerptAround(body, "Track Live"))
	}
	if strings.Contains(body, "stuck?") {
		t.Error("a just-claimed row must not carry the stuck badge")
	}
	// With a claimed row on top, the table and tooltip must not call it all
	// upcoming work (#1205 review).
	if !strings.Contains(body, `aria-label="Queue work in progress and upcoming"`) {
		t.Errorf("in-flight table label not updated: %s", excerptAround(body, "mx-table"))
	}
	if !strings.Contains(body, "Rows the background worker is processing now") {
		t.Error("in-flight section tooltip does not name the claimed rows")
	}
}

// TestHandleDashboard_UpNextInFlightLiveAndOrphan renders a live claim beside
// an orphan: both appear, and only the orphan is flagged stuck.
func TestHandleDashboard_UpNextInFlightLiveAndOrphan(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	now := time.Now().UTC()
	insertProcessing(t, sqlDB, "Artist L", "Track Live", now.Add(-2*time.Minute).Format(time.RFC3339))
	insertProcessing(t, sqlDB, "Artist O", "Track Orphan", now.Add(-3*time.Hour).Format(time.RFC3339))

	body := dashboardBody(t, sqlDB)
	rows := strings.Split(body, `class="mx-upnext-inflight"`)
	if len(rows) != 3 {
		t.Fatalf("want 2 in-flight rows, got %d", len(rows)-1)
	}
	// Oldest claim first: the orphan, then the live row.
	orphan := strings.SplitN(rows[1], "</tr>", 2)[0]
	live := strings.SplitN(rows[2], "</tr>", 2)[0]
	if !strings.Contains(orphan, "Track Orphan") || !strings.Contains(orphan, "stuck?") || !strings.Contains(orphan, ">3h<") {
		t.Errorf("orphan row wrong: %s", orphan)
	}
	if !strings.Contains(live, "Track Live") || !strings.Contains(live, ">2m<") {
		t.Errorf("live row wrong: %s", live)
	}
	if strings.Contains(live, "stuck?") {
		t.Error("live claim must not carry the stuck badge")
	}
}

// TestHandleDashboard_UpNextInFlightUnknownClaim: no recorded claim time reads
// "unknown" with no badge (no age is known to judge).
func TestHandleDashboard_UpNextInFlightUnknownClaim(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	insertProcessing(t, sqlDB, "Artist U", "Track Unknown", "")
	body := dashboardBody(t, sqlDB)
	if !strings.Contains(body, ">unknown<") {
		t.Errorf("zero ClaimedAt must render unknown: %s", excerptAround(body, "Track Unknown"))
	}
	if strings.Contains(body, "stuck?") {
		t.Error("unknown claim time must not carry the stuck badge")
	}
}

func TestBuildInFlightRows_StuckBoundary(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	rows := buildInFlightRows([]reports.InFlightItem{
		{Title: "Edge", ClaimedAt: now.Add(-dashboardStuckAfter)},
		{Title: "Past", ClaimedAt: now.Add(-dashboardStuckAfter - time.Second)},
	}, now)
	if rows[0].Stuck || !rows[1].Stuck {
		t.Errorf("boundary: edge stuck=%v past stuck=%v, want false/true", rows[0].Stuck, rows[1].Stuck)
	}
}

// TestHandleDashboard_UpNextInFlightPrivacy: the in-flight row shows artist and
// title (session-guarded page) but never a source path.
func TestHandleDashboard_UpNextInFlightPrivacy(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	insertProcessing(t, sqlDB, "Artist P", "Track P", time.Now().UTC().Format(time.RFC3339))
	if _, err := sqlDB.ExecContext(context.Background(),
		`UPDATE work_queue SET source_path = '/private/music/secret.flac' WHERE title = 'Track P'`); err != nil {
		t.Fatalf("set source_path: %v", err)
	}
	body := dashboardBody(t, sqlDB)
	if !strings.Contains(body, "Track P") {
		t.Fatal("in-flight row missing")
	}
	if strings.Contains(body, "/private/music") || strings.Contains(body, "secret.flac") {
		t.Error("in-flight panel leaked a source path")
	}
}

// excerptAround returns a short slice of s around the first occurrence of marker,
// for readable test failure messages.
func excerptAround(s, marker string) string {
	i := strings.Index(s, marker)
	if i < 0 {
		return "(marker not found)"
	}
	start := max(i-40, 0)
	end := min(i+80, len(s))
	return s[start:end]
}
