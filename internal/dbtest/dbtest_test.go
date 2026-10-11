package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
)

func queryRows(t *testing.T, sqlDB *sql.DB, q string) []string {
	t.Helper()
	rows, err := sqlDB.QueryContext(context.Background(), q)
	if err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, fmt.Sprintf("%v", vals))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func scalar(t *testing.T, sqlDB *sql.DB, q string) string {
	t.Helper()
	var s string
	if err := sqlDB.QueryRowContext(context.Background(), q).Scan(&s); err != nil {
		t.Fatalf("scalar %q: %v", q, err)
	}
	return s
}

// persistentDump captures the state stored in the database file itself:
// schema, goose history (without timestamps), file-level pragmas, and every
// row of every user table (many migrations seed data). Run it on a handle
// that never migrated or repaired anything (db.OpenReadOnly), so a defective
// template cannot be healed before it is compared.
func persistentDump(t *testing.T, sqlDB *sql.DB) map[string][]string {
	t.Helper()
	out := map[string][]string{
		"schema": queryRows(t, sqlDB, "SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY type, name"),
		"goose":  queryRows(t, sqlDB, "SELECT version_id, is_applied FROM goose_db_version ORDER BY id"),
	}
	for _, pragma := range []string{"journal_mode", "user_version", "page_size", "auto_vacuum", "application_id"} {
		out["pragma:"+pragma] = []string{scalar(t, sqlDB, "PRAGMA "+pragma)}
	}
	tables := queryRows(t, sqlDB, "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name <> 'goose_db_version' ORDER BY name")
	if len(queryRows(t, sqlDB, "SELECT name FROM sqlite_master WHERE name='sqlite_sequence'")) == 1 {
		tables = append(tables, "[sqlite_sequence]")
	}
	for _, tbl := range tables {
		name := strings.Trim(tbl, "[]")
		out["rows:"+name] = queryRows(t, sqlDB, fmt.Sprintf("SELECT * FROM %q ORDER BY 1", name))
	}
	return out
}

// connectionPragmas reads the pragmas db.Open sets per connection; they are
// not stored in the file, so they are read on a db.Open handle for both sides.
func connectionPragmas(t *testing.T, sqlDB *sql.DB) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, pragma := range []string{"foreign_keys", "busy_timeout", "synchronous"} {
		out[pragma] = scalar(t, sqlDB, "PRAGMA "+pragma)
	}
	return out
}

