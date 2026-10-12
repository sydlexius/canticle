package scanner_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/audiometa"
	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/scanner"
)

// The contract the scanner identity slice (#1574) builds on: for a file whose
// tags were recorded into audio_metadata, audiometa.Identity returns exactly
// the values the scan loop itself puts on the scan row for that file. If the
// two disagreed, serving a row from the cache would re-key scan_results and
// work_queue (artist + title is their unique key).
//
// Every fixture carries a MULTI-VALUE artist and album artist, the case whose
// extraction changed after cache rows already existed. The scan runs with
// recording enrichment on so ISRC, recording ID and duration are compared too.
func TestIdentityCacheMatchesScanRow(t *testing.T) {
	const (
		isrc = "ZZXXX2600001"
		mbid = "11111111-2222-4333-8444-555555555555"
	)
	vorbis := []string{
		"ARTIST=Marlow Finch", "ARTIST=Odile Varga",
		"ALBUMARTIST=Marlow Finch", "ALBUMARTIST=The Tin Larks",
		"TITLE=Quiet Harbor", "ALBUM=Low Tide",
		"ISRC=" + isrc, "MUSICBRAINZ_TRACKID=" + mbid,
	}
	for _, tc := range []struct {
		name, file string
		data       []byte
	}{
		{"flac", "track.flac", scanner.BuildFLACWithRawComments(t, vorbis)},
		{"ogg", "track.ogg", scanner.BuildOggVorbisWithRawComments(t, vorbis)},
		{"mp4", "track.m4a", scanner.BuildM4A(
			scanner.TagAtom("\xa9ART", "Marlow Finch", "Odile Varga"),
			scanner.TagAtom("aART", "Marlow Finch", "The Tin Larks"),
			scanner.TagAtom("\xa9nam", "Quiet Harbor"),
			scanner.TagAtom("\xa9alb", "Low Tide"),
			scanner.FreeformAtom("ISRC", isrc),
			scanner.FreeformAtom("MusicBrainz Track Id", mbid),
		)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			path := filepath.Join(dir, tc.file)
			if err := os.WriteFile(path, tc.data, 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}

			// The scanner's own row for the file.
			rows, err := scanner.NewScanner().ScanLibrary(ctx, dir, scanner.ScanOptions{MaxDepth: 1, EnrichRecording: true})
			if err != nil || len(rows) != 1 {
				t.Fatalf("ScanLibrary = %d row(s), err %v; want 1", len(rows), err)
			}
			tr := rows[0].Track
			want := audiometa.Identity{
				Artist: tr.ArtistName, AlbumArtist: tr.AlbumArtist, Title: tr.TrackName, Album: tr.AlbumName,
				ISRC: tr.ISRC, MBID: tr.RecordingMBID, TrackLength: tr.TrackLength,
			}
			// Guard the fixture: a comparison of two empty structs proves nothing.
			// ISRC is matched by substring because this test pins agreement, not
			// the ISRC extraction itself (the MP4 freeform value is not bare).
			if !strings.Contains(want.Artist, "; ") || !strings.Contains(want.AlbumArtist, "; ") ||
				want.Title == "" || want.Album == "" || !strings.Contains(want.ISRC, isrc) || want.MBID != mbid {
				t.Fatalf("fixture did not scan as a fully tagged multi-value file: %+v", want)
			}
			// DURATION IS COVERED BY THE FLAC FIXTURE ONLY. The Ogg and M4A
			// fixtures have no parseable duration, so TrackLength is 0 on both
			// sides for them and the comparison below says nothing about it.
			// Fail loudly if the FLAC fixture ever loses its duration too.
			if tc.name == "flac" && want.TrackLength <= 0 {
				t.Fatalf("flac fixture scanned with no duration (TrackLength %d); the duration comparison would be vacuous", want.TrackLength)
			}

			// The cache write `scan index-metadata` performs, then the identity read.
			facts, err := scanner.ReadAudioFacts(path)
			if err != nil {
				t.Fatalf("ReadAudioFacts: %v", err)
			}
			store := audiometa.New(dbtest.Open(t), scanner.DurationReaderVersion)
			if err := store.Record(ctx, path, facts); err != nil {
				t.Fatalf("Record: %v", err)
			}
			got, found, err := store.Identity(ctx, path, facts.MTimeNano, facts.SizeBytes)
			if err != nil || !found {
				t.Fatalf("Identity = found %v, err %v; want a hit", found, err)
			}
			if got != want {
				t.Errorf("cached identity differs from the scan row:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}
