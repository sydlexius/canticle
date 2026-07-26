package identity

import "testing"

// REPRO 1: the case-0 fallthrough (primary key PRESENT but matching nothing,
// secondary key rescues) is not covered by any existing test.
// TestResolveExact_FallsThroughToSecondaryKey passes orphanMBID="", which exits
// at the `id == ""` guard, a DIFFERENT branch. This exercises the real one.
func TestRepro_FallthroughWhenPrimaryPresentButUnmatched(t *testing.T) {
	pool := []Candidate{{Ref: "a", MBID: "other-mbid", ISRC: "isrc-a"}}
	v, ref := ResolveExact("orphan-mbid-not-in-pool", "isrc-a", NormalizeKeys([]string{"mbid", "isrc"}), pool)
	if v != VerdictUnique || ref != "a" {
		t.Fatalf("ResolveExact = (%v,%q), want (Unique,a): a primary-key MISS must fall through to ISRC", v, ref)
	}
}

// REPRO 2: the positional-degrade condition is &&; with only ONE side naming
// itself, the guard compares a name against a STEM. No existing test pins this.
func TestRepro_OneSidedNameComparesNameAgainstStem(t *testing.T) {
	// Orphan has no name, candidate does. Guard must NOT degrade to true.
	ok, score, tagged := HeuristicNameGuard(
		NameSignal{Stem: "zzzzz-unrelated-stem"},
		NameSignal{Artist: "The Artist", Title: "The Title", Stem: "cand-stem"}, 0.9)
	if ok {
		t.Fatalf("one-sided name with unrelated stem: ok=true score=%v, want false", score)
	}
	if !tagged {
		t.Error("one side carried tags, so tagged must be true (only a both-sides-bare pair degrades)")
	}
}

// REPRO 3: threshold boundary. score == minConfidence must PASS (>=).
func TestRepro_ThresholdBoundaryIsInclusive(t *testing.T) {
	orphan := NameSignal{Artist: "A", Title: "B", Stem: "s"}
	cand := NameSignal{Artist: "A", Title: "C", Stem: "s2"}
	_, score, _ := HeuristicNameGuard(orphan, cand, 0.0)
	ok, _, _ := HeuristicNameGuard(orphan, cand, score)
	if !ok {
		t.Fatalf("score == minConfidence (%v) must pass the guard (>=), got false", score)
	}
}

// REPRO 4: whitespace-only / padded orphan identity.
func TestRepro_WhitespaceIdentity(t *testing.T) {
	pool := []Candidate{{Ref: "a", MBID: "abc-123"}}
	if v, ref := ResolveExact("  abc-123  ", "", NormalizeKeys([]string{"mbid"}), pool); v != VerdictUnique || ref != "a" {
		t.Fatalf("padded orphan MBID = (%v,%q), want (Unique,a)", v, ref)
	}
	// Whitespace-only identity must be treated as absent, not matched against
	// a whitespace-only candidate value.
	poolWS := []Candidate{{Ref: "a", MBID: "   "}}
	if v, ref := ResolveExact("   ", "", NormalizeKeys([]string{"mbid"}), poolWS); v != VerdictNone {
		t.Fatalf("whitespace-only identity = (%v,%q), want VerdictNone", v, ref)
	}
}

// REPRO 5: empty-string candidate identity must never match an orphan whose
// value at that key is empty -- the "everything with no ISRC is the same track"
// catastrophe. (Guarded by the id=="" continue; pinning it.)
func TestRepro_EmptyCandidateIdentityNeverMatches(t *testing.T) {
	pool := []Candidate{{Ref: "a"}, {Ref: "b"}, {Ref: "c"}}
	if v, ref := ResolveExact("", "", NormalizeKeys([]string{"mbid", "isrc"}), pool); v != VerdictNone {
		t.Fatalf("empty orphan vs empty pool = (%v,%q), want VerdictNone", v, ref)
	}
}
