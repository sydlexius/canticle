package worker

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
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

// musixmatchHost is the only host the test transport forwards; anything else
// is refused, so no request can leave the machine.
const musixmatchHost = "apic.musixmatch.com"

type rewriteTransport struct{ target *url.URL }

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != musixmatchHost {
		return nil, fmt.Errorf("test transport: refusing request to unexpected host %q", r.URL.Host)
	}
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = rt.target.Scheme, rt.target.Host
	return http.DefaultTransport.RoundTrip(r)
}

// stubbedMusixmatch is the REAL Musixmatch client over an HTTP client of its
// own that reaches only a local server. The server answers every request with
// status: 403 with an HTML body is an edge refusal (#1372), 500 an ordinary
// server fault, 404 an ordinary miss.
func stubbedMusixmatch(t *testing.T, initial int64) (client *musixmatch.Client, hits, status *atomic.Int64) {
	t.Helper()
	hits, status = &atomic.Int64{}, &atomic.Int64{}
	status.Store(initial)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token.get") {
			// The token mint is not a lookup: it is not counted in hits.
			_, _ = io.WriteString(w, `{"message":{"header":{"status_code":200},"body":{"user_token":"`+refusedToken+`"}}}`)
			return
		}
		hits.Add(1)
		code := int(status.Load())
		w.WriteHeader(code)
		if code != http.StatusNotFound {
			_, _ = io.WriteString(w, "<html><head><title>refused</title></head><body>"+strings.Repeat("edge refusal ", 200)+"</body></html>")
		}
	}))
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	hc := &http.Client{Timeout: 10 * time.Second, Transport: rewriteTransport{target: target}}
	token, err := musixmatch.NewTokenMinter(hc).Mint(context.Background())
	if err != nil {
		t.Fatalf("mint token through the stub: %v", err)
	}
	return musixmatch.NewClientWithHTTP(token, hc), hits, status
}

// titleFetcher is the healthy second lane: it has lyrics for "Held ..." titles
// and answers a clean no-match for everything else.
type titleFetcher struct{ calls *atomic.Int64 }

func (f titleFetcher) FindLyrics(_ context.Context, t models.Track) (models.Song, error) {
	f.calls.Add(1)
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

// laneRig is the real worker, orchestrator, Musixmatch client and SQLite queue
// with a healthy petitlyrics-named second lane and a frozen, settable clock.
type laneRig struct {
	w                           *Worker
	db                          *sql.DB
	hits, status, fallbackCalls *atomic.Int64
	now                         time.Time
	delays                      []time.Duration
	enqueue                     func(title string)
}

func newLaneRig(t *testing.T, mode string, status int64) *laneRig {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	r := &laneRig{db: sqlDB, fallbackCalls: &atomic.Int64{}, now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	r.enqueue = func(title string) {
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
	var client *musixmatch.Client
	client, r.hits, r.status = stubbedMusixmatch(t, status)
	r.w = New(q, cache.New(sqlDB), client, &capturingWriter{})
	r.w.SetFallbackProviders(providers.New(providers.PetitLyrics, titleFetcher{calls: r.fallbackCalls}))
	r.w.SetProviderRecorder(q)
	r.w.SetRecordingEnrichmentDefault(false)
	r.w.SetProvidersMode(mode)
	r.w.setClock(func() time.Time { return r.now })
	r.w.sleep = func(_ context.Context, d time.Duration) { r.delays = append(r.delays, d) }
	return r
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

			rig := newLaneRig(t, mode, http.StatusForbidden)
			w, sqlDB, hits := rig.w, rig.db, rig.hits
			// A row the healthy lane misses comes first, so the refusal is seen by a
			// dispatch in which both lanes report (parallel cancels a loser).
			for _, title := range []string{"Missing 1", "Held 1", "Missing 2", "Held 2", "Held 3"} {
				rig.enqueue(title)
			}

			if err := w.Run(ctx); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if len(rig.delays) != 0 {
				t.Errorf("worker-global backoff delays = %v; want none while the healthy lane answers", rig.delays)
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
			rig.status.Store(http.StatusNotFound)
			rig.now = rig.now.Add(2 * time.Hour)
			rig.enqueue("Missing 3")
			if err := w.Run(ctx); err != nil {
				t.Fatalf("Run after recovery: %v", err)
			}
			if got := hits.Load(); got != 2 {
				t.Errorf("recovered lane was asked %d times in total; want 2", got)
			}
			if h := w.LaneHealth()[0]; h.State != orchestrator.LaneStateClosed || h.Refused {
				t.Errorf("recovered lane health = %+v; want closed and not Refused", h)
			}
			if len(rig.delays) != 0 {
				t.Errorf("worker-global backoff delays after recovery = %v; want none", rig.delays)
			}
		})
	}
}

// A lane fault that does NOT open its breaker (HTTP 500 here) is not
// lane-bounded, so with a clean miss on the other lane it still feeds the
// worker-global backoff as on main; else every such row would re-ask both
// providers on its own schedule forever (a lane-level bound is #1375).
func TestRun_UnboundedLaneFaultFeedsGlobalBackoff(t *testing.T) {
	for _, mode := range []string{orchestrator.ModeOrdered, orchestrator.ModeParallel} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			const rows = 4
			rig := newLaneRig(t, mode, http.StatusInternalServerError)
			for i := 1; i <= rows; i++ {
				rig.enqueue(fmt.Sprintf("Missing %d", i))
			}
			w, sqlDB := rig.w, rig.db

			if err := w.Run(ctx); err != nil {
				t.Fatalf("Run: %v", err)
			}

			// One growing global delay per failed row, as on main.
			want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute}
			if !slices.Equal(rig.delays, want) {
				t.Errorf("worker-global backoff delays = %v; want %v", rig.delays, want)
			}
			// Each lane is asked once per row in the pass, never more: the rows are
			// not re-dispatched ahead of their own retry time.
			if got := rig.hits.Load(); got != rows {
				t.Errorf("faulting lane was asked %d times for %d rows", got, rows)
			}
			if got := rig.fallbackCalls.Load(); got != rows {
				t.Errorf("answering lane was asked %d times for %d rows", got, rows)
			}
			var failed int
			if err := sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_queue WHERE status = 'failed' AND attempts = 1 AND miss_count = 0`).Scan(&failed); err != nil {
				t.Fatalf("count failed rows: %v", err)
			}
			if failed != rows {
				t.Errorf("failed rows = %d; want %d", failed, rows)
			}
			if h := w.LaneHealth()[0]; h.State != orchestrator.LaneStateClosed || h.Refused {
				t.Errorf("faulting lane health = %+v; want closed and not Refused (a 500 does not open it)", h)
			}
		})
	}
}
