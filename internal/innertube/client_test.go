package innertube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- AC: status codes map to sentinels; body cap and redirect pinning ---

func TestStatusError_MapsSentinels(t *testing.T) {
	cases := []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusForbidden, ErrForbidden},
		{http.StatusTooManyRequests, ErrRateLimited},
		{http.StatusBadRequest, ErrClientVersion},
	}
	for _, tc := range cases {
		err := statusError(tc.status)
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: got %v, want wrapping %v", tc.status, err, tc.want)
		}
	}
	if statusError(http.StatusOK) != nil {
		t.Error("200 should map to nil")
	}
}

// TestErrClientVersion_WrapsErrForbidden pins errors.go's documented
// wrapping relationship, exercised through this client's own mapping.
func TestErrClientVersion_WrapsErrForbidden(t *testing.T) {
	if !errors.Is(ErrClientVersion, ErrForbidden) {
		t.Fatal("ErrClientVersion must wrap ErrForbidden")
	}
}

// TestPostJSON_SendsAndReturnsBody pins the plumbing postJSON is responsible
// for -- method, path, the API key query parameter, the JSON content type,
// an identifying User-Agent, the marshaled body -- and asserts the RESPONSE
// BYTES it hands back, which is the value the three call methods parse.
func TestPostJSON_SendsAndReturnsBody(t *testing.T) {
	const wantBody = `{"ok":true}`

	var (
		gotMethod, gotPath, gotKey string
		gotUA, gotContentType      string
		gotPayload                 map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotKey = r.URL.Query().Get("key")
		gotUA = r.Header.Get("User-Agent")
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotPayload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(wantBody))
	}))
	t.Cleanup(srv.Close)

	c := NewClient()
	c.baseURL = srv.URL

	raw, err := c.postJSON(context.Background(), "/some/path", map[string]string{"field": "value"})
	if err != nil {
		t.Fatalf("postJSON: %v", err)
	}
	if string(raw) != wantBody {
		t.Errorf("postJSON returned %q, want %q", raw, wantBody)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/some/path" {
		t.Errorf("path = %q, want /some/path", gotPath)
	}
	if gotKey != apiKey {
		t.Error("postJSON must send the API key as a query parameter")
	}
	if gotContentType != "application/json" {
		t.Errorf("content type = %q, want application/json", gotContentType)
	}
	if gotUA == "" || strings.HasPrefix(gotUA, "Go-http-client") {
		t.Errorf("client must send an identifying User-Agent, got %q", gotUA)
	}
	if !strings.Contains(gotUA, "canticle") {
		t.Errorf("User-Agent should identify canticle, got %q", gotUA)
	}
	if gotPayload["field"] != "value" {
		t.Errorf("body not marshaled through: got %v", gotPayload)
	}
}

// TestPostJSON_MapsStatusToSentinel checks the status mapping reaches callers
// through postJSON, not only through statusError in isolation.
func TestPostJSON_MapsStatusToSentinel(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrUnauthorized},
		{http.StatusForbidden, ErrForbidden},
		{http.StatusTooManyRequests, ErrRateLimited},
		{http.StatusBadRequest, ErrClientVersion},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(srv.Close)
			c := NewClient()
			c.baseURL = srv.URL

			raw, err := c.postJSON(context.Background(), "/x", map[string]string{})
			if !errors.Is(err, tc.want) {
				t.Errorf("want %v, got %v", tc.want, err)
			}
			if raw != nil {
				t.Errorf("want nil bytes on a non-200, got %d bytes", len(raw))
			}
		})
	}
}

func TestReadBody_CapsResponseSize(t *testing.T) {
	oversized := strings.Repeat("a", maxResponseSize+1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(oversized))
	}))
	t.Cleanup(srv.Close)
	c := NewClient()
	c.baseURL = srv.URL

	_, err := c.postJSON(context.Background(), "/x", map[string]string{})
	if err == nil {
		t.Fatal("want an error for an oversized response")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error should report the size cap, got: %v", err)
	}
}

// --- AC: context cancellation is honored ---

func TestPostJSON_HonorsContextCancellation(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	defer close(block)
	t.Cleanup(srv.Close)

	c := NewClient()
	c.baseURL = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.postJSON(ctx, "/x", map[string]string{})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error for a canceled context")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("want context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("postJSON did not honor context cancellation within 5s")
	}
}

func TestPostJSON_HonorsContextDeadline(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	defer close(block)
	t.Cleanup(srv.Close)

	c := NewClient()
	c.baseURL = srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.postJSON(ctx, "/x", map[string]string{})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("want context.DeadlineExceeded, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("postJSON did not honor the context deadline within 5s")
	}
}

// --- AC: no live network access in any test ---
//
// Every test above targets an httptest.Server via an injected baseURL; none
// resolves a real host. NewClient's default baseURL (music.youtube.com) is
// never dialed because every test overrides c.baseURL immediately after
// construction.

// TestNewClient_DefaultsAreSane is a smoke test for NewClient itself,
// asserting the defaults without making any request.
func TestNewClient_DefaultsAreSane(t *testing.T) {
	c := NewClient()
	if c.baseURL != defaultBaseURL {
		t.Errorf("baseURL = %q, want %q", c.baseURL, defaultBaseURL)
	}
	if c.Name() != "innertube" {
		t.Errorf("Name() = %q, want innertube", c.Name())
	}
	if c.httpClient.CheckRedirect == nil {
		t.Error("NewClient must wire the SSRF redirect guard")
	}
}

// TestParseDurationSeconds pins the duration-text-run parser used by search
// candidate extraction, including its fail-open-on-unparsable contract
