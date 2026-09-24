package scanner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/testutil"
)

// caseSensitiveFS reports whether dir's filesystem distinguishes "a" from "A"
// in a file name. macOS (APFS, the default local dev box) and Windows are
// case-insensitive; CI (ubuntu-latest, ext4/tmpfs) and the production
// deployment target are case-sensitive. Mirrors internal/sidecar's and
// internal/revalidate's helpers of the same name and reasoning; duplicated
// rather than exported because it is test-only and this package has no
// test-support package to share it from.
func caseSensitiveFS(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "casecheck.tmp")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		t.Fatalf("write case probe: %v", err)
	}
	_, err := os.Stat(filepath.Join(dir, "CASECHECK.tmp"))
	return os.IsNotExist(err)
}

// writeCaseFixture writes a tagged FLAC at stem+".flac" so ScanLibrary can
// read real tags from it.
func writeCaseFixture(t *testing.T, dir, stem string) {
	t.Helper()
	if err := testutil.WriteFLACFileWithComments(dir, stem+".flac", 44100, 44100*30,
		map[string]string{"ARTIST": "Some Artist", "TITLE": "Some Title"}); err != nil {
		t.Fatalf("write fixture %s.flac: %v", stem, err)
	}
}

// TestScanLibrary_UppercaseSettledSidecarIsNotRequeued is the scanner half of
// #1051: a track settled as "song.LRC" must read as settled exactly as the
// writer's own settledSidecar treats it (#989), or a case-sensitive
// deployment re-queues and re-fetches it on every scan.
func TestScanLibrary_UppercaseSettledSidecarIsNotRequeued(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveFS(t, dir) {
		t.Skip(`filesystem is case-insensitive; os.Stat("song.lrc") already finds "song.LRC" without the fix`)
	}
	writeCaseFixture(t, dir, "song")
	if err := os.WriteFile(filepath.Join(dir, "song.LRC"), []byte("[00:01.00]x"), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	sc := NewScanner()
	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("got %d result(s); want 0 -- an uppercase-extension .LRC must read as settled, not re-queue the track for fetch", len(res))
	}
}

// TestScanLibrary_UppercaseSettledUnsyncedIsNotRequeued mirrors the .LRC case
// for the unsynced sidecar: a settled "song.TXT" must also short-circuit the
// fetch, not just "song.lrc".
func TestScanLibrary_UppercaseSettledUnsyncedIsNotRequeued(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveFS(t, dir) {
		t.Skip(`filesystem is case-insensitive; os.Stat("song.txt") already finds "song.TXT" without the fix`)
	}
	writeCaseFixture(t, dir, "song")
	if err := os.WriteFile(filepath.Join(dir, "song.TXT"), []byte("plain words"), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	sc := NewScanner()
	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("got %d result(s); want 0 -- an uppercase-extension .TXT must read as settled, not re-queue the track for fetch", len(res))
	}
}

// TestScanLibrary_DifferingStemCaseNeverSettles is the C1 guard (#989's
// round-1 critical defect, reprised): "Intro.lrc" must never be treated as
// "intro"'s sidecar, even on a case-sensitive filesystem where both names can
// coexist. Only the extension's case may fold; the stem must stay
// byte-identical.
func TestScanLibrary_DifferingStemCaseNeverSettles(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveFS(t, dir) {
		t.Skip("filesystem is case-insensitive; \"Intro.lrc\" and \"intro.lrc\" would collide as one file")
	}
	writeCaseFixture(t, dir, "intro")
	if err := os.WriteFile(filepath.Join(dir, "Intro.lrc"), []byte("[00:01.00]x"), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	sc := NewScanner()
	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d result(s); want 1 -- intro.flac must still be enqueued: \"Intro.lrc\" belongs to a different track and must never settle it", len(res))
	}
	if res[0].Status != "pending" {
		t.Errorf("Status = %q; want %q", res[0].Status, "pending")
	}
}

// TestScanLibrary_DanglingSidecarSymlinkIsEnqueued is the I1 regression guard
// (#1051 hostile-review round): resolvedSidecarPath must agree with the
// writer's settledSidecar on a DANGLING symlink named "song.lrc" -- i.e. a
// symlink whose target does not exist. An earlier revision handed the exact
// candidate name straight to sidecar.Listing.Variants, which resolves it with
// os.Lstat: Lstat succeeds on a dangling symlink (it stats the link itself,
// never the missing target), so that revision read the track as settled and
// it was silently never fetched again. settledSidecar uses os.Stat, which
// FOLLOWS the link and fails with not-exist for a dangling one, so the writer
// has always treated this track as unsettled. The fix probes os.Stat first,
// matching settledSidecar exactly, so the scanner must reach the same
// verdict: the track is still pending and gets enqueued.
func TestScanLibrary_DanglingSidecarSymlinkIsEnqueued(t *testing.T) {
	dir := t.TempDir()
	writeCaseFixture(t, dir, "song")
	if err := os.Symlink(filepath.Join(dir, "does-not-exist.lrc"), filepath.Join(dir, "song.lrc")); err != nil {
		t.Fatalf("write dangling symlink: %v", err)
	}

	sc := NewScanner()
	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d result(s); want 1 -- a dangling sidecar symlink must not read as settled, or the track is never fetched again", len(res))
	}
	if res[0].Status != "pending" {
		t.Errorf("Status = %q; want %q", res[0].Status, "pending")
	}
}

// TestScanLibrary_DirectorySidecarNameReadsSettled is the I1 regression guard's
// other half: a DIRECTORY named "song.lrc" must resolve exactly as the
// writer's settledSidecar does -- present, because settledSidecar's os.Stat
// succeeds on a directory and its guard only cares about "is something
// already there", not what kind of thing it is. An earlier revision handed
// the exact candidate name straight to sidecar.Listing.Variants, whose
// os.Lstat-based exact-match slot explicitly EXCLUDES a directory (Variants
// accepts only a non-directory there), so that revision read the same path as
// UNSETTLED while the writer read it as settled -- scanner and writer
// disagreeing about the very same file. The fix's os.Stat-first probe makes
// both packages agree.
func TestScanLibrary_DirectorySidecarNameReadsSettled(t *testing.T) {
	dir := t.TempDir()
	writeCaseFixture(t, dir, "song")
	if err := os.Mkdir(filepath.Join(dir, "song.lrc"), 0o750); err != nil {
		t.Fatalf("mkdir sidecar-named directory: %v", err)
	}

	sc := NewScanner()
	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("got %d result(s); want 0 -- a directory named song.lrc must read as settled here exactly as the writer's settledSidecar treats it", len(res))
	}
}

// TestScanLibrary_UnicodeStemFoldNeverSettles guards the same invariant
// against a Unicode simple-case-fold alias rather than an ASCII one:
// "ſong.LRC" (LATIN SMALL LETTER LONG S, which strings.EqualFold folds to
// plain "s") must never be treated as "song"'s sidecar. sidecar.Variants uses
// ASCII-only folding for exactly this reason; this pins the scanner's
// consumption of it.
func TestScanLibrary_UnicodeStemFoldNeverSettles(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveFS(t, dir) {
		t.Skip("filesystem is case-insensitive; the OS itself may alias these names")
	}
	writeCaseFixture(t, dir, "song")
	if err := os.WriteFile(filepath.Join(dir, "ſong.LRC"), []byte("[00:01.00]x"), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	sc := NewScanner()
	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1})
	if err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d result(s); want 1 -- song.flac must still be enqueued: a Unicode fold of the stem must never settle it", len(res))
	}
}
