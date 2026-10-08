package commands

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/lyricblock"
)

func (f *qiFixture) blockCount(t *testing.T) int {
	t.Helper()
	sqlDB, err := db.Open(f.ctx, f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlDB.Close() }()
	var n int
	if err := sqlDB.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM lyric_blocks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *qiFixture) runBlock(t *testing.T, fn func(*bytes.Buffer) int) (string, int) {
	t.Helper()
	var out bytes.Buffer
	code := fn(&out)
	return out.String(), code
}

func TestScanMarkWrongDryRunChangesNothing(t *testing.T) {
	f := newQIFixture(t)
	backup := filepath.Join(t.TempDir(), "b.jsonl")
	out, code := f.runBlock(t, func(b *bytes.Buffer) int {
		return runMarkWrong(f.ctx, b, ScanMarkWrongCmd{ID: f.id, Backup: backup, Tail: true, ConfigPath: f.cfg})
	})
	if code != 0 || !strings.Contains(out, "dry run: nothing changed") || !strings.Contains(out, "lyric files that would be backed up: 1") {
		t.Fatalf("dry mark-wrong = %d:\n%s", code, out)
	}
	qiNoLeak(t, "dry mark-wrong", out, f)
	for _, line := range []string{"lyric files removed", "new blocks", "reopened", "cache entries invalidated"} {
		if strings.Contains(out, line) {
			t.Errorf("dry run prints outcome line %q:\n%s", line, out)
		}
	}
	if b, _ := os.ReadFile(f.lrcPath); string(b) != qiLRC {
		t.Errorf("lrc changed in a dry run: %q", b)
	}
	if n := f.blockCount(t); n != 0 {
		t.Errorf("dry run wrote %d blocks", n)
	}
	if _, err := os.Stat(backup); err == nil {
		t.Error("dry run wrote a backup file")
	}
}

func TestScanMarkWrongListUnblockRoundTrip(t *testing.T) {
	f := newQIFixture(t)
	backup := filepath.Join(t.TempDir(), "b.jsonl")
	out, code := f.runBlock(t, func(b *bytes.Buffer) int {
		return runMarkWrong(f.ctx, b, ScanMarkWrongCmd{ID: f.id, Yes: true, Backup: backup, ConfigPath: f.cfg})
	})
	if code != 0 || !strings.Contains(out, "lyric files backed up: 1") || !strings.Contains(out, "new blocks: 1") || !strings.Contains(out, "backup: "+backup) {
		t.Fatalf("mark-wrong = %d:\n%s", code, out)
	}
	qiNoLeak(t, "mark-wrong", out, f)
	if _, err := os.Stat(f.lrcPath); err == nil {
		t.Error("the .lrc should have been removed")
	}
	if raw, err := os.ReadFile(backup); err != nil || !strings.Contains(string(raw), `"op":"mark-wrong"`) {
		t.Fatalf("backup missing or wrong: %v %q", err, raw)
	}
	if f.blockCount(t) != 1 {
		t.Fatal("expected one block")
	}

	list := func(tail bool) (string, int) {
		return f.runBlock(t, func(b *bytes.Buffer) int {
			return runListBlocks(f.ctx, b, ScanListBlocksCmd{Tail: tail, ConfigPath: f.cfg})
		})
	}
	out, code = list(false)
	if code != 0 || !strings.Contains(out, "blocks: 1") {
		t.Fatalf("list-blocks = %d:\n%s", code, out)
	}
	qiNoLeak(t, "list-blocks", out, f)
	tailOut, _ := list(true)
	if !strings.Contains(tailOut, "block 1:") || !strings.Contains(tailOut, "work item") {
		t.Errorf("--tail lacks per-block detail:\n%s", tailOut)
	}

	unblock := func(a ScanUnblockCmd) (string, int) {
		a.ConfigPath = f.cfg
		return f.runBlock(t, func(b *bytes.Buffer) int { return runUnblock(f.ctx, b, a) })
	}
	out, code = unblock(ScanUnblockCmd{WorkItem: f.id})
	if code != 0 || !strings.Contains(out, "dry run: nothing changed") || !strings.Contains(out, "blocks that would be removed: 1") {
		t.Fatalf("dry unblock = %d:\n%s", code, out)
	}
	qiNoLeak(t, "dry unblock", out, f)
	if f.blockCount(t) != 1 {
		t.Fatal("dry-run unblock removed a block")
	}
	out, code = unblock(ScanUnblockCmd{WorkItem: f.id, Yes: true})
	if code != 0 || !strings.Contains(out, "blocks removed: 1") {
		t.Fatalf("unblock = %d:\n%s", code, out)
	}
	qiNoLeak(t, "unblock", out, f)
	if f.blockCount(t) != 0 {
		t.Error("block not removed")
	}
}

