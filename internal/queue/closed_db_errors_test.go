package queue

import (
	"context"
	"errors"
	"testing"
)

func TestClassedErrorKeepsTheOriginalChain(t *testing.T) {
	base := errors.New("boom")
	err := WithFailureClass(base, FailureNetwork)
	if !errors.Is(err, base) {
		t.Errorf("errors.Is(classed, base) = false, want the chain preserved")
	}
	if got := FailureClassOf(err); got != FailureNetwork {
		t.Errorf("class = %q, want %q", got, FailureNetwork)
	}
}

func TestLyricEditAndManualInstrumentalReadsReportADatabaseFailure(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	if err := dbh.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.SetLyricEdit(ctx, 1, 5); err == nil {
		t.Error("SetLyricEdit on a closed database returned nil")
	}
	if err := q.ClearLyricEdit(ctx, 1); err == nil {
		t.Error("ClearLyricEdit on a closed database returned nil")
	}
	if _, marked, err := q.ManualInstrumental(ctx, 1); err == nil || marked {
		t.Errorf("ManualInstrumental = marked %v, err %v, want an error and not marked", marked, err)
	}
	if n, err := q.ClearDone(ctx); err == nil || n != 0 {
		t.Errorf("ClearDone = %d, %v, want 0 and an error", n, err)
	}
	if n, err := q.CountDone(ctx); err == nil || n != 0 {
		t.Errorf("CountDone = %d, %v, want 0 and an error", n, err)
	}
	if err := q.Release(ctx, 1); err == nil {
		t.Error("Release on a closed database returned nil")
	}
	if ok, err := q.UnsettleInstrumental(ctx, 1); err == nil || ok {
		t.Errorf("UnsettleInstrumental = %v, %v, want false and an error", ok, err)
	}
	if ok, err := q.ResetInstrumentalToUnclassified(ctx, 1); err == nil || ok {
		t.Errorf("ResetInstrumentalToUnclassified = %v, %v, want false and an error", ok, err)
	}
	if _, err := q.ManualInstrumentalAmong(ctx, []int64{1}); err == nil {
		t.Error("ManualInstrumentalAmong on a closed database returned nil")
	}
	if err := q.SetOutcomeType(ctx, 1, "synced"); err == nil {
		t.Error("SetOutcomeType on a closed database returned nil")
	}
	if n, err := q.CountUpgradeInFlight(ctx); err == nil || n != 0 {
		t.Errorf("CountUpgradeInFlight = %d, %v, want 0 and an error", n, err)
	}
}
