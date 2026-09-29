package lyrics

import (
	"errors"
	"log/slog"
	"os"
	"strings"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/sidecar"
)

// Rung is a lyric result's position on the #553 quality ladder. A higher value
// is strictly better, and RungWord is the only terminal rung: everything below
// it stays eligible for upgrade. On the five canonical shapes (nothing,
// instrumental flag alone, plain body, line-synced, word-synced) the order
// matches orchestrator.Quality's, and TestQualityAgreesWithRung pins exactly
// those. Mixed shapes are NOT pinned to agree: an instrumental flag beside
// subtitles or a body, and word timings HasQualifyingWords rejects, rank
// differently under the two (the test records the current values as known
// divergences, so a change to either is visible).
type Rung int

const (
	// RungNone means nothing usable (no sidecar on disk, or an empty result).
	RungNone Rung = iota
	// RungInstrumental is the instrumental marker .txt.
	RungInstrumental
	// RungUnsynced is plain lyric text (.txt, or a .lrc with no timestamps).
	RungUnsynced
	// RungLine is a line-synced .lrc.
	RungLine
	// RungWord is a .lrc where at least one line carries qualifying word timing
	// (inline A2 markers or an owned .elrc companion). The only terminal rung.
	RungWord
)

// ErrKeptBetter is returned by WriteLRC when the on-disk sidecar sits on a
// higher rung than the candidate and the write was not forced (#553). Nothing
// was written or removed. Callers must not record the candidate as what landed.
var ErrKeptBetter = errors.New("lyrics: kept a better sidecar already on disk")

// KeptError is the concrete error WriteLRC returns for a refused downgrade. It
// matches ErrKeptBetter under errors.Is and carries what the guard found on
// disk, so a caller settling the row can describe the file that is actually
// there rather than the candidate it refused.
type KeptError struct {
	// OnDisk is the kept sidecar's rung.
	OnDisk Rung
	// Synced is true when the kept sidecar is a .lrc (any extension case).
	Synced bool
	// Judged is false when the sidecar could not be read: its rung is then
	// the highest its extension allows, a keep-it guess rather than a reading.
	Judged bool
}

func (e *KeptError) Error() string { return ErrKeptBetter.Error() }

// Is makes errors.Is(err, ErrKeptBetter) hold for a *KeptError.
func (e *KeptError) Is(target error) bool { return target == ErrKeptBetter }

// RungOfSong ranks a candidate by its content, using WriteLRC's own content
// selection (the instrumental flag wins, then synced lines, then a plain body)
// and the writer's own word predicate (HasQualifyingWords). Whether the words
// can actually land depends on the writer's mode; see LRCWriter.candidateRung.
func RungOfSong(song models.Song) Rung {
	switch {
	case song.Track.Instrumental == 1:
		return RungInstrumental
	case len(song.Subtitles.Lines) > 0:
		if HasQualifyingWords(song) {
			return RungWord
		}
		return RungLine
	case song.Lyrics.LyricsBody != "":
		return RungUnsynced
	default:
		return RungNone
	}
}

// RungOnDisk classifies what is on disk for fp's stem, by inspecting the files
// themselves rather than any provenance header. A .lrc (any extension-case
// variant, #989) wins over a .txt, mirroring the scanner's settle order.
//
// A sidecar that exists but cannot be read is ranked as high as its extension
// allows (RungWord for a .lrc, RungUnsynced for a .txt): the guard's job is to
// never destroy a file it could not judge, so doubt keeps the file.
func RungOnDisk(fp string, l sidecar.Listing) Rung {
	return classifyOnDisk(fp, l).OnDisk
}

// classifyOnDisk is RungOnDisk plus the facts a kept completion stamps from
// (KeptError): whether the sidecar is a .lrc, and whether it was readable.
func classifyOnDisk(fp string, l sidecar.Listing) KeptError {
	stem := sidecar.StemOf(fp)
	if v := l.Variants(stem + sidecar.ExtLineSynced); len(v) > 0 {
		tier, err := ClassifyLRCFile(v[0])
		switch {
		case err != nil:
			slog.Warn("could not classify existing .lrc; keeping it", "path", v[0], "error", err)
			return KeptError{OnDisk: RungWord, Synced: true}
		case tier == TierWord:
			return KeptError{OnDisk: RungWord, Synced: true, Judged: true}
		case tier == TierLine:
			return KeptError{OnDisk: RungLine, Synced: true, Judged: true}
		default:
			return KeptError{OnDisk: RungUnsynced, Synced: true, Judged: true}
		}
	}
	if v := l.Variants(stem + sidecar.ExtUnsynced); len(v) > 0 {
		data, err := os.ReadFile(v[0]) //nolint:gosec // reason: path is the writer's own resolved sidecar target
		if err != nil {
			return KeptError{OnDisk: RungUnsynced}
		}
		if strings.Contains(string(data), InstrumentalMarker) {
			return KeptError{OnDisk: RungInstrumental, Judged: true}
		}
		return KeptError{OnDisk: RungUnsynced, Judged: true}
	}
	return KeptError{OnDisk: RungNone, Judged: true}
}
