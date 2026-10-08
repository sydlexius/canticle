package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/sydlexius/canticle/internal/aligner"
	"github.com/sydlexius/canticle/internal/reports"
)

const (
	// autoHealthTTL is how long one health answer is served before a refresh.
	autoHealthTTL = 30 * time.Second
	// autoHealthProbeTimeout bounds one refresh from this side, above the
	// client's own 3 s probe deadline.
	autoHealthProbeTimeout = 5 * time.Second
)

// AutoAligner is what the web layer needs from the aligner sidecar client
// (internal/aligner) for the player's Auto alignment action (#1008). Health
// is nil only for a live sidecar. AlignFile aligns lines to audio the caller
// opened under its own confinement (see auto_run.go).
type AutoAligner interface {
	Health(ctx context.Context) error
	AlignFile(ctx context.Context, audio io.Reader, lines []string) (aligner.Result, error)
}

// AutoBlocks is the lyric-block lookup the Auto alignment run consults before
// it re-times an on-disk body (#1399). *lyricblock.Store satisfies it and fails
// open: a lookup error reads as not blocked, and is logged by the store.
type AutoBlocks interface {
	AnyBlocked(ctx context.Context, artistKey, titleKey string, fingerprints []string) bool
}

// AttachAutoBlocks makes the Auto alignment run refuse a lyric body blocked for
// the row's track identity. A nil checker is a wiring fault: logged, no guard.
func (u *UI) AttachAutoBlocks(b AutoBlocks) {
	if b == nil {
		slog.Error("auto alignment: no lyric-block checker supplied; blocked lyrics will not be refused")
		return
	}
	u.autoBlocks = b
}

// autoState is the attached aligner plus its cached availability. A render
// only reads the atomics; the goroutine refresh starts alone calls Health.
type autoState struct {
	aligner AutoAligner
	// maxConcurrent is word_sync_generate.concurrency, the cap on alignments
	// running at once (autoRuns.begin refuses a new run past it).
	maxConcurrent int
	runs          autoRuns
	healthy       atomic.Bool
	checkedAt     atomic.Int64 // unix nanoseconds of the last finished probe; 0 = never
	refreshing    atomic.Bool
	now           func() time.Time
}

// AttachAutoAligner offers the Auto alignment action on the player page.
// maxConcurrent caps simultaneous alignments (below 1 reads as 1). One
// asynchronous probe primes the availability cache, so attaching never waits
// on the sidecar. A nil aligner is a wiring fault: logged, action off.
func (u *UI) AttachAutoAligner(a AutoAligner, maxConcurrent int) {
	if a == nil {
		slog.Error("auto alignment: no aligner supplied; the action stays off")
		return
	}
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	u.auto = &autoState{aligner: a, maxConcurrent: maxConcurrent, now: time.Now,
		runs: autoRuns{m: map[int64]*autoRun{}, timeout: autoRunTimeout, maxAudio: autoMaxAudioBytes,
			reapEvery: autoReapEvery, closeWait: autoCloseWait}}
	u.auto.refresh()
}

// CloseAuto cancels every Auto alignment run, stops the reaper and waits for
// the run goroutines to end, at most autoCloseWait (a Warn says when that was
// not enough). A start afterwards answers 503. Idempotent, and a no-op when
// no aligner is attached. Call it at shutdown.
func (u *UI) CloseAuto() {
	if u.auto != nil {
		u.auto.runs.close()
	}
}

// available is the cached health answer. A stale (or never filled) cache
// starts a refresh and still answers at once with what it has, so a render
// never waits on the sidecar.
func (a *autoState) available() bool {
	if a.stale() {
		a.refresh()
	}
	return a.healthy.Load()
}

// stale reports a cache that was never filled, is a TTL old, or is dated in
// the future: checkedAt is wall-clock, so a clock stepped backwards would
// otherwise serve one answer, unrefreshed, for the size of the step.
func (a *autoState) stale() bool {
	at := a.checkedAt.Load()
	if at == 0 {
		return true
	}
	age := a.now().Sub(time.Unix(0, at))
	return age < 0 || age >= autoHealthTTL
}

// refresh starts one probe unless one is already running.
func (a *autoState) refresh() {
	if !a.refreshing.CompareAndSwap(false, true) {
		return
	}
	// A probe that finished since the caller's stale check refilled the cache.
	if !a.stale() {
		a.refreshing.Store(false)
		return
	}
	go func() {
		defer a.refreshing.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), autoHealthProbeTimeout)
		defer cancel()
		err := a.aligner.Health(ctx)
		ok := err == nil
		changed := a.healthy.Swap(ok) != ok
		first := a.checkedAt.Swap(a.now().UnixNano()) == 0
		// The first answer is logged whatever it is: healthy starts false, so
		// a sidecar that is down at boot is not a change, and without this
		// line nothing would say why the action is missing. Only the class of
		// the error is logged, never its text.
		switch {
		case first:
			slog.Log(ctx, autoLogLevel(ok), "auto alignment: first aligner health probe finished",
				"available", ok, "result", autoHealthClass(err))
		case changed:
			slog.Log(ctx, autoLogLevel(ok), "auto alignment: aligner availability changed",
				"available", ok, "result", autoHealthClass(err))
		}
	}()
}

func autoLogLevel(ok bool) slog.Level {
	if ok {
		return slog.LevelInfo
	}
	return slog.LevelWarn
}

// autoHealthClass names a Health result without quoting it.
func autoHealthClass(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, aligner.ErrUnhealthy):
		return "unhealthy_answer"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "unreachable"
}

// autoEligible is the ONE place that decides whether a row may be offered
// Auto alignment: today, the offset editor's rows. Later slices extend it.
func autoEligible(t reports.PreviewTarget) bool {
	return t.LineEditable
}

// autoURL is the row's Auto alignment endpoint for the editor panel's
// data-auto-url, or "" (attribute absent) unless an aligner is attached, its
// cached health says available, and the row is eligible.
func (u *UI) autoURL(t reports.PreviewTarget, id int64) string {
	if u.auto == nil || !autoEligible(t) || !u.auto.available() {
		return ""
	}
	return "/preview/" + strconv.FormatInt(id, 10) + "/auto"
}
