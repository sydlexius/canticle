// Package reports provides read-only, run-on-demand reports over the existing
// SQLite data (work_queue, scan_results, provider_outcomes). Every method is
// strictly read-only: there are no write paths and no schema migrations -- all
// five reports are answerable over columns the DB already holds.
//
// The repository wraps *sql.DB and follows the same pattern as internal/cache.
package reports

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// timeFormat matches the layout work_queue.completed_at is stored in
// (see internal/queue: time.RFC3339).
const timeFormat = time.RFC3339

// Repo provides read-only report queries over the application database.
type Repo struct {
	db *sql.DB
}

// New returns a Repo backed by db.
func New(db *sql.DB) *Repo {
	return &Repo{db: db}
}

// QueueSummary is a count of work_queue rows per status. Every status field is
// populated (zero when no rows have that status) so callers can render a
// complete table without special-casing absent statuses.
type QueueSummary struct {
	Pending    int64
	Processing int64
	Done       int64
	Failed     int64
	Deferred   int64
	Total      int64
}

// QueueSummary returns the count of work_queue rows grouped by status.
//
// Source: work_queue.status (CHECK-constrained to pending/processing/done/
// failed/deferred by migrations 001 + 012). Zero-count statuses are reported
// as 0 rather than omitted.
func (r *Repo) QueueSummary(ctx context.Context) (QueueSummary, error) {
	var s QueueSummary
	rows, err := r.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM work_queue GROUP BY status`)
	if err != nil {
		return QueueSummary{}, fmt.Errorf("reports: queue summary: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return QueueSummary{}, fmt.Errorf("reports: scan queue summary: %w", err)
		}
		switch status {
		case "pending":
			s.Pending = count
		case "processing":
			s.Processing = count
		case "done":
			s.Done = count
		case "failed":
			s.Failed = count
		case "deferred":
			s.Deferred = count
		}
		s.Total += count
	}
	if err := rows.Err(); err != nil {
		return QueueSummary{}, fmt.Errorf("reports: queue summary rows: %w", err)
	}
	return s, nil
}

// ResultClass is the derived classification of a completed work_queue row.
type ResultClass string

const (
	// ResultSynced means an .lrc (synced lyrics) file was written.
	ResultSynced ResultClass = "synced"
	// ResultUnsyncedOrInstrumental means a .txt file was written. This groups
	// plain unsynced lyrics and instrumental markers together: both land in a
	// .txt output and are not distinguishable from output_paths alone (per the
	// issue's Design Choice 2). Use InstrumentalInventory to isolate
	// audio-detected instrumentals.
	ResultUnsyncedOrInstrumental ResultClass = "unsynced-or-instrumental"
	// ResultMiss means the item exhausted its miss budget with no lyrics found
	// (status='done', last_error='miss limit reached', no output written).
	ResultMiss ResultClass = "miss"
	// ResultUnknown means the row could not be classified (e.g. legacy row with
	// no/empty output_paths and no miss marker).
	ResultUnknown ResultClass = "unknown"
)

// RecentOutcome is one recently-completed track with its derived result class.
type RecentOutcome struct {
	Artist string
	Title  string
	Album  string
	// CompletedAt is the completion timestamp; zero value when completed_at is
	// NULL (such rows sort last).
	CompletedAt time.Time
	// ProviderLane is the winning provider lane recorded at completion; empty
	// when NULL (not recorded, or a miss with no winning provider).
	ProviderLane string
	// Result is the classification derived from last_error / output_paths.
	Result ResultClass
}

// RecentOutcomes returns the most recently completed (status='done') tracks,
// newest first by completed_at (NULLs sorted last), capped at limit.
//
// Source: work_queue rows where status='done'. The result classification is
// computed in SQL: last_error='miss limit reached' -> miss; otherwise the first
// output filename (json_extract(output_paths, '$[0].filename')) ending in .lrc
// -> synced, .txt -> unsynced-or-instrumental (grouped, see ResultClass),
// anything else -> unknown. json_valid guards legacy rows whose output_paths is
// empty/non-JSON so json_extract never errors.
func (r *Repo) RecentOutcomes(ctx context.Context, limit int) ([]RecentOutcome, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT artist, title, album, completed_at, provider_lane,
            CASE
                WHEN last_error = 'miss limit reached' THEN 'miss'
                WHEN json_valid(output_paths)
                    AND json_extract(output_paths, '$[0].filename') LIKE '%.lrc' THEN 'synced'
                WHEN json_valid(output_paths)
                    AND json_extract(output_paths, '$[0].filename') LIKE '%.txt' THEN 'unsynced-or-instrumental'
                ELSE 'unknown'
            END AS result
         FROM work_queue
         WHERE status = 'done'
         ORDER BY completed_at IS NULL, completed_at DESC, id DESC
         LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("reports: recent outcomes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []RecentOutcome
	for rows.Next() {
		var (
			o            RecentOutcome
			completedAt  sql.NullString
			providerLane sql.NullString
			result       string
		)
		if err := rows.Scan(&o.Artist, &o.Title, &o.Album, &completedAt, &providerLane, &result); err != nil {
			return nil, fmt.Errorf("reports: scan recent outcome: %w", err)
		}
		if completedAt.Valid && completedAt.String != "" {
			t, err := time.Parse(timeFormat, completedAt.String)
			if err != nil {
				return nil, fmt.Errorf("reports: parse completed_at %q: %w", completedAt.String, err)
			}
			o.CompletedAt = t
		}
		o.ProviderLane = providerLane.String
		o.Result = ResultClass(result)
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: recent outcomes rows: %w", err)
	}
	return out, nil
}

// ProviderEffectiveness is the hit/miss tally and derived hit-rate for one
// provider lane.
type ProviderEffectiveness struct {
	Lane   string
	Hits   int64
	Misses int64
	// HitRate is Hits / (Hits + Misses), in [0,1]; 0 when the lane has no
	// recorded attempts. This is a TRUE per-track hit-rate (issue #282): Hits and
	// Misses are per-track attempt outcomes from lane_attempts, so a lane that was
	// tried but lost to a later winning lane is correctly counted as a miss.
	HitRate float64
}

// ProviderEffectiveness returns per-lane hit/miss counts and a TRUE per-track
// hit-rate (issue #282).
//
// Source-of-truth decision: the lane_attempts table (migration 022), NOT the
// attempt-weighted provider_outcomes aggregate (migration 018). lane_attempts
// records one row per (track, lane) for every ATTEMPTED lane: hit=1 for the lane
// that served the track and hit=0 for every other attempted lane, INCLUDING a
// lane that lost to a later winning lane. That is the exact case provider_outcomes
// cannot express (it records a miss only on the all-lanes-benign-miss path), so a
// hit-rate derived from it over-states tried-but-not-winning lanes. provider_outcomes
// is intentionally kept in parallel (still maintained by the worker and read by
// /metrics); this report no longer reads it.
//
// NO BACKFILL: lane_attempts is empty for all traffic that predates migration 022
// (the per-lane history did not exist before, so it cannot be reconstructed). Until
// new attempts accrue, this query returns no rows and Report 3 shows its empty
// state. The hit-rate is computed in Go to keep integer-vs-float division explicit
// and avoid SQL NULL-on-zero-divisor edge cases.
func (r *Repo) ProviderEffectiveness(ctx context.Context) ([]ProviderEffectiveness, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT lane,
		        SUM(CASE WHEN hit = 1 THEN 1 ELSE 0 END) AS hits,
		        SUM(CASE WHEN hit = 0 THEN 1 ELSE 0 END) AS misses
		   FROM lane_attempts
		  GROUP BY lane
		  ORDER BY lane`)
	if err != nil {
		return nil, fmt.Errorf("reports: provider effectiveness: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ProviderEffectiveness
	for rows.Next() {
		var p ProviderEffectiveness
		if err := rows.Scan(&p.Lane, &p.Hits, &p.Misses); err != nil {
			return nil, fmt.Errorf("reports: scan provider effectiveness: %w", err)
		}
		if total := p.Hits + p.Misses; total > 0 {
			p.HitRate = float64(p.Hits) / float64(total)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: provider effectiveness rows: %w", err)
	}
	return out, nil
}

// InstrumentalTrack is one audio-detected instrumental work_queue row, joined to
// its source file(s).
type InstrumentalTrack struct {
	WorkQueueID int64
	Artist      string
	Title       string
	// FilePath is the source audio path from scan_results; empty when the row
	// has no linked scan_results (e.g. CLI-enqueued items). A work_queue row
	// that collapsed multiple files yields one InstrumentalTrack per file.
	FilePath string
	// DetectRequested is the per-item detect_instrumental request flag stamped
	// at enqueue (migration 016): NULL (not Valid) = no decision stamped, the
	// worker used the global config default; 0 = detection off; 1 = detection
	// on. This is the REQUEST source, distinct from the detection RESULT
	// (instrumental_result), which equals 1 for every row returned here.
	DetectRequested sql.NullInt64
}

// InstrumentalInventory returns work_queue rows the audio detector confirmed as
// instrumental (instrumental_result = 1), joined to scan_results for file
// identity, and surfaces the detect_instrumental request flag so callers can
// distinguish detection-requested from detected-instrumental.
//
// Source: work_queue.instrumental_result (migration 018; NULL=not run, 0=ran
// not instrumental, 1=detected instrumental) filtered to =1, left-joined via
// work_queue_scan_results (migration 010) to scan_results for file_path, with
// work_queue.detect_instrumental (migration 016) carried as the request flag.
// The LEFT JOIN keeps CLI-enqueued rows that have no scan_results link.
func (r *Repo) InstrumentalInventory(ctx context.Context) ([]InstrumentalTrack, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT wq.id, wq.artist, wq.title, wq.detect_instrumental, COALESCE(sr.file_path, '')
         FROM work_queue wq
         LEFT JOIN work_queue_scan_results wqsr ON wqsr.work_queue_id = wq.id
         LEFT JOIN scan_results sr ON sr.id = wqsr.scan_result_id
         WHERE wq.instrumental_result = 1
         ORDER BY wq.id, sr.id`)
	if err != nil {
		return nil, fmt.Errorf("reports: instrumental inventory: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []InstrumentalTrack
	for rows.Next() {
		var t InstrumentalTrack
		if err := rows.Scan(&t.WorkQueueID, &t.Artist, &t.Title, &t.DetectRequested, &t.FilePath); err != nil {
			return nil, fmt.Errorf("reports: scan instrumental track: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: instrumental inventory rows: %w", err)
	}
	return out, nil
}

// CountInstrumental returns the number of work_queue rows the audio detector
// confirmed as instrumental (instrumental_result = 1).
func (r *Repo) CountInstrumental(ctx context.Context) (int64, error) {
	var n int64
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM work_queue WHERE instrumental_result = 1`).Scan(&n); err != nil {
		return 0, fmt.Errorf("reports: count instrumental: %w", err)
	}
	return n, nil
}

// FailureGroup is a count of failed/deferred work_queue rows sharing one status
// and reason.
type FailureGroup struct {
	// Status is 'failed' or 'deferred'. Grouping keeps the two distinct because
	// they mean different things: 'deferred' rows are benign misses awaiting a
	// later retry, while 'failed' rows hit a hard error.
	Status string
	// Reason is the normalized work_queue.last_error text ('unknown' when no
	// error was recorded), matching the empty-string-to-unknown normalization
	// used by internal/queue.CountFailuresByReason.
	Reason string
	Count  int64
}

// FailureAnalysis returns failed and deferred work_queue rows grouped by reason
// (last_error), with a count per group, ordered most-frequent first.
//
// Source: work_queue rows where status IN ('failed','deferred'), grouped by
// (status, normalized last_error). Status is included in the grouping so a
// deferred miss and a hard failure carrying the same last_error text are
// reported separately. An empty last_error normalizes to 'unknown' (via
// COALESCE over NULLIF), matching internal/queue.CountFailuresByReason.
func (r *Repo) FailureAnalysis(ctx context.Context) ([]FailureGroup, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT status, COALESCE(NULLIF(last_error, ''), 'unknown') AS reason, COUNT(*) AS n
         FROM work_queue
         WHERE status IN ('failed', 'deferred')
         GROUP BY status, reason
         ORDER BY n DESC, status, reason`)
	if err != nil {
		return nil, fmt.Errorf("reports: failure analysis: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []FailureGroup
	for rows.Next() {
		var g FailureGroup
		if err := rows.Scan(&g.Status, &g.Reason, &g.Count); err != nil {
			return nil, fmt.Errorf("reports: scan failure group: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reports: failure analysis rows: %w", err)
	}
	return out, nil
}
