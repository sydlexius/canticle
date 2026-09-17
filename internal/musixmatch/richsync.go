package musixmatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/sydlexius/canticle/internal/models"
)

// ErrUnparsableRichSyncBody indicates track.richsync.get returned a
// richsync_body this client cannot decode into line entries at all.
//
// The richsync twin of ErrUnparsableSubtitleBody, with the same VISIBILITY
// purpose: a distinct, greppable name for "the payload shape changed", never
// conflated with "this track has no word data". Like its twin, the error text
// carries ONLY a byte count -- the body is the lyric content itself and must
// never reach work_queue.last_error or a log.
var ErrUnparsableRichSyncBody = errors.New("musixmatch: unrecognized richsync_body encoding")

// richSyncBindToleranceMS is how far a richsync line entry's start may sit from
// a subtitle cue and still be considered the same line.
//
// Calibrated, not chosen by feel. models.Time carries hundredths and
// lrcnormalize parses 1-3 fractional digits, so two representations of the SAME
// instant can differ by ~10 ms before any editorial difference. 300 ms is an
// order of magnitude above that -- enough to absorb genuine disagreement between
// two pipelines that timed the same track independently, far below the gap
// between adjacent sung lines, which is the only source of a FALSE bind.
// Widening it recovers no divergent line; it only admits the neighboring cue as
// a second candidate, which the ambiguity rule then drops anyway.
const richSyncBindToleranceMS = 300

// richSyncUnitSanityFactor bounds how far past the last subtitle cue a computed
// word start may land before the body is rejected.
//
// It catches a UNIT error and nothing else. ts/te/o are believed to be seconds
// and are scaled by 1000 here; if the provider sends milliseconds every stamp is
// 1000x too large and the failure is SILENT in the worst way -- nothing binds,
// nothing is emitted, and the track reads as "no richsync" forever.
//
// The denominator is the last CUE, not the catalog Track.TrackLength the design
// sketched: a pure parser holds the cues, and widening the signature for one
// check is the worse trade.
//
// The bound is the LARGER of a multiple and an additive slack, because each arm
// alone fails on a different real body:
//
//   - A pure MULTIPLE is meaninglessly tight when the cue span is tiny (a cue at
//     0.2s allows 0.8s), so ordinary trailing content trips it. That is what
//     forced an earlier factor up to 100 -- which then let a real 1000x through,
//     since a ratio only measures the error RELATIVE to how much of the track the
//     body covers, and a sparsely-covered track shrinks it below any fixed factor.
//   - A pure ABSOLUTE ceiling cannot work either: 15 seconds of content sent in
//     milliseconds reads as 4.2 hours, which no ceiling can reject without also
//     rejecting a legitimate long-form recording.
//
// Together they are tight where it matters: the slack absorbs the short-span case
// the multiple cannot, so the multiple stays at 4 and a 1000x error is caught at
// any track length and any coverage fraction. A tripwire, not a plausibility test.
const (
	richSyncUnitSanityFactor  = 4
	richSyncUnitSanitySlackMS = 60_000
)

// richSyncChunk is one timed chunk of a line: text, and offset from the line
// start, as the provider spells them.
//
// float64 is not a style preference. An int decode truncates every offset to a
// whole second, so every chunk in a line shares one stamp and a2Words'
// uniformStarts guard then CORRECTLY refuses the markers -- a feature that
// silently does nothing rather than failing loudly. float64 also reads an
// integer-valued JSON number correctly, so only the scaling depends on the unit
// being seconds.
type richSyncChunk struct {
	C string  `json:"c"`
	O float64 `json:"o"`
}

// richSyncEntry is one line of a richsync body. Field names follow the wire
// spelling so the mapping to a captured payload stays legible.
type richSyncEntry struct {
	TS float64         `json:"ts"`
	TE float64         `json:"te"`
	X  string          `json:"x"`
	L  []richSyncChunk `json:"l"`
}

// parseRichSyncBody decodes a richsync_body into word timings correlated against
// the subtitle cues the same lookup produced. Pure: no I/O, no receiver.
//
// The body is a JSON-encoded STRING whose contents are themselves JSON, so the
// decode is two steps; either failure returns ErrUnparsableRichSyncBody.
//
// An EMPTY decode is a failure, not a success: json.Unmarshal accepts "null"
// without error and "[]" yields an empty slice, and parseSubtitleBody records
// what handing a caller a clean nil error with nothing in it costs. Zero BINDS
// is a different thing and deliberately NOT an error -- the body decoded, its
// lines simply do not correspond to these cues.
//
// cues is read, never mutated and never sorted in place: it is the caller's
// song.Subtitles.Lines, whose order IS the writer's emission order.
func parseRichSyncBody(raw []byte, cues []models.Lines) ([]models.WordTiming, error) {
	var inner string
	if err := json.Unmarshal(raw, &inner); err != nil {
		return nil, fmt.Errorf("%w (%d bytes)", ErrUnparsableRichSyncBody, len(raw))
	}

	var entries []richSyncEntry
	if err := json.Unmarshal([]byte(inner), &entries); err != nil {
		return nil, fmt.Errorf("%w (%d bytes)", ErrUnparsableRichSyncBody, len(raw))
	}

	// COUNT CHUNKS, not entries. Every field is optional to encoding/json, so a
	// body whose keys were renamed upstream decodes into N entries of {TS:0,
	// L:nil} without error -- an entry count sees N and calls it a success,
	// returning zero word timings and no error. That is the SILENT payload
	// change this sentinel exists to name, so the emptiness test has to run over
	// the data actually being read. A body carrying no words at all is
	// indistinguishable from one this parser cannot read, and both are failures.
	words := 0
	for _, e := range entries {
		words += len(e.L)
	}
	if words == 0 {
		return nil, fmt.Errorf("%w (%d bytes)", ErrUnparsableRichSyncBody, len(raw))
	}

	if err := checkRichSyncUnits(entries, cues, len(raw)); err != nil {
		return nil, err
	}
	return correlateRichSync(entries, cues), nil
}

