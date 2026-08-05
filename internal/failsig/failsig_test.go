package failsig

import (
	"strings"
	"testing"
)

// The five write failures on the reference install differ ONLY by item id and
// path, so the report showed five "categories" for one root cause -- and each
// carried an absolute library path onto a Prometheus label an external scraper
// retains off-host (#478).
//
// Both properties are asserted together because they are the same fix: the
// variable text that defeats grouping IS the private metadata that must not
// leak.
func TestNormalizeCollapsesWriteFailures(t *testing.T) {
	in := []string{
		`worker: write item 77508 output /Share/Music/Artist (27)/Album One/07. Track.lrc: refusing to write: output dir "/Share/Music/Artist (27)/Album One" does not exist`,
		`worker: write item 77485 output /Share/Music/Artist (27)/Other/05. Different.lrc: refusing to write: output dir "/Share/Music/Artist (27)/Other" does not exist`,
		`worker: write item 77542 output /Share/Music/Other Artist/The 6th/06. Song.lrc: refusing to write: output dir "/Share/Music/Other Artist/The 6th" does not exist`,
	}

	got := make(map[string]bool)
	for _, s := range in {
		n := Normalize(s)
		got[n] = true
		if strings.Contains(n, "/Share/") {
			t.Errorf("normalized signature still carries a library path:\n%s", n)
		}
		if strings.Contains(n, "77508") || strings.Contains(n, "77485") || strings.Contains(n, "77542") {
			t.Errorf("normalized signature still carries an item id:\n%s", n)
		}
	}
	if len(got) != 1 {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		t.Errorf("one root cause produced %d groups, want 1:\n%s", len(got), strings.Join(keys, "\n"))
	}
}

// Transport failures embed an ephemeral source port and a container IP, so two
// instances of one outage never collapse.
func TestNormalizeCollapsesTransportFailures(t *testing.T) {
	a := Normalize(`lane musixmatch: find lyrics: musixmatch: transport error: proxyconnect tcp: dial tcp: lookup proxy-host on 127.0.0.11:53: read udp 127.0.0.1:56723->127.0.0.11:53: i/o timeout`)
	b := Normalize(`lane musixmatch: find lyrics: musixmatch: transport error: proxyconnect tcp: dial tcp: lookup proxy-host on 127.0.0.11:53: read udp 127.0.0.1:47269->127.0.0.11:53: i/o timeout`)
	if a != b {
		t.Errorf("two instances of one transport outage did not collapse:\n%s\n%s", a, b)
	}
	if strings.Contains(a, "56723") {
		t.Errorf("normalized signature retains an ephemeral port:\n%s", a)
	}
}

// ffmpeg stamps an allocator address into its decoder diagnostics, and that
// address differs on every run -- so two occurrences of ONE corrupt-file failure
// landed in two groups. Found by running the normalizer over the real production
// signatures rather than over fixtures, which is the only reason it surfaced.
func TestNormalizeCollapsesHexAddresses(t *testing.T) {
	a := Normalize("detector: sample audio with ffmpeg: exit status 69: [mp3float @ 0x14e1cf868a80] Header missing")
	b := Normalize("detector: sample audio with ffmpeg: exit status 69: [mp3float @ 0x7f2a11b04220] Header missing")
	if a != b {
		t.Errorf("one ffmpeg failure produced two groups; the allocator address varies per run:\n%s\n%s", a, b)
	}
	if strings.Contains(a, "0x14e1") {
		t.Errorf("normalized signature retains a memory address:\n%s", a)
	}
	// The exit status is the actionable part and must survive.
	if !strings.Contains(a, "exit status 69") {
		t.Errorf("the exit status was normalized away; it is the signal:\n%s", a)
	}
}

// A DISTINCTION THAT MUST SURVIVE. Over-normalizing is the failure mode that
// would make this change harmful rather than merely useless: collapsing a 500
// into a 400 hides a permanent misconfiguration inside a transient-outage group.
// The status code is the signal, not noise.
func TestNormalizeKeepsDistinctStatusCodes(t *testing.T) {
	a := Normalize("lane musixmatch: find lyrics: musixmatch: unexpected matcher status_code 500")
	b := Normalize("lane musixmatch: find lyrics: musixmatch: unexpected matcher status_code 400")
	if a == b {
		t.Errorf("a 500 and a 400 collapsed into one group; the status code is the actionable signal:\n%s", a)
	}
}

// Two genuinely different root causes must stay apart even though both are
// "musixmatch failed".
func TestNormalizeKeepsDistinctRootCauses(t *testing.T) {
	a := Normalize("lane musixmatch: find lyrics: musixmatch: unexpected matcher status_code 500")
	b := Normalize("lane musixmatch: find lyrics: musixmatch: transport error: proxyconnect tcp: connection refused")
	if a == b {
		t.Errorf("a provider 5xx and a transport failure collapsed:\n%s", a)
	}
}

// The benign-miss signatures already group perfectly (13 groups over 20,124 rows
// on the reference install). Normalization must not disturb them -- a change
// that "improves" the working case is a regression in disguise.
func TestNormalizeLeavesCleanSignaturesAlone(t *testing.T) {
	for _, s := range []string{
		"orchestrator: lane benign miss (no result)",
		"musixmatch: no lyrics available: no synced or unsynced lyrics",
		"musixmatch: no results found",
		"musixmatch: truncated or empty response body: subtitle_body empty despite HasSubtitles=1",
		"unknown",
	} {
		if got := Normalize(s); got != s {
			t.Errorf("a clean signature was altered:\n got %q\nwant %q", got, s)
		}
	}
}

// A normalized signature is a GROUPING KEY, so it must be stable: the same input
// always yields the same output, and normalizing twice changes nothing. Without
// idempotence a re-normalized value could drift into its own group.
func TestNormalizeIsIdempotent(t *testing.T) {
	in := `worker: write item 42 output /Share/Music/A/B/c.lrc: refusing to write: output dir "/Share/Music/A/B" does not exist`
	once := Normalize(in)
	if twice := Normalize(once); twice != once {
		t.Errorf("not idempotent:\n once %q\ntwice %q", once, twice)
	}
}

// Empty and whitespace-only input must not become a distinct group -- the SQL
// already maps an empty last_error to "unknown", and normalization must not
// reintroduce a second empty bucket alongside it.
func TestNormalizeEmptyIsStable(t *testing.T) {
	for _, s := range []string{"", "   ", "\n\t "} {
		if got := Normalize(s); got != "" {
			t.Errorf("Normalize(%q) = %q, want empty", s, got)
		}
	}
}

// A multi-line error (the detector lane wraps its cause on a second line) keeps
// its shape: the first line is the category, and later lines are where the
// variable text lives.
func TestNormalizeHandlesMultiLine(t *testing.T) {
	got := Normalize("detector request failed: orchestrator: lane outage\ndetector: classifier unavailable: Post \"http://yamnet:8080/classify\": dial tcp 172.22.0.2:8080: connect: connection refused")
	if !strings.Contains(got, "detector request failed") {
		t.Errorf("the leading category was lost:\n%s", got)
	}
	if strings.Contains(got, "172.22.0.2") {
		t.Errorf("normalized signature retains a container IP:\n%s", got)
	}
}
