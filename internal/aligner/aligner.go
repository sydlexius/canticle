// Package aligner is a Go client for the forced-alignment sidecar in issue
// #1005 (Demucs vocal separation + a faster-whisper transcript plus a direct
// wav2vec2 forced alignment, deploy/aligner/, #1016). It
// forced-aligns caller-supplied lyric lines to an audio file and returns
// per-word timestamps plus the sidecar's own transcript, for a caller to
// gate acceptance with verification.Similarity.
//
// Slice 2 of epic #482 (#1006): a typed, breaker-protected, bounded-read
// HTTP client and nothing else. NO PRODUCTION CALLER yet.
//
// Contract (deploy/aligner/app.py + its README "Contract" section): POST
// /align is multipart/form-data, a "file" field (audio) and a "lyrics"
// field (lines newline-joined, UTF-8). The sidecar's _parse_lines splits
// ONLY on "\n" (never str.splitlines()), strips each line with Python
// str.strip(), and drops the ones that come out empty before assigning
// line_index (0-based, in order over the sidecar's OWN filtered lines) --
// see filteredToRawLineIndex/isPythonBlank, which must use the SAME blank
// test as the sidecar (Python's whitespace set is wider than Go's
// unicode.IsSpace), not len(lines). Because a sidecar line_index counts only
// non-blank lines, Align remaps every returned Word.LineIndex back to its
// raw index in the caller's lines slice (see decodeAndValidate), so
// Word.LineIndex always indexes the lines slice the caller passed, never the
// sidecar's filtered one. Because the sidecar splits on "\n" only, a caller
// line containing "\n" would silently become two sidecar lines and shift
// every later line_index; Align rejects such a line with ErrEmbeddedNewline
// before sending anything. A bare trailing "\r" is contract-legal
// (str.strip() removes it). The 200 response is JSON:
//
//	{"words":[{"text":"...","start_ms":0,"end_ms":100,"line_index":0,"confidence":0.9}],"transcript":"..."}
//
// Status: breaker accounting by response.
//
//	200 (has "words")   success, RecordSuccess
//	200 (missing "words") ErrInvalidResponse, Trip (same as an undecodable body)
//	400/411/413/422     caller-input error, RecordBenignMiss
//	404/405             wrong URL or method, Trip (misconfiguration, not a miss)
//	429                 ErrBusy (ALIGNER_MAX_PENDING), RecordBenignMiss
//	3xx                 never followed (see CheckRedirect in NewHTTPClient), Trip
//	any other 4xx       Trip (unknown means misconfigured, the conservative read)
//	5xx                 pipeline failure, Trip
//
// The reference sidecar image is CPU-only (a CUDA build is #1013).
//
// The sidecar also accepts an optional "language" form field (an ISO 639-1
// hint that skips its own language detection); this client does not send
// it.
package aligner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/config"
)

// maxResponseSize bounds how much of the sidecar's response body is ever read
// into memory. A response that would exceed this is a hard error, never a
// truncated parse.
const maxResponseSize = 4 << 20

// multipartFilename is the constant filename sent in the multipart "file"
// field. The sidecar ignores this field's value entirely (it identifies the
// upload part, not the track), and a track's real file basename is private
// library metadata with no reason to leave the process.
const multipartFilename = "audio"

// defaultCircuitBackoffBase and defaultCircuitOpenDuration seed the client's
// own breaker when the caller does not supply one, matching the worker's
// provider-breaker defaults.
const (
	defaultCircuitBackoffBase  = 60 * time.Second
	defaultCircuitOpenDuration = 30 * time.Minute
)

