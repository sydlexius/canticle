package aligner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
)

// newTestAudio writes a small temp file to stand in for an audio file; Align
// never inspects its content, only reads and forwards its bytes.
func newTestAudio(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "aligner-test-*.wav")
	if err != nil {
		t.Fatalf("create temp audio: %v", err)
	}
	if _, err := f.WriteString("not-really-audio"); err != nil {
		t.Fatalf("write temp audio: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp audio: %v", err)
	}
	return f.Name()
}

func newTestBreaker() *circuit.Breaker {
	return circuit.New(time.Minute, time.Hour)
}

func newClientForServer(t *testing.T, srv *httptest.Server, breaker *circuit.Breaker) *HTTPClient {
	t.Helper()
	c, err := NewHTTPClient(srv.URL, 5*time.Second, breaker)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	return c
}

// jsonServer returns an httptest.Server that always answers 200 with body.
func jsonServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

// TestAlign_RequestMatchesSidecarContract parses the actual multipart request
// Align sends: field names/encoding must match the sidecar (`file`,
// `lyrics` as plain newline text, never JSON), and the request must carry a
// concrete Content-Length (the sidecar 411s a POST without one). Mutation-
// checked: renaming fields, re-JSON-encoding lyrics, or streaming the body
// (losing Content-Length) each fail an assertion below, not a build/panic.
func TestAlign_RequestMatchesSidecarContract(t *testing.T) {
	const audioContent = "not-really-audio"
	var gotFileName, gotLyricsField string
	var gotFileBytes []byte
	var hasOldAudioField, hasOldLinesField, gotChunked bool
	var gotContentLength int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentLength = r.ContentLength
		for _, te := range r.TransferEncoding {
			if te == "chunked" {
				gotChunked = true
			}
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("ParseMultipartForm: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if fhs, ok := r.MultipartForm.File["file"]; ok && len(fhs) == 1 {
			gotFileName = fhs[0].Filename
			f, err := fhs[0].Open()
			if err != nil {
				t.Errorf("open uploaded file: %v", err)
			} else {
				gotFileBytes, _ = io.ReadAll(f)
				_ = f.Close()
			}
		}
		_, hasOldAudioField = r.MultipartForm.File["audio"]
		_, hasOldLinesField = r.MultipartForm.Value["lines"]
		gotLyricsField = r.FormValue("lyrics")

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"words":[],"transcript":""}`))
	}))
	defer srv.Close()

	c := newClientForServer(t, srv, newTestBreaker())
	_, err := c.Align(context.Background(), newTestAudio(t), []string{"line one", "line two"})
	if err != nil {
		t.Fatalf("Align: %v", err)
	}

	if gotFileName != multipartFilename {
		t.Fatalf(`multipart "file" filename = %q; want the constant %q (never the audio path basename, which is private library metadata)`, gotFileName, multipartFilename)
	}
	if hasOldAudioField {
		t.Fatal(`multipart request still carries the old "audio" file field`)
	}
	if string(gotFileBytes) != audioContent {
		t.Fatalf("uploaded audio bytes = %q; want %q", gotFileBytes, audioContent)
	}
	if hasOldLinesField {
		t.Fatal(`multipart request still carries the old "lines" field`)
	}
	const wantLyrics = "line one\nline two"
	if gotLyricsField != wantLyrics {
		t.Fatalf("lyrics field = %q; want %q (newline-joined plain text, not JSON)", gotLyricsField, wantLyrics)
	}
	if strings.HasPrefix(strings.TrimSpace(gotLyricsField), "[") {
		t.Fatalf("lyrics field looks JSON-encoded: %q; sidecar reads plain newline-separated text", gotLyricsField)
	}
	if gotContentLength <= 0 {
		t.Fatalf("request Content-Length = %d; want > 0 (sidecar 411s without one)", gotContentLength)
	}
	if gotChunked {
		t.Fatal("request used chunked Transfer-Encoding; want a concrete Content-Length")
	}
}

// TestAlign_LineIndexRespectsBlankLineFiltering verifies the client checks a
// response's line_index against the sidecar's blank-line-filtered count,
// not the raw line count, and that filteredLineCount's blank test (via
// isPythonBlank) matches Python's str.strip() end-to-end through Align.
func TestAlign_LineIndexRespectsBlankLineFiltering(t *testing.T) {
	// 3 lines, 1 blank -> 2 non-blank; index 1 is "world" (3rd element).
	blankMiddleLines := []string{"hello", "", "world"}
	// Python-blank-but-not-Go-blank runes must also be dropped.
	pySpaceOnlyLines := []string{"hello", "\u001c\u001d", "world"}

	cases := []struct {
		name      string
		lines     []string
		lineIndex int
		wantValid bool
	}{
		{"index_within_filtered_count_is_valid", blankMiddleLines, 1, true},
		{"index_at_raw_len_but_beyond_filtered_count_is_invalid", blankMiddleLines, 2, false},
		{"index_far_out_of_range_is_invalid", []string{"only one line"}, 5, false},
		{"python_only_blank_runes_dropped_like_ordinary_blank", pySpaceOnlyLines, 1, true},
		{"python_only_blank_runes_still_bound_the_range", pySpaceOnlyLines, 2, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"words":[{"text":"x","start_ms":0,"end_ms":100,"line_index":%d,"confidence":0.9}],"transcript":"x"}`, tc.lineIndex)
			srv := jsonServer(body)
			defer srv.Close()

			c := newClientForServer(t, srv, newTestBreaker())
			res, err := c.Align(context.Background(), newTestAudio(t), tc.lines)
			if tc.wantValid {
				if err != nil {
					t.Fatalf("Align: %v; want line_index %d accepted", err, tc.lineIndex)
				}
				if len(res.Words) != 1 {
					t.Fatalf("len(Words) = %d; want 1", len(res.Words))
				}
				return
			}
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("Align error = %v; want ErrInvalidResponse (line_index %d out of range)", err, tc.lineIndex)
			}
		})
	}
}

