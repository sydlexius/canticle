package commands

import (
	"context"
	"database/sql"
	"io"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/revalidate"
)

// #1130: a remediation changes the file a work_queue row points at, so the row's
// outcome_type / sync_tier must stop describing a synced .lrc. Both the serve
// sweep and `revalidate --apply` are covered, one case per action, against real
// SQLite.

const (
	lateCueBody     = "[00:10.00]alpha\n[02:30.00]beta\n" // 150s cue vs a 120s track
	categoricalBody = "[00:10.00]alpha\n[05:00.00]beta\n" // 300s cue vs a 120s track
)

var fileStateCases = []struct {
	name        string
	body        string
	action      revalidate.Action
	wantOutcome string // "" means NULL
}{
	{"demote", lateCueBody, revalidate.ActionDemote, "unsynced"},
	{"quarantine", categoricalBody, revalidate.ActionQuarantine, ""},
	{"purge", categoricalBody, revalidate.ActionPurge, ""},
	// Demote under --purge builds a KindPurge move that still writes the .txt.
	{"demote+purge", lateCueBody, revalidate.ActionDemote, "unsynced"},
}

// readFileState returns the row's outcome_type and sync_tier ("" for NULL).
func readFileState(t *testing.T, dbPath string) (outcome, tier string) {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	var o, s sql.NullString
	if err := sqlDB.QueryRow(`SELECT outcome_type, sync_tier FROM work_queue`).Scan(&o, &s); err != nil {
		t.Fatalf("read file state: %v", err)
	}
	return o.String, s.String
}

// tierTheRow gives the seeded synced row the word tier a real fetch would have.
func tierTheRow(t *testing.T, dbPath string) {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	if _, err := sqlDB.Exec(`UPDATE work_queue SET sync_tier = 'word'`); err != nil {
		t.Fatalf("seed tier: %v", err)
	}
}

func TestSweepRestampsTheFileStateAfterEachAction(t *testing.T) {
	for _, tc := range fileStateCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			job, _, root, lrc := sweepFixture(t, nil)
			dbPath := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(lrc))), "sweep.db")
			writeFile(t, lrc, tc.body)
			tierTheRow(t, dbPath)
			job.rev = revalidate.New(
				func(context.Context, string, int64, int64) (int, bool, error) { return 120, true, nil },
				revalidate.Options{
					Roots:             []string{root},
					MisSyncedAction:   tc.action,
					CategoricalAction: tc.action,
					QuarantineDir:     filepath.Join(t.TempDir(), "quarantine"),
				},
			)
			res, err := job.runCycle(ctx)
			if err != nil || res.Remedied != 1 {
				t.Fatalf("runCycle remedied=%d err=%v; the fixture did not remediate", res.Remedied, err)
			}
			outcome, tier := readFileState(t, dbPath)
			if outcome != tc.wantOutcome || tier != "" {
				t.Errorf("after %s: outcome_type=%q sync_tier=%q, want %q and NULL", tc.name, outcome, tier, tc.wantOutcome)
			}
		})
	}
}

func TestRevalidateApplyRestampsTheFileStateAfterEachAction(t *testing.T) {
	for _, tc := range fileStateCases {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, root, lrc := revalidateFixture(t, tc.body)
			audio := lrc[:len(lrc)-len(".lrc")] + ".mp3"
			seedRevalidateRow(t, cfgPath, audio)
			dbPath := filepath.Join(filepath.Dir(cfgPath), "canticle.db")
			tierTheRow(t, dbPath)
			cmd := RevalidateCmd{Roots: []string{root}, ConfigPath: cfgPath, Apply: true,
				QuarantineDir: filepath.Join(t.TempDir(), "q"), Purge: tc.action == revalidate.ActionPurge || tc.name == "demote+purge"}
			if code := runRevalidate(t.Context(), io.Discard, cmd); code != 0 {
				t.Fatalf("apply exit = %d", code)
			}
			outcome, tier := readFileState(t, dbPath)
			if outcome != tc.wantOutcome || tier != "" {
				t.Errorf("after %s: outcome_type=%q sync_tier=%q, want %q and NULL", tc.name, outcome, tier, tc.wantOutcome)
			}
		})
	}
}

// TestRestampFileStateLeavesANonRemediatedRowAlone: a finding with no action
// (off, or a verdict that does not remediate) must not touch the row.
func TestRestampFileStateLeavesANonRemediatedRowAlone(t *testing.T) {
	ok, err := restampFileState(context.Background(), nil, revalidate.Finding{ID: 1})
	if ok || err != nil {
		t.Errorf("restampFileState with no action = %v, %v; want false, nil (and no queue call)", ok, err)
	}
}
