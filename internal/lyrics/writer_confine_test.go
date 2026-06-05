package lyrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
)

func syncedSong() models.Song {
	return models.Song{
		Track: models.Track{ArtistName: "Artist", TrackName: "Track"},
		Subtitles: models.Synced{Lines: []models.Lines{
			{Text: "hello", Time: models.Time{Minutes: 0, Seconds: 1, Hundredths: 0}},
		}},
	}
}

// TestWriteLRC_ConfinedInRootUnchanged verifies that a writer configured with a
// confinement root still writes legitimate in-root output normally (acceptance:
// behavior unchanged for legitimate in-root writes).
func TestWriteLRC_ConfinedInRootUnchanged(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "albums", "one")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}

	w := NewLRCWriter(root)
	if err := w.WriteLRC(syncedSong(), "song.lrc", sub); err != nil {
		t.Fatalf("confined write into root: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(sub, "song.lrc"))
	if err != nil {
		t.Fatalf("reading written file: %v", err)
	}
	if !strings.Contains(string(got), "[ar:Artist]") || !strings.Contains(string(got), "hello") {
		t.Fatalf("unexpected file content: %q", got)
	}
	// No leftover temp files.
	entries, err := os.ReadDir(sub)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("leftover temp file: %s", e.Name())
		}
	}
}