// TestIsPythonBlank is mutation-checked: reverting isPythonBlank to a bare
// strings.TrimSpace(s) == "" fails the u001c-u001f cases, since Go's
// unicode.IsSpace does not treat those four code points as whitespace but
// CPython's str.strip() does (measured by enumerating chr(c).isspace() for
// c in range(0x110000) against unicode.IsSpace over the same range: the
// only difference is U+001C..U+001F).
func TestIsPythonBlank(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"", true}, {"   \t\n", true}, {"hello", false}, {"  hello  ", false},
		{"\u001c", true}, {"\u001d", true}, {"\u001e", true}, {"\u001f", true},
		{" \u001f ", true}, {"\u001fx", false},
	}
	for _, tc := range cases {
		if got := isPythonBlank(tc.line); got != tc.want {
			t.Errorf("isPythonBlank(%q) = %v; want %v", tc.line, got, tc.want)
		}
	}
}

// TestAlign_Success covers a normal response and its edge case (no aligned
// words, e.g. an instrumental passage, is still a valid result).
func TestAlign_Success(t *testing.T) {
	cases := []struct {
		name           string
		body           string
		wantWords      int
		wantTranscript string
	}{
		{"words_present", `{"words":[{"text":"hello","start_ms":0,"end_ms":200,"line_index":0,"confidence":0.95},{"text":"world","start_ms":210,"end_ms":400,"line_index":0,"confidence":0.9}],"transcript":"hello world"}`, 2, "hello world"},
		{"empty_words_is_valid", `{"words":[],"transcript":""}`, 0, ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv := jsonServer(tc.body)
			defer srv.Close()

			breaker := newTestBreaker()
			c := newClientForServer(t, srv, breaker)
			res, err := c.Align(context.Background(), newTestAudio(t), []string{"line"})
			if err != nil {
				t.Fatalf("Align: %v", err)
			}
			if len(res.Words) != tc.wantWords {
				t.Fatalf("len(Words) = %d; want %d", len(res.Words), tc.wantWords)
			}
			if res.Transcript != tc.wantTranscript {
				t.Fatalf("Transcript = %q; want %q", res.Transcript, tc.wantTranscript)
			}
			if got := breaker.Allow(); got != circuit.StateClosed {
				t.Fatalf("breaker.Allow() = %v; want StateClosed after success", got)
			}
		})
	}
}

