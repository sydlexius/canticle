package lyricblock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
)

func dirState(t *testing.T, dir string) (names []string, settled string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		names = append(names, e.Name())
	}
	b, _ := os.ReadFile(filepath.Join(dir, "track.txt"))
	return names, string(b)
}

// WriteLRC refuses a blocked result before any mutation, whatever the
// provider's returned artist/title say; a different body and an unstamped song
// still write.
func TestWriteLRC_RefusesBlockedResultBeforeMutation(t *testing.T) {
	ctx := context.Background()
	s, d, _ := newStore(t)
	dir := t.TempDir()
	w := lyrics.NewLRCWriter()
	w.SetBlockChecker(s)

	wrong := models.Song{Track: models.Track{ArtistName: "Provider Spelling", TrackName: "Provider Title"},
		Lyrics: models.Lyrics{LyricsBody: "wrong words\nmore wrong words\n"}}
	wrong.IdentityArtistKey, wrong.IdentityTitleKey = normalize.NormalizeKey("Query Artist"), normalize.NormalizeKey("Query Title")
	if _, err := s.Add(ctx, d, Block{ArtistKey: "Query Artist", TitleKey: "Query Title", Fingerprint: SongFingerprints(wrong)[0]}); err != nil {
		t.Fatal(err)
	}
	// A settled .txt is already on disk; the refused write must leave it as is.
	if err := os.WriteFile(filepath.Join(dir, "track.txt"), []byte("settled words\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	namesBefore, settledBefore := dirState(t, dir)

	if err := w.WriteLRC(wrong, "track.lrc", dir); !errors.Is(err, lyrics.ErrBlocked) {
		t.Fatalf("WriteLRC = %v; want ErrBlocked", err)
	}
	if err := w.WriteLRCNoDowngrade(wrong, "track.lrc", dir); !errors.Is(err, lyrics.ErrBlocked) {
		t.Fatalf("WriteLRCNoDowngrade = %v; want ErrBlocked", err)
	}
	if names, settled := dirState(t, dir); !slices.Equal(names, namesBefore) || settled != settledBefore {
		t.Fatalf("disk changed by a refused write: %v %q -> %v %q", namesBefore, settledBefore, names, settled)
	}

	other := wrong
	other.Lyrics.LyricsBody = "entirely different words\n"
	if err := w.WriteLRC(other, "other.lrc", dir); err != nil {
		t.Fatalf("a different body for the same track must write: %v", err)
	}
	unstamped := wrong
	unstamped.IdentityArtistKey, unstamped.IdentityTitleKey = "", ""
	if err := w.WriteLRC(unstamped, "unstamped.lrc", dir); err != nil {
		t.Fatalf("an unstamped song is never blocked: %v", err)
	}
	if err := lyrics.NewLRCWriter().WriteLRC(wrong, "nilcheck.lrc", dir); err != nil {
		t.Fatalf("a writer with no checker never blocks: %v", err)
	}
}

// A NUL inside a tag must not break the identity (#1394): the two keys travel
// as separate fields, so there is no separator to confuse.
func TestSongBlocked_NulInsideATagStillBlocks(t *testing.T) {
	ctx := context.Background()
	s, d, _ := newStore(t)
	artist, title := normalize.NormalizeKey("Art\x00ist"), normalize.NormalizeKey("Ti\x00tle")
	song := models.Song{Lyrics: models.Lyrics{LyricsBody: "wrong words\nmore wrong words\n"}, IdentityArtistKey: artist, IdentityTitleKey: title}
	if _, err := s.Add(ctx, d, Block{ArtistKey: artist, TitleKey: title, Fingerprint: SongFingerprints(song)[0]}); err != nil {
		t.Fatal(err)
	}
	if !s.SongBlocked(ctx, song) {
		t.Fatal("a NUL in the tags made the track silently unblockable")
	}
}
