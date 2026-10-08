package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/internal/tablesort"
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
	u.registerMarkRoutes(reg)
}

// queueBucketInfo is the heading and one-line meaning of each bucket page.
var queueBucketInfo = map[reports.Bucket][2]string{
	reports.BucketPending:     {"Queued", "Tracks waiting for their first lyrics lookup."},
	reports.BucketProcessing:  {"Processing", "Tracks a worker has claimed and is working on now."},
	reports.BucketDeferred:    {"Retrying", "Tracks waiting for the worker to try again: lookups that found nothing yet, and word-sync rechecks."},
	reports.BucketFailed:      {"Errored", "Tracks whose last lookup hit an error; they are retried automatically."},
	reports.BucketFinished:    {"Finished", "Tracks with word-synced lyrics, plus tracks marked instrumental by hand: nothing further to gain."},
	reports.BucketSettled:     {"Settled (upgradable)", "Tracks with lyrics that could still be upgraded to word sync. Also holds tracks whose every result you blocked, which wait until you unblock them."},
	reports.BucketUnavailable: {"Given up", "Tracks given up on after repeated misses."},
	reports.BucketBlocked:     {"Blocked", "Tracks where every lyric result found was one you marked wrong, so nothing is on disk."},
}

// lineTopBucketInfo overrides Finished and Settled when no word tier is reachable
// (reports.TopRungLine, #1275, #1350: word sync off or no word-capable lane): line-synced is the best result there.
var lineTopBucketInfo = map[reports.Bucket][2]string{
	reports.BucketFinished: {"Finished", "Tracks with line- or word-synced lyrics, plus tracks marked instrumental by hand: the best result available here."},
	reports.BucketSettled:  {"Settled (upgradable)", "Tracks with lyrics that could still be upgraded to line sync. Also holds tracks whose every result you blocked, which wait until you unblock them."},
}

// bucketInfo is the bucket's heading and meaning under rung top.
func bucketInfo(b reports.Bucket, top reports.TopRung) [2]string {
	if info, ok := lineTopBucketInfo[b]; ok && top == reports.TopRungLine {
		return info
	}
	return queueBucketInfo[b]
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
	state, err := parseQueueViewState(r.URL.Query(), bucket, u.reports.TopRung())
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

	// The Library filter offers the configured libraries; an id naming none is
	// ignored (dropped here so no link carries it), never an error. An htmx
	// "Show more" fragment renders no select, so it skips the query: its links
	// carry what the (already validated) page it came from carried.
	fragment := r.Header.Get("HX-Request") == "true"
	var libs []reports.BucketLibrary
	if !fragment {
		libs, err = u.reports.Libraries(r.Context())
		if err != nil {
			slog.Error("queue libraries query failed", "bucket", string(bucket), "error", err)
			http.Error(w, "queue query failed", http.StatusInternalServerError)
			return
		}
		state.Library = knownLibrary(libs, state.Library)
	}
	// Fetch one extra row to know whether another page exists, rather than
	// guessing from a full page (which would offer an empty "Show more").
	spec := reports.BucketSpec(bucket)
	order := spec.Resolve(state.Sort, state.Dir)
	// A forged or stale cursor (wrong shape for this sort) is not an error: the
	// page falls back to the top of the list.
	cursor, ok := spec.DecodeCursor(order, state.After)
	if !ok {
		state.After = ""
	}
	rows, err := u.reports.ListBucketFiltered(r.Context(), bucket, state.filter(), order, cursor, queuePageSize+1)
	if err != nil {
		slog.Error("queue bucket query failed", "bucket", string(bucket), "error", err)
		http.Error(w, "queue query failed", http.StatusInternalServerError)
		return
	}
	more := len(rows) > queuePageSize
	if more {
		rows = rows[:queuePageSize]
	}
	info := bucketInfo(bucket, u.reports.TopRung())
	view := templates.QueueView{Key: string(bucket), Title: info[0], Blurb: info[1], Status: templates.MarkStatusFromQuery(r.URL.Query()), After: cursor.ID,
		Query: state.Query, StartHref: state.href(string(bucket), ""), ClearHref: state.withoutQuery().href(string(bucket), ""),
		Columns: buildQueueColumns(string(bucket), state, spec, order), Sort: state.Sort, Dir: state.Dir,
		Chips: buildQueueChips(bucket, state, u.reports.TopRung()), Hidden: queueHiddenFilters(state), Filtered: state.chipsActive(),
		Libraries: buildLibraryOptions(libs, state.Library), Lanes: buildLaneOptions(state.Lane),
		Reasons: buildReasonOptions(bucket, state.Reason)}
	// Only the retired bucket can be revived; failed rows are already retried,
	// so no other bucket offers an action.
	view.ReviveLink = bucket == reports.BucketUnavailable && u.queueActions != nil
	for _, row := range rows {
		view.Rows = append(view.Rows, buildQueueRow(row, bucket, state))
	}
	if more {
		last := rows[len(rows)-1]
		view.NextCursor = last.ID
		// A cursor the decoder would refuse must never be emitted: the next page
		// would fall back to page 1 and repeat rows. Fail loudly, with the row id
		// only (the value is private library metadata), and offer no pager.
		if enc := (tablesort.Cursor{ID: last.ID, Val: last.SortVal}).Encode(); len(enc) > tablesort.MaxCursorBytes {
			slog.Error("queue cursor exceeds the cap; no further pages offered", "bucket", string(bucket), "row_id", last.ID)
		} else {
			view.MoreHref = state.href(string(bucket), enc)
		}
	}

	// Counts only: the query text is library metadata and is never logged.
	slog.Debug("queue bucket served", "bucket", string(bucket), "searched", state.Query != "", "rows", len(rows), "more", more)

	if fragment {
		render(w, r, templates.QueueRows(view))
		return
	}
	render(w, r, templates.QueuePage(u.version, view, u.buildRail(""), u.musixmatchInactive, u.musixmatchServing))
}