// Sentinel errors. Wrapped with context via fmt.Errorf/%w so errors.Is keeps
// working through the wrapping this package and any caller add.
var (
	// ErrBreakerOpen is returned when the circuit breaker is open and the
	// sidecar is not called at all.
	ErrBreakerOpen = errors.New("aligner: circuit breaker is open")
	// ErrResponseTooLarge is returned when the sidecar's response body exceeds
	// maxResponseSize. The response is never partially parsed in this case.
	ErrResponseTooLarge = errors.New("aligner: response too large")
	// ErrInvalidResponse is returned when the response decodes as JSON but
	// fails the internal-consistency checks in decodeAndValidate
	// (non-monotonic or out-of-range timings, invalid line index,
	// out-of-range confidence).
	ErrInvalidResponse = errors.New("aligner: invalid response")
	// ErrEmbeddedNewline is returned when a caller-supplied line contains a
	// "\n": the wire format joins lines with "\n" and the sidecar splits on
	// "\n" only, so an embedded newline would silently split into two
	// sidecar lines and shift every later line_index.
	ErrEmbeddedNewline = errors.New("aligner: line contains an embedded newline")
	// ErrInvalidUTF8 is returned when a caller-supplied line is not valid
	// UTF-8. The sidecar is a Starlette app: a non-UTF-8 multipart form field
	// decodes as latin-1, silently turning a stray 0x85 or 0xA0 byte (valid
	// latin-1, invalid UTF-8) into different text -- or, in the pattern this
	// package's ErrEmbeddedNewline precondition already guards against,
	// content that reads as blank on the sidecar side and shifts every later
	// line_index. Rejecting invalid UTF-8 before sending anything avoids
	// depending on that decode behavior at all.
	ErrInvalidUTF8 = errors.New("aligner: line is not valid UTF-8")
	// ErrBusy is returned when the sidecar answers 429 (ALIGNER_MAX_PENDING
	// already admitted). It is a benign miss like any other 4xx and does not
	// trip the breaker, but a caller needs to tell it apart from a genuine
	// rejection. Use errors.As to recover the advertised delay via *BusyError.
	ErrBusy = errors.New("aligner: sidecar is busy")
)

// BusyError wraps ErrBusy with the sidecar's advertised retry delay, parsed
// from the response's Retry-After header (RFC 9110: seconds or an
// HTTP-date). RetryAfter is 0 when the header was absent or unparsable --
// treat that as "unknown, pick your own backoff", never "retry
// immediately". errors.Is(err, ErrBusy) works through Unwrap.
type BusyError struct {
	RetryAfter time.Duration
}

func (e *BusyError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("aligner: sidecar busy, retry after %s", e.RetryAfter)
	}
	return "aligner: sidecar busy"
}

// Unwrap makes errors.Is(err, ErrBusy) true for a *BusyError.
func (e *BusyError) Unwrap() error { return ErrBusy }

// maxRetryAfter clamps parseRetryAfter's result: the sidecar always sends a
// fixed 30s today, but the header is caller-influenced input (an
// HTTP-date arithmetic overflow, or a malicious/misconfigured
// intermediary), so an out-of-range value is clamped rather than handed to
// a caller's own backoff/sleep unbounded.
const maxRetryAfter = time.Hour

// parseRetryAfter parses an HTTP Retry-After header value. The reference
// sidecar always sends a fixed "30" (seconds), but this does not assume
// that: a bare non-negative integer is read as seconds, and an HTTP-date is
// supported via the standard library's own parser. Anything unparsable, a
// negative value, or a past date returns 0 (unknown); anything larger than
// maxRetryAfter clamps to it.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if isAllDigits(v) {
		// Parse as a big.Int-free range check: strconv.Atoi/ParseInt fails
		// outright on a value beyond the platform int range (e.g.
		// "9223372036854775808"), which would otherwise fall through to the
		// HTTP-date parse (always fails on digits-only input) and silently
		// return 0 (unknown) for what is clearly an oversized value, not
		// garbage. An all-digit string that doesn't fit clamps to the max
		// instead.
		secs, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return maxRetryAfter
		}
		// Clamp the seconds value itself, before converting to a
		// time.Duration: a large-enough secs overflows the int64
		// nanosecond multiplication below (e.g. 10000000000s) and would
		// otherwise wrap to a garbage (even negative) Duration.
		if secs > int64(maxRetryAfter/time.Second) {
			return maxRetryAfter
		}
		return clampRetryAfter(time.Duration(secs) * time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return clampRetryAfter(d)
		}
	}
	return 0
}

