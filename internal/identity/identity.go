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
func ResolveExact(orphanMBID, orphanISRC string, keys Keys, pool []Candidate) (Verdict, string) {
	for _, key := range keys {
		id := strings.TrimSpace(orphanValue(orphanMBID, orphanISRC, key))
		if id == "" {
			continue
		}
		var matches []string
		for _, c := range pool {
			if strings.EqualFold(strings.TrimSpace(candidateValue(c, key)), id) {
				matches = append(matches, c.Ref)
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

// HeuristicNameGuard scores an orphan's artist/title (or a positional stem,
// when no name is available on either side) against one candidate's
// artist/title via Jaro-Winkler, and reports whether the score clears
// minConfidence. It degrades to true when neither side carries a name at all,
// so a purely positional pairing (a single orphan matched with a single
// missing-sidecar file in the same directory) still succeeds -- the guard has
// nothing to disprove.
func HeuristicNameGuard(orphanArtist, orphanTitle, orphanStem string, candidate Candidate, candidateStem string, minConfidence float64) (bool, float64) {
	hasOrphanName := orphanArtist != "" || orphanTitle != ""
	hasCandidateName := candidate.Artist != "" || candidate.Title != ""
	if !hasOrphanName && !hasCandidateName {
		return true, 0
	}
	orphanStr := orphanStem
	if hasOrphanName {
		orphanStr = strings.TrimSpace(orphanArtist + " " + orphanTitle)
	}
	candidateStr := candidateStem
	if hasCandidateName {
		candidateStr = strings.TrimSpace(candidate.Artist + " " + candidate.Title)
	}
	score := normalize.MatchConfidence(orphanStr, candidateStr)
	return score >= minConfidence, score
}
