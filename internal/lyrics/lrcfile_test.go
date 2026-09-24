package lyrics

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/lrcnormalize"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/timing"
)

// writeLRCFixture writes body to a temp .lrc and returns its path.
func writeLRCFixture(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fixture.lrc")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

// TestReadSyncedLRC_HeaderTagsAreNotCues is the seam's core contract: a
// canticle-written header must never reach the verdict as lyric timing.
func TestReadSyncedLRC_HeaderTagsAreNotCues(t *testing.T) {
	p := writeLRCFixture(t, "[ar:Placeholder Artist]\n[ti:Placeholder Title]\n[source:testlane]\n\n[00:01.00]alpha\n[00:02.00]beta\n")
	synced, err := ReadSyncedLRC(p)
	if err != nil {
		t.Fatalf("ReadSyncedLRC: %v", err)
	}
	if len(synced.Lines) != 2 {
		t.Fatalf("cue count = %d, want 2 (header tags must not be cues): %+v", len(synced.Lines), synced.Lines)
	}
	if synced.Lines[0].Time.Total != 1 || synced.Lines[1].Time.Total != 2 {
		t.Errorf("cue times = %v/%v, want 1/2", synced.Lines[0].Time.Total, synced.Lines[1].Time.Total)
	}
}

// TestReadSyncedLRC_ExpandsStackedTimestamps proves the seam reuses
// lrcnormalize rather than a second parser: a stacked line yields one cue per
// timestamp.
func TestReadSyncedLRC_ExpandsStackedTimestamps(t *testing.T) {
	p := writeLRCFixture(t, "[00:05.00][01:10.00]refrain\n")
	synced, err := ReadSyncedLRC(p)
	if err != nil {
		t.Fatalf("ReadSyncedLRC: %v", err)
	}
	if len(synced.Lines) != 2 {
		t.Fatalf("cue count = %d, want 2 expanded cues", len(synced.Lines))
	}
	if synced.Lines[1].Time.Total != 70 {
		t.Errorf("second cue = %v, want 70", synced.Lines[1].Time.Total)
	}
}

// TestReadSyncedLRC_BOMStripped: a BOM must not blind the first line.
func TestReadSyncedLRC_BOMStripped(t *testing.T) {
	p := writeLRCFixture(t, "\ufeff[00:01.00]alpha\n")
	synced, err := ReadSyncedLRC(p)
	if err != nil {
		t.Fatalf("ReadSyncedLRC: %v", err)
	}
	if len(synced.Lines) != 1 {
		t.Fatalf("cue count = %d, want 1", len(synced.Lines))
	}
}

// TestReadSyncedLRC_NoCuesIsNotAnError: an unparsable sidecar is a state, not a
// failure, and must reach the predicate as "no timing evidence".
func TestReadSyncedLRC_NoCuesIsNotAnError(t *testing.T) {
	p := writeLRCFixture(t, "just some plain words\nand more of them\n")
	synced, err := ReadSyncedLRC(p)
	if err != nil {
		t.Fatalf("ReadSyncedLRC: %v", err)
	}
	if len(synced.Lines) != 0 {
		t.Fatalf("cue count = %d, want 0", len(synced.Lines))
	}
	outcome, _, _, err := EvaluateLRCFile(p, 180)
	if err != nil {
		t.Fatalf("EvaluateLRCFile: %v", err)
	}
	if outcome != timing.Ok {
		t.Errorf("outcome = %q, want %q (no timing evidence must fail open)", outcome, timing.Ok)
	}
}

func TestReadSyncedLRC_MissingFileErrors(t *testing.T) {
	if _, err := ReadSyncedLRC(filepath.Join(t.TempDir(), "absent.lrc")); err == nil {
		t.Fatal("want an error for a missing file")
	}
}

// TestEvaluateLRCFile_Outcomes walks the seam end-to-end for each verdict.
func TestEvaluateLRCFile_Outcomes(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		duration int
		want     timing.TimingOutcome
	}{
		{"ok", "[00:10.00]alpha\n[01:00.00]beta\n", 120, timing.Ok},
		{"mis_synced", "[00:10.00]alpha\n[02:30.00]beta\n", 120, timing.MisSynced},
		{"categorical", "[00:10.00]alpha\n[05:00.00]beta\n", 120, timing.Categorical},
		{"unknown_duration", "[00:10.00]alpha\n[05:00.00]beta\n", 0, timing.UnknownDuration},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := writeLRCFixture(t, tc.body)
			got, _, cues, err := EvaluateLRCFile(p, tc.duration)
			if err != nil {
				t.Fatalf("EvaluateLRCFile: %v", err)
			}
			if got != tc.want {
				t.Errorf("outcome = %q, want %q", got, tc.want)
			}
			if cues != 2 {
				t.Errorf("cues = %d, want 2", cues)
			}
		})
	}
}