// isAllDigits reports whether v is non-empty and consists only of ASCII
// digits (no sign, no whitespace -- v is already trimmed by the caller).
// Distinguishing "all-digits but too big to parse" from "not a number at
// all" is what lets parseRetryAfter clamp the former to maxRetryAfter
// instead of silently reading it as 0 (unknown) like ordinary garbage.
func isAllDigits(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// clampRetryAfter bounds d to [0, maxRetryAfter].
func clampRetryAfter(d time.Duration) time.Duration {
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}

// isBenignClientError reports whether status is a caller-input error the
// sidecar itself validated and rejected -- 400 (bad request shape/body),
// 411 (missing Content-Length), 413 (payload too large), or 422
// (unprocessable audio/lyrics). These are the ONLY 4xx statuses treated as
// benign; every other 4xx (including 404/405, and any status this client
// does not otherwise recognize) trips the breaker, per the status table in
// the package doc.
func isBenignClientError(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusLengthRequired, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

// Word is one forced-aligned word with its timing and owning line.
type Word struct {
	// Text is the word as the sidecar returned it (expected to echo the
	// caller-supplied lyric text, not a blind transcription).
	Text string
	// StartMS and EndMS are milliseconds from the start of the audio.
	StartMS int
	EndMS   int
	// LineIndex indexes into the lines slice the caller passed to Align.
	LineIndex int
	// Confidence is the sidecar's own per-word confidence, in [0, 1].
	Confidence float64
}

// Result is a successful alignment: word-level timings plus the sidecar's own
// transcript. The transcript lets a caller run verification.Similarity as a
// cheap content gate without a second HTTP round-trip.
type Result struct {
	Words      []Word
	Transcript string
}

// Aligner forced-aligns known lyric lines to an audio file. The interface
// boundary lets a caller (slice 3/4) inject a fake in its own tests without
// depending on this package's HTTP details.
type Aligner interface {
	Align(ctx context.Context, audioPath string, lines []string) (Result, error)
}

// HTTPClient calls the #1005 sidecar contract over HTTP. It is
// context-aware, applies a caller-supplied timeout, protects the sidecar with
// an injected or self-owned circuit breaker, and bounds how much of the
// response it will read.
type HTTPClient struct {
	baseURL string
	client  *http.Client
	breaker *circuit.Breaker
}

// NewHTTPClient creates a client for the aligner sidecar at baseURL. timeout
// bounds each HTTP round-trip; forced alignment runs far longer than fixed
// timeouts elsewhere and depends on the operator's hardware, so it is a
// required parameter, not a constant. A non-positive timeout is rejected
// rather than defaulted (0 means "no timeout" to net/http).
//
// breaker may be nil, in which case the client self-provisions one with the
// package defaults; passing one lets a caller share a lifecycle with other
// lanes, as internal/orchestrator does for provider lanes.
func NewHTTPClient(baseURL string, timeout time.Duration, breaker *circuit.Breaker) (*HTTPClient, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return nil, fmt.Errorf("aligner: url must not be empty")
	}
	if err := config.ValidateHTTPURL(baseURL); err != nil {
		return nil, fmt.Errorf("aligner: invalid url: %w", err)
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("aligner: timeout must be positive")
	}
	if breaker == nil {
		breaker = circuit.New(defaultCircuitBackoffBase, defaultCircuitOpenDuration)
	}
	return &HTTPClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		client: &http.Client{
			Timeout: timeout,
			// The sidecar contract has no redirect step; a 3xx here means
			// misconfiguration (a reverse proxy or a trailing-slash
			// mismatch), not a hop to follow. Returning the redirect
			// response as-is (instead of transparently following it) keeps
			// the audio from being re-POSTed to a target this client never
			// validated, and lets it reach the non-2xx path below, which
			// trips the breaker like any other unexpected response.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		breaker: breaker,
	}, nil
}

// Breaker returns the client's circuit breaker, for a caller that wants to
// observe or log its state (mirroring orchestrator.Lane.Breaker()).
func (c *HTTPClient) Breaker() *circuit.Breaker {
	return c.breaker
}

// Align forced-aligns lines to the audio at audioPath via the sidecar. It
// returns ErrBreakerOpen without making a request when the breaker is open,
// and ErrEmbeddedNewline or ErrInvalidUTF8 without making a request if any
// line contains "\n" or is not valid UTF-8 (see the package doc).
//
// Breaker accounting: see the status table in the package doc. In short, a
// successful response with a "words" key records a breaker success; 400,
// 411, 413, 422, and 429 (ErrBusy) are benign misses; every other non-2xx
// response, a transport failure, and a 200 that fails to decode/validate
// (including a missing "words" key) trip the breaker.
func (c *HTTPClient) Align(ctx context.Context, audioPath string, lines []string) (Result, error) {
	if strings.TrimSpace(audioPath) == "" {
		return Result{}, fmt.Errorf("aligner: audio path is empty")
	}
	if len(lines) == 0 {
		return Result{}, fmt.Errorf("aligner: lines must not be empty")
	}
	for i, l := range lines {
		if strings.Contains(l, "\n") {
			return Result{}, fmt.Errorf("aligner: line %d: %w", i, ErrEmbeddedNewline)
		}
		if !utf8.ValidString(l) {
			return Result{}, fmt.Errorf("aligner: line %d: %w", i, ErrInvalidUTF8)
		}
	}
	if c.breaker.Allow() == circuit.StateOpen {
		return Result{}, ErrBreakerOpen
	}

	req, err := c.buildRequest(ctx, audioPath, lines)
	if err != nil {
		return Result{}, err
	}

	res, err := c.client.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// Report the context's own error (not the breaker below) so a
			// caller can distinguish "we gave up" from "the sidecar is
			// broken" -- a caller-initiated cancel is not breaker evidence.
			return Result{}, fmt.Errorf("aligner: %w", ctxErr)
		}
		c.breaker.Trip()
		return Result{}, fmt.Errorf("aligner: request failed: %w", err)
	}
	defer func() {
		_ = res.Body.Close()
	}()

	if res.StatusCode == http.StatusTooManyRequests {
		// ALIGNER_MAX_PENDING already admitted; "try again later", not a
		// rejection of the request, so it gets its own typed error. Benign
		// miss, per the breaker-accounting doc above -- must not trip. Body
		// drained (never included: it is a fixed {"detail": "..."}, but this
		// keeps the same no-lyric-text-in-errors discipline).
		c.breaker.RecordBenignMiss()
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 8<<10))
		return Result{}, &BusyError{RetryAfter: parseRetryAfter(res.Header.Get("Retry-After"))}
	}
	if isBenignClientError(res.StatusCode) {
		// A caller-input error the sidecar itself validated and rejected
		// (bad request shape, no/invalid Content-Length, payload too large,
		// unprocessable audio/lyrics) -- a benign miss, per the doc above.
		c.breaker.RecordBenignMiss()
		errBody, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
		return Result{}, fmt.Errorf("aligner: request rejected, status %d: %s", res.StatusCode, strings.TrimSpace(string(errBody)))
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// Everything else non-2xx trips the breaker: a redirect this client
		// never follows (see CheckRedirect above), 404/405 (a wrong URL or
		// HTTP method -- misconfiguration, not a benign miss), any other
		// undocumented 4xx (unknown means misconfigured, the same
		// conservative read as 404/405, rather than risking the breaker
		// never tripping on a persistent bad deployment), and 5xx (a
		// pipeline failure).
		c.breaker.Trip()
		errBody, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
		return Result{}, fmt.Errorf("aligner: sidecar error, status %d: %s", res.StatusCode, strings.TrimSpace(string(errBody)))
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseSize+1))
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			// Same carve-out as the Do() error path above: a caller-initiated
			// cancel or deadline can abort the body read just as easily as
			// the round-trip itself, and it is equally not breaker evidence.
			return Result{}, fmt.Errorf("aligner: %w", ctxErr)
		}
		c.breaker.Trip()
		return Result{}, fmt.Errorf("aligner: read response: %w", err)
	}
	if len(body) > maxResponseSize {
		c.breaker.Trip()
		return Result{}, fmt.Errorf("%w (%d byte limit)", ErrResponseTooLarge, maxResponseSize)
	}

	result, err := decodeAndValidate(body, filteredToRawLineIndex(lines))
	if err != nil {
		c.breaker.Trip()
		return Result{}, err
	}

	c.breaker.RecordSuccess()
	return result, nil
}

