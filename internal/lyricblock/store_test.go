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

func addFP(t *testing.T, s *Store, d *sql.DB, artist, title, fp string) {
	t.Helper()
	if _, err := s.Add(context.Background(), d, Block{ArtistKey: artist, TitleKey: title, Fingerprint: fp}); err != nil {
		t.Fatal(err)
	}
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
	if _, err := s.Add(ctx, d, Block{ArtistKey: "a", TitleKey: "t"}); err != ErrNoFingerprint {
		t.Fatalf("empty fingerprint err = %v, want ErrNoFingerprint", err)
	}

	if !s.Blocked(ctx, "VEXA DUNN", "quill moor", "fp1") {
		t.Error("Blocked(match) = false")
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
	s, d, _ := newStore(t)
	addWQ(t, d, "Live Artist", "Live Title")
	addFP(t, s, d, "live artist", "live title", "fp")
	addFP(t, s, d, "gone artist", "gone title", "fp")
	got, err := s.List(context.Background(), ListFilter{Orphans: true})
	if err != nil || len(got) != 1 || got[0].ArtistKey != "gone artist" {
		t.Fatalf("orphans = %+v, %v; want only the identity with no work_queue row", got, err)
	}
}

func TestStore_Rekey(t *testing.T) {
	ctx := context.Background()
	s, d, _ := newStore(t)
	add := func(a, ti, fp string) { addFP(t, s, d, a, ti, fp) }
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
	if !s.AnyBlocked(ctx, "new a", "t", []string{"fp1"}) || !s.AnyBlocked(ctx, "new a", "t", []string{"fp2"}) || !s.Blocked(ctx, "untouched", "t", "fp1") {
		t.Error("destination lost a fingerprint or an unrelated identity was touched")
	}
	if got, _ := s.List(ctx, ListFilter{ArtistKey: "old a", TitleKey: "t"}); len(got) != 0 {
		t.Errorf("old identity still holds %d blocks", len(got))
	}
	if n, err := s.Rekey(ctx, d, "new a", "t", "new a", "t"); err != nil || n != 0 || !s.Blocked(ctx, "new a", "t", "fp1") {
		t.Errorf("same-identity Rekey = %d, %v; want 0, nil and no loss", n, err)
	}
}

func TestStore_Rekey_RollsBackWithCallerTx(t *testing.T) {
	ctx := context.Background()
	s, d, _ := newStore(t)
	addFP(t, s, d, "a", "t", "fp")
	tx, err := d.BeginTx(ctx, nil)
	if err == nil {
		_, err = s.Rekey(ctx, tx, "a", "t", "b", "t")
	}
	if err != nil || tx.Rollback() != nil {
		t.Fatalf("rekey in tx: %v", err)
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
	addFP(t, s, d, "count a", "t", "fp1")
	addFP(t, s, d, "count a", "t", "fp2")
	got, err := s.CountFor(ctx, []int64{a, b, 9999})
	if err != nil || len(got) != 1 || got[a] != 2 {
		t.Errorf("CountFor = %v, %v; want {%d: 2}", got, err, a)
	}
	if got, err := s.CountFor(ctx, nil); err != nil || len(got) != 0 {
		t.Errorf("CountFor(nil) = %v, %v", got, err)
	}
}

func TestStore_ListOrderIsInsertionOrder(t *testing.T) {
	s, d, _ := newStore(t)
	for _, fp := range []string{"zz", "aa", "mm"} {
		addFP(t, s, d, "a", "t", fp)
	}
	got, err := s.List(context.Background(), ListFilter{})
	if err != nil || len(got) != 3 || got[0].Fingerprint != "zz" || got[2].Fingerprint != "mm" {
		t.Fatalf("List = %+v, %v; want insertion order zz, aa, mm", got, err)
	}
}

func TestStore_AnyBlocked(t *testing.T) {
	ctx := context.Background()
	s, d, buf := newStore(t)
	addFP(t, s, d, "a", "t", "fp2")
	if !s.AnyBlocked(ctx, "A", "t", []string{"", "fp1", "fp2"}) {
		t.Error("AnyBlocked with a blocked member = false")
	}
	for name, fps := range map[string][]string{"none match": {"fp1", "fp3"}, "empties": {"", ""}, "nil": nil} {
		if s.AnyBlocked(ctx, "a", "t", fps) {
			t.Errorf("AnyBlocked(%s) = true", name)
		}
	}
	if s.AnyBlocked(ctx, "a", "other", []string{"fp2"}) {
		t.Error("AnyBlocked on another identity = true")
	}
	if buf.Len() != 0 {
		t.Errorf("a plain miss logged %q, want silence", buf.String())
	}
}

func TestStore_AnyBlockedFailsOpen(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tc := range map[string]struct {
		ctx   context.Context
		level string
		close bool
	}{"read error": {context.Background(), "level=ERROR", true}, "canceled context": {canceled, "level=WARN", false}} {
		s, d, buf := newStore(t)
		addFP(t, s, d, "a", "t", "fp")
		if tc.close {
			_ = d.Close() // every later read errors
		}
		if s.AnyBlocked(tc.ctx, "a", "t", []string{"fp"}) {
			t.Errorf("%s: blocked = true; must fail open", name)
		}
		if out := buf.String(); !strings.Contains(out, tc.level) || !strings.Contains(out, "failing open") || (tc.level == "level=WARN") == strings.Contains(out, "level=ERROR") {
			t.Errorf("%s: log = %q, want %s", name, out, tc.level)
		}
	}
}
