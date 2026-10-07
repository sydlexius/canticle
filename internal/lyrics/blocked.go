package lyrics

import (
	"context"
	"errors"

	"github.com/sydlexius/canticle/internal/models"
)

// ErrBlocked is WriteLRC's refusal of a result whose body the operator marked
// wrong for this track (#1394). Nothing on disk was touched.
var ErrBlocked = errors.New("lyrics: result is blocked for this track")

// BlockChecker reports whether a fetched result is blocked for the identity in
// song.IdentityKey. It must fail open and treat an empty key as not blocked.
// lyricblock.Store implements it; this package never imports the database. A
// nil BlockChecker means no blocking: fetch mode has no database, so blocks do
// not apply there.
type BlockChecker interface {
	SongBlocked(ctx context.Context, song models.Song) bool
}

type blockIdentityKey struct{}

// WithBlockIdentity returns ctx carrying the work-queue row's block identity
// (queue.IdentityKeys of the row's own track), so the orchestrator and the cache
// accept predicate ask the checker under the row's key rather than the resolved
// (album-artist-substituted) query track's.
func WithBlockIdentity(ctx context.Context, artistKey, titleKey string) context.Context {
	return context.WithValue(ctx, blockIdentityKey{}, [2]string{artistKey, titleKey})
}

// StampBlockIdentity copies the identity WithBlockIdentity put on ctx onto song.
// A ctx without one leaves song unstamped, which no checker treats as blocked.
func StampBlockIdentity(ctx context.Context, song models.Song) models.Song {
	if k, ok := ctx.Value(blockIdentityKey{}).([2]string); ok {
		song.IdentityArtistKey, song.IdentityTitleKey = k[0], k[1]
	}
	return song
}
