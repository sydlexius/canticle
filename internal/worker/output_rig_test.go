package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/prune"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scanner"
)

// healRig is a worker over REAL SQLite, a REAL writer and two library roots
// (music, musicB) in a temp tree, holding one pending row in music whose source
// audio exists (#1430).
type healRig struct {
	db         *sql.DB
	q          *queue.DBQueue
	w          *Worker
	fetcher    *fakeFetcher
	healer     *countingHealer
	id         int64
	root       string // library "music"
	rootB      string // library "musicB", populated
	libA, libB int64
	srcDir     string // the first row's source directory, which exists
}

// countingHealer counts RootOnline and LibraryRoots calls through the real Pruner.
type countingHealer struct {
	*prune.Pruner
	online    map[string]int
	rootCalls int
}

func (c *countingHealer) RootOnline(root string) bool {
	c.online[root]++
	return c.Pruner.RootOnline(root)
}

func (c *countingHealer) LibraryRoots(ctx context.Context) ([]string, error) {
	c.rootCalls++
	return c.Pruner.LibraryRoots(ctx)
}

func newHealRig(t *testing.T, paths func(srcDir, root, rootB string) []models.OutputPath) *healRig {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	r := &healRig{root: filepath.Join(base, "music"), rootB: filepath.Join(base, "musicB")}
	sqlDB, err := db.Open(ctx, filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	r.db = sqlDB
	r.srcDir = filepath.Join(r.root, "artist", "New")
	src := mkSource(t, r.srcDir)
	mkSource(t, filepath.Join(r.rootB, "artist", "Other"))
	for i, root := range []string{r.root, r.rootB} {
		lib, err := library.New(sqlDB).Add(ctx, root, filepath.Base(root), models.LibrarySettings{})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			r.libA = lib.ID
		} else {
			r.libB = lib.ID
		}
	}
	r.q = queue.NewDBQueue(sqlDB)
	r.q.SetRandomized(false)
	r.id = r.enqueue(t, "One", r.srcDir, src, paths(r.srcDir, r.root, r.rootB))
	r.fetcher = &fakeFetcher{song: fallthroughSong(90, "line")}
	r.w = New(r.q, cache.New(sqlDB), r.fetcher, lyrics.NewLRCWriter(r.root, r.rootB))
	r.w.SetMetadataReader((&fakeMetadataReader{meta: scanner.AudioMetadata{TrackLength: fallthroughFileSeconds}}).read)
	r.healer = &countingHealer{Pruner: prune.New(sqlDB), online: map[string]int{}}
	r.w.SetOutputHealer(r.healer)
	return r
}

func mkSource(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "01.flac")
	if err := os.WriteFile(src, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	return src
}

func (r *healRig) enqueue(t *testing.T, title, dir, src string, paths []models.OutputPath) int64 {
	t.Helper()
	item, err := r.q.Enqueue(context.Background(), models.Inputs{
		Track:       models.Track{ArtistName: "Synthetic Artist", TrackName: "Synthetic " + title},
		Outdir:      dir,
		Filename:    "01.lrc",
		SourcePath:  src,
		OutputPaths: paths,
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return item.ID
}

func (r *healRig) rowOf(t *testing.T, id int64) (status string, attempts int, paths []models.OutputPath, next string) {
	t.Helper()
	var raw string
	if err := r.db.QueryRow(`SELECT status, attempts, output_paths, next_attempt_at FROM work_queue WHERE id = ?`, id).Scan(&status, &attempts, &raw, &next); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &paths); err != nil {
		t.Fatal(err)
	}
	return status, attempts, paths, next
}

func (r *healRig) row(t *testing.T) (string, int, []models.OutputPath) {
	t.Helper()
	s, a, p, _ := r.rowOf(t, r.id)
	return s, a, p
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// takeOffline leaves root as an unmounted share does: present but empty.
func takeOffline(t *testing.T, root string) {
	t.Helper()
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
}
