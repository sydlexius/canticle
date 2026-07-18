package app

import (
	"context"
	"encoding/csv"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/sydlexius/canticle/internal/backoff"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/queue"
)

// App owns all processing state and orchestrates the lyrics fetch loop.
type App struct {
	inputs       *queue.InputsQueue
	failed       *queue.InputsQueue
	fetcher      musixmatch.Fetcher
	writer       lyrics.Writer
	mode         string
	total        int
	cooldown     int
	baseBackoff  time.Duration
	maxBackoff   time.Duration
	sleep        func(context.Context, time.Duration) bool
	failureCount int
}

// NewApp creates an App with the given dependencies.
// The inputs queue should be pre-populated by the scanner.
// A new failed queue is created internally.
func NewApp(fetcher musixmatch.Fetcher, writer lyrics.Writer, inputs *queue.InputsQueue, cooldown int, mode string) *App {
	return &App{
		inputs:      inputs,
		failed:      queue.NewInputsQueue(),
		fetcher:     fetcher,
		writer:      writer,
		mode:        mode,
		total:       inputs.Len(),
		cooldown:    cooldown,
		baseBackoff: backoff.DefaultBase,
		maxBackoff:  backoff.DefaultMax,
		sleep:       sleep,
	}
}

// Run executes the main processing loop, respecting context cancellation for
// graceful shutdown on Ctrl+C / SIGTERM.
func (a *App) Run(ctx context.Context) error {
	for !a.inputs.Empty() {
		if ctx.Err() != nil {
			break
		}

		cur, err := a.inputs.Next()
		if err != nil {
			slog.Error("unexpected empty queue", "error", err)
			break
		}
		slog.Debug("searching song", "artist", cur.Track.ArtistName, "track", cur.Track.TrackName)
		song, err := a.fetcher.FindLyrics(ctx, cur.Track)
		// A benign miss (no matching track, or a match with no fetchable lyrics)
		// means the upstream result is stable and will not change on a near-term
		// retry, so it must not trip the geometric backoff or increment the
		// failure counter. It still goes to the failed bucket so it is reported
		// as not-fetched.
		benignMiss := err != nil && musixmatch.IsBenignMiss(err)
		if err == nil {
			a.failureCount = 0
			cur, err = a.inputs.Pop()
			if err != nil {
				slog.Error("unexpected empty queue on pop", "error", err)
				break
			}
			slog.Debug("formatting lyrics")
			if writeErr := a.writer.WriteLRC(song, cur.Filename, cur.Outdir); writeErr != nil {
				slog.Error("failed to save lyrics", "error", writeErr)
				a.failed.Push(cur)
			}
		} else {
			if benignMiss {
				slog.Debug("no lyrics available", "artist", cur.Track.ArtistName, "track", cur.Track.TrackName, "reason", err)
			} else {
				a.failureCount++
				slog.Error("lyrics fetch failed", "error", err)
			}
			item, popErr := a.inputs.Pop()
			if popErr != nil {
				slog.Error("unexpected empty queue on pop", "error", popErr)
				break
			}
			a.failed.Push(item)
		}
		if err != nil && !benignMiss {
			a.backoffTimer(ctx, a.failureCount)
		} else {
			a.timer(ctx)
		}
	}

	return a.handleFailed()
}

// timer counts down between API calls, respecting context cancellation.
func (a *App) timer(ctx context.Context) {
	if a.inputs.Empty() {
		return
	}

	for i := a.cooldown; i >= 0; i-- {
		fmt.Printf("    Please wait... %ds    \r", i)
		if i == 0 {
			break
		}
		if !a.sleep(ctx, time.Second) {
			return
		}
	}
	fmt.Printf("\n\n")
}

func (a *App) backoffTimer(ctx context.Context, attempts int) {
	if a.inputs.Empty() {
		return
	}

	delay := backoff.Geometric(attempts, a.baseBackoff, a.maxBackoff)
	if delay <= 0 {
		return
	}
	slog.Warn("backing off after lyrics fetch failure", "attempts", attempts, "delay", delay)
	_ = a.sleep(ctx, delay)
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// handleFailed processes remaining and failed items after the loop exits.
func (a *App) handleFailed() error {
	fmt.Printf("\n")
	if !a.inputs.Empty() {
		for !a.inputs.Empty() {
			item, err := a.inputs.Pop()
			if err != nil {
				return fmt.Errorf("draining inputs queue: %w", err)
			}
			a.failed.Push(item)
		}
	}
	slog.Info("fetch complete", "success", a.total-a.failed.Len(), "total", a.total)
	if a.failed.Empty() {
		return nil
	}
	slog.Warn("failed to fetch lyrics", "count", a.failed.Len())

	if a.mode == "dir" {
		slog.Info("you can try again with the same command")
		return nil
	}

	t := time.Now().Format("20060102_150405")
	fn := t + "_failed.txt"
	slog.Info("saving list of failed items", "file", fn)

	f, err := os.Create(fn) //nolint:gosec // filename is generated from timestamp, not user input
	if err != nil {
		return fmt.Errorf("creating failed items file: %w", err)
	}

	w := csv.NewWriter(f)
	for !a.failed.Empty() {
		cur, err := a.failed.Pop()
		if err != nil {
			_ = f.Close()
			return fmt.Errorf("unexpected empty queue writing failed items: %w", err)
		}
		if err := w.Write([]string{cur.Track.ArtistName, cur.Track.TrackName}); err != nil {
			_ = f.Close()
			return fmt.Errorf("writing failed item: %w", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		_ = f.Close()
		return fmt.Errorf("flushing failed items: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing failed items file: %w", err)
	}
	return nil
}
