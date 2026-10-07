package lyricblock

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	dbpkg "github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/queue"
)

// ErrUnblockTarget is returned when an UnblockRequest does not name exactly one
// of a block or a work item.
var ErrUnblockTarget = errors.New("lyricblock: set exactly one of BlockID and WorkItemID")

// settledBlocked is the work_queue.outcome_type a row carries once every result
// for it was blocked. The worker arm that writes it is #1395; until it lands no
// row has the value and Unblock reopens nothing.
const settledBlocked = "blocked"

// UnblockRequest names what to unblock: one block, or every block on a work
// item's track identity.
type UnblockRequest struct {
	BlockID    int64
	WorkItemID int64
}

// UnblockResult holds counts only.
type UnblockResult struct {
	Removed  int // blocks deleted
	Reopened int // rows settled as blocked that went back to pending
}

// Unblock deletes the named block(s) and reopens a done row on the same track
// identity that settled as blocked, in one transaction. It does not restore any
// lyric file: the next fetch decides what the track gets.
func (s *Service) Unblock(ctx context.Context, req UnblockRequest) (UnblockResult, error) {
	if (req.BlockID == 0) == (req.WorkItemID == 0) {
		return UnblockResult{}, ErrUnblockTarget
	}
	var artistKey, titleKey string
	var err error
	if req.BlockID != 0 {
		err = s.db.QueryRowContext(ctx, `SELECT artist_key, title_key FROM lyric_blocks WHERE id = ?`, req.BlockID).Scan(&artistKey, &titleKey)
	} else {
		err = s.db.QueryRowContext(ctx, `SELECT artist_key, title_key FROM work_queue WHERE id = ?`, req.WorkItemID).Scan(&artistKey, &titleKey)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return UnblockResult{}, ErrNotFound
	}
	if err != nil {
		return UnblockResult{}, fmt.Errorf("lyricblock: unblock lookup: %w", err)
	}
	var res UnblockResult
	err = dbpkg.RetryOnBusy(ctx, busyAttempts, func() error {
		res = UnblockResult{}
		return s.unblockTx(ctx, req, artistKey, titleKey, &res)
	})
	if err != nil {
		return UnblockResult{}, err
	}
	s.log.Info("lyricblock: unblocked", "block_id", req.BlockID, "work_item_id", req.WorkItemID, "removed", res.Removed, "reopened", res.Reopened)
	return res, nil
}

func (s *Service) unblockTx(ctx context.Context, req UnblockRequest, artistKey, titleKey string, res *UnblockResult) error {
	// List before the transaction: the pool has one connection.
	ids := []int64{req.BlockID}
	if req.BlockID == 0 {
		blocks, lerr := s.store.List(ctx, ListFilter{ArtistKey: artistKey, TitleKey: titleKey})
		if lerr != nil {
			return lerr
		}
		ids = ids[:0]
		for _, b := range blocks {
			ids = append(ids, b.ID)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lyricblock: begin unblock: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		ok, rerr := s.store.Remove(ctx, tx, id)
		if rerr != nil {
			return rerr
		}
		if ok {
			res.Removed++
		}
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM work_queue WHERE artist_key = ? AND title_key = ? AND status = 'done' AND outcome_type = ?`,
		artistKey, titleKey, settledBlocked)
	if err != nil {
		return fmt.Errorf("lyricblock: find blocked rows: %w", err)
	}
	var wq []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("lyricblock: scan blocked row: %w", err)
		}
		wq = append(wq, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return fmt.Errorf("lyricblock: blocked rows: %w", err)
	}
	for _, id := range wq {
		ok, rerr := queue.ReopenDoneRowTx(ctx, tx, id, time.Now())
		if rerr != nil {
			return rerr
		}
		if ok {
			res.Reopened++
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lyricblock: commit unblock: %w", err)
	}
	return nil
}
