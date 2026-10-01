package failsig

import "testing"

// The shape tables below use strings modeled on what the code emits (the
// producers were grepped: internal/musixmatch/client.go, petitlyrics and
// innertube "request: %w" wraps, worker write errors) with invented names; a few
// are paraphrases of OS or library text. They run through Normalize first: the
// classifier reads the normalized signature, so the test exercises the same
// pipeline a caller will.

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
		"musixmatch: unauthorized: HTTP 401 (token rejected or, per observed behavior, egress IP throttled)",
		"lane musixmatch: find lyrics: musixmatch: token renewal required",
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
		"musixmatch: matcher rejected the request (client error): inner status_code 404",
		"innertube: HTTP 403: forbidden",
		"innertube: HTTP 400: client version",
		"petitlyrics: application id revoked?",
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

// One input per marker, each carrying NO other marker, so deleting a marker
// reddens exactly its own row.
func TestClassifyEachMarkerAlone(t *testing.T) {
	for _, tc := range []struct{ name, sig string }{
		{"transport error", "lane a: transport error"},
		{"connection refused", "lane a: connection refused"},
		{"connection reset", "lane a: read: connection reset by peer"},
		{"connection timed out", "lane a: connection timed out"},
		{"broken pipe", "lane a: write: broken pipe"},
		{"unexpected eof", `lane a: Get "https://x.example/y": unexpected EOF`},
		{"dial tcp", "lane a: dial tcp <addr>"},
		{"proxyconnect", "lane a: proxyconnect tcp"},
		{"tls handshake", "lane a: tls handshake failure"},
		{"network is unreachable", "lane a: connect: network is unreachable"},
		{"no such host", "lane a: lookup x.example: no such host"},
		{"i/o timeout", "lane a: read: i/o timeout"},
		{"timeout", "lane a: timeout awaiting headers"},
		{"timed out", "lane a: timed out waiting"},
		{"operation timed out", "lane a: connect: operation timed out"},
		{"deadline exceeded", "lane a: rpc: deadline exceeded"},
		{"context deadline exceeded", "lane a: context deadline exceeded"},
		{"context canceled", "lane a: context canceled"},
		{"rate limited", "lane a: rate limited"},
		{"throttled", "lane a: throttled"},
		{"circuit open", "lane a: circuit open"},
		{"lane unavailable", "lane a: lane unavailable"},
		{"classifier unavailable", "detector: classifier unavailable"},
		{"temporarily unavailable", "lane a: temporarily unavailable"},
		{"server sent goaway", "lane a: http2: server sent GOAWAY and closed the connection"},
		{"database is locked", "queue: database is locked"},
		{"sqlite_busy", "queue: step failed (SQLITE_BUSY)"},
		{"host is down", "read /mnt/share/a.mp3: host is down"},
		{"stale nfs file handle", "read /mnt/share/a.mp3: stale NFS file handle"},
		{"eof (whole segment)", `petitlyrics: request: Post "https://x.example/y": EOF`},
		{"parenthesized", "lane a: lane (circuit open)"},
	} {
		if got := Classify(Normalize(tc.sig)); got != Transient {
			t.Errorf("%s: Classify(%q) = %s, want transient", tc.name, tc.sig, got)
		}
	}
}

// Text the writer does not control must not fake a marker. Names are invented.
func TestClassifyMarkersCannotBeFaked(t *testing.T) {
	for _, sig := range []string{
		"read /mnt/Timeout Band permission denied",
		"worker: write item 9 output /mnt/Throttled Records/a.lrc: permission denied",
		"lyrics: nothing to save for Timeout - Some Song",
		"lyrics: nothing to save for Rate Limited - Connection Refused",
		"lyrics: nothing to save for Broken Pipe Orchestra - Eof",
	} {
		if got := Classify(Normalize(sig)); got != Persistent {
			t.Errorf("Classify(%q) = %s, want persistent (marker faked by a name or path)", sig, got)
		}
	}
}

// A non-decimal exit code is consumed whole, so an HTTP status after it is
// still read.
func TestClassifyExitStatusThenHTTPStatus(t *testing.T) {
	if got := Classify("worker: ffmpeg: exit status 0xC0000005, then HTTP 503"); got != Transient {
		t.Errorf("hex exit status then HTTP 503 = %s, want transient", got)
	}
	if got := Classify("worker: ffmpeg: exit status 0xC0000005, then HTTP 403"); got != Persistent {
		t.Errorf("hex exit status then HTTP 403 = %s, want persistent", got)
	}
}