// TestAlign_BreakerAccounting: 400/411/413/422/429 are benign misses and
// must NOT trip the breaker; 404/405 (wrong URL/method), any other 4xx, a
// 5xx, an undecodable 200 body, and a 200 missing "words" must trip it.
// Mutation-checked: swapping the 5xx arm's Trip for RecordBenignMiss fails
// server_error_trips_breaker; treating 404/405 as benign fails those cases.
func TestAlign_BreakerAccounting(t *testing.T) {
	cases := []struct {
		name            string
		statusCode      int
		body            string
		headers         map[string]string
		wantTripped     bool
		wantInvalidResp bool
	}{
		{"bad_request_is_benign_miss", http.StatusBadRequest, "bad request", nil, false, false},
		{"length_required_is_benign_miss", http.StatusLengthRequired, "length required", nil, false, false},
		{"payload_too_large_is_benign_miss", http.StatusRequestEntityTooLarge, "too large", nil, false, false},
		{"unprocessable_is_benign_miss", http.StatusUnprocessableEntity, "unprocessable", nil, false, false},
		{"busy_429_is_benign_miss", http.StatusTooManyRequests, `{"detail":"aligner busy"}`, map[string]string{"Retry-After": "30"}, false, false},
		{"not_found_trips_breaker", http.StatusNotFound, "not found", nil, true, false},
		{"method_not_allowed_trips_breaker", http.StatusMethodNotAllowed, "method not allowed", nil, true, false},
		{"other_4xx_trips_breaker", http.StatusForbidden, "forbidden", nil, true, false},
		{"server_error_trips_breaker", http.StatusInternalServerError, "boom", nil, true, false},
		{"malformed_json_trips_breaker", http.StatusOK, `{not json`, nil, true, false},
		{"missing_words_key_trips_breaker", http.StatusOK, `{"status":"ok"}`, nil, true, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			breaker := newTestBreaker()
			c := newClientForServer(t, srv, breaker)
			_, err := c.Align(context.Background(), newTestAudio(t), []string{"line"})
			if err == nil {
				t.Fatal("Align: want error, got nil")
			}
			if gotTripped := breaker.Trips() > 0; gotTripped != tc.wantTripped {
				t.Fatalf("breaker tripped = %v; want %v", gotTripped, tc.wantTripped)
			}
			if tc.wantInvalidResp && !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("Align error = %v; want ErrInvalidResponse", err)
			}
		})
	}
}

