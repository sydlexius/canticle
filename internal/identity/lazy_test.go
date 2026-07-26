package identity

import (
	"iter"
	"testing"
)

// countingSeq returns a candidate sequence plus a pointer to the number of
// candidates actually REALIZED (yielded). A caller whose realization is a disk
// read cares about this number, not about the pool's size.
func countingSeq(pool []Candidate) (iter.Seq[Candidate], *int) {
	n := 0
	return func(yield func(Candidate) bool) {
		for _, c := range pool {
			n++
			if !yield(c) {
				return
			}
		}
	}, &n
}

// The whole point of the Seq form: an orphan carrying no identity value for
// any configured key must never realize a single candidate. This is the
// package-level twin of the realign repro that measures readProv invocations.
func TestResolveExactSeq_NoOrphanIdentityRealizesNothing(t *testing.T) {
	pool := make([]Candidate, 25)
	for i := range pool {
		pool[i] = Candidate{Ref: "r", MBID: "m", ISRC: "i"}
	}
	seq, realized := countingSeq(pool)
	if v, ref := ResolveExactSeq("", "", NormalizeKeys([]string{"mbid", "isrc"}), seq); v != VerdictNone || ref != "" {
		t.Fatalf("ResolveExactSeq = (%v,%q), want (None,\"\")", v, ref)
	}
	if *realized != 0 {
		t.Fatalf("realized %d candidates for an identity-less orphan, want 0", *realized)
	}
}

// A whitespace-only identity is treated as absent, so it too must realize
// nothing -- the trim happens before the pull, not after.
func TestResolveExactSeq_WhitespaceOnlyIdentityRealizesNothing(t *testing.T) {
	seq, realized := countingSeq([]Candidate{{Ref: "a", MBID: "m"}})
	if v, _ := ResolveExactSeq("   ", "", NormalizeKeys([]string{"mbid"}), seq); v != VerdictNone {
		t.Fatalf("whitespace identity = %v, want VerdictNone", v)
	}
	if *realized != 0 {
		t.Fatalf("realized %d candidates for a whitespace-only identity, want 0", *realized)
	}
}

// A key the orphan has no value for must not trigger a pass over the pool:
// only keys the orphan actually carries cost an iteration.
func TestResolveExactSeq_OnlyCarriedKeysIterate(t *testing.T) {
	pool := []Candidate{{Ref: "a", ISRC: "isrc-a"}, {Ref: "b", ISRC: "isrc-b"}}
	seq, realized := countingSeq(pool)
	// keys = [mbid, isrc] but the orphan carries only isrc: exactly one pass.
	if v, ref := ResolveExactSeq("", "isrc-b", NormalizeKeys([]string{"mbid", "isrc"}), seq); v != VerdictUnique || ref != "b" {
		t.Fatalf("ResolveExactSeq = (%v,%q), want (Unique,b)", v, ref)
	}
	if *realized != len(pool) {
		t.Fatalf("realized %d, want %d (one pass over the pool, mbid skipped)", *realized, len(pool))
	}
}

// A second match settles the key as a conflict, and a conflict carries no ref,
// so the resolver must stop pulling rather than realize the rest of the pool.
func TestResolveExactSeq_ConflictStopsPullingEarly(t *testing.T) {
	pool := []Candidate{
		{Ref: "a", MBID: "dup"},
		{Ref: "b", MBID: "dup"},
		{Ref: "c", MBID: "dup"},
		{Ref: "d", MBID: "dup"},
	}
	seq, realized := countingSeq(pool)
	if v, ref := ResolveExactSeq("dup", "", NormalizeKeys([]string{"mbid"}), seq); v != VerdictConflict || ref != "" {
		t.Fatalf("ResolveExactSeq = (%v,%q), want (Conflict,\"\")", v, ref)
	}
	if *realized != 2 {
		t.Fatalf("realized %d candidates, want 2 (stop at the second match)", *realized)
	}
}

// A primary-key MISS still costs a full pass, then falls through to the
// secondary key for a second pass. Pins the re-iteration contract the doc
// comment promises (and that realign's provenance cache makes free).
func TestResolveExactSeq_FallthroughReiterates(t *testing.T) {
	pool := []Candidate{{Ref: "a", MBID: "other", ISRC: "isrc-a"}}
	seq, realized := countingSeq(pool)
	if v, ref := ResolveExactSeq("no-such-mbid", "isrc-a", NormalizeKeys([]string{"mbid", "isrc"}), seq); v != VerdictUnique || ref != "a" {
		t.Fatalf("ResolveExactSeq = (%v,%q), want (Unique,a)", v, ref)
	}
	if *realized != 2*len(pool) {
		t.Fatalf("realized %d, want %d (one pass per carried key)", *realized, 2*len(pool))
	}
}

// The slice form must be observably identical to the Seq form -- it is the
// prune-side entry point, and one implementation is the entire premise of this
// package.
func TestResolveExact_SliceMatchesSeq(t *testing.T) {
	pool := []Candidate{
		{Ref: "a", MBID: "m-a", ISRC: "i-a"},
		{Ref: "b", MBID: "m-b", ISRC: "i-a"},
	}
	keys := NormalizeKeys([]string{"mbid", "isrc"})
	cases := []struct{ mbid, isrc string }{
		{"m-a", ""}, {"", "i-a"}, {"m-zzz", "i-a"}, {"", ""}, {"m-b", "i-a"},
	}
	for _, tc := range cases {
		seq, _ := countingSeq(pool)
		wantV, wantRef := ResolveExactSeq(tc.mbid, tc.isrc, keys, seq)
		gotV, gotRef := ResolveExact(tc.mbid, tc.isrc, keys, pool)
		if gotV != wantV || gotRef != wantRef {
			t.Fatalf("ResolveExact(%q,%q) = (%v,%q), Seq = (%v,%q): the two forms must agree",
				tc.mbid, tc.isrc, gotV, gotRef, wantV, wantRef)
		}
	}
}
