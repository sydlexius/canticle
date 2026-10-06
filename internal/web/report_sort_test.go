package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// getReport fetches a report page (full page, no htmx) with the given query.
func getReport(t *testing.T, sqlDBMux http.Handler, key, query string) string {
	t.Helper()
	path := "/reports/" + key
	if query != "" {
		path += "?" + query
	}
	rec := httptest.NewRecorder()
	sqlDBMux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", path, rec.Code)
	}
	return rec.Body.String()
}

// assertOrder fails unless every title appears in body, in the given order.
func assertOrder(t *testing.T, label, body string, titles ...string) {
	t.Helper()
	prev := -1
	for _, title := range titles {
		i := strings.Index(body, ">"+title+"<")
		if i < 0 {
			t.Fatalf("%s: title %q missing from body", label, title)
		}
		if i < prev {
			t.Errorf("%s: %q out of order (want %v)", label, title, titles)
		}
		prev = i
	}
}

// badSorts are request values that must fall back to a table's default.
func badSorts(ns string) []string {
	return []string{
		ns + "_sort=bogus&" + ns + "_dir=sideways",
		ns + "_sort=" + url.QueryEscape("title; DROP TABLE work_queue--"),
		ns + "_sort=status", // in the shared vocabulary but not this table's
	}
}

// TestReportSortRecentOutcomes pins the Recent outcomes ordering for the default and for sort and dir params, including that bad values fall back to the default.
func TestReportSortRecentOutcomes(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	out := `[{"outdir":"/o","filename":"a.lrc"}]`
	insertDone(t, sqlDB, "Cc", "musixmatch", out, "2026-06-17T12:00:00Z")
	insertDone(t, sqlDB, "Aa", "musixmatch", out, "2026-06-17T10:00:00Z")
	insertDone(t, sqlDB, "Bb", "musixmatch", out, "2026-06-17T11:00:00Z")
	mux := newReportsUIServer(t, sqlDB)

	// Default: newest completion first, exactly as before.
	assertOrder(t, "default", getReport(t, mux, "recent-outcomes", ""), "Cc", "Bb", "Aa")
	assertOrder(t, "title asc", getReport(t, mux, "recent-outcomes", "ro_sort=title&ro_dir=asc"), "Aa", "Bb", "Cc")
	for _, q := range badSorts("ro") {
		body := getReport(t, mux, "recent-outcomes", q)
		assertOrder(t, q, body, "Cc", "Bb", "Aa")
		if strings.Contains(body, "DROP TABLE") || strings.Contains(body, "bogus") {
			t.Errorf("%s: request text reflected into the page", q)
		}
	}
	body := getReport(t, mux, "recent-outcomes", "")
	if !strings.Contains(body, `aria-sort="descending"`) || !strings.Contains(body, `href="/reports/recent-outcomes?ro_dir=asc&amp;ro_sort=completed"`) {
		t.Error("active Completed header should be descending and toggle to asc under ro_ params")
	}
	if !strings.Contains(body, `<th scope="col">Detail</th>`) {
		t.Error("Detail header should be plain")
	}
	if strings.Contains(body, `href="/reports/recent-outcomes?dir=`) || strings.Contains(body, "&amp;sort=") || strings.Contains(body, "?sort=") {
		t.Error("recent outcomes links must use the ro_ namespace, not plain sort/dir")
	}
}

// TestReportRefreshKeepsSort pins that Refresh targets the report path plus the
// table's own validated sort (and nothing else), so the URL and the table agree.
func TestReportRefreshKeepsSort(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	insertDone(t, sqlDB, "Cc", "musixmatch", `[{"outdir":"/o","filename":"a.lrc"}]`, "2026-06-17T12:00:00Z")
	mux := newReportsUIServer(t, sqlDB)
	body := getReport(t, mux, "recent-outcomes", "ro_sort=artist&ro_dir=asc&rq_sort=ratio&ro_bogus=1")
	want := `href="/reports/recent-outcomes?ro_dir=asc&amp;ro_sort=artist"`
	if !strings.Contains(body, want) || !strings.Contains(body, `hx-get="/reports/recent-outcomes?ro_dir=asc&amp;ro_sort=artist"`) {
		t.Errorf("Refresh should carry the table's own sort only (want %s)", want)
	}
	plain := getReport(t, mux, "recent-outcomes", "ro_sort=bogus")
	if !strings.Contains(plain, `hx-get="/reports/recent-outcomes"`) {
		t.Error("an invalid sort must leave Refresh at the bare report path")
	}
}