// TestAlign_RedirectTripsBreakerAndIsNeverFollowed: a 3xx response is never
// followed (the client's CheckRedirect returns http.ErrUseLastResponse), so
// the redirect target must receive ZERO requests, and the 3xx itself falls
// through to the non-2xx path and trips the breaker. Mutation-checked:
// removing CheckRedirect lets net/http silently follow the redirect and
// re-POST the audio to targetCount's server, failing targetCount.
func TestAlign_RedirectTripsBreakerAndIsNeverFollowed(t *testing.T) {
	var targetCount atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"words":[],"transcript":""}`))
	}))
	defer target.Close()

	cases := []struct {
		name       string
		statusCode int
	}{
		{"moved_permanently_301", http.StatusMovedPermanently},
		{"temporary_redirect_307", http.StatusTemporaryRedirect},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			targetCount.Store(0)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL+"/align")
				w.WriteHeader(tc.statusCode)
			}))
			defer srv.Close()

			breaker := newTestBreaker()
			c := newClientForServer(t, srv, breaker)
			_, err := c.Align(context.Background(), newTestAudio(t), []string{"line"})
			if err == nil {
				t.Fatal("Align: want error, got nil")
			}
			if breaker.Trips() == 0 {
				t.Fatal("breaker.Trips() = 0; want a trip on an unfollowed redirect")
			}
			if got := targetCount.Load(); got != 0 {
				t.Fatalf("redirect target request count = %d; want 0 (the audio must never be re-POSTed to a redirect target)", got)
			}
		})
	}
}

// TestAlign_BusyReturns429AsErrBusy: 429 must surface as ErrBusy (errors.Is)
// carrying the parsed Retry-After in a *BusyError. Mutation-checked:
// removing the 429 special case falls through to the generic 4xx branch,
// failing errors.Is(err, ErrBusy).
func TestAlign_BusyReturns429AsErrBusy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"detail":"aligner busy"}`))
	}))
	defer srv.Close()

	c := newClientForServer(t, srv, newTestBreaker())
	_, err := c.Align(context.Background(), newTestAudio(t), []string{"line"})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("Align error = %v; want ErrBusy", err)
	}
	var busyErr *BusyError
	if !errors.As(err, &busyErr) {
		t.Fatalf("Align error = %v; want *BusyError in chain", err)
	}
	if busyErr.RetryAfter != 30*time.Second {
		t.Fatalf("BusyError.RetryAfter = %v; want 30s", busyErr.RetryAfter)
	}
}

// TestParseRetryAfter tables parseRetryAfter's cases: a plain integer (what
// the reference sidecar always sends), an HTTP-date, unparsable/missing
// input falling back to 0 (unknown), and the [0, maxRetryAfter] clamp
// (mutation-checked: removing the clamp lets "10000000000" seconds or a
// far-future HTTP-date through unbounded, failing their assertions below).
func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"seconds", "30", 30 * time.Second},
		{"zero_seconds", "0", 0},
		{"empty", "", 0},
		{"garbage", "not-a-value", 0},
		{"negative", "-5", 0},
		{"past_http_date", "Mon, 01 Jan 2001 00:00:00 GMT", 0},
		{"huge_seconds_clamps_to_max", "10000000000", maxRetryAfter},
		{"far_future_http_date_clamps_to_max", "Mon, 01 Jan 2200 00:00:00 GMT", maxRetryAfter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfter(tc.in); got != tc.want {
				t.Fatalf("parseRetryAfter(%q) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestAlign_EmbeddedNewlineRejected: a caller line containing "\n"/"\r\n"
// would silently split into two sidecar lines and shift every later
// line_index, so Align rejects it before any HTTP call; a bare trailing
// "\r" is contract-legal and must NOT be rejected. Mutation-checked:
// removing the precondition check lets "line_feed"/"crlf" reach the fake
// server, failing requestCount.
func TestAlign_EmbeddedNewlineRejected(t *testing.T) {
	cases := []struct {
		name      string
		lines     []string
		wantErr   bool
		wantCalls int32
	}{
		{"line_feed", []string{"good", "bad\nline"}, true, 0},
		{"crlf", []string{"bad\r\nline"}, true, 0},
		{"bare_trailing_cr_is_legal", []string{"good\r"}, false, 1},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var requestCount atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCount.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"words":[],"transcript":""}`))
			}))
			defer srv.Close()

			c := newClientForServer(t, srv, newTestBreaker())
			_, err := c.Align(context.Background(), newTestAudio(t), tc.lines)
			if tc.wantErr {
				if !errors.Is(err, ErrEmbeddedNewline) {
					t.Fatalf("Align error = %v; want ErrEmbeddedNewline", err)
				}
			} else if err != nil {
				t.Fatalf("Align: %v; want no error for a bare trailing CR", err)
			}
			if got := requestCount.Load(); got != tc.wantCalls {
				t.Fatalf("requestCount = %d; want %d", got, tc.wantCalls)
			}
		})
	}
}

// TestAlign_InvalidUTF8Rejected: a caller line that is not valid UTF-8 is
// rejected before any HTTP call. Starlette (the sidecar's framework) decodes
// a non-UTF-8 multipart form field as latin-1, so a stray high byte would
// silently become different text on the sidecar side rather than failing
// loudly. Mutation-checked: removing the utf8.ValidString check lets the
// invalid line reach the fake server, failing requestCount.
func TestAlign_InvalidUTF8Rejected(t *testing.T) {
	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"words":[],"transcript":""}`))
	}))
	defer srv.Close()

	c := newClientForServer(t, srv, newTestBreaker())
	_, err := c.Align(context.Background(), newTestAudio(t), []string{"good", "bad\x85line"})
	if !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("Align error = %v; want ErrInvalidUTF8", err)
	}
	if got := requestCount.Load(); got != 0 {
		t.Fatalf("requestCount = %d; want 0 (invalid UTF-8 must skip the HTTP call)", got)
	}
}

