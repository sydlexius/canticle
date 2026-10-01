package web

import (
	"strconv"

	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/web/templates"
)

// queueBucket is one work-queue bucket as the UI names it: the drill-down key,
// the short label, the hover tooltip, and the QueueSummary field it reads.
type queueBucket struct {
	Key     reports.Bucket
	Label   string
	Tooltip string
	Value   func(reports.QueueSummary) int64
}

// queueBuckets is the ONE definition of the work-queue buckets (#599). The
// dashboard tiles (buildQueueTiles), the doughnut's labels and values
// (buildQueueChart) and the Reports queue-summary rows (queueSummaryRows) are
// all derived from it, so a bucket cannot carry one name on a tile and another
// on its chart segment or Reports row. The chart color map in
// web/static/js/chart-init.js (QUEUE_COLOR_VARS) is keyed by Label, so a rename
// here must rename that key too; TestQueueBucketsHaveChartColors enforces it.
//
// Every value reads work_queue through reports.QueueSummary. Done is shown as
// its two halves, Finished and Settled (upgradable), never beside them (#553,
// maintainer decision 2026-09-24): with word-synced output the only terminal
// state, a bare "Done" reads as "finished" while most of it is a current-best
// snapshot. Replacing rather than adding keeps the set one status axis that
// sums to Total.
var queueBuckets = []queueBucket{
	{
		Key:     reports.BucketPending,
		Label:   "Pending",
		Tooltip: "Tracks waiting to be picked up by the background worker.",
		Value:   func(s reports.QueueSummary) int64 { return s.Pending },
	},
	{
		Key:     reports.BucketProcessing,
		Label:   "Processing",
		Tooltip: "Tracks currently being fetched by the worker.",
		Value:   func(s reports.QueueSummary) int64 { return s.Processing },
	},
	{
		Key:     reports.BucketFinished,
		Label:   "Finished",
		Tooltip: "Completed tracks whose lyrics carry word-level timing on disk. The only terminal state: nothing further to gain.",
		Value:   func(s reports.QueueSummary) int64 { return s.Finished },
	},
	{
		Key:     reports.BucketSettled,
		Label:   "Settled (upgradable)",
		Tooltip: "Completed tracks at their current best result (line-synced, unsynced, instrumental, or tier not yet recorded). Not treated as finished: word-synced is the only finished state.",
		Value:   func(s reports.QueueSummary) int64 { return s.SettledUpgradable },
	},
	{
		Key:     reports.BucketFailed,
		Label:   "Failed",
		Tooltip: "Tracks that hit an error; retried after a backoff delay.",
		Value:   func(s reports.QueueSummary) int64 { return s.Failed },
	},
	{
		Key:     reports.BucketDeferred,
		Label:   "Deferred",
		Tooltip: "No lyrics found yet; re-checked later.",
		Value:   func(s reports.QueueSummary) int64 { return s.Deferred },
	},
	{
		// Unavailable (#477): an exhausted benign miss, distinct from Done (which
		// implies a written sidecar) and from Failed/Deferred (still active or
		// retrying). Revival is manual only: RecheckRetired's sole production
		// caller is `queue recheck --retired`; a provider-set change does not
		// revive.
		Key:     reports.BucketUnavailable,
		Label:   "Unavailable",
		Tooltip: "Tracks retired after every lyrics source repeatedly found nothing; revived by 'queue recheck --retired'.",
		Value:   func(s reports.QueueSummary) int64 { return s.Unavailable },
	},
}

// queueSummaryRows shapes a QueueSummary into the Reports queue-summary table:
// one row per queueBuckets entry, in order, then Total.
func queueSummaryRows(s reports.QueueSummary) []templates.QueueSummaryRow {
	rows := make([]templates.QueueSummaryRow, 0, len(queueBuckets)+1)
	for _, b := range queueBuckets {
		rows = append(rows, templates.QueueSummaryRow{Status: b.Label, Count: strconv.FormatInt(b.Value(s), 10)})
	}
	return append(rows, templates.QueueSummaryRow{Status: "Total", Count: strconv.FormatInt(s.Total, 10), IsTotal: true})
}
