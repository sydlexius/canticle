package musixmatch

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// Fixtures here are ENTIRELY SYNTHETIC: placeholder words (alpha/bravo/...),
// round dummy timestamps, and no identifiers of any kind. A richsync body IS
// the lyric, so nothing resembling real lyric text, a real artist or track
// name, or a real commontrack_id may ever enter this repo.

// richSyncMacroBody builds a full macro.subtitles.get response whose
// track.richsync.get sub-call is spelled by the caller, so one builder serves
// the present/absent/non-200/empty/unparsable cases without drifting apart in
// the FIVE other sub-calls, which are what make the response a valid
// line-synced result in the first place.
//
// The two cues sit at 1.00s and 3.00s, far enough apart that the parser's
// 300 ms bind tolerance cannot confuse them.
func richSyncMacroBody(richSyncCall string) string {
	body := `{
		"message": {
			"header": {"status_code": 200},
			"body": {
				"macro_calls": {
					"matcher.track.get": {
						"message": {
							"header": {"status_code": 200},
							"body": {
								"track": {
									"track_name": "title",
									"artist_name": "artist",
									"album_name": "album",
									"has_subtitles": 1,
									"has_lyrics": 1
								}
							}
						}
					},
					"track.lyrics.get": {"message": {"body": {}}},
					"track.subtitles.get": {
						"message": {
							"body": {
								"subtitle_list": [
									{
										"subtitle": {
											"subtitle_body": "[{\"text\":\"alpha bravo\",\"time\":{\"total\":1.0,\"minutes\":0,\"seconds\":1,\"hundredths\":0}},{\"text\":\"charlie delta\",\"time\":{\"total\":3.0,\"minutes\":0,\"seconds\":3,\"hundredths\":0}}]"
										}
									}
								]
							}
						}
					}` + richSyncCall + `
				}
			}
		}
	}`
	return body
}

// richSyncCall wraps a richsync_body (already JSON-string-escaped) in the
// sub-call envelope at the inner status code given.
func richSyncCall(status int, escapedBody string) string {
	return `,
					"track.richsync.get": {
						"message": {
							"header": {"status_code": ` + strconv.Itoa(status) + `},
							"body": {"richsync": {"richsync_body": "` + escapedBody + `"}}
						}
					}`
}

// validRichSyncBody is the doubly-encoded payload the provider sends: a JSON
// string whose CONTENTS are a JSON array. Two entries at ts 1.0 and 3.0,
// matching the two cues above, so both bind.
const validRichSyncBody = `[{\"ts\":1.0,\"te\":2.0,\"x\":\"alpha bravo\",\"l\":[{\"c\":\"alpha\",\"o\":0.0},{\"c\":\" bravo\",\"o\":0.5}]},` +
	`{\"ts\":3.0,\"te\":4.0,\"x\":\"charlie delta\",\"l\":[{\"c\":\"charlie\",\"o\":0.0},{\"c\":\" delta\",\"o\":0.4}]}]`

func newCountingClient(t *testing.T, body string) (*Client, *countingRoundTripper) {
	t.Helper()
	crt := &countingRoundTripper{fn: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, body), nil
	}}
	client := NewClient("test-token")
	client.httpClient = &http.Client{Transport: crt}
	return client, crt
}

func probeTrack() models.Track {
	return models.Track{TrackName: "title", ArtistName: "artist"}
}

// TestFindLyricsRequestsRichSyncAsAnOptionalSubCall pins the two parameters
// that make the richsync sub-call ride the request canticle already issues.
// Without them the provider returns five macro calls and no word data at all.
func TestFindLyricsRequestsRichSyncAsAnOptionalSubCall(t *testing.T) {
	var gotOptional, gotCompact string
	client := NewClient("test-token")
	client.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotOptional = req.URL.Query().Get("optional_calls")
		gotCompact = req.URL.Query().Get("richsync_compact_type")
		return jsonResponse(http.StatusOK, richSyncMacroBody(richSyncCall(200, validRichSyncBody))), nil
	})}

	if _, err := client.FindLyrics(context.Background(), probeTrack()); err != nil {
		t.Fatalf("FindLyrics returned %v; want nil", err)
	}
	if gotOptional != "track.richsync" {
		t.Errorf("optional_calls = %q; want track.richsync", gotOptional)
	}
	if gotCompact != "words" {
		t.Errorf("richsync_compact_type = %q; want words", gotCompact)
	}
}

// TestRichSyncCostsNoExtraRequest is the property that makes this whole slice
// cheap, and it is the one most likely to be lost to a well-meaning refactor
// that "just fetches the richsync endpoint too". Richsync is a bundled
// sub-call, so exactly ONE round trip may leave the client, and a second would
// also mean a second paced request against a rate-limited provider.
func TestRichSyncCostsNoExtraRequest(t *testing.T) {
	client, crt := newCountingClient(t, richSyncMacroBody(richSyncCall(200, validRichSyncBody)))

	song, err := client.FindLyrics(context.Background(), probeTrack())
	if err != nil {
		t.Fatalf("FindLyrics returned %v; want nil", err)
	}
	if len(song.WordTimings) == 0 {
		t.Fatal("no word timings; the fixture is not exercising the richsync path this test is counting requests for")
	}
	if crt.calls != 1 {
		t.Fatalf("outbound requests = %d; want exactly 1 (richsync is a bundled sub-call, never a second call)", crt.calls)
	}
}