// TestAlign_OversizedResponse is mutation-checked: relaxing the size bound to
// allow a truncated parse fails the Words-empty assertion.
func TestAlign_OversizedResponse(t *testing.T) {
	huge := strings.Repeat("a", maxResponseSize+1024)
	body := fmt.Sprintf(`{"words":[],"transcript":"%s"}`, huge)

	srv := jsonServer(body)
	defer srv.Close()

	c := newClientForServer(t, srv, newTestBreaker())
	res, err := c.Align(context.Background(), newTestAudio(t), []string{"line"})
	if err == nil {
		t.Fatal("Align: want error on oversized response, got nil")
	}
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("Align error = %v; want ErrResponseTooLarge", err)
	}
	if len(res.Words) != 0 || res.Transcript != "" {
		t.Fatalf("Align result = %+v; want zero-value Result (no partial parse)", res)
	}
}

// TestAlign_ResponseValidationFailures tables decodeAndValidate's checks: a
// negative start, an end before its own start, a non-monotonic start across
// words, and confidence above/below [0, 1]. NaN confidence fails at JSON
// decode instead (not valid JSON), so it still wants a non-nil error but not
// necessarily ErrInvalidResponse.
func TestAlign_ResponseValidationFailures(t *testing.T) {
	cases := []struct {
		name               string
		body               string
		wantInvalidSpecErr bool
	}{
		{"negative_start", `{"words":[{"text":"x","start_ms":-1,"end_ms":100,"line_index":0,"confidence":0.5}],"transcript":"x"}`, true},
		{"end_before_start", `{"words":[{"text":"x","start_ms":100,"end_ms":50,"line_index":0,"confidence":0.5}],"transcript":"x"}`, true},
		{"non_monotonic_start", `{"words":[{"text":"a","start_ms":500,"end_ms":600,"line_index":0,"confidence":0.5},{"text":"b","start_ms":100,"end_ms":200,"line_index":0,"confidence":0.5}],"transcript":"a b"}`, true},
		{"confidence_above_range", `{"words":[{"text":"x","start_ms":0,"end_ms":100,"line_index":0,"confidence":1.5}],"transcript":"x"}`, true},
		{"confidence_below_range", `{"words":[{"text":"x","start_ms":0,"end_ms":100,"line_index":0,"confidence":-0.1}],"transcript":"x"}`, true},
		{"confidence_nan", `{"words":[{"text":"x","start_ms":0,"end_ms":100,"line_index":0,"confidence":NaN}],"transcript":"x"}`, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv := jsonServer(tc.body)
			defer srv.Close()

			c := newClientForServer(t, srv, newTestBreaker())
			_, err := c.Align(context.Background(), newTestAudio(t), []string{"line"})
			if err == nil {
				t.Fatal("Align: want error, got nil")
			}
			if tc.wantInvalidSpecErr && !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("Align error = %v; want ErrInvalidResponse", err)
			}
		})
	}
}

