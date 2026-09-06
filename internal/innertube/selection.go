package innertube

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

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
		// TIES BREAK ON VideoID, not on slice position (853-R5F1). Ties are
		// the COMMON case, not a corner: duplicate uploads of one track (an
		// official video, a topic channel, a re-upload) carry identical
		// artist, title and duration, so they score identically. Taking the
		// first such candidate makes the winner depend on the order innertube
		// happened to return them, which is arbitrary and can differ between
		// two identical searches -- the same set would select a different
		// lyric. A total order on an ID the server supplies is not a better
		// CHOICE among duplicates (nothing here can tell which upload is
		// canonical) but it is a DETERMINISTIC one, which is what a cache key
		// and a reproducible fetch need.
		score := scoreCandidate(c, requested)
		switch {
		case !found, score > bestScore:
			best, bestScore, found = c, score, true
		case score == bestScore && c.VideoID < best.VideoID:
			best = c
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
	artistOK, artistComparable := artistFieldCorresponds(requested.ArtistName, c.Artist)
	titleOK, titleComparable := titleFieldCorresponds(requested.TrackName, c.Title)

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

// TOKEN CORRESPONDENCE: THE SECOND HALF OF THE TITLE GATE.
//
// The Jaro-Winkler floor alone has a measured false-accept class it cannot
// close, because the class is not a scoring problem. Holding the artist
// identical and varying only the title:
//
//	requested "Placeholder Song Title" vs "Placeholder Song Titles"     -> ~0.99
//	requested "Placeholder Song Title" vs "Placeholder Song Title Pt.2" -> ~0.98
//
// Those are DIFFERENT SONGS with different words, and both sit above any floor
// that still admits the weakest legitimate shape (a leading article, 0.8426).
// No floor value separates them; the separation is not on the similarity axis
// at all. Edit distance is small precisely because the difference is small --
// one plural, one part number -- and that is exactly the difference that makes
// it another song.
//
// WHY THIS CLASS IS WORSE THAN THE OTHERS, and why it earns a structural check
// rather than a tuning pass. Reason 3 in the durationCloseTolerance block
// leans on internal/timing.Evaluate to catch a grossly wrong runtime
// downstream. That net exists and works -- for a candidate whose runtime is
// wrong. A sibling track from the same release runs about as long, so its cues
// never overrun, timing.Evaluate returns Ok, and internal/lyrics promotes
// another song's words to a .lrc beside the user's audio. There is NO net below
// this gate for this class, which is what makes it the silent-corruption case
// the package doc names.
//
// Ranking does not rescue it either: when the correct upload and the sibling
// are both present the correct one wins, but search returning ONLY the
// near-neighbor is the ordinary case for obscure material -- which is the
// material a lyrics provider is most needed for.
//
// THE RULE. A candidate title corresponds only if, alongside clearing the
// floor, its normalized token multiset differs from the requested title's only
// by tokens drawn from a known VARIANT vocabulary. An unmatched CONTENT token
// REJECTS. This is deterministic and needs no calibration: it asks a different
// question from the floor ("are these the same words, modulo release
// packaging") rather than a stricter version of the same one.
//
// It preserves every legitimate variant class, which is the point -- most of
// what this gate accepts is CORRECT and must stay accepted. A live version, a
// remaster, a radio edit, an acoustic take, a deluxe reissue, a karaoke or
// instrumental cut all carry the same words and the same timing; rejecting
// them would trade a silent-corruption bug for a feature that never returns a
// lyric.
//
// TITLE ONLY. Artist is NOT subject to this rule -- see artistFieldCorresponds,
// which needs the opposite treatment. Conflating the two reintroduces one
// defect or the other.
//
// WHAT THIS DELIBERATELY DOES NOT HANDLE (the vocabulary is itself an attack
// surface, so the residue is written down rather than implied):
//
//   - A VARIANT WORD USED AS CONTENT. A song genuinely titled with one of these
//     words is compared on a vocabulary that treats that word as noise, so
//     "Live" vs "Karaoke" would differ only by variant tokens. Mitigated, not
//     eliminated: an accept requires at least one shared CONTENT token, so two
//     titles made ENTIRELY of variant vocabulary can never correspond by this
//     path (they still may by exact token identity, which is not a false
//     accept). A title with one content word plus a variant-word content word
//     remains reachable, and is accepted as the residual cost of admitting the
//     legitimate classes above.
//   - UPLOAD DECORATION IS EXCLUDED FROM THE VOCABULARY WHOLESALE, and the
//     exclusion is LOAD BEARING rather than an unfinished list. "video",
//     "audio", "hd", "official", "lyrics"/"lyric" and "visualizer" are all
//     genuine YouTube upload decoration and NONE of them is vocabulary, so
//     "(Official Video)", "(Official Audio)", "[HD]" and "(Visualizer)" all
//     REJECT. The reason is the sibling class this whole rule exists to close:
//     admitting "video" makes a title correspond to that title with "Video"
//     prepended, which is a DIFFERENT SONG by exactly the measured defect.
//     There is no rule available here that tells a decoration "video" apart
//     from a content "video", so the family is excluded uniformly rather than
//     token by token -- a partial list is the trap, because the next reader
//     reads four members, infers the missing ones were an oversight, and
//     "completes" it. TestUploadDecorationStaysRejected pins this so that
//     completing the list breaks a test rather than a library.
//     The line drawn is RELEASE packaging (how the RECORDING is issued --
//     remaster, deluxe, radio edit) stays; UPLOAD/PLATFORM decoration (how the
//     video is presented) does not. The cost is a false REJECT on decorated
//     uploads, which is the recoverable direction.
//   - "WITH" IS NOT A FEATURING MARKER here, though it often reads as one:
//     truncating at it would collapse a title to its first words and make two
//     different songs sharing a prefix correspond. Only feat/ft/featuring
//     truncate.
//   - DIRECTION IS NOT DISTINGUISHED. A variant token is tolerated whether it
//     appears in the requested title or the candidate's. Asking for a remaster
//     and being handed the plain cut (or the reverse) is the same words either
//     way, so the symmetry is intended rather than an oversight.
//   - A NAMED VENUE OR A CREDITED REMIXER REJECTS, and only the BARE decoration
//     accepts. "(Live)", "(Remix)" and "(2011 Remaster)" carry nothing but
//     vocabulary, so they pass; "(Live at the Venue)", "(Live in the City)" and
//     a remix crediting the remixer each contribute the venue or remixer word
//     as an unmatched CONTENT token and reject. This is left as is on the same
//     reasoning that excludes "video" and "audio" below: a venue or a person's
//     name is indistinguishable from a real content word by any rule available
//     here, so admitting it means admitting every content token that looks like
//     one. A credited remix is also frequently a genuinely different
//     arrangement rather than the same words. The cost is a false REJECT, the
//     recoverable direction.
//   - TWO DIFFERENT PERFORMANCES OF THE SAME SONG CORRESPOND -- "(Live)" vs
//     "(Acoustic)", and likewise "(Live)"/"(Demo)" or "(Acoustic)"/"(Karaoke)".
//     DECIDED, NOT MISSED, and it reverses both axes of the sibling-title class
//     this rule exists to close. That class is a DIFFERENT SONG with different
//     words at a near-identical runtime, so timing.Evaluate is blind to it and
//     there is no net below this gate. A performance variant is the SAME WORDS,
//     so an accept is not corruption, and the runtimes genuinely DIFFER, so the
//     net that missed the sibling catches this one: a live take judged against
//     studio audio computes MisSynced and is demoted to .txt with the words
//     kept, an extended remix computes Categorical and is quarantined. The
//     residue is two takes within timing's own tolerance of each other -- the
//     right words with sub-tolerance drift, which is exactly what the already
//     accepted remaster and radio-edit classes tolerate. Closing it would need
//     a SECOND vocabulary partitioning performance tokens from packaging ones,
//     doubling the surface that is itself the risk here, and it would start
//     rejecting "asked for the live cut, got the plain upload" -- same words,
//     accepted by design per DIRECTION IS NOT DISTINGUISHED above.
//   - Case, punctuation, diacritics and "&"/"and" are erased by tokenization,
//     so they never reach the vocabulary. APOSTROPHES ARE ERASED TOO, but by an
//     explicit replacer rather than by the split -- see apostropheEraser, and
//     do not assume any other punctuation JOINS rather than SEPARATES.

// titleVariantTokens is the RELEASE-packaging vocabulary: tokens that may
// differ between two titles naming the SAME words. Membership criterion, so
// additions stay principled: a token belongs here only if it names how the
// RECORDING was ISSUED AND is rare as a standalone content word.
//
// TWO EXCLUSIONS ARE DELIBERATE AND BOTH ARE LOAD BEARING. "part"/"pt" are
// absent because a part number names a different song, which is the measured
// defect. UPLOAD DECORATION ("video", "audio", "hd", "official",
// "lyrics"/"lyric", "visualizer") is absent as a family -- see the block above
// and TestUploadDecorationStaysRejected. Neither gap is an oversight; do not
// "complete" either one.
//
// DROPPING "official", "lyrics", "lyric" and "visualizer" (82b04e6) WAS A
// BEHAVIOR CHANGE, not merely a pin of the stated policy to the data. A title
// that genuinely uses one of those four words as CONTENT on one side now
// REJECTS where it previously accepted -- measured: an otherwise-identical
// pair still accepts (0.9062) when the varying word stays in the vocabulary,
// while the same shape with a dropped word measures 0.7814 to 0.8992 and
// rejects. That is an ACCEPTED COST, not an oversight: keeping those four
// tokens would have reopened the upload-decoration sibling class this
// exclusion exists to close, and a false reject is the recoverable direction.
// Noted here so a reader who sees "pin" does not assume nothing moved.
var titleVariantTokens = map[string]struct{}{
	"live": {}, "unplugged": {}, "acoustic": {}, "demo": {},
	"remaster": {}, "remastered": {}, "remasters": {}, "reissue": {},
	"remix": {}, "remixed": {}, "mix": {}, "edit": {}, "radio": {},
	"instrumental": {}, "karaoke": {}, "mono": {}, "stereo": {},
	"deluxe": {}, "bonus": {}, "extended": {}, "anniversary": {},
	"edition": {}, "version": {}, "original": {}, "session": {}, "sessions": {},
	"take": {}, "explicit": {}, "clean": {},
	// Produced by the phrase collapse in tokenizeTitle, never by a bare word:
	// "up" and "down" are far too common as content to admit on their own.
	"spedup": {}, "sloweddown": {},
}

// ignorableTokens are dropped from BOTH fields before comparison. Articles and
// the conjunction move around legitimately (a leading vs trailing article, an
// "&" written out) without changing which song or which act is named.
var ignorableTokens = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "and": {},
}

