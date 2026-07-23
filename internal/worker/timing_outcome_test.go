package worker

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/timing"
)

func tLine(sec float64, text string) models.Lines {
	return models.Lines{Text: text, Time: models.Time{Total: sec}}
}

// syncedSong deliberately leaves Track.TrackLength ZERO: the duration reaches
// the predicate as a separate argument (the audio-file value, not the provider
// value the song carries), so a test that relied on the song's own length would
// silently pass against the wrong source. Track length here would be a trap.
func syncedSong(lines ...models.Lines) models.Song {
	return models.Song{
		Track:     models.Track{ArtistName: "A", TrackName: "T"},
		Subtitles: models.Synced{Lines: lines},
	}
}

var testNow = time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

// errStamp stands in for a bookkeeping write failure.
var errStamp = errors.New("stamp failed")

func TestTimingRecordFromSong(t *testing.T) {
	tests := []struct {
		name         string
		song         models.Song
		duration     int
		wantOutcome  string
		wantMeasured bool
	}{
		{
			name:         "compliant synced lyric",
			song:         syncedSong(tLine(10, "a"), tLine(150, "b")),
			duration:     200,
			wantOutcome:  string(timing.Ok),
			wantMeasured: true,
		},
		{
			name:         "overrun past tolerance",
			song:         syncedSong(tLine(10, "a"), tLine(120, "b")),
			duration:     100,
			wantOutcome:  string(timing.MisSynced),
			wantMeasured: true,
		},
		{
			name:         "far past the end",
			song:         syncedSong(tLine(400, "a")),
			duration:     100,
			wantOutcome:  string(timing.Categorical),
			wantMeasured: true,
		},
		{
			// TrackLength 0 is the real state whenever recording enrichment is
			// off or the file's tags are unreadable. It must fail open AND be
			// distinguishable from a measured verdict.
			name:         "unknown duration fails open and is unmeasured",
			song:         syncedSong(tLine(400, "a")),
			duration:     0,
			wantOutcome:  string(timing.UnknownDuration),
			wantMeasured: false,
		},
		{
			// A synced result (len > 0, so a .lrc is written) whose only lines
			// are decorative: Evaluate returns Ok with no comparison. This must
			// persist as UNMEASURED, or a fake 0 magnitude lands in the column.
			name:         "all-decorative synced lyric is unmeasured",
			song:         syncedSong(tLine(400, "♪"), tLine(500, "[ar:x]")),
			duration:     100,
			wantOutcome:  string(timing.Ok),
			wantMeasured: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := timingRecordFromSong(tt.song, tt.duration, testNow)
			if rec.Outcome != tt.wantOutcome {
				t.Errorf("Outcome = %q, want %q", rec.Outcome, tt.wantOutcome)
			}
			if rec.Measured != tt.wantMeasured {
				t.Errorf("Measured = %v, want %v", rec.Measured, tt.wantMeasured)
			}
			// An unmeasured record must carry no magnitude: a downstream stamp
			// that ignored Measured would otherwise persist a fake 0.
			if !rec.Measured && (rec.Magnitude != 0 || rec.Ratio != 0) {
				t.Errorf("unmeasured record carries magnitude: %+v", rec)
			}
			if !rec.EvaluatedAt.Equal(testNow) {
				t.Errorf("EvaluatedAt = %v, want %v", rec.EvaluatedAt, testNow)
			}
		})
	}
}

// TestTimingRecordFromSong_NonSyncedLeavesNoVerdict: an unsynced or instrumental
// settle carries no line timing, so there is nothing to judge. That is distinct
// from a verdict of "fine" and must leave the columns NULL.
func TestTimingRecordFromSong_NonSyncedLeavesNoVerdict(t *testing.T) {
	for _, tc := range []struct {
		name string
		song models.Song
	}{
		{"unsynced", models.Song{Lyrics: models.Lyrics{LyricsBody: "some words"}}},
		{"instrumental", models.Song{Track: models.Track{Instrumental: 1}}},
		{"empty", models.Song{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := timingRecordFromSong(tc.song, 100, testNow); rec != (queue.TimingRecord{}) {
				t.Errorf("record = %+v, want zero value (no line timing to judge)", rec)
			}
		})
	}
}

