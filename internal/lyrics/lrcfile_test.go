package lyrics

import (
	"os"
	"path/filepath"
	"testing"

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

// TestPlainBody_StripsWordMarkers is the C2 fix (#480 prerequisite).
//
// PlainBody flattens cues read back OFF DISK, so once canticle writes A2 word
// markers those cues carry them. Without stripping, a demotion persists
// timestamp garbage into the user's plain-lyrics .txt -- and that is not
// recoverable from the .txt afterwards.
//
// This is independent of what triggers the demotion: any A2 file that demotes
// for any legitimate reason (a genuine overrun) hits it. Only the disk-read
// path is affected -- the accept-time demotion flattens song.Subtitles, which
// is unmarked -- and the disk-read path is the one that runs over the whole
// library.
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
