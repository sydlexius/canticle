package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/audiodur"
	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
)

// revalidateFixture builds a config + database + library root holding one audio
// file and one .lrc, and returns (configPath, root, lrcPath).
//
// The audio_durations cache is primed for the audio file through the real
// audiodur store, so the command reaches a verdict through the same lookup it
// uses in production (#441). Without that priming every file reads as
// unknown-duration and fails open, which is the correct behavior but tests
// nothing about remediation.
func revalidateFixture(t *testing.T, lrcBody string) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "library", secretishAlbumDir)
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	audio := filepath.Join(root, secretishTrackName+".mp3")
	if err := os.WriteFile(audio, []byte("stub"), 0o600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	lrc := filepath.Join(root, secretishTrackName+".lrc")
	if err := os.WriteFile(lrc, []byte(lrcBody), 0o600); err != nil {
		t.Fatalf("write lrc: %v", err)
	}

	dbPath := filepath.Join(dir, "canticle.db")
	cfgPath := filepath.Join(dir, "config.toml")
	cfg := "[db]\npath = \"" + filepath.ToSlash(dbPath) + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	primeDuration(t, dbPath, audio, fixtureDurationSeconds)
	return cfgPath, filepath.Join(dir, "library"), lrc
}

// fixtureDurationSeconds is the duration every fixture audio file is recorded
// at. The .lrc bodies in these tests are written against it.
const fixtureDurationSeconds = 120

// primeDuration records seconds as the cached exact duration of audio, through
// the same audiodur store the command reads from.
func primeDuration(t *testing.T, dbPath, audio string, seconds int) {
	t.Helper()
	sqlDB, err := db.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	fi, err := os.Stat(audio)
	if err != nil {
		t.Fatalf("stat audio: %v", err)
	}
	if err := audiodur.New(sqlDB).Record(t.Context(), audio, fi.ModTime().UnixNano(), fi.Size(), seconds); err != nil {
		t.Fatalf("record duration: %v", err)
	}
}

// secretishAlbumDir and secretishTrackName stand in for the private library
// metadata a real run walks. They are deliberately distinctive strings so the
// stdout-privacy test can assert their ABSENCE from the report: a directory
// tree carries artist/album/title, which must never reach stdout.
const (
	secretishAlbumDir  = "PrivateAlbumName"
	secretishTrackName = "PrivateTrackTitle"
)

// TestRevalidateDryRunWritesNothingAndReportsCounts is the CLI-level dry-run
// rail: no --apply means the .lrc survives, no quarantine directory appears, and
// the report is a count line.
func TestRevalidateDryRunWritesNothingAndReportsCounts(t *testing.T) {
	cfgPath, root, lrc := revalidateFixture(t, "[00:10.00]alpha\n[05:00.00]beta\n")
	var out bytes.Buffer
	code := runRevalidate(t.Context(), &out, RevalidateCmd{
		Roots: []string{root}, ConfigPath: cfgPath,
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0: %s", code, out.String())
	}
	if _, err := os.Stat(lrc); err != nil {
		t.Errorf("a dry run removed the .lrc: %v", err)
	}
	if !strings.Contains(out.String(), "scanned=") {
		t.Errorf("no count line in the report: %s", out.String())
	}
	if !strings.Contains(out.String(), "dry run") {
		t.Errorf("the report does not say it was a dry run: %s", out.String())
	}
}

// TestRevalidateStdoutIsAggregateOnly is the PRIVACY rail. A library path
// carries the artist, album, and track title. This runs the command in the mode
// that has the most to say -- an apply that actually quarantines a file -- and
// asserts that no identifying fragment of the tree reaches stdout.
func TestRevalidateStdoutIsAggregateOnly(t *testing.T) {
	cfgPath, root, _ := revalidateFixture(t, "[00:10.00]alpha\n[05:00.00]beta\n")
	quarantine := filepath.Join(t.TempDir(), "q")

	for _, apply := range []bool{false, true} {
		var out bytes.Buffer
		code := runRevalidate(t.Context(), &out, RevalidateCmd{
			Roots: []string{root}, ConfigPath: cfgPath, Apply: apply, QuarantineDir: quarantine,
		})
		if code != 0 {
			t.Fatalf("apply=%v: exit = %d: %s", apply, code, out.String())
		}
		got := out.String()
		for _, forbidden := range []string{secretishTrackName, secretishAlbumDir, ".lrc", root} {
			if strings.Contains(got, forbidden) {
				t.Errorf("apply=%v: stdout leaked %q -- library paths carry artist/album/title and must never be printed.\ngot: %s",
					apply, forbidden, got)
			}
		}
	}
}

// TestRevalidateApplyQuarantinesNotDeletes is the CLI-level reversibility rail.
func TestRevalidateApplyQuarantinesNotDeletes(t *testing.T) {
	body := "[00:10.00]alpha\n[05:00.00]beta\n"
	cfgPath, root, lrc := revalidateFixture(t, body)
	quarantine := filepath.Join(t.TempDir(), "q")

	var out bytes.Buffer
	code := runRevalidate(t.Context(), &out, RevalidateCmd{
		Roots: []string{root}, ConfigPath: cfgPath, Apply: true, QuarantineDir: quarantine,
	})
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, out.String())
	}
	if _, err := os.Stat(lrc); !os.IsNotExist(err) {
		t.Errorf("the .lrc is still in the library: %v", err)
	}
	found := false
	_ = filepath.WalkDir(quarantine, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".lrc") {
			b, rerr := os.ReadFile(p)
			if rerr == nil && string(b) == body {
				found = true
			}
		}
		return nil
	})
	if !found {
		t.Error("no byte-identical copy under the quarantine root -- the file was DELETED, not moved aside")
	}
}

