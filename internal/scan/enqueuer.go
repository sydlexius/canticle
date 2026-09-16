package scan

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/timing"
)

// PendingResultStore reads and updates scan results eligible for queuing.
type PendingResultStore interface {
	ListPendingByLibrary(ctx context.Context, libraryID int64) ([]models.ScanResult, error)
	SetStatus(ctx context.Context, ids []int64, status string) error
}

// LyricsCache reports whether lyrics already exist for a scanned track.
type LyricsCache interface {
	// LookupAccepted checks the cache for (artist, title, durationBucket) and
	// serves the found row only when accept(lyrics) reports true; otherwise it
	// reads as sql.ErrNoRows, exactly as a genuine miss would, and is not
	// counted as a served cache hit (#952; see cache.CacheRepo.LookupAccepted).
	// Pass durationBucket=0 when the recording duration is not yet known.
	LookupAccepted(ctx context.Context, artist, title string, durationBucket int, accept func(lyrics string) bool) (string, error)
}

// WorkQueue enqueues durable lyrics work.
type WorkQueue interface {
	Enqueue(ctx context.Context, inputs models.Inputs, priority int) (queue.WorkItem, error)
}

// DurationLookup reads a cached exact audio duration for a file, keyed by
// (canonical path, mtime, size). *audiodur.Store satisfies it directly; it is
// declared here in primitives (rather than importing internal/audiodur) so
// scan gains no new dependency for an optional seam. See EnqueuePending's use
// of it (#952): scan_results carries no duration column at all (unlike
// work_queue and its Track.TrackLength stamp), so res.Track.TrackLength is
// always 0 for a scan-side row, and the accept check below would otherwise
// silently fall back to the CACHED song's own catalog length -- almost always
// a pass, since a lyric is rarely miscategorized against the very metadata it
// was fetched with. A resolved audio duration is what lets this check actually
// catch what the worker would refuse.
type DurationLookup interface {
	Lookup(ctx context.Context, path string, mtimeNano, size int64) (int, bool, error)
}

// TimingVerdict is a track's persisted accept-time timing decision, together
// with the provider generation that produced it.
type TimingVerdict struct {
	// Outcome is the stored work_queue.timing_outcome value (#440).
	Outcome timing.TimingOutcome
	// ProvidersVersion is the provider-set generation in effect when the verdict
	// was reached. It is what makes the suppression expire: see shouldSuppress.
	ProvidersVersion int
}

// TimingVerdictStore reads the durable timing verdict recorded for a track.
//
// This is the consumer that migration 034's columns never had: the worker has
// stamped timing_outcome since #440, but nothing read it back, so the pipeline
// had no memory of having already rejected a track (#679).
//
// found is false when no verdict was ever recorded, which is the normal case for
// a track that has simply not been fetched yet.
type TimingVerdictStore interface {
	LookupTiming(ctx context.Context, artist, title string) (TimingVerdict, bool, error)
}

// TimingVerdictReader is the raw queue-side read, in primitives.
//
// *queue.DBQueue satisfies it directly. Keeping the queue package free of a
// scan-package type preserves the existing dependency direction (scan imports
// queue, never the reverse); TimingVerdicts bridges the two shapes.
type TimingVerdictReader interface {
	LookupTiming(ctx context.Context, artist, title string) (outcome string, providersVersion int, found bool, err error)
}

// TimingVerdicts adapts a TimingVerdictReader to TimingVerdictStore.
type TimingVerdicts struct {
	Reader TimingVerdictReader
}

// LookupTiming converts the stored outcome string into a typed verdict. The
// column holds an internal/timing TimingOutcome verbatim (see migration 034), so
// the conversion is a cast rather than a parse; an unrecognized value simply
// never matches Categorical and therefore never suppresses.
func (t TimingVerdicts) LookupTiming(ctx context.Context, artist, title string) (TimingVerdict, bool, error) {
	if t.Reader == nil {
		return TimingVerdict{}, false, nil
	}
	outcome, version, found, err := t.Reader.LookupTiming(ctx, artist, title)
	if err != nil || !found {
		return TimingVerdict{}, false, err
	}
	return TimingVerdict{
		Outcome:          timing.TimingOutcome(outcome),
		ProvidersVersion: version,
	}, true, nil
}