// TestWriteLRC_ConfinedRefusesEscapingSymlinkSwap is the core #102 guard: a
// directory component inside the root is swapped for a symlink that escapes the
// root AFTER the caller validated the path. The confined write must fail the
// open rather than follow the symlink and write outside the root.
func TestWriteLRC_ConfinedRefusesEscapingSymlinkSwap(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir() // a sibling dir, NOT under root

	// Legit-looking directory the caller would have validated.
	sub := filepath.Join(root, "albums")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	// Attacker swaps the component for a symlink escaping the root.
	if err := os.Remove(sub); err != nil {
		t.Fatalf("remove sub: %v", err)
	}
	if err := os.Symlink(external, sub); err != nil {
		t.Fatalf("symlink swap: %v", err)
	}

	w := NewLRCWriter(root)
	err := w.WriteLRC(syncedSong(), "song.lrc", sub)
	if err == nil {
		t.Fatal("expected confined write to refuse the escaping symlink, got nil error")
	}

	// Nothing must have been written into the symlink target outside the root.
	entries, readErr := os.ReadDir(external)
	if readErr != nil {
		t.Fatalf("readdir external: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("write escaped the root into %s: %v", external, entries)
	}
}

// TestWriteLRC_ConfinedRefusesSymlinkedOutdir covers the outdir itself being a
// symlink that escapes the root.
func TestWriteLRC_ConfinedRefusesSymlinkedOutdir(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()

	link := filepath.Join(root, "out")
	if err := os.Symlink(external, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	w := NewLRCWriter(root)
	if err := w.WriteLRC(syncedSong(), "song.lrc", link); err == nil {
		t.Fatal("expected refusal writing through a symlinked outdir, got nil error")
	}

	entries, err := os.ReadDir(external)
	if err != nil {
		t.Fatalf("readdir external: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("write escaped the root into %s: %v", external, entries)
	}
}

// TestWriteLRC_ConfinedUnsyncedAndInstrumental exercises the confined path for
// the .txt (unsynced) and instrumental content types.
func TestWriteLRC_ConfinedUnsyncedAndInstrumental(t *testing.T) {
	root := t.TempDir()
	w := NewLRCWriter(root)

	unsynced := models.Song{
		Track:  models.Track{ArtistName: "A", TrackName: "B"},
		Lyrics: models.Lyrics{LyricsBody: "plain lyrics"},
	}
	if err := w.WriteLRC(unsynced, "song.lrc", root); err != nil {
		t.Fatalf("confined unsynced write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "song.txt")); err != nil {
		t.Fatalf("expected .txt output for unsynced content: %v", err)
	}

	instrumental := models.Song{Track: models.Track{ArtistName: "A", TrackName: "C", Instrumental: 1}}
	if err := w.WriteLRC(instrumental, "inst.lrc", root); err != nil {
		t.Fatalf("confined instrumental write: %v", err)
	}
	// Instrumentals are unsynced content and are written as .txt, not .lrc.
	got, err := os.ReadFile(filepath.Join(root, "inst.txt"))
	if err != nil {
		t.Fatalf("reading instrumental: %v", err)
	}
	content := string(got)
	// Plain marker: no timestamp, no tag headers.
	const want = "♪ Instrumental ♪\n"
	if content != want {
		t.Fatalf("instrumental content = %q, want %q", content, want)
	}
	if strings.Contains(content, "[00:00.00]") {
		t.Fatalf("instrumental marker must not contain an LRC timestamp, got: %q", content)
	}
	if strings.Contains(content, "[by:") || strings.Contains(content, "[ar:") || strings.Contains(content, "[ti:") {
		t.Fatalf("instrumental marker must not contain tag headers, got: %q", content)
	}
}

// TestWriteLRC_ConfinedOverwriteAndStaleSidecar covers the confined overwrite
// (removing an existing target) and stale-sidecar removal (writing .lrc deletes
// an existing .txt).
func TestWriteLRC_ConfinedOverwriteAndStaleSidecar(t *testing.T) {
	root := t.TempDir()
	w := NewLRCWriter(root)

	// Seed an existing target and a stale .txt sidecar.
	if err := os.WriteFile(filepath.Join(root, "song.lrc"), []byte("old"), 0o644); err != nil {
		t.Fatalf("seed lrc: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "song.txt"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed txt: %v", err)
	}

	if err := w.WriteLRC(syncedSong(), "song.lrc", root); err != nil {
		t.Fatalf("confined overwrite: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, "song.lrc"))
	if err != nil {
		t.Fatalf("reading overwritten file: %v", err)
	}
	if string(got) == "old" || !strings.Contains(string(got), "[ar:Artist]") {
		t.Fatalf("file was not overwritten: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "song.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale .txt sidecar should have been removed, stat err = %v", err)
	}
}

// TestWriteLRC_ConfinedInRootSymlinkResolves verifies the key advantage of the
// re-resolve approach over an open-time os.Root guard: a directory component
// that is a symlink pointing to ANOTHER location inside the same root resolves
// and writes normally (common in symlinked library layouts), rather than being
// refused. The file must land at the symlink's real in-root target.
func TestWriteLRC_ConfinedInRootSymlinkResolves(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	link := filepath.Join(root, "albums") // in-root symlink -> root/real
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	w := NewLRCWriter(root)
	if err := w.WriteLRC(syncedSong(), "song.lrc", link); err != nil {
		t.Fatalf("in-root symlinked dir must resolve and write, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(real, "song.lrc")); err != nil {
		t.Fatalf("expected file at the symlink's real in-root target: %v", err)
	}
}

// TestWriteLRC_ConfinedRefusesUnresolvableDir covers the confined refusal when
// the output dir is lexically under a root but cannot be resolved (it does not
// exist), so containment cannot be re-confirmed before the write.
func TestWriteLRC_ConfinedRefusesUnresolvableDir(t *testing.T) {
	root := t.TempDir()
	missingSub := filepath.Join(root, "no", "such", "dir")
	w := NewLRCWriter(root)
	if err := w.WriteLRC(syncedSong(), "song.lrc", missingSub); err == nil {
		t.Fatal("expected refusal writing to an unresolvable confined dir, got nil")
	}
}

// TestWriteLRC_ConfinedNestedRootsAnchorTightest verifies multi-root and
// longest-root selection: with both a parent and a nested child root configured,
// a write under the child must still resolve and confine correctly (the tightest
// matching root wins).
func TestWriteLRC_ConfinedNestedRootsAnchorTightest(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "library", "music")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("mkdir child: %v", err)
	}

	// Roots given in parent-first order; the child (longest match) should win.
	w := NewLRCWriter(parent, child)
	if err := w.WriteLRC(syncedSong(), "song.lrc", child); err != nil {
		t.Fatalf("nested-root confined write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(child, "song.lrc")); err != nil {
		t.Fatalf("expected file written under the child root: %v", err)
	}

	// A write under only the parent root (not the child) must also work.
	parentOnly := filepath.Join(parent, "other")
	if err := os.Mkdir(parentOnly, 0o755); err != nil {
		t.Fatalf("mkdir parentOnly: %v", err)
	}
	if err := w.WriteLRC(syncedSong(), "p.lrc", parentOnly); err != nil {
		t.Fatalf("parent-root confined write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parentOnly, "p.lrc")); err != nil {
		t.Fatalf("expected file written under the parent root: %v", err)
	}
}

// TestWriteLRC_UnconfinedFollowsSymlinkSwap documents the baseline the
// confinement closes: with no roots (e.g. directory mode), the writer follows a
// swapped symlink and the file lands outside. This proves the symlink setup in
// the confined tests is a genuine escape, so their refusal is meaningful.
func TestWriteLRC_UnconfinedFollowsSymlinkSwap(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()

	link := filepath.Join(root, "out")
	if err := os.Symlink(external, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	w := NewLRCWriter() // no confinement roots
	if err := w.WriteLRC(syncedSong(), "song.lrc", link); err != nil {
		t.Fatalf("unconfined write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(external, "song.lrc")); err != nil {
		t.Fatalf("expected unconfined write to follow the symlink into %s: %v", external, err)
	}
}

// TestWriteLRC_OutsideRootsUsesUnconfinedPath verifies that an output directory
// not under any configured root still writes (the confinement is additive: it
// does not break writes the worker legitimately makes outside the library roots,
// e.g. a configured default output dir).
func TestWriteLRC_OutsideRootsUsesUnconfinedPath(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir() // not under root

	w := NewLRCWriter(root)
	if err := w.WriteLRC(syncedSong(), "song.lrc", other); err != nil {
		t.Fatalf("write outside roots should fall back to unconfined path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(other, "song.lrc")); err != nil {
		t.Fatalf("expected file written via unconfined path: %v", err)
	}
}