// TestRevalidateTailFileCarriesTheDetail: the per-file detail stdout refuses to
// print must be available to the operator who explicitly asks for it.
func TestRevalidateTailFileCarriesTheDetail(t *testing.T) {
	cfgPath, root, _ := revalidateFixture(t, "[00:10.00]alpha\n[05:00.00]beta\n")
	tail := filepath.Join(t.TempDir(), "offenders.tsv")

	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{
		Roots: []string{root}, ConfigPath: cfgPath, Tail: tail,
	}); code != 0 {
		t.Fatalf("exit = %d: %s", code, out.String())
	}
	b, err := os.ReadFile(tail)
	if err != nil {
		t.Fatalf("read tail: %v", err)
	}
	if !strings.Contains(string(b), secretishTrackName) {
		t.Errorf("the tail file does not carry the offender path: %s", b)
	}
	if strings.Contains(out.String(), secretishTrackName) {
		t.Error("writing a tail file must not also leak the path to stdout")
	}
}

// TestRevalidateUnknownOnFailIsAUsageError.
func TestRevalidateUnknownOnFailIsAUsageError(t *testing.T) {
	cfgPath, root, _ := revalidateFixture(t, "[00:10.00]alpha\n")
	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{
		Roots: []string{root}, ConfigPath: cfgPath, OnFail: "shred", QuarantineDir: t.TempDir(),
	}); code != 2 {
		t.Errorf("exit = %d, want 2: %s", code, out.String())
	}
}

// TestRevalidateIsReachableAsASubcommand guards the wiring the reachability test
// exists for, at the level an operator actually types.
func TestRevalidateIsReachableAsASubcommand(t *testing.T) {
	var out bytes.Buffer
	Run(t.Context(), []string{"revalidate", "--help"}, &out, Deps{})
	if strings.Contains(out.String(), legacyUsageMarker) {
		t.Fatalf("revalidate fell through to the legacy parser: %s", out.String())
	}
	for _, flag := range []string{"--apply", "--on-fail", "--purge", "--quarantine-dir", "--tail"} {
		if !strings.Contains(out.String(), flag) {
			t.Errorf("help does not offer %s: %s", flag, out.String())
		}
	}
}

