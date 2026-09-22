package providers

import "testing"

func TestWordCapable(t *testing.T) {
	for name, want := range map[string]bool{
		Musixmatch: true, " PetitLyrics ": true, InnerTube: false, "detector": false, "": false,
	} {
		if got := WordCapable(name); got != want {
			t.Errorf("WordCapable(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestWordGeneration_OrderIndependent reddens if the sort is dropped: the two
// orders hash different joined strings.
func TestWordGeneration_OrderIndependent(t *testing.T) {
	a := WordGeneration([]string{Musixmatch, PetitLyrics})
	b := WordGeneration([]string{"PETITLYRICS", Musixmatch, Musixmatch})
	if a != b {
		t.Fatalf("order/case/duplicates changed the generation: %d vs %d", a, b)
	}
}

func TestWordGeneration_ChangesWithWordLaneSet(t *testing.T) {
	both := WordGeneration([]string{Musixmatch, PetitLyrics})
	if one := WordGeneration([]string{Musixmatch}); one == both {
		t.Fatalf("removing a word-capable lane did not change the generation (%d)", both)
	}
	if none := WordGeneration(nil); none == both {
		t.Fatalf("an empty word lane set shares the generation %d", both)
	}
}

// TestWordGeneration_IgnoresLanesThatCannotServeWords: enabling a line-only
// lane must not expire every "absent" verdict.
func TestWordGeneration_IgnoresLanesThatCannotServeWords(t *testing.T) {
	base := WordGeneration([]string{Musixmatch})
	if got := WordGeneration([]string{InnerTube, Musixmatch, "detector"}); got != base {
		t.Fatalf("a non-word-capable lane changed the generation: %d vs %d", got, base)
	}
}

func TestWordGeneration_ChangesWithRevision(t *testing.T) {
	names := []string{Musixmatch, PetitLyrics}
	cur := wordGeneration(WordCapabilityRevision, names)
	if cur != WordGeneration(names) {
		t.Fatal("WordGeneration does not use WordCapabilityRevision")
	}
	if next := wordGeneration(WordCapabilityRevision+1, names); next == cur {
		t.Fatalf("a revision bump did not change the generation (%d)", cur)
	}
	if cur < 0 || cur > 0x7fff_ffff {
		t.Fatalf("generation %d is outside the 31-bit range", cur)
	}
}
