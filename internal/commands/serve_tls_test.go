package commands

import (
	"bytes"
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/lyrics"
	"github.com/sydlexius/mxlrcgo-svc/internal/musixmatch"
)

// freePort returns a currently-free TCP address on loopback. There is a small
// race between closing the probe listener and the server binding it, acceptable
// for a local integration test.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("probe close: %v", err)
	}
	return addr
}

// TestRunServe_TLSSelfSignedServesHTTPS is the lane-5 integration test: runServe
// with [server.tls].self_signed brings up an HTTPS listener that serves /healthz
// over TLS (handler sees r.TLS), persists the self-signed pair under
// <dir(db_path)>/tls/, and the optional redirect listener 301s plain HTTP to the
// HTTPS address.
func TestRunServe_TLSSelfSignedServesHTTPS(t *testing.T) {
	// runServe -> initLogging -> logging.Init calls slog.SetDefault, mutating the
	// process-global default logger. Snapshot and restore it so this test does not
	// pollute others under -shuffle=on.
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	t.Setenv("MXLRC_DOCKER", "")
	t.Setenv("MUSIXMATCH_TOKEN", "tok")
	keyFile := filepath.Join(t.TempDir(), "test.key")
	t.Setenv("MXLRC_SECRETS_KEY_FILE", keyFile)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "serve.db")
	httpsAddr := freePort(t)
	redirectAddr := freePort(t)

	cfgPath := filepath.Join(dir, "config.toml")
	var b strings.Builder
	b.WriteString("[db]\npath = " + tomlString(dbPath) + "\n\n")
	b.WriteString("[providers]\nprimary = \"musixmatch\"\n\n")
	b.WriteString("[server]\naddr = " + tomlString(httpsAddr) + "\n\n")
	b.WriteString("[server.tls]\nself_signed = true\nredirect_http = " + tomlString(redirectAddr) + "\n")
	if err := os.WriteFile(cfgPath, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var out bytes.Buffer
	go func() {
		done <- runServe(
			ctx,
			&out,
			ServeCmd{ConfigPath: cfgPath},
			func(string) musixmatch.Fetcher { return fakeFetcher{} },
			func(...string) lyrics.Writer { return fakeWriter{} },
		)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("runServe did not return after cancel")
		}
	})

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // G402: test trusts the locally generated self-signed cert.
		},
		Timeout: 2 * time.Second,
	}

	// Poll the HTTPS health endpoint until the listener is up.
	healthURL := "https://" + httpsAddr + "/healthz"
	var resp *http.Response
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = client.Get(healthURL)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("HTTPS health never came up: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status = %d; want 200", resp.StatusCode)
	}
	if resp.TLS == nil {
		t.Error("response not served over TLS")
	}

	// The self-signed pair must be persisted under <dir(db_path)>/tls/.
	if _, err := os.Stat(filepath.Join(dir, "tls", "cert.pem")); err != nil {
		t.Errorf("self-signed cert not persisted: %v", err)
	}

	// The redirect listener 301s plain HTTP to the HTTPS address.
	noRedirect := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	var rresp *http.Response
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rresp, err = noRedirect.Get("http://" + redirectAddr + "/config")
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("redirect listener never came up: %v", err)
	}
	defer func() { _ = rresp.Body.Close() }()
	if rresp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("redirect status = %d; want 301", rresp.StatusCode)
	}
	loc := rresp.Header.Get("Location")
	if !strings.HasPrefix(loc, "https://") || !strings.HasSuffix(loc, "/config") {
		t.Errorf("redirect Location = %q; want https://...:<port>/config", loc)
	}
}

// TestRunServe_TLSBadCertFailsFast: a cert_file/key_file pointing at unreadable
// PEM makes runServe fail before serving (exit 1).
func TestRunServe_TLSBadCertFailsFast(t *testing.T) {
	// runServe -> initLogging -> logging.Init calls slog.SetDefault, mutating the
	// process-global default logger. Snapshot and restore it so this test does not
	// pollute others under -shuffle=on.
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	t.Setenv("MXLRC_DOCKER", "")
	t.Setenv("MUSIXMATCH_TOKEN", "tok")
	keyFile := filepath.Join(t.TempDir(), "test.key")
	t.Setenv("MXLRC_SECRETS_KEY_FILE", keyFile)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "serve.db")
	badCert := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(badCert, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.toml")
	var b strings.Builder
	b.WriteString("[db]\npath = " + tomlString(dbPath) + "\n\n")
	b.WriteString("[providers]\nprimary = \"musixmatch\"\n\n")
	b.WriteString("[server.tls]\ncert_file = " + tomlString(badCert) + "\nkey_file = " + tomlString(badCert) + "\n")
	if err := os.WriteFile(cfgPath, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var out bytes.Buffer
	code := runServe(
		context.Background(),
		&out,
		ServeCmd{ConfigPath: cfgPath},
		func(string) musixmatch.Fetcher { return fakeFetcher{} },
		func(...string) lyrics.Writer { return fakeWriter{} },
	)
	if code != 1 {
		t.Fatalf("bad cert: exit code = %d, want 1", code)
	}
}
