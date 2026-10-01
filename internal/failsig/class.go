package failsig

import (
	"regexp"
	"strings"
)

// Class says whether a failure signature is worth waiting out or needs a
// person. It is the classifier half of #478's "transient vs persistent visually
// distinct" criterion.
//
// NO CALLER YET. This is the pure classifier only; the Failure Analysis badge
// that consumes it is wired in a later slice (#478-3). Nothing in serve mode
// reads a Class today.
type Class string

const (
	// Transient failures are expected to clear on their own: a provider 5xx, a
	// rate limit, a dropped connection, a timeout, a DNS blip, or a canceled
	// context. Retrying without any change is reasonable.
	Transient Class = "transient"
	// Persistent failures will repeat until something changes: a 4xx, a write or
	// permission error, a missing output directory. It is also the verdict for any
	// signature this package does not recognize, so an unfamiliar failure
	// SURFACES instead of being filed under "will fix itself".
	Persistent Class = "persistent"
)

// String returns the class name, for badges and logs.
func (c Class) String() string { return string(c) }

// statusRe pulls an HTTP status out of the shapes the providers actually emit:
// "unexpected matcher status_code 500", "HTTP 429: ...", "API error: status 503,
// body: ...", "transcribe status 502: ...". Exactly three digits on a word
// boundary, so a two-digit ffmpeg exit code ("exit status 69") never matches.
var statusRe = regexp.MustCompile(`\b(?:status_code|status|http)\s+(\d{3})\b`)

// exitStatusRe removes a process exit status before statusRe runs, because
// "exit status 503" is shaped like an HTTP status but is not one.
var exitStatusRe = regexp.MustCompile(`exit status \S+`)

// transientMarkers are lower-case substrings of the signatures that mean "try
// again later". They are matched against a signature Normalize already
// produced, so no path, port, or address can hide or fake one.
//
// Deliberately NOT here: a bare "unavailable" (an "ffmpeg unavailable at ..."
// is a misconfiguration, not an outage) and a bare "refused" (a "refusing to
// write" is a deterministic write guard).
var transientMarkers = []string{
	"transport error",
	"connection refused",
	"connection reset",
	"broken pipe",
	"unexpected eof",
	"dial tcp",
	"proxyconnect",
	"tls handshake",
	"network is unreachable",
	"no such host",
	"i/o timeout",
	"timeout",
	"timed out",
	"deadline exceeded",
	"context canceled",
	"rate limited",
	"throttled",
	"circuit open",
	"classifier unavailable",
	"temporarily unavailable",
}

// Classify buckets a signature produced by Normalize. Order:
//
//  1. An HTTP status decides first when one is present: 408 and 429 (a request
//     timeout and a rate limit) and every 5xx are Transient; any other 4xx is
//     Persistent. Other numbers (2xx/3xx) carry no verdict.
//  2. Otherwise a transport/timeout/DNS/cancellation marker is Transient.
//  3. Everything else, including write and permission errors and every
//     unrecognized signature, is Persistent.
//
// Write and permission failures need no rule of their own: they fall through to
// step 3 by design, and TestClassifyPersistentShapes pins that they do.
func Classify(sig string) Class {
	s := strings.ToLower(sig)
	if m := statusRe.FindStringSubmatch(exitStatusRe.ReplaceAllString(s, "")); m != nil {
		code := m[1]
		switch {
		case code == "408" || code == "429":
			return Transient
		case code[0] == '5':
			return Transient
		case code[0] == '4':
			return Persistent
		}
	}
	for _, mk := range transientMarkers {
		if strings.Contains(s, mk) {
			return Transient
		}
	}
	return Persistent
}
