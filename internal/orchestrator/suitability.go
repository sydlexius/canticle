package orchestrator

import "github.com/sydlexius/canticle/internal/models"

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
	// QualitySynced means the result carries time-synced subtitle lines.
	QualitySynced
)

// QualityOf classifies a song's lyric quality. The precedence mirrors the LRC
// writer's content selection (synced lines, then an unsynced body, then an
// instrumental marker), so the orchestrator's ranking agrees with what the
// writer will actually emit.
func QualityOf(song models.Song) Quality {
	switch {
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
	if QualityOf(song) < QualityUnsynced {
		return false
	}
	if guard != nil && guard.Enabled() {
		if ok, _ := guard.Accept(song); !ok {
			return false
		}
	}
	return true
}
