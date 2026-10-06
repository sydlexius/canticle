package queue

import (
	"context"
	"database/sql"
)

// scanResultNoLiveSibling is the owner guard every 'done' writeback to
// scan_results carries (#1038), so the completion paths cannot drift apart. A
// scan_result can be linked to more than one work_queue row: a forced rescan
// re-keys a changed file, identity repair skips the old row while it is
// processing, and Enqueue links the result under the new key while the old
// link survives. Without this guard the first row to finish marks the result
// done while its sibling is still unfinished.
//
// The single placeholder is the id of the work_queue row doing the writeback;
// that row never blocks itself. A sibling blocks while it is live: any status
// but done/unavailable (so failed and deferred retries count), EXCEPT a
// re-trip of an already-settled row (a word recheck, word_timing_state =
// 'queued', or an upgrade trip, upgrade_queued = 1). A re-trip's scan ledger
// was settled before the trip began and SettleUpgradeTrip writes nothing back,
// so letting one block would strand the result short of done.
//
// The last sibling to finish writes the result back. A blocking sibling that
// is deleted instead of finishing (Cleanup, CancelByLibrary) resets the
// results it was the last live owner of to 'pending' in the same transaction
// (resetScanResultsPending), because a scan offers only pending results to
// Enqueue and would otherwise never revisit them.
const scanResultNoLiveSibling = `
           AND NOT EXISTS (
               SELECT 1 FROM work_queue_scan_results sib
               JOIN work_queue sw ON sw.id = sib.work_queue_id
               WHERE sib.scan_result_id = scan_results.id
                 AND sib.work_queue_id <> ?
                 AND sw.status NOT IN ('done', 'unavailable')
                 AND COALESCE(sw.word_timing_state, '') <> 'queued'
                 AND sw.upgrade_queued = 0)`

// scanResultsDoneWriteback marks every scan_result linked to one work_queue row
// done, under the shared owner guard. Args: the work_queue id, twice.
const scanResultsDoneWriteback = `UPDATE scan_results SET status = 'done'
         WHERE id IN (SELECT scan_result_id FROM work_queue_scan_results WHERE work_queue_id = ?)
           AND status != 'done'` + scanResultNoLiveSibling

// writeBackScanResultsDone runs scanResultsDoneWriteback for workQueueID inside
// tx. Callers wrap the error with their own context.
func writeBackScanResultsDone(ctx context.Context, tx *sql.Tx, workQueueID int64) error {
	_, err := tx.ExecContext(ctx, scanResultsDoneWriteback, workQueueID, workQueueID)
	return err
}

// scanResultsPendingReset resets to 'pending' the scan_results linked to one
// work_queue row that deleting it would strand: not already done, and no OTHER
// live sibling remains (the same guard as the done writeback). Args: the
// work_queue id, twice.
const scanResultsPendingReset = `UPDATE scan_results SET status = 'pending'
         WHERE id IN (SELECT scan_result_id FROM work_queue_scan_results WHERE work_queue_id = ?)
           AND status != 'done'` + scanResultNoLiveSibling

// resetScanResultsPending runs scanResultsPendingReset for workQueueID inside
// tx. Call it BEFORE deleting the row, while the junction still links it.
func resetScanResultsPending(ctx context.Context, tx *sql.Tx, workQueueID int64) error {
	_, err := tx.ExecContext(ctx, scanResultsPendingReset, workQueueID, workQueueID)
	return err
}