// Enqueuer bridges pending scan results to the durable work queue.
type Enqueuer struct {
	Results  PendingResultStore
	Cache    LyricsCache
	Queue    WorkQueue
	Priority int
	// DetectOverride is the scan-CLI override for instrumental detection
	// (--detect-instrumental/--no-detect-instrumental); nil means no override.
	// GlobalDetectDefault is the global config default used when neither the
	// override nor the per-library setting is set. EnqueuePending resolves the
	// per-item decision from these via config.ResolveBool and stamps it onto each
	// enqueued item (stamp-on-insert; the worker reads it back later).
	DetectOverride      *bool
	GlobalDetectDefault bool
	// Timing reads the durable timing verdict so a Categorical track is not
	// re-fetched on every scan (#679). Optional: a nil Timing disables the
	// suppression entirely and preserves the pre-#679 behavior, so a caller that
	// does not wire it enqueues exactly as before.
	Timing TimingVerdictStore
	// ProvidersVersion is the CURRENT provider-set generation, compared against
	// the generation stored with a verdict to decide whether the verdict still
	// speaks for today's provider set. Zero means unknown, which never suppresses.
	ProvidersVersion int
	// Durations resolves a scanned file's exact audio duration for the #952
	// cache-hit timing check below. Optional: nil means "unknown" for every
	// row, which is the safe direction -- a row whose cached entry carries
	// synced lines is then routed to the worker rather than accepted here (see
	// EnqueuePending), never the reverse. A nil Durations therefore changes
	// behavior versus a plain res.Track.TrackLength fallback (that fallback is
	// no longer consulted at all; see the DurationLookup doc), but never
	// panics and never makes this check accept something it would otherwise
	// have refused.
	Durations DurationLookup
}

// shouldSuppress reports whether a track's stored timing verdict means this scan
// must not re-enqueue it.
//
// ONLY Categorical suppresses. That verdict means the lyric was timed to a
// different, longer recording -- the words themselves are suspect -- so the
// accept-time guard (#439) deliberately writes NOTHING. With no sidecar on disk,
// the row is indistinguishable from a track that was never fetched, so it is
// re-enqueued, re-fetched, re-judged and re-rejected on every scan forever.
//
// MisSynced is deliberately NOT suppressed. It writes a .txt, and #439's
// settled-sidecar check makes a later re-fetch a no-op: wasteful, never
// destructive. Suppressing it would hide a recoverable track for no gain.
//
// THE SUPPRESSION EXPIRES WITH THE PROVIDER GENERATION, and that is a deliberate
// choice rather than a default. A Categorical verdict is a statement about what
// the providers served at ONE MOMENT; permanent suppression would mean never
// finding correctly-timed lyrics that a provider starts serving later. Tying
// expiry to providers_version reuses the generation counter that already retires
// stale cache entries, instead of inventing a second, parallel expiry scheme.
//
// A zero current generation (unknown) never suppresses: without a trustworthy
// comparison the safe direction is to re-examine, since the cost is one refetch
// while the cost of wrongly suppressing is losing the track indefinitely.
func (e *Enqueuer) shouldSuppress(v TimingVerdict) bool {
	if v.Outcome != timing.Categorical {
		return false
	}
	if e.ProvidersVersion == 0 {
		return false
	}
	return v.ProvidersVersion == e.ProvidersVersion
}

// resolveFileDuration looks up the exact, independently-measured audio
// duration for filePath via e.Durations, keyed exactly as the worker records
// it (recordDuration, internal/worker/worker.go): the canonical
// (symlink-resolved) path, plus the current file's mtime and size. found is
// false whenever no usable duration is available -- a nil Durations, an empty
// path, a stat failure, or a genuine cache miss -- and the caller must treat
// that as "unknown", never as "zero seconds".
//
// One os.Stat plus one indexed DB read per row that actually reaches a found
// cache entry (never for a plain miss, since this is called from inside
// accept). A stat or lookup error is logged and swallowed: this check is an
// optimization on top of the worker's own judgment, not a new failure mode a
// scan must abort for.
func (e *Enqueuer) resolveFileDuration(ctx context.Context, filePath string) (seconds int, found bool) {
	if e.Durations == nil || strings.TrimSpace(filePath) == "" {
		return 0, false
	}
	info, err := os.Stat(filePath)
	if err != nil {
		slog.Debug("scan: stat failed while resolving audio duration for a cache-hit check; treating as unknown",
			"path", filePath, "error", err)
		return 0, false
	}
	key := pathutil.CanonicalPath(filePath)
	seconds, found, err = e.Durations.Lookup(ctx, key, info.ModTime().UnixNano(), info.Size())
	if err != nil {
		slog.Debug("scan: audio duration lookup failed while resolving a cache-hit check; treating as unknown",
			"path", key, "error", err)
		return 0, false
	}
	return seconds, found
}