// addRevalidateLibrary registers root as a library in the fixture database.
func addRevalidateLibrary(t *testing.T, cfgPath, name, root string) {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sqlDB, err := db.Open(t.Context(), cfg.DB.Path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	if _, err := library.New(sqlDB).Add(t.Context(), root, name, models.LibrarySettings{}); err != nil {
		t.Fatalf("add library: %v", err)
	}
}

// TestRevalidateDefaultsToEveryConfiguredLibrary: with no positional roots the
// pass walks what the database knows about.
func TestRevalidateDefaultsToEveryConfiguredLibrary(t *testing.T) {
	cfgPath, root, _ := revalidateFixture(t, "[00:10.00]alpha\n[05:00.00]beta\n")
	addRevalidateLibrary(t, cfgPath, "Main", root)

	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("exit = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "scanned=1") {
		t.Errorf("the configured library was not walked: %s", out.String())
	}
}

// TestRevalidateLibraryScopeResolvesByName.
func TestRevalidateLibraryScopeResolvesByName(t *testing.T) {
	cfgPath, root, _ := revalidateFixture(t, "[00:10.00]alpha\n[05:00.00]beta\n")
	addRevalidateLibrary(t, cfgPath, "Main", root)

	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{ConfigPath: cfgPath, Library: "Main"}); code != 0 {
		t.Fatalf("exit = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "scanned=1") {
		t.Errorf("the scoped library was not walked: %s", out.String())
	}
}

// TestRevalidateUnknownLibraryIsAnError.
func TestRevalidateUnknownLibraryIsAnError(t *testing.T) {
	cfgPath, _, _ := revalidateFixture(t, "[00:10.00]alpha\n")
	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{ConfigPath: cfgPath, Library: "nope"}); code != 1 {
		t.Errorf("exit = %d, want 1: %s", code, out.String())
	}
}

// TestRevalidateWithNoLibrariesIsANoOp: nothing configured, nothing to do, and
// the message says so rather than silently reporting zeros.
func TestRevalidateWithNoLibrariesIsANoOp(t *testing.T) {
	cfgPath, _, _ := revalidateFixture(t, "[00:10.00]alpha\n")
	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("exit = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "no roots to scan") {
		t.Errorf("want an explicit no-roots message: %s", out.String())
	}
}

// TestRevalidateApplyWithNothingToRemediate: a clean library reports so and
// writes no backup file.
func TestRevalidateApplyWithNothingToRemediate(t *testing.T) {
	cfgPath, root, lrc := revalidateFixture(t, "[00:10.00]alpha\n[01:00.00]beta\n")
	backup := filepath.Join(t.TempDir(), "backup.jsonl")

	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{
		Roots: []string{root}, ConfigPath: cfgPath, Apply: true,
		QuarantineDir: t.TempDir(), Backup: backup,
	}); code != 0 {
		t.Fatalf("exit = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "nothing to remediate") {
		t.Errorf("want a nothing-to-do message: %s", out.String())
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Errorf("a no-op apply wrote a backup file")
	}
	if _, err := os.Stat(lrc); err != nil {
		t.Errorf("a compliant .lrc was touched: %v", err)
	}
}

// TestRevalidateUnknownDurationIsCalledOut: the operator is told why files were
// skipped, so a cold duration cache does not look like a clean library.
func TestRevalidateUnknownDurationIsCalledOut(t *testing.T) {
	cfgPath, root, _ := revalidateFixture(t, "[00:10.00]alpha\n[05:00.00]beta\n")
	// Touching the audio invalidates the primed (mtime, size) key, so the
	// lookup misses exactly as a cold cache would.
	audio := filepath.Join(root, secretishAlbumDir, secretishTrackName+".mp3")
	if err := os.WriteFile(audio, []byte("changed bytes"), 0o600); err != nil {
		t.Fatalf("rewrite audio: %v", err)
	}
	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{Roots: []string{root}, ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("exit = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "unknown-duration=1") {
		t.Errorf("the unknown-duration count is not reported: %s", out.String())
	}
	if !strings.Contains(out.String(), "left untouched") {
		t.Errorf("the operator is not told the files were skipped: %s", out.String())
	}
}

// TestRevalidateBadTailPathIsAnError: an unwritable tail file must fail the run
// rather than silently discarding the detail the operator asked for.
func TestRevalidateBadTailPathIsAnError(t *testing.T) {
	cfgPath, root, _ := revalidateFixture(t, "[00:10.00]alpha\n[05:00.00]beta\n")
	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{
		Roots: []string{root}, ConfigPath: cfgPath,
		Tail: filepath.Join(t.TempDir(), "missing-dir", "tail.tsv"),
	}); code != 1 {
		t.Errorf("exit = %d, want 1: %s", code, out.String())
	}
}

// TestRevalidateBadConfigPathIsAnError.
func TestRevalidateBadConfigPathIsAnError(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(bad, []byte("this is not = valid toml ["), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{ConfigPath: bad}); code != 1 {
		t.Errorf("exit = %d, want 1: %s", code, out.String())
	}
}
