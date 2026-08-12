package petitlyrics

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// captureTransport tees every response body to a file before handing the
// response on, so a probe can keep the raw bytes that Client.request parses and
// discards.
//
// It must NOT consume the body: the client still has to read it. Teeing means
// read-all then restore, never read-and-forget.
type captureTransport struct {
	base http.RoundTripper
	dir  string

	mu sync.Mutex
	n  int
}

func newCaptureTransport(dir string) *captureTransport {
	return &captureTransport{base: http.DefaultTransport, dir: dir}
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := c.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	body, readErr := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("capture transport: read body: %w", readErr)
	}

	c.mu.Lock()
	idx := c.n
	c.n++
	c.mu.Unlock()

	// Named by index only. A filename must never carry a track title.
	path := filepath.Join(c.dir, fmt.Sprintf("sample-%04d.xml", idx))
	if writeErr := os.WriteFile(path, body, 0o600); writeErr != nil {
		return nil, fmt.Errorf("capture transport: write %s: %w", path, writeErr)
	}

	// Restore the body so the client can still read it.
	res.Body = io.NopCloser(bytesReader(body))
	return res, nil
}

func (c *captureTransport) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// TestCaptureTransport_TeesBodyAndLeavesItReadable is the load-bearing test for
// this wrapper. A transport that captures the body but forgets to restore it
// breaks every request while looking correct in isolation, and that failure
// would only surface against the live API, mid-sweep.
func TestCaptureTransport_TeesBodyAndLeavesItReadable(t *testing.T) {
	dir := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		_, _ = w.Write([]byte(`<response><songs><song><lyricsId>1</lyricsId></song></songs></response>`))
	}))
	t.Cleanup(srv.Close)

	ct := newCaptureTransport(dir)
	client := &http.Client{Transport: ct}

	res, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	// The client must still be able to read the body.
	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if !strings.Contains(string(got), "<lyricsId>1</lyricsId>") {
		t.Errorf("restored body is not readable by the caller: got %q", got)
	}

	// And the capture file must hold the same bytes.
	captured, err := os.ReadFile(filepath.Join(dir, "sample-0000.xml"))
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	if string(captured) != string(got) {
		t.Errorf("capture and restored body differ:\ncaptured: %q\nrestored: %q", captured, got)
	}

	if ct.count() != 1 {
		t.Errorf("count() = %d, want 1", ct.count())
	}
}

// bytesReader is a tiny indirection so the RoundTrip body restore reads clearly.
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
