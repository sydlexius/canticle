package commands

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
)

type upstreamSeed struct {
	name    string
	lane    string
	status  string
	sidecar string // file name beside the audio ("" = none)
	body    string
	symlink bool // make the sidecar a symlink to itself-elsewhere
}

const upTagged = "[source:innertube]\n[upstream:lyricfind]\n[re:canticle]\n[00:01.00]la\n"

// seedUpstream builds a config + DB with one row per spec. Names are invented.
func seedUpstream(t *testing.T, specs []upstreamSeed) (ctx context.Context, cfgPath, dbPath, dir string, ids map[string]int64) {
	t.Helper()
	ctx = context.Background()
	dir = t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	cfgPath = filepath.Join(dir, "config.toml")
	reconcilePathsCfg(t, cfgPath, dbPath)
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	ids = map[string]int64{}
	for i, s := range specs {
		audio := filepath.Join(dir, s.name+".flac")
		if s.sidecar != "" {
			p := filepath.Join(dir, s.sidecar)
			if s.symlink {
				target := filepath.Join(t.TempDir(), "real")
				if err := os.WriteFile(target, []byte(s.body), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, p); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(p, []byte(s.body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		key := "k" + string(rune('a'+i))
		var id int64
		if err := sqlDB.QueryRowContext(ctx,
			`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, status, outcome_type, provider_lane)
             VALUES ('Zed Quartet', ?, ?, ?, ?, ?, 'synced', ?) RETURNING id`,
			"Song "+s.name, key, key, audio, s.status, s.lane).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", s.name, err)
		}
		ids[s.name] = id
	}
	return ctx, cfgPath, dbPath, dir, ids
}

func readUpstream(t *testing.T, ctx context.Context, dbPath string, id int64) sql.NullString {
	t.Helper()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	var v sql.NullString
	if err := sqlDB.QueryRowContext(ctx, `SELECT upstream FROM work_queue WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatalf("read upstream: %v", err)
	}
	return v
}

var upstreamSpecs = []upstreamSeed{
	{name: "fill", lane: "innertube", status: "done", sidecar: "fill.lrc", body: upTagged},
	{name: "elrc", lane: "innertube", status: "done", sidecar: "elrc.elrc", body: upTagged},
	{name: "txt", lane: "innertube", status: "done", sidecar: "txt.txt", body: upTagged},
	{name: "variant", lane: "innertube", status: "done", sidecar: "variant.LRC", body: upTagged},
	{name: "mismatch", lane: "innertube", status: "done", sidecar: "mismatch.lrc", body: "[source:musixmatch]\n[upstream:lyricfind]\n[00:01.00]la\n"},
	{name: "missing", lane: "innertube", status: "done"},
	{name: "unreadable", lane: "innertube", status: "done", sidecar: "unreadable.lrc", body: upTagged, symlink: true},
	{name: "notags", lane: "innertube", status: "done", sidecar: "notags.lrc", body: "[00:01.00]la\n"},
	{name: "noupstream", lane: "innertube", status: "done", sidecar: "noupstream.lrc", body: "[source:innertube]\n[00:01.00]la\n"},
	{name: "inflight", lane: "innertube", status: "processing", sidecar: "inflight.lrc", body: upTagged},
	{name: "direct", lane: "musixmatch", status: "done", sidecar: "direct.lrc", body: upTagged},
}

// TestRunReconcileUpstream_DryRunMatchesApply: a dry run writes nothing and
// reports the SAME counts the apply produces; the apply fills exactly the rows
// whose sidecar agrees, each skip reason counted on its own.
func TestRunReconcileUpstream_DryRunMatchesApply(t *testing.T) {
	ctx, cfgPath, dbPath, dir, ids := seedUpstream(t, upstreamSpecs)

	var dry bytes.Buffer
	if code := runReconcileUpstream(ctx, &dry, ScanReconcileUpstreamCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("dry exit=%d out=%s", code, dry.String())
	}
	for name, id := range ids {
		if got := readUpstream(t, ctx, dbPath, id); got.Valid {
			t.Errorf("dry run wrote upstream for %s: %q", name, got.String)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "reconcile-upstream-backup-*.jsonl")); len(m) != 0 {
		t.Errorf("dry run wrote a backup: %v", m)
	}

	var applied bytes.Buffer
	if code := runReconcileUpstream(ctx, &applied, ScanReconcileUpstreamCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, applied.String())
	}
	const wantCounts = "scanned 10 row(s); %s 4 (skipped: source_mismatch=1 no_sidecar=1 unreadable=1 no_tags=1 no_upstream=1 processing=1 raced=0 write_failed=0)"
	if want := strings.Replace(wantCounts, "%s", "would fill", 1); !strings.Contains(dry.String(), want) {
		t.Errorf("dry counts want %q; got %s", want, dry.String())
	}
	if want := strings.Replace(wantCounts, "%s", "filled", 1); !strings.Contains(applied.String(), want) {
		t.Errorf("apply counts want %q; got %s", want, applied.String())
	}
	for _, n := range []string{"fill", "elrc", "txt", "variant"} {
		if got := readUpstream(t, ctx, dbPath, ids[n]); !got.Valid || got.String != "lyricfind" {
			t.Errorf("%s: upstream = %+v, want lyricfind", n, got)
		}
	}
	for _, n := range []string{"mismatch", "missing", "unreadable", "notags", "noupstream", "inflight", "direct"} {
		if got := readUpstream(t, ctx, dbPath, ids[n]); got.Valid {
			t.Errorf("%s: upstream = %q, want NULL", n, got.String)
		}
	}

	// Aggregate-only stdout: no path, artist or title.
	for _, out := range []string{dry.String(), strings.SplitN(applied.String(), "backup of", 2)[0]} {
		for _, leak := range []string{dir, "Zed Quartet", "Song ", ".lrc", ".flac"} {
			if strings.Contains(out, leak) {
				t.Errorf("stdout leaks %q: %s", leak, out)
			}
		}
	}

	// Backup: 0600, one record per filled row, no path or title.
	m, _ := filepath.Glob(filepath.Join(dir, "reconcile-upstream-backup-*.jsonl"))
	if len(m) != 1 {
		t.Fatalf("want one backup file; got %v", m)
	}
	if fi, err := os.Stat(m[0]); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("backup mode: %v %v", fi, err)
	}
	b, _ := os.ReadFile(m[0]) //nolint:gosec // test-controlled path
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 4 {
		t.Fatalf("want 4 backup records, got %d: %q", len(lines), b)
	}
	for _, l := range lines {
		var rec upstreamBackupRecord
		if err := json.Unmarshal([]byte(l), &rec); err != nil || rec.Lane != "innertube" || rec.Upstream != "lyricfind" || rec.ID == 0 {
			t.Errorf("bad backup line %q: %v", l, err)
		}
	}

	// Rerun: the filled rows are no longer candidates.
	var again bytes.Buffer
	if code := runReconcileUpstream(ctx, &again, ScanReconcileUpstreamCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("rerun exit=%d", code)
	}
	if !regexp.MustCompile(`scanned 6 row\(s\); filled 0`).MatchString(again.String()) {
		t.Errorf("rerun want 6 scanned, 0 filled; got %s", again.String())
	}
}

// TestRunReconcileUpstream_SkipsBlankSourcePath: a row with no source_path has
// no derivable sidecar and is counted, not guessed.
func TestRunReconcileUpstream_SkipsBlankSourcePath(t *testing.T) {
	ctx, cfgPath, dbPath, _, ids := seedUpstream(t, []upstreamSeed{{name: "blank", lane: "innertube", status: "done"}})
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET source_path = ''`); err != nil {
		t.Fatal(err)
	}
	_ = sqlDB.Close()
	var out bytes.Buffer
	if code := runReconcileUpstream(ctx, &out, ScanReconcileUpstreamCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(out.String(), "no_sidecar=1") || readUpstream(t, ctx, dbPath, ids["blank"]).Valid {
		t.Errorf("want no_sidecar=1 and NULL; got %s", out.String())
	}
}
