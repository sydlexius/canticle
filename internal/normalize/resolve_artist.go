package normalize

import "strings"

// ResolveArtist picks the primary artist to use for lyric matching. The
// album-artist tag is preferred because it is, for most releases, the cleanest
// single primary-artist string and is free of the multi-value concatenation
// that the track-artist tag can carry (e.g. FLAC repeated ARTIST comments or
// ID3v2.4 null-joined values). Generic compilation placeholders are NOT the
// track's artist, so they fall back to the track artist instead.
//
// When the album-artist is chosen it is returned trimmed of surrounding
// whitespace: the resolved value feeds the provider query directly (not only the
// cache key, which NormalizeKey trims separately), so leading/trailing spaces
// would otherwise degrade match quality. The track-artist fallback is returned
// as-is.
func ResolveArtist(albumArtist, artist string) string {
	trimmed := strings.TrimSpace(albumArtist)
	if trimmed != "" && !isGenericAlbumArtist(trimmed) {
		return trimmed
	}
	return artist
}

// isGenericAlbumArtist reports whether s is a compilation/various-artists
// placeholder that should not be used as the matching artist.
func isGenericAlbumArtist(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "various artists", "various", "va":
		return true
	default:
		return false
	}
}
