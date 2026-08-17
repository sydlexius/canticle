// Package identity implements the shared exact-match and heuristic
// name-similarity tiers used to re-attach something (a lyric sidecar, a
// database row) to the audio file it belongs to after a move, rename, or
// reorganization (#640).
//
// It exists so two independently-written resolvers never disagree about where
// a file went. Before this package, internal/realign carried a private
// four-tier resolver for re-attaching orphaned .lrc/.txt sidecars to renamed
// audio; internal/prune needed the exact same exact/heuristic matching logic to
// decide whether a work_queue/scan_results row whose source vanished was
// genuinely deleted or merely moved. Two copies of "does this ISRC/MBID/name
// match that candidate" would eventually drift -- one resolver could re-link a
// sidecar to file A while the other re-links the DB row to file B, silently
// pointing a lyric file and its own database record at two different audio
// files. This package is that one shared ladder; both realign and prune build
// Candidate sets from their own data and delegate the verdict here.
//
// The package is deliberately free of filesystem and database concerns: it
// takes plain values in and returns a plain verdict out. Both callers own
// building their candidate pool (realign walks a directory tree and reads tags;
// prune queries scan_results for the library's other rows) and own interpreting
// the verdict (realign renames a file; prune re-links or retains a database
// row).
package identity

import (
	"iter"
	"slices"
	"strings"

	"github.com/sydlexius/canticle/internal/normalize"
)

// Verdict is the outcome of the exact-match tier.
type Verdict int

const (
	// VerdictNone means no candidate matched any configured identity key: the
	// orphan carries no usable identifier that this candidate pool can resolve.
	VerdictNone Verdict = iota
	// VerdictUnique means exactly one candidate matched, at the first identity
	// key (in caller-supplied order) that produced any match at all.
	VerdictUnique
	// VerdictConflict means more than one candidate shares the same identity
	// value at the first key that produced a match -- an ambiguity that must
	// never be resolved by guessing.
	VerdictConflict
)

// Candidate is one abstract match target: a file, or the surviving side of a
// database row, carrying the same identity/name signals the orphan side is
// compared against. Ref is an opaque handle the caller round-trips back out of
// a Unique verdict (a file path for realign, a row/source-path for prune);
// this package never interprets it.
type Candidate struct {
	Ref    string
	MBID   string
	ISRC   string
	Artist string
	Title  string
	// Stem is the filename-derived name evidence, used only as a fallback when
	// neither Artist nor Title is present. It is SEPARATE from Ref because Ref is
	// an opaque round-trip handle -- realign's Ref is a full path, and scoring a
	// path against a stem silently depresses every similarity score.
	Stem string
}

// Keys is the ordered set of identity fields the exact tier consults, most
// authoritative first. NormalizeKeys produces this from raw config input.
type Keys []string

