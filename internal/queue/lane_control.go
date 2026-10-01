package queue

import (
	"context"
	"fmt"
	"strings"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
)

// LatestServedTrack returns the most recently completed track the named provider
// lane was stamped onto, as the provider was asked for it, and false when the
// lane has no such row (#1195).
//
// A provider_lane stamp is durable evidence the provider served that track, so
// it can stand in for the liveness control a lane would otherwise have to earn
// again after every restart. The artist is resolved exactly as the worker
// resolves it before querying a provider (normalize.ResolveArtist, album artist
// preferred), so the control re-asks the provider the question it answered.
//
// Only a SETTLED, error-free row counts (status 'done', empty last_error). A
// provider_lane stamp outlives the win it recorded: purge-provenance resets a
// row to deferred and keeps the stamp, and if the lane then misses it RetireMiss
// settles it 'unavailable' with a fresh completed_at, which would otherwise win
// the ordering and seed a control the lane demonstrably does NOT serve. Prune's
// retireUnresolvable settles a row 'done' with a non-empty last_error, so the
// error filter excludes that shape too. Every path that settles a genuine win
// (Complete, the word-recheck and upgrade-trip settles) clears last_error.
//
// Rows whose title or resolved artist is blank are skipped: a control that
// cannot be queried is no control. Most recent completion wins; a row with no
// completed_at sorts last, then newest id. The artist is checked after
// resolution rather than in SQL because a generic album artist over a blank
// track artist resolves blank; iteration stops at the first usable row. Called
// once per serve startup. Read-only.
func (q *DBQueue) LatestServedTrack(ctx context.Context, lane string) (track models.Track, found bool, retErr error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT artist, album_artist, title, album FROM work_queue
         WHERE provider_lane = ?
           AND status = 'done'
           AND COALESCE(last_error, '') = ''
           AND TRIM(title) <> ''
         ORDER BY completed_at IS NULL, completed_at DESC, id DESC`,
		lane,
	)
	if err != nil {
		return models.Track{}, false, fmt.Errorf("queue: latest served track: %w", err)
	}
	defer func() {
		if cerr := rows.Close(); cerr != nil && retErr == nil {
			retErr = fmt.Errorf("queue: close latest served track: %w", cerr)
		}
	}()
	for rows.Next() {
		var artist, albumArtist, title, album string
		if err := rows.Scan(&artist, &albumArtist, &title, &album); err != nil {
			return models.Track{}, false, fmt.Errorf("queue: scan latest served track: %w", err)
		}
		// Trim only to judge usability: the worker queries with the stored
		// values as-is (ResolveArtist returns the track-artist fallback
		// untrimmed), so the seed must carry the same identity.
		resolved := normalize.ResolveArtist(albumArtist, artist)
		if strings.TrimSpace(resolved) == "" {
			continue
		}
		return models.Track{
			ArtistName:  resolved,
			TrackName:   title,
			AlbumName:   album,
			AlbumArtist: albumArtist,
		}, true, nil
	}
	if err := rows.Err(); err != nil {
		return models.Track{}, false, fmt.Errorf("queue: iterate latest served track: %w", err)
	}
	return models.Track{}, false, nil
}
