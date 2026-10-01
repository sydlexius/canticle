package reports_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/failsig"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
)

var failedUpdatedAt = []string{
	"2026-01-01T10:00:00Z", "2026-01-01T12:00:00Z", "2026-01-01T11:00:00Z",
	"2026-01-01T09:00:00Z", "2026-01-01T08:00:00Z",
}

func seedFailureItems(t *testing.T) *reports.Repo {
	t.Helper()
	sqlDB := openTestDB(t)
	// An AFTER UPDATE trigger restamps updated_at to now on every write, which
	// would erase the distinct timestamps seeded below; drop it for this DB.
	if _, err := sqlDB.ExecContext(context.Background(), `DROP TRIGGER update_work_queue_updated_at`); err != nil {
		t.Fatalf("drop updated_at trigger: %v", err)
	}
	// Raw last_error values that differ but normalize to one signature, plus a
	// distinct one, a no-reason row with attempts, and a row the #789 guard hides.
	for i, le := range []string{
		"write /music/a111/x.lrc: permission denied",
		"write /music/a222/x.lrc: permission denied",
		"write /music/a333/x.lrc: permission denied",
		"musixmatch: unexpected matcher status_code 500",
		"",
	} {
		id := insertWorkItem(t, sqlDB, workItem{
			artist: "A", title: fmt.Sprintf("f%d", i), status: "failed", lastError: le, attempts: 1,
		})
		// Distinct updated_at, deliberately not monotonic in id, so the
		// newest-first order is observable: f1 newest, then f2, then f0.
		setWorkItemColumn(t, sqlDB, id, "updated_at", true, failedUpdatedAt[i])
	}
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "guarded", status: "failed", attempts: 0})
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "d1", status: "deferred", lastError: "no match", missCount: 1})
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "d2", status: "deferred", lastError: "no match", missCount: 2})
	// Parked rows share the ordinary deferred rows' normalized error, so only
	// the deferred row filter keeps them out of the group and its items.
	parkedRefused := insertWorkItem(t, sqlDB, workItem{artist: "A", title: "parked-refused", status: "deferred", lastError: "no match", missCount: 1})
	setWorkItemColumn(t, sqlDB, parkedRefused, "refused_waits", true, 1)
	parkedUpgrade := insertWorkItem(t, sqlDB, workItem{artist: "A", title: "parked-upgrade", status: "deferred", lastError: "no match", missCount: 1})
	setWorkItemColumn(t, sqlDB, parkedUpgrade, "upgrade_queued", true, 1)
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "parked-word", status: "deferred", lastError: "no match", missCount: 1, wordTimingState: "queued"})
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "pend", status: "pending", lastError: "no match"})
	return reports.New(sqlDB)
}

func bigGroupSignature(t *testing.T, repo *reports.Repo) string {
	t.Helper()
	groups, err := repo.FailureAnalysis(context.Background())
	if err != nil {
		t.Fatalf("FailureAnalysis: %v", err)
	}
	for _, g := range groups {
		if g.Count == 3 {
			return g.Reason
		}
	}
	t.Fatalf("no 3-row group in %+v (normalization split or merged unexpectedly)", groups)
	return ""
}

// The agreement invariant: every group the report returns expands to exactly
// its count of rows.
func TestFailureGroupItemsAgreeWithGroups(t *testing.T) {
	repo := seedFailureItems(t)
	ctx := context.Background()
	for _, tc := range []struct {
		status string
		groups func(context.Context) ([]reports.FailureGroup, error)
	}{
		{"failed", repo.FailureAnalysis},
		{"deferred", repo.DeferredMisses},
	} {
		groups, err := tc.groups(ctx)
		if err != nil {
			t.Fatalf("%s groups: %v", tc.status, err)
		}
		if len(groups) == 0 {
			t.Fatalf("%s: no groups seeded", tc.status)
		}
		for _, g := range groups {
			items, err := repo.FailureGroupItems(ctx, g.Status, g.Reason, reports.MaxFailureItemsLimit)
			if err != nil {
				t.Fatalf("items(%s,%q): %v", g.Status, g.Reason, err)
			}
			if int64(len(items)) != g.Count {
				t.Errorf("group %s %q: count %d, items %d", g.Status, g.Reason, g.Count, len(items))
			}
			for _, it := range items {
				if strings.HasPrefix(it.Title, "parked-") {
					t.Errorf("parked row %q leaked into %s group %q", it.Title, g.Status, g.Reason)
				}
				if it.Reason != g.Reason || it.Status != g.Status {
					t.Errorf("item %d in wrong group: %q/%q", it.ID, it.Status, it.Reason)
				}
			}
		}
	}
}