// checkRichSyncUnits rejects a body whose word starts imply a track far longer
// than the cues do (see richSyncUnitSanityFactor).
//
// It reads every DECODED entry, not what correlation emits: a 1000x error binds
// nothing, so a check on the output would see an empty slice and pass. With no
// cues there is no bound, so it is skipped rather than guessed at.
func checkRichSyncUnits(entries []richSyncEntry, cues []models.Lines, rawLen int) error {
	lastCueMS := 0
	for _, c := range cues {
		if ms := toMS(c.Time.Total); ms > lastCueMS {
			lastCueMS = ms
		}
	}
	if lastCueMS <= 0 {
		return nil
	}

	maxStartMS := 0
	for _, e := range entries {
		for _, ch := range e.L {
			if ms := toMS(e.TS + ch.O); ms > maxStartMS {
				maxStartMS = ms
			}
		}
	}
	allowedMS := max(lastCueMS*richSyncUnitSanityFactor, lastCueMS+richSyncUnitSanitySlackMS)
	if maxStartMS > allowedMS {
		return fmt.Errorf("%w: a word starts at %d ms but the cues end at %d ms, which is what a"+
			" seconds/milliseconds unit error looks like (%d bytes)",
			ErrUnparsableRichSyncBody, maxStartMS, lastCueMS, rawLen)
	}
	return nil
}

// cueRef is one cue reduced to what correlation needs: its instant, and its
// position in the CALLER's slice, which is the index a WordTiming carries.
type cueRef struct {
	ms  int
	idx int
}

// correlateRichSync binds each line entry to at most one cue BY TIMESTAMP and
// emits one WordTiming per chunk of every bound entry.
//
// Pairing by slice index is forbidden, and not as a matter of taste: it is issue
// #489, an open bug here, where writeSyncedLRC pairs a bilingual translation to
// its original by index and silently misaligns the moment the cue counts
// diverge. These two payloads are at least as free to diverge -- different
// endpoints, different pipelines, and parseSubtitleBody's LRC branch runs
// lrcnormalize.ParseBody, which EXPANDS a compressed multi-timestamp line into
// one cue per timestamp, something richsync has no reason to have done.
//
// Why a downstream guard cannot cover this: an index-paired implementation
// attaches one line's timings to another line's text, a2Words'
// wordsReconstructLine guard catches MOST of it and quietly falls back to a
// plain cue, and the feature then appears to work partially while degrading
// invisibly.
//
// The rule, entries in ascending ts:
//
//	candidates = UNBOUND cues within richSyncBindToleranceMS of the entry start
//	exactly one -> bind;  zero -> drop;  several -> bind only if exactly one of
//	them matches the entry's line text ignoring whitespace, else drop
//
// Ambiguity refuses rather than guessing, mirroring checkMatchCorresponds'
// asymmetry: a false reject costs only one line's word markers, since the plain
// cue still writes, while a false bind stamps one line's timing onto another's
// words.
func correlateRichSync(entries []richSyncEntry, cues []models.Lines) []models.WordTiming {
	// Local sorted index: the scan wants time order and the caller's slice must
	// keep its own. parseSubtitleBody's JSON branch does not sort, so time order
	// cannot be assumed.
	refs := make([]cueRef, 0, len(cues))
	for i, c := range cues {
		refs = append(refs, cueRef{ms: toMS(c.Time.Total), idx: i})
	}
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].ms < refs[j].ms })

	ordered := make([]richSyncEntry, len(entries))
	copy(ordered, entries)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].TS < ordered[j].TS })

	bound := make(map[int]bool, len(cues))
	var out []models.WordTiming
	attempted := 0

	for _, e := range ordered {
		if len(e.L) == 0 {
			// Nothing to emit, so binding would only make a cue ineligible for a
			// later entry that does carry chunks.
			continue
		}
		attempted++
		idx, ok := bindEntry(e, refs, cues, bound)
		if !ok {
			continue
		}
		bound[idx] = true
		out = append(out, chunkTimings(e, idx)...)
	}

	// Info, not Debug: a low bind rate silently costs the result a full quality
	// tier and should be rare; if it becomes common that signals a payload-shape
	// change worth noticing in production. Counts only -- a richsync line IS the
	// lyric, so no text, title or artist may appear here.
	if attempted > 0 && len(bound)*2 < attempted {
		slog.Info("musixmatch: most richsync lines did not correspond to a subtitle cue; word timings are partial",
			"entries", attempted, "bound", len(bound))
	}
	return out
}