// TestAlign_ContextTermination covers both ways a caller context can end a
// request in flight (cancel, deadline); each must surface the context's own
// sentinel error, not a generic transport one, and each gets its OWN breaker
// to assert a caller-initiated cancel/timeout is not breaker evidence.
// Mutation-checked: removing the ctx.Err() carve-out at the top of Align's
// transport-error branch falls through to c.breaker.Trip(), failing either
// subtest's Trips()==0 assertion when run alone (-run
// TestAlign_ContextTermination/cancel or /timeout).
func TestAlign_ContextTermination(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	t.Run("cancel", func(t *testing.T) {
		breaker := newTestBreaker()
		c := newClientForServer(t, srv, breaker)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()
		_, err := c.Align(ctx, newTestAudio(t), []string{"line"})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Align error = %v; want context.Canceled", err)
		}
		if got := breaker.Trips(); got != 0 {
			t.Fatalf("breaker.Trips() = %d; want 0 (a caller-initiated cancel is not breaker evidence)", got)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		breaker := newTestBreaker()
		c := newClientForServer(t, srv, breaker)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := c.Align(ctx, newTestAudio(t), []string{"line"})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Align error = %v; want context.DeadlineExceeded", err)
		}
		if got := breaker.Trips(); got != 0 {
			t.Fatalf("breaker.Trips() = %d; want 0 (a caller-initiated timeout is not breaker evidence)", got)
		}
	})
}

// TestAlign_BreakerOpenSkipsCall is mutation-checked: removing the
// breaker.Allow() gate at the top of Align fails the requestCount assertion.
func TestAlign_BreakerOpenSkipsCall(t *testing.T) {
	var requestCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	breaker := newTestBreaker()
	breaker.Trip() // opens the breaker
	c := newClientForServer(t, srv, breaker)
	_, err := c.Align(context.Background(), newTestAudio(t), []string{"line"})
	if !errors.Is(err, ErrBreakerOpen) {
		t.Fatalf("Align error = %v; want ErrBreakerOpen", err)
	}
	if got := requestCount.Load(); got != 0 {
		t.Fatalf("requestCount = %d; want 0 (breaker open must skip the HTTP call)", got)
	}
}

// TestAlign_HalfOpenRecoveryClosesBreaker drives a breaker through a full
// trip -> window-elapsed -> half-open -> successful-probe cycle using the
// breaker's own injectable clock (circuit.Breaker.SetClock), and asserts it
// ends closed. Mutation-checked: deleting Align's c.breaker.RecordSuccess()
// call leaves probing set after the successful probe, so the final
// breaker.Allow() still reads StateHalfOpen, failing the assertion.
func TestAlign_HalfOpenRecoveryClosesBreaker(t *testing.T) {
	srv := jsonServer(`{"words":[],"transcript":""}`)
	defer srv.Close()

	breaker := circuit.New(time.Minute, time.Hour)
	current := time.Now()
	breaker.SetClock(func() time.Time { return current })

	breaker.Trip() // opens the breaker for its trip-1 window (1 minute)
	if got := breaker.Allow(); got != circuit.StateOpen {
		t.Fatalf("breaker.Allow() = %v; want StateOpen immediately after Trip", got)
	}

	current = current.Add(time.Minute + time.Second) // past the window
	if got := breaker.Allow(); got != circuit.StateHalfOpen {
		t.Fatalf("breaker.Allow() = %v; want StateHalfOpen once the window elapses", got)
	}

	c := newClientForServer(t, srv, breaker)
	if _, err := c.Align(context.Background(), newTestAudio(t), []string{"line"}); err != nil {
		t.Fatalf("Align: %v", err)
	}

	if got := breaker.Allow(); got != circuit.StateClosed {
		t.Fatalf("breaker.Allow() = %v; want StateClosed after a successful probe", got)
	}
}

