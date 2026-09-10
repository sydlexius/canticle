package reports_test

import (
	"context"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/reports"
)

// #929: LastLRCNormalization sources the "last LRC normalization" dashboard
// summary from the maintenance_markers row internal/commands.
// markLRCNormalizeApply writes -- the read side of that seam. Real SQLite,
// no mocks, per the repo's testing convention.

func TestLastLRCNormalization_NeverRun(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	got, err := repo.LastLRCNormalization(ctx)
	if err != nil {
		t.Fatalf("LastLRCNormalization on a fresh db: %v", err)
	}
	if got.Ever {
		t.Errorf("fresh db: Ever = true, want false")
	}
	if got.Normalized != 0 || !got.CompletedAt.IsZero() {
		t.Errorf("fresh db: got %+v, want zero value", got)
	}
}

func TestLastLRCNormalization_ReadsStampedMarker(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	const completedAt = "2026-09-01T12:30:00Z"
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO maintenance_markers (name, completed_at, detail_count) VALUES (?, ?, ?)`,
		reports.MaintenanceMarkerLRCNormalize, completedAt, 7); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	got, err := repo.LastLRCNormalization(ctx)
	if err != nil {
		t.Fatalf("LastLRCNormalization: %v", err)
	}
	if !got.Ever {
		t.Fatal("Ever = false, want true after a marker row exists")
	}
	if got.Normalized != 7 {
		t.Errorf("Normalized = %d, want 7", got.Normalized)
	}
	wantTime, _ := time.Parse(time.RFC3339, completedAt)
	if !got.CompletedAt.Equal(wantTime) {
		t.Errorf("CompletedAt = %v, want %v", got.CompletedAt, wantTime)
	}
}

// A row present under an UNRELATED marker name (e.g. the identity-repair or
// lrc-stacked-check markers, which never carry a detail_count) must not be
// picked up -- the query is scoped to MaintenanceMarkerLRCNormalize by name.
func TestLastLRCNormalization_IgnoresOtherMarkers(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO maintenance_markers (name) VALUES ('identity_backfill_466')`); err != nil {
		t.Fatalf("seed unrelated marker: %v", err)
	}

	got, err := repo.LastLRCNormalization(ctx)
	if err != nil {
		t.Fatalf("LastLRCNormalization: %v", err)
	}
	if got.Ever {
		t.Errorf("an unrelated marker row must not read as Ever=true: %+v", got)
	}
}

// A zero-Normalized stamped row (the pass ran and genuinely rewrote nothing)
// must still read as Ever=true -- distinguishing "ran, found nothing" from
// "never ran" is the whole point of the Ever field.
func TestLastLRCNormalization_ZeroCountStillReadsAsRun(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO maintenance_markers (name, completed_at, detail_count) VALUES (?, ?, ?)`,
		reports.MaintenanceMarkerLRCNormalize, "2026-09-01T00:00:00Z", 0); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	got, err := repo.LastLRCNormalization(ctx)
	if err != nil {
		t.Fatalf("LastLRCNormalization: %v", err)
	}
	if !got.Ever {
		t.Error("Ever = false, want true for a zero-count stamped row")
	}
	if got.Normalized != 0 {
		t.Errorf("Normalized = %d, want 0", got.Normalized)
	}
}

func TestLastLRCNormalization_ClosedDB(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	if _, err := repo.LastLRCNormalization(ctx); err == nil {
		t.Error("want an error against a closed database")
	}
}

// #929 constraint: this feature must not add a new reports.ResultClass or
// otherwise change what Recent Outcomes shows. RecentOutcomes stays
// functionally unchanged -- this pins that a normal work_queue completion
// still classifies exactly as before and that no lrc-normalized-shaped
// class exists.
func TestRecentOutcomes_UnaffectedByLRCNormalizationMarker(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	insertWorkItem(t, sqlDB, workItem{
		artist: "A", title: "T", status: "done",
		completedAt: "2026-09-01T10:00:00Z", outcomeType: "synced",
	})
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO maintenance_markers (name, completed_at, detail_count) VALUES (?, ?, ?)`,
		reports.MaintenanceMarkerLRCNormalize, "2026-09-01T12:00:00Z", 3); err != nil {
		t.Fatalf("seed marker: %v", err)
	}

	got, err := repo.RecentOutcomes(ctx, 20)
	if err != nil {
		t.Fatalf("RecentOutcomes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d outcomes, want exactly 1 (the maintenance marker must not surface here): %+v", len(got), got)
	}
	if got[0].Result != reports.ResultSynced {
		t.Errorf("Result = %q, want %q", got[0].Result, reports.ResultSynced)
	}
}
