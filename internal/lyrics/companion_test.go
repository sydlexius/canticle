package lyrics

import (
	"bufio"
	"errors"
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

// modeWriter builds a writer configured the way commands maps each
// output.word_sync_mode value onto the two writer switches.
func modeWriter(inline, companion bool) *LRCWriter {
	w := NewLRCWriter()
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

// TestWriteLRC_ShippedGateFollowsMode pins the SHIPPED state: with
// sidecar.KindWordSynced active (#986) the companion switch alone decides.
// Companion on writes the .elrc beside an unmarked .lrc; companion off --
// word_sync_mode "off" or "inline" -- writes no .elrc at all.
func TestWriteLRC_ShippedGateFollowsMode(t *testing.T) {
	if !sidecar.Active(sidecar.KindWordSynced) {
		t.Fatal("KindWordSynced is inactive: the shipped writer can never write a companion")
	}
	for _, tc := range []struct {
		name      string
		companion bool
		want      string
	}{
		{"companion on", true, "song.elrc,song.lrc"},
		{"companion off", false, "song.lrc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			w := NewLRCWriter() // shipped gate, no seam
			w.SetWordSyncCompanion(tc.companion)
			if err := w.WriteLRC(a2Song(), "song.lrc", dir); err != nil {
				t.Fatalf("WriteLRC: %v", err)
			}
			if got := dirFiles(t, dir); strings.Join(got, ",") != tc.want {
				t.Fatalf("files = %v, want %s", got, tc.want)
			}
			if lrc := readFileString(t, filepath.Join(dir, "song.lrc")); strings.Contains(lrc, "<00:") {
				t.Errorf("companion mode put inline markers in the .lrc:\n%s", lrc)
			}
		})
	}
}

// TestWriteLRC_DefaultWriterDemotionLeavesForeignCompanion: a writer left at
// its defaults (no SetWordSyncCompanion call) that demotes to .txt must not
// delete a .elrc without [by:canticle]; it is not canticle's.
func TestWriteLRC_DefaultWriterDemotionLeavesForeignCompanion(t *testing.T) {
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
	// Tagged as canticle's own: only such a companion is ever removed.
	if err := os.WriteFile(p, []byte("[by:canticle]\n[00:01.00]<00:01.00>stale\n"), 0o600); err != nil {
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

// TestWriteLRC_CategoricalKeepsSettledCompanion: a Categorical verdict judges
// the CANDIDATE, and quarantine leaves the settled .lrc on disk untouched. The
// companion beside it belongs to that kept .lrc, so it must survive too.
func TestWriteLRC_CategoricalKeepsSettledCompanion(t *testing.T) {
	dir := t.TempDir()
	lrc := filepath.Join(dir, "song.lrc")
	if err := os.WriteFile(lrc, []byte("[00:01.00]settled\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	elrc := seedCompanion(t, dir)

	// 400s cue against 100s audio: ratio 4.0, Categorical.
	song := guardSong(100, cue(10, "first"), cue(400, "last"))
	if err := modeWriter(false, true).WriteLRC(song, "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	mustExist(t, lrc)
	if got := readFileString(t, elrc); !strings.Contains(got, "stale") {
		t.Errorf("quarantine touched the settled .lrc's companion: %q", got)
	}
}

// TestWriteLRC_LRCRewriteRemovesMismatchedCompanion: a companion is aligned to
// one specific .lrc. A rewrite that produces no companion of its own (mode off,
// or no qualifying line) must remove the old one, or new line timing sits
// beside word timing for the lyric it replaced.
func TestWriteLRC_LRCRewriteRemovesMismatchedCompanion(t *testing.T) {
	for _, tc := range []struct {
		name      string
		companion bool
		song      models.Song
	}{
		{"mode without companion", false, a2Song()},
		{"no qualifying line", true, guardSong(0, cue(1, "plain line"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			elrc := seedCompanion(t, dir)
			reg := selfwrite.New(time.Minute)
			w := modeWriter(false, tc.companion)
			w.SetSelfWriteRegistry(reg)
			if err := w.WriteLRC(tc.song, "song.lrc", dir); err != nil {
				t.Fatalf("WriteLRC: %v", err)
			}
			mustExist(t, filepath.Join(dir, "song.lrc"))
			mustNotExist(t, elrc)
			if !reg.Suppress(elrc) {
				t.Error("companion removal was not recorded as a self-write")
			}
		})
	}
}

// TestWriteLRC_LRCRewriteReplacesCompanion: a rewrite that does produce a
// companion overwrites the old one with timing for the new .lrc.
func TestWriteLRC_LRCRewriteReplacesCompanion(t *testing.T) {
	dir := t.TempDir()
	elrc := seedCompanion(t, dir)
	if err := modeWriter(false, true).WriteLRC(a2Song(), "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	got := readFileString(t, elrc)
	if strings.Contains(got, "stale") || !strings.Contains(got, "<00:01.50>alpha") {
		t.Errorf("companion was not replaced for the new .lrc: %q", got)
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
	w := modeWriter(false, true)
	w.companionWrite = func(string, string, []string, func(*bufio.Writer) error) error {
		return errors.New("injected")
	}
	err := w.WriteLRC(a2Song(), "song.lrc", dir)
	if err == nil || !strings.Contains(err.Error(), "companion") {
		t.Fatalf("want a companion write error, got %v", err)
	}
	mustExist(t, filepath.Join(dir, "song.lrc"))
	mustNotExist(t, filepath.Join(dir, "song.elrc"))
}

// TestWriteLRC_StaleCompanionRemovedBeforeLRC: the old companion is removed
// BEFORE the .lrc is replaced, not after. That order is what guarantees a
// crash or a failed companion write never leaves a companion describing the
// lyric this write replaced. Proven by failing the .lrc write itself: the
// stale companion must already be gone.
func TestWriteLRC_StaleCompanionRemovedBeforeLRC(t *testing.T) {
	dir := t.TempDir()
	elrc := seedCompanion(t, dir)
	// A non-empty directory at the .lrc path makes its final Remove fail.
	if err := os.MkdirAll(filepath.Join(dir, "song.lrc", "x"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := modeWriter(false, true).WriteLRC(a2Song(), "song.lrc", dir); err == nil {
		t.Fatal("want an error when the .lrc cannot be written")
	}
	mustNotExist(t, elrc)
}

// TestWriteLRC_ForeignCompanionNeverRemoved: a .elrc without [by:canticle]
// is another tool's or the operator's. Neither an off-mode rewrite nor a
// demotion may delete it.
func TestWriteLRC_ForeignCompanionNeverRemoved(t *testing.T) {
	for _, tc := range []struct {
		name string
		song models.Song
	}{
		{"off-mode rewrite", a2Song()},
		{"demotion", guardSong(100, cue(10, "first line"), cue(120, "last line"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			elrc := filepath.Join(dir, "song.elrc")
			if err := os.WriteFile(elrc, []byte("[00:01.00]<00:01.00>foreign\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := modeWriter(false, false).WriteLRC(tc.song, "song.lrc", dir); err != nil {
				t.Fatalf("WriteLRC: %v", err)
			}
			if got := readFileString(t, elrc); !strings.Contains(got, "foreign") {
				t.Errorf("foreign companion was modified or removed: %q", got)
			}
		})
	}
}

// TestWriteLRC_FailedLRCWritesNoCompanion is the other half of the crash order:
// if the .lrc cannot land, no companion may appear beside the stale or missing
// line-synced file it would then silently disagree with.
func TestWriteLRC_FailedLRCWritesNoCompanion(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory at the .lrc path makes its final Remove fail.
	if err := os.MkdirAll(filepath.Join(dir, "song.lrc", "x"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := modeWriter(false, true).WriteLRC(a2Song(), "song.lrc", dir); err == nil {
		t.Fatal("want an error when the .lrc cannot be written")
	}
	mustNotExist(t, filepath.Join(dir, "song.elrc"))
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

// TestWriteLRC_ForeignCompanionNeverOverwritten: a sidecar-mode write that
// WOULD produce a companion must not replace a foreign .elrc either. The .lrc
// is still written; the companion is skipped.
func TestWriteLRC_ForeignCompanionNeverOverwritten(t *testing.T) {
	dir := t.TempDir()
	elrc := filepath.Join(dir, "song.elrc")
	if err := os.WriteFile(elrc, []byte("[00:01.00]<00:01.00>foreign\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := selfwrite.New(time.Minute)
	w := modeWriter(false, true)
	w.SetSelfWriteRegistry(reg)
	if err := w.WriteLRC(a2Song(), "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	mustExist(t, filepath.Join(dir, "song.lrc"))
	if got := readFileString(t, elrc); !strings.Contains(got, "foreign") {
		t.Errorf("foreign companion was overwritten: %q", got)
	}
	// An untouched path is not canticle's write: recording it would make the
	// watcher drop a third party's own change to it.
	if reg.Suppress(elrc) {
		t.Error("an untouched foreign companion was recorded as a self-write")
	}
}

// TestWriteLRC_SpecialFileAtCompanionPathNotOpened: a FIFO at the companion
// path is classified from Lstat and never opened. Opening one blocks until a
// writer appears, so a regression here hangs the test rather than failing it;
// the deadline turns that into a failure.
func TestWriteLRC_SpecialFileAtCompanionPathNotOpened(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "song.elrc")
	if err := mkfifo(fifo); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- modeWriter(false, true).WriteLRC(a2Song(), "song.lrc", dir) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WriteLRC: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WriteLRC blocked on the FIFO at the companion path")
	}
	mustExist(t, filepath.Join(dir, "song.lrc"))
	fi, err := os.Lstat(fifo)
	if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("the FIFO was replaced or removed: mode=%v err=%v", fi, err)
	}
}

// TestWriteLRC_FailedCompanionRemovalAbortsBeforeLRC: if an owned stale
// companion cannot be removed, the .lrc must NOT be replaced, or the old word
// timing would sit beside the new line timing.
func TestWriteLRC_FailedCompanionRemovalAbortsBeforeLRC(t *testing.T) {
	dir := t.TempDir()
	lrc := filepath.Join(dir, "song.lrc")
	if err := os.WriteFile(lrc, []byte("[00:01.00]old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedCompanion(t, dir)
	w := modeWriter(false, false)
	w.companionRemove = func(string) error { return errors.New("injected") }

	err := w.WriteLRC(a2Song(), "song.lrc", dir)
	if err == nil || !strings.Contains(err.Error(), "removing stale word-synced companion") {
		t.Fatalf("want the removal error, got %v", err)
	}
	if got := readFileString(t, lrc); !strings.Contains(got, "old") {
		t.Errorf(".lrc was replaced despite the failed companion removal: %q", got)
	}
}

// TestWordsLanded (#982 slice 4) reads each mode's landing off the disk the
// writer just produced: inline needs only qualifying words; a companion must
// be canticle's own; off, and words a2 refuses, never land.
func TestWordsLanded(t *testing.T) {
	refused := a2Song()
	refused.WordTimings = []models.WordTiming{{Line: 0, Text: "zzz", StartMS: 1500, EndMS: 2000}}
	cases := []struct {
		name              string
		inline, companion bool
		song              models.Song
		foreign, badName  bool
		want              bool
	}{
		{"inline", true, false, a2Song(), false, false, true},
		{"sidecar", false, true, a2Song(), false, false, true},
		{"sidecar, foreign companion", false, true, a2Song(), true, false, false},
		{"sidecar, unsafe filename", false, true, a2Song(), false, true, false},
		{"off", false, false, a2Song(), false, false, false},
		{"inline, words a2 refuses", true, false, refused, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			w := modeWriter(tc.inline, tc.companion)
			if tc.foreign {
				if err := os.WriteFile(filepath.Join(dir, "song.elrc"), []byte("[00:01.00]theirs\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.WriteLRC(tc.song, "song.lrc", dir); err != nil {
				t.Fatalf("WriteLRC: %v", err)
			}
			if got := w.WordSyncEnabled(); got != (tc.inline || tc.companion) {
				t.Fatalf("WordSyncEnabled = %v", got)
			}
			name := "song.lrc"
			if tc.badName {
				name = "../song.lrc"
			}
			if got := w.WordsLanded(tc.song, name, dir); got != tc.want {
				t.Fatalf("WordsLanded = %v; want %v", got, tc.want)
			}
		})
	}
	root := t.TempDir()
	if NewLRCWriter(root).WordsLanded(a2Song(), "song.lrc", filepath.Join(root, "missing")) {
		t.Fatal("WordsLanded = true for an unresolvable outdir")
	}
}