// wireWord and wireResult mirror the sidecar's JSON field names (snake_case)
// without leaking those names into the exported Word/Result types. Every
// field is a pointer so JSON decode leaves it nil when the key is absent OR
// explicitly null, distinct from a present zero value (a genuine start_ms of
// 0, or an empty-string Text) -- see decodeAndValidate, which rejects a word
// with any nil field before it can pass through as a silently-zeroed Word.
type wireWord struct {
	Text       *string  `json:"text"`
	StartMS    *int     `json:"start_ms"`
	EndMS      *int     `json:"end_ms"`
	LineIndex  *int     `json:"line_index"`
	Confidence *float64 `json:"confidence"`
}

type wireResult struct {
	// Words is a pointer so JSON decode leaves it nil when the "words" key
	// is absent from the response, distinct from a present-but-empty array
	// (a track with no aligned words, e.g. an instrumental passage -- a
	// valid result, see TestAlign_Success/empty_words_is_valid). A 200
	// response missing "words" entirely is not the sidecar's documented
	// contract and is treated as invalid, the same as an undecodable body.
	Words      *[]wireWord `json:"words"`
	Transcript string      `json:"transcript"`
}

// pythonExtraSpace holds the code points CPython 3.11's str.isspace()/
// str.strip() treat as whitespace that Go's unicode.IsSpace does not:
// U+001C..U+001F (file/group/record/unit separators). Measured by
// enumerating chr(c).isspace() for c in range(0x110000) in CPython and
// comparing against unicode.IsSpace over the same range: this is the
// complete and only difference, not a partial list.
var pythonExtraSpace = map[rune]struct{}{0x1C: {}, 0x1D: {}, 0x1E: {}, 0x1F: {}}

