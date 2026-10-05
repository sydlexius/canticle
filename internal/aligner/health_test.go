package aligner

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/circuit"
)

// paddedBody returns a healthy JSON body of exactly n bytes.
func paddedBody(n int) string {
	const head, tail = `{"status":"ok","pad":"`, `"}`
	return head + strings.Repeat("x", n-len(head)-len(tail)) + tail
}

func TestHealthStatuses(t *testing.T) {
	okBody := `{"status":"ok","device":"cpu"}`
	cases := []struct {
		name    string
		status  int
		body    string
		healthy bool
	}{
		{"ok", http.StatusOK, okBody, true},
		{"other status field", http.StatusOK, `{"status":"loading"}`, false},
		{"missing status field", http.StatusOK, `{}`, false},
		{"non-200", http.StatusServiceUnavailable, okBody, false},
		{"malformed JSON", http.StatusOK, `{"status":`, false},
		{"oversized body", http.StatusOK, `{"status":"ok","pad":"` + strings.Repeat("x", maxHealthResponseSize) + `"}`, false},
		{"exactly at the bound", http.StatusOK, paddedBody(maxHealthResponseSize), true},
		{"one byte over the bound", http.StatusOK, paddedBody(maxHealthResponseSize + 1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotMethod string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotMethod = r.URL.Path, r.Method
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			breaker := newTestBreaker()
			err := newClientForServer(t, srv, breaker).Health(context.Background())
			if (err == nil) != tc.healthy {
				t.Fatalf("Health() error = %v, want healthy=%v", err, tc.healthy)
			}
			if !tc.healthy && !errors.Is(err, ErrUnhealthy) {
				t.Errorf("Health() error = %v, want ErrUnhealthy", err)
			}
			if gotPath != "/health" || gotMethod != http.MethodGet {
				t.Errorf("probe was %s %s, want GET /health", gotMethod, gotPath)
			}
			if breaker.Trips() != 0 || breaker.EverSucceeded() {
				t.Error("a health probe moved the breaker")
			}
		})
	}
}

func TestHealthURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://h:1":           "http://h:1/health",
		"http://h:1/":          "http://h:1/health",
		"http://h:1/align":     "http://h:1/health",
		"http://h:1/x/align":   "http://h:1/x/health",
		"http://h:1/x/":        "http://h:1/x/health",
		"http://align":         "http://align/health",
		"http://align/align":   "http://align/health",
		"http://h:1/x?key=abc": "http://h:1/x/health",
		"http://h:1/x#frag":    "http://h:1/x/health",
		"http://u:p@h:1/x/":    "http://u:p@h:1/x/health",
	} {
		c := &HTTPClient{baseURL: in}
		got, err := c.healthURL()
		if err != nil || got != want {
			t.Errorf("healthURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := (&HTTPClient{baseURL: "http://h:1/%zz?secret=1"}).healthURL(); err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("unparsable base: error = %v, want one without the url", err)
	}
}

// A transport error never carries the configured url, userinfo, token or host.
func TestHealthErrorOmitsConfiguredURL(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	c := &HTTPClient{baseURL: "http://user:hunter2@" + addr + "/tok-sekrit", client: &http.Client{Timeout: time.Second}}
	err = c.Health(context.Background())
	if err == nil {
		t.Fatal("Health() succeeded against a closed port")
	}
	for _, leak := range []string{"hunter2", "tok-sekrit", addr, "user"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q leaks %q", err, leak)
		}
	}
}

// The read bound holds on a body that never ends: refused well inside 3 s.
func TestHealthBoundsEndlessBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := []byte(strings.Repeat("x", 4096))
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	start := time.Now()
	err := newClientForServer(t, srv, nil).Health(context.Background())
	if !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("Health() error = %v, want ErrUnhealthy", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Health() took %v on an endless body", d)
	}
}

// An open breaker neither blocks nor is moved by a probe.
func TestHealthIgnoresOpenBreaker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	breaker := newTestBreaker()
	breaker.Trip()
	if breaker.Trips() == 0 || breaker.Allow() != circuit.StateOpen {
		t.Fatal("could not open the breaker")
	}
	before := breaker.Trips()
	if err := newClientForServer(t, srv, breaker).Health(context.Background()); err != nil {
		t.Fatalf("Health() = %v with an open breaker, want nil", err)
	}
	if breaker.Trips() != before {
		t.Errorf("Trips() = %d, want %d", breaker.Trips(), before)
	}
}

// A redirect is never followed: the target would answer a healthy 200.
func TestHealthDoesNotFollowRedirect(t *testing.T) {
	followed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	err := newClientForServer(t, srv, nil).Health(context.Background())
	if !errors.Is(err, ErrUnhealthy) || followed {
		t.Fatalf("Health() error = %v, followed = %v; want ErrUnhealthy and no follow", err, followed)
	}
}

// hangingServer answers nothing until the test ends.
func hangingServer(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	return srv
}

// The probe's own 3 s deadline, not the client's 5 s timeout, ends the call.
func TestHealthTimesOut(t *testing.T) {
	c := newClientForServer(t, hangingServer(t), nil)
	start := time.Now()
	err := c.Health(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Health() error = %v, want a deadline error", err)
	}
	if d := time.Since(start); d < healthTimeout || d > 4900*time.Millisecond {
		t.Errorf("Health() returned after %v, want about %v", d, healthTimeout)
	}
}

func TestHealthContextCancelReturnsPromptly(t *testing.T) {
	c := newClientForServer(t, hangingServer(t), nil)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	err := c.Health(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Health() error = %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Health() took %v after a cancel", d)
	}
}
