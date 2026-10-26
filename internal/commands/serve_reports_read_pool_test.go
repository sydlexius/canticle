package commands

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/musixmatch"
)

// writeWebUIServeConfig writes a serve config with the web UI on and returns
// its path, the database path and the listen address. extra is appended
// verbatim to [server].
func writeWebUIServeConfig(t *testing.T, extra string) (cfgPath, dbPath, addr string) {
	t.Helper()
	return writeServeConfigUI(t, true, extra)
}

// writeServeConfigUI is writeWebUIServeConfig with the web UI switch exposed.
func writeServeConfigUI(t *testing.T, webUI bool, extra string) (cfgPath, dbPath, addr string) {
	t.Helper()
	t.Setenv("MXLRC_DOCKER", "")
	t.Setenv("MUSIXMATCH_TOKEN", "tok")
	t.Setenv("MXLRC_SECRETS_KEY_FILE", filepath.Join(t.TempDir(), "test.key"))
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "serve.db")
	cfgPath = filepath.Join(dir, "config.toml")
	addr = freePort(t)
	cfg := "[db]\npath = " + tomlString(dbPath) + "\n\n" +
		"[providers]\nprimary = \"musixmatch\"\n\n" +
		"[server]\naddr = " + tomlString(addr) + "\nweb_ui_enabled = " + strconv.FormatBool(webUI) + "\n" + extra
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath, dbPath, addr
}

// TestRunServe_ReportsReadPoolOpenFailureAbortsStartup pins the #1649 fail-fast:
// when the web UI's read pool cannot open, serve exits 1 before it listens
// rather than falling back to the writer. The database file is removed just
// before the pool opens, so the writer (holding its descriptor) is up but the
// pool's stat fails.
func TestRunServe_ReportsReadPoolOpenFailureAbortsStartup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cannot remove a database file the writer still holds open on Windows")
	}
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	cfgPath, dbPath, _ := writeWebUIServeConfig(t, "")

	var mu sync.Mutex
	var fired []string
	var removeErr error
	serveStartupOrderHook = func(c string) {
		mu.Lock()
		defer mu.Unlock()
		fired = append(fired, c)
		if c == "reports_read_pool_opening" {
			removeErr = os.Remove(dbPath)
		}
	}
	t.Cleanup(func() { serveStartupOrderHook = nil })

	done := make(chan int, 1)
	go func() {
		done <- runServe(context.Background(), &bytes.Buffer{}, ServeCmd{ConfigPath: cfgPath},
			func(string) musixmatch.Fetcher { return fakeFetcher{} },
			func(...string) lyrics.Writer { return fakeWriter{} })
	}()
	var code int
	select {
	case code = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("runServe did not return after the read pool failed to open")
	}

	mu.Lock()
	defer mu.Unlock()
	if removeErr != nil {
		t.Fatalf("remove db before the pool opened: %v", removeErr)
	}
	if code != 1 {
		t.Errorf("runServe code = %d; want 1 when the read pool cannot open", code)
	}
	if slices.Contains(fired, "http_listening") {
		t.Errorf("checkpoints = %v; serve listened despite the read pool failure", fired)
	}
}

