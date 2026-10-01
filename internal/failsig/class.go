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

// transientMarkers are lower-case phrases of the signatures that mean "try
// again later". A marker counts only when it OPENS a segment of the signature
// (see segments), never when it merely appears somewhere inside one. Matching
// anywhere would let text the writer does not control fake a verdict: a path
// ("read /mnt/Timeout Band permission denied") or an artist/title carried in a
// writer error ("nothing to save for Timeout - Song"). Go and the providers
// build their errors as "context: cause", so the diagnostic phrase starts its
// own ": "-delimited segment; a path or a name sits in the middle of one.
// Segment-start was chosen over "last segment only" because several real
// signatures carry the marker mid-chain ("musixmatch: transport error:
// proxyconnect tcp: ..."), where a tail-only rule would lose it. It is not proof
// against a title that itself contains ": timeout"; no text rule can be.
//
// Deliberately NOT here: a bare "unavailable" (an "ffmpeg unavailable at ..."
// is a misconfiguration, not an outage) and a bare "refused" (a "refusing to
// write" is a deterministic write guard). petitlyrics' "application id
// revoked?" is deliberately persistent: a revoked app id will not recover.
var transientMarkers = []string{
	"transport error",
	"connection refused",
	"connection reset",
	"connection timed out",
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
	"operation timed out",
	"deadline exceeded",
	"context deadline exceeded",
	"context canceled",
	"rate limited",
	"throttled",
	"circuit open",
	"lane unavailable",
	"classifier unavailable",
	"temporarily unavailable",
	"server sent goaway",
	"database is locked",
	"sqlite_busy",
	// A network share that drops out: transient for the same reason a refused
	// connection is. Both clear when the host or mount comes back.
	"host is down",
	"stale nfs file handle",
}

// transientExact are markers that must be a WHOLE segment, because as a prefix
// they would match unrelated words. "eof" is how Go prints a bare io.EOF after a
// request ("Post \"...\": EOF").
var transientExact = []string{"eof"}

// segments splits a signature into its ": "-delimited parts, trimmed, with a
// parenthesized note ("... (circuit open)", "... (rate limited)") promoted to a
// segment of its own so a marker inside parentheses still opens one.
func segments(s string) []string {
	s = strings.ReplaceAll(s, " (", ": ")
	parts := strings.Split(s, ": ")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// musixmatchThrottle reports the two Musixmatch shapes that orchestrator.
// ClassifyOutcome files under OutcomeAuthRateLimit (ErrUnauthorized and
// ErrTokenRenewalRequired) and that the worker treats as throttling: a bare 401
// there is observed to be an egress IP throttle, not a dead credential
// (worker.go, "a later bare 401 is correctly read as throttling"). The status
// branch would call the 401 Persistent, so this rule runs first.
func musixmatchThrottle(segs []string) bool {
	for i := 1; i < len(segs); i++ {
		if segs[i-1] != "musixmatch" {
			continue
		}
		if strings.HasPrefix(segs[i], "unauthorized") || strings.HasPrefix(segs[i], "token renewal required") {
			return true
		}
	}
	return false
}

// Classify buckets a signature produced by Normalize. It classifies FAILED rows
// (status=failed) only. Deferred misses and retired (unavailable) rows have their
// own states and are out of scope here, so there is deliberately no third class
// for #478's "unavailable". Order:
//
//  1. The Musixmatch 401/token-renewal shapes are Transient (see
//     musixmatchThrottle), to agree with orchestrator.ClassifyOutcome and the
//     worker.
//  2. An HTTP status decides next when one is present: 408 and 429 (a request
//     timeout and a rate limit) and every 5xx are Transient; any other 4xx is
//     Persistent. Other numbers (2xx/3xx) carry no verdict.
//  3. Otherwise a transport/timeout/DNS/cancellation marker opening a segment is
//     Transient.
//  4. Everything else, including write and permission errors and every
//     unrecognized signature, is Persistent.
//
// Write and permission failures need no rule of their own: they fall through to
// step 4 by design, and TestClassifyPersistentShapes pins that they do.
func Classify(sig string) Class {
	s := strings.ToLower(sig)
	segs := segments(s)
	if musixmatchThrottle(segs) {
		return Transient
	}
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
	for _, seg := range segs {
		for _, mk := range transientMarkers {
			if strings.HasPrefix(seg, mk) {
				return Transient
			}
		}
		for _, mk := range transientExact {
			if seg == mk {
				return Transient
			}
		}
	}
	return Persistent
}