// bindEntry returns the caller-slice index of the one cue this entry
// corresponds to, or ok=false when there is no unambiguous answer. A cue already
// taken is filtered out BEFORE the count is judged, so it cannot make an
// otherwise clean bind look ambiguous.
func bindEntry(e richSyncEntry, refs []cueRef, cues []models.Lines, bound map[int]bool) (int, bool) {
	startMS := toMS(e.TS)

	var candidates []int
	for _, r := range refs {
		if bound[r.idx] {
			continue
		}
		if diff := r.ms - startMS; diff >= -richSyncBindToleranceMS && diff <= richSyncBindToleranceMS {
			candidates = append(candidates, r.idx)
		}
	}

	switch len(candidates) {
	case 0:
		return 0, false
	case 1:
		// One candidate is NOT a free pass. Entries bind in ascending ts and the
		// first bind wins, so a slightly-early entry reaching a cue inside the
		// window takes it before the entry that matches that cue EXACTLY is ever
		// considered -- one candidate at the time of asking, two once the whole
		// body is in view. That writes one line's words onto another line's text,
		// which is the wrong-content failure this rule exists to refuse, and
		// a2Words' fidelity guard only masks it when the texts differ a lot.
		//
		// So a lone candidate must still not CONTRADICT the text. Disagreement is
		// only decidable when both sides carry one: richsync's x is optional, and
		// an absent text is no evidence either way, so an empty on either side
		// keeps the timestamp's verdict rather than voting against it.
		idx := candidates[0]
		if !textsConflict(cues[idx].Text, e.X) {
			return idx, true
		}
		return 0, false
	}

	// Several cues in the window. Fall back to the line text, the one other thing
	// both payloads carry per line. Whitespace is ignored because each pipeline
	// joined its own chunks; case is not, because a case difference between two
	// renderings of the same performance is a real editorial difference.
	var matched []int
	for _, idx := range candidates {
		if stripSpaceRunes(cues[idx].Text) == stripSpaceRunes(e.X) {
			matched = append(matched, idx)
		}
	}
	if len(matched) == 1 {
		return matched[0], true
	}
	return 0, false
}

// textsConflict reports whether two per-line texts POSITIVELY disagree.
//
// Absence is not disagreement: richsync's x is optional, and an empty on either
// side is no evidence, so only two present-and-different texts conflict.
// Whitespace is ignored because each pipeline joined its own chunks; case is
// not, because a case difference between two renderings of one performance is a
// real editorial difference rather than a formatting artifact.
func textsConflict(cueText, entryText string) bool {
	a, b := stripSpaceRunes(cueText), stripSpaceRunes(entryText)
	return a != "" && b != "" && a != b
}

// chunkTimings converts a bound entry's chunks into WordTimings on line.
//
// Text passes through VERBATIM: richsync emits spacing as its own chunks, and
// trimming would still satisfy a2Words' whitespace-insensitive fidelity guard
// while silently removing the spaces from what a player renders.
//
// EndMS is derived, since a chunk carries only a start: the next chunk's
// absolute start, and the entry's te for the last one, giving a gapless span.
// a2Words does not read EndMS today, but a zero there reads as "this word has no
// duration" rather than "nobody knew". Both stamps clamp non-negative, matching
// petitlyrics/decode.go and what models.WordTiming requires of producers.
func chunkTimings(e richSyncEntry, line int) []models.WordTiming {
	out := make([]models.WordTiming, 0, len(e.L))
	for i, ch := range e.L {
		endSec := e.TE
		if i+1 < len(e.L) {
			endSec = e.TS + e.L[i+1].O
		}
		// EndMS floors at StartMS, never merely at zero. Two provider shapes
		// invert the span otherwise: a last chunk whose offset runs past the
		// entry's te, and chunks not ascending by o (the next chunk's start is
		// read as this one's end without assuming that order). A negative-length
		// word is not a value any consumer should have to defend against, and it
		// is latent only because a2Words does not read EndMS yet.
		start := max(toMS(e.TS+ch.O), 0)
		out = append(out, models.WordTiming{
			Line:    line,
			Text:    ch.C,
			StartMS: start,
			EndMS:   max(toMS(endSec), start),
		})
	}
	return out
}

// toMS converts a provider timestamp to integer milliseconds. The scaling is the
// ONE place the seconds assumption is load-bearing; see richSyncChunk.
func toMS(seconds float64) int {
	return int(math.Round(seconds * 1000))
}

// stripSpaceRunes removes every Unicode space, so two spellings of the same
// content compare equal. Mirrors internal/lyrics/a2.go's stripSpace.
func stripSpaceRunes(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}
