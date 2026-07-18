package commands

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
)

func setupReconcileLRC(t *testing.T) (cfgPath, root string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfgPath = filepath.Join(dir, "config.toml")
	root = filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	content := "[db]\npath = \"" + strings.ReplaceAll(dbPath, `\`, `\\`) + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}
	return cfgPath, root
}

func TestRunReconcileLRC_DryRunApplyIdempotent(t *testing.T) {
	cfgPath, root := setupReconcileLRC(t)
	stacked := filepath.Join(root, "a", "stacked.lrc")
	clean := filepath.Join(root, "a", "clean.lrc")
	a2 := filepath.Join(root, "a", "word.lrc")
	if err := os.MkdirAll(filepath.Dir(stacked), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, stacked, "[00:39.26][00:47.06][01:03.06]Chorus\n[01:10.00]Line\n")
	mustWrite(t, clean, "[00:10.00]a\n[00:20.00]b\n")
	mustWrite(t, a2, "[00:12.00]<00:12.00>Word<00:13.00>sync\n")

	// Dry run: reports one candidate, writes nothing.
	var dry bytes.Buffer
	if rc := runReconcileLRC(context.Background(), &dry, ScanReconcileLRCCmd{ConfigPath: cfgPath}); rc != 0 {
		t.Fatalf("dry-run rc=%d out=%s", rc, dry.String())
	}
	if !strings.Contains(dry.String(), "would rewrite 1") {
		t.Errorf("dry-run output: %q", dry.String())
	}
	if got := readFile(t, stacked); !strings.Contains(got, "][") {
		t.Error("dry run mutated the stacked file")
	}

	// Apply: rewrites the stacked file, backs it up, leaves the A2 file untouched.
	var apply bytes.Buffer
	if rc := runReconcileLRC(context.Background(), &apply, ScanReconcileLRCCmd{ConfigPath: cfgPath, Yes: true}); rc != 0 {
		t.Fatalf("apply rc=%d out=%s", rc, apply.String())
	}
	if !strings.Contains(apply.String(), "rewrote 1") {
		t.Errorf("apply output: %q", apply.String())
	}
	if got := readFile(t, stacked); strings.Contains(got, "][") {
		t.Errorf("stacked file not expanded: %q", got)
	}
	if _, err := os.Stat(stacked + ".orig"); err != nil {
		t.Errorf(".orig backup missing: %v", err)
	}
	if _, err := os.Stat(a2 + ".orig"); !os.IsNotExist(err) {
		t.Error("A2 word-sync file was wrongly backed up / touched")
	}

	// Re-run apply: idempotent, nothing left to do.
	var again bytes.Buffer
	if rc := runReconcileLRC(context.Background(), &again, ScanReconcileLRCCmd{ConfigPath: cfgPath, Yes: true}); rc != 0 {
		t.Fatalf("re-run rc=%d", rc)
	}
	if !strings.Contains(again.String(), "rewrote 0") {
		t.Errorf("re-run not idempotent: %q", again.String())
	}
}

func TestRunReconcileLRC_LibraryFilterNotFoundAndBadConfig(t *testing.T) {
	cfgPath, root := setupReconcileLRC(t)
	mustWrite(t, filepath.Join(root, "s.lrc"), "[00:30.00][01:05.00]C\n")

	// --library by name resolves and reports the candidate.
	var byLib bytes.Buffer
	if rc := runReconcileLRC(context.Background(), &byLib, ScanReconcileLRCCmd{ConfigPath: cfgPath, Library: "lib"}); rc != 0 {
		t.Fatalf("--library rc=%d out=%s", rc, byLib.String())
	}
	if !strings.Contains(byLib.String(), "would rewrite 1") {
		t.Errorf("--library output: %q", byLib.String())
	}

	// Unknown --library returns non-zero with a not-found message.
	var missing bytes.Buffer
	if rc := runReconcileLRC(context.Background(), &missing, ScanReconcileLRCCmd{ConfigPath: cfgPath, Library: "nope"}); rc == 0 {
		t.Error("unknown library should return non-zero")
	}
	if !strings.Contains(missing.String(), "not found") {
		t.Errorf("not-found output: %q", missing.String())
	}

	// A malformed config returns non-zero.
	bad := filepath.Join(t.TempDir(), "bad.toml")
	mustWrite(t, bad, "this is not = valid [toml\n")
	if rc := runReconcileLRC(context.Background(), &bytes.Buffer{}, ScanReconcileLRCCmd{ConfigPath: bad}); rc == 0 {
		t.Error("malformed config should return non-zero")
	}
}

func TestRunScanCmd_DispatchesReconcileLRC(t *testing.T) {
	cfgPath, root := setupReconcileLRC(t)
	mustWrite(t, filepath.Join(root, "s.lrc"), "[00:30.00][01:05.00]C\n")

	// Direct sub-config.
	var buf bytes.Buffer
	if rc := runScanCmd(context.Background(), &buf, ScanCmd{ReconcileLRC: &ScanReconcileLRCCmd{ConfigPath: cfgPath}}); rc != 0 {
		t.Fatalf("dispatch rc=%d out=%s", rc, buf.String())
	}
	if !strings.Contains(buf.String(), "would rewrite 1") {
		t.Errorf("dispatch output: %q", buf.String())
	}

	// Parent --config inherited when the subcommand omits it.
	var buf2 bytes.Buffer
	if rc := runScanCmd(context.Background(), &buf2, ScanCmd{ConfigPath: cfgPath, ReconcileLRC: &ScanReconcileLRCCmd{}}); rc != 0 {
		t.Fatalf("inherited-config rc=%d out=%s", rc, buf2.String())
	}
}

func TestRunReconcileLRC_DoesNotDeleteOperatorBackupOnZeroRewrite(t *testing.T) {
	cfgPath, root := setupReconcileLRC(t)
	mustWrite(t, filepath.Join(root, "clean.lrc"), "[00:10.00]a\n") // nothing to rewrite

	// Operator points --backup at a pre-existing file holding their own data.
	opBackup := filepath.Join(t.TempDir(), "operator.jsonl")
	mustWrite(t, opBackup, "OPERATOR DATA\n")

	var buf bytes.Buffer
	if rc := runReconcileLRC(context.Background(), &buf, ScanReconcileLRCCmd{ConfigPath: cfgPath, Yes: true, Backup: opBackup}); rc != 0 {
		t.Fatalf("rc=%d out=%s", rc, buf.String())
	}
	if !strings.Contains(buf.String(), "rewrote 0") {
		t.Errorf("expected zero rewrites: %q", buf.String())
	}
	// C1: the operator's pre-existing file must be untouched (never deleted).
	if got := readFile(t, opBackup); got != "OPERATOR DATA\n" {
		t.Errorf("operator backup file was modified/deleted: %q", got)
	}
}

func TestRunReconcileLRC_NoLibraryRoots(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfgPath := filepath.Join(dir, "config.toml")
	mustWrite(t, cfgPath, "[db]\npath = \""+strings.ReplaceAll(dbPath, `\`, `\\`)+"\"\n")
	// Open the DB (creates schema) but add no library.
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	if rc := runReconcileLRC(ctx, &out, ScanReconcileLRCCmd{ConfigPath: cfgPath}); rc != 0 {
		t.Fatalf("rc=%d out=%s", rc, out.String())
	}
	if !strings.Contains(out.String(), "no library roots") {
		t.Errorf("output: %q", out.String())
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRunReconcileLRC_ReportsBlockedWithPointerToPaths pins the operator-facing
// half of #487. A blocked file is the ONLY tally that demands follow-up, and the
// count alone is useless -- the paths live in the WARN lines. So the summary must
// carry a distinct blocked count (not folded into skipped, or a zero blocked
// count would not be a real all-clear) and, when non-zero, point at where the
// paths are.
func TestRunReconcileLRC_ReportsBlockedWithPointerToPaths(t *testing.T) {
	cfgPath, root := setupReconcileLRC(t)
	blocked := filepath.Join(root, "blocked.lrc")

	// Still stacked, with a pre-existing .orig barring a verifiable rewrite.
	mustWrite(t, blocked, "[00:39.26][00:47.06]Chorus\n")
	mustWrite(t, blocked+".orig", "operator's own backup\n")

	var buf bytes.Buffer
	// rc=1 deliberately (#487): a blocked file is still stacked and was left
	// untouched, so a script must not read the run as an all-clear while the WARN
	// tells a human that action is required.
	if rc := runReconcileLRC(context.Background(), &buf, ScanReconcileLRCCmd{ConfigPath: cfgPath, Yes: true}); rc != 1 {
		t.Fatalf("rc=%d; want 1 when a file is blocked. out=%s", rc, buf.String())
	}
	out := buf.String()

	if !strings.Contains(out, "1 blocked") {
		t.Errorf("summary must carry a distinct blocked count; got:\n%s", out)
	}
	if !strings.Contains(out, "see the BLOCKED warnings above for paths") {
		t.Errorf("a non-zero blocked count must point at the paths; got:\n%s", out)
	}
	// The operator's backup is theirs: a blocked file must never be rewritten and
	// the .orig must survive untouched.
	if got := readFile(t, blocked+".orig"); !strings.Contains(got, "operator's own backup") {
		t.Errorf(".orig was clobbered; it is the operator's file: %q", got)
	}
	if got := readFile(t, blocked); !strings.Contains(got, "][") {
		t.Error("blocked file was rewritten despite the .orig barring it")
	}
}

// A clean run must not emit the follow-up line: it fires only when there is
// something to follow up on.
func TestRunReconcileLRC_NoBlockedPointerWhenNoneBlocked(t *testing.T) {
	cfgPath, root := setupReconcileLRC(t)
	mustWrite(t, filepath.Join(root, "stacked.lrc"), "[00:39.26][00:47.06]Chorus\n")

	var buf bytes.Buffer
	if rc := runReconcileLRC(context.Background(), &buf, ScanReconcileLRCCmd{ConfigPath: cfgPath, Yes: true}); rc != 0 {
		t.Fatalf("rc=%d out=%s", rc, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "0 blocked") {
		t.Errorf("summary should report a zero blocked count as a real all-clear; got:\n%s", out)
	}
	if strings.Contains(out, "see the BLOCKED warnings above") {
		t.Errorf("the follow-up pointer must not fire when nothing is blocked; got:\n%s", out)
	}
}
