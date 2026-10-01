package failsig

import "testing"

// Every input below is a raw last_error shape produced by the code today (or
// quoted in failsig_test.go), run through Normalize first: the classifier's
// contract is that it reads the normalized signature, so the test exercises the
// same pipeline a caller will.

func TestClassifyTransientShapes(t *testing.T) {
	for _, raw := range []string{
		"lane musixmatch: find lyrics: musixmatch: unexpected matcher status_code 500",
		"musixmatch API error: status 503, body: upstream down",
		"petitlyrics: unexpected HTTP status 502",
		"innertube: HTTP 429: rate limited",
		"petitlyrics: HTTP 429: rate limited",
		"musixmatch: token endpoint HTTP 429",
		"verification: transcribe status 504: gateway timeout",
		"lane musixmatch: find lyrics: musixmatch: transport error: proxyconnect tcp: connection refused",
		"detector: classifier unavailable: Post \"http://yamnet:8080/classify\": dial tcp 172.22.0.2:8080: connect: connection refused",
		"Get \"https://x.example/y\": context deadline exceeded",
		"worker: sample: context canceled",
		"dial tcp: lookup api.example on 10.0.0.1:53: no such host",
		"read tcp 10.0.0.5:51234->1.2.3.4:443: i/o timeout",
		"musixmatch: token mint refused (rate limited)",
		"orchestrator: lane unavailable (circuit open)",
		"musixmatch: inner status_code 408",
	} {
		if got := Classify(Normalize(raw)); got != Transient {
			t.Errorf("Classify(%q) = %s, want transient", raw, got)
		}
	}
}

func TestClassifyPersistentShapes(t *testing.T) {
	for _, raw := range []string{
		`worker: write item 77508 output /Share/Music/Artist (27)/Album One/07. Track.lrc: refusing to write: output dir "/Share/Music/Artist (27)/Album One" does not exist`,
		"read /mnt/a permission denied",
		"worker: write item 5 output /mnt/a/b.lrc: open /mnt/a/b.lrc.tmp: read-only file system",
		"worker: write item 5 output /mnt/a/b.lrc: no space left on device",
		"musixmatch: matcher HTTP 401: token rejected",
		"innertube: HTTP 403: forbidden",
		"innertube: HTTP 400: client version",
		"musixmatch: ErrMatcherClientError: inner status_code 404",
		"musixmatch API error: status 400, body: bad",
		"detector: audio file is missing",
		"detector: sample audio with ffmpeg: exit status 69: [mp3float @ 0x14e1cf868a80] Header missing",
		"worker: ffmpeg: exit status 0xC0000005",
		"some brand new failure nobody has seen",
		"unknown",
		"",
	} {
		if got := Classify(Normalize(raw)); got != Persistent {
			t.Errorf("Classify(%q) = %s, want persistent", raw, got)
		}
	}
}

// An ffmpeg exit status is shaped like an HTTP status and must never be read as
// one: "exit status 503" is a process code, not a provider outage.
func TestClassifyExitStatusIsNotHTTP(t *testing.T) {
	if got := Classify("detector: ffmpeg: exit status 503"); got != Persistent {
		t.Errorf("exit status 503 read as an HTTP status: %s", got)
	}
}

// The status decides before the text does: a 4xx whose body mentions a transient
// word stays Persistent, so a bad request is never filed under "will retry".
func TestClassifyStatusBeatsMarker(t *testing.T) {
	if got := Classify("musixmatch API error: status 400, body: request timeout in payload"); got != Persistent {
		t.Errorf("4xx with a timeout word in its body = %s, want persistent", got)
	}
}

// A 2xx/3xx number carries no verdict, so the text rules decide.
func TestClassifyNonErrorStatusFallsThrough(t *testing.T) {
	if got := Classify("musixmatch: status_code 200 but response missing track data"); got != Persistent {
		t.Errorf("200 with no marker = %s, want persistent", got)
	}
	if got := Classify("musixmatch: status_code 200: transport error: eof timeout"); got != Transient {
		t.Errorf("200 with a transport marker = %s, want transient", got)
	}
}

func TestClassString(t *testing.T) {
	if Transient.String() != "transient" || Persistent.String() != "persistent" {
		t.Errorf("class names changed: %q %q", Transient, Persistent)
	}
}