// TestEvaluateLRCFile_TrailingDecorativeMarkerIsNotFlagged is the ~33% case from
// Investigation-0 on #438: a perfectly-synced lyric whose ONLY past-duration
// timestamp is a trailing decorative music-note marker. A naive max-timestamp
// check calls this MisSynced and destroys a good file. The seam must consume
// timing.Evaluate's corrected max, so the verdict is Ok.
func TestEvaluateLRCFile_TrailingDecorativeMarkerIsNotFlagged(t *testing.T) {
	// Last SUNG line at 1:50 against a 120s track (fine); a decorative marker
	// parked at 3:00, which is both past Tolerance and past CategoricalRatio.
	for _, marker := range []string{"♪", "♫ ♫", "♪ Instrumental ♪", "\U0001F3B5"} {
		p := writeLRCFixture(t, "[00:10.00]alpha\n[01:50.00]beta\n[03:00.00]"+marker+"\n")
		got, mag, _, err := EvaluateLRCFile(p, 120)
		if err != nil {
			t.Fatalf("EvaluateLRCFile: %v", err)
		}
		if got != timing.Ok {
			t.Errorf("marker %q: outcome = %q, want %q -- a trailing decorative marker must NOT be remediated", marker, got, timing.Ok)
		}
		if mag.Measured && mag.OverrunSeconds > timing.Tolerance {
			t.Errorf("marker %q: magnitude %v came from the RAW max, not the corrected one", marker, mag.OverrunSeconds)
		}
	}
}

// TestPlainBody_DropsDecorativeCues: a demoted .txt gains no stray marker lines.
func TestPlainBody_DropsDecorativeCues(t *testing.T) {
	synced := models.Synced{Lines: []models.Lines{
		{Text: "alpha", Time: models.Time{Total: 1}},
		{Text: "♪", Time: models.Time{Total: 2}},
		{Text: "  ", Time: models.Time{Total: 3}},
		{Text: "beta", Time: models.Time{Total: 4}},
	}}
	if got, want := PlainBody(synced), "alpha\nbeta\n"; got != want {
		t.Errorf("PlainBody = %q, want %q", got, want)
	}
}

func TestPlainBody_AllDecorativeIsEmpty(t *testing.T) {
	synced := models.Synced{Lines: []models.Lines{{Text: "♪", Time: models.Time{Total: 1}}}}
	if got := PlainBody(synced); got != "" {
		t.Errorf("PlainBody = %q, want empty", got)
	}
}

