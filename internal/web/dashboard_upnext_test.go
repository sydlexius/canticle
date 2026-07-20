package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/reports"
)

func TestTierLabel(t *testing.T) {
	// PriorityMiss=-100 -> miss; PriorityScan=0 and PriorityWebhook=10 -> fresh.
	tests := []struct {
		priority int
		want     string
	}{
		{-100, "miss"},
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
	if !strings.Contains(body, "3 buffered of 3 eligible") {
		t.Errorf("dashboard missing buffered/eligible header; body has: %s", excerptAround(body, "buffered"))
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
	if strings.Contains(body, `aria-label="Upcoming queue work"`) {
		t.Error("empty state must not render the ordered table")
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
