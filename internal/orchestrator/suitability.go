package orchestrator

import (
	"log/slog"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
)

// Quality ranks a lyric result by usefulness. A higher value is strictly
// better: synced > unsynced > instrumental > none. The orchestrator uses it to
// pick the best-available fallback when no lane yields a suitable result.
type Quality int

const (
	// QualityNone means the result carries no usable lyrics and is not even an
	// instrumental marker (empty body, no synced lines, not flagged instrumental).
	QualityNone Quality = iota
	// QualityInstrumental means the track is flagged instrumental but carries no
	// lyric text. It is a valid best-available final output (the worker writes the
	// instrumental marker) but is NOT suitable on its own (see IsSuitable).
	QualityInstrumental
	// QualityUnsynced means the result carries an unsynced lyric body.
	QualityUnsynced
	// QualitySynced means the result carries line-level time-synced subtitles.
	QualitySynced
	// QualityWordSynced means the result carries per-word timings in addition to
	// line cues. It ranks above QualitySynced because it is strictly more
	// information: the line cues are still present, plus word-level detail the
	// Enhanced-LRC (A2) writer can use.
	QualityWordSynced
)

// QualityOf classifies a song's lyric quality. The precedence mirrors the LRC
// writer's content selection (synced lines, then an unsynced body, then an
// instrumental marker), so the orchestrator's ranking agrees with what the
// writer will actually emit.
//
// Word-sync is recognized by the presence of WordTimings alongside line cues.
// ANY word timings promote the result, deliberately: for RANKING purposes a
// partially word-timed result is still strictly better than a line-only one --
// the line cues are unchanged and the word detail is a bonus, so preferring it
// can never make output worse.
//
// That is a ranking rule, NOT a terminal-state rule. Whether a result is good
// enough to stop improving needs a higher bar, because coverage is uneven in
// practice -- see the measurement cited on models.Song.WordTimings. Marking a
// half-timed result terminal would permanently exclude it from upgrade. That
// threshold belongs to the upgrade-eligibility policy (#553), which owns
// terminal-ness; this function only orders results.
func QualityOf(song models.Song) Quality {
	switch {
	case len(song.Subtitles.Lines) > 0 && len(song.WordTimings) > 0:
		return QualityWordSynced
	case len(song.Subtitles.Lines) > 0:
		return QualitySynced
	case song.Lyrics.LyricsBody != "":
		return QualityUnsynced
	case song.Track.Instrumental == 1:
		return QualityInstrumental
	default:
		return QualityNone
	}
}

// ScriptGuard rejects lyric results whose body is dominated by scripts outside
// a configured allowlist. It matches worker.ScriptGuard so the same concrete
// guard (internal/langguard) satisfies both. A nil guard, or one whose Enabled
// reports false, imposes no filtering.
type ScriptGuard interface {
	Accept(models.Song) (bool, string)
	Enabled() bool
}

// IsSuitable reports whether a song is good enough to commit as the dispatch's
// result without consulting further lanes. A result is suitable iff the script
// guard passes (a nil or disabled guard always passes) AND its quality is at
// least unsynced. An instrumental marker or an empty body is never suitable on
// its own: it is retained only as a best-available fallback by the orchestrator.
func IsSuitable(song models.Song, guard ScriptGuard) bool {
	// A detector-sourced instrumental (DetectorVersion set) is a terminal verdict:
	// it settles the track and short-circuits the remaining lanes. A provider
	// instrumental carries no DetectorVersion and stays best-available only.
	detectorInstrumental := song.Track.Instrumental == 1 && song.DetectorVersion != ""
	if !detectorInstrumental && QualityOf(song) < QualityUnsynced {
		return false
	}
	if guard != nil && guard.Enabled() {
		// The reason is BOUND and logged, never discarded (#773). This site cannot
		// persist it -- IsSuitable is a pure predicate scoring a candidate before
		// any row is settled, so there is no row to write to, and the lane that
		// ultimately settles records its own verdict via SettleGuardRejected.
		//
		// It is still worth emitting: previously this branch returned a bare false,
		// so a candidate dropped HERE left no trace anywhere at any log level. That
		// is the silent-failure shape -- a guard that computes a precise reason and
		// then says nothing. Debug rather than Warn because rejecting one candidate
		// is routine fall-through (the orchestrator advances to the next lane), not
		// an incident; the terminal rejection is what warrants Warn, and the worker
		// already logs that one.
		if ok, reason := guard.Accept(song); !ok {
			slog.Debug("lane candidate rejected by script guard; advancing",
				"reason", reason, "quality", QualityOf(song))
			return false
		}
	}
	return true
}

// judgeAgainst returns song with the audio duration the writer's accept-time
// timing guard (#439) will judge it against. The query track's TrackLength is
// that duration: the worker settles it from the audio file's own tags before
// dispatch and stamps the very same value onto Song.AudioDurationSeconds before
// the write. A zero leaves the writer's own fallback (the provider's catalog
// length) in charge, and a song with neither fails open -- identically on both
// sides, because both call lyrics.DecidePromotion.
func judgeAgainst(song models.Song, track models.Track) models.Song {
	song.AudioDurationSeconds = track.TrackLength
	return song
}

// timingDecision is the writer's own promotion decision for song (#950). The
// orchestrator never recomputes or copies a threshold: the predicate stays owned
// by internal/timing and the decision by lyrics.DecidePromotion, so what the
// orchestrator treats as usable is by construction what the writer would land.
func timingDecision(song models.Song, track models.Track) lyrics.PromotionDecision {
	decision, _, _ := lyrics.DecidePromotion(judgeAgainst(song, track))
	return decision
}

// candidate is what the dispatch does with one lane result (#950).
type candidate int

const (
	// candidateRetain: not suitable (script guard, below unsynced quality) or
	// timed to a different recording (quarantined). Kept only as the ranked
	// last resort; see retainQuality.
	candidateRetain candidate = iota
	// candidateCommit: suitable AND the timing guard promotes it as-is, so it
	// may end the dispatch.
	candidateCommit
	// candidateHold: suitable, but the timing guard would demote it to .txt
	// (MisSynced / degenerate). It does not end the dispatch, yet it outranks
	// every retained result: a script-guard-rejected one writes nothing.
	candidateHold
)

// classifyCandidate judges a lane result ONCE: the script guard runs exactly
// one time per result, as it did before #950, and the timing decision is the
// writer's own (lyrics.DecidePromotion), so no threshold lives here.
func classifyCandidate(song models.Song, track models.Track, guard ScriptGuard) candidate {
	if !IsSuitable(song, guard) {
		return candidateRetain
	}
	switch timingDecision(song, track) {
	case lyrics.PromoteAsIs:
		return candidateCommit
	case lyrics.DemoteToUnsynced:
		return candidateHold
	case lyrics.Quarantine:
	}
	return candidateRetain
}

// retainQuality ranks a non-committed result by what the writer would actually
// land, not by what the provider sent. A quarantined synced result writes
// nothing, so it ranks QualityNone and any later result (even a provider
// instrumental marker) outranks it; a demoted one lands as .txt, so it ranks
// QualityUnsynced.
func retainQuality(song models.Song, track models.Track) Quality {
	switch timingDecision(song, track) {
	case lyrics.Quarantine:
		return QualityNone
	case lyrics.DemoteToUnsynced:
		return QualityUnsynced
	case lyrics.PromoteAsIs:
	}
	return QualityOf(song)
}
