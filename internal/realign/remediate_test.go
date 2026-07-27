package realign

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
)

// fixtureBody is the .lrc content every remediation fixture starts from. These
// tests exercise the ACTION machinery, not the predicate, so the body only has
// to be a recognizable file whose bytes a quarantine must preserve.
const fixtureBody = "[00:01.00]alpha\n"

// remediateFixture writes a .lrc under a temp root and returns (root, lrcPath).
func remediateFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	lrc := filepath.Join(root, "track.lrc")
	if err := os.WriteFile(lrc, []byte(fixtureBody), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return root, lrc
}

// applyOne runs a single move through Apply and returns its outcome plus the
// backup file's contents.
func applyOne(t *testing.T, mv Move) (Applied, string) {
	t.Helper()
	backup := filepath.Join(t.TempDir(), "backup.jsonl")
	applied, err := New(nil, config.RealignConfig{}).Apply([]Move{mv}, backup, Policy{AllowHeuristic: true})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) != 1 {
		t.Fatalf("applied %d outcomes, want 1", len(applied))
	}
	b, rerr := os.ReadFile(backup) //nolint:gosec // test fixture path
	if rerr != nil {
		return applied[0], ""
	}
	return applied[0], string(b)
}

// TestApplyQuarantineMovesRatherThanDeletes: the reversibility rail at the apply
// layer. The file leaves the library but exists, byte-identical, at the target.
func TestApplyQuarantineMovesRatherThanDeletes(t *testing.T) {
	root, lrc := remediateFixture(t)
	target := filepath.Join(t.TempDir(), "q", "sub", "track.lrc")

	got, backup := applyOne(t, Move{Orphan: lrc, Target: target, Kind: KindQuarantine, Eligible: true})
	if got.Err != nil {
		t.Fatalf("quarantine: %v", got.Err)
	}
	if _, err := os.Stat(lrc); !os.IsNotExist(err) {
		t.Errorf("the source survived a quarantine: %v", err)
	}
	b, err := os.ReadFile(target) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("the quarantined file is missing -- it was DELETED, not moved: %v", err)
	}
	if string(b) != fixtureBody {
		t.Errorf("quarantined content = %q, want %q", b, fixtureBody)
	}
	if !strings.Contains(backup, `"kind":"quarantine"`) {
		t.Errorf("backup record does not carry the kind: %s", backup)
	}
	_ = root
}

// TestApplyDemoteWritesTextBeforeMovingTheLrc: ordering matters, so the .txt is
// present and the .lrc is recoverable from quarantine.
func TestApplyDemoteWritesTextBeforeMovingTheLrc(t *testing.T) {
	_, lrc := remediateFixture(t)
	quarantined := filepath.Join(t.TempDir(), "q", "track.lrc")
	txt := strings.TrimSuffix(lrc, ".lrc") + ".txt"

	got, backup := applyOne(t, Move{
		Orphan: lrc, Target: quarantined, Kind: KindDemote, Eligible: true,
		TextPath: txt, TextBody: "alpha\n",
	})
	if got.Err != nil {
		t.Fatalf("demote: %v", got.Err)
	}
	b, err := os.ReadFile(txt) //nolint:gosec // test fixture path
	if err != nil || string(b) != "alpha\n" {
		t.Fatalf("demoted .txt = %q, err %v", b, err)
	}
	if _, err := os.Stat(quarantined); err != nil {
		t.Errorf("the demoted .lrc is not recoverable: %v", err)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(backup)), &rec); err != nil {
		t.Fatalf("backup is not valid JSONL: %v (%s)", err, backup)
	}
	if rec["text_path"] != txt {
		t.Errorf("backup omits the demoted text path, so the undo is incomplete: %v", rec)
	}
}

// TestApplyDemoteLeavesTheLrcWhenTheTextWriteFails: backup-first ordering means
// a failed .txt write costs nothing.
func TestApplyDemoteLeavesTheLrcWhenTheTextWriteFails(t *testing.T) {
	_, lrc := remediateFixture(t)
	// A text path inside a non-directory can never be created.
	bad := filepath.Join(lrc, "nested", "track.txt")

	got, _ := applyOne(t, Move{
		Orphan: lrc, Target: filepath.Join(t.TempDir(), "q.lrc"), Kind: KindDemote, Eligible: true,
		TextPath: bad, TextBody: "alpha\n",
	})
	if got.Err == nil {
		t.Fatal("want an error when the demoted .txt cannot be written")
	}
	if _, err := os.Stat(lrc); err != nil {
		t.Errorf("the .lrc was moved despite a failed .txt write: %v", err)
	}
}

// TestApplyDemoteRefusesAnEmptyBody: never write an empty sidecar.
func TestApplyDemoteRefusesAnEmptyBody(t *testing.T) {
	_, lrc := remediateFixture(t)
	got, _ := applyOne(t, Move{
		Orphan: lrc, Target: filepath.Join(t.TempDir(), "q.lrc"), Kind: KindDemote, Eligible: true,
		TextPath: strings.TrimSuffix(lrc, ".lrc") + ".txt", TextBody: "",
	})
	if got.Err == nil {
		t.Fatal("want an error rather than an empty .txt")
	}
}