func TestFailureGroupItemsClassAndOrder(t *testing.T) {
	repo := seedFailureItems(t)
	sig := bigGroupSignature(t, repo)
	items, err := repo.FailureGroupItems(context.Background(), "failed", sig, 10)
	if err != nil || len(items) != 3 {
		t.Fatalf("items=%d err=%v", len(items), err)
	}
	for _, it := range items {
		if it.Class != failsig.Persistent {
			t.Errorf("class = %q, want persistent", it.Class)
		}
		if it.Title == "" || it.Artist != "A" {
			t.Errorf("display fields missing: %+v", it)
		}
	}
	for i, want := range []string{"f1", "f2", "f0"} {
		if items[i].Title != want {
			t.Errorf("newest-first position %d = %q, want %q", i, items[i].Title, want)
		}
	}
	trans, err := repo.FailureGroupItems(context.Background(), "failed", "musixmatch: unexpected matcher status_code 500", 10)
	if err != nil || len(trans) != 1 || trans[0].Class != failsig.Transient {
		t.Errorf("5xx row: %+v, %v", trans, err)
	}
	none, err := repo.FailureGroupItems(context.Background(), "failed", queue.NoReasonRecorded, 10)
	if err != nil || len(none) != 1 || none[0].Class != failsig.Persistent {
		t.Errorf("no-reason row: %+v, %v", none, err)
	}
}

func TestFailureGroupItemsDeferredHaveNoClass(t *testing.T) {
	repo := seedFailureItems(t)
	items, err := repo.FailureGroupItems(context.Background(), "deferred", "no match", 10)
	if err != nil || len(items) == 0 {
		t.Fatalf("deferred items=%d err=%v", len(items), err)
	}
	for _, it := range items {
		if it.Class != "" {
			t.Errorf("deferred item %q class = %q, want empty", it.Title, it.Class)
		}
	}
}

func TestFailureGroupItemsLimitAndFilters(t *testing.T) {
	repo := seedFailureItems(t)
	ctx := context.Background()
	sig := bigGroupSignature(t, repo)
	if got, _ := repo.FailureGroupItems(ctx, "failed", sig, 2); len(got) != 2 {
		t.Errorf("limit 2 returned %d", len(got))
	}
	if got, _ := repo.FailureGroupItems(ctx, "failed", sig, 0); len(got) != 1 {
		t.Errorf("limit 0 clamps to 1, got %d", len(got))
	}
	if got, _ := repo.FailureGroupItems(ctx, "deferred", sig, 10); len(got) != 0 {
		t.Errorf("status filter leaked %d rows", len(got))
	}
	if got, err := repo.FailureGroupItems(ctx, "failed", "no such signature", 10); err != nil || len(got) != 0 {
		t.Errorf("unknown signature: %v, %v", got, err)
	}
	if _, err := repo.FailureGroupItems(ctx, "pending", sig, 10); err == nil {
		t.Error("unsupported status must error")
	}
	if got, err := repo.FailureGroupItems(ctx, "failed", sig, 1<<20); err != nil || len(got) != 3 {
		t.Errorf("oversize limit should clamp: %d, %v", len(got), err)
	}
}

func TestFailureGroupItemsClosedDB(t *testing.T) {
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)
	_ = sqlDB.Close()
	if _, err := repo.FailureGroupItems(context.Background(), "failed", "x", 1); err == nil {
		t.Error("expected error on closed db")
	}
}

