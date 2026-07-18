package petitlyrics

import (
	"context"

	"github.com/sydlexius/canticle/internal/models"
)

// Fetcher abstracts lyrics lookup from petitlyrics.com. It mirrors the shared
// providers.Fetcher contract so the client is interchangeable with other
// provider adapters.
type Fetcher interface {
	FindLyrics(ctx context.Context, track models.Track) (models.Song, error)
}