// TestClassifySynced covers the pure cue-based classification (#1075): word
// markers win, then line cues, then unsynced for no cues at all.
func TestClassifySynced(t *testing.T) {
	tests := []struct {
		name string
		body string
		want SyncTier
	}{
		{"word markers", "[00:01.00]<00:01.00>alpha <00:01.50>beta\n", TierWord},
		{"line only", "[00:01.00]alpha beta\n[00:02.00]gamma\n", TierLine},
		{"no timestamps", "just plain words\nand more\n", TierUnsynced},
		{"empty", "", TierUnsynced},
		// Documented edge cases (#1075 hostile-review finding 6), pinned as
		// characterization tests rather than fixed: canticle's own A2 grammar
		// is exactly 2 fractional digits (timing.wordMarkerRe), so a 3-digit-ms
		// marker does not match and reads as plain (unstripped) text -> Line.
		{"three-digit-ms marker does not match the A2 grammar: reads line", "[00:01.00]<00:01.000>alpha\n", TierLine},
		// A cue that is only a marker plus a decorative character still reads
		// Word: ClassifySynced asks whether stripping the marker changed the
		// text, not whether what remains is meaningful lyric content (that is
		// timing.IsDecorative, a different predicate used for what a demotion
		// persists).
		{"marker plus decorative-only text still reads word", "[00:01.00]<00:01.00>♪\n", TierWord},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := lrcnormalize.ParseBody(tc.body)
			if got := ClassifySynced(models.Synced{Lines: doc.Cues}); got != tc.want {
				t.Errorf("ClassifySynced(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// TestClassifyLRCFile covers the disk-reading wrapper: plain cue tiers (no
// companion involved) plus the error passthrough for a missing file.
func TestClassifyLRCFile(t *testing.T) {
	tests := []struct {
		name string
		body string
		want SyncTier
	}{
		{"line only", "[00:01.00]alpha\n[00:02.00]beta\n", TierLine},
		{"word markers", "[00:01.00]<00:01.00>alpha <00:01.50>beta\n", TierWord},
		{"no timestamps", "just some plain words\nand more of them\n", TierUnsynced},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ClassifyLRCFile(writeLRCFixture(t, tc.body))
			if err != nil {
				t.Fatalf("ClassifyLRCFile: %v", err)
			}
			if got != tc.want {
				t.Errorf("tier = %q, want %q", got, tc.want)
			}
		})
	}
	t.Run("missing file errors", func(t *testing.T) {
		if _, err := ClassifyLRCFile(filepath.Join(t.TempDir(), "absent.lrc")); err == nil {
			t.Fatal("want an error for a missing file")
		}
	})
}

// TestClassifyLRCFile_Companion covers the .elrc companion upgrade: only an
// OWNED companion ([by:canticle]) upgrades a plain line-synced .lrc to Word;
// a foreign one (matching OwnedCompanionOf's own foreign-file rule) never
// does. lrcBody lets a case override the default line-synced fixture --
// "unsynced .lrc, owned companion" (#1075 hostile-review finding 5) proves
// the upgrade is scoped to Line only: a .lrc with NO timestamps at all stays
// Unsynced even beside an owned companion, because a live write can never
// produce that pair (planCompanion only writes a fresh companion for a
// synced write with a qualifying line, and removes an owned one on any
// unsynced settle) -- so an unsynced .lrc paired with one on disk is a stale
// companion nothing has pruned, not evidence the file is word-synced.
func TestClassifyLRCFile_Companion(t *testing.T) {
	for _, tc := range []struct {
		name, lrcBody, companionBody string
		want                         SyncTier
	}{
		{"owned upgrades line to word", "", "[by:canticle]\n[00:01.00]<00:01.00>alpha\n", TierWord},
		{"foreign never upgrades", "", "[00:01.00]<00:01.00>alpha\n", TierLine},
		{"owned companion never upgrades an unsynced lrc", "just plain words\nno timestamps\n",
			"[by:canticle]\n[00:01.00]<00:01.00>alpha\n", TierUnsynced},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			lrc := filepath.Join(dir, "fixture.lrc")
			body := tc.lrcBody
			if body == "" {
				body = "[00:01.00]alpha\n[00:02.00]beta\n"
			}
			if err := os.WriteFile(lrc, []byte(body), 0o600); err != nil {
				t.Fatalf("write lrc: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "fixture.elrc"), []byte(tc.companionBody), 0o600); err != nil {
				t.Fatalf("write companion: %v", err)
			}
			got, err := ClassifyLRCFile(lrc)
			if err != nil {
				t.Fatalf("ClassifyLRCFile: %v", err)
			}
			if got != tc.want {
				t.Errorf("tier = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPlainBody_StripsWordMarkers is the C2 fix (#480 prerequisite): PlainBody
// flattens cues read back off disk, so an A2-marked cue must not persist
// timestamp garbage into the user's plain-lyrics .txt.
func TestPlainBody_StripsWordMarkers(t *testing.T) {
	got := PlainBody(models.Synced{Lines: []models.Lines{
		{Text: "<00:01.50>alpha <00:02.00>beta"},
		{Text: "<05:00.00>♪"}, // decorative even when marked: dropped
		{Text: "<00:03.00>gamma"},
	}})

	want := "alpha beta\ngamma\n"
	if got != want {
		t.Errorf("PlainBody = %q; want %q -- a demoted .txt must carry words, never timestamps", got, want)
	}
}
