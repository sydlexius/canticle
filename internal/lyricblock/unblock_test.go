package lyricblock

import (
	"errors"
	"testing"

	"github.com/sydlexius/canticle/internal/queue"
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

func (f *fx) addBlock(t *testing.T, artistKey, fp string) int64 {
	t.Helper()
	if _, err := f.svc.store.Add(f.ctx, f.db, Block{ArtistKey: artistKey, TitleKey: "quill moor", Fingerprint: fp}); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := f.db.QueryRow(`SELECT id FROM lyric_blocks WHERE fingerprint = ?`, fp).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUnblockByBlockIDReopensTheBlockedRow(t *testing.T) {
	f := newFx(t)
	f.mustExec(t, `UPDATE work_queue SET outcome_type = 'blocked'`)
	keep := f.addBlock(t, "vexa dunn", "fp-keep")
	drop := f.addBlock(t, "vexa dunn", "fp-drop")
	res, err := f.svc.Unblock(f.ctx, UnblockRequest{BlockID: drop})
	if err != nil || res.Removed != 1 || res.Reopened != 1 {
		t.Fatalf("Unblock = %+v, %v", res, err)
	}
	if f.count(t, `SELECT COUNT(*) FROM lyric_blocks WHERE id = ?`, keep) != 1 || f.count(t, `SELECT COUNT(*) FROM lyric_blocks WHERE id = ?`, drop) != 0 {
		t.Error("only the named block should be removed")
	}
	if f.status(t) != "pending" {
		t.Errorf("status = %q, want pending", f.status(t))
	}
}

func TestUnblockRejectsBothTargetsAndUnknownWorkItem(t *testing.T) {
	f := newFx(t)
	if _, err := f.svc.Unblock(f.ctx, UnblockRequest{BlockID: 1, WorkItemID: f.id}); !errors.Is(err, ErrUnblockTarget) {
		t.Errorf("both targets err = %v", err)
	}
	if _, err := f.svc.Unblock(f.ctx, UnblockRequest{WorkItemID: f.id + 999}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown work item err = %v", err)
	}
}

func TestUnblockLeavesANonBlockedDoneRowSettled(t *testing.T) {
	f := newFx(t)
	f.addBlock(t, "vexa dunn", "fp-a")
	res, err := f.svc.Unblock(f.ctx, UnblockRequest{WorkItemID: f.id})
	if err != nil || res.Removed != 1 || res.Reopened != 0 {
		t.Fatalf("Unblock = %+v, %v", res, err)
	}
	if f.status(t) != "done" {
		t.Errorf("status = %q, want done", f.status(t))
	}
}

// An empty key on the work item is a value to match, not a wildcard: it must
// not delete another artist's block that shares the title.
func TestUnblockEmptyArtistKeyDoesNotSweepOtherArtists(t *testing.T) {
	f := newFx(t)
	var emptyID int64
	if err := f.db.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, outdir, filename, status, outcome_type)
		VALUES ('', 'Quill Moor', '', 'quill moor', ?, 'other.flac', 'done', 'synced') RETURNING id`, f.dir).Scan(&emptyID); err != nil {
		t.Fatal(err)
	}
	other := f.addBlock(t, "vexa dunn", "fp-other")
	own := f.addBlock(t, "", "fp-own")
	res, err := f.svc.Unblock(f.ctx, UnblockRequest{WorkItemID: emptyID})
	if err != nil || res.Removed != 1 {
		t.Fatalf("Unblock = %+v, %v", res, err)
	}
	if f.count(t, `SELECT COUNT(*) FROM lyric_blocks WHERE id = ?`, other) != 1 {
		t.Error("another artist's block was removed")
	}
	if f.count(t, `SELECT COUNT(*) FROM lyric_blocks WHERE id = ?`, own) != 0 {
		t.Error("the empty-key block was not removed")
	}
}

// The race the review found, driven in the racing order by hand: the worker holds
// the row in processing and has observed the block; the unblock commits first;
// the worker's settle then runs. It must NOT leave a done/blocked row with no
// block (nothing could reopen it by block id), and a work-item unblock with no
// blocks left still reopens a row stranded that way.
func TestUnblockRacingAWorkerSettleNeverStrandsABlockedRow(t *testing.T) {
	f := newFx(t)
	f.write(t, "song.lrc", lrcBody)
	if _, err := f.svc.Mark(f.ctx, f.req(nil)); err != nil {
		t.Fatal(err)
	}
	f.mustExec(t, `UPDATE work_queue SET status = 'processing', outcome_type = NULL`)
	if res, err := f.svc.Unblock(f.ctx, UnblockRequest{WorkItemID: f.id}); err != nil || res.Removed != 1 || res.Reopened != 0 {
		t.Fatalf("Unblock = %+v, %v; want one block removed, no row to reopen", res, err)
	}
	outcome, err := queue.NewDBQueue(f.db).SettleBlocked(f.ctx, f.id)
	if err != nil || outcome == queue.Settled {
		t.Fatalf("SettleBlocked after the unblock = (%v, %v); want a refusal", outcome, err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM work_queue WHERE outcome_type = 'blocked'`); n != 0 {
		t.Fatalf("%d row(s) settled blocked with no block left", n)
	}

	// A row stranded before this fix (done/blocked, no block): the work-item
	// unblock removes 0 blocks and still reopens it.
	f.mustExec(t, `UPDATE work_queue SET status = 'done', outcome_type = 'blocked'`)
	res, err := f.svc.Unblock(f.ctx, UnblockRequest{WorkItemID: f.id})
	if err != nil || res.Removed != 0 || res.Reopened != 1 {
		t.Fatalf("recovery Unblock = %+v, %v; want 0 removed, 1 reopened", res, err)
	}
}

