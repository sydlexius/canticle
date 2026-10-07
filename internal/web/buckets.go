package web

import (
	"strconv"

	"github.com/sydlexius/canticle/internal/reports"
)

// queueBucket is one work-queue bucket as the UI names it: the drill-down key,
// the short label, the hover tooltip, and the QueueSummary field it reads.
type queueBucket struct {
	Key     reports.Bucket
	Label   string
	Tooltip string
	// LineTooltip, when set, replaces Tooltip under reports.TopRungLine (word
	// sync off, #1275). The Label never changes: the chart colors key on it.
	LineTooltip string
	Value       func(reports.QueueSummary) int64
}

// tooltip is the hover text under rung top.
func (b queueBucket) tooltip(top reports.TopRung) string {
	if top == reports.TopRungLine && b.LineTooltip != "" {
		return b.LineTooltip
	}
	return b.Tooltip
}

// queueBuckets is the ONE definition of the work-queue buckets (#599). The
// dashboard tiles (buildQueueTiles), the doughnut's labels and values
// (buildQueueChart) are
// all derived from it, so a bucket cannot carry one name on a tile and another
// on its chart segment. The chart color map in
// web/static/js/chart-init.js (CAT_VARS) is keyed by Label, so a rename
// here must rename that key too; TestQueueBucketsHaveChartColors enforces it.
//
// Every value reads work_queue through reports.QueueSummary. Done is shown as
// its two halves, Finished and Settled (upgradable), never beside them (#553,
// maintainer decision 2026-09-24): with word-synced output the only terminal
// state, a bare "Done" reads as "finished" while most of it is a current-best
// snapshot. Buckets run activity first (Retrying, Errored, Queued) then the
// settled states. There is deliberately no Processing bucket: an in-flight row
// is shown in the dashboard's Up Next panel (#599), so the buckets sum to Total
// minus the rows currently claimed by the worker.
var queueBuckets = []queueBucket{
	{
		Key:     reports.BucketDeferred,
		Label:   "Retrying",
		Tooltip: "Tracks waiting for the worker to try again after a delay: lookups that found nothing yet, and word-sync rechecks.",
		Value:   func(s reports.QueueSummary) int64 { return s.Deferred },
	},
	{
		Key:     reports.BucketFailed,
		Label:   "Errored",
		Tooltip: "Tracks that hit an error. They will be retried automatically after a backoff delay.",
		Value:   func(s reports.QueueSummary) int64 { return s.Failed },
	},
	{
		Key:     reports.BucketPending,
		Label:   "Queued",
		Tooltip: "Tracks waiting for their first lookup by the background worker.",
		Value:   func(s reports.QueueSummary) int64 { return s.Pending },
	},
	{
		Key:         reports.BucketFinished,
		Label:       "Finished",
		Tooltip:     "Completed tracks with word-level timing on disk, plus instrumentals you marked by hand. The only terminal state: nothing further to gain.",
		LineTooltip: "Completed tracks with line- or word-synced lyrics on disk, plus instrumentals you marked by hand. No word-synced tier is available here, so line-synced is the best result: nothing further to gain.",
		Value:       func(s reports.QueueSummary) int64 { return s.Finished },
	},
	{
		Key:         reports.BucketSettled,
		Label:       "Settled (upgradable)",
		Tooltip:     "Completed tracks at their current best result (line-synced, unsynced, detected or provider-flagged instrumental, or tier not yet recorded). Not finished: a later run may still improve them. Hand-marked instrumentals are not here.",
		LineTooltip: "Completed tracks below line sync (unsynced, detected or provider-flagged instrumental, or tier not yet recorded). No word-synced tier is available here, so they could still be upgraded to line sync. Hand-marked instrumentals are not here.",
		Value:       func(s reports.QueueSummary) int64 { return s.SettledUpgradable },
	},
	{
		// Given up is status 'unavailable' (#477): an exhausted benign miss,
		// distinct from Done (which implies a written sidecar) and from
		// Errored/Retrying (still active or retrying). Revival is manual only:
		// RecheckRetired's sole production caller is `queue recheck --retired`; a
		// provider-set change does not revive.
		Key:     reports.BucketUnavailable,
		Label:   "Given up",
		Tooltip: "Tracks retired after every lyrics source repeatedly found nothing; revived by 'queue recheck --retired'.",
		Value:   func(s reports.QueueSummary) int64 { return s.Unavailable },
	},
}

// tooltip is the hover text under rung top.
func (b resultBucket) tooltip(top reports.TopRung) string {
	if top == reports.TopRungLine && b.LineTooltip != "" {
		return b.LineTooltip
	}
	return b.Tooltip
}

