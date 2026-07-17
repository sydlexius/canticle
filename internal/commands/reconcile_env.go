package commands

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/doxazo-net/canticle/internal/config"
	"github.com/doxazo-net/canticle/internal/db"
	"github.com/doxazo-net/canticle/internal/detector"
	"github.com/doxazo-net/canticle/internal/library"
	"github.com/doxazo-net/canticle/internal/queue"
)

// detectorEnv carries the dependencies every detector-driven reconcile command
// needs: resolved config, an open database, a constructed detector, the durable
// queue, and the optional single-library narrowing. Callers must Close it.
//
// It exists because these commands agree on their setup and differ only in which
// rows they select and what they do with the verdict. `scan reconcile` re-checks
// rows the detector already confirmed and clears the marker on disagreement;
// `scan reconcile-instrumental` (#499) checks rows it has never seen and writes a
// marker on agreement. Opposite directions, identical wiring.
type detectorEnv struct {
	cfg      config.Config
	db       *sql.DB
	detector detector.Detector
	queue    *queue.DBQueue

	// libraryID narrows selection to one library when the operator passed
	// --library; nil means every library. libLabel is the pre-rendered operator
	// facing suffix (" (library %q, id=%d)") or empty.
	libraryID *int64
	libLabel  string
}

// Close releases the database handle. Safe on a nil env.
func (e *detectorEnv) Close() {
	if e == nil || e.db == nil {
		return
	}
	_ = e.db.Close() //nolint:errcheck // reason: best-effort close on command exit
}

// queueEnv carries the dependencies a pure-stored-telemetry reconcile command
// needs: resolved config, an open database, the durable queue, and the
// optional single-library narrowing. Callers must Close it.
//
// Unlike detectorEnv, it never constructs an audio detector and never requires
// one configured: instrumentalrecalib re-decides vocal-gate rejections from
// telemetry already stamped on each row (music_sum/vocal_peak/speech_mean),
// making no provider or sidecar calls, so gating it on a reachable classifier
// would wrongly block a command that needs none.
type queueEnv struct {
	cfg   config.Config
	db    *sql.DB
	queue *queue.DBQueue

	// libraryID narrows selection to one library when the operator passed
	// --library; nil means every library. libLabel is the pre-rendered
	// operator-facing suffix (" (library %q, id=%d)") or empty.
	libraryID *int64
	libLabel  string
}

// Close releases the database handle. Safe on a nil env.
func (e *queueEnv) Close() {
	if e == nil || e.db == nil {
		return
	}
	_ = e.db.Close() //nolint:errcheck // reason: best-effort close on command exit
}

// resolveEnvLibrary resolves the operator-supplied --library argument (name or
// numeric id) against lib, returning the narrowed id and its pre-rendered
// operator-facing label. Shared by openDetectorEnv and openQueueEnv so the two
// setups agree on --library semantics.
func resolveEnvLibrary(ctx context.Context, out io.Writer, sqlDB *sql.DB, libraryArg string) (id *int64, label string, code int, err error) {
	lib, rerr := resolveLibrary(ctx, library.New(sqlDB), libraryArg)
	if rerr != nil {
		if errors.Is(rerr, sql.ErrNoRows) {
			_, _ = fmt.Fprintf(out, "library %q not found\n", libraryArg)
			return nil, "", 1, rerr
		}
		slog.Error("failed to resolve library", "error", rerr)
		return nil, "", 1, rerr
	}
	got := lib.ID
	return &got, fmt.Sprintf(" (library %q, id=%d)", lib.Name, lib.ID), 0, nil
}

// openQueueEnv performs the setup shared by reconcile commands that re-decide
// from stored telemetry alone: config, database, optional --library
// narrowing, and the durable queue. It does not resolve ffmpeg or construct a
// detector, so it works whether or not a classifier is configured or
// reachable.
//
// On failure it returns a nil env and a process exit code, having already
// written the operator-facing message to out or logged the internal error;
// the caller returns that code verbatim. On success the caller owns the env
// and must Close it.
func openQueueEnv(ctx context.Context, out io.Writer, configPath, libraryArg string) (*queueEnv, int) {
	cfg, err := config.Load(configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return nil, 1
	}
	sqlDB, err := db.Open(ctx, cfg.DB.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return nil, 1
	}

	env := &queueEnv{cfg: cfg, db: sqlDB}

	if strings.TrimSpace(libraryArg) != "" {
		id, label, code, rerr := resolveEnvLibrary(ctx, out, sqlDB, libraryArg)
		if rerr != nil {
			env.Close()
			return nil, code
		}
		env.libraryID = id
		env.libLabel = label
	}

	workQueue := queue.NewDBQueue(sqlDB)
	workQueue.SetRandomized(cfg.Queue.Randomize)
	env.queue = workQueue

	return env, 0
}

// openDetectorEnv performs the setup shared by the detector-driven reconcile
// commands. verb names the caller in the not-configured message ("reconcile",
// "backfill") so the operator is told which command went inert.
//
// On failure it returns a nil env and a process exit code, having already written
// the operator-facing message to out or logged the internal error; the caller
// returns that code verbatim. On success the caller owns the env and must Close it.
func openDetectorEnv(ctx context.Context, out io.Writer, configPath, libraryArg, verb string) (*detectorEnv, int) {
	cfg, err := config.Load(configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return nil, 1
	}
	sqlDB, err := db.Open(ctx, cfg.DB.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return nil, 1
	}

	env := &detectorEnv{cfg: cfg, db: sqlDB}

	if strings.TrimSpace(libraryArg) != "" {
		lib, rerr := resolveLibrary(ctx, library.New(sqlDB), libraryArg)
		if rerr != nil {
			env.Close()
			if errors.Is(rerr, sql.ErrNoRows) {
				_, _ = fmt.Fprintf(out, "library %q not found\n", libraryArg)
				return nil, 1
			}
			slog.Error("failed to resolve library", "error", rerr)
			return nil, 1
		}
		id := lib.ID
		env.libraryID = &id
		env.libLabel = fmt.Sprintf(" (library %q, id=%d)", lib.Name, lib.ID)
	}

	// Bail before resolving (or auto-downloading) ffmpeg when no classifier is
	// configured: these commands are inert without the detector.
	if strings.TrimSpace(cfg.InstrumentalDetector.ClassifierURL) == "" {
		env.Close()
		_, _ = fmt.Fprintf(out, "instrumental detector is not configured (set instrumental_detector.classifier_url); cannot %s\n", verb)
		return nil, 1
	}

	// Build the detector from the same config/ffmpeg/cooldown wiring serve uses, so
	// these commands inherit the cooldown and low-priority ffmpeg sampling. The
	// detector is process-local: it does not coordinate cooldown with a concurrently
	// running serve process against the same classifier.
	ffmpegPath, err := resolveFFmpeg(ctx, cfg)
	if err != nil {
		env.Close()
		slog.Error("failed to resolve ffmpeg", "error", err)
		return nil, 1
	}
	det, err := newAudioDetector(cfg, ffmpegPath)
	if err != nil {
		env.Close()
		slog.Error("failed to construct audio detector", "error", err)
		return nil, 1
	}
	if det == nil {
		env.Close()
		_, _ = fmt.Fprintf(out, "instrumental detector is not configured (set instrumental_detector.classifier_url); cannot %s\n", verb)
		return nil, 1
	}
	env.detector = det

	workQueue := queue.NewDBQueue(sqlDB)
	workQueue.SetRandomized(cfg.Queue.Randomize)
	env.queue = workQueue

	return env, 0
}
