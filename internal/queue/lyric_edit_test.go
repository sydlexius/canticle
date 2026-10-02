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

func TestLyricEditedAmong(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	edited := seedUpgradeRow(t, dbh, "edited", "")
	plain := seedUpgradeRow(t, dbh, "plain", "")
	if err := q.SetLyricEdit(ctx, edited, 300); err != nil {
		t.Fatal(err)
	}
	got, err := q.LyricEditedAmong(ctx, []int64{edited, plain, 99999})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[edited] {
		t.Errorf("LyricEditedAmong = %v, want only the edited row %d", got, edited)
	}
	if got, err := q.LyricEditedAmong(ctx, nil); err != nil || len(got) != 0 {
		t.Errorf("empty ids = %v, %v; want an empty map", got, err)
	}
	_ = dbh.Close()
	if _, err := q.LyricEditedAmong(ctx, []int64{edited}); err == nil {
		t.Error("closed database: want an error")
	}
}

// Each population is its own test so a mutation of one predicate reddens
// exactly the test that names it.
func TestEditedRowSkipsUpgradeUnsyncedArm(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	hold := upgradeNow.Add(-7 * 24 * time.Hour)
	id := seedUpgradeRow(t, dbh, "unsynced", "")
	ids, err := q.ListUpgradeCandidates(ctx, hold, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ids, id) {
		t.Fatalf("precondition: candidates %v lack %d", ids, id)
	}
	if err := q.SetLyricEdit(ctx, id, 300); err != nil {
		t.Fatal(err)
	}
	ids, err = q.ListUpgradeCandidates(ctx, hold, 10)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(ids, id) {
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
	ids, err := q.ListUpgradeCandidates(ctx, hold, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ids, id) {
		t.Fatalf("precondition: candidates %v lack %d", ids, id)
	}
	if err := q.SetLyricEdit(ctx, id, 300); err != nil {
		t.Fatal(err)
	}
	ids, err = q.ListUpgradeCandidates(ctx, hold, 10)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(ids, id) {
		t.Errorf("edited row %d offered to the upgrade sweep (mis_synced arm)", id)
	}
	if flipped, err := q.MarkUpgradeQueued(ctx, []int64{id}, hold); err != nil || len(flipped) != 0 {
		t.Errorf("edited row flipped: %v err=%v", flipped, err)
	}
}

// editedRowSkipsWordGenerate seeds one word-generate candidate shaped by set,
// confirms it is listed, marks it edited, and asserts it no longer is (#1228).
func editedRowSkipsWordGenerate(t *testing.T, arm, set string) {
	t.Helper()
	q, dbh := upgradeQueue(t)
	id := seedGenerateRow(t, dbh, arm, set)
	if got := listGenerate(t, q, genTestVersion, 10); !slices.Contains(got, id) {
		t.Fatalf("precondition: candidates %v lack %d", got, id)
	}
	if err := q.SetLyricEdit(context.Background(), id, 300); err != nil {
		t.Fatal(err)
	}
	if got := listGenerate(t, q, genTestVersion, 10); slices.Contains(got, id) {
		t.Errorf("edited row %d offered to word generation (%s arm)", id, arm)
	}
}

func TestEditedRowSkipsWordGenerateLineArm(t *testing.T) {
	editedRowSkipsWordGenerate(t, "line", "")
}

func TestEditedRowSkipsWordGenerateRetimeArm(t *testing.T) {
	editedRowSkipsWordGenerate(t, "retime", `outcome_type = 'unsynced', sync_tier = NULL, word_timing_state = NULL,
		word_timing_generation = NULL, timing_outcome = 'mis_synced', timing_stamp_source = 'fetch'`)
}

func TestEditedRowSkipsWordRecheck(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	id := seedWordCandidate(t, dbh, "line")
	opts := WordRecheckOptions{}
	ids, err := q.ListWordRecheckCandidates(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ids, id) {
		t.Fatalf("precondition: candidates %v lack %d", ids, id)
	}
	if err := q.SetLyricEdit(ctx, id, 300); err != nil {
		t.Fatal(err)
	}
	ids, err = q.ListWordRecheckCandidates(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(ids, id) {
		t.Errorf("edited row %d offered to the word recheck", id)
	}
	var n int
	n, err = q.CountWordRecheckCandidates(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("recheck count = %d, want 0", n)
	}
	// The flip is the path that actually arms the worker; it revalidates with
	// the same predicate, so an edited row must not flip either.
	flipped, err := q.MarkWordRecheckQueued(ctx, []int64{id}, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(flipped) != 0 {
		t.Errorf("edited row flipped to the word recheck: %v", flipped)
	}
}
