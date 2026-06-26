package scanner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/testutil"
)

const syncedLRC = "[00:01.00]hello\n[00:02.00]world"

// TestScanLibrary_SyncedLyricsExtract: a FLAC with a valid (timestamped)
// SYNCEDLYRICS comment and no sidecar gets a .lrc sidecar (not .txt) and is not
// enqueued.
func TestScanLibrary_SyncedLyricsExtract(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 441000,
		map[string]string{"SYNCEDLYRICS": syncedLRC}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sc := NewScanner()

	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 100, EmbeddedLyrics: "extract"})
	if err != nil {
		t.Fatalf("scan extract: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("synced extract: got %d results; want 0 (skipped after .lrc extraction)", len(res))
	}
	got, err := os.ReadFile(filepath.Join(dir, "song.lrc"))
	if err != nil {
		t.Fatalf("expected song.lrc sidecar: %v", err)
	}
	if string(got) != syncedLRC {
		t.Errorf("sidecar = %q; want %q", string(got), syncedLRC)
	}
	if _, err := os.Stat(filepath.Join(dir, "song.txt")); !os.IsNotExist(err) {
		t.Errorf(".txt sidecar should not be written for synced extraction")
	}
}

// Malformed SYNCEDLYRICS (no timestamps) must not produce a .lrc; the track
// falls through and is enqueued for a normal fetch.
func TestScanLibrary_SyncedLyricsMalformed_FallsThrough(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 441000,
		map[string]string{"SYNCEDLYRICS": "just prose, no timestamps at all"}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sc := NewScanner()

	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 100, EmbeddedLyrics: "extract"})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("malformed synced: got %d results; want 1 (enqueued for fetch)", len(res))
	}
	if _, err := os.Stat(filepath.Join(dir, "song.lrc")); !os.IsNotExist(err) {
		t.Errorf("no .lrc should be written for non-timestamped SYNCEDLYRICS")
	}
}

// When both a valid SYNCEDLYRICS and unsynced lyrics are present, synced wins: a
// .lrc is written and no .txt.
func TestScanLibrary_SyncedPrecedenceOverUnsynced(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 441000,
		map[string]string{"SYNCEDLYRICS": syncedLRC, "LYRICS": "plain unsynced"}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sc := NewScanner()

	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 100, EmbeddedLyrics: "extract"})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("precedence: got %d results; want 0 (skipped)", len(res))
	}
	if _, err := os.Stat(filepath.Join(dir, "song.lrc")); err != nil {
		t.Errorf(".lrc should be written when synced wins: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "song.txt")); !os.IsNotExist(err) {
		t.Errorf(".txt must not be written when synced wins")
	}
}

// A pre-existing sidecar is never overwritten (sidecar precedence is inherited
// from the pre-embedded-block check).
func TestScanLibrary_SyncedLyrics_ExistingSidecarWins(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 441000,
		map[string]string{"SYNCEDLYRICS": syncedLRC}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "song.lrc"), []byte("EXISTING"), 0o644); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}
	sc := NewScanner()

	if _, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 100, EmbeddedLyrics: "extract"}); err != nil {
		t.Fatalf("scan: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "song.lrc"))
	if string(got) != "EXISTING" {
		t.Errorf(".lrc = %q; want EXISTING (never overwrite)", string(got))
	}
}

// respect mode treats a valid SYNCEDLYRICS as "already has lyrics": skip, write
// nothing.
func TestScanLibrary_SyncedLyricsRespect(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 441000,
		map[string]string{"SYNCEDLYRICS": syncedLRC}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sc := NewScanner()

	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 100, EmbeddedLyrics: "respect"})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("respect synced: got %d results; want 0 (skipped)", len(res))
	}
	if _, err := os.Stat(filepath.Join(dir, "song.lrc")); !os.IsNotExist(err) {
		t.Errorf("respect must not write any sidecar")
	}
}

// Whitespace-only SYNCEDLYRICS is treated as absent; the unsynced .txt path
// applies when m.Lyrics() is non-empty.
func TestScanLibrary_EmptySyncedFallsToUnsynced(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 441000,
		map[string]string{"SYNCEDLYRICS": "   ", "LYRICS": "plain unsynced"}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sc := NewScanner()

	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 100, EmbeddedLyrics: "extract"})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("empty synced: got %d results; want 0 (extracted .txt)", len(res))
	}
	got, err := os.ReadFile(filepath.Join(dir, "song.txt"))
	if err != nil {
		t.Fatalf("expected song.txt sidecar: %v", err)
	}
	if string(got) != "plain unsynced" {
		t.Errorf(".txt = %q; want %q", string(got), "plain unsynced")
	}
	if _, err := os.Stat(filepath.Join(dir, "song.lrc")); !os.IsNotExist(err) {
		t.Errorf("no .lrc for whitespace-only SYNCEDLYRICS")
	}
}

// A malformed SYNCEDLYRICS alongside non-empty unsynced lyrics: warn on the bad
// synced, then fall through to write the unsynced .txt (the unsynced branch, not
// just fall-through-to-fetch).
func TestScanLibrary_MalformedSyncedFallsToUnsynced(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 441000,
		map[string]string{"SYNCEDLYRICS": "prose, no timestamps", "LYRICS": "plain unsynced"}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sc := NewScanner()

	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 100, EmbeddedLyrics: "extract"})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("malformed synced + unsynced: got %d results; want 0 (.txt extracted)", len(res))
	}
	got, err := os.ReadFile(filepath.Join(dir, "song.txt"))
	if err != nil {
		t.Fatalf("expected song.txt sidecar: %v", err)
	}
	if string(got) != "plain unsynced" {
		t.Errorf(".txt = %q; want %q", string(got), "plain unsynced")
	}
	if _, err := os.Stat(filepath.Join(dir, "song.lrc")); !os.IsNotExist(err) {
		t.Errorf("no .lrc for malformed SYNCEDLYRICS")
	}
}

// Extract then rescan the same dir: the second scan skips the now-present .lrc
// and leaves it unchanged (the real extract-then-rescan loop).
func TestScanLibrary_SyncedLyricsExtract_RescanSkips(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 441000,
		map[string]string{"SYNCEDLYRICS": syncedLRC}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sc := NewScanner()
	opts := ScanOptions{MaxDepth: 100, EmbeddedLyrics: "extract"}

	if _, err := sc.ScanLibrary(context.Background(), dir, opts); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	res, err := sc.ScanLibrary(context.Background(), dir, opts)
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("rescan: got %d results; want 0 (skipped)", len(res))
	}
	got, _ := os.ReadFile(filepath.Join(dir, "song.lrc"))
	if string(got) != syncedLRC {
		t.Errorf(".lrc changed on rescan = %q; want unchanged", string(got))
	}
}

// respect mode with only a malformed SYNCEDLYRICS (no valid lyrics): must NOT
// skip -- the track is enqueued for a normal fetch.
func TestScanLibrary_RespectMalformedSyncedOnly_Enqueues(t *testing.T) {
	dir := t.TempDir()
	if err := testutil.WriteFLACFileWithComments(dir, "song.flac", 44100, 441000,
		map[string]string{"SYNCEDLYRICS": "prose, no timestamps"}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	sc := NewScanner()

	res, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 100, EmbeddedLyrics: "respect"})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("respect malformed-only: got %d results; want 1 (enqueued)", len(res))
	}
}
