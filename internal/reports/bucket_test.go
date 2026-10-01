package reports_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
)

func bucketTitles(rows []reports.BucketRow) map[string]bool {
	m := make(map[string]bool, len(rows))
	for _, r := range rows {
		m[r.Title] = true
	}
	return m
}

func listAll(t *testing.T, repo *reports.Repo, b reports.Bucket) []reports.BucketRow {
	t.Helper()
	var all []reports.BucketRow
	var after int64
	for i := 0; i < 1000; i++ {
		page, err := repo.ListBucket(context.Background(), b, after, reports.MaxBucketLimit)
		if err != nil {
			t.Fatalf("ListBucket(%s): %v", b, err)
		}
		all = append(all, page...)
		if len(page) < reports.MaxBucketLimit {
			return all
		}
		after = page[len(page)-1].ID
	}
	t.Fatal("pagination did not terminate")
	return nil
}

func TestListBucketMembership(t *testing.T) {
	sqlDB := openTestDB(t)
	for _, w := range []workItem{
		{artist: "A", title: "pend", status: "pending"},
		{artist: "A", title: "proc", status: "processing"},
		{artist: "A", title: "defer", status: "deferred", missCount: 2, attempts: 3, nextAttemptAt: "2030-01-01T00:00:00Z"},
		{artist: "A", title: "fail", status: "failed", lastError: "boom"},
		{artist: "A", title: "word", status: "done", outcomeType: "synced", syncTier: "word"},
		{artist: "A", title: "line", status: "done", outcomeType: "synced", syncTier: "line"},
		{artist: "A", title: "demoted", status: "done", outcomeType: "synced", syncTier: "word", timingOutcome: "mis_synced"},
		{artist: "A", title: "retired", status: "unavailable", lastError: "miss limit reached"},
	} {
		insertWorkItem(t, sqlDB, w)
	}
	repo := reports.New(sqlDB)
	want := map[reports.Bucket][]string{
		reports.BucketPending:     {"pend"},
		reports.BucketProcessing:  {"proc"},
		reports.BucketDeferred:    {"defer"},
		reports.BucketFailed:      {"fail"},
		reports.BucketFinished:    {"word"},
		reports.BucketSettled:     {"line", "demoted"},
		reports.BucketUnavailable: {"retired"},
	}
	for _, b := range reports.Buckets() {
		got := bucketTitles(listAll(t, repo, b))
		if len(got) != len(want[b]) {
			t.Errorf("%s: got %v, want %v", b, got, want[b])
			continue
		}
		for _, title := range want[b] {
			if !got[title] {
				t.Errorf("%s: missing %q (got %v)", b, title, got)
			}
		}
	}
	rows := listAll(t, repo, reports.BucketDeferred)
	if r := rows[0]; r.MissCount != 2 || r.Attempts != 3 || r.NextAttemptAt != "2030-01-01T00:00:00Z" || r.Status != "deferred" || r.UpdatedAt == "" {
		t.Errorf("deferred row fields = %+v", r)
	}
	if r := listAll(t, repo, reports.BucketPending)[0]; r.Reason != queue.NoReasonRecorded {
		t.Errorf("empty last_error Reason = %q, want %q", r.Reason, queue.NoReasonRecorded)
	}
}

func TestListBucketFinishedSettledPartitionDone(t *testing.T) {
	repo, wantDone, wantFinished := seedFinishedSplit(t)
	fin := listAll(t, repo, reports.BucketFinished)
	set := listAll(t, repo, reports.BucketSettled)
	if int64(len(fin)) != wantFinished {
		t.Errorf("finished = %d, want %d", len(fin), wantFinished)
	}
	if int64(len(fin)+len(set)) != wantDone {
		t.Errorf("finished+settled = %d, want done %d", len(fin)+len(set), wantDone)
	}
	seen := map[int64]bool{}
	for _, r := range append(fin, set...) {
		if seen[r.ID] {
			t.Errorf("row %d in both finished and settled", r.ID)
		}
		seen[r.ID] = true
		if r.Status != "done" {
			t.Errorf("row %d status %q, want done", r.ID, r.Status)
		}
	}
}

