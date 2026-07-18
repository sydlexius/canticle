package musixmatch

import (
	"context"

	"github.com/sydlexius/canticle/internal/models"
)

// Fetcher abstracts lyrics lookup from the Musixmatch API.
type Fetcher interface {
	FindLyrics(ctx context.Context, track models.Track) (models.Song, error)
}
