package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/aligner"
	"github.com/sydlexius/canticle/internal/reports"
)

// fakeAuto is an AutoAligner whose Health answer and blocking are scripted.
type fakeAuto struct {
	calls atomic.Int32
	err   atomic.Pointer[error]
	// gate, when set, blocks Health until closed, whatever its ctx says.
	gate atomic.Pointer[chan struct{}]
}

func (f *fakeAuto) Health(context.Context) error {
	f.calls.Add(1)
	if g := f.gate.Load(); g != nil {
		<-*g
	}
	if e := f.err.Load(); e != nil {
		return *e
	}
	return nil
}

func (f *fakeAuto) block(t *testing.T) (release func()) {
	t.Helper()
	g := make(chan struct{})
	f.gate.Store(&g)
	var once sync.Once
	release = func() { once.Do(func() { f.gate.Store(nil); close(g) }) }
	t.Cleanup(release)
	return release
}

// attachSettled attaches f and waits for the priming probe to finish.
func attachSettled(t *testing.T, u *UI, f *fakeAuto) {
	t.Helper()
	u.AttachAutoAligner(f, 0)
	waitFor(t, "the priming probe", func() bool { return u.auto.checkedAt.Load() != 0 && !u.auto.refreshing.Load() })
}

const (
	autoAttr     = "data-auto-url"
	autoFirstLog = "first aligner health probe finished"
)

func TestAutoURLPrimedAtAttachAndRendered(t *testing.T) {
	e := newEditEnv(t)
	f := &fakeAuto{}
	attachSettled(t, e.ui, f)
	if got := f.calls.Load(); got != 1 {
		t.Fatalf("Health calls after attach = %d, want 1 (primed once)", got)
	}
	if e.ui.auto.maxConcurrent != 1 {
		t.Errorf("maxConcurrent = %d, want a value below 1 read as 1", e.ui.auto.maxConcurrent)
	}
	body := e.page(e.id).Body.String()
	if want := autoAttr + `="/preview/` + e.id + `/auto"`; !strings.Contains(body, want) {
		t.Fatalf("editable row with a healthy aligner: page lacks %s", want)
	}
	if got := f.calls.Load(); got != 1 {
		t.Errorf("Health calls after a render inside the TTL = %d, want 1", got)
	}
}

func TestAutoURLAbsentUnlessEveryConditionHolds(t *testing.T) {
	down := errors.New("down")
	for _, name := range []string{"no aligner attached", "unhealthy"} {
		t.Run(name, func(t *testing.T) {
			e := newEditEnv(t)
			if name == "unhealthy" {
				f := &fakeAuto{}
				f.err.Store(&down)
				attachSettled(t, e.ui, f)
			}
			if body := e.page(e.id).Body.String(); !strings.Contains(body, `id="mx-edit"`) || strings.Contains(body, autoAttr) {
				t.Fatal("want the editor panel without " + autoAttr)
			}
		})
	}
	t.Run("ineligible row", func(t *testing.T) {
		e := newEditEnv(t)
		attachSettled(t, e.ui, &fakeAuto{})
		e.put(t, "other.lrc", editLRC)
		id := e.seedTier(t, e.writeFile(t, e.root, "other.flac"), "word")
		if body := e.page(itoa(id)).Body.String(); strings.Contains(body, autoAttr) {
			t.Fatal("word-tier row was offered " + autoAttr)
		}
		// The page also hides the panel there, so eligibility is pinned alone.
		if got := e.ui.autoURL(reports.PreviewTarget{LineEditable: false}, id); got != "" {
			t.Fatalf("autoURL for a row that is not line-editable = %q, want empty", got)
		}
		if got := e.ui.autoURL(reports.PreviewTarget{LineEditable: true}, id); got == "" {
			t.Fatal("autoURL for a line-editable row is empty (control)")
		}
	})
}

