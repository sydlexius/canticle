package lyrics

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/selfwrite"
	"github.com/sydlexius/canticle/internal/sidecar"
)

// gateOn forces the companion activation gate open for one writer. It is the
// test-only seam described on LRCWriter.companionGate: the shipped gate reads
// sidecar.Active(sidecar.KindWordSynced), which is false until slice 4 of #986.
func gateOn(w *LRCWriter) *LRCWriter {
	w.companionGate = func() bool { return true }
	return w
}

// modeWriter builds a writer configured the way commands maps each
// output.word_sync_mode value onto the two writer switches.
func modeWriter(inline, companion bool) *LRCWriter {
	w := gateOn(NewLRCWriter())
	w.SetWordSync(inline)
	w.SetWordSyncCompanion(companion)
	return w
}

func dirFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// TestWriteLRC_WordSyncModes_FileSet pins the exact output of each of the four
// modes (#986): which files exist, and which of them carry inline markers.
func TestWriteLRC_WordSyncModes_FileSet(t *testing.T) {
	cases := []struct {
		name              string
		inline, companion bool
		want              []string
		lrcMarked         bool
	}{
		{"sidecar", false, true, []string{"song.elrc", "song.lrc"}, false},
		{"off", false, false, []string{"song.lrc"}, false},
		{"inline", true, false, []string{"song.lrc"}, true},
		{"both", true, true, []string{"song.elrc", "song.lrc"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := modeWriter(tc.inline, tc.companion).WriteLRC(a2Song(), "song.lrc", dir); err != nil {
				t.Fatalf("WriteLRC: %v", err)
			}
			if got := dirFiles(t, dir); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("files = %v, want %v", got, tc.want)
			}
			lrc := readFileString(t, filepath.Join(dir, "song.lrc"))
			if marked := strings.Contains(lrc, "<00:01.50>alpha"); marked != tc.lrcMarked {
				t.Errorf(".lrc marked = %v, want %v:\n%s", marked, tc.lrcMarked, lrc)
			}
			if tc.companion {
				elrc := readFileString(t, filepath.Join(dir, "song.elrc"))
				if !strings.Contains(elrc, "[00:01.50]<00:01.50>alpha <00:02.00>beta") {
					t.Errorf("companion lacks the A2 body:\n%s", elrc)
				}
			}
		})
	}
}

// TestWriteLRC_SidecarLRCByteIdenticalToOff is the compatibility promise of the
// sidecar default: turning word timings on must not change one byte of the
// .lrc every player already reads.
func TestWriteLRC_SidecarLRCByteIdenticalToOff(t *testing.T) {
	song := a2Song()
	song.WinningLane = "musixmatch"
	song.FetchedAt = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	offDir, sideDir := t.TempDir(), t.TempDir()
	if err := modeWriter(false, false).WriteLRC(song, "song.lrc", offDir); err != nil {
		t.Fatalf("off: %v", err)
	}
	if err := modeWriter(false, true).WriteLRC(song, "song.lrc", sideDir); err != nil {
		t.Fatalf("sidecar: %v", err)
	}
	off := readFileString(t, filepath.Join(offDir, "song.lrc"))
	side := readFileString(t, filepath.Join(sideDir, "song.lrc"))
	if off != side {
		t.Errorf("sidecar .lrc differs from off .lrc:\noff:\n%s\nsidecar:\n%s", off, side)
	}
}