func readOnlyDump(t *testing.T, path string) map[string][]string {
	t.Helper()
	sqlDB, err := db.OpenReadOnly(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenReadOnly %s: %v", path, err)
	}
	defer func() { _ = sqlDB.Close() }()
	return persistentDump(t, sqlDB)
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

func TestTemplateEqualsFreshOpen(t *testing.T) {
	ctx := context.Background()
	freshPath := filepath.Join(t.TempDir(), "fresh.db")
	fresh, err := db.Open(ctx, freshPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatalf("close fresh: %v", err)
	}
	// The copy has never been opened read-write: nothing has re-run
	// migrations over it, so any defect in the template is still visible.
	copyPath := Path(t)

	want, got := readOnlyDump(t, freshPath), readOnlyDump(t, copyPath)
	if len(want) != len(got) {
		t.Fatalf("dump key count: fresh %d templated %d", len(want), len(got))
	}
	if got["pragma:journal_mode"][0] != "wal" {
		t.Errorf("journal_mode = %q, want wal", got["pragma:journal_mode"][0])
	}
	if len(got["schema"]) < 10 || len(got["goose"]) < 60 {
		t.Errorf("template looks unmigrated: %d schema rows, %d goose rows", len(got["schema"]), len(got["goose"]))
	}
	for k, w := range want {
		g := got[k]
		if strings.Join(w, "\n") != strings.Join(g, "\n") {
			t.Errorf("%s differs:\n fresh:     %v\n templated: %v", k, w, g)
		}
	}

	// Per-connection pragmas come from db.Open on both sides.
	freshConn, err := db.Open(ctx, freshPath)
	if err != nil {
		t.Fatalf("reopen fresh: %v", err)
	}
	t.Cleanup(func() { _ = freshConn.Close() })
	copyConn, err := OpenAt(ctx, t, copyPath)
	if err != nil {
		t.Fatalf("OpenAt copy: %v", err)
	}
	t.Cleanup(func() { _ = copyConn.Close() })
	wantConn, gotConn := connectionPragmas(t, freshConn), connectionPragmas(t, copyConn)
	if wantConn["foreign_keys"] != "1" {
		t.Errorf("fresh foreign_keys = %q, want 1 (comparison is meaningless otherwise)", wantConn["foreign_keys"])
	}
	for k, w := range wantConn {
		if gotConn[k] != w {
			t.Errorf("pragma %s: fresh %q templated %q", k, w, gotConn[k])
		}
	}

	if wm, gm := fileMode(t, freshPath), fileMode(t, copyPath); wm != gm {
		t.Errorf("file mode: fresh %v templated %v", wm, gm)
	}
}

func TestOpenIsolation(t *testing.T) {
	ctx := context.Background()
	a, b := Open(t), Open(t)
	if _, err := a.ExecContext(ctx, "INSERT INTO libraries (name, path) VALUES ('iso', '/iso')"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got := scalar(t, a, "SELECT COUNT(*) FROM libraries WHERE name='iso'"); got != "1" {
		t.Fatalf("writer copy count = %s, want 1", got)
	}
	if got := scalar(t, b, "SELECT COUNT(*) FROM libraries WHERE name='iso'"); got != "0" {
		t.Errorf("write leaked into another copy: count = %s", got)
	}
	c := Open(t)
	if got := scalar(t, c, "SELECT COUNT(*) FROM libraries WHERE name='iso'"); got != "0" {
		t.Errorf("write leaked into a later copy: count = %s", got)
	}
}

func TestOpenAtDoesNotOverwriteExisting(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sub", "x.db")
	first, err := OpenAt(ctx, t, path)
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	if _, err := first.ExecContext(ctx, "INSERT INTO libraries (name, path) VALUES ('keep', '/keep')"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	second, err := OpenAt(ctx, t, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if got := scalar(t, second, "SELECT COUNT(*) FROM libraries WHERE name='keep'"); got != "1" {
		t.Errorf("reopen lost data: count = %s, want 1", got)
	}
}

func TestPathIsMigratedAndUnopened(t *testing.T) {
	ctx := context.Background()
	path := Path(t)
	sqlDB, err := db.OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if got := scalar(t, sqlDB, "SELECT COUNT(*) FROM sqlite_master WHERE name='work_queue'"); got != "1" {
		t.Errorf("work_queue missing from Path copy")
	}
}

func TestPathIsolation(t *testing.T) {
	ctx := context.Background()
	p1, p2 := Path(t), Path(t)
	if p1 == p2 {
		t.Fatalf("Path returned the same file twice: %s", p1)
	}
	a, err := db.Open(ctx, p1)
	if err != nil {
		t.Fatalf("open p1: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, err := db.Open(ctx, p2)
	if err != nil {
		t.Fatalf("open p2: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	if _, err := a.ExecContext(ctx, "INSERT INTO libraries (name, path) VALUES ('iso', '/iso')"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got := scalar(t, a, "SELECT COUNT(*) FROM libraries WHERE name='iso'"); got != "1" {
		t.Fatalf("writer copy count = %s, want 1", got)
	}
	if got := scalar(t, b, "SELECT COUNT(*) FROM libraries WHERE name='iso'"); got != "0" {
		t.Errorf("write leaked into the other Path copy: count = %s", got)
	}
}

// errFatal is the sentinel fakeTB.Fatalf panics with, mirroring FailNow's
// stop-the-goroutine behavior without failing the real test.
type errFatal struct{}

// fakeTB records Fatalf instead of failing. Only the methods OpenAt uses are
// implemented; anything else panics on the nil embedded TB, which would flag
// OpenAt starting to use a new method.
type fakeTB struct {
	testing.TB
	msg string
	dir string
}

func (f *fakeTB) TempDir() string { return f.dir }

func (f *fakeTB) Cleanup(func()) {}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.msg = fmt.Sprintf(format, args...)
	panic(errFatal{})
}

func TestOpenAtRejectsUnseedablePaths(t *testing.T) {
	for _, path := range []string{"", ":memory:", "file:x.db", "file::memory:?cache=shared"} {
		t.Run(path, func(t *testing.T) {
			ftb := &fakeTB{}
			func() {
				defer func() {
					r := recover()
					if _, ok := r.(errFatal); !ok {
						t.Fatalf("recover() = %v, want the Fatalf sentinel", r)
					}
				}()
				sqlDB, err := OpenAt(context.Background(), ftb, path)
				if sqlDB != nil {
					_ = sqlDB.Close()
				}
				t.Fatalf("OpenAt(%q) returned (%v) instead of failing the test", path, err)
			}()
			if !strings.Contains(ftb.msg, "cannot be seeded") {
				t.Errorf("Fatalf message = %q, want it to explain the rejection", ftb.msg)
			}
		})
	}
}

func TestOpenAtZeroByteFileIsMigratedByRealOpen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "empty.db")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatalf("seed empty file: %v", err)
	}
	sqlDB, err := OpenAt(ctx, t, path)
	if err != nil {
		t.Fatalf("OpenAt: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if got := scalar(t, sqlDB, "SELECT COUNT(*) FROM sqlite_master WHERE name='work_queue'"); got != "1" {
		t.Errorf("zero-byte file was not migrated by the real open")
	}
}

func TestRequireNoWAL(t *testing.T) {
	base := filepath.Join(t.TempDir(), "x.db")
	if err := requireNoWAL(base); err != nil {
		t.Errorf("no WAL file: %v", err)
	}
	if err := os.WriteFile(base+"-wal", nil, 0600); err != nil {
		t.Fatalf("write empty wal: %v", err)
	}
	if err := requireNoWAL(base); err != nil {
		t.Errorf("empty WAL must be accepted: %v", err)
	}
	if err := os.WriteFile(base+"-wal", []byte("pages"), 0600); err != nil {
		t.Fatalf("write wal: %v", err)
	}
	if err := requireNoWAL(base); err == nil || !strings.Contains(err.Error(), "WAL") {
		t.Errorf("non-empty WAL: err = %v, want a WAL error", err)
	}
}

func TestWriteExclusiveKeepsExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	if err := writeExclusive(path, []byte("first")); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeExclusive(path, []byte("second")); err != nil {
		t.Fatalf("second write must treat EEXIST as success: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "first" {
		t.Errorf("existing file overwritten: %q", got)
	}
}

func TestMaterializeStatError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A regular file used as a directory makes stat fail with ENOTDIR, which
	// is neither "exists" nor "not exist".
	if err := materialize(filepath.Join(file, "x.db")); err == nil {
		t.Error("materialize under a regular file: want an error")
	}
	if err := materialize(""); err == nil {
		t.Error("materialize empty path: want an error")
	}
}

func TestWriteExclusiveCreateError(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	path := filepath.Join(dir, "x.db")
	err := writeExclusive(path, []byte("x"))
	if err == nil {
		t.Skip("directory is writable despite chmod 0500 (running as root?)")
	}
	if !strings.Contains(err.Error(), "create") {
		t.Errorf("err = %v, want a create error", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Errorf("failed create left a file behind")
	}
}

func TestPathAndOpenFailLoudlyWhenMaterializeFails(t *testing.T) {
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(plain, nil, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	for name, call := range map[string]func(testing.TB){
		"Path": func(tb testing.TB) { Path(tb) },
		"Open": func(tb testing.TB) { Open(tb) },
	} {
		t.Run(name, func(t *testing.T) {
			// TempDir under a regular file: the later stat fails with ENOTDIR.
			ftb := &fakeTB{dir: plain}
			func() {
				defer func() {
					if _, ok := recover().(errFatal); !ok {
						t.Fatalf("%s did not fail the test", name)
					}
				}()
				call(ftb)
			}()
			if !strings.Contains(ftb.msg, "dbtest.") {
				t.Errorf("Fatalf message = %q", ftb.msg)
			}
		})
	}
}