// featMarkers introduce a featured-credit suffix. In a TITLE they truncate:
// everything from the marker onward is a credit, not the song's words. In an
// ARTIST they are merely ignorable, because the names after them are part of
// the act being compared (see artistFieldCorresponds).
var featMarkers = map[string]struct{}{
	"feat": {}, "feats": {}, "ft": {}, "featuring": {},
}

// apostrophes are ERASED before splitting rather than split on. This is the
// one punctuation class that must not be a token BOUNDARY: a contraction or a
// possessive tokenizes to fragments if it is, so a title differing from a
// candidate only by a dropped apostrophe compares as three tokens against one
// and no fragment is in the vocabulary, rejecting in both directions. A local
// tag differing from an upload title by a dropped apostrophe is one of the most
// common real-world differences there is, and the similarity floor handles it
// correctly on its own (measured ~0.98), so the token rule must not undo that.
//
// THIS LIST IS NOT AN AUDIT OF THE APOSTROPHE FAMILY, and that distinction
// matters more than any one entry in it. Below are the forms that were
// actually MEASURED to reproduce the boundary bug and got an entry as a
// result -- not a sweep of Unicode apostrophe-like codepoints, and not a claim
// that every such codepoint behaves one of two ways. A THIRD behavior exists
// and this replacer does nothing for it: U+02BB (MODIFIER LETTER TURNED
// COMMA) and U+A78C (LATIN SMALL LETTER SALTILLO) are Unicode LETTERS, so
// FieldsFunc never treats them as a boundary in the first place -- they stay
// EMBEDDED inside the token whether or not they appear here, the same way
// U+02BC below does. A future apostrophe-shaped codepoint needs to be
// MEASURED, not assumed into either bucket: check its Unicode category and
// whether NormalizeKey disposes of it before deciding it needs an entry here
// at all.
//
// EVERY FORM LISTED HERE WAS MEASURED THROUGH NormalizeKey FIRST, because this
// replacer runs on the normalized string and a form NormalizeKey already
// disposes of could never reach it. Measured: the straight apostrophe, U+2018,
// U+2019, U+02BC, U+2032 (PRIME) and the backtick all SURVIVE NormalizeKey
// unchanged (they are punctuation or a modifier letter, not combining marks),
// so each one really would be a FieldsFunc boundary and each entry below is
// load bearing. U+055A (ARMENIAN APOSTROPHE) was measured to survive
// NormalizeKey the same way and would be a boundary too, but it is NOT added
// here -- U+2032 is the one of the measured set plausible enough in a real
// upload title to earn the entry; an unbounded codepoint list is the same
// overclaim this comment exists to retire, just written as code instead of
// prose.
//
// U+00B4 ACUTE ACCENT IS DELIBERATELY ABSENT and that absence is a measurement,
// not an omission: NFKD decomposes it to a space plus a combining acute, the
// mark is stripped, and it reaches this replacer as a SPACE. It is already a
// word boundary before any of this runs, and adding it here would be dead code
// that reads as coverage. The same is true of any other form NFKD decomposes to
// a space -- measure before adding one.
var apostropheEraser = strings.NewReplacer(
	"'", "",
	"\u2019", "", // right single quotation mark
	"\u2018", "", // left single quotation mark
	"\u02bc", "", // modifier letter apostrophe
	"\u2032", "", // prime
	"`", "",
)