// isPythonSpace reports whether r is whitespace under CPython's str.strip().
func isPythonSpace(r rune) bool {
	if unicode.IsSpace(r) {
		return true
	}
	_, extra := pythonExtraSpace[r]
	return extra
}

// isPythonBlank reports whether s is "blank" exactly as the sidecar's
// _parse_lines defines it: empty after Python str.strip(). This is the ONE
// place the client tests a line for blankness; any future caller must go
// through this function too, not re-derive the whitespace set.
func isPythonBlank(s string) bool {
	return strings.TrimFunc(s, isPythonSpace) == ""
}

// filteredToRawLineIndex returns, for each non-blank line in lines (per
// isPythonBlank, mirroring the sidecar's own _parse_lines), that line's raw
// index in lines, in order. Its length is the sidecar's own filtered line
// count -- what a response's line_index must be checked against, since blank
// lines are dropped and never consume a line_index there (raw len(lines)
// would be wrong whenever any line is blank). Its VALUES are what
// decodeAndValidate remaps a validated line_index through: the sidecar
// counts line_index over its own filtered lines, but Word.LineIndex is
// documented as indexing the caller's original lines slice, so a caller
// index must be translated back to the raw slice position it actually
// corresponds to -- otherwise, with lines = ["hello", "", "world"], a word
// from "world" (sidecar line_index 1, since "hello" is line_index 0 and the
// blank line consumes no index) would be reported as caller LineIndex 1,
// which is the blank line in the caller's slice, not "world".
func filteredToRawLineIndex(lines []string) []int {
	raw := make([]int, 0, len(lines))
	for i, l := range lines {
		if !isPythonBlank(l) {
			raw = append(raw, i)
		}
	}
	return raw
}