// EnqueuePending reads pending scan results for libraryID, skips cache hits,
// and enqueues cache misses for worker processing. It returns the number of
// rows enqueued and the number short-circuited as cache hits so callers can log
// a per-scan summary; on error the partial counts so far are returned alongside.
func (e *Enqueuer) EnqueuePending(ctx context.Context, lib models.Library) (enqueued, cacheHits int, retErr error) {
	if e.Results == nil {
		return 0, 0, fmt.Errorf("scan: enqueuer results dependency is nil")
	}
	if e.Cache == nil {
		return 0, 0, fmt.Errorf("scan: enqueuer cache dependency is nil")
	}
	if e.Queue == nil {
		return 0, 0, fmt.Errorf("scan: enqueuer queue dependency is nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}

	// Resolve the per-library instrumental-detection decision once (CLI override >
	// per-library setting > global default) and stamp it onto every item enqueued
	// for this library; the worker reads it back at fetch time.
	detect := config.ResolveBool(e.DetectOverride, lib.DetectInstrumental, e.GlobalDetectDefault)
	libraryID := lib.ID

	results, err := e.Results.ListPendingByLibrary(ctx, libraryID)
	if err != nil {
		return 0, 0, fmt.Errorf("scan: list pending for enqueue: %w", err)
	}

	// Tracks skipped this pass because a Categorical verdict quarantined them.
	// Logged once at the end rather than returned: a per-track line would be
	// noise on a library where the count is stable across scans, and the full
	// operator surface (listing and re-examining quarantined tracks) belongs to
	// #629 rather than a second mechanism built here.
	suppressed := 0

	for _, res := range results {
		if err := ctx.Err(); err != nil {
			return enqueued, cacheHits, err
		}
		// accept judges a found row with the SAME predicate the worker's cache
		// lookup uses (lyrics.RefusedByTimingGuard), so a row the accept-time
		// timing guard would quarantine reads as a miss here too (#952): a build
		// before #950/#951 could have cached such an entry ahead of the guard,
		// and this scan-side check runs before the worker ever sees the track,
		// so without this the row is marked done forever with nothing written.
		//
		// res.Track.TrackLength is ALWAYS ZERO here: scan_results carries no
		// duration column (ListPendingByLibrary never selects one), unlike
		// work_queue rows, which the worker re-reads from the audio file at fetch
		// time (refreshRecordingIdentity). Passing 0 straight to
		// RefusedByTimingGuard would silently fall back to the CACHED song's own
		// catalog TrackLength -- the very recording the lyric was timed against --
		// which almost always passes, defeating the check this exists to run. So
		// resolveFileDuration below is consulted LAZILY (only once a cache row is
		// actually found) to get the file's real, independently-measured duration
		// before judging.
		accept := func(raw string) bool {
			fileDuration, fileDurationKnown := e.resolveFileDuration(ctx, res.FilePath)
			song := lyrics.DecodeCachedSong(raw, res.Track)
			if fileDurationKnown {
				return !lyrics.RefusedByTimingGuard(song, fileDuration)
			}
			// The file's real duration is unknown. A synced cached entry COULD be
			// a categorical mismatch against the real file -- accepting it here is
			// irreversible (it marks the row done and the track never reaches the
			// worker, which is the one path that judges against a freshly re-read
			// duration). Route doubt to the worker instead: treat as a miss so the
			// track is enqueued and judged there, where a real duration is always
			// re-derived. An unsynced/plain or instrumental entry carries no
			// timing to refuse and is still served as before.
			if len(song.Subtitles.Lines) == 0 || song.Track.Instrumental == 1 {
				return true
			}
			return false
		}
		_, err := e.Cache.LookupAccepted(ctx, res.Track.ArtistName, res.Track.TrackName, normalize.DurationBucket(res.Track.TrackLength), accept)
		switch {
		case err == nil:
			if err := e.Results.SetStatus(ctx, []int64{res.ID}, StatusDone); err != nil {
				return enqueued, cacheHits, fmt.Errorf("scan: mark cache hit done %d: %w", res.ID, err)
			}
			cacheHits++
			continue
		case errors.Is(err, sql.ErrNoRows):
		default:
			return enqueued, cacheHits, fmt.Errorf("scan: cache lookup %d: %w", res.ID, err)
		}

		// A Categorical verdict wrote no sidecar, so nothing on disk stops this
		// track being re-fetched forever. Consult the durable verdict instead
		// (#679), placed here -- after the cache check, before the row is reserved
		// -- so a suppressed track stays pending and is re-examined the moment the
		// provider generation moves, rather than being consumed by this scan.
		//
		// FAILS OPEN by construction: a lookup error is logged and ignored, since
		// suppression is an optimization and a failed read must never silently
		// drop work. Losing a track looks identical to having fetched it.
		if e.Timing != nil {
			verdict, found, terr := e.Timing.LookupTiming(ctx, res.Track.ArtistName, res.Track.TrackName)
			switch {
			case terr != nil:
				slog.Debug("scan: timing verdict lookup failed; enqueueing anyway",
					"result_id", res.ID, "error", terr)
			case found && e.shouldSuppress(verdict):
				slog.Debug("scan: skipping track quarantined by a categorical timing verdict",
					"result_id", res.ID, "providers_version", verdict.ProvidersVersion)
				suppressed++
				continue
			}
		}

		if err := e.Results.SetStatus(ctx, []int64{res.ID}, StatusProcessing); err != nil {
			return enqueued, cacheHits, fmt.Errorf("scan: reserve result %d: %w", res.ID, err)
		}
		inputs, err := scanInputs(res)
		if err != nil {
			if restoreErr := e.Results.SetStatus(ctx, []int64{res.ID}, StatusPending); restoreErr != nil {
				return enqueued, cacheHits, fmt.Errorf("scan: build inputs for result %d: %w; restore pending: %w", res.ID, err, restoreErr)
			}
			return enqueued, cacheHits, fmt.Errorf("scan: build inputs for result %d: %w", res.ID, err)
		}
		inputs.DetectInstrumental = &detect
		if _, err := e.Queue.Enqueue(ctx, inputs, e.Priority); err != nil {
			if restoreErr := e.Results.SetStatus(ctx, []int64{res.ID}, StatusPending); restoreErr != nil {
				return enqueued, cacheHits, fmt.Errorf("scan: enqueue result %d: %w; restore pending: %w", res.ID, err, restoreErr)
			}
			return enqueued, cacheHits, fmt.Errorf("scan: enqueue result %d: %w", res.ID, err)
		}
		enqueued++
	}
	// Never leave the suppression silent: a track skipped here produces no work
	// item, no sidecar and no queue row, so without this line an operator has no
	// way to tell "nothing left to fetch" from "N tracks are quarantined".
	if suppressed > 0 {
		slog.Info("scan: skipped tracks quarantined by a categorical timing verdict",
			"library_id", libraryID, "suppressed", suppressed,
			"providers_version", e.ProvidersVersion)
	}
	return enqueued, cacheHits, nil
}

// OnScanComplete adapts EnqueuePending to Scheduler.OnScanComplete.
func (e *Enqueuer) OnScanComplete(ctx context.Context, lib models.Library, _ []models.ScanResult, _ string, _ Trigger) error {
	_, _, err := e.EnqueuePending(ctx, lib)
	return err
}

// ResultInputs converts a scan result into queue inputs using the same outdir,
// filename, source path, and output-path derivation as scan-created work. The
// webhook resolver uses this so inventory-matched work is enqueued identically
// to work the scheduler enqueues.
func ResultInputs(res models.ScanResult) (models.Inputs, error) {
	return scanInputs(res)
}

func scanInputs(res models.ScanResult) (models.Inputs, error) {
	outdir := res.Outdir
	if outdir == "" && res.FilePath != "" {
		outdir = filepath.Dir(res.FilePath)
	}
	filename := res.Filename
	if filename == "" && res.FilePath != "" {
		base := filepath.Base(res.FilePath)
		filename = strings.TrimSuffix(base, filepath.Ext(base)) + ".lrc"
	}
	if outdir == "" && filename == "" && res.FilePath == "" {
		return models.Inputs{}, fmt.Errorf("invalid scan result: missing file path and output destination")
	}
	return models.Inputs{
		Track:        res.Track,
		Outdir:       outdir,
		Filename:     filename,
		SourcePath:   res.FilePath,
		ScanResultID: res.ID,
		OutputPaths: []models.OutputPath{{
			Outdir:   outdir,
			Filename: filename,
		}},
	}, nil
}
