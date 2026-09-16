package identityrepair

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/queue"
)

// DivergenceResult tallies RepairDivergence's outcome.
type DivergenceResult struct {
	Scanned         int // candidate work_queue rows examined
	Rekeyed         int // whole-group re-key-in-place corrections
	Merged          int // whole-group merges into an existing correct-key row
	DisplaySynced   int // key already agreed; only display columns were synced
	Unlinked        int // scan_results rows unlinked from a disagreeing queue row and reset to pending
	Deleted         int // work_queue rows deleted after every linked member was unlinked (no member still matches)
	ProcessingSkips int // candidate groups skipped because the queue row was mid-flight
}

// queueRow is a work_queue row's stored identity, as relevant to divergence
// detection.
type queueRow struct {
	id                  int64
	artist, albumArtist string
	artistKey           string
	titleKey            string
	status              string
}

// scanMember is one scan_results row linked to a candidate work_queue row.
type scanMember struct {
	id          int64
	libraryID   int64
	filePath    string
	artist      string
	albumArtist string
	artistKey   string
}

// divergenceOutcome reports what repairOneDivergentRow did for one candidate
// work_queue row.
type divergenceOutcome struct {
	rekeyed, merged, displaySynced, unlinked, deleted int
	processingSkip                                    bool
}

// RepairDivergence finds work_queue rows whose stored artist identity has
// fallen out of sync with the scan_results row(s) linked to them and repairs
// them from the database alone -- no file re-read (issue #963).
//
// THE GAP THIS CLOSES: a scan's baseUpsert (internal/scan/repository.go)
// rewrites scan_results' identity columns on conflict, but nothing propagates
// that correction to the coupled work_queue row. Run (the tag re-read pass)
// cannot see this either: it compares a file's on-disk tags against
// scan_results, and scan_results is ALREADY correct in this case, so Run finds
// nothing to change. The two rows silently diverge, and the affected track
// queries providers under the stale identity forever. Because the correct
// identity already sits in scan_results, this is a pure DB-only join/compare,
// unlike Run's per-row file open.
//
// SHARED QUEUE ROW: a work_queue row can be linked (via work_queue_scan_results)
// to several scan_results rows -- files whose normalized identity collapsed to
// one dedup key at enqueue time. When a later scan corrects SOME of those
// scan_results rows but not others (or corrects them to different identities),
// the group disagrees about what the queue row's identity should be:
//   - If every linked scan_results row now agrees on ONE key that differs from
//     the queue row's own key, the whole group has moved together: the queue
//     row is re-keyed (or merged into an existing row at that key) exactly as
//     Run's apply does for a single scan_result, reusing the same #960/#961
//     machinery (probeQueueConflict / reconcileQueue) so a 'done' row is
//     reopened with settle state cleared, an 'unavailable' row is never
//     reopened, and a mid-flight 'processing' row (at either the old or the
//     target key) is skipped rather than disturbed.
//   - If the group disagrees (some members still match the queue row's key,
//     or the diverging members disagree with EACH OTHER), the queue row is
//     never re-keyed -- that would orphan whichever members are still correct.
//     Instead, only the scan_results rows that no longer match the queue row's
//     key are unlinked from it (junction delete, plus clearing the scalar
//     work_queue.scan_result_id when it points at the unlinked row) and reset
//     to status='pending' so the next scan's EnqueuePending re-creates a queue
//     row at each one's own corrected identity (internal/scan/enqueuer.go:
//     ListPendingByLibrary -> Enqueue). See internal/scan/repository.go's
//     baseUpsert doc comment and internal/queue/queue.go's Enqueue for why this
//     is a correct and complete reset: a pending scan_results row with an
//     already-correct artist/artist_key needs nothing else to re-enqueue
//     cleanly. If that unlinking leaves the queue row with NO linked member at
//     all -- every member disagreed with its stored key -- the row itself is
//     deleted: left alive it would still fetch and write lyrics under its
//     stale identity the next time the worker dequeues it, and nothing else
//     ever revisits a 'pending'/'deferred'/'failed' row's identity once it is
//     no longer a divergence candidate.
//
// A candidate group's queue row in status='processing' is skipped ENTIRELY
// (not just the re-key sub-case): the row is mid-fetch, so nothing here should
// write to it. The divergence persists and is retried on the next pass --
// self-healing, matching #960/#961's existing "never disturb a processing
// row" invariant.
func (r *Repairer) RepairDivergence(ctx context.Context, opts Options) (DivergenceResult, error) {
	var res DivergenceResult
	ids, err := r.loadDivergentWorkQueueIDs(ctx, opts.LibraryID, opts.PathPrefix)
	if err != nil {
		return res, err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Scanned++
		if opts.Progress != nil && res.Scanned%progressEvery == 0 {
			opts.Progress(res.Scanned)
		}
		outcome, err := r.repairOneDivergentRow(ctx, id, opts.DryRun, opts.Report)
		if err != nil {
			return res, err
		}
		res.Rekeyed += outcome.rekeyed
		res.Merged += outcome.merged
		res.DisplaySynced += outcome.displaySynced
		res.Unlinked += outcome.unlinked
		res.Deleted += outcome.deleted
		if outcome.processingSkip {
			res.ProcessingSkips++
		}
	}
	return res, nil
}