func TestListBucketKeysetPagination(t *testing.T) {
	sqlDB := openTestDB(t)
	const n = 11
	for i := 0; i < n; i++ {
		insertWorkItem(t, sqlDB, workItem{artist: "A", title: string(rune('a' + i)), status: "pending"})
		insertWorkItem(t, sqlDB, workItem{artist: "B", title: string(rune('a' + i)), status: "failed"})
	}
	repo := reports.New(sqlDB)
	var got []int64
	var after int64
	for pages := 0; ; pages++ {
		if pages > n {
			t.Fatal("pagination did not terminate")
		}
		page, err := repo.ListBucket(context.Background(), reports.BucketPending, after, 4)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page {
			got = append(got, r.ID)
		}
		if len(page) < 4 {
			break
		}
		after = page[len(page)-1].ID
	}
	if len(got) != n {
		t.Fatalf("paged rows = %d, want %d (%v)", len(got), n, got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Errorf("ids not strictly ascending: %v", got)
		}
	}
}

func TestListBucketMultiLibraryRow(t *testing.T) {
	sqlDB := openTestDB(t)
	lib1 := insertLibrary(t, sqlDB)
	res, err := sqlDB.ExecContext(context.Background(), `INSERT INTO libraries (path, name) VALUES ('/other', 'lib2')`)
	if err != nil {
		t.Fatal(err)
	}
	lib2, _ := res.LastInsertId()
	shared := insertWorkItem(t, sqlDB, workItem{artist: "A", title: "shared", status: "unavailable"})
	solo := insertWorkItem(t, sqlDB, workItem{artist: "A", title: "solo", status: "unavailable"})
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "cli", status: "unavailable"})
	linkScanResult(t, sqlDB, shared, insertScanResult(t, sqlDB, lib1, "/music/a.flac"))
	linkScanResult(t, sqlDB, shared, insertScanResult(t, sqlDB, lib2, "/other/a.flac"))
	linkScanResult(t, sqlDB, solo, insertScanResult(t, sqlDB, lib1, "/music/b.flac"))
	// Same library, same row: DISTINCT must list it once.
	linkScanResult(t, sqlDB, shared, insertScanResult(t, sqlDB, lib1, "/music/a-dup.flac"))

	rows := listAll(t, reports.New(sqlDB), reports.BucketUnavailable)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (a multi-library row must appear once)", len(rows))
	}
	libs := map[string]int{}
	for _, r := range rows {
		libs[r.Title] = len(r.Libraries)
		if r.Title == "shared" {
			// Documented order: ascending library id.
			if len(r.Libraries) == 2 && (r.Libraries[0].ID != lib1 || r.Libraries[1].ID != lib2) {
				t.Errorf("shared libraries = %+v, want ids [%d %d] ascending", r.Libraries, lib1, lib2)
			}
		}
	}
	if libs["shared"] != 2 || libs["solo"] != 1 || libs["cli"] != 0 {
		t.Errorf("library counts = %v, want shared=2 solo=1 cli=0", libs)
	}
}

func TestListBucketReasonMatchesFailureAnalysis(t *testing.T) {
	sqlDB := openTestDB(t)
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "vol", status: "failed", attempts: 1,
		lastError: `write "/music/Some Artist/track.lrc": connection to 10.1.2.3:8080 refused`})
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "blank", status: "failed", attempts: 1, lastError: "   "})
	repo := reports.New(sqlDB)

	groups, err := repo.FailureAnalysis(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	groupReasons := map[string]bool{}
	for _, g := range groups {
		groupReasons[g.Reason] = true
	}
	rows := listAll(t, repo, reports.BucketFailed)
	if len(rows) != 2 {
		t.Fatalf("failed rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if !groupReasons[r.Reason] {
			t.Errorf("%s: Reason %q is not a FailureAnalysis group (groups %v)", r.Title, r.Reason, groupReasons)
		}
		switch r.Title {
		case "vol":
			if strings.Contains(r.Reason, "/music/") || strings.Contains(r.Reason, "10.1.2.3") {
				t.Errorf("volatile Reason not normalized: %q", r.Reason)
			}
		case "blank":
			if r.Reason != queue.NoReasonRecorded {
				t.Errorf("whitespace-only Reason = %q, want %q", r.Reason, queue.NoReasonRecorded)
			}
		}
	}
}

