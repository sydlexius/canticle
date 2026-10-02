package web

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/web/templates"
)

// queuePageSize is how many rows one /queue/{bucket} page (or "Show more"
// fragment) lists. A display bound, far under reports.MaxBucketLimit.
const queuePageSize = 50

// QueueActions is the seam for write actions on the queue pages: the revive
// flow in queue_actions.go calls it. The read-only view never does; a nil
// UI.queueActions means no action UI is rendered at all. *queue.DBQueue
// satisfies it (wired in the server layer).
type QueueActions interface {
	RecheckRetiredPreview(ctx context.Context) (queue.RecheckRetiredPreview, error)
	RecheckRetiredExpect(ctx context.Context, libraryID *int64, expected int64) (int64, error)
}

// AttachQueueActions wires the queue action backend onto an already-constructed
// UI (the post-construction pattern used by the server layer).
func (u *UI) AttachQueueActions(a QueueActions) { u.queueActions = a }

// registerQueueRoutes registers the queue drill-down routes through reg, so they
// are guarded exactly like every other page route.
func (u *UI) registerQueueRoutes(reg routeReg) {
	reg("GET /queue", u.handleQueueIndex)
	reg("GET /queue/{bucket}", u.handleQueueBucket)
	reg("GET /queue/unavailable/revive", u.handleReviveRetiredPreview)
	reg("POST /queue/unavailable/revive", u.handleReviveRetiredConfirm)
}

// queueBucketInfo is the heading and one-line meaning of each bucket page.
var queueBucketInfo = map[reports.Bucket][2]string{
	reports.BucketPending:     {"Queued", "Tracks waiting for their first lyrics lookup."},
	reports.BucketProcessing:  {"Processing", "Tracks a worker has claimed and is working on now."},
	reports.BucketDeferred:    {"Retrying", "Tracks waiting for the worker to try again: lookups that found nothing yet, and word-sync rechecks."},
	reports.BucketFailed:      {"Errored", "Tracks whose last lookup hit an error; they are retried automatically."},
	reports.BucketFinished:    {"Finished", "Tracks with word-synced lyrics, the best result there is."},
	reports.BucketSettled:     {"Settled (upgradable)", "Tracks with lyrics that could still be upgraded to word sync."},
	reports.BucketUnavailable: {"Given up", "Tracks given up on after repeated misses."},
}

