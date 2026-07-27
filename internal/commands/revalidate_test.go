package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/audiodur"
	"github.com/sydlexius/canticle/internal/db"
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
			b, rerr := os.ReadFile(p) //nolint:gosec // test fixture path
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
	b, err := os.ReadFile(tail) //nolint:gosec // test fixture path
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
