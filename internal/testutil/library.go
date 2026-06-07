package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GenSpec describes a synthetic library tree to generate.
type GenSpec struct {
	Artists int // number of artists
	Albums  int // albums per artist
	Tracks  int // tracks per album
	// EmbedLyrics embeds a USLT (unsynced) frame in every generated track.
	EmbedLyrics bool
	// LRCEvery, when > 0, writes a stub .lrc sidecar for every Nth track
	// (1 = all, 2 = every other, etc.); 0 writes none.
	LRCEvery int
	// Realistic, when true, generates real (catalog-matchable) artist/title/album
	// names from RealisticTracks instead of synthetic "Artist NN" placeholders,
	// so an actual fetch returns real lyrics. The Artists/Albums/Tracks dims are
	// ignored in this mode; the count is len(RealisticTracks).
	Realistic bool
}

// RealisticTrack is a real artist/title/album known to exist in mainstream lyric
// catalogs, used by Realistic mode for end-to-end fetch testing.
type RealisticTrack struct{ Artist, Title, Album string }

// RealisticTracks are real, widely-available songs (several verified present in
// Musixmatch) for exercising the real fetch path. Synthetic "Artist NN" names do
// not match any catalog, so realistic mode is required for end-to-end UAT.
var RealisticTracks = []RealisticTrack{
	{"Adele", "Hello", "25"},
	{"Rihanna", "Stay", "Unapologetic"},
	{"Lady Gaga", "Shallow", "A Star Is Born Soundtrack"},
	{"Jeff Buckley", "Hallelujah", "Grace"},
	{"Lionel Richie", "Hello", "Can't Slow Down"},
	{"Coldplay", "Yellow", "Parachutes"},
	{"Queen", "Bohemian Rhapsody", "A Night at the Opera"},
	{"Ed Sheeran", "Shape of You", "÷"},
	{"Oasis", "Wonderwall", "(What's the Story) Morning Glory?"},
	{"Billie Eilish", "bad guy", "When We All Fall Asleep, Where Do We Go?"},
}

// GenerateLibrary writes a synthetic library tree under root:
// root/Artist NN/Album NN/Track NN.mp3, each tagged via GenerateID3v2. It
// returns the number of audio files written. It is used by the genlib tool and
// by load/concurrency tests; the tags are real enough for the scanner and
// dhowden/tag to parse.
func GenerateLibrary(root string, spec GenSpec) (int, error) {
	if spec.Realistic {
		return generateRealistic(root, spec)
	}
	if spec.Artists <= 0 || spec.Albums <= 0 || spec.Tracks <= 0 {
		return 0, fmt.Errorf("testutil: artists/albums/tracks must all be > 0")
	}
	count := 0
	for a := 1; a <= spec.Artists; a++ {
		artist := fmt.Sprintf("Artist %02d", a)
		for al := 1; al <= spec.Albums; al++ {
			album := fmt.Sprintf("Album %02d", al)
			dir := filepath.Join(root, artist, album)
			if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // test/tool fixture tree
				return count, fmt.Errorf("testutil: mkdir %s: %w", dir, err)
			}
			for tr := 1; tr <= spec.Tracks; tr++ {
				title := fmt.Sprintf("Track %02d", tr)
				stem := fmt.Sprintf("%02d - %s", tr, title)
				lyrics := ""
				if spec.EmbedLyrics {
					lyrics = fmt.Sprintf("%s by %s\nla la la\n", title, artist)
				}
				if err := WriteAudioFile(dir, stem+".mp3", artist, title, album, lyrics); err != nil {
					return count, err
				}
				count++
				if spec.LRCEvery > 0 && count%spec.LRCEvery == 0 {
					if err := WriteLRCFile(dir, stem+".lrc"); err != nil {
						return count, err
					}
				}
			}
		}
	}
	return count, nil
}

// generateRealistic writes one tagged file per RealisticTracks entry as
// root/<Artist>/<Album>/01 - <Title>.mp3, honoring EmbedLyrics and LRCEvery.
func generateRealistic(root string, spec GenSpec) (int, error) {
	count := 0
	for _, rt := range RealisticTracks {
		dir := filepath.Join(root, sanitize(rt.Artist), sanitize(rt.Album))
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // test/tool fixture tree
			return count, fmt.Errorf("testutil: mkdir %s: %w", dir, err)
		}
		stem := "01 - " + sanitize(rt.Title)
		lyrics := ""
		if spec.EmbedLyrics {
			lyrics = fmt.Sprintf("%s by %s\nla la la\n", rt.Title, rt.Artist)
		}
		if err := WriteAudioFile(dir, stem+".mp3", rt.Artist, rt.Title, rt.Album, lyrics); err != nil {
			return count, err
		}
		count++
		if spec.LRCEvery > 0 && count%spec.LRCEvery == 0 {
			if err := WriteLRCFile(dir, stem+".lrc"); err != nil {
				return count, err
			}
		}
	}
	return count, nil
}

// sanitize replaces characters that are invalid in a path component (on Windows
// as well as Unix) so a catalog title like "(What's the Story) Morning Glory?"
// yields a portable directory or file name. Trailing whitespace is trimmed
// because Windows rejects path components ending in a space.
func sanitize(s string) string {
	r := strings.NewReplacer(
		"/", "-",
		"\\", "-",
		":", "-",
		"*", "-",
		"|", "-",
		"?", "",
		"\"", "'",
		"<", "(",
		">", ")",
	)
	return strings.TrimSpace(r.Replace(s))
}
