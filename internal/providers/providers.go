package providers

import (
	"context"
	"fmt"
	"strings"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
)

const (
	// Musixmatch is the built-in Musixmatch provider name.
	Musixmatch = "musixmatch"
	// PetitLyrics is the Petit Lyrics provider name. The adapter lives in
	// internal/petitlyrics and is selectable via providers.primary =
	// "petitlyrics" (wired in internal/commands.selectedProvider).
	PetitLyrics = "petitlyrics"
)

// Fetcher is the shared lyrics lookup behavior used by provider adapters.
type Fetcher interface {
	FindLyrics(ctx context.Context, track models.Track) (models.Song, error)
}

// LyricsProvider identifies a lyrics lookup provider.
type LyricsProvider interface {
	Fetcher
	Name() string
}

type namedProvider struct {
	name    string
	fetcher Fetcher
}

// New wraps a fetcher with a provider name.
func New(name string, fetcher Fetcher) LyricsProvider {
	return namedProvider{name: NormalizeName(name), fetcher: fetcher}
}

func (p namedProvider) Name() string {
	return p.name
}

func (p namedProvider) FindLyrics(ctx context.Context, track models.Track) (models.Song, error) {
	return p.fetcher.FindLyrics(ctx, track)
}

// Select chooses the configured provider from the available candidates.
func Select(primary string, disabled []string, candidates ...LyricsProvider) (LyricsProvider, error) {
	primary = NormalizeName(primary)
	if primary == "" {
		primary = Musixmatch
	}
	if providerDisabled(primary, disabled) {
		return nil, fmt.Errorf("provider %q is disabled", primary)
	}
	for _, c := range candidates {
		if c != nil && NormalizeName(c.Name()) == primary {
			return c, nil
		}
	}
	return nil, fmt.Errorf("unsupported lyrics provider %q", primary)
}

// NormalizeName canonicalizes provider names for config comparisons.
func NormalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func providerDisabled(name string, disabled []string) bool {
	name = NormalizeName(name)
	for _, v := range disabled {
		if NormalizeName(v) == name {
			return true
		}
	}
	return false
}
