package lyricblock

import (
	"errors"
	"testing"
)

func TestUnblock(t *testing.T) {
	f := newFx(t)
	f.write(t, "song.lrc", lrcBody)
	if _, err := f.svc.Mark(f.ctx, f.req(nil)); err != nil {
		t.Fatal(err)
	}
	// Slice 3a (#1395) settles such a row; seed that state.
	f.mustExec(t, `UPDATE work_queue SET status = 'done', outcome_type = 'blocked'`)
	if _, err := f.svc.Unblock(f.ctx, UnblockRequest{}); !errors.Is(err, ErrUnblockTarget) {
		t.Errorf("empty request err = %v", err)
	}
	if _, err := f.svc.Unblock(f.ctx, UnblockRequest{BlockID: 999}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown block err = %v", err)
	}
	res, err := f.svc.Unblock(f.ctx, UnblockRequest{WorkItemID: f.id})
	if err != nil || res.Removed != 1 || res.Reopened != 1 {
		t.Fatalf("Unblock = %+v, %v", res, err)
	}
	if f.count(t, `SELECT COUNT(*) FROM lyric_blocks`) != 0 || f.count(t, `SELECT COUNT(*) FROM work_queue WHERE status = 'pending' AND outcome_type IS NULL`) != 1 {
		t.Error("block not removed or row not reopened")
	}
}

// A block marked on a row whose album artist differs from its track artist is
// removed by the same work-item request: both use the row's keys.
func TestUnblockRemovesWhatMarkStoredForADifferingAlbumArtist(t *testing.T) {
	f := newFx(t)
	f.mustExec(t, `UPDATE work_queue SET album_artist = 'Ostrel Fenn'`)
	f.write(t, "song.lrc", lrcBody)
	if _, err := f.svc.Mark(f.ctx, f.req(nil)); err != nil {
		t.Fatal(err)
	}
	if res, err := f.svc.Unblock(f.ctx, UnblockRequest{WorkItemID: f.id}); err != nil || res.Removed != 1 {
		t.Fatalf("Unblock = %+v, %v", res, err)
	}
}