// TestNeedsAttentionMembershipOrderAndReason pins #654 AC2: failed rows come
// before deferred rows, each newest updated_at first; membership is the Failure
// Analysis / Deferred misses filter (parked word-recheck, upgrade-trip and
// refused rows and the #789 never-attempted row are excluded); the reason is
// normalized and only failed rows are classified. Recent Outcomes lists none of
// them (AC1). All values are synthetic.
func TestNeedsAttentionMembershipOrderAndReason(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	// The updated_at trigger would restamp every seeded value to now.
	if _, err := sqlDB.ExecContext(ctx, `DROP TRIGGER update_work_queue_updated_at`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	seed := func(w workItem, updatedAt string) int64 {
		id := insertWorkItem(t, sqlDB, w)
		setWorkItemColumn(t, sqlDB, id, "updated_at", true, updatedAt)
		return id
	}
	// A deferred row newer than every failed row: it must still sort after them.
	seed(workItem{artist: "A", title: "d-new", status: "deferred", lastError: "orchestrator: lane benign miss (no result)", missCount: 1}, "2026-09-01T12:00:00Z")
	seed(workItem{artist: "A", title: "d-old", status: "deferred", lastError: "orchestrator: lane benign miss (no result)", missCount: 1}, "2026-09-01T08:00:00Z")
	seed(workItem{artist: "A", title: "f-old", status: "failed", lastError: "write /srv/Synthetic Artist/x.lrc: permission denied", attempts: 1}, "2026-09-01T09:00:00Z")
	seed(workItem{artist: "A", title: "f-new", status: "failed", lastError: "lane x: transport error: dial tcp 10.0.0.1:443: i/o timeout", attempts: 1}, "2026-09-01T10:00:00Z")
	// Excluded: never attempted (#789), and three parked deferred shapes.
	seed(workItem{artist: "A", title: "f-guard", status: "failed"}, "2026-09-01T11:00:00Z")
	seed(workItem{artist: "A", title: "d-word", status: "deferred", lastError: "orchestrator: lane benign miss (no result)", missCount: 1, wordTimingState: "queued"}, "2026-09-01T13:00:00Z")
	up := seed(workItem{artist: "A", title: "d-upgrade", status: "deferred", lastError: "orchestrator: lane benign miss (no result)", missCount: 1}, "2026-09-01T13:00:00Z")
	setWorkItemColumn(t, sqlDB, up, "upgrade_queued", true, 1)
	ref := seed(workItem{artist: "A", title: "d-refused", status: "deferred", lastError: "orchestrator: lane benign miss (no result)", missCount: 1}, "2026-09-01T13:00:00Z")
	setWorkItemColumn(t, sqlDB, ref, "refused_waits", true, 1)
	seed(workItem{artist: "A", title: "done", status: "done", outcomeType: "synced", completedAt: "2026-09-01T07:00:00Z"}, "2026-09-01T07:00:00Z")

	repo := reports.New(sqlDB)
	got, err := repo.NeedsAttention(ctx, 25)
	if err != nil {
		t.Fatalf("NeedsAttention: %v", err)
	}
	var titles []string
	for _, it := range got {
		titles = append(titles, it.Title)
	}
	want := []string{"f-new", "f-old", "d-new", "d-old"}
	if len(titles) != len(want) {
		t.Fatalf("titles = %v; want %v", titles, want)
	}
	for i := range want {
		if titles[i] != want[i] {
			t.Fatalf("titles = %v; want %v", titles, want)
		}
	}
	if got[0].Class != failsig.Transient || got[1].Class != failsig.Persistent {
		t.Errorf("failed classes = %q, %q; want transient, persistent", got[0].Class, got[1].Class)
	}
	if got[2].Class != "" {
		t.Errorf("deferred row classified %q; want empty", got[2].Class)
	}
	if got[1].Reason != "write <path>: permission denied" {
		t.Errorf("reason not normalized: %q", got[1].Reason)
	}
	if got[0].UpdatedAt != "2026-09-01T10:00:00Z" {
		t.Errorf("UpdatedAt = %q", got[0].UpdatedAt)
	}

	// The limit caps the combined list, failed rows taking the budget first.
	capped, err := repo.NeedsAttention(ctx, 3)
	if err != nil || len(capped) != 3 || capped[2].Title != "d-new" {
		t.Errorf("limit 3 = %v (err %v); want f-new, f-old, d-new", capped, err)
	}

	closed := openTestDB(t)
	_ = closed.Close()
	if _, err := reports.New(closed).NeedsAttention(ctx, 5); err == nil {
		t.Error("NeedsAttention on closed DB: want error")
	}

	recent, err := repo.RecentOutcomes(ctx, 50)
	if err != nil {
		t.Fatalf("RecentOutcomes: %v", err)
	}
	for _, o := range recent {
		if o.Title != "done" {
			t.Errorf("Recent Outcomes lists a non-outcome row %q", o.Title)
		}
	}
}

// TestNeedsAttentionLimitClampAndTieBreak pins two properties of the listing
// itself. A limit below 1 is clamped to 1 (SQLite reads a negative LIMIT as
// "no limit", which would list every failed and deferred row), and one above
// MaxFailureItemsLimit to the cap. Two rows sharing an updated_at order by id
// DESC, so the listing is stable across renders. All values are synthetic.
func TestNeedsAttentionLimitClampAndTieBreak(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	if _, err := sqlDB.ExecContext(ctx, `DROP TRIGGER update_work_queue_updated_at`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	var ids []int64
	for _, title := range []string{"first", "second", "third"} {
		id := insertWorkItem(t, sqlDB, workItem{artist: "A", title: title, status: "failed", lastError: "synthetic: permission denied", attempts: 1})
		setWorkItemColumn(t, sqlDB, id, "updated_at", true, "2026-09-01T10:00:00Z")
		ids = append(ids, id)
	}
	repo := reports.New(sqlDB)

	for _, limit := range []int{0, -1} {
		got, err := repo.NeedsAttention(ctx, limit)
		if err != nil {
			t.Fatalf("NeedsAttention(%d): %v", limit, err)
		}
		if len(got) != 1 {
			t.Fatalf("NeedsAttention(%d) returned %d rows; want 1 (clamped)", limit, len(got))
		}
		// The tie-break: equal updated_at, so the highest id comes first.
		if got[0].ID != ids[2] {
			t.Errorf("NeedsAttention(%d)[0].ID = %d; want %d (id DESC on a tie)", limit, got[0].ID, ids[2])
		}
	}

	all, err := repo.NeedsAttention(ctx, reports.MaxFailureItemsLimit+1)
	if err != nil {
		t.Fatalf("NeedsAttention(max+1): %v", err)
	}
	if len(all) != 3 || all[0].ID != ids[2] || all[1].ID != ids[1] || all[2].ID != ids[0] {
		t.Errorf("tie order = %+v; want ids %d, %d, %d", all, ids[2], ids[1], ids[0])
	}
}