// buildQueueRow formats one bucket row for display.
func buildQueueRow(row reports.BucketRow, from reports.Bucket, state queueViewState) templates.QueueRow {
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
		PreviewHref:   queuePreviewHref(row, from, state),
		PreviewLabel:  queuePreviewLabel(row),
		Edited:        row.Edited,
		EditedTitle:   formatEditOffset(row.OffsetMS),
	}
}

// formatEditOffset renders a saved offset in seconds with an explicit sign; a
// value that rounds to zero at two decimals is "0.00 s" (no "+0.00"/"-0.00"),
// matching the editor's own zero.
func formatEditOffset(ms int64) string {
	s := fmt.Sprintf("%+.2f", float64(ms)/1000)
	if s == "+0.00" || s == "-0.00" {
		s = "0.00"
	}
	return s + " s"
}

// queuePreviewLabel is the player link text: the editor is writable only on a
// line-editable row (reports.BucketRow.LineEditable).
func queuePreviewLabel(row reports.BucketRow) string {
	if row.LineEditable {
		return "Preview / edit timing"
	}
	return "Preview"
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
func queuePreviewHref(row reports.BucketRow, from reports.Bucket, state queueViewState) string {
	if !row.Previewable {
		return ""
	}
	v := state.backLinkState().values()
	v.Set("from", string(from))
	return "/preview/" + strconv.FormatInt(row.ID, 10) + "?" + v.Encode()
}

// queueColumns is the table's column order (Artist, Album, Title first). A
// column with no sort key (Status, Reason, Libraries, Lyrics) is not orderable.
var queueColumns = []struct{ label, key string }{
	{"Artist", tablesort.KeyArtist}, {"Album", tablesort.KeyAlbum}, {"Title", tablesort.KeyTitle},
	{"Status", ""}, {"Reason", ""}, {"Next attempt", tablesort.KeyNextAttempt},
	{"Misses", tablesort.KeyMisses}, {"Attempts", tablesort.KeyAttempts}, {"Updated", tablesort.KeyUpdated},
	{"Libraries", ""}, {"Lyrics", ""},
}

// buildQueueColumns shapes the header row for the shared SortHeader component:
// each sortable header links to the order a click requests, keeping the search.
func buildQueueColumns(bucket string, state queueViewState, spec tablesort.Spec, active tablesort.Order) []templates.SortHeaderView {
	cols := make([]sortCol, len(queueColumns))
	for i, c := range queueColumns {
		cols[i] = sortCol{c.label, c.key}
	}
	return sortHeaders("/queue/"+bucket, cols, spec, active, "", state.filterValues())
}

// queueChipLabels are the chip labels, keyed by chip. Which chips a bucket
// shows, and in what order, is reports.BucketChips: the one place that decides.
var queueChipLabels = map[reports.Chip]string{
	reports.ChipLineSynced: "Line-synced (editable)",
	reports.ChipEdited:     "Hand-edited",
	reports.ChipWordSynced: "Word-synced",
	reports.ChipMissynced:  "Mis-synced",
}

// buildQueueChips is the chip row for the buckets that offer chips (nil
// elsewhere). Each chip links to the same view with that one chip toggled,
// keeping search, sort and the other chips and dropping the cursor (a stale
// position would hide rows). Line-synced, Word-synced and Mis-synced are mutually
// exclusive: turning one on turns the others off in the link.
//
// No chip shows a count, and nothing issues one. tier and edited read unindexed
// columns, so a count is a scan of the done partition per page view. A Settled
// mis-synced count (hypothetical) would look cheap (idx_work_queue_missynced) but
// is not once composed with the Settled bucket clause, which also reads
// outcome_type, sync_tier and last_error: EXPLAIN QUERY PLAN gives "SEARCH
// work_queue USING INDEX idx_work_queue_missynced (timing_outcome=? AND status=?)",
// a partial-index search with a table lookup per row, not a COVERING INDEX read.
func buildQueueChips(bucket reports.Bucket, state queueViewState, top reports.TopRung) []templates.QueueChip {
	offered := reports.BucketChips(bucket, top)
	if len(offered) == 0 {
		return nil
	}
	out := make([]templates.QueueChip, 0, len(offered))
	for _, c := range offered {
		next := state
		var active bool
		switch c {
		case reports.ChipLineSynced:
			active = state.Tier == reports.TierLine
			if active {
				next.Tier = ""
			} else {
				next.Tier, next.MisSynced, next.Word = reports.TierLine, false, false
			}
		case reports.ChipWordSynced:
			active = state.Word
			next.Word = !state.Word
		case reports.ChipEdited:
			active = state.Edited
			next.Edited = !state.Edited
		case reports.ChipMissynced:
			active = state.MisSynced
			next.MisSynced = !state.MisSynced
			if next.MisSynced {
				next.Tier = ""
			}
		}
		out = append(out, templates.QueueChip{Label: queueChipLabels[c], Active: active, Href: next.href(string(bucket), "")})
	}
	return out
}

// knownLibrary is id when it names one of libs, else 0 (no filter).
func knownLibrary(libs []reports.BucketLibrary, id int64) int64 {
	for _, l := range libs {
		if l.ID == id {
			return id
		}
	}
	return 0
}

// buildLibraryOptions is the Library select's options (nil when no library is
// configured, so the control is not rendered).
func buildLibraryOptions(libs []reports.BucketLibrary, selected int64) []templates.QueueOption {
	if len(libs) == 0 {
		return nil
	}
	out := make([]templates.QueueOption, 0, len(libs))
	for _, l := range libs {
		out = append(out, templates.QueueOption{Value: strconv.FormatInt(l.ID, 10), Label: l.Name, Selected: l.ID == selected})
	}
	return out
}

// buildLaneOptions is the Source select's options, labeled as the dashboard
// and reports label the same lanes.
func buildLaneOptions(selected string) []templates.QueueOption {
	var out []templates.QueueOption
	for _, l := range reports.Lanes() {
		out = append(out, templates.QueueOption{Value: l, Label: laneLabel(l), Selected: l == selected})
	}
	return out
}

// buildReasonOptions is the failure-reason select's options (nil on a bucket
// that does not offer the filter, so the control is not rendered).
func buildReasonOptions(bucket reports.Bucket, selected string) []templates.QueueOption {
	var out []templates.QueueOption
	for _, c := range reports.ReasonCategories(bucket) {
		out = append(out, templates.QueueOption{Value: c.Key, Label: c.Label, Selected: c.Key == selected})
	}
	return out
}

// queueHiddenFilters are the chip params the search form re-submits so a new
// search keeps the active chips.
func queueHiddenFilters(state queueViewState) []templates.QueueHidden {
	v := state.filterValues()
	v.Del("q")
	out := make([]templates.QueueHidden, 0, len(v))
	for _, k := range []string{"tier", "word", "edited", "missync"} {
		if val := v.Get(k); val != "" {
			out = append(out, templates.QueueHidden{Name: k, Value: val})
		}
	}
	return out
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
