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
