package queue

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestLyricEditSetClear(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	id := seedUpgradeRow(t, dbh, "edit", "")
	if _, edited, err := q.LyricEdit(ctx, id); err != nil || edited {
		t.Fatalf("fresh row: edited=%v err=%v", edited, err)
	}
	if err := q.SetLyricEdit(ctx, id, -450); err != nil {
		t.Fatal(err)
	}
	off, edited, err := q.LyricEdit(ctx, id)
	if err != nil || !edited || off != -450 {
		t.Fatalf("after set: off=%d edited=%v err=%v", off, edited, err)
	}
	if err := q.ClearLyricEdit(ctx, id); err != nil {
		t.Fatal(err)
	}
	off, edited, err = q.LyricEdit(ctx, id)
	if err != nil || edited || off != 0 {
		t.Errorf("after clear: off=%d edited=%v err=%v", off, edited, err)
	}
}

// Each population is its own test so a mutation of one predicate reddens
// exactly the test that names it.
func TestEditedRowSkipsUpgradeUnsyncedArm(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	hold := upgradeNow.Add(-7 * 24 * time.Hour)
	id := seedUpgradeRow(t, dbh, "unsynced", "")
	if ids, _ := q.ListUpgradeCandidates(ctx, hold, 10); !slices.Contains(ids, id) {
		t.Fatalf("precondition: candidates %v lack %d", ids, id)
	}
	if err := q.SetLyricEdit(ctx, id, 300); err != nil {
		t.Fatal(err)
	}
	if ids, _ := q.ListUpgradeCandidates(ctx, hold, 10); slices.Contains(ids, id) {
		t.Errorf("edited row %d offered to the upgrade sweep (unsynced arm)", id)
	}
	if flipped, err := q.MarkUpgradeQueued(ctx, []int64{id}, hold); err != nil || len(flipped) != 0 {
		t.Errorf("edited row flipped: %v err=%v", flipped, err)
	}
}

func TestEditedRowSkipsUpgradeMissyncedArm(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	hold := upgradeNow.Add(-7 * 24 * time.Hour)
	id := seedUpgradeRow(t, dbh, "missynced",
		"outcome_type = 'synced', sync_tier = 'line', timing_outcome = 'mis_synced', timing_stamp_source = 'sweep'")
	if ids, _ := q.ListUpgradeCandidates(ctx, hold, 10); !slices.Contains(ids, id) {
		t.Fatalf("precondition: candidates %v lack %d", ids, id)
	}
	if err := q.SetLyricEdit(ctx, id, 300); err != nil {
		t.Fatal(err)
	}
	if ids, _ := q.ListUpgradeCandidates(ctx, hold, 10); slices.Contains(ids, id) {
		t.Errorf("edited row %d offered to the upgrade sweep (mis_synced arm)", id)
	}
	if flipped, err := q.MarkUpgradeQueued(ctx, []int64{id}, hold); err != nil || len(flipped) != 0 {
		t.Errorf("edited row flipped: %v err=%v", flipped, err)
	}
}

func TestEditedRowSkipsWordRecheck(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	id := seedWordCandidate(t, dbh, "line")
	opts := WordRecheckOptions{}
	if ids, _ := q.ListWordRecheckCandidates(ctx, opts); !slices.Contains(ids, id) {
		t.Fatalf("precondition: candidates %v lack %d", ids, id)
	}
	if err := q.SetLyricEdit(ctx, id, 300); err != nil {
		t.Fatal(err)
	}
	if ids, _ := q.ListWordRecheckCandidates(ctx, opts); slices.Contains(ids, id) {
		t.Errorf("edited row %d offered to the word recheck", id)
	}
	if n, _ := q.CountWordRecheckCandidates(ctx, opts); n != 0 {
		t.Errorf("recheck count = %d, want 0", n)
	}
}