// A dry run changes nothing and its counts equal what the real run then
// returns, for both targets.
func TestUnblockDryRunCountsWhatTheRealRunReturns(t *testing.T) {
	for _, byBlock := range []bool{false, true} {
		f := newFx(t)
		f.mustExec(t, `UPDATE work_queue SET outcome_type = 'blocked'`)
		f.addBlock(t, "vexa dunn", "fp-keep")
		drop := f.addBlock(t, "vexa dunn", "fp-drop")
		req := UnblockRequest{WorkItemID: f.id}
		if byBlock {
			req = UnblockRequest{BlockID: drop}
		}
		dry := req
		dry.DryRun = true
		want, err := f.svc.Unblock(f.ctx, dry)
		if err != nil {
			t.Fatal(err)
		}
		if f.count(t, `SELECT COUNT(*) FROM lyric_blocks`) != 2 || f.status(t) != "done" {
			t.Fatalf("byBlock=%v: dry run changed state", byBlock)
		}
		got, err := f.svc.Unblock(f.ctx, req)
		if err != nil || got != want || got.Reopened != 1 {
			t.Errorf("byBlock=%v: dry %+v, real %+v, %v", byBlock, want, got, err)
		}
	}
	f := newFx(t)
	if _, err := f.svc.Unblock(f.ctx, UnblockRequest{BlockID: 999, DryRun: true}); !errors.Is(err, ErrNotFound) {
		t.Errorf("dry run unknown id err = %v", err)
	}
}

// An empty key is a value, not a wildcard, in the dry-run count too.
func TestUnblockDryRunEmptyArtistKeyDoesNotCountOtherArtists(t *testing.T) {
	f := newFx(t)
	var emptyID int64
	if err := f.db.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, outdir, filename, status, outcome_type)
		VALUES ('', 'Quill Moor', '', 'quill moor', ?, 'other.flac', 'done', 'synced') RETURNING id`, f.dir).Scan(&emptyID); err != nil {
		t.Fatal(err)
	}
	f.addBlock(t, "vexa dunn", "fp-other")
	res, err := f.svc.Unblock(f.ctx, UnblockRequest{WorkItemID: emptyID, DryRun: true})
	if err != nil || res.Removed != 0 {
		t.Fatalf("dry run = %+v, %v; want 0 removed", res, err)
	}
}
