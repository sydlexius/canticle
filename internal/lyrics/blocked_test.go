package lyrics

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

type fakeBlockChecker struct {
	blocked bool
	seen    []models.Song
}

func (f *fakeBlockChecker) SongBlocked(_ context.Context, song models.Song) bool {
	f.seen = append(f.seen, song)
	return f.blocked
}

func TestStampBlockIdentity(t *testing.T) {
	song := models.Song{Track: models.Track{ArtistName: "A", TrackName: "T"}}

	got := StampBlockIdentity(WithBlockIdentity(context.Background(), "akey", "tkey"), song)
	if got.IdentityArtistKey != "akey" || got.IdentityTitleKey != "tkey" {
		t.Fatalf("stamped keys = %q/%q; want akey/tkey", got.IdentityArtistKey, got.IdentityTitleKey)
	}
	if got.Track != song.Track {
		t.Fatalf("stamping changed the track: %+v", got.Track)
	}

	bare := StampBlockIdentity(context.Background(), song)
	if bare.IdentityArtistKey != "" || bare.IdentityTitleKey != "" {
		t.Fatalf("ctx without identity stamped keys %q/%q; want none", bare.IdentityArtistKey, bare.IdentityTitleKey)
	}
}

func TestWriteLRC_BlockBackstop(t *testing.T) {
	const old = "[00:01.00]previous settled words\n"
	song := models.Song{
		Track:             models.Track{ArtistName: "Test Artist", TrackName: "Blocked Track"},
		Subtitles:         models.Synced{Lines: []models.Lines{{Text: "wrong words", Time: models.Time{Seconds: 5}}}},
		IdentityArtistKey: "akey",
		IdentityTitleKey:  "tkey",
	}

	for _, tc := range []struct {
		name      string
		checker   *fakeBlockChecker // nil: no checker installed
		wantErr   error
		wantWrite bool
	}{
		{"blocked", &fakeBlockChecker{blocked: true}, ErrBlocked, false},
		{"not blocked", &fakeBlockChecker{}, nil, true},
		{"no checker", nil, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			fp := filepath.Join(dir, Slugify("Test Artist - Blocked Track")+".lrc")
			if err := os.WriteFile(fp, []byte(old), 0o600); err != nil {
				t.Fatal(err)
			}
			w := NewLRCWriter()
			if tc.checker != nil {
				w.SetBlockChecker(tc.checker)
			}

			err := w.WriteLRC(song, "", dir)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("WriteLRC err = %v; want %v", err, tc.wantErr)
			}
			data, rerr := os.ReadFile(fp) //nolint:gosec // reason: test path under t.TempDir
			if rerr != nil {
				t.Fatal(rerr)
			}
			if wrote := string(data) != old; wrote != tc.wantWrite {
				t.Fatalf("sidecar rewritten = %v; want %v (content %q)", wrote, tc.wantWrite, data)
			}
			if tc.checker != nil {
				if len(tc.checker.seen) != 1 || tc.checker.seen[0].IdentityArtistKey != "akey" || tc.checker.seen[0].IdentityTitleKey != "tkey" {
					t.Fatalf("checker saw %+v; want one call carrying the song's identity keys", tc.checker.seen)
				}
			}
		})
	}
}
