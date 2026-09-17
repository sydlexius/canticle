package musixmatch

import (
	"context"
	"net/http"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// matcherBodyWithTrack wraps a synthetic matcher.track.get track node in the
// full macro envelope FindLyrics parses, so these tests drive the SAME
// json.Unmarshal of the whole track node that production uses rather than a
// hand-rolled unmarshal that would prove only that Go can decode a struct.
//
// Everything in it is synthetic: placeholder words, a round dummy id, a round
// dummy timestamp. No provider content, no real catalog identifiers. Kept
// inline rather than in testdata/ because the whole point is to exercise the
// client's HTTP path via roundTripFunc, which is how every other test in this
// package supplies a response body.
func matcherBodyWithTrack(trackNode string) string {
	return `{
		"message": {
			"header": {"status_code": 200},
			"body": {
				"macro_calls": {
					"matcher.track.get": {
						"message": {
							"header": {"status_code": 200},
							"body": {"track": ` + trackNode + `}
						}
					},
					"track.lyrics.get": {"message": {"body": {}}},
					"track.subtitles.get": {
						"message": {
							"body": {
								"subtitle_list": [
									{
										"subtitle": {
											"subtitle_body": "[{\"text\":\"alpha bravo charlie\",\"time\":{\"total\":1.00,\"minutes\":0,\"seconds\":1,\"hundredths\":0}}]"
										}
									}
								]
							}
						}
					}
				}
			}
		}
	}`
}

func findAlphaBravo(t *testing.T, body string) (models.Song, error) {
	t.Helper()
	client := NewClient("test-token")
	client.httpClient = &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, body), nil
	})}
	return client.FindLyrics(context.Background(), models.Track{
		TrackName:  "alpha",
		ArtistName: "bravo",
		AlbumName:  "charlie",
	})
}

// TestFindLyricsPopulatesCommontrackID pins the assumption #613 rests on: the
// commontrack_id that keys track.richsync.get arrives on models.Track with NO
// call-site change, because findLyricsOnce unmarshals the entire matcher track
// node into song.Track. If that bulk unmarshal is ever replaced by explicit
// per-field reads, this fails here instead of the richsync lane silently going
// dark once it is built.
//
// BOTH JSON encodings are driven. That is not defensive padding: the same
// unmarshal call's error return fails the WHOLE lookup, so a field that decodes
// only one of the two shapes would break every matched track over an identifier
// nothing consumes yet. models.FlexID exists for exactly this, and these two
// cases are its production-path proof.
func TestFindLyricsPopulatesCommontrackID(t *testing.T) {
	cases := []struct {
		name string
		node string
		want models.FlexID
	}{
		{
			name: "numeric, as the provider serves it",
			node: `{"track_name":"alpha","artist_name":"bravo","album_name":"charlie","commontrack_id":10000001,"has_subtitles":1,"has_lyrics":1}`,
			want: "10000001",
		},
		{
			name: "string, tolerated without failing the lookup",
			node: `{"track_name":"alpha","artist_name":"bravo","album_name":"charlie","commontrack_id":"10000001","has_subtitles":1,"has_lyrics":1}`,
			want: "10000001",
		},
		{
			name: "absent, leaves the field empty",
			node: `{"track_name":"alpha","artist_name":"bravo","album_name":"charlie","has_subtitles":1,"has_lyrics":1}`,
			want: "",
		},
		{
			name: "null, leaves the field empty",
			node: `{"track_name":"alpha","artist_name":"bravo","album_name":"charlie","commontrack_id":null,"has_subtitles":1,"has_lyrics":1}`,
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			song, err := findAlphaBravo(t, matcherBodyWithTrack(tc.node))
			if err != nil {
				t.Fatalf("FindLyrics: %v", err)
			}
			if song.Track.CommontrackID != tc.want {
				t.Errorf("CommontrackID = %q; want %q", song.Track.CommontrackID, tc.want)
			}
			// The surrounding result must be untouched in every case: slice A
			// is behavior-free, and an identifier must never cost a caller its
			// line-synced lyrics.
			if len(song.Subtitles.Lines) != 1 {
				t.Errorf("subtitle lines = %d; want 1", len(song.Subtitles.Lines))
			}
		})
	}
}

// TestFindLyricsUnexpectedCommontrackIDShapeDoesNotFailTheLookup covers the
// shape nobody predicted. A wholesale unmarshal turns a surprising encoding of
// an OPTIONAL identifier into a failed lookup for the whole track; FlexID
// swallows it to an empty value instead. Asserting the lyrics still arrive is
// the point -- the empty id is merely the side effect.
func TestFindLyricsUnexpectedCommontrackIDShapeDoesNotFailTheLookup(t *testing.T) {
	song, err := findAlphaBravo(t, matcherBodyWithTrack(
		`{"track_name":"alpha","artist_name":"bravo","album_name":"charlie","commontrack_id":{"nested":true},"has_subtitles":1,"has_lyrics":1}`))
	if err != nil {
		t.Fatalf("FindLyrics: %v", err)
	}
	if song.Track.CommontrackID != "" {
		t.Errorf("CommontrackID = %q; want empty", song.Track.CommontrackID)
	}
	if len(song.Subtitles.Lines) != 1 {
		t.Fatalf("subtitle lines = %d; want 1", len(song.Subtitles.Lines))
	}
}