// The Auto controls (#1008 S8) render exactly where data-auto-url does: an
// editable row with an available aligner. Otherwise they are absent, never
// shown disabled, while the offset editor itself still renders.
func TestAutoButtonRenderedOnlyWithAutoURL(t *testing.T) {
	controls := []string{`id="mx-auto-run"`, `id="mx-auto-stop"`, `id="mx-auto-progress"`}
	for _, name := range []string{"available", "no aligner attached", "unhealthy"} {
		t.Run(name, func(t *testing.T) {
			e := newEditEnv(t)
			f := &fakeAuto{}
			if name == "unhealthy" {
				down := errors.New("down")
				f.err.Store(&down)
			}
			if name != "no aligner attached" {
				attachSettled(t, e.ui, f)
			}
			body := e.page(e.id).Body.String()
			if !strings.Contains(body, `id="mx-edit"`) {
				t.Fatal("editor panel missing (control)")
			}
			want := name == "available"
			for _, c := range controls {
				if got := strings.Contains(body, c); got != want {
					t.Errorf("%s rendered = %v, want %v", c, got, want)
				}
			}
			if want && !strings.Contains(body, `<button type="button" id="mx-auto-run" class="mx-edit-btn">Auto</button>`) {
				t.Error("Auto button markup changed: want an enabled type=button labeled Auto")
			}
			if want && !strings.Contains(body, `id="mx-auto-stop" class="mx-edit-btn" hidden`) {
				t.Error("the stop button must start hidden")
			}
		})
	}
}

// A render must never wait on the sidecar: with a Health that never returns,
// attach and the page both come back at once and report unavailable.
func TestAutoRenderNeverWaitsOnAligner(t *testing.T) {
	e := newEditEnv(t)
	f := &fakeAuto{}
	// Registered before block, so it runs after the release: the probe's log
	// line must land before the next test captures the default logger.
	t.Cleanup(func() {
		waitFor(t, "the released probe", func() bool { return e.ui.auto != nil && !e.ui.auto.refreshing.Load() })
	})
	f.block(t)
	done := make(chan string, 1)
	go func() {
		e.ui.AttachAutoAligner(f, 2)
		done <- e.page(e.id).Body.String()
	}()
	select {
	case body := <-done:
		if !strings.Contains(body, `id="mx-edit"`) || strings.Contains(body, autoAttr) {
			t.Fatal("hung aligner: want the editor panel without " + autoAttr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("page render waited on a hung aligner health probe")
	}
}

func TestAutoStaleCacheStartsExactlyOneRefresh(t *testing.T) {
	logs := captureLogs(t)
	e := newEditEnv(t)
	f := &fakeAuto{}
	attachSettled(t, e.ui, f)
	if got := logs.String(); !strings.Contains(got, autoFirstLog) || !strings.Contains(got, "available=true result=ok") {
		t.Errorf("no first-probe log on the first healthy probe: %q", got)
	}
	var ahead atomic.Int64
	e.ui.auto.now = func() time.Time { return time.Now().Add(time.Duration(ahead.Load())) }

	// Just inside the TTL nothing is probed.
	ahead.Store(int64(autoHealthTTL - time.Second))
	if !e.ui.auto.available() || f.calls.Load() != 1 {
		t.Fatalf("inside the TTL: calls = %d, want 1 and available", f.calls.Load())
	}

	// Past the TTL, concurrent renders share one refresh and read the cache.
	ahead.Store(int64(autoHealthTTL + time.Second))
	down := errors.New("down")
	f.err.Store(&down)
	release := f.block(t)
	var wg sync.WaitGroup
	var withAttr atomic.Int32
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if strings.Contains(e.page(e.id).Body.String(), autoAttr) {
				withAttr.Add(1)
			}
		}()
	}
	wg.Wait()
	waitFor(t, "the refresh to start", func() bool { return f.calls.Load() >= 2 })
	if got := f.calls.Load(); got != 2 {
		t.Fatalf("Health calls after 16 stale renders = %d, want 2 (one refresh)", got)
	}
	if got := withAttr.Load(); got != 16 {
		t.Errorf("renders served the cached healthy answer = %d of 16", got)
	}
	logs.Reset()
	release()
	waitFor(t, "the refresh to land", func() bool { return !e.ui.auto.refreshing.Load() })
	if strings.Contains(e.page(e.id).Body.String(), autoAttr) {
		t.Error("page still offers " + autoAttr + " after the refresh reported unhealthy")
	}
	if got := f.calls.Load(); got != 2 {
		t.Errorf("Health calls after the refresh landed = %d, want 2 (fresh again)", got)
	}
	if got := logs.String(); !strings.Contains(got, "aligner availability changed") || !strings.Contains(got, "available=false") {
		t.Errorf("no availability log on the change to unhealthy: %q", got)
	}
}