// handleQueueBucket lists the rows behind one dashboard queue counter. An htmx
// request gets just the next rows (the "Show more" fragment); a plain
// navigation gets the full page, so every pager link is a real destination.
func (u *UI) handleQueueBucket(w http.ResponseWriter, r *http.Request) {
	// Row content is operational detail; never let a browser or proxy cache it.
	w.Header().Set("Cache-Control", "no-store")
	bucket, err := reports.ParseBucket(r.PathValue("bucket"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if u.reports == nil {
		slog.Error("reports repo not wired; cannot serve queue bucket", "bucket", string(bucket))
		http.Error(w, "queue data source unavailable", http.StatusServiceUnavailable)
		return
	}
	state, err := parseQueueViewState(r.URL.Query())
	if err != nil {
		http.Error(w, "invalid queue parameters: "+err.Error(), http.StatusBadRequest)
		return
	}

	// A query that normalizes to nothing (whitespace only) applies no filter, so
	// it is no search: drop it here, before it reaches the repo, the view, or any
	// pager link, so the page never claims a search that is not happening. The
	// search box then shows empty rather than echoing the stray whitespace.
	if normalize.NormalizeKey(state.Query) == "" {
		state.Query = ""
	}

	// Fetch one extra row to know whether another page exists, rather than
	// guessing from a full page (which would offer an empty "Show more").
	rows, err := u.reports.ListBucketFiltered(r.Context(), bucket, reports.BucketFilter{Query: state.Query}, state.After, queuePageSize+1)
	if err != nil {
		slog.Error("queue bucket query failed", "bucket", string(bucket), "error", err)
		http.Error(w, "queue query failed", http.StatusInternalServerError)
		return
	}
	more := len(rows) > queuePageSize
	if more {
		rows = rows[:queuePageSize]
	}
	info := queueBucketInfo[bucket]
	view := templates.QueueView{Key: string(bucket), Title: info[0], Blurb: info[1], After: state.After,
		Query: state.Query, StartHref: state.href(string(bucket), 0), ClearHref: (queueViewState{}).href(string(bucket), 0)}
	// Only the retired bucket can be revived; failed rows are already retried,
	// so no other bucket offers an action.
	view.ReviveLink = bucket == reports.BucketUnavailable && u.queueActions != nil
	for _, row := range rows {
		view.Rows = append(view.Rows, buildQueueRow(row, bucket))
	}
	if more {
		view.NextCursor = rows[len(rows)-1].ID
		view.MoreHref = state.href(string(bucket), view.NextCursor)
	}

	// Counts only: the query text is library metadata and is never logged.
	slog.Debug("queue bucket served", "bucket", string(bucket), "searched", state.Query != "", "rows", len(rows), "more", more)

	if r.Header.Get("HX-Request") == "true" {
		render(w, r, templates.QueueRows(view))
		return
	}
	render(w, r, templates.QueuePage(u.version, view, u.buildRail(""), u.musixmatchInactive, u.musixmatchServing))
}

// buildQueueRow formats one bucket row for display.
func buildQueueRow(row reports.BucketRow, from reports.Bucket) templates.QueueRow {
	names := make([]string, 0, len(row.Libraries))
	for _, l := range row.Libraries {
		names = append(names, l.Name)
	}
	libs := "-"
	if len(names) > 0 {
		libs = strings.Join(names, ", ")
	}
	// Complete does not reset next_attempt_at, so a settled row keeps a stale
	// retry time; it has no next attempt.
	nextAttempt := formatQueueTime(row.NextAttemptAt)
	if row.Status == queue.StatusDone || row.Status == queue.StatusUnavailable {
		nextAttempt = "-"
	}
	return templates.QueueRow{
		Artist:        row.Artist,
		Title:         row.Title,
		Album:         row.Album,
		Status:        row.Status,
		Reason:        row.Reason,
		NextAttemptAt: nextAttempt,
		MissCount:     strconv.FormatInt(row.MissCount, 10),
		Attempts:      strconv.FormatInt(row.Attempts, 10),
		UpdatedAt:     formatQueueTime(row.UpdatedAt),
		Libraries:     libs,
		PreviewHref:   queuePreviewHref(row, from),
	}
}

// queuePreviewHref is the player link for a row whose sidecar is a synced
// .lrc, or "" for every other row. The preview page 404s unless a regular .lrc
// sits beside the audio, but a list view must not stat per row (that wakes
// disks, the #684 shape), so eligibility is the row's recorded state
// (BucketRow.Previewable, the dashboard's own tier predicates). A sidecar
// removed since its tier was stamped is the one case that still 404s.
//
// from is the bucket the row is listed in; it rides as ?from= so the player can
// link back to that list (#1241). The page re-validates it against the bucket
// allowlist, so a hand-edited value can only fall back to /queue.
func queuePreviewHref(row reports.BucketRow, from reports.Bucket) string {
	if !row.Previewable {
		return ""
	}
	return "/preview/" + strconv.FormatInt(row.ID, 10) + "?from=" + string(from)
}

// formatQueueTime renders a stored timestamp in the same display zone as the
// other reports (serverDisplayLocation: TZ when valid, else UTC). An empty value is "-"; a value in an
// unrecognized layout is shown verbatim rather than dropped.
func formatQueueTime(raw string) string {
	if raw == "" {
		return "-"
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			if t.Unix() <= 0 {
				// The column's "no schedule" default is the Unix epoch; showing
				// 1969/1970 would read as a real, ancient retry time.
				return "-"
			}
			return formatReportTime(t, serverDisplayLocation())
		}
	}
	return raw
}
