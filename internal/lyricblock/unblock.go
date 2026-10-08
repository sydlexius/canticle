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
	var res UnblockResult
	err := dbpkg.RetryOnBusy(ctx, busyAttempts, func() error {
		res = UnblockResult{}
		return s.unblockTx(ctx, req, &res)
	})
	if err != nil {
		return UnblockResult{}, err
	}
	s.log.Info("lyricblock: unblocked", "block_id", req.BlockID, "work_item_id", req.WorkItemID, "removed", res.Removed, "reopened", res.Reopened)
	return res, nil
}

// unblockTx runs the whole unblock in one transaction: the target's identity is
// read, its blocks are deleted by exact key equality (empty keys included, so a
// row with an empty key never sweeps another identity's blocks), and the
// blocked row is reopened. A block added concurrently is therefore either
// deleted here or committed after, never left behind a reopened row.
func (s *Service) unblockTx(ctx context.Context, req UnblockRequest, res *UnblockResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("lyricblock: begin unblock: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var artistKey, titleKey string
	if req.BlockID != 0 {
		err = tx.QueryRowContext(ctx, `SELECT artist_key, title_key FROM lyric_blocks WHERE id = ?`, req.BlockID).Scan(&artistKey, &titleKey)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT artist_key, title_key FROM work_queue WHERE id = ?`, req.WorkItemID).Scan(&artistKey, &titleKey)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lyricblock: unblock lookup: %w", err)
	}
	if req.BlockID != 0 {
		ok, rerr := s.store.Remove(ctx, tx, req.BlockID)
		if rerr != nil {
			return rerr
		}
		if ok {
			res.Removed++
		}
	} else {
		n, derr := s.store.DeleteByIdentityTx(ctx, tx, artistKey, titleKey)
		if derr != nil {
			return derr
		}
		res.Removed = n
	}
	// work_queue is unique on (artist_key, title_key), so at most one row matches.
	ok, err := queue.ReopenBlockedTx(ctx, tx, artistKey, titleKey, time.Now())
	if err != nil {
		return err
	}
	if ok {
		res.Reopened++
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lyricblock: commit unblock: %w", err)
	}
	return nil
}
