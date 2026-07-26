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
	ok, score, tagged := HeuristicNameGuard(
		NameSignal{Artist: "The Artist", Title: "The Title", Stem: "stem"},
		NameSignal{Artist: "The Artist", Title: "The Title", Stem: "stem2"}, 0.75)
	if !ok || !tagged {
		t.Fatalf("HeuristicNameGuard identical names: ok=%v tagged=%v score=%v, want true/true", ok, tagged, score)
	}
}

func TestHeuristicNameGuard_BelowThreshold(t *testing.T) {
	ok, _, _ := HeuristicNameGuard(
		NameSignal{Artist: "Completely Different Artist", Title: "Totally Other Title", Stem: "stem"},
		NameSignal{Artist: "Zzz", Title: "Qqq", Stem: "stem2"}, 0.9)
	if ok {
		t.Fatalf("HeuristicNameGuard dissimilar names: ok=true, want false")
	}
}

// TestHeuristicNameGuard_SharedArtistDoesNotInflate is the #672 regression at
// the scorer level: two unrelated titles by the SAME artist must not clear the
// default 0.75 floor. Before the fix both sides were flattened to
// "<artist> <title>" and the shared artist prefix carried the pair over the
// floor (measured 0.87 in production) even though the titles alone scored 0.39.
func TestHeuristicNameGuard_SharedArtistDoesNotInflate(t *testing.T) {
	const artist = "Marbled Kestrel Choir"
	ok, score, _ := HeuristicNameGuard(
		NameSignal{Artist: artist, Title: "Harbor Lantern", Stem: "harbor-lantern"},
		NameSignal{Artist: artist, Title: "Sunken Cartography", Stem: "sunken-cartography"}, 0.75)
	if ok {
		t.Errorf("unrelated titles by the same artist scored %.4f and passed the 0.75 floor; "+
			"the artist must not contribute to the comparison", score)
	}
	if score >= 0.75 {
		t.Errorf("score = %.4f; want well below 0.75 (titles alone score ~0.59)", score)
	}
}

// TestHeuristicNameGuard_SameArtistSameTitleStillMatches: dropping the artist
// from the comparison must cost nothing on a TRUE match. Identical titles score
// 1.0 with or without the artist component.
func TestHeuristicNameGuard_SameArtistSameTitleStillMatches(t *testing.T) {
	ok, score, _ := HeuristicNameGuard(
		NameSignal{Artist: "Marbled Kestrel Choir", Title: "Harbor Lantern", Stem: "old-name"},
		NameSignal{Artist: "Marbled Kestrel Choir", Title: "Harbor Lantern", Stem: "07 - harbor lantern"}, 0.75)
	if !ok || score != 1.0 {
		t.Errorf("identical titles = (ok %v, score %.4f); want (true, 1.0)", ok, score)
	}
}

// TestNameScore_StemFallbackAndMixedSides pins the stem-fallback ladder
// (title > stem > artist) and the mixed case where one side carries tags and
// the other only a stem: a track-numbered stem must still match the tagged
// title, and must not match an unrelated one.
func TestNameScore_StemFallbackAndMixedSides(t *testing.T) {
	tagged := NameSignal{Artist: "Marbled Kestrel Choir", Title: "Harbor Lantern", Stem: "old-copy"}
	sameStem := NameSignal{Stem: "05. Harbor Lantern"}
	otherStem := NameSignal{Stem: "05. Sunken Cartography"}

	same, taggedFlag := NameScore(tagged, sameStem)
	if !taggedFlag {
		t.Error("one tagged side must report tagged=true")
	}
	other, _ := NameScore(tagged, otherStem)
	if same < 0.75 {
		t.Errorf("tagged title vs its own numbered stem = %.4f; want >= 0.75", same)
	}
	if other >= 0.75 {
		t.Errorf("tagged title vs an unrelated stem = %.4f; want < 0.75", other)
	}

	// Artist-only tags carry no track-level information, so the stem wins.
	artistOnly := NameSignal{Artist: "Marbled Kestrel Choir", Stem: "05. Harbor Lantern"}
	if s, _ := NameScore(artistOnly, sameStem); s != 1.0 {
		t.Errorf("artist-only side must fall back to its stem: score = %.4f, want 1.0", s)
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
	ok, score, tagged := HeuristicNameGuard(NameSignal{Stem: "stem"}, NameSignal{Stem: "stem2"}, 0.75)
	if !ok || score != 0 || tagged {
		t.Fatalf("HeuristicNameGuard with no names = (%v, %v, tagged=%v), want (true, 0, false)", ok, score, tagged)
	}
}
