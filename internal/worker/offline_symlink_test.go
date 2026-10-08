package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

// symlinkRoot builds <base>/real/ and <base>/music -> <base>/real. base is
// itself resolved first: t.TempDir lives under /var -> /private/var on macOS,
// and an unresolved base would make the configured spelling differ from the
// resolved one through that unrelated link too, which the test must not depend
// on. It returns the configured root and the resolved root.
func symlinkRoot(t *testing.T) (configured, resolved string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	resolved = filepath.Join(base, "real")
	if err := os.Mkdir(resolved, 0o755); err != nil {
		t.Fatal(err)
	}
	configured = filepath.Join(base, "music")
	if err := os.Symlink(resolved, configured); err != nil {
		t.Fatal(err)
	}
	return configured, resolved
}

// A webhook row carries the symlink-resolved path while the library root is the
// configured spelling; the guard must match the row under either (#1430).
func TestParkIfLibraryOffline_SymlinkedRoot(t *testing.T) {
	configured, resolved := symlinkRoot(t)
	for _, tc := range []struct {
		name   string
		src    string
		online bool
		parked bool
	}{
		{"resolved spelling, offline", filepath.Join(resolved, "a", "s.flac"), false, true},
		{"resolved spelling, online", filepath.Join(resolved, "a", "s.flac"), true, false},
		{"configured spelling, offline", filepath.Join(configured, "a", "s.flac"), false, true},
		{"configured spelling, online", filepath.Join(configured, "a", "s.flac"), true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHealer(map[string]bool{configured: tc.online}, configured)
			w, _ := clockedWorker(h)
			got, err := w.parkIfLibraryOffline(context.Background(), queue.WorkItem{ID: 1, Inputs: models.Inputs{SourcePath: tc.src}})
			if err != nil || got != tc.parked {
				t.Fatalf("parked = %v, err = %v; want parked = %v", got, err, tc.parked)
			}
			if h.probes[configured] != 1 || len(h.probes) != 1 {
				t.Errorf("probes = %v, want exactly one, on the configured root", h.probes)
			}
		})
	}
}

// When the root stops resolving (target gone with the mount), the spelling
// remembered from the last good resolve keeps matching resolved-path rows.
func TestParkIfLibraryOffline_AliasSurvivesFailedResolve(t *testing.T) {
	configured, resolved := symlinkRoot(t)
	h := newFakeHealer(map[string]bool{configured: false}, configured)
	w, clock := clockedWorker(h)
	item := queue.WorkItem{ID: 1, Inputs: models.Inputs{SourcePath: filepath.Join(resolved, "s.flac")}}
	if _, err := w.parkIfLibraryOffline(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(resolved); err != nil { // the symlink now dangles: EvalSymlinks fails
		t.Fatal(err)
	}
	*clock = clock.Add(2 * rootOnlineTTL) // force a root-list refresh
	got, err := w.parkIfLibraryOffline(context.Background(), item)
	if err != nil || !got {
		t.Fatalf("parked = %v, err = %v; want the row still parked after the resolve failed", got, err)
	}
}