// splitTokens lowercases, strips diacritics, ERASES apostrophes, and splits on
// everything else that is not a letter or digit, so remaining punctuation, "&"
// and whitespace runs all vanish rather than becoming tokens.
//
// The two treatments are different on purpose and the difference is the whole
// point: an erased rune JOINS what surrounds it into one token, a split rune
// SEPARATES it into two. Only the apostrophe wants the former -- see
// apostropheEraser.
func splitTokens(s string) []string {
	normalized := apostropheEraser.Replace(normalize.NormalizeKey(s))
	return strings.FieldsFunc(normalized, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// isYearToken reports whether a token is a four-digit year in [1900, 2099].
//
// The window is deliberate: any OTHER number is CONTENT, which is what keeps a
// part number a rejecting difference. The bounds are load bearing and measured
// -- "Placeholder 1899" vs "Placeholder 1900" and "Placeholder 2099" vs
// "Placeholder 2100" both reject at 0.9500, the same confidence at which a
// pair inside the window would be judged on the year rule instead.
func isYearToken(tok string) bool {
	if len(tok) != 4 {
		return false
	}
	n, err := strconv.Atoi(tok)
	return err == nil && n >= 1900 && n <= 2099
}

// isVariantToken reports whether an unmatched token is release packaging
// rather than content.
//
// yearsAreContent inverts the year rule for ONE comparison, and it exists
// because a year is packaging or content depending on what the OTHER title
// carries -- a fact no per-token predicate can see. See
// titleTokensCorrespond, which computes it.
//
// A four-digit year is otherwise treated as packaging: it is how a remaster or
// reissue is labeled ("Song (2011 Remaster)"). Any OTHER number is CONTENT.
func isVariantToken(tok string, yearsAreContent bool) bool {
	if _, ok := titleVariantTokens[tok]; ok {
		return true
	}
	if isYearToken(tok) {
		return !yearsAreContent
	}
	return false
}

// yearsDiffer reports whether both multisets carry a year AND no year is
// shared between them.
//
// THIS IS THE DISCRIMINATOR FOR THE YEAR SIBLING CLASS, and its failure
// direction is the one that matters. Treating every four-digit year as
// packaging admitted a whole family of DIFFERENT SONGS by the same artist:
// measured, "Nocturne 1984" vs "Nocturne 2019" accepted at 0.9385, and
// "Placeholder 1990" vs "Placeholder 1991" at 0.9750 -- numerically the same
// territory as the part-number siblings #883 filed as the canonical defect.
// Both sides reduced to [content, <year>], the year was deemed packaging, the
// shared content token satisfied the accept, and another song's words would be
// written next to the user's audio. internal/timing cannot catch it: two
// pieces from one artist's catalog routinely run similar lengths.
//
// The rule is STRUCTURAL rather than a threshold, which is what lets it
// separate these without costing a legitimate accept. No confidence floor can:
// these pairs score 0.85 to 0.98, ABOVE the weakest legitimate field in the
// calibration corpus (0.7597, a leading article), so any floor high enough to
// reject them rejects that too.
//
// ONE SIDE CARRYING A YEAR IS STILL PACKAGING, deliberately. That is exactly
// the remaster shape the year rule was written for -- "Song" vs
// "Song (2011 Remaster)" -- where the year appears on one side only. The
// discriminator fires solely when BOTH titles are dated and the dates
// disagree, which is when the year is carrying the identity rather than the
// pressing.
//
// A false REJECT here costs one missing lyric and a retry; a false ACCEPT
// writes another song's words to disk and looks correct. The rule is written
// against that asymmetry.
func yearsDiffer(req, cand map[string]int) bool {
	var reqYears, candYears []string
	for tok := range req {
		if isYearToken(tok) {
			reqYears = append(reqYears, tok)
		}
	}
	for tok := range cand {
		if isYearToken(tok) {
			candYears = append(candYears, tok)
		}
	}
	if len(reqYears) == 0 || len(candYears) == 0 {
		return false
	}
	for _, ry := range reqYears {
		for _, cy := range candYears {
			if ry == cy {
				return false
			}
		}
	}
	return true
}

// tokenizeTitle produces the comparable token sequence for a title: ignorable
// tokens dropped, "sped up"/"slowed down" collapsed to a single variant token,
// and everything from a featuring marker onward truncated away.
func tokenizeTitle(s string) []string {
	raw := splitTokens(s)
	out := make([]string, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		tok := raw[i]
		if _, ok := featMarkers[tok]; ok {
			break
		}
		if _, ok := ignorableTokens[tok]; ok {
			continue
		}
		if i+1 < len(raw) {
			switch {
			case tok == "sped" && raw[i+1] == "up":
				out, i = append(out, "spedup"), i+1
				continue
			case tok == "slowed" && raw[i+1] == "down":
				out, i = append(out, "sloweddown"), i+1
				continue
			}
		}
		out = append(out, tok)
	}
	return out
}

// countTokens turns a token sequence into a multiset.
func countTokens(toks []string) map[string]int {
	counts := make(map[string]int, len(toks))
	for _, t := range toks {
		counts[t]++
	}
	return counts
}

// titleTokensCorrespond applies the multiset rule documented above.
//
// It fails OPEN when either side tokenizes to nothing (a title that is entirely
// punctuation, or entirely a featuring credit): there is no token evidence to
// judge on, and the floor has already been cleared by the caller, so inventing
// a rejection here would be a guess rather than a finding.
func titleTokensCorrespond(requested, got string) bool {
	reqToks := tokenizeTitle(requested)
	gotToks := tokenizeTitle(got)
	if len(reqToks) == 0 || len(gotToks) == 0 {
		return true
	}

	req := countTokens(reqToks)
	cand := countTokens(gotToks)

	// Computed ONCE for this comparison and threaded through every
	// isVariantToken call below, so all three decisions agree about whether a
	// year is content here.
	yearsAreContent := yearsDiffer(req, cand)

	// Identical multisets are the same words by construction -- accepted
	// before the shared-content-token requirement below, which would otherwise
	// reject a title made entirely of vocabulary words matching itself.
	sameMultiset := len(req) == len(cand)
	if sameMultiset {
		for tok, n := range req {
			if cand[tok] != n {
				sameMultiset = false
				break
			}
		}
	}
	if sameMultiset {
		return true
	}

	// Every unmatched token, in either direction, must be packaging.
	for tok, n := range req {
		if n > cand[tok] && !isVariantToken(tok, yearsAreContent) {
			return false
		}
	}
	for tok, n := range cand {
		if n > req[tok] && !isVariantToken(tok, yearsAreContent) {
			return false
		}
	}

	// And the two titles must share at least one CONTENT token, so that two
	// different titles built only from vocabulary words cannot correspond
	// merely by both being made of noise.
	for tok, n := range req {
		if n > 0 && cand[tok] > 0 && !isVariantToken(tok, yearsAreContent) {
			return true
		}
	}
	return false
}

// titleFieldCorresponds is the title half of the gate: the floor AND the token
// multiset rule. Both must hold, so the token rule can only ever make the gate
// STRICTER than the floor alone.
func titleFieldCorresponds(requested, got string) (ok, comparable bool) {
	ok, comparable = fieldCorresponds(requested, got)
	if !comparable || !ok {
		return ok, comparable
	}
	return titleTokensCorrespond(requested, got), true
}

// artistTokensEqual reports whether two artist strings name the same tokens
// with the same multiplicities, ignoring ORDER.
//
// Order-insensitivity is the whole point: a credited-order swap names the same
// act. MULTIPLICITY is kept, and that is a correction (853-R5F1). An earlier
// revision collapsed duplicates into a set, which made a doubled-word act
// indistinguishable from its single-word namesake and created a wrong-artist
// ACCEPT class -- a repeated-word act versus a differently-named one, or the
// "X X & Y" versus "X & Y" collaboration shape, compared equal and a lyric
// from the wrong act could be written. A multiset accepts every legitimate
// reorder the set accepted, so discarding multiplicity bought nothing and
// cost a false accept.
//
// It requires FULL equality, never overlap. Two different acts that merely
// share a token still have unequal multisets and are rejected; accepting on
// overlap would make any act sharing one common word correspond to any other,
// a far larger false-accept class than the false rejects this path removes.
//
// Featuring markers are dropped rather than truncating, unlike a title: the
// names following them are part of the act being named, so a swap of the
// credited order has to compare equal.
func artistTokensEqual(requested, got string) bool {
	counts := func(s string) map[string]int {
		out := make(map[string]int)
		for _, tok := range splitTokens(s) {
			if _, ok := ignorableTokens[tok]; ok {
				continue
			}
			if _, ok := featMarkers[tok]; ok {
				continue
			}
			out[tok]++
		}
		return out
	}
	req, cand := counts(requested), counts(got)
	if len(req) == 0 || len(cand) == 0 || len(req) != len(cand) {
		return false
	}
	for tok, n := range req {
		if cand[tok] != n {
			return false
		}
	}
	return true
}

// artistFieldCorresponds is the artist half of the gate: the floor OR an exact
// token-multiset match. This is a WIDENING, and it is the opposite treatment from
// the title's on purpose.
//
// The measured defect it fixes: legitimate artist REORDERINGS score BELOW the
// worst wrong-track accept. A featured-credit ordering swap measured 0.6736 and
// a trailing-article form 0.7076, both under the 0.75 floor, while a wrong
// track reached 0.8788. Jaro-Winkler is an ordered-string measure, so moving a
// name from the front to the back reads as a large edit even though the act
// named is identical -- the similarity axis is simply the wrong instrument for
// this field.
//
// The comparison is a token MULTISET, like the title rule -- both are
// order-insensitive, so an earlier claim here that the title rule "preserves
// order" and therefore could not fix this was simply false about the code it
// cited (853-R5F2). What the two rules do NOT share is the rest of the title
// machinery: the variant-token and shared-content-token requirements exist to
// separate a track from its remix or live sibling, and applying them to an
// artist would re-introduce the reordering false rejects this path removes.
// Each field gets the comparison its failure mode needs; the difference is
// that machinery, not the multiset.
func artistFieldCorresponds(requested, got string) (ok, comparable bool) {
	ok, comparable = fieldCorresponds(requested, got)
	if !comparable {
		return false, false
	}
	if ok {
		return true, true
	}
	return artistTokensEqual(requested, got), true
}