// TestTimingRecordFromSong_DelegatesToTimingPackage is the anti-duplication
// guard, and the reason #438 landed first. A trailing decorative marker past the
// audio end is the ~31% false-positive case: any reimplementation here that
// keyed on the raw last cue would report an overrun. Delegation is what makes
// this Ok, so this test fails the moment the logic is forked.
func TestTimingRecordFromSong_DelegatesToTimingPackage(t *testing.T) {
	// Last sung line at 90s inside a 100s track; only the decorative glyph at
	// 160s sits past the end.
	song := syncedSong(tLine(10, "a"), tLine(90, "b"), tLine(160, "♪"))
	rec := timingRecordFromSong(song, 100, testNow)
	if rec.Outcome != string(timing.Ok) {
		t.Errorf("Outcome = %q, want ok (a trailing decorative marker is not an overrun)", rec.Outcome)
	}
	if rec.Magnitude > 0 {
		t.Errorf("Magnitude = %v, want <= 0 (max must ignore the decorative line)", rec.Magnitude)
	}
}

// TestStampTimingOutcome_PersistsVerdict covers the wiring: the verdict reaches
// the queue, and a non-synced settle writes nothing at all.
func TestStampTimingOutcome_PersistsVerdict(t *testing.T) {
	q := &fakeQueue{}
	w := &Worker{queue: q, now: func() time.Time { return testNow }}
	item := queue.WorkItem{ID: 7, Inputs: models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "T"}}}

	w.stampTimingOutcome(t.Context(), item, syncedSong(tLine(120, "a")), 100)
	rec, ok := q.timingOutcomes[7]
	if !ok {
		t.Fatal("no timing outcome stamped")
	}
	if rec.Outcome != string(timing.MisSynced) {
		t.Errorf("Outcome = %q, want mis_synced", rec.Outcome)
	}
	if rec.Magnitude != 20 {
		t.Errorf("Magnitude = %v, want 20", rec.Magnitude)
	}

	// A non-synced settle stamps nothing: no row should be touched.
	q2 := &fakeQueue{}
	w2 := &Worker{queue: q2, now: func() time.Time { return testNow }}
	w2.stampTimingOutcome(t.Context(), queue.WorkItem{ID: 8},
		models.Song{Lyrics: models.Lyrics{LyricsBody: "words"}}, 100)
	if len(q2.timingOutcomes) != 0 {
		t.Errorf("stamped %d outcomes for a non-synced settle, want 0", len(q2.timingOutcomes))
	}
}

// TestStampTimingOutcome_StampFailureIsNonFatal: the output is already on disk
// by this point, so a bookkeeping write must never fail the item.
func TestStampTimingOutcome_StampFailureIsNonFatal(t *testing.T) {
	q := &fakeQueue{timingOutcomeErr: errStamp}
	w := &Worker{queue: q, now: func() time.Time { return testNow }}
	// Must not panic and must return normally.
	w.stampTimingOutcome(t.Context(), queue.WorkItem{ID: 9}, syncedSong(tLine(120, "a")), 100)
}

// TestStampTimingOutcome_LogsOnlyNonCompliant pins the "log overruns" half of
// the change: a non-compliant result emits exactly one warn carrying the outcome
// and magnitude, and a compliant or unmeasured result emits none. Without this,
// dropping the warn leaves the suite green.
func TestStampTimingOutcome_LogsOnlyNonCompliant(t *testing.T) {
	tests := []struct {
		name     string
		song     models.Song
		duration int
		wantLog  bool
	}{
		{"overrun logs", syncedSong(tLine(120, "a")), 100, true},
		{"categorical logs", syncedSong(tLine(400, "a")), 100, true},
		{"compliant is silent", syncedSong(tLine(50, "a")), 100, false},
		{"unknown duration is silent", syncedSong(tLine(400, "a")), 0, false},
		{"non-synced is silent", models.Song{Lyrics: models.Lyrics{LyricsBody: "w"}}, 100, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			defer slog.SetDefault(prev)

			q := &fakeQueue{}
			w := &Worker{queue: q, now: func() time.Time { return testNow }}
			w.stampTimingOutcome(t.Context(), queue.WorkItem{ID: 1,
				Inputs: models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "T"}}}, tt.song, tt.duration)

			logged := strings.Contains(buf.String(), "timing overruns the audio")
			if logged != tt.wantLog {
				t.Errorf("overrun logged = %v, want %v (log: %q)", logged, tt.wantLog, buf.String())
			}
			if tt.wantLog && !strings.Contains(buf.String(), "outcome=") {
				t.Errorf("overrun log missing the outcome field: %q", buf.String())
			}
		})
	}
}
