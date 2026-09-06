package innertube

import (
	"fmt"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
)

// matchMinConfidence is the Jaro-Winkler floor a comparable field must reach
// before a candidate may be trusted to be the requested track.
//
// 0.75 is NOT a fresh number: it is the floor internal/musixmatch already uses
// for the same question (client.go, #840). Reusing it keeps one calibrated
// separation point across providers rather than inventing a second one here.
//
// Its calibration corpus, RE-MEASURED rather than inherited (853-R5F4 -- the
// numbers previously quoted here, 0.8051 legitimate and 0.6095 mismatch, came
// from the prose at client.go:791, which disagrees with that file's own table
// at client.go:749; the table is right):
//
//	Beatles     -> The Beatles                   0.7597  legitimate, WEAKEST
//	Artist One  -> Artist One feat. Artist Two   0.8741  legitimate
//	(unrelated artist pair)                      0.6338  mismatch, closest
//
// So the real margin is 0.7597 against a 0.75 floor -- 0.01, far tighter than
// the 0.8051 figure implied. A leading article is the case that nearly fails,
// which is worth knowing before anyone moves this number.
//
// The #848 innertube spike measured this provider against the same floor:
//
//	correct resolution   artist 1.000  title 1.000  -> accept
//	nonsense query       artist 0.577  title 0.481  -> reject
//
// Both nonsense fields sit below the closest mismatch, so the innertube
// evidence sits inside the separation the floor was calibrated on.
const matchMinConfidence = 0.75

// Ranking weights. Text dominates duration by construction: the text terms
// span [0, 2*textWeight] and the duration term is capped at
// durationExactBonus, so duration can only reorder candidates whose combined
// text confidence differs by less than durationExactBonus/textWeight = 0.1.
// Above the 0.75 floor there is only 0.25 of headroom per field, so that
// window is genuinely "the text cannot tell these apart".
//
// That ordering is deliberate and is the same rule petitlyrics' scoreCandidate
// states -- a stronger signal can never be outvoted by a weaker one -- applied
// to a different question. petitlyrics ranks "which RECORDING is this", where
// duration outranks text. Selection here ranks "which candidate corresponds to
// the track we ASKED FOR", where text IS the correspondence evidence and
// duration is not (see the durationCloseTolerance block). Letting duration
// outvote text would promote a duration-alike candidate over a
// correspondence-alike one, the gate would then reject the winner, and a good
// candidate sitting one rank below would be lost -- a false reject manufactured
// entirely by bad ranking.
const (
	textWeight          = 10.0
	durationCloseBonus  = 0.5
	durationFarBonus    = 0.25
	durationExactBonus  = 1.0
	durationExactWindow = 1
)

// durationCloseTolerance and durationFarTolerance bound the duration
// TIE-BREAK, in seconds. The values follow internal/petitlyrics' calibrated
// pair (3s of ordinary tagging/reporting drift; 8s as the outer edge of "still
// plausibly the same recording") rather than being chosen here, because this
// package has no corpus of its own to calibrate against.
//
// DURATION'S ROLE IS DECIDED EXPLICITLY (issue #853 AC): it RANKS, it never
// REJECTS. Three reasons, in order of weight:
//
//  1. Duration is not correspondence evidence. Any number of unrelated songs
//     share any given runtime, so a duration match cannot argue a candidate is
//     the requested track, and a duration mismatch cannot argue it is not --
//     a remaster, a video with an added outro, or a tagger rounding through a
//     container estimate all move it legitimately. A signal that is neither
//     necessary nor sufficient makes a poor gate.
//
//  2. A duration REJECT threshold here would be UNCALIBRATED. The #848 spike
//     measured text confidence against a control group; it measured no
//     duration-rejection threshold at all. Shipping an eyeballed one is the
//     exact failure mode this slice exists to avoid -- showingResultsForRenderer
//     also looked like a clean discriminator until a control group collapsed it.
//
//  3. The library ALREADY HAS a calibrated duration guard, downstream and
//     better positioned. internal/timing.Evaluate judges the accepted lyric's
//     own cue timings against the audio file's exact duration, on thresholds
//     calibrated over a 28.7k-track corpus, and internal/lyrics refuses to
//     promote such a result. A candidate whose runtime is grossly wrong
//     produces cues that overrun the audio and is caught there, by a predicate
//     that was measured. Duplicating that judgment here, uncalibrated and on
//     less information (a catalog runtime rather than the file's), would add
//     false rejects without removing a single false accept.
//
// So duration earns its keep where it is genuinely informative: ordering a list
// of candidates that have ALL already cleared the correspondence gate, which is
// the "which of these recordings" question it is actually good at. A zero
// duration on either side contributes nothing and never penalizes -- absence of
// a value is not evidence (SearchCandidate.DurationSeconds documents 0 as "not
// supplied, fails open").
const (
	durationCloseTolerance = 3
	durationFarTolerance   = 8
)