// NormalizeKeys lowercases, filters to the known identity keys ("mbid",
// "isrc"), and de-duplicates while preserving order. Shared by realign and
// prune so both read config.RealignConfig.IdentityKeys the same way and can
// never disagree about key precedence.
func NormalizeKeys(keys []string) Keys {
	seen := map[string]bool{}
	out := make(Keys, 0, len(keys))
	for _, k := range keys {
		k = strings.ToLower(strings.TrimSpace(k))
		if k != "mbid" && k != "isrc" {
			continue
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// orphanValue returns the value the caller carries for identity key k, from
// an orphan's known mbid/isrc pair.
func orphanValue(mbid, isrc, key string) string {
	switch key {
	case "mbid":
		return mbid
	case "isrc":
		return isrc
	}
	return ""
}

// candidateValue returns c's value for identity key k.
func candidateValue(c Candidate, key string) string {
	switch key {
	case "mbid":
		return c.MBID
	case "isrc":
		return c.ISRC
	}
	return ""
}

// ResolveExact finds the unique candidate in pool whose MBID or ISRC matches
// the orphan's, honoring keys order (most authoritative first: the first key
// that produces ANY match at all decides the verdict, so a conflict at the
// primary key is never silently rescued by a secondary key that happens to be
// unambiguous).
//
// Returns (VerdictNone, "") when no key in the pool matches, (VerdictUnique,
// ref) when exactly one candidate matches at the first key that matched
// anything, and (VerdictConflict, "") when more than one candidate shares that
// value.
//
// This is the slice form, for a caller whose candidate pool is already
// materialized in memory at no further cost (internal/prune holds its pool as
// scan_results rows already loaded and stat-checked). A caller for whom
// producing a candidate is EXPENSIVE -- internal/realign must read ID3 tags off
// disk to learn a candidate's identity -- must use ResolveExactSeq instead, so
// no candidate is ever realized unless the ladder actually needs to inspect it.
// Both forms run the same decision logic: this one is a one-line delegation.
func ResolveExact(orphanMBID, orphanISRC string, keys Keys, pool []Candidate) (Verdict, string) {
	return ResolveExactSeq(orphanMBID, orphanISRC, keys, slices.Values(pool))
}

// ResolveExactSeq is ResolveExact over a lazy candidate sequence, and is the
// sole implementation of the exact tier's decision logic.
//
// The sequence is pulled ONLY from inside the per-key loop, and only once the
// orphan is known to carry a non-empty value for that key. An orphan with no
// identity at all therefore never iterates candidates even once, so a caller
// whose iteration performs I/O (realign reading tags off disk) pays nothing for
// the case it cannot possibly resolve. This ordering is load-bearing, not an
// optimization detail: materializing the pool before the key loop reads every
// candidate file in a directory to answer a question that needed no reads,
// which is exactly the regression this signature exists to prevent.
//
// The sequence is re-iterated once per identity key that the orphan carries a
// value for (at most len(keys) times, and only until a key produces a match).
// A caller whose realization is expensive should therefore memoize it; realign
// does, via its per-plan provenance cache.
func ResolveExactSeq(orphanMBID, orphanISRC string, keys Keys, candidates iter.Seq[Candidate]) (Verdict, string) {
	for _, key := range keys {
		id := strings.TrimSpace(orphanValue(orphanMBID, orphanISRC, key))
		if id == "" {
			continue
		}
		var matches []string
		for c := range candidates {
			if strings.EqualFold(strings.TrimSpace(candidateValue(c, key)), id) {
				matches = append(matches, c.Ref)
				// Two matches already settle this key as a conflict, and a
				// conflict carries no ref, so nothing a third match could add
				// changes the answer. Stop pulling: for realign that is one
				// fewer tag read per remaining candidate.
				if len(matches) > 1 {
					break
				}
			}
		}
		switch len(matches) {
		case 0:
			continue
		case 1:
			return VerdictUnique, matches[0]
		default:
			return VerdictConflict, ""
		}
	}
	return VerdictNone, ""
}

// NameSignal is one side's name evidence for the heuristic tier: the tag-derived
// artist and title, plus the filesystem stem that stands in when tags are absent.
// Both sides of a comparison are described the same way, so a tagged sidecar and
// an untagged audio file (or the reverse) compare coherently.
type NameSignal struct {
	Artist string
	Title  string
	Stem   string
}

// HasName reports whether this side carries TAG-derived name evidence. A bare
// stem does not count: the stem is a fallback, not evidence that the file was
// ever identified.
func (n NameSignal) HasName() bool {
	return strings.TrimSpace(n.Artist) != "" || strings.TrimSpace(n.Title) != ""
}

// discriminator returns the single string this side contributes to a
// name-similarity comparison, most discriminating first: title, else stem, else
// artist.
//
// The ARTIST IS DELIBERATELY EXCLUDED when a title or stem is available (#672).
// Inside an album directory -- the scope in which realign pairs an orphan with a
// candidate -- every candidate shares the same artist, so the artist contributes
// a large constant to EVERY pairwise score and discriminates between none of
// them. Concatenating artist and title (the pre-#672 behavior) let two unrelated
// tracks by the same artist score 0.87 against each other, clearing the 0.75
// floor purely on the shared prefix, while their titles alone scored 0.39. It
// costs nothing on a true match: two sides naming the same track have identical
// titles, and identical strings score 1.0 with or without the artist.
//
// The stem outranks the artist for the same reason: a stem like
// "05. Some Title" carries the title, whereas an artist-only tag carries no
// track-level information at all.
func (n NameSignal) discriminator() string {
	if t := strings.TrimSpace(n.Title); t != "" {
		return t
	}
	if s := strings.TrimSpace(n.Stem); s != "" {
		return s
	}
	return strings.TrimSpace(n.Artist)
}

// NameScore is the SINGLE definition of how two sides' names are scored against
// each other, shared by every name-similarity tier so no two tiers can drift
// into disagreeing about the same pair.
//
// score is the Jaro-Winkler similarity of the two discriminators (see
// NameSignal.discriminator) and is always computed, including when neither side
// carries tags -- that case degrades to a stem-vs-stem comparison, which is what
// an N:M matcher over a folder of untagged renamed tracks needs.
//
// tagged reports whether EITHER side carried tag-derived name evidence. It is
// false only when both sides are bare stems, and exists so a caller that wants
// to treat "no name evidence anywhere" as a non-verdict (the positional 1:1
// degradation in HeuristicNameGuard) can tell that case apart from a genuine
// low score.
func NameScore(orphan, candidate NameSignal) (score float64, tagged bool) {
	tagged = orphan.HasName() || candidate.HasName()
	return normalize.MatchConfidence(orphan.discriminator(), candidate.discriminator()), tagged
}

// HeuristicNameGuard scores one orphan against one candidate via NameScore and
// reports whether the score clears minConfidence.
//
// When NEITHER side carries a tag-derived name, the guard has nothing to
// disprove and degrades to (true, 0, false), so a purely positional pairing -- a
// single untagged orphan matched with the single missing-sidecar file in the
// same directory, e.g. a plain .txt instrumental marker -- still succeeds. The
// returned tagged flag lets the caller see that the verdict was a degradation
// rather than a passing score, so it can decline to apply further score-based
// rules (a margin test) that a zero score would fail spuriously.
func HeuristicNameGuard(orphan, candidate NameSignal, minConfidence float64) (ok bool, score float64, tagged bool) {
	score, tagged = NameScore(orphan, candidate)
	if !tagged {
		return true, 0, false
	}
	return score >= minConfidence, score, true
}

// HeuristicResult is ResolveHeuristic's verdict plus the scores behind it, so a
// caller can report WHY it declined without recomputing anything. RunnerUp is
// meaningful only when HasRival; a lone target with nothing to confuse it has no
// runner-up rather than one scoring zero.
type HeuristicResult struct {
	Verdict  Verdict
	Ref      string
	Score    float64
	RunnerUp float64
	HasRival bool
}

// ResolveHeuristic resolves an orphan against a candidate pool by NAME
// SIMILARITY, as the tier below ResolveExact: it is consulted when the orphan
// carries no MBID or ISRC for the exact tier to match on.
//
// Promoted here from internal/realign (#740) so prune can reach it. Prune
// previously treated "no MBID and no ISRC" as proof that no relink could ever
// resolve a row and retired it as permanently unactionable -- but that premise
// was false precisely because this tier exists, and it was unreachable from
// prune because it lived in another package. Unresolved is not unresolvable, and
// only the latter justifies a terminal decision.
//
// The semantics are realign's, preserved rather than redesigned:
//
//   - Best candidate wins, but ONLY if it beats the runner-up by minMargin. A
//     near-tie is VerdictConflict, never a coin flip: a score its runner-up
//     nearly matches is a name signal that cannot tell the tracks apart, and
//     pairing on it attaches a sidecar to the WRONG song (#672).
//   - Below minConfidence is VerdictNone -- nothing matched, as distinct from
//     "several things matched equally well".
//   - The UNTAGGED degradation from HeuristicNameGuard is honored: when neither
//     side carries a tag-derived name the score is a placeholder zero, so the
//     margin test would reject every such pair spuriously. A LONE untagged
//     candidate is therefore a positional pairing and resolves (this is what
//     lets a plain instrumental-marker sidecar re-attach); two or more untagged
//     candidates is a conflict, because position identifies nothing among them.
//
// TARGETS AND RIVALS ARE SEPARATE SETS, and conflating them is a correctness
// bug rather than a simplification. Targets are what the orphan may be attached
// TO; rivals are everything the name signal must distinguish it FROM. They are
// usually different:
//
//   - prune passes the same slice for both -- every present file is both a
//     possible destination and a possible confusion.
//   - realign passes only the SIDECAR-LESS audio as targets, but every audio
//     file in the directory as rivals. A file that already has a sidecar is not
//     a legal destination, yet it still has to be out-scored: an orphan that
//     matches it better than the gap is evidence the name cannot tell the tracks
//     apart. Scoring against targets alone would silently attach the sidecar to
//     the wrong song, which is the #672 defect.
//
// The returned score is the winning similarity, or the best observed score on a
// non-unique verdict, so a caller can report WHY it declined.
func ResolveHeuristic(orphan NameSignal, targets, rivals []Candidate, minConfidence, minMargin float64) HeuristicResult {
	if len(targets) == 0 {
		return HeuristicResult{Verdict: VerdictNone}
	}

	score := func(c Candidate) (float64, bool) {
		return NameScore(orphan, NameSignal{Artist: c.Artist, Title: c.Title, Stem: c.Stem})
	}

	bestRef, best, seen := "", 0.0, false
	anyTagged := false
	for _, c := range targets {
		s, tagged := score(c)
		anyTagged = anyTagged || tagged
		if !seen || s > best {
			bestRef, best, seen = c.Ref, s, true
		}
	}

	// The runner-up is the best score among everything that is NOT the chosen
	// target -- drawn from rivals, so a sidecar-bearing file still counts as
	// confusable even though it can never be the destination.
	runnerUp, hasRival := 0.0, false
	for _, c := range rivals {
		if c.Ref == bestRef {
			continue
		}
		s, tagged := score(c)
		anyTagged = anyTagged || tagged
		if !hasRival || s > runnerUp {
			runnerUp, hasRival = s, true
		}
	}

	// Positional degradation: no side anywhere carried a tag-derived name, so
	// every score is a placeholder zero and comparing them is meaningless.
	//
	// Keyed on the TARGET COUNT alone, deliberately NOT on the absence of rivals.
	// realign's 1:1 tier pairs a lone untagged orphan with the lone gap even when
	// the directory holds other audio -- an instrumental .txt with no name
	// evidence on either side still has to re-attach. Requiring no rivals broke
	// exactly that case, and realign's own suite caught it.
	//
	// A second TARGET is different: with no name evidence and two legal
	// destinations, position identifies nothing and picking one is a guess.
	if !anyTagged {
		if len(targets) == 1 {
			return HeuristicResult{Verdict: VerdictUnique, Ref: targets[0].Ref}
		}
		return HeuristicResult{Verdict: VerdictConflict, HasRival: hasRival}
	}

	if best < minConfidence {
		return HeuristicResult{Verdict: VerdictNone, Score: best, RunnerUp: runnerUp, HasRival: hasRival}
	}
	// A runner-up within minMargin of the winner means the signal cannot
	// discriminate. Only meaningful when a rival actually exists; a lone target
	// with nothing else in the directory has no runner-up to be too close to.
	if hasRival && best-runnerUp < minMargin {
		return HeuristicResult{Verdict: VerdictConflict, Score: best, RunnerUp: runnerUp, HasRival: true}
	}
	return HeuristicResult{Verdict: VerdictUnique, Ref: bestRef, Score: best, RunnerUp: runnerUp, HasRival: hasRival}
}
