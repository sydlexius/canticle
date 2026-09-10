package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/reports"
)

// #929: end-to-end coverage for the dashboard's "last LRC normalization"
// summary line, from the maintenance_markers row through
// reports.Repo.LastLRCNormalization to the rendered page.

// Pre-pass state (no marker row yet) must render a sensible sentence, not a
// bare "0" and not a broken/empty tile -- an explicit hard constraint on #929.
func TestHandleDashboard_LRCNormalizeSummary_NeverRun(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	mux := newReportsUIServer(t, sqlDB)

	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No LRC normalization pass has run yet") {
		t.Errorf("pre-pass state did not render the never-run sentence; body snippet search failed")
	}
	if strings.Contains(body, "0 files rewritten") || strings.Contains(body, "0 file rewritten") {
		t.Error("pre-pass state must not render as a bare zero-count pass")
	}
}

// A stamped marker renders the count and timestamp, and never a path.
func TestHandleDashboard_LRCNormalizeSummary_Stamped(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	if _, err := sqlDB.Exec(
		`INSERT INTO maintenance_markers (name, completed_at, detail_count) VALUES (?, ?, ?)`,
		reports.MaintenanceMarkerLRCNormalize, "2026-09-01T12:30:00Z", 42); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	mux := newReportsUIServer(t, sqlDB)
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "42 files rewritten") {
		t.Errorf("stamped summary did not render the count; body missing '42 files rewritten'")
	}
	if !strings.Contains(body, "Last LRC normalization") {
		t.Error("stamped summary missing its label")
	}
}

// Singular/plural noun agreement: exactly 1 file must read "1 file", not
// "1 files" -- a small thing, but it is the kind of copy defect that makes an
// operator-facing summary look unpolished on the most common real count.
func TestHandleDashboard_LRCNormalizeSummary_SingularNoun(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	if _, err := sqlDB.Exec(
		`INSERT INTO maintenance_markers (name, completed_at, detail_count) VALUES (?, ?, ?)`,
		reports.MaintenanceMarkerLRCNormalize, "2026-09-01T12:30:00Z", 1); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	mux := newReportsUIServer(t, sqlDB)
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "1 file rewritten") {
		t.Errorf("want singular '1 file rewritten'; body: %s", extractAround(body, "LRC normalization"))
	}
	if strings.Contains(body, "1 files rewritten") {
		t.Error("want singular noun for count=1, got plural")
	}
}

// Follow-up to #929: a marker stamped with Normalized==0 (a pass that
// genuinely ran and found nothing to rewrite -- the marker is stamped on
// EVERY applied run, per internal/commands.markLRCNormalizeApply) must render
// its own "clean bill of health" sentence, DISTINCT from both the never-run
// sentence (no marker row at all) and the nonzero-count sentence. A test that
// only checked "not empty" would have passed under the OLD two-branch
// behavior (which rendered "0 files rewritten" here) and is worthless for
// pinning this fix -- so this asserts the exact new wording is present AND
// that neither of the other two sentences' distinguishing text leaked in.
func TestHandleDashboard_LRCNormalizeSummary_ZeroCount(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	if _, err := sqlDB.Exec(
		`INSERT INTO maintenance_markers (name, completed_at, detail_count) VALUES (?, ?, ?)`,
		reports.MaintenanceMarkerLRCNormalize, "2026-09-01T12:30:00Z", 0); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	mux := newReportsUIServer(t, sqlDB)
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "no stacked sidecars found") {
		t.Errorf("zero-count run did not render the clean-bill-of-health sentence; snippet: %s", extractAround(body, "LRC normalization"))
	}
	// Must NOT read as the never-run state.
	if strings.Contains(body, "No LRC normalization pass has run yet") {
		t.Error("zero-count run rendered the never-run sentence; a pass DID run")
	}
	// Must NOT read as "0 files rewritten" (the old, pre-fix wording) or carry
	// a rewritten-count phrase at all -- this pass rewrote nothing, so a count
	// sentence here would misstate what happened.
	if strings.Contains(body, "0 files rewritten") || strings.Contains(body, "0 file rewritten") {
		t.Error("zero-count run rendered the count sentence instead of the clean-bill-of-health sentence")
	}
	if strings.Contains(body, "files rewritten") || strings.Contains(body, "file rewritten") {
		t.Error("zero-count run must not use \"rewritten\" phrasing at all")
	}
}

// Direct unit coverage of formatLRCNormalizeSummary's three branches, so the
// exact sentence text is pinned independent of page markup/whitespace.
func TestFormatLRCNormalizeSummary_ThreeBranches(t *testing.T) {
	neverRun := formatLRCNormalizeSummary(reports.LRCNormalizationSummary{}, nil)
	zero := formatLRCNormalizeSummary(reports.LRCNormalizationSummary{Ever: true, Normalized: 0}, nil)
	one := formatLRCNormalizeSummary(reports.LRCNormalizationSummary{Ever: true, Normalized: 1}, nil)
	many := formatLRCNormalizeSummary(reports.LRCNormalizationSummary{Ever: true, Normalized: 5}, nil)

	if !strings.Contains(neverRun, "No LRC normalization pass has run yet") {
		t.Errorf("never-run sentence: %q", neverRun)
	}
	if !strings.Contains(zero, "no stacked sidecars found") {
		t.Errorf("zero-count sentence: %q", zero)
	}
	if !strings.Contains(one, "1 file rewritten") || strings.Contains(one, "1 files rewritten") {
		t.Errorf("singular-count sentence: %q", one)
	}
	if !strings.Contains(many, "5 files rewritten") {
		t.Errorf("plural-count sentence: %q", many)
	}
	// All four must be pairwise distinct strings -- the whole point of the
	// three-branch fix is that these no longer collapse.
	all := []string{neverRun, zero, one, many}
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			if all[i] == all[j] {
				t.Errorf("sentences %d and %d are identical, want distinct: %q", i, j, all[i])
			}
		}
	}
}

// PRIVACY (hard constraint): the summary must never carry a filesystem path,
// artist, title, album, or lyric text -- counts and a timestamp only. A
// maintenance_markers row physically cannot carry any of those (the schema
// has no such column), so this test pins the CONTRACT at the rendered-page
// level: the summary sentence contains only the expected words/digits/
// punctuation, never anything path-shaped (a "/" or a file extension).
func TestHandleDashboard_LRCNormalizeSummary_NoPathLeak(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	if _, err := sqlDB.Exec(
		`INSERT INTO maintenance_markers (name, completed_at, detail_count) VALUES (?, ?, ?)`,
		reports.MaintenanceMarkerLRCNormalize, "2026-09-01T12:30:00Z", 3); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	mux := newReportsUIServer(t, sqlDB)
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	snippet := extractAround(body, "LRC normalization")
	if snippet == "" {
		t.Fatal("could not find the LRC normalization line in the rendered page")
	}
	for _, forbidden := range []string{".lrc", ".mp3", ".flac", "/music", "/mnt"} {
		if strings.Contains(snippet, forbidden) {
			t.Errorf("summary snippet leaked a path-shaped token %q: %s", forbidden, snippet)
		}
	}
}

// extractAround returns a small window of body around the first occurrence of
// needle, for building a readable failure message / narrow substring check
// without asserting on the whole page body.
func extractAround(body, needle string) string {
	i := strings.Index(body, needle)
	if i < 0 {
		return ""
	}
	start := i - 40
	if start < 0 {
		start = 0
	}
	end := i + 120
	if end > len(body) {
		end = len(body)
	}
	return body[start:end]
}
