package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/auth"
	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/queue"
)

type fakeAuth struct {
	raw      string
	required auth.Scope
	err      error
}

func (f *fakeAuth) ValidateKey(_ context.Context, raw string, required auth.Scope) (auth.Key, error) {
	f.raw = raw
	f.required = required
	if f.err != nil {
		return auth.Key{}, f.err
	}
	return auth.Key{ID: "key"}, nil
}

type fakeQueue struct {
	items      []models.Inputs
	priorities []int
	cleanups   []models.Inputs
	err        error
}

func (f *fakeQueue) Enqueue(_ context.Context, inputs models.Inputs, priority int) (queue.WorkItem, error) {
	if f.err != nil {
		return queue.WorkItem{}, f.err
	}
	f.items = append(f.items, inputs)
	f.priorities = append(f.priorities, priority)
	return queue.WorkItem{ID: int64(len(f.items)), Inputs: inputs, Priority: priority}, nil
}

func (f *fakeQueue) Cleanup(_ context.Context, inputs models.Inputs) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.cleanups = append(f.cleanups, inputs)
	return 1, nil
}

func TestLidarrWebhookDownloadEnqueuesBeforeOK(t *testing.T) {
	a := &fakeAuth{}
	q := &fakeQueue{}
	h := NewHandler(a, q, "lyrics")
	body := `{
		"eventType":"Download",
		"artist":{"artistName":"Artist"},
		"album":{"title":"Album"},
		"tracks":[{"title":"One"},{"title":"Two"}],
		"extra":"ignored"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=query-key", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d; body %q", rec.Code, http.StatusOK, rec.Body.String())
	}
	if a.raw != "query-key" || a.required != auth.ScopeWebhook {
		t.Fatalf("auth raw/scope = %q/%q; want query-key/%q", a.raw, a.required, auth.ScopeWebhook)
	}
	if len(q.items) != 2 {
		t.Fatalf("queued items = %d; want 2", len(q.items))
	}
	if q.items[0].Track.ArtistName != "Artist" || q.items[0].Track.TrackName != "One" || q.items[0].Track.AlbumName != "Album" {
		t.Fatalf("first queued item = %+v; want Artist/One/Album", q.items[0].Track)
	}
	if q.items[0].Outdir != "lyrics" || len(q.items[0].OutputPaths) != 1 || q.items[0].OutputPaths[0].Outdir != "lyrics" {
		t.Fatalf("output destination = %+v; want lyrics outdir", q.items[0])
	}
	for i, v := range q.priorities {
		if v != queue.PriorityWebhook {
			t.Fatalf("queued priority[%d] = %d; want %d", i, v, queue.PriorityWebhook)
		}
	}
}

func TestLidarrWebhookBearerAuthAndSingleTrackRetag(t *testing.T) {
	a := &fakeAuth{}
	q := &fakeQueue{}
	h := NewHandler(a, q, "lyrics")
	body := `{"eventType":"TrackRetag","artist":{"artistName":"Artist"},"track":{"title":"One"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer bearer-key")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d; body %q", rec.Code, http.StatusOK, rec.Body.String())
	}
	if a.raw != "bearer-key" {
		t.Fatalf("auth raw = %q; want bearer-key", a.raw)
	}
	if len(q.items) != 1 || q.items[0].Track.TrackName != "One" {
		t.Fatalf("queued items = %+v; want one TrackRetag item", q.items)
	}
}