func TestScanUnblockByBlockIDAndRefusals(t *testing.T) {
	f := newQIFixture(t)
	if _, code := f.runBlock(t, func(b *bytes.Buffer) int {
		return runMarkWrong(f.ctx, b, ScanMarkWrongCmd{ID: f.id, Yes: true, Backup: filepath.Join(t.TempDir(), "b.jsonl"), ConfigPath: f.cfg})
	}); code != 0 {
		t.Fatal("mark-wrong failed")
	}
	unblock := func(a ScanUnblockCmd) (string, int) {
		a.ConfigPath = f.cfg
		return f.runBlock(t, func(b *bytes.Buffer) int { return runUnblock(f.ctx, b, a) })
	}
	if out, code := unblock(ScanUnblockCmd{ID: 999}); code != 1 || !strings.Contains(out, "refused: not found") {
		t.Errorf("unknown block id = %d:\n%s", code, out)
	}
	if out, code := unblock(ScanUnblockCmd{ID: 999, Yes: true}); code != 1 || !strings.Contains(out, "refused: not found") {
		t.Errorf("unknown block id --yes = %d:\n%s", code, out)
	}
	if _, code := unblock(ScanUnblockCmd{}); code != 2 {
		t.Errorf("no selector = %d, want 2", code)
	}
	if _, code := unblock(ScanUnblockCmd{ID: 1, WorkItem: 1}); code != 2 {
		t.Errorf("both selectors = %d, want 2", code)
	}
	if out, code := unblock(ScanUnblockCmd{ID: 1, Yes: true}); code != 0 || !strings.Contains(out, "blocks removed: 1") {
		t.Errorf("unblock by id = %d:\n%s", code, out)
	}
}

func TestScanMarkWrongRefusals(t *testing.T) {
	f := newQIFixture(t)
	run := func(a ScanMarkWrongCmd) (string, int) {
		a.ConfigPath = f.cfg
		return f.runBlock(t, func(b *bytes.Buffer) int { return runMarkWrong(f.ctx, b, a) })
	}
	if out, code := run(ScanMarkWrongCmd{ID: 9999}); code != 1 || !strings.Contains(out, "refused: not found") {
		t.Errorf("unknown id = %d:\n%s", code, out)
	}
	if _, code := run(ScanMarkWrongCmd{}); code != 2 {
		t.Errorf("no id = %d, want 2", code)
	}
	// A backup inside a library root would be replaced by the run itself.
	if _, code := run(ScanMarkWrongCmd{ID: f.id, Yes: true, Backup: filepath.Join(f.dir, "b.jsonl")}); code != 2 {
		t.Errorf("unsafe backup = %d, want 2", code)
	}
	if _, err := os.Stat(f.lrcPath); err != nil {
		t.Error("refused run removed the .lrc")
	}
}

// An error returned after Mark changed things is a failure, not a refusal, even
// when it wraps a refusal sentinel (the reopen step can wrap ErrBusy).
func TestScanMarkWrongRefusalMappingNeedsAZeroResult(t *testing.T) {
	err := fmt.Errorf("files removed but row not reopened: %w", lyricblock.ErrBusy)
	if w := markWrongRefusal(err, lyricblock.MarkResult{}); w != "in flight" {
		t.Errorf("zero result = %q, want in flight", w)
	}
	if w := markWrongRefusal(err, lyricblock.MarkResult{Files: 1, Removed: 1}); w != "" {
		t.Errorf("non-zero result = %q, want no refusal", w)
	}
}