// SelectCandidate applies the correspondence guard to an innertube search
// response and returns the one candidate that corresponds to the requested
// track, or an error wrapping ErrNotFound.
//
// WHY THIS FUNCTION EXISTS. Innertube search HAS NO EMPTY STATE. A deliberately
// nonsensical artist/title returns a confident, fully-timed, wholly unrelated
// result: a valid videoId, a real lyrics tab, dozens of monotonic cues of some
// other song. There is no "not found" in the payload to detect, and no field in
// it that reliably says so -- showingResultsForRenderer, the one candidate
// signal, was MEASURED AGAINST A CONTROL GROUP and flagged only 1 of 4 nonsense
// queries while firing on 4 of 4 real tracks. Good specificity, unusable
// sensitivity; it is not used here and must not be built on. Correspondence has
// to be established by the caller, from the values innertube itself returned.
//
// THE ASYMMETRY THAT DECIDES EVERY JUDGMENT CALL BELOW. A false REJECT costs
// one missing lyric file: nothing is written, the queue row defers and retries.
// A false ACCEPT writes another song's words to a .lrc next to the user's audio,
// looks entirely correct, and (per the writer's format-transition rule) a later
// re-fetch can delete a better sidecar than it replaces. One is recoverable and
// cheap; the other silently corrupts the library. Every threshold, every
// fail-open and every fail-closed here is chosen against that.
//
// SHAPE: GATE EVERY CANDIDATE, THEN RANK THE SURVIVORS. Both halves are load
// bearing and they are different jobs. Ranking alone is how a wrong candidate
// wins by being the best of a bad set -- the nonsense response contains exactly
// one candidate, so it ranks first trivially. Gating alone cannot choose among
// several plausible candidates. Gating FIRST (rather than ranking first and
// gating the winner, as internal/musixmatch does for its single-result matcher
// endpoint) is the stricter arrangement for a LIST: it makes it impossible for a
// non-corresponding candidate to displace a corresponding one, so the returned
// winner has passed the gate by construction. The invariant is asserted again
// on the winner before return, so it is explicit rather than merely emergent.
//
// PLAY COUNT IS NOT USED, AND MUST NOT BE. Obscure but legitimate tracks carry
// low counts, so a popularity term rejects exactly the material a lyrics
// provider is most needed for -- the measured false-positive shape of #767,
// where a threshold tuned on mainstream material fired on obscure tracks.
// SearchCandidate does not carry a play count and should not gain one for this.
//
// The returned error carries NO field values. It reaches logs and the
// failure-analysis report, and the library's track metadata is private
// (matching internal/musixmatch's checkMatchCorresponds, whose error is
// deliberately value-free for the same reason).
func SelectCandidate(candidates []SearchCandidate, requested models.Track) (SearchCandidate, error) {
	if len(candidates) == 0 {
		return SearchCandidate{}, fmt.Errorf("innertube: search returned no candidates: %w", ErrNotFound)
	}

	best := SearchCandidate{}
	bestScore := -1.0
	found := false

	for _, c := range candidates {
		// A candidate with no videoId cannot be continued into a next() call,
		// so it is unusable regardless of how well it corresponds. This is a
		// usability check, not a correspondence one.
		if c.VideoID == "" {
			continue
		}
		if err := checkCorresponds(requested, c); err != nil {
			continue
		}
		if score := scoreCandidate(c, requested); !found || score > bestScore {
			best, bestScore, found = c, score, true
		}
	}

	if !found {
		return SearchCandidate{}, fmt.Errorf("innertube: no search candidate corresponds to the requested track: %w", ErrNotFound)
	}

	// Re-assert the gate on the winner. Redundant by construction above, and
	// kept deliberately: "the thing we return has cleared the floor" is the
	// whole promise of this package to the library, and a promise that is only
	// implied by the loop's structure is one a future refactor can quietly
	// drop.
	if err := checkCorresponds(requested, best); err != nil {
		return SearchCandidate{}, err
	}
	return best, nil
}

