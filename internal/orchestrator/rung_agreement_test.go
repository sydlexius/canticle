package orchestrator

import (
	"testing"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
)

// TestQualityAgreesWithRung pins the dispatch ranking (QualityOf) to the
// writer's no-downgrade ladder (lyrics.RungOfSong, #553) over the five
// CANONICAL shapes only (one song per rung): every pair must order the same
// way under both, so the orchestrator's preferred canonical result is never
// one the writer then refuses as a downgrade. Mixed shapes are NOT pinned to
// agree; see TestQualityRungKnownDivergences.
func TestQualityAgreesWithRung(t *testing.T) {
	line := models.Lines{Text: "alpha beta", Time: models.Time{Total: 1.5, Seconds: 1, Hundredths: 50}}
	songs := []models.Song{
		{},
		{Track: models.Track{Instrumental: 1}},
		{Lyrics: models.Lyrics{LyricsBody: "words"}},
		{Subtitles: models.Synced{Lines: []models.Lines{line}}},
		{Subtitles: models.Synced{Lines: []models.Lines{line}}, WordTimings: []models.WordTiming{
			{Line: 0, Text: "alpha ", StartMS: 1500, EndMS: 2000},
			{Line: 0, Text: "beta", StartMS: 2000, EndMS: 2500},
		}},
	}
	for i, a := range songs {
		if got := int(lyrics.RungOfSong(a)); got != int(QualityOf(a)) {
			t.Errorf("song %d: rung %d != quality %d", i, got, QualityOf(a))
		}
		for j, b := range songs {
			if (QualityOf(a) < QualityOf(b)) != (lyrics.RungOfSong(a) < lyrics.RungOfSong(b)) {
				t.Errorf("songs %d,%d order differently: quality %d<%d vs rung %d<%d",
					i, j, QualityOf(a), QualityOf(b), lyrics.RungOfSong(a), lyrics.RungOfSong(b))
			}
		}
	}
}

// TestQualityRungKnownDivergences records the mixed shapes on which QualityOf
// and lyrics.RungOfSong currently DISAGREE (#553 review), asserting today's
// values so a change to either ranking is visible here rather than silent.
// QualityOf ranks content presence (subtitles, then body, then the flag, and
// any word timings at all); RungOfSong follows WriteLRC's content selection
// (the instrumental flag wins) and the writer's word predicate
// (HasQualifyingWords). These are not asserted to be correct, only current.
func TestQualityRungKnownDivergences(t *testing.T) {
	line := models.Lines{Text: "alpha beta", Time: models.Time{Total: 1.5, Seconds: 1, Hundredths: 50}}
	synced := models.Synced{Lines: []models.Lines{line}}
	for _, tc := range []struct {
		name    string
		song    models.Song
		quality Quality
		rung    lyrics.Rung
	}{
		{"instrumental flag beside subtitles", models.Song{Track: models.Track{Instrumental: 1}, Subtitles: synced},
			QualitySynced, lyrics.RungInstrumental},
		{"instrumental flag beside a body", models.Song{Track: models.Track{Instrumental: 1}, Lyrics: models.Lyrics{LyricsBody: "words"}},
			QualityUnsynced, lyrics.RungInstrumental},
		// The words do not reconstruct the cue text, so HasQualifyingWords
		// rejects them while QualityOf counts any timing at all.
		{"non-qualifying word timings", models.Song{Subtitles: synced, WordTimings: []models.WordTiming{
			{Line: 0, Text: "gamma", StartMS: 1500, EndMS: 2000},
		}}, QualityWordSynced, lyrics.RungLine},
	} {
		if q, r := QualityOf(tc.song), lyrics.RungOfSong(tc.song); q != tc.quality || r != tc.rung {
			t.Errorf("%s: quality %d rung %d; recorded divergence is quality %d rung %d",
				tc.name, q, r, tc.quality, tc.rung)
		}
	}
}
