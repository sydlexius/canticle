package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/instrumentalmark"
	"github.com/sydlexius/canticle/internal/lyricblock"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/web"
)

// TestWithMarkActionsWiring proves the server-level seam: the option reaches the
// mounted web UI and enables the mark confirm route, while omitting it, or
// passing deps with no database path, leaves the same route answering 404.
func TestWithMarkActionsWiring(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	sqlDB, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	var id int64
	if err := sqlDB.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status)
		VALUES ('Some Artist', 'Some Title', 'k', 'k', 'Al', 'done') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("insert work item: %v", err)
	}
	deps := func(path string) web.MarkDeps {
		return web.MarkDeps{
			DB:           sqlDB,
			Instrumental: instrumentalmark.New(sqlDB, lyrics.NewLRCWriter(t.TempDir())),
			Blocks:       lyricblock.New(sqlDB, slog.Default(), nil),
			DBPath:       path,
		}
	}
	route := "/queue/" + strconv.FormatInt(id, 10) + "/instrumental"

	cases := []struct {
		name string
		opts []Option
		want int
	}{
		{"wired", []Option{WithMarkActions(deps(dbPath))}, http.StatusOK},
		{"option omitted", nil, http.StatusNotFound},
		{"disabled deps (no db path)", []Option{WithMarkActions(deps(""))}, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := append([]Option{WithWebUI(config.Config{}, "vtest"), WithReportsDB(sqlDB)}, tc.opts...)
			h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics", opts...)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, route, nil))
			if rec.Code != tc.want {
				t.Fatalf("GET %s = %d, want %d", route, rec.Code, tc.want)
			}
		})
	}
}