func TestListBucketLimitClamp(t *testing.T) {
	sqlDB := openTestDB(t)
	for i := 0; i < reports.MaxBucketLimit+2; i++ {
		insertWorkItem(t, sqlDB, workItem{artist: "A", title: fmt.Sprintf("t%04d", i), status: "pending"})
	}
	repo := reports.New(sqlDB)
	for _, lim := range []int{-1, 0} {
		rows, err := repo.ListBucket(context.Background(), reports.BucketPending, 0, lim)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Errorf("limit %d returned %d rows, want 1", lim, len(rows))
		}
	}
	rows, err := repo.ListBucket(context.Background(), reports.BucketPending, 0, reports.MaxBucketLimit+100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != reports.MaxBucketLimit {
		t.Errorf("oversize limit returned %d rows, want %d", len(rows), reports.MaxBucketLimit)
	}
}

func TestListBucketUnknownBucket(t *testing.T) {
	repo := reports.New(openTestDB(t))
	if _, err := repo.ListBucket(context.Background(), reports.Bucket("done"), 0, 10); err == nil {
		t.Error("ListBucket(unknown) = nil error, want error")
	}
	if _, err := reports.ParseBucket("nope"); err == nil {
		t.Error("ParseBucket(unknown) = nil error")
	}
	if b, err := reports.ParseBucket("finished"); err != nil || b != reports.BucketFinished {
		t.Errorf("ParseBucket(finished) = %q, %v", b, err)
	}
}

func TestParseBucketRoundTrip(t *testing.T) {
	for _, b := range reports.Buckets() {
		got, err := reports.ParseBucket(string(b))
		if err != nil || got != b {
			t.Errorf("ParseBucket(%q) = %q, %v; want %q, nil", b, got, err, b)
		}
	}
	if _, err := reports.ParseBucket("nope"); err == nil {
		t.Error("ParseBucket(unknown) returned nil error")
	}
}

func TestListBucketEmptyAndUnlinked(t *testing.T) {
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)
	rows, err := repo.ListBucket(context.Background(), reports.BucketPending, 0, 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("empty bucket = %v, %v; want no rows, nil", rows, err)
	}
	// A CLI-enqueued row with no scan link.
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "solo", status: "pending"})
	rows, err = repo.ListBucket(context.Background(), reports.BucketPending, 0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListBucket = %v, %v; want 1 row", rows, err)
	}
	if len(rows[0].Libraries) != 0 {
		t.Errorf("row = %+v; want no libraries", rows[0])
	}
}

func TestListBucketQueryErrorsPropagate(t *testing.T) {
	sqlDB := openTestDB(t)
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "x", status: "pending"})
	repo := reports.New(sqlDB)

	// The library lookup fails after the page query succeeded.
	if _, err := sqlDB.Exec(`DROP TABLE work_queue_scan_results`); err != nil {
		t.Fatalf("drop junction: %v", err)
	}
	if _, err := repo.ListBucket(context.Background(), reports.BucketPending, 0, 10); err == nil {
		t.Error("expected library lookup error")
	}

	// The page query itself fails on a closed database.
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := repo.ListBucket(context.Background(), reports.BucketPending, 0, 10); err == nil {
		t.Error("expected page query error on closed DB")
	}
}