// checkCorresponds reports whether a candidate corresponds to the requested
// track, returning an error wrapping ErrNotFound when it does not.
//
// EVERY COMPARABLE FIELD MUST CLEAR THE FLOOR. This mirrors
// internal/musixmatch's checkMatchCorresponds, including the reasoning that
// replaced its own earlier permissive rule: requiring only ONE field to clear
// the floor lets two realistic wrong-track classes straight through -- a COVER
// holds the title at 1.0 while the artist drops to ~0.50, and a wrongly-served
// compilation track holds the artist at 1.0 while the title drops to ~0.61.
// Both are another song's words. Legitimate variance, measured, keeps BOTH
// fields high.
//
// A BLANK FIELD IS NOT COMPARABLE AND IS SKIPPED -- never counted as a pass.
// Absence of a value is not evidence of correspondence any more than it is
// evidence of mismatch. The consequence is the important part: when only one
// field is comparable, THAT FIELD ALONE must still clear the floor.
//
// WHEN NO FIELD IS COMPARABLE, THIS REJECTS. That is a deliberate divergence
// from musixmatch, which accepts in the same situation because its probe path
// legitimately issues blank-field lookups whose response has nothing to verify
// against.
//
// THE PREMISE FOR DIVERGING IS UNVERIFIED, PENDING THE CALLER -- flagged here
// rather than left to be inherited as settled. The reasoning is that a search
// query is BUILT from the requested artist and title, so an all-blank request
// could not have produced a meaningful query in the first place. That cannot be
// checked today: the query builder is a later slice and SelectCandidate has no
// non-test caller yet, so nothing in the tree establishes how a blank-field
// request reaches search, or whether one can at all.
//
// It is safe to ship in that state because the direction is CONSERVATIVE.
// Rejecting on zero correspondence evidence costs a lookup that was already
// meaningless; accepting would return an arbitrary result with nothing
// verifying it, which is the silent-corruption case this guard exists to stop.
// If the premise is wrong, the symptom is a false REJECT -- the recoverable
// side of the asymmetry the package doc names.
//
// WHOEVER LANDS THE QUERY BUILDER SHOULD RE-CHECK THIS: if a legitimate caller
// turns out to issue a blank-field lookup (a probe path of the kind musixmatch
// has), this branch is what will reject it, and the divergence should be
// revisited against that real caller rather than against this assumption.
func checkCorresponds(requested models.Track, c SearchCandidate) error {
	artistOK, artistComparable := fieldCorresponds(requested.ArtistName, c.Artist)
	titleOK, titleComparable := fieldCorresponds(requested.TrackName, c.Title)

	if !artistComparable && !titleComparable {
		return fmt.Errorf("innertube: no comparable field to verify the candidate against: %w", ErrNotFound)
	}
	if (!artistComparable || artistOK) && (!titleComparable || titleOK) {
		return nil
	}
	// No field values in the message: this reaches logs and the
	// failure-analysis report, and the library's metadata is private.
	return fmt.Errorf("innertube: search candidate does not correspond to the requested track: %w", ErrNotFound)
}