// TestApplyDemoteKeepsASettledTxt: existing content on disk outranks anything
// this pass would write.
func TestApplyDemoteKeepsASettledTxt(t *testing.T) {
	_, lrc := remediateFixture(t)
	txt := strings.TrimSuffix(lrc, ".lrc") + ".txt"
	if err := os.WriteFile(txt, []byte("settled\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _ := applyOne(t, Move{
		Orphan: lrc, Target: filepath.Join(t.TempDir(), "q.lrc"), Kind: KindDemote, Eligible: true,
		TextPath: txt, TextBody: "replacement\n",
	})
	if got.Err != nil {
		t.Fatalf("demote: %v", got.Err)
	}
	b, _ := os.ReadFile(txt) //nolint:gosec // test fixture path
	if string(b) != "settled\n" {
		t.Errorf("a settled .txt was overwritten: %q", b)
	}
}

// TestApplyPurgeDeletes: the opt-in irreversible path.
func TestApplyPurgeDeletes(t *testing.T) {
	_, lrc := remediateFixture(t)
	got, backup := applyOne(t, Move{Orphan: lrc, Kind: KindPurge, Eligible: true})
	if got.Err != nil {
		t.Fatalf("purge: %v", got.Err)
	}
	if _, err := os.Stat(lrc); !os.IsNotExist(err) {
		t.Errorf("purge did not delete: %v", err)
	}
	if !strings.Contains(backup, `"kind":"purge"`) {
		t.Errorf("purge is not recorded in the audit trail: %s", backup)
	}
}

// TestApplyRemediationRefusesASymlink: a link must not redirect a delete or a
// move outside the library root.
func TestApplyRemediationRefusesASymlink(t *testing.T) {
	root, lrc := remediateFixture(t)
	outside := filepath.Join(t.TempDir(), "real.lrc")
	if err := os.WriteFile(outside, []byte("real\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Remove(lrc); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(outside, lrc); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, kind := range []string{KindPurge, KindQuarantine} {
		got, _ := applyOne(t, Move{Orphan: lrc, Target: filepath.Join(root, "q.lrc"), Kind: kind, Eligible: true})
		if got.Err == nil {
			t.Errorf("kind %s: a symlinked sidecar must be refused", kind)
		}
		if _, err := os.Stat(outside); err != nil {
			t.Fatalf("kind %s: the symlink target was destroyed: %v", kind, err)
		}
	}
}

// TestApplyRemediationRefusesAMissingSource.
func TestApplyRemediationRefusesAMissingSource(t *testing.T) {
	root := t.TempDir()
	got, _ := applyOne(t, Move{
		Orphan: filepath.Join(root, "absent.lrc"), Target: filepath.Join(root, "q.lrc"),
		Kind: KindQuarantine, Eligible: true,
	})
	if got.Err == nil {
		t.Fatal("want an error for a source that is not there")
	}
}

// TestApplyRemediationRefusesAnOccupiedTarget: never clobber.
func TestApplyRemediationRefusesAnOccupiedTarget(t *testing.T) {
	_, lrc := remediateFixture(t)
	target := filepath.Join(t.TempDir(), "occupied.lrc")
	if err := os.WriteFile(target, []byte("existing\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _ := applyOne(t, Move{Orphan: lrc, Target: target, Kind: KindQuarantine, Eligible: true})
	if got.Err == nil {
		t.Fatal("want an error rather than a clobbered destination")
	}
	b, _ := os.ReadFile(target) //nolint:gosec // test fixture path
	if string(b) != "existing\n" {
		t.Errorf("the destination was clobbered: %q", b)
	}
}

// TestApplyRemediationRejectsAnUnknownKind: an unrecognized action must never
// fall through to a filesystem change.
func TestApplyRemediationRejectsAnUnknownKind(t *testing.T) {
	_, lrc := remediateFixture(t)
	got, _ := applyOne(t, Move{Orphan: lrc, Target: filepath.Join(t.TempDir(), "q.lrc"), Kind: "shred", Eligible: true})
	if got.Err == nil {
		t.Fatal("want an error for an unknown remediation kind")
	}
	if _, err := os.Stat(lrc); err != nil {
		t.Errorf("an unknown kind touched the file: %v", err)
	}
}

// TestApplyRemediationRequiresATarget: a move with nowhere to go is an error,
// never a silent delete.
func TestApplyRemediationRequiresATarget(t *testing.T) {
	_, lrc := remediateFixture(t)
	got, _ := applyOne(t, Move{Orphan: lrc, Kind: KindQuarantine, Eligible: true})
	if got.Err == nil {
		t.Fatal("want an error for a quarantine with no target")
	}
	if _, err := os.Stat(lrc); err != nil {
		t.Errorf("a targetless quarantine removed the file: %v", err)
	}
}

// TestApplyDemoteRequiresATextPath.
func TestApplyDemoteRequiresATextPath(t *testing.T) {
	_, lrc := remediateFixture(t)
	got, _ := applyOne(t, Move{Orphan: lrc, Target: filepath.Join(t.TempDir(), "q.lrc"), Kind: KindDemote, Eligible: true})
	if got.Err == nil {
		t.Fatal("want an error for a demote with no text path")
	}
}

// TestRenameMovesKeepTheirZeroValueKind guards the compatibility claim: a Move
// built before the remediation kinds existed still renames.
func TestRenameMovesKeepTheirZeroValueKind(t *testing.T) {
	_, lrc := remediateFixture(t)
	target := filepath.Join(filepath.Dir(lrc), "renamed.lrc")
	got, backup := applyOne(t, Move{Orphan: lrc, Target: target, Method: "exact", Eligible: true})
	if got.Err != nil {
		t.Fatalf("rename: %v", got.Err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("the rename did not happen: %v", err)
	}
	if strings.Contains(backup, `"kind"`) {
		t.Errorf("a plain rename must not gain a kind field: %s", backup)
	}
}