// A work item with an empty artist key must not count another artist's block
// on the same title.
func TestScanUnblockDryRunEmptyKeyCountsOnlyItsOwnBlocks(t *testing.T) {
	f := newQIFixture(t)
	sqlDB, err := db.Open(f.ctx, f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var emptyID int64
	if err := sqlDB.QueryRowContext(f.ctx, `INSERT INTO work_queue (artist, title, artist_key, title_key, outdir, filename, status, outcome_type)
		VALUES ('', 'Quill Moor', '', 'quill moor', ?, 'other.flac', 'done', 'synced') RETURNING id`, f.dir).Scan(&emptyID); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.ExecContext(f.ctx, `INSERT INTO lyric_blocks (artist_key, title_key, fingerprint) VALUES ('vexa dunn', 'quill moor', 'fp-other')`); err != nil {
		t.Fatal(err)
	}
	_ = sqlDB.Close()
	out, code := f.runBlock(t, func(b *bytes.Buffer) int {
		return runUnblock(f.ctx, b, ScanUnblockCmd{WorkItem: emptyID, ConfigPath: f.cfg})
	})
	if code != 0 || !strings.Contains(out, "blocks that would be removed: 0") {
		t.Fatalf("dry unblock = %d:\n%s", code, out)
	}
}

// scan unblock --orphans clears only blocks whose track has no work_queue row;
// a dry run removes nothing, and the flag excludes --id and --work-item.
func TestScanUnblockOrphans(t *testing.T) {
	f := newQIFixture(t)
	if _, code := f.runBlock(t, func(b *bytes.Buffer) int {
		return runMarkWrong(f.ctx, b, ScanMarkWrongCmd{ID: f.id, Yes: true, Backup: filepath.Join(t.TempDir(), "b.jsonl"), ConfigPath: f.cfg})
	}); code != 0 {
		t.Fatal("mark-wrong failed")
	}
	sqlDB, err := db.Open(f.ctx, f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.ExecContext(f.ctx, `INSERT INTO lyric_blocks (artist_key, title_key, fingerprint) VALUES ('gone artist', 'gone title', 'fp-gone')`); err != nil {
		t.Fatal(err)
	}
	_ = sqlDB.Close()
	run := func(a ScanUnblockCmd) (string, int) {
		a.ConfigPath, a.Orphans = f.cfg, true
		return f.runBlock(t, func(b *bytes.Buffer) int { return runUnblock(f.ctx, b, a) })
	}
	before := f.blockCount(t)

	out, code := run(ScanUnblockCmd{Tail: true})
	if code != 0 || !strings.Contains(out, "orphaned blocks that would be removed: 1") || !strings.Contains(out, "gone artist") {
		t.Fatalf("dry --orphans = %d:\n%s", code, out)
	}
	if n := f.blockCount(t); n != before {
		t.Fatalf("dry run changed the block count %d -> %d", before, n)
	}
	out, code = run(ScanUnblockCmd{Yes: true})
	if code != 0 || !strings.Contains(out, "orphaned blocks removed: 1") || strings.Contains(out, "gone") {
		t.Fatalf("--orphans --yes = %d:\n%s", code, out)
	}
	if n := f.blockCount(t); n != before-1 {
		t.Errorf("blocks after = %d, want %d (the tracked block survives)", n, before-1)
	}
	if _, code := run(ScanUnblockCmd{ID: 1}); code != 2 {
		t.Errorf("--orphans with --id = %d, want 2", code)
	}
	if _, code := run(ScanUnblockCmd{WorkItem: 1}); code != 2 {
		t.Errorf("--orphans with --work-item = %d, want 2", code)
	}
}