// TestRichSyncPopulatesWordTimingsCorrelatedToCues asserts the parser's output
// actually lands on song.WordTimings AND that each timing points at the cue
// whose timestamp it matches -- not merely that the slice is non-empty, which
// an index-paired implementation would also satisfy.
func TestRichSyncPopulatesWordTimingsCorrelatedToCues(t *testing.T) {
	client, _ := newCountingClient(t, richSyncMacroBody(richSyncCall(200, validRichSyncBody)))

	song, err := client.FindLyrics(context.Background(), probeTrack())
	if err != nil {
		t.Fatalf("FindLyrics returned %v; want nil", err)
	}
	if len(song.Subtitles.Lines) != 2 {
		t.Fatalf("cues = %d; want 2", len(song.Subtitles.Lines))
	}
	if len(song.WordTimings) != 4 {
		t.Fatalf("word timings = %d; want 4 (two chunks on each of two entries)", len(song.WordTimings))
	}

	want := []models.WordTiming{
		{Line: 0, Text: "alpha", StartMS: 1000, EndMS: 1500},
		{Line: 0, Text: " bravo", StartMS: 1500, EndMS: 2000},
		{Line: 1, Text: "charlie", StartMS: 3000, EndMS: 3400},
		{Line: 1, Text: " delta", StartMS: 3400, EndMS: 4000},
	}
	for i, w := range want {
		if song.WordTimings[i] != w {
			t.Errorf("WordTimings[%d] = %+v; want %+v", i, song.WordTimings[i], w)
		}
	}
}

// TestRichSyncFailuresNeverCostTheLineSyncedSong is the hard constraint: the
// primary result is already good, so EVERY richsync failure mode degrades to
// "no word timings" rather than failing the lookup. A regression here does not
// lose word markers -- it loses the synced lyric entirely, for every track
// whose richsync payload is missing or malformed, which is most of them.
func TestRichSyncFailuresNeverCostTheLineSyncedSong(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			// The ordinary case for a track with no word data at all: the
			// provider simply omits the optional sub-call.
			name: "sub-call absent",
			body: richSyncMacroBody(""),
		},
		{
			// A non-200 inner status carrying a VALID body, so only the status
			// check can reject it. With an empty body here the next guard would
			// catch it instead and the status check would be untested.
			name: "inner status 404",
			body: richSyncMacroBody(richSyncCall(404, validRichSyncBody)),
		},
		{
			name: "empty richsync_body",
			body: richSyncMacroBody(richSyncCall(200, "")),
		},
		{
			// Well-formed JSON at BOTH decode levels whose keys are not the
			// ones this parser reads -- the payload-shape change the sentinel
			// exists to name, and the only case that reaches the parser's
			// error path at all.
			name: "unparsable richsync_body",
			body: richSyncMacroBody(richSyncCall(200, `[{\"nope\":1}]`)),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, crt := newCountingClient(t, tc.body)

			song, err := client.FindLyrics(context.Background(), probeTrack())
			if err != nil {
				t.Fatalf("FindLyrics returned %v; want nil -- an optional richsync failure must never cost the line-synced result", err)
			}
			if len(song.Subtitles.Lines) == 0 {
				t.Fatal("no cues; the line-synced result was lost to a richsync failure")
			}
			if song.WordTimings != nil {
				t.Errorf("WordTimings = %+v; want nil", song.WordTimings)
			}
			if crt.calls != 1 {
				t.Errorf("outbound requests = %d; want exactly 1", crt.calls)
			}
			// The parser's sentinel is package-internal by design: the
			// orchestrator's sentinel enumeration carries an exemption
			// asserting it never reaches a lane, and that exemption is only
			// true while this holds.
			if errors.Is(err, ErrUnparsableRichSyncBody) {
				t.Error("ErrUnparsableRichSyncBody escaped findLyricsOnce; the orchestrator exemption assumes it cannot")
			}
			// And nothing here may be reclassified as a benign miss either:
			// that would defer a track whose lyrics were successfully fetched.
			if IsBenignMiss(err) {
				t.Error("IsBenignMiss(err) = true on a SUCCESSFUL lookup; a richsync failure must not change the lookup's classification")
			}
		})
	}
}

// TestRichSyncIgnoredWithoutLineSync pins the placement: the parse is nested
// inside the HasSubtitles branch, so an unsynced-lyrics result never carries
// word timings even if a richsync sub-call somehow rides along. Word timings
// index into cues, so timings without cues are unanchored data.
func TestRichSyncIgnoredWithoutLineSync(t *testing.T) {
	body := `{"message":{"header":{"status_code":200},"body":{"macro_calls":{
		"matcher.track.get":{"message":{"header":{"status_code":200},"body":{"track":{
			"track_name":"title","artist_name":"artist","has_subtitles":0,"has_lyrics":1}}}},
		"track.lyrics.get":{"message":{"body":{"lyrics":{"lyrics_body":"alpha bravo"}}}},
		"track.subtitles.get":{"message":{"body":{}}},
		"track.richsync.get":{"message":{"header":{"status_code":200},"body":{"richsync":{"richsync_body":"` + validRichSyncBody + `"}}}}
	}}}}`

	client, _ := newCountingClient(t, body)
	song, err := client.FindLyrics(context.Background(), probeTrack())
	if err != nil {
		t.Fatalf("FindLyrics returned %v; want nil", err)
	}
	if song.WordTimings != nil {
		t.Errorf("WordTimings = %+v; want nil on a result with no cues to index into", song.WordTimings)
	}
}
