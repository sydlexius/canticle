package scanner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/testutil"
)

// extractSyncedFixture writes a FLAC carrying a valid SYNCEDLYRICS comment and
// runs one "extract" scan over it, leaving the tag-extracted .lrc sidecar in
// place. It returns the directory and the scanner, so a caller can rescan the
// same tree with different reopen flags -- the real extract-then-rescan loop.
func extractSyncedFixture(t *testing.T) (string, *Scanner) {
	t.Helper()
	dir := t.TempDir()
	if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 441000,
		map[string]string{"SYNCEDLYRICS": syncedLRC}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sc := NewScanner()
	if _, err := sc.ScanLibrary(context.Background(), dir,
		ScanOptions{MaxDepth: 100, EmbeddedLyrics: "extract"}); err != nil {
		t.Fatalf("seed extract scan: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "song.lrc")); err != nil {
		t.Fatalf("fixture precondition: expected extracted song.lrc: %v", err)
	}
	return dir, sc
}

// --update must re-queue a track whose synced .lrc came from tag extraction
// (#575). The top-level switch already falls through for lrcExists && Update,
// but the embedded-lyrics block then skipped unconditionally in the hasSynced
// arm, silently defeating --update for these tracks. Embedded SYNCEDLYRICS is
// whatever previous tooling wrote and is frequently worse than a provider
// result, so without this the track is pinned to it with no supported refresh.
func TestScanLibrary_SyncedExtracted_UpdateRequeues(t *testing.T) {
	dir, sc := extractSyncedFixture(t)

	res, err := sc.ScanLibrary(context.Background(), dir,
		ScanOptions{MaxDepth: 100, EmbeddedLyrics: "extract", Update: true})
	if err != nil {
		t.Fatalf("update rescan: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("update rescan: got %d results; want 1 (re-queued for fetch)", len(res))
	}
	// The sidecar stays byte-identical: re-extraction is a no-op (linkSidecar
	// never overwrites), and the fetch that replaces it happens downstream.
	got, err := os.ReadFile(filepath.Join(dir, "song.lrc"))
	if err != nil {
		t.Fatalf("read sidecar after update rescan: %v", err)
	}
	if string(got) != syncedLRC {
		t.Errorf(".lrc = %q; want unchanged %q", string(got), syncedLRC)
	}
}

// Regression guard, not a RED test: --upgrade must still skip an extracted
// synced .lrc. There is nothing above synced to promote it to, so upgrade has
// no work to do here -- only --update reopens a settled synced sidecar.
func TestScanLibrary_SyncedExtracted_UpgradeStillSkips(t *testing.T) {
	dir, sc := extractSyncedFixture(t)

	res, err := sc.ScanLibrary(context.Background(), dir,
		ScanOptions{MaxDepth: 100, EmbeddedLyrics: "extract", Upgrade: true})
	if err != nil {
		t.Fatalf("upgrade rescan: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("upgrade rescan: got %d results; want 0 (nothing to promote to)", len(res))
	}
}

// Two identical --update passes over a fresh library must agree. Gating the
// fall-through on a pre-extraction lrcExists made the first pass write the
// sidecar and skip, and only the second pass re-queue -- so --update was
// non-idempotent and AC "update re-queues an extracted synced .lrc" held only
// from the second run onward. The sidecar is written either way (extraction
// runs before the branch), so there is nothing to protect by skipping here.
func TestScanLibrary_SyncedExtracted_UpdateIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 441000,
		map[string]string{"SYNCEDLYRICS": syncedLRC}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sc := NewScanner()
	opts := ScanOptions{MaxDepth: 100, EmbeddedLyrics: "extract", Update: true}

	first, err := sc.ScanLibrary(context.Background(), dir, opts)
	if err != nil {
		t.Fatalf("first update scan: %v", err)
	}
	second, err := sc.ScanLibrary(context.Background(), dir, opts)
	if err != nil {
		t.Fatalf("second update scan: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("--update is not idempotent: pass1=%d, pass2=%d results", len(first), len(second))
	}
	if len(first) != 1 {
		t.Fatalf("update over fresh extraction: got %d results; want 1 (re-queued for fetch)", len(first))
	}
	// The sidecar is still written on the first pass -- falling through to
	// enqueue must not cost the extraction.
	if _, err := os.Stat(filepath.Join(dir, "song.lrc")); err != nil {
		t.Errorf("first --update pass must still write the extracted .lrc: %v", err)
	}
}

// Regression guard for the f.Close invariant, on the config every production
// caller of directory-mode --update actually uses: GetSongDir hardcodes
// EnrichRecording: true. The new fall-through deliberately does NOT close f,
// because probeDuration still reads from the handle downstream. A premature
// close here would surface as a zero TrackLength (or a use-after-close).
func TestScanLibrary_SyncedExtracted_UpdateEnrichRecording(t *testing.T) {
	dir, sc := extractSyncedFixture(t)

	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{
		MaxDepth: 100, EmbeddedLyrics: "extract", Update: true, EnrichRecording: true,
	})
	if err != nil {
		t.Fatalf("update rescan with enrichment: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("update rescan: got %d results; want 1", len(res))
	}
	if res[0].Track.TrackLength == 0 {
		t.Errorf("TrackLength = 0; want the probed duration -- the handle must still be open when probeDuration reads it")
	}
}

// off mode is a no-op: the embedded block never runs, so the reopen gate is
// unreachable and SYNCEDLYRICS is ignored entirely. --update still enqueues,
// but via the ordinary fetch path and with no sidecar written.
func TestScanLibrary_SyncedOff_UpdateUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 441000,
		map[string]string{"SYNCEDLYRICS": syncedLRC}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sc := NewScanner()

	res, err := sc.ScanLibrary(context.Background(), dir,
		ScanOptions{MaxDepth: 100, EmbeddedLyrics: "off", Update: true})
	if err != nil {
		t.Fatalf("off update scan: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("off + update: got %d results; want 1 (ordinary fetch path)", len(res))
	}
	if _, err := os.Stat(filepath.Join(dir, "song.lrc")); !os.IsNotExist(err) {
		t.Errorf("off mode must never write a sidecar")
	}
}

// respect mode is unchanged by the reopen gate: it never writes a sidecar, and
// an --update pass still skips a track whose embedded lyrics are respected.
func TestScanLibrary_SyncedRespect_UpdateUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 441000,
		map[string]string{"SYNCEDLYRICS": syncedLRC}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sc := NewScanner()

	res, err := sc.ScanLibrary(context.Background(), dir,
		ScanOptions{MaxDepth: 100, EmbeddedLyrics: "respect", Update: true})
	if err != nil {
		t.Fatalf("respect update scan: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("respect + update: got %d results; want 0 (skipped)", len(res))
	}
	if _, err := os.Stat(filepath.Join(dir, "song.lrc")); !os.IsNotExist(err) {
		t.Errorf("respect must not write any sidecar")
	}
}
