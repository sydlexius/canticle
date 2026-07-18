package providers

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

type fakeFetcher struct{}

func (fakeFetcher) FindLyrics(context.Context, models.Track) (models.Song, error) {
	return models.Song{}, nil
}

func TestSelectDefaultsToMusixmatch(t *testing.T) {
	p, err := Select("", nil, New(Musixmatch, fakeFetcher{}))
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if p.Name() != Musixmatch {
		t.Fatalf("provider = %q; want %q", p.Name(), Musixmatch)
	}
}

func TestSelectRejectsDisabledProvider(t *testing.T) {
	_, err := Select(Musixmatch, []string{" MUSIXMATCH "}, New(Musixmatch, fakeFetcher{}))
	if err == nil {
		t.Fatal("Select returned nil error; want disabled provider error")
	}
}

func TestSelectRejectsUnsupportedProvider(t *testing.T) {
	_, err := Select("future", nil, New(Musixmatch, fakeFetcher{}))
	if err == nil {
		t.Fatal("Select returned nil error; want unsupported provider error")
	}
}

func TestKnown(t *testing.T) {
	known := Known()
	if len(known) != 2 {
		t.Fatalf("Known() = %v; want exactly the two built-in providers", known)
	}
	if known[0] != Musixmatch || known[1] != PetitLyrics {
		t.Fatalf("Known() = %v; want [%q %q]", known, Musixmatch, PetitLyrics)
	}
}

func TestIsKnown(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"musixmatch", true},
		{" PetitLyrics ", true}, // case-insensitive, trimmed
		{"bogus", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsKnown(c.name); got != c.want {
			t.Errorf("IsKnown(%q) = %v; want %v", c.name, got, c.want)
		}
	}
}

// adaptiveFetcher implements both Fetcher and AdaptivePacer to verify the
// namedProvider wrapper forwards adaptive notifications to the inner fetcher.
type adaptiveFetcher struct {
	throttles int
	successes int
}

func (a *adaptiveFetcher) FindLyrics(context.Context, models.Track) (models.Song, error) {
	return models.Song{}, nil
}
func (a *adaptiveFetcher) OnThrottle() { a.throttles++ }
func (a *adaptiveFetcher) OnSuccess()  { a.successes++ }

func TestNamedProviderForwardsAdaptivePacer(t *testing.T) {
	af := &adaptiveFetcher{}
	p := New(Musixmatch, af)

	ap, ok := p.(AdaptivePacer)
	if !ok {
		t.Fatal("namedProvider does not satisfy AdaptivePacer")
	}
	ap.OnThrottle()
	ap.OnThrottle()
	ap.OnSuccess()
	if af.throttles != 2 {
		t.Fatalf("forwarded throttles = %d; want 2", af.throttles)
	}
	if af.successes != 1 {
		t.Fatalf("forwarded successes = %d; want 1", af.successes)
	}
}

func TestNamedProviderAdaptiveNoopForPlainFetcher(t *testing.T) {
	// A fetcher without AdaptivePacer: the wrapper's methods must be safe no-ops.
	p := New(Musixmatch, fakeFetcher{})
	ap, ok := p.(AdaptivePacer)
	if !ok {
		t.Fatal("namedProvider should always satisfy AdaptivePacer")
	}
	// Must not panic even though the inner fetcher does not implement the iface.
	ap.OnThrottle()
	ap.OnSuccess()
}
