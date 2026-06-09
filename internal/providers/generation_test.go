package providers_test

import (
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/providers"
)

// TestGeneration_SortedOrderInsensitive verifies that changing the order of the
// provider list does not change the generation. The generation depends only on
// the SET of active provider names, not on their order, so reordering alone
// must not invalidate cached results.
func TestGeneration_SortedOrderInsensitive(t *testing.T) {
	g1 := providers.Generation([]string{"musixmatch", "petitlyrics"})
	g2 := providers.Generation([]string{"petitlyrics", "musixmatch"})
	if g1 != g2 {
		t.Fatalf("generation should be order-insensitive: got %d and %d", g1, g2)
	}
}

// TestGeneration_ChangesWhenProviderAdded verifies that adding a provider
// changes the generation so stale cached results are invalidated.
func TestGeneration_ChangesWhenProviderAdded(t *testing.T) {
	single := providers.Generation([]string{"musixmatch"})
	two := providers.Generation([]string{"musixmatch", "petitlyrics"})
	if single == two {
		t.Fatalf("generation should differ after adding a provider: both = %d", single)
	}
}

// TestGeneration_ChangesWhenProviderRemoved verifies that removing a provider
// changes the generation.
func TestGeneration_ChangesWhenProviderRemoved(t *testing.T) {
	two := providers.Generation([]string{"musixmatch", "petitlyrics"})
	one := providers.Generation([]string{"musixmatch"})
	if two == one {
		t.Fatalf("generation should differ after removing a provider: both = %d", two)
	}
}

// TestGeneration_CaseInsensitive verifies that provider names are normalized
// before hashing so "Musixmatch" and "musixmatch" produce the same generation.
func TestGeneration_CaseInsensitive(t *testing.T) {
	lower := providers.Generation([]string{"musixmatch"})
	upper := providers.Generation([]string{"MUSIXMATCH"})
	mixed := providers.Generation([]string{"Musixmatch"})
	if lower != upper || lower != mixed {
		t.Fatalf("generation should be case-insensitive: got lower=%d upper=%d mixed=%d", lower, upper, mixed)
	}
}

// TestGeneration_EmptySlice produces a stable (non-crashing) value for an empty
// provider set.
func TestGeneration_EmptySlice(t *testing.T) {
	_ = providers.Generation(nil)
	_ = providers.Generation([]string{})
}

// TestGeneration_Deterministic verifies the generation is stable across calls.
func TestGeneration_Deterministic(t *testing.T) {
	names := []string{"musixmatch", "petitlyrics"}
	g1 := providers.Generation(names)
	g2 := providers.Generation(names)
	if g1 != g2 {
		t.Fatalf("generation must be deterministic: got %d then %d", g1, g2)
	}
}
