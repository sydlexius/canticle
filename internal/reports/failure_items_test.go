package reports_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/sydlexius/canticle/internal/failsig"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
)

func seedFailureItems(t *testing.T) *reports.Repo {
	t.Helper()
	sqlDB := openTestDB(t)
	// Raw last_error values that differ but normalize to one signature, plus a
	// distinct one, a no-reason row with attempts, and a row the #789 guard hides.
	for i, le := range []string{
		"write /music/a111/x.lrc: permission denied",
		"write /music/a222/x.lrc: permission denied",
		"write /music/a333/x.lrc: permission denied",
		"musixmatch: unexpected matcher status_code 500",
		"",
	} {
		insertWorkItem(t, sqlDB, workItem{
			artist: "A", title: fmt.Sprintf("f%d", i), status: "failed", lastError: le, attempts: 1,
		})
	}
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "guarded", status: "failed", attempts: 0})
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "d1", status: "deferred", lastError: "no match", missCount: 1})
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "d2", status: "deferred", lastError: "no match", missCount: 2})
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
	for i, it := range items {
		if it.Class != failsig.Persistent {
			t.Errorf("class = %q, want persistent", it.Class)
		}
		if it.Title == "" || it.Artist != "A" {
			t.Errorf("display fields missing: %+v", it)
		}
		if i > 0 && items[i-1].UpdatedAt == it.UpdatedAt && items[i-1].ID < it.ID {
			t.Errorf("not newest first: ids %d then %d", items[i-1].ID, it.ID)
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