// decodeAndValidate parses the sidecar's JSON body and checks internal
// consistency: a present "words" key (see wireResult.Words); non-negative,
// non-decreasing timings; end not before start; a line index within
// len(filteredToRaw) (the sidecar's own filtered line count, NOT
// len(lines)); and a confidence in [0, 1]. A word that passes validation has
// its LineIndex remapped through filteredToRaw[w.LineIndex] before being
// returned, so the caller-facing Word.LineIndex indexes the ORIGINAL lines
// slice the caller passed to Align, not the sidecar's blank-filtered one.
//
// A validation failure's error text NEVER includes the offending word's Text
// or any lyric content -- only its position and the failing numeric field.
func decodeAndValidate(body []byte, filteredToRaw []int) (Result, error) {
	var wire wireResult
	if err := json.Unmarshal(body, &wire); err != nil {
		return Result{}, fmt.Errorf("aligner: decode response: %w", err)
	}
	if wire.Words == nil {
		return Result{}, fmt.Errorf(`%w: response has no "words" key`, ErrInvalidResponse)
	}

	lineCount := len(filteredToRaw)
	words := make([]Word, 0, len(*wire.Words))
	prevStart := -1
	for i, w := range *wire.Words {
		if w.Text == nil || w.StartMS == nil || w.EndMS == nil || w.LineIndex == nil || w.Confidence == nil {
			return Result{}, fmt.Errorf("%w: word %d is missing a required field", ErrInvalidResponse, i)
		}
		startMS, endMS, lineIndex, confidence := *w.StartMS, *w.EndMS, *w.LineIndex, *w.Confidence
		if startMS < 0 {
			return Result{}, fmt.Errorf("%w: word %d has negative start_ms", ErrInvalidResponse, i)
		}
		if endMS < startMS {
			return Result{}, fmt.Errorf("%w: word %d end_ms precedes start_ms", ErrInvalidResponse, i)
		}
		if startMS < prevStart {
			return Result{}, fmt.Errorf("%w: word %d start_ms is not monotonic non-decreasing", ErrInvalidResponse, i)
		}
		prevStart = startMS
		if lineIndex < 0 || lineIndex >= lineCount {
			return Result{}, fmt.Errorf("%w: word %d line_index %d out of range [0,%d)", ErrInvalidResponse, i, lineIndex, lineCount)
		}
		if math.IsNaN(confidence) || confidence < 0 || confidence > 1 {
			return Result{}, fmt.Errorf("%w: word %d confidence out of range [0,1]", ErrInvalidResponse, i)
		}
		words = append(words, Word{
			Text:       *w.Text,
			StartMS:    startMS,
			EndMS:      endMS,
			LineIndex:  filteredToRaw[lineIndex],
			Confidence: confidence,
		})
	}

	return Result{Words: words, Transcript: wire.Transcript}, nil
}

// buildRequest constructs the multipart POST per the package-doc contract.
func (c *HTTPClient) buildRequest(ctx context.Context, audioPath string, lines []string) (*http.Request, error) {
	f, err := openAudioFile(audioPath)
	if err != nil {
		return nil, fmt.Errorf("aligner: open audio: %w", err)
	}
	defer func() {
		_ = f.Close()
	}()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	// The multipart filename is a constant, never audioPath's basename: the
	// sidecar ignores the filename field entirely, and a track's file
	// basename is private library metadata that has no reason to cross the
	// wire.
	fw, err := mw.CreateFormFile("file", multipartFilename)
	if err != nil {
		return nil, fmt.Errorf("aligner: create multipart file: %w", err)
	}
	if _, err := io.Copy(fw, f); err != nil {
		return nil, fmt.Errorf("aligner: copy audio: %w", err)
	}
	if err := mw.WriteField("lyrics", strings.Join(lines, "\n")); err != nil {
		return nil, fmt.Errorf("aligner: write lyrics field: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("aligner: close multipart body: %w", err)
	}

	// body is a *bytes.Buffer, so http.NewRequestWithContext sets a concrete
	// Content-Length (never chunked Transfer-Encoding) -- the sidecar 411s a
	// POST with no valid Content-Length. See TestBuildRequest_ContentLength.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.alignURL(), &body)
	if err != nil {
		return nil, fmt.Errorf("aligner: create request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req, nil
}

func (c *HTTPClient) alignURL() string {
	if strings.HasSuffix(c.baseURL, "/align") {
		return c.baseURL
	}
	return c.baseURL + "/align"
}

// openAudioFile opens audioPath for reading. audioPath is an operator/scanned
// library path, not untrusted user input, mirroring the same pattern used by
// internal/verification and internal/detector.
func openAudioFile(audioPath string) (*os.File, error) {
	return os.Open(audioPath) //nolint:gosec // reason: audioPath is a scanned library file path, not untrusted user input
}
