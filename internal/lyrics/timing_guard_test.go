package lyrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/timing"
)

// guardSong builds a synced song with the given audio duration and cues.
func guardSong(audioSeconds int, lines ...models.Lines) models.Song {
	return models.Song{
		Track:                models.Track{ArtistName: "Artist", TrackName: "Track"},
		Subtitles:            models.Synced{Lines: lines},
		AudioDurationSeconds: audioSeconds,
	}
}

// cue builds a text-bearing line at a whole second.
func cue(sec int, text string) models.Lines {
	return models.Lines{
		Text: text,
		Time: models.Time{Total: float64(sec), Minutes: sec / 60, Seconds: sec % 60},
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected %s not to exist (err=%v)", path, err)
	}
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected %s to exist: %v", path, err)
	}
}

// TestWriteLRC_TimingGuard_DemoteFreshFetchWritesTxt is AC #3: a MisSynced
// result on a FRESH fetch (no sidecar on disk) must not be promoted to .lrc, but
// the words must survive as .txt. Demote-and-keep-words is content-safe: a
// flagged overrun is the right song's words with the wrong timing.
func TestWriteLRC_TimingGuard_DemoteFreshFetchWritesTxt(t *testing.T) {
	dir := t.TempDir()
	// Last sung cue at 120s against a 100s track: a 20s overrun, ratio 1.2 --
	// past Tolerance but under CategoricalRatio, so MisSynced.
	song := guardSong(100, cue(10, "first line"), cue(120, "last line"))

	w := NewLRCWriter()
	if err := w.WriteLRC(song, "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}

	mustNotExist(t, filepath.Join(dir, "song.lrc"))
	txt := filepath.Join(dir, "song.txt")
	mustExist(t, txt)

	data, err := os.ReadFile(txt) //nolint:gosec // test path from t.TempDir
	if err != nil {
		t.Fatalf("reading demoted .txt: %v", err)
	}
	body := string(data)
	for _, want := range []string{"first line", "last line"} {
		if !strings.Contains(body, want) {
			t.Errorf("demoted .txt is missing %q; got %q", want, body)
		}
	}
	// A demotion writes plain words, never timestamps: the timing is exactly
	// what was rejected.
	if strings.Contains(body, "[00:") || strings.Contains(body, "[02:00") {
		t.Errorf("demoted .txt must not carry LRC timestamps; got %q", body)
	}
}

// TestWriteLRC_TimingGuard_DemotePrefersLyricsBody: when the provider also
// served an unsynced body, that is the authoritative plain text and is what the
// demotion writes, rather than a reconstruction from the cues.
func TestWriteLRC_TimingGuard_DemotePrefersLyricsBody(t *testing.T) {
	dir := t.TempDir()
	song := guardSong(100, cue(10, "cue text"), cue(120, "late cue"))
	song.Lyrics = models.Lyrics{LyricsBody: "authoritative body\n"}

	w := NewLRCWriter()
	if err := w.WriteLRC(song, "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	mustNotExist(t, filepath.Join(dir, "song.lrc"))
	data, err := os.ReadFile(filepath.Join(dir, "song.txt")) //nolint:gosec // test path from t.TempDir
	if err != nil {
		t.Fatalf("reading demoted .txt: %v", err)
	}
	if string(data) != "authoritative body\n" {
		t.Errorf("demoted .txt = %q, want the provider's unsynced body verbatim", string(data))
	}
}

// TestWriteLRC_TimingGuard_DemoteUpgradeKeepsExistingTxt is the upgrade half
// of AC #3: a .txt already on disk is settled content. A MisSynced candidate
// must neither be promoted to .lrc nor allowed to churn (or truncate) the
// existing sidecar.
func TestWriteLRC_TimingGuard_DemoteUpgradeKeepsExistingTxt(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "song.txt")
	const settled = "the settled unsynced words\n"
	if err := os.WriteFile(existing, []byte(settled), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("seeding existing .txt: %v", err)
	}

	song := guardSong(100, cue(10, "first line"), cue(120, "last line"))
	w := NewLRCWriter()
	if err := w.WriteLRC(song, "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}

	mustNotExist(t, filepath.Join(dir, "song.lrc"))
	data, err := os.ReadFile(existing) //nolint:gosec // test path from t.TempDir
	if err != nil {
		t.Fatalf("reading existing .txt: %v", err)
	}
	if string(data) != settled {
		t.Errorf("existing .txt was modified: got %q, want %q", string(data), settled)
	}
	// Nothing else was left behind (no temp files, no second sidecar).
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected only the pre-existing .txt, found %d entries: %v", len(entries), entries)
	}
}

// TestWriteLRC_TimingGuard_CategoricalWritesNothing: a lyric timed to a
// different, longer recording is quarantined. Unlike MisSynced these words are
// not trustworthy content for this file, so nothing is written and nothing
// already on disk is disturbed.
func TestWriteLRC_TimingGuard_CategoricalWritesNothing(t *testing.T) {
	t.Run("fresh_fetch", func(t *testing.T) {
		dir := t.TempDir()
		// 400s cue vs a 100s track: ratio 4.0, well past CategoricalRatio.
		song := guardSong(100, cue(30, "a"), cue(400, "way past the end"))
		w := NewLRCWriter()
		if err := w.WriteLRC(song, "song.lrc", dir); err != nil {
			t.Fatalf("WriteLRC: %v", err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading dir: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("quarantine must write nothing, found %d entries: %v", len(entries), entries)
		}
	})

	t.Run("existing_txt_untouched", func(t *testing.T) {
		dir := t.TempDir()
		existing := filepath.Join(dir, "song.txt")
		const settled = "settled words\n"
		if err := os.WriteFile(existing, []byte(settled), 0o644); err != nil { //nolint:gosec // test fixture
			t.Fatalf("seeding existing .txt: %v", err)
		}
		song := guardSong(100, cue(30, "a"), cue(400, "way past the end"))
		w := NewLRCWriter()
		if err := w.WriteLRC(song, "song.lrc", dir); err != nil {
			t.Fatalf("WriteLRC: %v", err)
		}
		mustNotExist(t, filepath.Join(dir, "song.lrc"))
		data, err := os.ReadFile(existing) //nolint:gosec // test path from t.TempDir
		if err != nil {
			t.Fatalf("reading existing .txt: %v", err)
		}
		if string(data) != settled {
			t.Errorf("quarantine must not disturb an existing .txt: got %q", string(data))
		}
	})
}

// TestWriteLRC_TimingGuard_TrailingDecorativeMarkerStillPromotes is the
// Investigation-0 regression guard, end to end at the write boundary: ~33% of
// naively-flagged files are perfectly-synced lyrics whose only past-duration
// timestamp is a trailing decorative marker. The guard consumes
// timing.Evaluate's corrected max (text-bearing lines only), so these must
// still be promoted to .lrc. Computing a max here instead would demote a third
// of the flagged corpus.
func TestWriteLRC_TimingGuard_TrailingDecorativeMarkerStillPromotes(t *testing.T) {
	markers := []struct{ name, text string }{
		{"bare music note", "♪"},
		{"repeated glyphs", "♪♪♪"},
		{"instrumental marker form", "♪ Instrumental ♪"},
		{"whitespace only", "   "},
		{"lrc tag line", "[ar:Some Artist]"},
	}
	for _, m := range markers {
		t.Run(m.name, func(t *testing.T) {
			dir := t.TempDir()
			// Last SUNG line at 90s sits comfortably inside a 100s track; the
			// decorative cue at 160s would be a 60s overrun on a raw max.
			song := guardSong(100, cue(10, "a"), cue(90, "last sung line"), cue(160, m.text))
			w := NewLRCWriter()
			if err := w.WriteLRC(song, "song.lrc", dir); err != nil {
				t.Fatalf("WriteLRC: %v", err)
			}
			mustExist(t, filepath.Join(dir, "song.lrc"))
			mustNotExist(t, filepath.Join(dir, "song.txt"))
		})
	}
}

// TestWriteLRC_TimingGuard_UnknownDurationFailsOpen is AC #4. An unknown
// duration is not evidence of anything; rejecting on it would demote every
// track whose file carries no duration tag.
func TestWriteLRC_TimingGuard_UnknownDurationFailsOpen(t *testing.T) {
	for _, dur := range []int{0, -5} {
		dir := t.TempDir()
		// A cue far past any plausible end -- only the unknown duration saves it.
		song := guardSong(dur, cue(10, "a"), cue(4000, "b"))
		w := NewLRCWriter()
		if err := w.WriteLRC(song, "song.lrc", dir); err != nil {
			t.Fatalf("WriteLRC(duration=%d): %v", dur, err)
		}
		mustExist(t, filepath.Join(dir, "song.lrc"))
	}
}

// TestWriteLRC_TimingGuard_CompliantSyncedStillRemovesStaleTxt pins that the
// guard is inserted BEFORE the promotion rather than replacing it: an Ok
// verdict keeps the pre-existing opposite-sidecar cleanup exactly as it was.
func TestWriteLRC_TimingGuard_CompliantSyncedStillRemovesStaleTxt(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "song.txt")
	if err := os.WriteFile(stale, []byte("old unsynced"), 0o644); err != nil { //nolint:gosec // test fixture
		t.Fatalf("seeding stale .txt: %v", err)
	}
	song := guardSong(100, cue(10, "a"), cue(95, "b"))
	w := NewLRCWriter()
	if err := w.WriteLRC(song, "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	mustExist(t, filepath.Join(dir, "song.lrc"))
	mustNotExist(t, stale)
}

// TestWriteLRC_TimingGuard_FallsBackToTrackLength: when no audio-file duration
// is threaded through, the guard still evaluates against the provider's catalog
// length rather than silently failing open. This comparison is near-circular
// (the lyric was timed against that same catalog length), so it catches only
// gross mismatches -- which is why the worker and fetch mode both set
// AudioDurationSeconds from the file.
func TestWriteLRC_TimingGuard_FallsBackToTrackLength(t *testing.T) {
	dir := t.TempDir()
	song := models.Song{
		Track: models.Track{ArtistName: "Artist", TrackName: "Track", TrackLength: 100},
		Subtitles: models.Synced{Lines: []models.Lines{
			cue(10, "a"), cue(400, "way past the end"),
		}},
	}
	w := NewLRCWriter()
	if err := w.WriteLRC(song, "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	mustNotExist(t, filepath.Join(dir, "song.lrc"))
}

// TestWriteLRC_TimingGuard_InstrumentalIsNotGated: the instrumental branch is
// authoritative and carries no lyric timing to judge. Musixmatch delivers a
// synced subtitle line alongside the instrumental flag, so a guard that ran on
// subtitles regardless of branch would quarantine instrumental markers.
func TestWriteLRC_TimingGuard_InstrumentalIsNotGated(t *testing.T) {
	dir := t.TempDir()
	song := guardSong(100, cue(400, "way past the end"))
	song.Track.Instrumental = 1
	w := NewLRCWriter()
	if err := w.WriteLRC(song, "song.txt", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	mustExist(t, filepath.Join(dir, "song.txt"))
}

// TestDecidePromotion covers the exported decision the worker consumes to label
// the row's outcome_type, so the DB record matches what actually landed on disk.
func TestDecidePromotion(t *testing.T) {
	tests := []struct {
		name string
		song models.Song
		want PromotionDecision
	}{
		{"non-synced song is promoted verbatim", models.Song{
			Track:  models.Track{ArtistName: "a", TrackName: "b"},
			Lyrics: models.Lyrics{LyricsBody: "words"},
		}, PromoteAsIs},
		{"compliant synced", guardSong(100, cue(90, "a")), PromoteAsIs},
		{"unknown duration fails open", guardSong(0, cue(4000, "a")), PromoteAsIs},
		{"trailing decorative marker", guardSong(100, cue(90, "a"), cue(400, "♪")), PromoteAsIs},
		{"MisSynced demotes", guardSong(100, cue(120, "a")), DemoteToUnsynced},
		{"categorical quarantines", guardSong(100, cue(400, "a")), Quarantine},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, _ := DecidePromotion(tt.song)
			if got != tt.want {
				t.Errorf("DecidePromotion() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestWriteLRC_TimingGuard_DegenerateWritesTxt covers #673 at accept time: an
// .lrc whose cues all share one timestamp is not synced, so it must never be
// written as one. The words are typically correct -- only the timing is
// fabricated -- so this demotes to .txt exactly as MisSynced does, rather than
// discarding.
//
// The pre-fix behavior was a silent PROMOTION: timing.Evaluate returned Ok for
// this shape (an all-zero file has max 0, so it never overruns), and
// DecidePromotion's default arm fails open by design. A new outcome that nobody
// maps therefore ships as "write the bad file", which is the failure mode this
// test pins.
func TestWriteLRC_TimingGuard_DegenerateWritesTxt(t *testing.T) {
	dir := t.TempDir()
	// The #673 production shape: many cues, one distinct timestamp.
	song := guardSong(240,
		cue(0, "first line"),
		cue(0, "second line"),
		cue(0, "third line"),
	)

	w := NewLRCWriter()
	if err := w.WriteLRC(song, "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}

	mustNotExist(t, filepath.Join(dir, "song.lrc"))
	txt := filepath.Join(dir, "song.txt")
	mustExist(t, txt)

	data, err := os.ReadFile(txt) //nolint:gosec // test path from t.TempDir
	if err != nil {
		t.Fatalf("reading demoted .txt: %v", err)
	}
	body := string(data)
	// The words survive the demotion -- that is the whole point of demoting
	// rather than quarantining.
	for _, want := range []string{"first line", "second line", "third line"} {
		if !strings.Contains(body, want) {
			t.Errorf("demoted .txt is missing %q; got %q", want, body)
		}
	}
	// And the fake timing does not.
	if strings.Contains(body, "[00:") {
		t.Errorf("demoted .txt must not carry LRC timestamps; got %q", body)
	}
}

// TestDecidePromotion_DegenerateDemotes pins the decision directly, without the
// filesystem. The writer test above proves the end-to-end effect; this proves
// the mapping itself, so a regression is attributable to the arm rather than to
// any write-path change.
func TestDecidePromotion_DegenerateDemotes(t *testing.T) {
	song := guardSong(240, cue(0, "a"), cue(0, "b"))

	decision, outcome, mag := DecidePromotion(song)

	if decision != DemoteToUnsynced {
		t.Errorf("decision = %v; want %v -- a degenerate lyric must never be promoted as synced", decision, DemoteToUnsynced)
	}
	if outcome != timing.Degenerate {
		t.Errorf("outcome = %q; want %q", outcome, timing.Degenerate)
	}
	// The magnitude derives from a timestamp the file does not honestly carry,
	// so it must not be persisted as if it were measured.
	if mag.Measured {
		t.Error("degenerate decision reports Measured=true; the numbers are fabricated and would land in the metrics columns")
	}
}
