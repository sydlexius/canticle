package worker

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/orchestrator"
	"github.com/sydlexius/canticle/internal/petitlyrics"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
)

const refusedToken = "synthetic-usertoken-1372"

type rewriteTransport struct {
	target *url.URL
	next   http.RoundTripper
}

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = rt.target.Scheme, rt.target.Host
	return rt.next.RoundTrip(r)
}

// refusingMusixmatch is the REAL Musixmatch client pointed at a local server
// that answers every request the way an edge refusal does (#1372): HTTP 403
// with an HTML body. Clearing refuse makes it answer 404, an ordinary miss.
func refusingMusixmatch(t *testing.T) (client *musixmatch.Client, hits *atomic.Int64, refuse *atomic.Bool) {
	t.Helper()
	hits, refuse = &atomic.Int64{}, &atomic.Bool{}
	refuse.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if !refuse.Load() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<html><head><title>403 Forbidden</title></head><body>"+strings.Repeat("edge refusal ", 200)+"</body></html>")
	}))
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	// The client has no exported HTTP seam and uses the default transport, so
	// the redirect is installed there for the life of this (non-parallel) test.
	prev := http.DefaultTransport
	http.DefaultTransport = rewriteTransport{target: target, next: prev}
	t.Cleanup(func() { http.DefaultTransport = prev })
	return musixmatch.NewClient(refusedToken), hits, refuse
}

// titleFetcher is the healthy second lane: it has lyrics for "Held ..." titles
// and answers a clean no-match for everything else.
type titleFetcher struct{}

func (titleFetcher) FindLyrics(_ context.Context, t models.Track) (models.Song, error) {
	if strings.HasPrefix(t.TrackName, "Held") {
		return syncedTrackSong(t), nil
	}
	return models.Song{}, petitlyrics.ErrNoMatch
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRun_RefusedLaneDoesNotStallQueue is the #1372 reproduction over the real
// worker, orchestrator, Musixmatch client and SQLite queue: one lane is refused
// with HTTP 403 on every call, the other is healthy. The queue must keep
// draining from the healthy lane with no worker-global backoff, the refused
// lane must be asked once (not once per row) and reported as refused, and it
// must be asked again, and close, once the provider answers.
func TestRun_RefusedLaneDoesNotStallQueue(t *testing.T) {
	for _, mode := range []string{orchestrator.ModeOrdered, orchestrator.ModeParallel} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			logs := &lockedBuffer{}
			prevLog := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
			t.Cleanup(func() { slog.SetDefault(prevLog) })

			sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatalf("db.Open: %v", err)
			}
			t.Cleanup(func() { _ = sqlDB.Close() })
			q := queue.NewDBQueue(sqlDB)
			q.SetRandomized(false)
			enqueue := func(title string) {
				t.Helper()
				if _, err := q.Enqueue(ctx, models.Inputs{
					Track:      models.Track{ArtistName: "Synthetic Artist", TrackName: title},
					Outdir:     "/out",
					Filename:   title + ".lrc",
					SourcePath: "/library/" + title + ".flac",
				}, queue.PriorityScan); err != nil {
					t.Fatalf("enqueue %q: %v", title, err)
				}
			}
			// A row the healthy lane misses comes first, so the refusal is seen by a
			// dispatch in which both lanes report (parallel cancels a loser).
			for _, title := range []string{"Missing 1", "Held 1", "Missing 2", "Held 2", "Held 3"} {
				enqueue(title)
			}

			client, hits, refuse := refusingMusixmatch(t)
			writer := &capturingWriter{}
			w := New(q, cache.New(sqlDB), client, writer)
			w.SetFallbackProviders(providers.New(providers.PetitLyrics, titleFetcher{}))
			w.SetProviderRecorder(q)
			w.SetRecordingEnrichmentDefault(false)
			w.SetProvidersMode(mode)
			now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
			w.setClock(func() time.Time { return now })
			var delays []time.Duration
			w.sleep = func(_ context.Context, d time.Duration) { delays = append(delays, d) }

			if err := w.Run(ctx); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if len(delays) != 0 {
				t.Errorf("worker-global backoff delays = %v; want none while the healthy lane answers", delays)
			}
			type rowState struct {
				status, lane, lastErr string
				misses, attempts      int
			}
			row := func(title string) (r rowState) {
				t.Helper()
				if err := sqlDB.QueryRowContext(ctx, `SELECT status, COALESCE(provider_lane, ''), COALESCE(last_error, ''), miss_count, attempts
					FROM work_queue WHERE title = ?`, title).Scan(&r.status, &r.lane, &r.lastErr, &r.misses, &r.attempts); err != nil {
					t.Fatalf("read row %q: %v", title, err)
				}
				return r
			}
			for _, title := range []string{"Held 1", "Held 2", "Held 3"} {
				if r := row(title); r.status != "done" || r.lane != providers.PetitLyrics {
					t.Errorf("%q = %+v; want done from the healthy lane", title, r)
				}
			}
			// The row that met the refusal is retried on its own schedule and is not
			// charged a miss: the refused lane gave no catalog answer.
			met := row("Missing 1")
			if met.status != "failed" || met.misses != 0 || met.attempts != 1 {
				t.Errorf("row that met the refusal = %+v; want failed, 0 misses, 1 attempt", met)
			}
			if len(met.lastErr) > 300 || strings.Contains(met.lastErr, refusedToken) {
				t.Errorf("last_error is unbounded or carries the token (%d bytes): %q", len(met.lastErr), met.lastErr)
			}
			if r := row("Missing 2"); r.status != "deferred" || r.misses != 1 {
				t.Errorf("row missed while the lane was open = %+v; want an ordinary deferred miss", r)
			}
			if got := hits.Load(); got != 1 {
				t.Errorf("refused lane was asked %d times for 5 rows; want 1 (its breaker must open)", got)
			}
			out := logs.String()
			if !strings.Contains(out, "refused by the provider") || !strings.Contains(out, "provider=musixmatch") {
				t.Errorf("no distinct refusal log for the lane; logs:\n%s", out)
			}
			if strings.Contains(out, refusedToken) || strings.Contains(out, strings.Repeat("edge refusal ", 20)) {
				t.Errorf("logs carry the token or an unbounded response body:\n%s", out)
			}
			if h := w.LaneHealth()[0]; h.State != orchestrator.LaneStateOpen || !h.Refused {
				t.Errorf("refused lane health = %+v; want open and Refused", h)
			}

			// Recovery without a restart: the provider answers again and the open
			// window has elapsed, so the next row probes the lane and closes it.
			refuse.Store(false)
			now = now.Add(2 * time.Hour)
			enqueue("Missing 3")
			if err := w.Run(ctx); err != nil {
				t.Fatalf("Run after recovery: %v", err)
			}
			if got := hits.Load(); got != 2 {
				t.Errorf("recovered lane was asked %d times in total; want 2", got)
			}
			if h := w.LaneHealth()[0]; h.State != orchestrator.LaneStateClosed || h.Refused {
				t.Errorf("recovered lane health = %+v; want closed and not Refused", h)
			}
			if len(delays) != 0 {
				t.Errorf("worker-global backoff delays after recovery = %v; want none", delays)
			}
		})
	}
}