// loadDivergentWorkQueueIDs returns the ids of work_queue rows with at least
// one linked scan_results row (matching title_key) whose stored artist,
// artist_key, or album_artist no longer matches the queue row's own. The list
// is read outside any transaction -- each candidate is re-validated inside its
// own transaction in repairOneDivergentRow, so a stale candidate (fixed or
// vanished between this read and its turn) is simply a safe no-op there.
//
// pathPrefix, when non-empty, additionally restricts candidates to
// scan_results rows whose file_path is at or under that directory (#963
// findings 3/4): a watcher-driven subtree rescan passes the rescanned path so
// this join costs the touched subtree, not the library. The SQL predicate is
// a half-open range seek (same technique as internal/prune's scope.childRange)
// that only NARROWS what the database returns; pathutil.WithinRoot is applied
// in Go on every row as the exact authority, so a lexical near-miss (a sibling
// directory sharing a name prefix, e.g. "/a/b" vs "/a/bc") can never leak in
// through the range predicate alone.
func (r *Repairer) loadDivergentWorkQueueIDs(ctx context.Context, libraryID *int64, pathPrefix string) ([]int64, error) {
	q := `SELECT DISTINCT wq.id, sr.file_path
	      FROM scan_results sr
	      JOIN work_queue_scan_results j ON j.scan_result_id = sr.id
	      JOIN work_queue wq ON wq.id = j.work_queue_id
	      WHERE sr.title_key = wq.title_key
	        AND (sr.artist_key != wq.artist_key OR sr.artist != wq.artist OR sr.album_artist != wq.album_artist)`
	var args []any
	if libraryID != nil {
		q += ` AND sr.library_id = ?`
		args = append(args, *libraryID)
	}
	if pathPrefix != "" {
		sep := string(filepath.Separator)
		lower := pathPrefix + sep
		upper := pathPrefix + string(filepath.Separator+1)
		q += ` AND (sr.file_path = ? OR (sr.file_path >= ? AND sr.file_path < ?))`
		args = append(args, pathPrefix, lower, upper)
	}
	q += ` ORDER BY wq.id`

	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("identityrepair: query divergent work_queue ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	seen := make(map[int64]bool)
	var out []int64
	for rows.Next() {
		var id int64
		var filePath string
		if err := rows.Scan(&id, &filePath); err != nil {
			return nil, fmt.Errorf("identityrepair: scan divergent work_queue id: %w", err)
		}
		if pathPrefix != "" && filePath != pathPrefix && !pathutil.WithinRoot(pathPrefix, filePath) {
			continue
		}
		if seen[id] {
			// DISTINCT applied to (wq.id, sr.file_path), not wq.id alone, so a queue
			// row with more than one divergent member's file_path in range can repeat
			// here; dedupe in Go rather than adding a second query shape.
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identityrepair: iterate divergent work_queue ids: %w", err)
	}
	return out, nil
}

// repairOneDivergentRow re-validates and resolves one candidate work_queue row
// inside its own transaction. dryRun computes and reports the same decision
// without writing: the transaction is always opened (its reads are what
// decide the outcome), but is committed only when !dryRun.
func (r *Repairer) repairOneDivergentRow(ctx context.Context, wqID int64, dryRun bool, report func(Change) error) (divergenceOutcome, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return divergenceOutcome{}, fmt.Errorf("identityrepair: begin divergence tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	wq, ok, err := loadQueueRowForDivergence(ctx, tx, wqID)
	if err != nil {
		return divergenceOutcome{}, err
	}
	if !ok {
		// Vanished since the candidate list was read (e.g. merged away by an
		// earlier candidate in this same pass): nothing to do.
		return divergenceOutcome{}, nil
	}
	if wq.status == queue.StatusProcessing {
		return divergenceOutcome{processingSkip: true}, nil
	}

	members, err := loadDivergenceGroupMembers(ctx, tx, wqID, wq.titleKey)
	if err != nil {
		return divergenceOutcome{}, err
	}
	if len(members) == 0 {
		// Race: the junction rows are gone (e.g. an earlier candidate's merge
		// re-pointed them elsewhere).
		return divergenceOutcome{}, nil
	}

	// One representative per distinct artist_key among the group (lowest scan_result
	// id), so a unanimous group has a single, deterministic source for the display
	// columns a re-key or sync writes.
	byKey := make(map[string]scanMember, len(members))
	for _, m := range members {
		if cur, ok := byKey[m.artistKey]; !ok || m.id < cur.id {
			byKey[m.artistKey] = m
		}
	}

	var outcome divergenceOutcome
	var changes []Change

	if len(byKey) == 1 {
		var key string
		var rep scanMember
		for k, v := range byKey {
			key, rep = k, v
		}
		switch {
		case key != wq.artistKey:
			// Unanimous correction: every linked scan_results row has moved off the
			// queue row's stored key. Reuse the exact re-key/merge machinery Run's
			// apply uses for a single scan_result (#960/#961): reopen a 'done'
			// survivor and clear its settle state, never reopen 'unavailable', merge
			// on conflict, and skip (without writing anything) if the target key's
			// row is itself mid-flight.
			ch := Change{
				WorkQueueID:    wq.id,
				ScanResultID:   rep.id,
				LibraryID:      rep.libraryID,
				FilePath:       rep.filePath,
				OldArtist:      wq.artist,
				NewArtist:      rep.artist,
				OldAlbumArtist: wq.albumArtist,
				NewAlbumArtist: rep.albumArtist,
				OldArtistKey:   wq.artistKey,
				NewArtistKey:   key,
			}
			lookup, err := probeQueueConflict(ctx, tx, ch, wq.titleKey)
			if err != nil {
				return divergenceOutcome{}, err
			}
			if lookup.skip {
				return divergenceOutcome{processingSkip: true}, nil
			}
			if dryRun {
				if lookup.conflictID != 0 {
					outcome.merged = 1
					ch.Op = OpQueueMerge
				} else {
					outcome.rekeyed = 1
					ch.Op = OpQueueRekey
				}
			} else {
				qOut, err := reconcileQueue(ctx, tx, ch, wq.titleKey, lookup)
				if err != nil {
					return divergenceOutcome{}, err
				}
				if qOut.queueMerged > 0 {
					outcome.merged = 1
					ch.Op = OpQueueMerge
				} else {
					outcome.rekeyed = 1
					ch.Op = OpQueueRekey
				}
			}
			changes = append(changes, ch)
		case rep.artist != wq.artist || rep.albumArtist != wq.albumArtist:
			// Key already agrees; only the display columns drifted -- mirrors
			// apply's !keyChanged sync branch. No conflict is possible (the key is
			// unchanged), and nothing is reopened: the artist_key that drives the
			// cache lookup and the provider query never moved.
			if !dryRun {
				if _, err := tx.ExecContext(ctx,
					`UPDATE work_queue SET artist = ?, album_artist = ? WHERE id = ?`,
					rep.artist, rep.albumArtist, wq.id); err != nil {
					return divergenceOutcome{}, fmt.Errorf("identityrepair: sync divergent work_queue %d: %w", wq.id, err)
				}
			}
			outcome.displaySynced = 1
			changes = append(changes, Change{
				Op:           OpQueueDisplaySync,
				WorkQueueID:  wq.id,
				ScanResultID: rep.id, LibraryID: rep.libraryID, FilePath: rep.filePath,
				OldArtist: wq.artist, NewArtist: rep.artist,
				OldAlbumArtist: wq.albumArtist, NewAlbumArtist: rep.albumArtist,
				OldArtistKey: wq.artistKey, NewArtistKey: key,
			})
		default:
			// The candidate query is stale (e.g. an earlier candidate in this pass
			// already synced it via a shared member): fully consistent already.
			return divergenceOutcome{}, nil
		}
	} else {
		// Disagreement: some members still match the queue row's own key, or the
		// diverging members disagree with each other. Never re-key -- that would
		// orphan whichever members are still correct. Unlink and reset only the
		// members that no longer match; every OTHER distinct key in a >1-key group
		// necessarily differs from wq.artistKey (at most one key can equal it), so
		// at least one member is always unlinked here.
		for _, m := range members {
			if m.artistKey == wq.artistKey {
				continue
			}
			if !dryRun {
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM work_queue_scan_results WHERE work_queue_id = ? AND scan_result_id = ?`,
					wq.id, m.id); err != nil {
					return divergenceOutcome{}, fmt.Errorf("identityrepair: unlink divergent scan_result %d from work_queue %d: %w", m.id, wq.id, err)
				}
				if _, err := tx.ExecContext(ctx,
					`UPDATE work_queue SET scan_result_id = NULL WHERE id = ? AND scan_result_id = ?`,
					wq.id, m.id); err != nil {
					return divergenceOutcome{}, fmt.Errorf("identityrepair: clear stale scalar link on work_queue %d: %w", wq.id, err)
				}
				if _, err := tx.ExecContext(ctx,
					`UPDATE scan_results SET status = 'pending' WHERE id = ?`, m.id); err != nil {
					return divergenceOutcome{}, fmt.Errorf("identityrepair: reset unlinked scan_result %d: %w", m.id, err)
				}
			}
			outcome.unlinked++
			changes = append(changes, Change{
				Op:           OpQueueUnlink,
				WorkQueueID:  wq.id,
				ScanResultID: m.id, LibraryID: m.libraryID, FilePath: m.filePath,
				OldArtist: wq.artist, NewArtist: m.artist,
				OldAlbumArtist: wq.albumArtist, NewAlbumArtist: m.albumArtist,
				OldArtistKey: wq.artistKey, NewArtistKey: m.artistKey,
			})
		}

		// If NO linked member still matches wq.artistKey, every member was just
		// unlinked above and the queue row now has nothing correct left to serve --
		// left alive, it would still write lyrics under its stale identity the next
		// time the worker dequeues it. Delete it (processing rows were already
		// skipped at the top of this function, before any of this ran). A matching
		// member surviving the loop means the row keeps its remaining, still-correct
		// link, so it is left exactly as before.
		if !dryRun {
			still, err := queueRowHasLinks(ctx, tx, wq.id)
			if err != nil {
				return divergenceOutcome{}, err
			}
			if !still {
				if _, err := tx.ExecContext(ctx, `DELETE FROM work_queue WHERE id = ?`, wq.id); err != nil {
					return divergenceOutcome{}, fmt.Errorf("identityrepair: delete orphaned divergent work_queue %d: %w", wq.id, err)
				}
				outcome.deleted = 1
				changes = append(changes, Change{
					Op:             OpQueueDelete,
					WorkQueueID:    wq.id,
					OldArtist:      wq.artist,
					OldAlbumArtist: wq.albumArtist,
					OldArtistKey:   wq.artistKey,
				})
			}
		} else {
			// Dry run: no unlink was written, so compute the same "would every member
			// end up unlinked" predicate directly from the in-memory group instead of
			// re-querying a database that was never mutated.
			anyMatch := false
			for _, m := range members {
				if m.artistKey == wq.artistKey {
					anyMatch = true
					break
				}
			}
			if !anyMatch {
				outcome.deleted = 1
				changes = append(changes, Change{
					Op:             OpQueueDelete,
					WorkQueueID:    wq.id,
					OldArtist:      wq.artist,
					OldAlbumArtist: wq.albumArtist,
					OldArtistKey:   wq.artistKey,
				})
			}
		}
	}

	if report != nil {
		for _, ch := range changes {
			if err := report(ch); err != nil {
				return divergenceOutcome{}, fmt.Errorf("identityrepair: report divergence change for scan_result %d: %w", ch.ScanResultID, err)
			}
		}
	}

	if dryRun {
		return outcome, nil // rolled back via the deferred Rollback; nothing was written
	}
	if err := tx.Commit(); err != nil {
		return divergenceOutcome{}, fmt.Errorf("identityrepair: commit divergence tx: %w", err)
	}
	return outcome, nil
}

// loadQueueRowForDivergence reads a work_queue row's stored identity and
// status. ok is false when the row no longer exists.
func loadQueueRowForDivergence(ctx context.Context, tx *sql.Tx, id int64) (queueRow, bool, error) {
	var q queueRow
	err := tx.QueryRowContext(ctx,
		`SELECT id, artist, album_artist, artist_key, title_key, status FROM work_queue WHERE id = ?`, id).
		Scan(&q.id, &q.artist, &q.albumArtist, &q.artistKey, &q.titleKey, &q.status)
	if errors.Is(err, sql.ErrNoRows) {
		return queueRow{}, false, nil
	}
	if err != nil {
		return queueRow{}, false, fmt.Errorf("identityrepair: load divergence work_queue %d: %w", id, err)
	}
	return q, true, nil
}

// queueRowHasLinks reports whether wqID still has at least one linked
// scan_results row via the junction table.
func queueRowHasLinks(ctx context.Context, tx *sql.Tx, wqID int64) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM work_queue_scan_results WHERE work_queue_id = ? LIMIT 1`, wqID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("identityrepair: check work_queue %d links: %w", wqID, err)
	}
	return true, nil
}

// loadDivergenceGroupMembers returns every scan_results row linked (via
// work_queue_scan_results) to wqID whose title_key matches the queue row's
// own -- the population whose artist identity determines what the queue row's
// identity should be. A linked row with a mismatched title_key is a separate,
// out-of-scope integrity question and is deliberately excluded rather than
// folded into the artist-only consensus this pass computes.
func loadDivergenceGroupMembers(ctx context.Context, tx *sql.Tx, wqID int64, titleKey string) ([]scanMember, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT sr.id, sr.library_id, sr.file_path, sr.artist, sr.album_artist, sr.artist_key
		 FROM scan_results sr
		 JOIN work_queue_scan_results j ON j.scan_result_id = sr.id
		 WHERE j.work_queue_id = ? AND sr.title_key = ?
		 ORDER BY sr.id`, wqID, titleKey)
	if err != nil {
		return nil, fmt.Errorf("identityrepair: load divergence group members for work_queue %d: %w", wqID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []scanMember
	for rows.Next() {
		var m scanMember
		if err := rows.Scan(&m.id, &m.libraryID, &m.filePath, &m.artist, &m.albumArtist, &m.artistKey); err != nil {
			return nil, fmt.Errorf("identityrepair: scan divergence group member: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identityrepair: iterate divergence group members: %w", err)
	}
	return out, nil
}
