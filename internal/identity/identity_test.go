package identity

import "testing"

func TestNormalizeKeys(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"order preserved", []string{"mbid", "isrc"}, []string{"mbid", "isrc"}},
		{"reordered input honored", []string{"isrc", "mbid"}, []string{"isrc", "mbid"}},
		{"case and whitespace", []string{" MBID ", "Isrc"}, []string{"mbid", "isrc"}},
		{"unknown filtered", []string{"mbid", "spotify_id", "isrc"}, []string{"mbid", "isrc"}},
		{"dedup keeps first", []string{"mbid", "mbid", "isrc"}, []string{"mbid", "isrc"}},
		{"empty", nil, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeKeys(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("NormalizeKeys(%v) = %v, want %v", tt.in, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("NormalizeKeys(%v) = %v, want %v", tt.in, got, tt.want)
				}
			}
		})
	}
}

func TestResolveExact_Unique(t *testing.T) {
	pool := []Candidate{
		{Ref: "a", MBID: "abc-123"},
		{Ref: "b", MBID: "def-456"},
	}
	v, ref := ResolveExact("abc-123", "", NormalizeKeys([]string{"mbid", "isrc"}), pool)
	if v != VerdictUnique || ref != "a" {
		t.Fatalf("ResolveExact = (%v, %q), want (Unique, a)", v, ref)
	}
}

func TestResolveExact_None(t *testing.T) {
	pool := []Candidate{{Ref: "a", MBID: "abc-123"}}
	v, ref := ResolveExact("zzz-999", "", NormalizeKeys([]string{"mbid", "isrc"}), pool)
	if v != VerdictNone || ref != "" {
		t.Fatalf("ResolveExact = (%v, %q), want (None, \"\")", v, ref)
	}
}

func TestResolveExact_NoIdentity(t *testing.T) {
	pool := []Candidate{{Ref: "a", MBID: "abc-123"}}
	v, ref := ResolveExact("", "", NormalizeKeys([]string{"mbid", "isrc"}), pool)
	if v != VerdictNone || ref != "" {
		t.Fatalf("ResolveExact with no orphan identity = (%v, %q), want (None, \"\")", v, ref)
	}
}

func TestResolveExact_Conflict(t *testing.T) {
	pool := []Candidate{
		{Ref: "a", MBID: "abc-123"},
		{Ref: "b", MBID: "abc-123"},
	}
	v, ref := ResolveExact("abc-123", "", NormalizeKeys([]string{"mbid", "isrc"}), pool)
	if v != VerdictConflict || ref != "" {
		t.Fatalf("ResolveExact duplicate MBID = (%v, %q), want (Conflict, \"\")", v, ref)
	}
}

// TestResolveExact_KeyOrderPrecedence: MBID is checked before ISRC. A conflict
// at the MBID tier must not be silently rescued by an unambiguous ISRC match --
// the first key that produces ANY match decides the verdict.
func TestResolveExact_KeyOrderPrecedence(t *testing.T) {
	pool := []Candidate{
		{Ref: "a", MBID: "shared-mbid", ISRC: "isrc-a"},
		{Ref: "b", MBID: "shared-mbid", ISRC: "isrc-b"},
	}
	v, ref := ResolveExact("shared-mbid", "isrc-a", NormalizeKeys([]string{"mbid", "isrc"}), pool)
	if v != VerdictConflict || ref != "" {
		t.Fatalf("ResolveExact = (%v, %q), want (Conflict, \"\") -- MBID conflict must not be rescued by an unambiguous ISRC", v, ref)
	}
}

// TestResolveExact_FallsThroughToSecondaryKey: when the primary key (mbid)
// matches nothing at all (not a conflict, a true miss), the secondary key
// (isrc) is still consulted.
func TestResolveExact_FallsThroughToSecondaryKey(t *testing.T) {
	pool := []Candidate{{Ref: "a", ISRC: "isrc-a"}}
	v, ref := ResolveExact("", "isrc-a", NormalizeKeys([]string{"mbid", "isrc"}), pool)
	if v != VerdictUnique || ref != "a" {
		t.Fatalf("ResolveExact = (%v, %q), want (Unique, a) via ISRC fallback", v, ref)
	}
}

// TestResolveExact_CaseInsensitive: identity comparison is case-insensitive,
// matching realign's original resolveExact behavior.
func TestResolveExact_CaseInsensitive(t *testing.T) {
	pool := []Candidate{{Ref: "a", MBID: "ABC-123"}}
	v, ref := ResolveExact("abc-123", "", NormalizeKeys([]string{"mbid"}), pool)
	if v != VerdictUnique || ref != "a" {
		t.Fatalf("ResolveExact case-insensitive = (%v, %q), want (Unique, a)", v, ref)
	}
}

func TestHeuristicNameGuard_AboveThreshold(t *testing.T) {
	ok, score := HeuristicNameGuard("The Artist", "The Title", "stem", Candidate{Artist: "The Artist", Title: "The Title"}, "stem2", 0.75)
	if !ok {
		t.Fatalf("HeuristicNameGuard identical names: ok=false score=%v, want true", score)
	}
}

func TestHeuristicNameGuard_BelowThreshold(t *testing.T) {
	ok, _ := HeuristicNameGuard("Completely Different Artist", "Totally Other Title", "stem", Candidate{Artist: "Zzz", Title: "Qqq"}, "stem2", 0.9)
	if ok {
		t.Fatalf("HeuristicNameGuard dissimilar names: ok=true, want false")
	}
}

// TestResolveExact_CandidateIdentityIsTrimmed: the CANDIDATE side is trimmed
// too, not just the orphan's. Tag writers pad values (a fixed-width ID3 frame,
// a trailing newline), and an untrimmed comparison would silently miss the one
// correct file and report VerdictNone -- a false "no match" that leaves the
// sidecar orphaned rather than re-attached.
func TestResolveExact_CandidateIdentityIsTrimmed(t *testing.T) {
	pool := []Candidate{{Ref: "a", MBID: "  abc-123\n"}}
	if v, ref := ResolveExact("abc-123", "", NormalizeKeys([]string{"mbid"}), pool); v != VerdictUnique || ref != "a" {
		t.Fatalf("padded CANDIDATE MBID = (%v,%q), want (Unique,a)", v, ref)
	}
}

// TestHeuristicNameGuard_NoNamesDegradesToPositional: when neither side
// carries a name, the guard has nothing to disprove and returns true.
func TestHeuristicNameGuard_NoNamesDegradesToPositional(t *testing.T) {
	ok, score := HeuristicNameGuard("", "", "stem", Candidate{}, "stem2", 0.75)
	if !ok || score != 0 {
		t.Fatalf("HeuristicNameGuard with no names = (%v, %v), want (true, 0)", ok, score)
	}
}