func TestLidarrWebhookLowercaseBearerAuth(t *testing.T) {
	a := &fakeAuth{}
	q := &fakeQueue{}
	h := NewHandler(a, q, "lyrics")
	body := `{"eventType":"TrackRetag","artist":{"artistName":"Artist"},"track":{"title":"One"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr", strings.NewReader(body))
	req.Header.Set("Authorization", "bearer lower-key")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d; body %q", rec.Code, http.StatusOK, rec.Body.String())
	}
	if a.raw != "lower-key" {
		t.Fatalf("auth raw = %q; want lower-key", a.raw)
	}
	if len(q.items) != 1 || q.items[0].Track.TrackName != "One" {
		t.Fatalf("queued items = %+v; want one TrackRetag item", q.items)
	}
}

func TestLidarrWebhookLogOnlyEventsDoNotEnqueue(t *testing.T) {
	for _, event := range []string{"Grab", "Rename"} {
		t.Run(event, func(t *testing.T) {
			a := &fakeAuth{}
			q := &fakeQueue{}
			h := NewHandler(a, q, "lyrics")
			req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=key", strings.NewReader(`{"eventType":"`+event+`"}`))
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; want %d; body %q", rec.Code, http.StatusOK, rec.Body.String())
			}
			if len(q.items) != 0 {
				t.Fatalf("queued items = %+v; want none", q.items)
			}
		})
	}
}

func TestLidarrWebhookDeleteCleansQueuedWork(t *testing.T) {
	a := &fakeAuth{}
	q := &fakeQueue{}
	h := NewHandler(a, q, "lyrics")
	body := `{"eventType":"Delete","artist":{"artistName":"Artist"},"tracks":[{"title":"One"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=key", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want %d; body %q", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(q.items) != 0 {
		t.Fatalf("queued items = %+v; want none", q.items)
	}
	if len(q.cleanups) != 1 || q.cleanups[0].Track.ArtistName != "Artist" || q.cleanups[0].Track.TrackName != "One" {
		t.Fatalf("cleanups = %+v; want Artist/One cleanup", q.cleanups)
	}
}

func TestLidarrWebhookAuthAndEnqueueErrors(t *testing.T) {
	t.Run("unauthorized", func(t *testing.T) {
		h := NewHandler(&fakeAuth{err: auth.ErrInvalidKey}, &fakeQueue{}, "lyrics")
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=bad", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d; want %d", rec.Code, http.StatusUnauthorized)
		}
	})

	t.Run("forbidden", func(t *testing.T) {
		h := NewHandler(&fakeAuth{err: auth.ErrForbiddenScope}, &fakeQueue{}, "lyrics")
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=bad", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d; want %d", rec.Code, http.StatusForbidden)
		}
	})

	t.Run("auth backend failure is retryable", func(t *testing.T) {
		h := NewHandler(&fakeAuth{err: errors.New("auth store down")}, &fakeQueue{}, "lyrics")
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=key", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d; want %d", rec.Code, http.StatusInternalServerError)
		}
	})

	t.Run("enqueue failure is retryable", func(t *testing.T) {
		h := NewHandler(&fakeAuth{}, &fakeQueue{err: errors.New("db down")}, "lyrics")
		body := `{"eventType":"Download","artist":{"artistName":"Artist"},"tracks":[{"title":"One"}]}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=key", strings.NewReader(body))
		rec := httptest.NewRecorder()

		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d; want %d", rec.Code, http.StatusInternalServerError)
		}
	})
}

type fakeReadiness struct{ err error }

func (f *fakeReadiness) PingContext(_ context.Context) error { return f.err }

type fakeStats struct {
	counts map[string]int64
	err    error
}

func (f *fakeStats) CountByStatus(_ context.Context) (map[string]int64, error) {
	return f.counts, f.err
}

func TestHealthzReturnsOK(t *testing.T) {
	h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d; want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("healthz body = %q; want status ok", rec.Body.String())
	}
}

func TestReadyzReportsDatabase(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics", WithReadiness(&fakeReadiness{}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("readyz status = %d; want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"database":"ok"`) {
			t.Fatalf("readyz body = %q; want database ok", rec.Body.String())
		}
	})

	t.Run("no checker omits database claim", func(t *testing.T) {
		h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics") // no WithReadiness
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("readyz status = %d; want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"status":"ready"`) {
			t.Fatalf("readyz body = %q; want status ready", rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "database") {
			t.Fatalf("readyz body = %q; must not claim a database check when none is configured", rec.Body.String())
		}
	})

	t.Run("database unavailable", func(t *testing.T) {
		h := NewHandler(&fakeAuth{}, &fakeQueue{}, "lyrics", WithReadiness(&fakeReadiness{err: errors.New("db file /secret/path down")}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("readyz status = %d; want 503", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "secret") {
			t.Fatalf("readyz body = %q; leaked error detail", rec.Body.String())
		}
	})
}

func TestStatusRequiresAdminAndReportsQueue(t *testing.T) {
	a := &fakeAuth{}
	stats := &fakeStats{counts: map[string]int64{"pending": 3, "failed": 1}}
	h := NewHandler(a, &fakeQueue{}, "lyrics", WithReadiness(&fakeReadiness{}), WithStatusReporter(stats))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/status?apikey=secret", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d; want 200", rec.Code)
	}
	if a.required != auth.ScopeAdmin {
		t.Fatalf("status required scope = %q; want admin", a.required)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"pending":3`) || !strings.Contains(body, `"failed":1`) {
		t.Fatalf("status body = %q; want queue counts", body)
	}
}

func TestStatusForbiddenWithoutAdminScope(t *testing.T) {
	h := NewHandler(&fakeAuth{err: auth.ErrForbiddenScope}, &fakeQueue{}, "lyrics", WithStatusReporter(&fakeStats{}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/status?apikey=key", nil))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status code = %d; want 403", rec.Code)
	}
}

func TestRedactURIHidesAPIKey(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/lidarr?apikey=secret&x=1", nil)
	got := redactURI(req.URL)
	if strings.Contains(got, "secret") {
		t.Fatalf("redacted URI = %q; contains secret", got)
	}
	if !strings.Contains(got, "apikey=REDACTED") {
		t.Fatalf("redacted URI = %q; want redacted apikey", got)
	}
}
