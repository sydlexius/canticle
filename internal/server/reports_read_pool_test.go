package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/web"
)

// TestWithReportsReadDBSplitsReadsFromWrites pins the #1649 wiring: the
// dashboard's reports read through the WithReportsReadDB pool, so a render
// completes while the writer's single connection is held by an open write
// transaction, and the revive write still goes through the WithReportsDB
// writer (the query-only pool would reject it).
func TestWithReportsReadDBSplitsReadsFromWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	writer, err := dbtest.OpenAt(ctx, t, path)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	var rowID int64
	if err := writer.QueryRowContext(ctx, `INSERT INTO work_queue (artist, title, artist_key, title_key, status, last_error)
		VALUES ('a', 't', 'a', 't', 'unavailable', 'miss limit reached') RETURNING id`).Scan(&rowID); err != nil {
		t.Fatalf("seed: %v", err)
	}
	pool, err := db.OpenReadPool(ctx, path, 4)
	if err != nil {
		t.Fatalf("open read pool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics",
		WithWebUI(config.Config{}, "vtest"),
		WithReportsDB(writer),
		WithReportsReadDB(pool))

	t.Run("dashboard renders while the writer is held", func(t *testing.T) {
		tx, err := writer.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, `INSERT INTO libraries (path, name) VALUES ('/held', 'Held')`); err != nil {
			t.Fatalf("uncommitted insert: %v", err)
		}
		// Well under the 15s request timeout; a render waiting on the held
		// writer connection would sit here until the deadline cancels it.
		reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil).WithContext(reqCtx))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /dashboard with the writer held = %d, want 200", rec.Code)
		}
	})

	t.Run("revive writes through the writer", func(t *testing.T) {
		const token = "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"
		form := url.Values{"csrf_token": {token}, "expected": {"1"}}
		req := httptest.NewRequest(http.MethodPost, "/queue/unavailable/revive", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: web.CSRFCookieName, Value: token})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST revive = %d, want 200", rec.Code)
		}
		var status string
		if err := writer.QueryRowContext(ctx, `SELECT status FROM work_queue WHERE id = ?`, rowID).Scan(&status); err != nil {
			t.Fatalf("read status: %v", err)
		}
		if status == "unavailable" {
			t.Error("revive left the row unavailable; the write did not land")
		}
	})
}
