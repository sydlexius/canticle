package lyricblock

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
)

func newStore(t *testing.T) (*Store, *sql.DB, *bytes.Buffer) {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	var buf bytes.Buffer
	return NewStore(sqlDB, slog.New(slog.NewTextHandler(&buf, nil))), sqlDB, &buf
}

func addWQ(t *testing.T, d *sql.DB, artist, title string) int64 {
	t.Helper()
	var id int64
	if err := d.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key) VALUES (?, ?, ?, ?) RETURNING id`,
		artist, title, strings.ToLower(artist), strings.ToLower(title)).Scan(&id); err != nil {
		t.Fatalf("insert work_queue: %v", err)
	}
	return id
}

func TestStore_AddListRemoveBlocked(t *testing.T) {
	ctx := context.Background()
	s, d, _ := newStore(t)

	ok, err := s.Add(ctx, d, Block{ArtistKey: " Vexa Dunn ", TitleKey: "Quill Moor", Fingerprint: "fp1", WorkQueueID: 7, Lane: "lane-a", Upstream: "up"})
	if err != nil || !ok {
		t.Fatalf("Add = %v, %v; want true, nil", ok, err)
	}
	// Same identity and fingerprint again: unique constraint makes it a no-op.
	if ok, err := s.Add(ctx, d, Block{ArtistKey: "vexa dunn", TitleKey: "quill moor", Fingerprint: "fp1"}); err != nil || ok {
		t.Fatalf("duplicate Add = %v, %v; want false, nil", ok, err)
	}
	var n int
	_ = d.QueryRow(`SELECT COUNT(*) FROM lyric_blocks`).Scan(&n)
	if n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	if _, err := s.Add(ctx, d, Block{ArtistKey: "a", TitleKey: "t"}); err != ErrNoFingerprint {
		t.Fatalf("empty fingerprint err = %v, want ErrNoFingerprint", err)
	}

	if !s.Blocked(ctx, "VEXA DUNN", "quill moor", "fp1") {
		t.Error("Blocked(match) = false")
	}
	for name, args := range map[string][3]string{
		"other fingerprint": {"vexa dunn", "quill moor", "fp2"},
		"other title":       {"vexa dunn", "other", "fp1"},
		"empty fingerprint": {"vexa dunn", "quill moor", ""},
	} {
		if s.Blocked(ctx, args[0], args[1], args[2]) {
			t.Errorf("Blocked(%s) = true", name)
		}
	}

	got, err := s.List(ctx, ListFilter{WorkItemID: 7})
	if err != nil || len(got) != 1 {
		t.Fatalf("List by id = %v, %v", got, err)
	}
	b := got[0]
	if b.ArtistKey != "vexa dunn" || b.Lane != "lane-a" || b.Upstream != "up" || b.WorkQueueID != 7 || !strings.HasSuffix(b.CreatedAt, "Z") {
		t.Errorf("block = %+v", b)
	}
	if got, _ := s.List(ctx, ListFilter{ArtistKey: "Vexa Dunn", TitleKey: "Quill Moor"}); len(got) != 1 {
		t.Errorf("List by identity = %d, want 1", len(got))
	}
	if got, _ := s.List(ctx, ListFilter{WorkItemID: 8}); len(got) != 0 {
		t.Errorf("List other id = %d, want 0", len(got))
	}

	if ok, err := s.Remove(ctx, d, b.ID); err != nil || !ok {
		t.Fatalf("Remove = %v, %v", ok, err)
	}
	if ok, _ := s.Remove(ctx, d, b.ID); ok {
		t.Error("second Remove = true")
	}
	if s.Blocked(ctx, "vexa dunn", "quill moor", "fp1") {
		t.Error("Blocked after Remove = true")
	}
}

func TestStore_ListOrphans(t *testing.T) {
	ctx := context.Background()
	s, d, _ := newStore(t)
	addWQ(t, d, "Live Artist", "Live Title")
	for _, id := range [][2]string{{"live artist", "live title"}, {"gone artist", "gone title"}} {
		if _, err := s.Add(ctx, d, Block{ArtistKey: id[0], TitleKey: id[1], Fingerprint: "fp"}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.List(ctx, ListFilter{Orphans: true})
	if err != nil || len(got) != 1 || got[0].ArtistKey != "gone artist" {
		t.Fatalf("orphans = %+v, %v; want only the identity with no work_queue row", got, err)
	}
}

func TestStore_Rekey(t *testing.T) {
	ctx := context.Background()
	s, d, _ := newStore(t)
	add := func(a, ti, fp string) {
		t.Helper()
		if _, err := s.Add(ctx, d, Block{ArtistKey: a, TitleKey: ti, Fingerprint: fp}); err != nil {
			t.Fatal(err)
		}
	}
	add("old a", "t", "fp1")
	add("old a", "t", "fp2")
	add("new a", "t", "fp2") // destination already holds fp2: collision
	add("untouched", "t", "fp1")

	moved, err := s.Rekey(ctx, d, "old a", "t", "new a", "t")
	if err != nil {
		t.Fatalf("Rekey: %v", err)
	}
	if moved != 1 {
		t.Errorf("moved = %d, want 1 (fp2 collided)", moved)
	}
	if !s.Blocked(ctx, "new a", "t", "fp1") || !s.Blocked(ctx, "new a", "t", "fp2") {
		t.Error("destination lost a fingerprint")
	}
	if got, _ := s.List(ctx, ListFilter{ArtistKey: "old a", TitleKey: "t"}); len(got) != 0 {
		t.Errorf("old identity still holds %d blocks", len(got))
	}
	if !s.Blocked(ctx, "untouched", "t", "fp1") {
		t.Error("unrelated identity was touched")
	}
	if n, err := s.Rekey(ctx, d, "new a", "t", "new a", "t"); err != nil || n != 0 {
		t.Errorf("same-identity Rekey = %d, %v; want 0, nil", n, err)
	}
	if !s.Blocked(ctx, "new a", "t", "fp1") {
		t.Error("same-identity Rekey dropped blocks")
	}
}

func TestStore_Rekey_RollsBackWithCallerTx(t *testing.T) {
	ctx := context.Background()
	s, d, _ := newStore(t)
	if _, err := s.Add(ctx, d, Block{ArtistKey: "a", TitleKey: "t", Fingerprint: "fp"}); err != nil {
		t.Fatal(err)
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rekey(ctx, tx, "a", "t", "b", "t"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if !s.Blocked(ctx, "a", "t", "fp") {
		t.Error("rolled-back Rekey was visible")
	}
}

func TestStore_CountFor(t *testing.T) {
	ctx := context.Background()
	s, d, _ := newStore(t)
	a := addWQ(t, d, "Count A", "T")
	b := addWQ(t, d, "Count B", "T")
	for _, fp := range []string{"fp1", "fp2"} {
		if _, err := s.Add(ctx, d, Block{ArtistKey: "count a", TitleKey: "t", Fingerprint: fp}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.CountFor(ctx, []int64{a, b, 9999})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[a] != 2 {
		t.Errorf("CountFor = %v, want {%d: 2}", got, a)
	}
	if got, err := s.CountFor(ctx, nil); err != nil || len(got) != 0 {
		t.Errorf("CountFor(nil) = %v, %v", got, err)
	}
}

func TestStore_BlockedFailsOpenAndLogs(t *testing.T) {
	s, d, buf := newStore(t)
	if _, err := s.Add(context.Background(), d, Block{ArtistKey: "a", TitleKey: "t", Fingerprint: "fp"}); err != nil {
		t.Fatal(err)
	}
	_ = d.Close() // every later read errors
	if s.Blocked(context.Background(), "a", "t", "fp") {
		t.Error("Blocked on a read error = true; must fail open")
	}
	if out := buf.String(); !strings.Contains(out, "failing open") || !strings.Contains(out, "level=ERROR") {
		t.Errorf("read error not logged, got %q", out)
	}
}
