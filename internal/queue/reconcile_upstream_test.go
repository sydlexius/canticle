package queue

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// TestSetUpstreamIfPending_RaceGuard: a row that changed between listing and
// apply (re-laned, re-opened, already filled) is never written, and no backup
// record is requested for it; the lane is never rewritten.
func TestSetUpstreamIfPending_RaceGuard(t *testing.T) {
	ctx := context.Background()
	q, sqlDB := upgradeQueue(t)
	seed := func(key, status, lane string, upstream any) UpstreamCandidate {
		var id int64
		if err := sqlDB.QueryRowContext(ctx,
			`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, status, provider_lane, upstream)
             VALUES ('A', ?, ?, ?, '/x/'||?||'.flac', ?, ?, ?) RETURNING id`, key, key, key, key, status, lane, upstream).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return UpstreamCandidate{ID: id, Status: "done", Lane: "innertube"}
	}
	for _, tc := range []struct {
		name     string
		c        UpstreamCandidate
		want     bool
		wantUp   string
		wantLane string
	}{
		{"agrees", seed("ok", "done", "innertube", nil), true, "lyricfind", "innertube"},
		{"relaned", seed("lane", "done", "musixmatch", nil), false, "", "musixmatch"},
		{"reopened", seed("open", "processing", "innertube", nil), false, "", "innertube"},
		{"filled", seed("full", "done", "innertube", "musixmatch"), false, "musixmatch", "innertube"},
	} {
		called := false
		got, err := q.SetUpstreamIfPending(ctx, tc.c, "lyricfind", func() error { called = true; return nil })
		if err != nil || got != tc.want || called != tc.want {
			t.Errorf("%s: got=%v called=%v err=%v; want %v", tc.name, got, called, err, tc.want)
		}
		var up sql.NullString
		var lane string
		if err := sqlDB.QueryRowContext(ctx, `SELECT upstream, provider_lane FROM work_queue WHERE id = ?`, tc.c.ID).Scan(&up, &lane); err != nil {
			t.Fatal(err)
		}
		if up.String != tc.wantUp || lane != tc.wantLane {
			t.Errorf("%s: upstream=%q lane=%q; want %q %q", tc.name, up.String, lane, tc.wantUp, tc.wantLane)
		}
	}
	// A failing backup rolls the write back.
	c := seed("bk", "done", "innertube", nil)
	if ok, err := q.SetUpstreamIfPending(ctx, c, "lyricfind", func() error { return errors.New("disk full") }); ok || err == nil {
		t.Errorf("backup failure: ok=%v err=%v; want false + error", ok, err)
	}
	var up sql.NullString
	_ = sqlDB.QueryRowContext(ctx, `SELECT upstream FROM work_queue WHERE id = ?`, c.ID).Scan(&up)
	if up.Valid {
		t.Errorf("backup failure left upstream %q", up.String)
	}
}