// TestReportSortCarriesOtherTables pins that a sort link carries the other
// tables' validated sort params, and drops an invalid one.
func TestReportSortCarriesOtherTables(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	insertDone(t, sqlDB, "Cc", "musixmatch", `[{"outdir":"/o","filename":"a.lrc"}]`, "2026-06-17T12:00:00Z")
	mux := newReportsUIServer(t, sqlDB)
	body := getReport(t, mux, "recent-outcomes", "ro_sort=title&rq_sort=ratio&rq_dir=asc&in_sort=bogus")
	if !strings.Contains(body, "rq_dir=asc&amp;rq_sort=ratio") {
		t.Error("header links lost the review queue's sort")
	}
	if strings.Contains(body, "in_sort") {
		t.Error("an invalid other-table sort must be dropped, not carried")
	}
}

// TestReportSortInstrumentals pins the Instrumentals ordering: id by default, then the namespaced in_sort and in_dir params.
func TestReportSortInstrumentals(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	// Insertion order (the default, by id) is Zed, Mid, Alpha.
	insertInstrumental(t, sqlDB, "Zed", 1, "")
	insertInstrumental(t, sqlDB, "Mid", 0, "")
	insertInstrumental(t, sqlDB, "Alpha", nil, "")
	mux := newReportsUIServer(t, sqlDB)

	assertOrder(t, "default", getReport(t, mux, "instrumental-inventory", ""), "Zed", "Mid", "Alpha")
	assertOrder(t, "title asc", getReport(t, mux, "instrumental-inventory", "in_sort=title&in_dir=asc"), "Alpha", "Mid", "Zed")
	for _, q := range badSorts("in") {
		assertOrder(t, q, getReport(t, mux, "instrumental-inventory", q), "Zed", "Mid", "Alpha")
	}
	body := getReport(t, mux, "instrumental-inventory", "")
	for _, plain := range []string{"ID", "File"} {
		if !strings.Contains(body, `<th scope="col">`+plain+`</th>`) {
			t.Errorf("%s header should be plain", plain)
		}
	}
}

// TestReportSortReviewQueue pins the Review queue ordering for the namespaced rq_sort and rq_dir params against its default.
func TestReportSortReviewQueue(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	insertReviewQueueRow(t, sqlDB, "Old", "mis_synced", 1, 3.0, "2026-08-01T00:00:00Z")
	insertReviewQueueRow(t, sqlDB, "New", "categorical", 2, 1.0, "2026-08-03T00:00:00Z")
	insertReviewQueueRow(t, sqlDB, "Mid", "mis_synced", 3, 2.0, "2026-08-02T00:00:00Z")
	mux := newReportsUIServer(t, sqlDB)

	assertOrder(t, "default", getReport(t, mux, "review-queue", ""), "New", "Mid", "Old")
	assertOrder(t, "ratio asc", getReport(t, mux, "review-queue", "rq_sort=ratio&rq_dir=asc"), "New", "Mid", "Old")
	assertOrder(t, "ratio desc", getReport(t, mux, "review-queue", "rq_sort=ratio&rq_dir=desc"), "Old", "Mid", "New")
	assertOrder(t, "overrun natural", getReport(t, mux, "review-queue", "rq_sort=overrun"), "Mid", "New", "Old")
	for _, q := range badSorts("rq") {
		assertOrder(t, q, getReport(t, mux, "review-queue", q), "New", "Mid", "Old")
	}
	if !strings.Contains(getReport(t, mux, "review-queue", ""), `<th scope="col">Lyrics</th>`) {
		t.Error("Lyrics header should be plain")
	}
}