// TestAutoFirstProbeIsAlwaysLogged pins that a sidecar down at boot says so
// once (the cache starts unavailable, so it is not a change), by class and
// never by error text, and that an unchanged later probe is silent.
func TestAutoFirstProbeIsAlwaysLogged(t *testing.T) {
	for _, tc := range []struct {
		class string
		err   error
	}{
		{"unreachable", errors.New("dial 192.0.2.7:9999 sekrit")},
		{"unhealthy_answer", fmt.Errorf("%w: sekrit", aligner.ErrUnhealthy)},
		{"timeout", fmt.Errorf("sekrit: %w", context.DeadlineExceeded)},
	} {
		logs := captureLogs(t)
		e := newEditEnv(t)
		f := &fakeAuto{}
		f.err.Store(&tc.err)
		attachSettled(t, e.ui, f)
		got := logs.String()
		if strings.Count(got, autoFirstLog) != 1 || !strings.Contains(got, "level=WARN") ||
			!strings.Contains(got, "available=false result="+tc.class) {
			t.Errorf("%s: want one Warn first-probe line with its class, got %q", tc.class, got)
		}
		if strings.Contains(got, "sekrit") {
			t.Errorf("%s: the log quotes the error text: %q", tc.class, got)
		}
		// A second probe with the same answer logs nothing.
		logs.Reset()
		e.ui.auto.checkedAt.Store(1)
		e.ui.auto.refresh()
		waitFor(t, "the second probe", func() bool { return f.calls.Load() == 2 && !e.ui.auto.refreshing.Load() })
		if logs.Len() != 0 {
			t.Errorf("%s: an unchanged later probe logged %q", tc.class, logs.String())
		}
	}
}

// TestAutoCacheAgeEdges pins the two ways a caller can be wrong about the
// cache's age: a refresh asked for after another probe already refilled it
// starts no probe, and an answer dated in the future (the wall clock stepped
// backwards) is stale, not fresh for the size of the step.
func TestAutoCacheAgeEdges(t *testing.T) {
	e := newEditEnv(t)
	f := &fakeAuto{}
	attachSettled(t, e.ui, f)
	a := e.ui.auto

	a.refresh()
	waitFor(t, "the refresh flag to clear", func() bool { return !a.refreshing.Load() })
	if got := f.calls.Load(); got != 1 {
		t.Fatalf("Health calls after a refresh over a fresh cache = %d, want 1 (re-check after the swap)", got)
	}

	a.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	if !a.stale() {
		t.Fatal("an answer dated 2h in the future reads fresh; a negative age must be stale")
	}
	if !a.available() {
		t.Error("a stale cache did not answer with what it has")
	}
	waitFor(t, "the refresh after the clock step", func() bool { return f.calls.Load() == 2 && !a.refreshing.Load() })
}

func TestAttachAutoAlignerNilIsLoudAndOff(t *testing.T) {
	logs := captureLogs(t)
	e := newEditEnv(t)
	e.ui.AttachAutoAligner(nil, 1)
	if e.ui.auto != nil {
		t.Fatal("a nil aligner was attached")
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "no aligner supplied") {
		t.Errorf("nil aligner was not logged at Error: %q", logs.String())
	}
}
