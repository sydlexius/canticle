package lyricblock

import (
	"context"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
)

var _ lyrics.BlockChecker = (*Store)(nil)

// SongBlocked reports whether song's body is blocked for the work-queue row
// identity in song.IdentityArtistKey/IdentityTitleKey (never song.Track's). An
// empty identity is not blocked. It fails open like AnyBlocked.
func (s *Store) SongBlocked(ctx context.Context, song models.Song) bool {
	if song.IdentityArtistKey == "" && song.IdentityTitleKey == "" {
		return false
	}
	return s.AnyBlocked(ctx, song.IdentityArtistKey, song.IdentityTitleKey, SongFingerprints(song))
}