// TestFailureGroupSort pins that a failure group sorts by the requested column
// and direction, and that its header links address the same group.
func TestFailureGroupSort(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	exec := func(q string, a ...any) error { _, err := sqlDB.ExecContext(context.Background(), q, a...); return err }
	const reason = "musixmatch: unexpected matcher status_code 500"
	// Default order is updated_at DESC, id DESC: Cc (newest id), Aa, Bb is the
	// reverse of insertion, so insert Bb, Aa, Cc.
	for _, title := range []string{"Bb", "Aa", "Cc"} {
		seedFailureRow(t, title, "failed", reason, exec)
	}
	mux := newReportsUIServer(t, sqlDB)
	get := func(extra string) string {
		q := url.Values{"status": {"failed"}, "signature": {reason}}.Encode()
		rec := getFailureGroupQuery(t, mux, q+extra)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		return rec.Body.String()
	}
	assertOrder(t, "default", get(""), "Cc", "Aa", "Bb")
	body := get("&fg_sort=title&fg_dir=asc")
	assertOrder(t, "title asc", body, "Aa", "Bb", "Cc")
	if !strings.Contains(body, `hx-target="closest td"`) || !strings.Contains(body, "fg_sort=title") {
		t.Error("sort links should be htmx GETs into the group's cell under the fg_ namespace")
	}
	// The group's identity travels with every sort link.
	if !strings.Contains(body, "status=failed") || !strings.Contains(body, "signature=") {
		t.Error("sort links lost the group's status/signature")
	}
	for _, q := range badSorts("fg") {
		assertOrder(t, q, get("&"+q), "Cc", "Aa", "Bb")
	}
}

// TestFailureGroupSortKeepsRowSet pins that sorting a truncated group arranges
// the rows it already shows (the newest page) and never swaps in others.
func TestFailureGroupSortKeepsRowSet(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	exec := func(q string, a ...any) error { _, err := sqlDB.ExecContext(context.Background(), q, a...); return err }
	const reason = "musixmatch: unexpected matcher status_code 500"
	// "A-old" sorts first by title but is the OLDEST row, outside the shown page.
	seedFailureRow(t, "A-old", "failed", reason, exec)
	for i := 0; i < failureGroupPageSize; i++ {
		seedFailureRow(t, "Z"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+string(rune('a'+i/26)), "failed", reason, exec)
	}
	mux := newReportsUIServer(t, sqlDB)
	q := url.Values{"status": {"failed"}, "signature": {reason}}.Encode()
	body := getFailureGroupQuery(t, mux, q+"&fg_sort=title&fg_dir=asc").Body.String()
	if strings.Contains(body, "A-old") {
		t.Error("sorting pulled in a row outside the newest page")
	}
	if !strings.Contains(body, "Showing the newest") {
		t.Error("truncation note missing")
	}
}

// TestDashboardHasNoSortLinks pins that the Dashboard tables, which share
// queries and look with the Reports tables, stay fixed-order.
func TestDashboardHasNoSortLinks(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	insertDone(t, sqlDB, "Dash Row", "musixmatch", `[{"outdir":"/o","filename":"a.lrc"}]`, "2026-06-19T12:00:00Z")
	seedFailureRow(t, "Failing Row", "failed", "worker: write item 1: permission denied",
		func(q string, a ...any) error { _, err := sqlDB.ExecContext(context.Background(), q, a...); return err })
	mux := newReportsUIServer(t, sqlDB)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard?ro_sort=title", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Dash Row") || !strings.Contains(body, "Failing Row") {
		t.Fatal("dashboard tables did not render the seeded rows")
	}
	for _, bad := range []string{"mx-sort-link", "aria-sort", "_sort="} {
		if strings.Contains(body, bad) {
			t.Errorf("dashboard must render no sort affordance, found %q", bad)
		}
	}
}