// TestWriteLRC_CompanionSkippedWhenNoLineQualifies: when no cue's timings pass
// a2Words' guards, a companion would be byte-identical to the .lrc and claim
// word timing it does not carry, so none is written.
func TestWriteLRC_CompanionSkippedWhenNoLineQualifies(t *testing.T) {
	dir := t.TempDir()
	song := a2Song()
	// Words that do not reconstruct the cue text: the fidelity guard refuses.
	song.WordTimings = []models.WordTiming{
		{Line: 0, Text: "gamma ", StartMS: 1500},
		{Line: 0, Text: "delta", StartMS: 2000},
	}
	if err := modeWriter(false, true).WriteLRC(song, "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	mustExist(t, filepath.Join(dir, "song.lrc"))
	mustNotExist(t, filepath.Join(dir, "song.elrc"))
}

// TestWriteLRC_CompanionGateOffWritesNone pins the SHIPPED state: until the
// sidecar table activates KindWordSynced (slice 4 of #986), the default mode
// writes no companion and the .lrc stays clean -- it must not fall back to
// inline. When slice 4 flips the flag this test is expected to fail, and should
// be rewritten to assert the companion instead.
func TestWriteLRC_CompanionGateOffWritesNone(t *testing.T) {
	if sidecar.Active(sidecar.KindWordSynced) {
		t.Fatal("KindWordSynced is active: rewrite this test for the activated state")
	}
	dir := t.TempDir()
	w := NewLRCWriter() // shipped gate, no seam
	w.SetWordSyncCompanion(true)
	if err := w.WriteLRC(a2Song(), "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	if got := dirFiles(t, dir); strings.Join(got, ",") != "song.lrc" {
		t.Fatalf("files = %v, want only song.lrc", got)
	}
	if lrc := readFileString(t, filepath.Join(dir, "song.lrc")); strings.Contains(lrc, "<00:") {
		t.Errorf("gated-off sidecar mode fell back to inline markers:\n%s", lrc)
	}
}

// TestWriteLRC_CompanionGateOffLeavesForeignFile: with the gate off canticle
// never wrote a .elrc, so one on disk is not canticle's and a demotion must not
// delete it.
func TestWriteLRC_CompanionGateOffLeavesForeignFile(t *testing.T) {
	dir := t.TempDir()
	elrc := filepath.Join(dir, "song.elrc")
	if err := os.WriteFile(elrc, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	song := guardSong(100, cue(10, "first line"), cue(120, "last line"))
	if err := NewLRCWriter().WriteLRC(song, "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	mustExist(t, filepath.Join(dir, "song.txt"))
	mustExist(t, elrc)
}

func seedCompanion(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "song.elrc")
	if err := os.WriteFile(p, []byte("[00:01.00]<00:01.00>stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestWriteLRC_DemotionRemovesStaleCompanion: a MisSynced result is demoted to
// .txt because its line timing was rejected, so word timings for it must not
// survive beside the .txt.
func TestWriteLRC_DemotionRemovesStaleCompanion(t *testing.T) {
	dir := t.TempDir()
	elrc := seedCompanion(t, dir)
	reg := selfwrite.New(time.Minute)
	w := modeWriter(false, true)
	w.SetSelfWriteRegistry(reg)

	// 120s cue against 100s audio: MisSynced, demoted.
	song := guardSong(100, cue(10, "first line"), cue(120, "last line"))
	if err := w.WriteLRC(song, "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	mustExist(t, filepath.Join(dir, "song.txt"))
	mustNotExist(t, elrc)
	if !reg.Suppress(elrc) {
		t.Error("companion removal was not recorded as a self-write")
	}
}

// TestWriteLRC_CategoricalRemovesStaleCompanion: a Categorical result writes
// nothing, and the word timings of a lyric now known to be timed to another
// recording are removed rather than left to claim word sync.
func TestWriteLRC_CategoricalRemovesStaleCompanion(t *testing.T) {
	dir := t.TempDir()
	elrc := seedCompanion(t, dir)
	reg := selfwrite.New(time.Minute)
	w := modeWriter(false, true)
	w.SetSelfWriteRegistry(reg)

	// 400s cue against 100s audio: ratio 4.0, Categorical.
	song := guardSong(100, cue(10, "first"), cue(400, "last"))
	if err := w.WriteLRC(song, "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	mustNotExist(t, filepath.Join(dir, "song.lrc"))
	mustNotExist(t, filepath.Join(dir, "song.txt"))
	mustNotExist(t, elrc)
	if !reg.Suppress(elrc) {
		t.Error("companion removal was not recorded as a self-write")
	}
}

// TestWriteLRC_LRCRewritePreservesCompanion: a companion is not an opposite. A
// .lrc rewrite that produces no companion of its own (mode off here) must not
// delete the one already on disk.
func TestWriteLRC_LRCRewritePreservesCompanion(t *testing.T) {
	dir := t.TempDir()
	elrc := seedCompanion(t, dir)
	if err := modeWriter(false, false).WriteLRC(a2Song(), "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	mustExist(t, filepath.Join(dir, "song.lrc"))
	if got := readFileString(t, elrc); !strings.Contains(got, "stale") {
		t.Errorf("existing companion was rewritten or removed: %q", got)
	}
}

// TestWriteLRC_CompanionCarriesProvenance: purge-provenance's [source:] filter
// can only find a companion that carries the same tags as its .lrc.
func TestWriteLRC_CompanionCarriesProvenance(t *testing.T) {
	dir := t.TempDir()
	song := a2Song()
	song.WinningLane = "musixmatch"
	song.Track.ISRC = "USX9P0000001"
	if err := modeWriter(false, true).WriteLRC(song, "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	elrc := readFileString(t, filepath.Join(dir, "song.elrc"))
	for _, want := range []string{"[by:canticle]", "[source:musixmatch]", "[isrc:USX9P0000001]"} {
		if !strings.Contains(elrc, want) {
			t.Errorf("companion missing %s:\n%s", want, elrc)
		}
	}
	pt, err := ReadProvenanceTags(filepath.Join(dir, "song.elrc"))
	if err != nil {
		t.Fatalf("ReadProvenanceTags: %v", err)
	}
	if pt.Source != "musixmatch" {
		t.Errorf("ReadProvenanceTags source = %q, want musixmatch", pt.Source)
	}
}

// TestWriteLRC_CompanionRecordedAsSelfWrite: without this the watcher treats
// canticle's own companion write as an external change (#685).
func TestWriteLRC_CompanionRecordedAsSelfWrite(t *testing.T) {
	dir := t.TempDir()
	reg := selfwrite.New(time.Minute)
	w := modeWriter(false, true)
	w.SetSelfWriteRegistry(reg)
	if err := w.WriteLRC(a2Song(), "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	if !reg.Suppress(filepath.Join(dir, "song.elrc")) {
		t.Error("companion write was not recorded as a self-write")
	}
}

// TestWriteLRC_CompanionWrittenAfterLRC pins the crash order: the .lrc lands
// first, so a failed companion write leaves a clean line-synced file behind,
// never a companion beside a missing .lrc.
func TestWriteLRC_CompanionWrittenAfterLRC(t *testing.T) {
	dir := t.TempDir()
	// A directory at the companion path makes its final Remove fail.
	if err := os.MkdirAll(filepath.Join(dir, "song.elrc", "x"), 0o750); err != nil {
		t.Fatal(err)
	}
	err := modeWriter(false, true).WriteLRC(a2Song(), "song.lrc", dir)
	if err == nil || !strings.Contains(err.Error(), "companion") {
		t.Fatalf("want a companion write error, got %v", err)
	}
	mustExist(t, filepath.Join(dir, "song.lrc"))
}

// TestSidecarNameFor covers the kind-taking sibling of SidecarName.
func TestSidecarNameFor(t *testing.T) {
	got, err := SidecarNameFor("A", "T", "song.lrc", sidecar.KindWordSynced)
	if err != nil || got != "song.elrc" {
		t.Errorf("SidecarNameFor(word) = %q, %v; want song.elrc", got, err)
	}
	if _, err := SidecarNameFor("A", "T", "song.lrc", sidecar.KindUnknown); err == nil {
		t.Error("SidecarNameFor(unknown kind) should error")
	}
	legacy, _ := SidecarName("A", "T", "song.txt", true)
	viaKind, _ := SidecarNameFor("A", "T", "song.txt", sidecar.KindLineSynced)
	if legacy != viaKind {
		t.Errorf("SidecarName = %q, SidecarNameFor = %q; want equal", legacy, viaKind)
	}
}

// TestInjectProvenance_AcceptsCompanion: a companion is a word-synced LRC and
// takes provenance tags like its .lrc; other extensions are still refused.
func TestInjectProvenance_AcceptsCompanion(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "song.elrc")
	if err := os.WriteFile(p, []byte("[00:01.00]<00:01.00>hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, _, err := InjectProvenance(p, ProvenanceTags{Source: "musixmatch"})
	if err != nil || n != 1 {
		t.Fatalf("InjectProvenance(.elrc) = %d, %v; want 1 tag injected", n, err)
	}
	if !strings.Contains(readFileString(t, p), "[source:musixmatch]") {
		t.Error("companion did not gain the [source:] tag")
	}
	txt := filepath.Join(dir, "song.txt")
	if err := os.WriteFile(txt, []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := InjectProvenance(txt, ProvenanceTags{Source: "x"}); err == nil {
		t.Error("InjectProvenance(.txt) should still refuse")
	}
}