// TestAlign_TransportErrorTripsBreaker calls a closed TCP listener (a
// connection-refused transport failure, distinct from any HTTP status) and
// asserts it trips the breaker. Mutation-checked: deleting Align's
// c.breaker.Trip() call in the transport-error branch leaves Trips() at 0,
// failing the assertion.
func TestAlign_TransportErrorTripsBreaker(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	breaker := newTestBreaker()
	c, err := NewHTTPClient("http://"+addr, time.Second, breaker)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	if _, err := c.Align(context.Background(), newTestAudio(t), []string{"line"}); err == nil {
		t.Fatal("Align: want a transport error connecting to a closed listener, got nil")
	}
	if got := breaker.Trips(); got != 1 {
		t.Fatalf("breaker.Trips() = %d; want 1", got)
	}
}

// TestNewHTTPClient tables every construction-error case (unusable URL,
// non-positive timeout) plus the nil-breaker case (self-provisions one).
func TestNewHTTPClient(t *testing.T) {
	errCases := []struct {
		name    string
		url     string
		timeout time.Duration
	}{
		{"empty_url", "", time.Second},
		{"unparsable_url", "not a url", time.Second},
		{"unsupported_scheme", "ftp://example.com", time.Second},
		{"zero_timeout", "http://example.com", 0},
		{"negative_timeout", "http://example.com", -time.Second},
	}
	for _, tc := range errCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewHTTPClient(tc.url, tc.timeout, nil); err == nil {
				t.Fatalf("NewHTTPClient(%q, %v): want error, got nil", tc.url, tc.timeout)
			}
		})
	}

	t.Run("nil_breaker_is_usable", func(t *testing.T) {
		c, err := NewHTTPClient("http://example.com", time.Second, nil)
		if err != nil {
			t.Fatalf("NewHTTPClient: %v", err)
		}
		if c.Breaker() == nil {
			t.Fatal("Breaker() = nil; want a self-owned breaker when none is injected")
		}
	})
}

// TestAlign_NoLyricTextInErrors guards the privacy rule directly: a
// validation failure's error text must never contain the lyric line text or
// the audio file path, since callers may log this error above Debug.
func TestAlign_NoLyricTextInErrors(t *testing.T) {
	const secretLyric = "a very private lyric line nobody should see in logs"
	srv := jsonServer(`{"words":[{"text":"x","start_ms":100,"end_ms":50,"line_index":0,"confidence":0.5}],"transcript":"x"}`)
	defer srv.Close()

	audioPath := newTestAudio(t)
	c := newClientForServer(t, srv, newTestBreaker())
	_, err := c.Align(context.Background(), audioPath, []string{secretLyric})
	if err == nil {
		t.Fatal("Align: want error, got nil")
	}
	msg := err.Error()
	if strings.Contains(msg, secretLyric) {
		t.Fatalf("error text contains the lyric line: %q", msg)
	}
	if strings.Contains(msg, audioPath) {
		t.Fatalf("error text contains the audio path: %q", msg)
	}
}

// TestAlign_Preconditions checks Align's precondition checks, which run
// before any HTTP call or breaker consultation.
func TestAlign_Preconditions(t *testing.T) {
	c, err := NewHTTPClient("http://example.com", time.Second, nil)
	if err != nil {
		t.Fatalf("NewHTTPClient: %v", err)
	}
	if _, err := c.Align(context.Background(), "", []string{"line"}); err == nil {
		t.Fatal("Align: want error on empty audio path, got nil")
	}
	if _, err := c.Align(context.Background(), newTestAudio(t), nil); err == nil {
		t.Fatal("Align: want error on empty lines, got nil")
	}
}
