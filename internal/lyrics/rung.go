package lyrics

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
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
// themselves rather than any provenance header. It judges EXACTLY what a write
// to fp would remove and nothing else: the exact target fp (overwritten), every
// extension-case variant (#989) of the opposite sidecar (staleSidecars), and
// every owned word-synced companion (ownedCompanions). The highest rung wins.
// The set is taken from the same functions the writer's mutations consume, so
// the judged set and the removed set cannot drift. A same-extension case
// variant of the target ("song.LRC" when writing "song.lrc") survives the
// write, so it is never judged and can never block it -- except as the owner
// of a companion the write removes (see companionLift). On a tie a .lrc wins
// over a .txt, mirroring the scanner's settle order.
//
// A sidecar that exists but cannot be judged is ranked as high as its
// extension allows (RungWord for a .lrc, RungUnsynced for a .txt): the guard's
// job is to never destroy a file it could not judge, so doubt keeps the file.
// That covers an unreadable file and any non-regular entry (a symlink, FIFO or
// device at the exact name, which Listing.Variants deliberately reports): such
// an entry is never read, so the guard neither follows a link out of the
// library nor blocks reading a FIFO. See readRegularNoFollow for how a swap
// between the check and the read is closed.
func RungOnDisk(fp string, l sidecar.Listing) Rung {
	return classifyOnDisk(fp, l, ownedCompanions(fp, l)).OnDisk
}

// classifyOnDisk is RungOnDisk plus the facts a kept completion stamps from
// (KeptError): whether the kept sidecar is a .lrc, and whether it was judged.
// companionRemoves is the write's own companion plan (planCompanion's
// removes, which is ownedCompanions), passed in rather than re-derived.
func classifyOnDisk(fp string, l sidecar.Listing, companionRemoves []string) KeptError {
	var lrcs, txts []string
	if fi, err := os.Lstat(fp); err == nil && !fi.IsDir() {
		// The exact target is overwritten; same presence rule as Listing.Variants.
		lrcs, txts = appendByKind(lrcs, txts, fp)
	}
	for _, v := range staleSidecars(fp, l) {
		lrcs, txts = appendByKind(lrcs, txts, v)
	}
	best := KeptError{OnDisk: RungNone, Judged: true}
	consider := func(k KeptError) {
		// Strictly greater: on a tie the earlier (.lrc, exact case) wins.
		if k.OnDisk > best.OnDisk {
			best = k
		}
	}
	for _, v := range lrcs {
		consider(classifySidecar(v, true))
	}
	for _, v := range txts {
		consider(classifySidecar(v, false))
	}
	if len(companionRemoves) > 0 {
		consider(companionLift(fp, l, lrcs))
	}
	return best
}

func appendByKind(lrcs, txts []string, path string) ([]string, []string) {
	if sidecar.KindOf(path) == sidecar.KindLineSynced {
		return append(lrcs, path), txts
	}
	return lrcs, append(txts, path)
}

// companionLift judges the owned companions a write removes. A companion
// carries no rung of its own: it lifts a line-synced .lrc of its stem to
// RungWord (ClassifyLRCFile's rule), so removing it downgrades exactly those
// .lrc files. The .lrc variants already judged are skipped (their verdict
// already includes the companion); any other variant with line-tier cues
// ("song.LRC" beside a write to "song.lrc") loses its word timing to the
// removal and ranks RungWord. An orphaned companion, with no .lrc beside it,
// lifts nothing, as before. An unreadable .lrc keeps RungWord (doubt keeps).
func companionLift(fp string, l sidecar.Listing, judged []string) KeptError {
	best := KeptError{OnDisk: RungNone, Judged: true}
	for _, v := range l.Variants(sidecar.StemOf(fp) + sidecar.ExtLineSynced) {
		if slices.Contains(judged, v) {
			continue
		}
		data, err := readRegularNoFollow(v)
		if err != nil {
			slog.Debug("could not inspect a companion's .lrc; keeping the companion", "path", v, "error", err)
			return KeptError{OnDisk: RungWord, Synced: true}
		}
		if ClassifySynced(parseSyncedLRC(data)) == TierLine {
			best = KeptError{OnDisk: RungWord, Synced: true, Judged: true}
		}
	}
	return best
}

// classifySidecar judges one on-disk sidecar. Only a regular file is read, and
// only through readRegularNoFollow's handle.
func classifySidecar(path string, synced bool) KeptError {
	keep := KeptError{OnDisk: RungUnsynced, Synced: synced} // unjudged: as high as the extension allows
	if synced {
		keep.OnDisk = RungWord
	}
	data, err := readRegularNoFollow(path)
	if err != nil {
		slog.Debug("could not inspect existing sidecar; keeping it", "path", path, "error", err)
		return keep
	}
	if synced {
		tier, err := classifyLRCContent(path, parseSyncedLRC(data))
		switch {
		case err != nil:
			slog.Debug("could not classify existing .lrc; keeping it", "path", path, "error", err)
			return keep
		case tier == TierWord:
			return KeptError{OnDisk: RungWord, Synced: true, Judged: true}
		case tier == TierLine:
			return KeptError{OnDisk: RungLine, Synced: true, Judged: true}
		default:
			return KeptError{OnDisk: RungUnsynced, Synced: true, Judged: true}
		}
	}
	if strings.Contains(string(data), InstrumentalMarker) {
		return KeptError{OnDisk: RungInstrumental, Judged: true}
	}
	return KeptError{OnDisk: RungUnsynced, Judged: true}
}

// errNotRegular is readRegularNoFollow's refusal of a non-regular entry.
var errNotRegular = errors.New("not a regular file")

// readRegularNoFollow reads path only if it is a regular file, judged on the
// HANDLE it reads from. The Lstat pre-filter keeps the common non-regular
// cases (symlink, FIFO, device) from ever being opened; the open itself is
// no-follow and non-blocking where the platform has those flags
// (openNoFollow), and the fstat of the opened handle must report a regular
// file, so an entry swapped in between the Lstat and the open is refused
// rather than read. Any error means "could not judge": callers keep the file.
func readRegularNoFollow(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errNotRegular
	}
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	hfi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !hfi.Mode().IsRegular() || !os.SameFile(fi, hfi) {
		return nil, errNotRegular
	}
	return io.ReadAll(f)
}