// resultBucket is one Results tile: what a COMPLETED track ended up with.
type resultBucket struct {
	Label   string
	Tooltip string
	// LineTooltip, when set, replaces Tooltip under reports.TopRungLine (#1275).
	LineTooltip string
	Value       func(reports.ResultsBreakdown) int64
	// Href is the Work Queue view listing exactly this tile's population under
	// rung top, or "" when no bucket + chip combination yields it (#1237). A
	// near-miss filter is never linked: the list's total must equal the tile's
	// count, which TestResultTilesLinkToEqualPopulations pins per rung.
	// lane, when non-empty, narrows the view to one source's rows (the source
	// page rows share these rules, so the two cannot drift).
	Href func(top reports.TopRung, lane string) string
}

// resultsHref is the /queue/{bucket} URL carrying the given chip state. It is
// built through queueViewState.href, the page's own serializer, so the link
// and what parseQueueViewState reads back cannot drift.
func resultsHref(b reports.Bucket, s queueViewState) string { return s.href(string(b), "") }

// wrongTimingQueueHref is the Work Queue's Mis-synced view: the Settled bucket
// with the Mis-synced chip, which Settled offers under both rungs.
func wrongTimingQueueHref() string {
	return resultsHref(reports.BucketSettled, queueViewState{MisSynced: true})
}

// reviewPreviewHref is the player link for a review-queue row, "" unless its
// recorded tier is a settled word/line one (the file may still be gone). No `from`: the row is not
// listed in a bucket, so the player's Back falls back to /queue.
func reviewPreviewHref(q reports.ReviewQueueItem) string {
	if !q.Previewable {
		return ""
	}
	return "/preview/" + strconv.FormatInt(q.ID, 10)
}

// resultBuckets is the ONE definition of the dashboard Results row (#599). It
// splits the completed (done) population by result type, so the tiles always
// sum to the Finished + Settled (upgradable) pair on the Work Queue row
// (reports.ResultsBreakdown puts every done row in exactly one bucket). It
// lives beside queueBuckets because both rows share the label, tooltip and
// reader shape and are edited together. The source page's by-type doughnut
// reuses these labels, so chart-init.js keys a color on each
// (TestResultBucketsHaveChartColors).
var resultBuckets = []resultBucket{
	{
		Label:   "Word-synced",
		Tooltip: "Synced lyrics with word-level timing on disk. Terminal: nothing further to gain from a re-fetch or word-sync recheck.",
		Value:   func(b reports.ResultsBreakdown) int64 { return b.WordSynced },
		// Finished also holds hand-marked instrumentals (#1405), and under the
		// line rung the line tier; the Word-synced chip (word=1) narrows it to
		// exactly this tile's population under both rungs.
		Href: func(_ reports.TopRung, lane string) string {
			return resultsHref(reports.BucketFinished, queueViewState{Lane: lane, Word: true})
		},
	},
	{
		Label:       "Line-synced",
		Tooltip:     "The .lrc on disk has line-level timing and no word timing. It may still be upgraded.",
		LineTooltip: "The .lrc on disk has line-level timing and no word timing: the best result available here.",
		Value:       func(b reports.ResultsBreakdown) int64 { return b.LineSynced },
		// The Line-synced chip lives on Settled under the word rung and moves to
		// Finished under the line rung (reports.BucketChips).
		Href: func(top reports.TopRung, lane string) string {
			b := reports.BucketSettled
			if top == reports.TopRungLine {
				b = reports.BucketFinished
			}
			if !reports.HasChip(b, reports.ChipLineSynced, top) {
				return ""
			}
			return resultsHref(b, queueViewState{Lane: lane, Tier: reports.TierLine})
		},
	},
	{
		Label:   "Unsynced",
		Tooltip: "Plain-text lyrics only (a .txt), with no timing on disk.",
		Value:   func(b reports.ResultsBreakdown) int64 { return b.Unsynced },
	},
	{
		Label:   "Instrumental",
		Tooltip: "Tracks marked instrumental (no lyrics expected): by audio detection, a provider's own flag, or by hand.",
		Value:   func(b reports.ResultsBreakdown) int64 { return b.Instrumental },
	},
	{
		Label:   "Tier unknown",
		Tooltip: "A synced .lrc with no recorded tier: completed before tier tracking, served from cache, or later demoted by the timing guard. 'canticle scan reconcile-sync-tier' classifies most from the file itself.",
		Value:   func(b reports.ResultsBreakdown) int64 { return b.SyncedTierUnknown },
	},
	{
		Label:   "Other",
		Tooltip: "Completed with nothing written: refused by the language guard, retired because the audio file is gone, or settled before outcomes were recorded.",
		Value:   func(b reports.ResultsBreakdown) int64 { return b.Other },
	},
}