// TestRunServe_WebUIStartsWithReportsReadPool pins the #1649 success path: with
// the web UI on, serve opens the read pool after the writer migrated the file,
// comes all the way up (the pool open is not a startup failure on a healthy
// database) and shuts down cleanly. Startup is canceled once the worker starts.
func TestRunServe_WebUIStartsWithReportsReadPool(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	cfgPath, _, _ := writeWebUIServeConfig(t, "")

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var fired []string
	serveStartupOrderHook = func(c string) {
		mu.Lock()
		fired = append(fired, c)
		mu.Unlock()
		if c == "worker_starting" {
			cancel()
		}
	}
	t.Cleanup(func() { serveStartupOrderHook = nil })

	done := make(chan int, 1)
	go func() {
		done <- runServe(runCtx, &bytes.Buffer{}, ServeCmd{ConfigPath: cfgPath},
			func(string) musixmatch.Fetcher { return fakeFetcher{} },
			func(...string) lyrics.Writer { return fakeWriter{} })
	}()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("runServe code = %d; want 0 (web UI serve with its read pool on a healthy db)", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runServe did not return after cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	pool, listening := slices.Index(fired, "reports_read_pool_opening"), slices.Index(fired, "http_listening")
	if pool < 0 || listening < 0 || pool > listening {
		t.Errorf("checkpoints = %v; want the read pool opened before the listener", fired)
	}
	closed, dbClosing := slices.Index(fired, "reports_read_pool_closed"), slices.Index(fired, "db_closing")
	if dbClosing < 0 {
		t.Errorf("checkpoints = %v; want a clean shutdown through db_closing", fired)
	}
	if closed < 0 || closed > dbClosing {
		t.Errorf("checkpoints = %v; want the read pool closed before the writer (db_closing)", fired)
	}
}

// TestRunServe_WebUIReportsReadPastHeldWriter pins that runServe actually hands
// the read pool to the web UI (#1649): with the writer's single connection held,
// a live GET /dashboard on the listener must still render. Without the
// WithReportsReadDB wiring the reports would read through that held connection
// and the request would hit its deadline. Loopback is a trusted network, so the
// request needs no session, only an existing admin (seeded through the writer).
func TestRunServe_WebUIReportsReadPastHeldWriter(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	cfgPath, _, addr := writeWebUIServeConfig(t, "")

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var writer *sql.DB
	var seedErr error
	serveWriterDBHook = func(d *sql.DB) {
		writer = d
		_, seedErr = d.ExecContext(runCtx,
			`INSERT INTO users (id, username, password_hash) VALUES ('u1', 'admin', 'x')`)
	}
	listening := make(chan struct{})
	serveStartupOrderHook = func(c string) {
		if c == "http_listening" {
			close(listening)
		}
	}
	t.Cleanup(func() { serveWriterDBHook, serveStartupOrderHook = nil, nil })

	done := make(chan int, 1)
	go func() {
		done <- runServe(runCtx, &bytes.Buffer{}, ServeCmd{ConfigPath: cfgPath},
			func(string) musixmatch.Fetcher { return fakeFetcher{} },
			func(...string) lyrics.Writer { return fakeWriter{} })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("runServe did not return after cancel")
		}
	})
	select {
	case <-listening:
	case code := <-done:
		t.Fatalf("runServe exited %d before listening", code)
	case <-time.After(15 * time.Second):
		t.Fatal("runServe did not start listening")
	}
	if seedErr != nil {
		t.Fatalf("seed admin through the writer: %v", seedErr)
	}

	client := &http.Client{
		Timeout:       3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	// Drop the keep-alive connection before shutdown, which would otherwise
	// wait out its grace period on it.
	defer client.CloseIdleConnections()
	get := func(when string) {
		t.Helper()
		resp, err := client.Get("http://" + addr + "/dashboard")
		if err != nil {
			t.Fatalf("GET /dashboard %s: %v", when, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /dashboard %s = %d, want 200", when, resp.StatusCode)
		}
	}
	holdCtx, holdCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer holdCancel()
	conn, err := writer.Conn(holdCtx)
	if err != nil {
		t.Fatalf("hold the writer's connection: %v", err)
	}
	defer func() { _ = conn.Close() }()
	get("with the writer's connection held")

	// GET /readyz pings the read pool too (#1652): a probe must not time out
	// behind the held writer and flip a healthy service to unavailable.
	resp, err := client.Get("http://" + addr + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz with the writer's connection held: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz with the writer's connection held = %d, want 200", resp.StatusCode)
	}
}

// TestRunServe_ReadyzPastHeldWriterWithWebUIOff pins that the readiness probe
// uses the read pool even when the web UI is off (#1652): a headless daemon's
// Docker healthcheck must not go unhealthy while a long write holds the
// writer's single connection.
func TestRunServe_ReadyzPastHeldWriterWithWebUIOff(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	cfgPath, _, addr := writeServeConfigUI(t, false, "")

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var writer *sql.DB
	serveWriterDBHook = func(d *sql.DB) { writer = d }
	listening := make(chan struct{})
	serveStartupOrderHook = func(c string) {
		if c == "http_listening" {
			close(listening)
		}
	}
	t.Cleanup(func() { serveWriterDBHook, serveStartupOrderHook = nil, nil })

	done := make(chan int, 1)
	go func() {
		done <- runServe(runCtx, &bytes.Buffer{}, ServeCmd{ConfigPath: cfgPath},
			func(string) musixmatch.Fetcher { return fakeFetcher{} },
			func(...string) lyrics.Writer { return fakeWriter{} })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("runServe did not return after cancel")
		}
	})
	select {
	case <-listening:
	case code := <-done:
		t.Fatalf("runServe exited %d before listening", code)
	case <-time.After(15 * time.Second):
		t.Fatal("runServe did not start listening")
	}

	holdCtx, holdCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer holdCancel()
	conn, err := writer.Conn(holdCtx)
	if err != nil {
		t.Fatalf("hold the writer's connection: %v", err)
	}
	defer func() { _ = conn.Close() }()

	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Get("http://" + addr + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz with the writer's connection held and the web UI off: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /readyz with the writer held and the web UI off = %d, want 200", resp.StatusCode)
	}
}
