package prune

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
)

// symlinkedLibrary registers a library whose configured root is a symlink to
// real, as a mounted share often is. Webhook rows carry the resolved spelling
// (real/...) while the root is stored as configured (link/...), so a heal or a
// stale check must match a path under either spelling (#1430). The temp base is
// resolved first so a platform alias (macOS /var -> /private/var) cannot decide
// the result.
func symlinkedLibrary(t *testing.T) (ctx context.Context, sqlDB *sql.DB, libID int64, link, real string) {
	t.Helper()
	ctx = context.Background()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real = filepath.Join(base, "real")
	link = filepath.Join(base, "link")
	if err := os.MkdirAll(filepath.Join(real, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	sqlDB, err = db.Open(ctx, filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	lib, err := library.New(sqlDB).Add(ctx, link, "lib", models.LibrarySettings{})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, sqlDB, lib.ID, link, real
}

// A webhook row's source is in the resolved spelling; its stale entry may be in
// either. Both heal to the row's own directory instead of reading as foreign.
func TestHealOutputPaths_SymlinkedRootEitherSpelling(t *testing.T) {
	for name, spell := range map[string]func(link, real string) string{
		"entry-in-configured-spelling": func(link, _ string) string { return link },
		"entry-in-resolved-spelling":   func(_, real string) string { return real },
	} {
		t.Run(name, func(t *testing.T) {
			ctx, sqlDB, libID, link, real := symlinkedLibrary(t)
			src := filepath.Join(real, "amb", "New", "01.flac")
			paths := []models.OutputPath{{Outdir: filepath.Join(spell(link, real), "amb", "Old"), Filename: "01.flac"}}
			id := seedRowWithOutputPaths(t, ctx, sqlDB, libID, src, "amb", paths)
			execWQ(t, ctx, sqlDB, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, id)
			got, healed, err := New(sqlDB).HealOutputPaths(ctx, id, src, filepath.Dir(src), "01.flac", paths)
			if err != nil || !healed || len(got) != 1 || got[0].Outdir != filepath.Dir(src) {
				t.Fatalf("heal = %+v healed=%v err=%v, want the row's own directory", got, healed, err)
			}
		})
	}
}

// secondSymlinkedLibrary registers a second library whose root is also a symlink
// (linkB -> realB), beside symlinkedLibrary's.
func secondSymlinkedLibrary(t *testing.T, ctx context.Context, sqlDB *sql.DB, link string) (realB string) {
	t.Helper()
	base := filepath.Dir(link)
	realB = filepath.Join(base, "realB")
	linkB := filepath.Join(base, "linkB")
	if err := os.MkdirAll(realB, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realB, linkB); err != nil {
		t.Fatal(err)
	}
	if _, err := library.New(sqlDB).Add(ctx, linkB, "libB", models.LibrarySettings{}); err != nil {
		t.Fatal(err)
	}
	return realB
}

// With both roots symlinked and the source in the resolved spelling, the ALIAS is
// the only thing that places source and entry in a root at all. An entry in the
// same library's resolved spelling heals (only the alias makes the two share a
// root); an entry in the other library's resolved spelling is a second copy there
// and stays refused (only the root-equality check keeps it so).
func TestHealOutputPaths_SymlinkedRootAliasAndCrossLibrary(t *testing.T) {
	for name, tc := range map[string]struct {
		sameLibrary bool
		want        bool
	}{
		"same-library-resolved-entry-heals":       {true, true},
		"other-library-resolved-entry-is-refused": {false, false},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, sqlDB, libID, link, real := symlinkedLibrary(t)
			realB := secondSymlinkedLibrary(t, ctx, sqlDB, link)
			entryRoot := realB
			if tc.sameLibrary {
				entryRoot = real
			}
			src := filepath.Join(real, "amb", "New", "01.flac")
			paths := []models.OutputPath{{Outdir: filepath.Join(entryRoot, "amb", "Old"), Filename: "01.flac"}}
			id := seedRowWithOutputPaths(t, ctx, sqlDB, libID, src, "amb", paths)
			execWQ(t, ctx, sqlDB, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, id)
			_, healed, err := New(sqlDB).HealOutputPaths(ctx, id, src, filepath.Dir(src), "01.flac", paths)
			if err != nil || healed != tc.want {
				t.Fatalf("healed=%v err=%v, want %v", healed, err, tc.want)
			}
		})
	}
}

// resolvedBase returns a temp base with platform aliases resolved, so a test's
// symlinks are the only aliasing in play.
func resolvedBase(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return base
}

// Two configured roots sharing one target (a -> b, d -> b). A path in either
// configured spelling, or the resolved one, names the same place, so a linked
// real file under any spelling of the entry's directory means a real second copy
// and nothing is proven stale. Before the fix the resolved spelling was owned by
// the LAST root only, so an `a`-spelled directory never compared equal to the
// `d`-spelled or resolved file.
func TestProvablyStale_TwoRootsOneTarget(t *testing.T) {
	base := resolvedBase(t)
	b := filepath.Join(base, "b")
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	a, d := filepath.Join(base, "a"), filepath.Join(base, "d")
	for _, l := range []string{a, d} {
		if err := os.Symlink(b, l); err != nil {
			t.Fatal(err)
		}
	}
	online := func(string) bool { return true }
	roots := newRootSet([]string{a, d})
	dir := filepath.Join(a, "amb", "Old") // missing entry directory
	for name, file := range map[string]string{
		"resolved-spelling":    filepath.Join(b, "amb", "Old", "01.flac"),
		"first-root-spelling":  filepath.Join(a, "amb", "Old", "01.flac"),
		"second-root-spelling": filepath.Join(d, "amb", "Old", "01.flac"),
	} {
		t.Run(name, func(t *testing.T) {
			if provablyStale(roots, online, []string{file}, 1, dir) {
				t.Errorf("provablyStale = true with a linked file inside the directory, want false")
			}
		})
	}
	// Nothing inside, anywhere: a genuinely stale entry in a configured spelling
	// is still proven stale.
	if !provablyStale(roots, online, []string{filepath.Join(a, "amb", "New", "01.flac")}, 1, dir) {
		t.Errorf("provablyStale = false with no file inside, want true")
	}
}

// A root symlink whose target is gone does not resolve, while the worker's cached
// online answer may still say true. A real file recorded under the resolved
// spelling is then unrecognizable as inside the entry's directory, so nothing
// under that root is proven stale, and the heal classifier reads a resolved
// spelling entry as a foreign root (counted skipped, no write).
func TestSymlinkRootUnresolved_NothingProvenAndNoHeal(t *testing.T) {
	base := resolvedBase(t)
	real, link := filepath.Join(base, "real"), filepath.Join(base, "link")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(real); err != nil { // the mount drops
		t.Fatal(err)
	}
	roots := newRootSet([]string{link})
	if !roots.unresolved[link] {
		t.Errorf("link root should be recorded unresolved")
	}
	online := func(string) bool { return true } // stale cached answer
	dir := filepath.Join(link, "amb", "Old")
	file := filepath.Join(real, "amb", "Old", "01.flac")
	if provablyStale(roots, online, []string{file}, 1, dir) {
		t.Errorf("provablyStale = true under an unresolved root, want false")
	}

	src := filepath.Join(real, "amb", "New", "01.flac")
	e := models.OutputPath{Outdir: filepath.Join(real, "amb", "Old"), Filename: "01.flac"}
	if _, v := classifyEntry(roots, filepath.Dir(src), "01.flac", src, e); v != verdictForeignRoot {
		t.Errorf("classifyEntry verdict = %v, want verdictForeignRoot", v)
	}
}

// Stale needs the root online and no tracked file in the entry. Under a
// symlinked root each condition must hold across spellings: an entry in the
// resolved spelling is proven stale when nothing is there, and a linked file in
// the OTHER spelling is a real second copy, never to be settled past.
func TestEntryProvablyStale_SymlinkedRootAcrossSpellings(t *testing.T) {
	online := func(string) bool { return true }
	cases := []struct {
		name     string
		entryIn  func(link, real string) string
		linkedIn func(link, real string) string // "" = no linked file
		want     bool
	}{
		{"resolved-entry-nothing-inside", func(_, r string) string { return r }, nil, true},
		{"configured-entry-linked-file-resolved", func(l, _ string) string { return l },
			func(_, r string) string { return r }, false},
		{"resolved-entry-linked-file-configured", func(_, r string) string { return r },
			func(l, _ string) string { return l }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, sqlDB, libID, link, real := symlinkedLibrary(t)
			src := filepath.Join(real, "amb", "New", "01.flac")
			id := seedRowWithOutputPaths(t, ctx, sqlDB, libID, src, "amb", []models.OutputPath{{Outdir: filepath.Dir(src), Filename: "01.flac"}})
			// Every case has a link row so the "no evidence" clause is not what decides.
			linkScanResult(t, ctx, sqlDB, libID, id, filepath.Join(real, "amb", "Elsewhere", "02.flac"))
			e := models.OutputPath{Outdir: filepath.Join(tc.entryIn(link, real), "amb", "Old"), Filename: "01.flac"}
			if tc.linkedIn != nil {
				linkScanResult(t, ctx, sqlDB, libID, id, filepath.Join(tc.linkedIn(link, real), "amb", "Old", "01.flac"))
			}
			got, err := New(sqlDB).EntryProvablyStale(ctx, id, src, e, online)
			if err != nil || got != tc.want {
				t.Errorf("EntryProvablyStale = %v err=%v, want %v", got, err, tc.want)
			}
		})
	}
}