// fieldCorresponds reports whether a returned field resembles the requested
// one, and whether the pair was comparable at all (both non-blank after
// normalization).
//
// Comparability is decided on the NORMALIZED form rather than a raw
// strings.TrimSpace. THE TWO CASES THAT MOTIVATED THAT ARE NOT EQUALLY SAFE,
// and an earlier version of this comment claimed they were:
//
//   - WHITESPACE PADDING is the safe half, and it is the ONLY thing this buys
//     over strings.TrimSpace. Measured (853-R5F3): zero-width characters do
//     NOT normalize to empty -- NormalizeKey("\u200b\u200c\u200d") returns
//     them unchanged -- so the "zero-width" half of an earlier version of this
//     comment was simply false, and whitespace is handled identically by
//     TrimSpace anyway. The honest statement of the trade is therefore that
//     NormalizeKey buys NOTHING here except the combining-mark stripping
//     described next, which is the unsafe half. It is kept because the gate
//     must judge the same normalized form the cache keys on, not because
//     padding removal earns it.
//
//   - COMBINING MARKS ARE THE UNSAFE HALF, and the direction matters.
//     NormalizeKey strips combining marks (NFKD, then Mn removal), so it does
//     NOT merely discard padding here -- it discards CONTENT. A field written
//     entirely in combining marks normalizes to empty and is then treated as
//     absent, which SUPPRESSES a comparison rather than performing one. In a
//     script where the marks carry meaning this can convert a genuine mismatch
//     into a skipped field, and when it is the only other field, the remaining
//     field alone then decides. That is a real widening of what can be
//     accepted, not a neutral reclassification.
//
// WHY IT IS LEFT AS IS. The mark-stripping is normalize.NormalizeKey's own
// deliberate behavior, shared with the cache keys and every other provider's
// matching, so narrowing it HERE would put this gate out of step with the rest
// of the library for a case no measurement has yet observed. The residual
// exposure is bounded by the surrounding rules rather than by this function:
// the no-comparable-field branch still REJECTS when nothing is left to check,
// and any field that does survive normalization must clear the floor on its
// own. The behavior is therefore unchanged and the claim, not the code, is what
// was wrong.
func fieldCorresponds(requested, got string) (ok, comparable bool) {
	if normalize.NormalizeKey(requested) == "" || normalize.NormalizeKey(got) == "" {
		return false, false
	}
	return normalize.MatchConfidence(requested, got) >= matchMinConfidence, true
}

// scoreCandidate ranks one gate-passing candidate against the requested track.
// Higher is better. It is a RANKER, not a gate: it is only ever called on
// candidates that have already cleared checkCorresponds, and it can never
// reject.
//
// Text first, duration only as a tie-break -- see the textWeight and
// durationCloseTolerance blocks for why that ordering is the opposite of
// petitlyrics' and why it has to be.
func scoreCandidate(c SearchCandidate, requested models.Track) float64 {
	var score float64

	// Text similarity on each comparable field. A non-comparable field
	// contributes nothing rather than a penalty, so a candidate that supplies
	// less evidence simply cannot out-rank one that supplies more.
	if _, comparable := fieldCorresponds(requested.ArtistName, c.Artist); comparable {
		score += textWeight * normalize.MatchConfidence(requested.ArtistName, c.Artist)
	}
	if _, comparable := fieldCorresponds(requested.TrackName, c.Title); comparable {
		score += textWeight * normalize.MatchConfidence(requested.TrackName, c.Title)
	}

	// Duration tie-break. requested.TrackLength is seconds; a zero on either
	// side means "not supplied" and contributes nothing (fails open).
	if requested.TrackLength > 0 && c.DurationSeconds > 0 {
		delta := requested.TrackLength - c.DurationSeconds
		if delta < 0 {
			delta = -delta
		}
		switch {
		case delta <= durationExactWindow:
			score += durationExactBonus
		case delta <= durationCloseTolerance:
			score += durationCloseBonus
		case delta <= durationFarTolerance:
			score += durationFarBonus
		}
	}

	return score
}
