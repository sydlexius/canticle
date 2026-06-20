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

// OnThrottle forwards a throttle notification to the underlying fetcher when it
// implements AdaptivePacer. It is a no-op for fetchers that do not, so the
// wrapper itself always satisfies AdaptivePacer regardless of the inner type.
func (p namedProvider) OnThrottle() {
	if ap, ok := p.fetcher.(AdaptivePacer); ok {
		ap.OnThrottle()
	}
}

// OnSuccess forwards a success notification to the underlying fetcher when it
// implements AdaptivePacer; it is a no-op otherwise.
func (p namedProvider) OnSuccess() {
	if ap, ok := p.fetcher.(AdaptivePacer); ok {
		ap.OnSuccess()
	}
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

// ValidateSelection checks the cross-field provider invariants the daemon needs
// to boot: the primary provider must not be in the disabled list (Select fails
// otherwise), and at least one known provider must remain enabled. It shares the
// providerDisabled primitive with Select, so it is the single source of truth
// for the settings write path's cross-field validation (a per-field "is this a
// known provider?" check cannot see these resulting-state invariants).
func ValidateSelection(primary string, disabled []string) error {
	primary = NormalizeName(primary)
	if primary == "" {
		primary = Musixmatch
	}
	if providerDisabled(primary, disabled) {
		return fmt.Errorf("provider %q is the primary source and cannot be disabled; change the primary first", primary)
	}
	for _, k := range Known() {
		if !providerDisabled(k, disabled) {
			return nil
		}
	}
	return fmt.Errorf("at least one provider must stay enabled")
}

// NormalizeName canonicalizes provider names for config comparisons.
func NormalizeName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// Known returns the built-in provider names in their canonical form.
func Known() []string {
	return []string{Musixmatch, PetitLyrics}
}

// IsKnown reports whether name (case-insensitively) matches a built-in provider.
func IsKnown(name string) bool {
	name = NormalizeName(name)
	for _, k := range Known() {
		if k == name {
			return true
		}
	}
	return false
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
