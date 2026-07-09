package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/doxazo-net/canticle/internal/db"
	"github.com/doxazo-net/canticle/internal/library"
	"github.com/doxazo-net/canticle/internal/models"
	"github.com/doxazo-net/canticle/internal/testutil"
)

const (
	realignTestMBID  = "550e8400-e29b-41d4-a716-446655440000"
	realignTestMBID2 = "660e8400-e29b-41d4-a716-446655440001"
	realignTestISRC  = "GBRC12345678"
)

// writeRealignCfg writes a minimal config: a [db] path and an optional [realign]
// body (raw TOML lines under the section). An empty body leaves every realign
// field at its conservative default.
func writeRealignCfg(t *testing.T, path, dbPath, realignBody string) {
	t.Helper()
	escape := func(s string) string { return strings.ReplaceAll(s, `\`, `\\`) }
	content := "[db]\npath = \"" + escape(dbPath) + "\"\n"
	if realignBody != "" {
		content += "\n[realign]\n" + realignBody + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writeRealignCfg: %v", err)
	}
}

// seedRealignLibrary opens the DB, runs migrations, and inserts one library row
// for root.
func seedRealignLibrary(t *testing.T, ctx context.Context, dbPath, root, name string) {
	t.Helper()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open seed: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	if _, err := library.New(sqlDB).Add(ctx, root, name, models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}
}

// writeSidecar writes a lyric sidecar with the given lines.
func writeSidecar(t *testing.T, dir, name string, lines ...string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write sidecar %s: %v", name, err)
	}
	return p
}

// writeMP3 writes a synthetic tagged .mp3 with optional ISRC/MBID.
func writeMP3(t *testing.T, dir, name, artist, title, isrc, mbid string) {
	t.Helper()
	var text, txxx map[string]string
	if isrc != "" {
		text = map[string]string{"TSRC": isrc}
	}
	if mbid != "" {
		txxx = map[string]string{"MusicBrainz Track Id": mbid}
	}
	if err := testutil.WriteAudioFileExtended(dir, name, artist, title, "Album", "", text, txxx); err != nil {
		t.Fatalf("write mp3 %s: %v", name, err)
	}
}

func readBackupRecords(t *testing.T, dir string) []realignBackupRecord {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "realign-backup-*.jsonl"))
	if err != nil {
		t.Fatalf("glob backup: %v", err)
	}
	if len(matches) == 0 {
		return nil
	}
	var recs []realignBackupRecord
	for _, m := range matches {
		b, err := os.ReadFile(m) //nolint:gosec // test-controlled path
		if err != nil {
			t.Fatalf("read backup: %v", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line == "" {
				continue
			}
			var rec realignBackupRecord
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("decode backup line %q: %v", line, err)
			}
			recs = append(recs, rec)
		}
	}
	return recs
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("expected %s to NOT exist", path)
	}
}

// TestRealign_IsRecognizedSubcommand guards the dispatch wiring: "realign" must
// be recognized as a subcommand (both in the go-arg Args struct and the
// usesSubcommand fast-path) so it is never parsed as a fetch positional.
func TestRealign_IsRecognizedSubcommand(t *testing.T) {
	if !usesSubcommand([]string{"realign"}) {
		t.Error("usesSubcommand(realign) = false; realign would fall through to fetch mode")
	}
}

// TestRealign_Heuristic covers the single-candidate filesystem tier gated by the
// name guard: one orphan sidecar and one audio file missing its sidecar, whose
// header artist/title match the audio's tags. Dry-run must touch nothing; --yes
// must rename and write a method-tagged backup.
func TestRealign_Heuristic(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seedRealignLibrary(t, ctx, dbPath, root, "main")
	cfgPath := filepath.Join(dir, "config.toml")
	writeRealignCfg(t, cfgPath, dbPath, "")

	writeMP3(t, root, "newname.mp3", "Artist", "Song", "", "")
	orphan := writeSidecar(t, root, "oldname.lrc", "[ar:Artist]", "[ti:Song]", "[00:01.00]la la")
	target := filepath.Join(root, "newname.lrc")

	// Dry run: lists the move, changes nothing.
	var dry bytes.Buffer
	if code := runRealign(ctx, &dry, RealignCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("dry-run exit=%d out=%s", code, dry.String())
	}
	if !strings.Contains(dry.String(), "would move [heuristic]") {
		t.Errorf("dry-run output missing heuristic move:\n%s", dry.String())
	}
	mustExist(t, orphan)
	mustNotExist(t, target)
	if recs := readBackupRecords(t, dir); recs != nil {
		t.Errorf("dry-run must not write a backup, got %d records", len(recs))
	}

	// Apply.
	var buf bytes.Buffer
	if code := runRealign(ctx, &buf, RealignCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, buf.String())
	}
	mustNotExist(t, orphan)
	mustExist(t, target)
	recs := readBackupRecords(t, dir)
	// NewPath is the symlink-resolved absolute path (macOS /var -> /private/var),
	// so compare by base name rather than the raw temp path.
	if len(recs) != 1 || recs[0].Method != "heuristic" || filepath.Base(recs[0].NewPath) != filepath.Base(target) {
		t.Errorf("backup = %+v; want one heuristic record for %s", recs, target)
	}
}

// TestRealign_ExactProvenance covers the exact tier: an orphan whose [mbid:]
// header uniquely matches one audio file's embedded MBID among several, so the
// pair cannot be resolved by the filesystem heuristic alone.
func TestRealign_ExactProvenance(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seedRealignLibrary(t, ctx, dbPath, root, "main")
	cfgPath := filepath.Join(dir, "config.toml")
	writeRealignCfg(t, cfgPath, dbPath, "")

	// Two audio files missing sidecars (so no heuristic single-pair), only one
	// carrying the orphan's MBID.
	writeMP3(t, root, "aaa.mp3", "Wrong", "Wrong", "", realignTestMBID)
	writeMP3(t, root, "bbb.mp3", "Other", "Other", "", realignTestMBID2)
	orphan := writeSidecar(t, root, "orphan.lrc", "[mbid:"+realignTestMBID+"]", "[00:01.00]la")
	target := filepath.Join(root, "aaa.lrc")

	var buf bytes.Buffer
	if code := runRealign(ctx, &buf, RealignCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, buf.String())
	}
	mustNotExist(t, orphan)
	mustExist(t, target)
	recs := readBackupRecords(t, dir)
	if len(recs) != 1 || recs[0].Method != "exact" {
		t.Errorf("backup = %+v; want one exact record", recs)
	}
}

// TestRealign_ExactISRCViaIdentityKeys proves identity_keys order is honored:
// with the orphan carrying only an ISRC, the exact tier matches via the isrc key.
func TestRealign_ExactISRCViaIdentityKeys(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seedRealignLibrary(t, ctx, dbPath, root, "main")
	cfgPath := filepath.Join(dir, "config.toml")
	writeRealignCfg(t, cfgPath, dbPath, "")

	writeMP3(t, root, "track.mp3", "X", "Y", realignTestISRC, "")
	writeMP3(t, root, "decoy.mp3", "Z", "W", "", "")
	orphan := writeSidecar(t, root, "orphan.lrc", "[isrc:"+realignTestISRC+"]", "[00:01.00]la")
	target := filepath.Join(root, "track.lrc")

	var buf bytes.Buffer
	if code := runRealign(ctx, &buf, RealignCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, buf.String())
	}
	mustNotExist(t, orphan)
	mustExist(t, target)
}

// TestRealign_CrossDirectory: an exact MBID match to an audio file in a different
// directory only applies when realign.cross_directory is enabled.
func TestRealign_CrossDirectory(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		crossDir  bool
		wantMoved bool
	}{
		{"disabled", false, false},
		{"enabled", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "test.db")
			root := filepath.Join(dir, "music")
			dir1 := filepath.Join(root, "d1")
			dir2 := filepath.Join(root, "d2")
			for _, d := range []string{dir1, dir2} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			}
			seedRealignLibrary(t, ctx, dbPath, root, "main")
			cfgPath := filepath.Join(dir, "config.toml")
			body := "cross_directory = false"
			if tc.crossDir {
				body = "cross_directory = true"
			}
			writeRealignCfg(t, cfgPath, dbPath, body)

			// Orphan alone in dir1; matching audio alone (no sidecar) in dir2.
			writeMP3(t, dir2, "audio.mp3", "A", "B", "", realignTestMBID)
			orphan := writeSidecar(t, dir1, "orphan.lrc", "[mbid:"+realignTestMBID+"]", "[00:01.00]la")
			target := filepath.Join(dir2, "audio.lrc")

			var buf bytes.Buffer
			if code := runRealign(ctx, &buf, RealignCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
				t.Fatalf("apply exit=%d out=%s", code, buf.String())
			}
			if tc.wantMoved {
				mustNotExist(t, orphan)
				mustExist(t, target)
			} else {
				mustExist(t, orphan)
				mustNotExist(t, target)
			}
		})
	}
}

// TestRealign_Ambiguous: a directory with two audio files missing sidecars and
// one provenance-less orphan cannot be paired; it is reported and left in place.
func TestRealign_Ambiguous(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seedRealignLibrary(t, ctx, dbPath, root, "main")
	cfgPath := filepath.Join(dir, "config.toml")
	writeRealignCfg(t, cfgPath, dbPath, "")

	writeMP3(t, root, "one.mp3", "A", "B", "", "")
	writeMP3(t, root, "two.mp3", "C", "D", "", "")
	orphan := writeSidecar(t, root, "orphan.lrc", "[00:01.00]la")

	var buf bytes.Buffer
	if code := runRealign(ctx, &buf, RealignCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "ambiguous:") {
		t.Errorf("output missing ambiguous report:\n%s", buf.String())
	}
	mustExist(t, orphan)
	if recs := readBackupRecords(t, dir); recs != nil {
		t.Errorf("ambiguous run must not move anything, got %d backup records", len(recs))
	}
}

// TestRealign_ConflictDestinationExists: an exact match whose destination sidecar
// already exists is a conflict and is never clobbered.
func TestRealign_ConflictDestinationExists(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seedRealignLibrary(t, ctx, dbPath, root, "main")
	cfgPath := filepath.Join(dir, "config.toml")
	writeRealignCfg(t, cfgPath, dbPath, "")

	writeMP3(t, root, "aaa.mp3", "A", "B", "", realignTestMBID)
	existing := writeSidecar(t, root, "aaa.lrc", "[00:09.00]existing")
	orphan := writeSidecar(t, root, "orphan.lrc", "[mbid:"+realignTestMBID+"]", "[00:01.00]la")

	var buf bytes.Buffer
	if code := runRealign(ctx, &buf, RealignCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "conflict:") {
		t.Errorf("output missing conflict report:\n%s", buf.String())
	}
	mustExist(t, orphan)
	// The pre-existing destination must be untouched.
	b, err := os.ReadFile(existing) //nolint:gosec // test path
	if err != nil || !strings.Contains(string(b), "existing") {
		t.Errorf("existing destination was clobbered: %v", err)
	}
}

// TestRealign_ConflictMultipleExact: two audio files share the orphan's MBID, so
// the exact tier cannot disambiguate and reports a conflict.
func TestRealign_ConflictMultipleExact(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seedRealignLibrary(t, ctx, dbPath, root, "main")
	cfgPath := filepath.Join(dir, "config.toml")
	writeRealignCfg(t, cfgPath, dbPath, "")

	writeMP3(t, root, "aaa.mp3", "A", "B", "", realignTestMBID)
	writeMP3(t, root, "bbb.mp3", "C", "D", "", realignTestMBID)
	orphan := writeSidecar(t, root, "orphan.lrc", "[mbid:"+realignTestMBID+"]", "[00:01.00]la")

	var buf bytes.Buffer
	if code := runRealign(ctx, &buf, RealignCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "conflict:") {
		t.Errorf("output missing conflict report:\n%s", buf.String())
	}
	mustExist(t, orphan)
}

// TestRealign_RequireProvenance: with require_provenance set, heuristic candidates
// are reported but skipped while exact matches still apply.
func TestRealign_RequireProvenance(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	root := filepath.Join(dir, "music")
	exactDir := filepath.Join(root, "exact")
	heurDir := filepath.Join(root, "heur")
	for _, d := range []string{exactDir, heurDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	seedRealignLibrary(t, ctx, dbPath, root, "main")
	cfgPath := filepath.Join(dir, "config.toml")
	writeRealignCfg(t, cfgPath, dbPath, "require_provenance = true")

	// Exact dir: MBID match -> eligible.
	writeMP3(t, exactDir, "aaa.mp3", "A", "B", "", realignTestMBID)
	exactOrphan := writeSidecar(t, exactDir, "orphan.lrc", "[mbid:"+realignTestMBID+"]", "[00:01.00]la")
	exactTarget := filepath.Join(exactDir, "aaa.lrc")

	// Heuristic dir: name-only match -> reported, gated, not applied.
	writeMP3(t, heurDir, "newname.mp3", "Artist", "Song", "", "")
	heurOrphan := writeSidecar(t, heurDir, "oldname.lrc", "[ar:Artist]", "[ti:Song]", "[00:01.00]la")
	heurTarget := filepath.Join(heurDir, "newname.lrc")

	var buf bytes.Buffer
	if code := runRealign(ctx, &buf, RealignCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, buf.String())
	}
	mustNotExist(t, exactOrphan)
	mustExist(t, exactTarget)
	// Heuristic move gated: orphan stays, target not created.
	mustExist(t, heurOrphan)
	mustNotExist(t, heurTarget)
	if !strings.Contains(buf.String(), "require_provenance") {
		t.Errorf("expected a gated-skip note mentioning require_provenance:\n%s", buf.String())
	}
	recs := readBackupRecords(t, dir)
	if len(recs) != 1 || recs[0].Method != "exact" {
		t.Errorf("backup = %+v; want only the exact move", recs)
	}
}

// TestRealign_ExtensionPreserved: a .txt orphan realigns to a .txt sidecar, never
// promoted to .lrc.
func TestRealign_ExtensionPreserved(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	seedRealignLibrary(t, ctx, dbPath, root, "main")
	cfgPath := filepath.Join(dir, "config.toml")
	writeRealignCfg(t, cfgPath, dbPath, "")

	writeMP3(t, root, "newname.mp3", "Artist", "Song", "", "")
	writeSidecar(t, root, "oldname.txt", "[ar:Artist]", "[ti:Song]", "some unsynced lyrics")
	txtTarget := filepath.Join(root, "newname.txt")
	lrcTarget := filepath.Join(root, "newname.lrc")

	var buf bytes.Buffer
	if code := runRealign(ctx, &buf, RealignCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, buf.String())
	}
	mustExist(t, txtTarget)
	mustNotExist(t, lrcTarget)
}

// TestRealign_LibraryScope: --library restricts the walk to a single root.
func TestRealign_LibraryScope(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	rootA := filepath.Join(dir, "libA")
	rootB := filepath.Join(dir, "libB")
	for _, d := range []string{rootA, rootB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	seedRealignLibrary(t, ctx, dbPath, rootA, "libA")
	seedRealignLibrary(t, ctx, dbPath, rootB, "libB")
	cfgPath := filepath.Join(dir, "config.toml")
	writeRealignCfg(t, cfgPath, dbPath, "")

	writeMP3(t, rootA, "newA.mp3", "Artist", "SongA", "", "")
	orphanA := writeSidecar(t, rootA, "oldA.lrc", "[ar:Artist]", "[ti:SongA]", "[00:01.00]la")
	targetA := filepath.Join(rootA, "newA.lrc")

	writeMP3(t, rootB, "newB.mp3", "Artist", "SongB", "", "")
	orphanB := writeSidecar(t, rootB, "oldB.lrc", "[ar:Artist]", "[ti:SongB]", "[00:01.00]la")
	targetB := filepath.Join(rootB, "newB.lrc")

	var buf bytes.Buffer
	if code := runRealign(ctx, &buf, RealignCmd{ConfigPath: cfgPath, Yes: true, Library: "libA"}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, buf.String())
	}
	// Only libA moved.
	mustNotExist(t, orphanA)
	mustExist(t, targetA)
	mustExist(t, orphanB)
	mustNotExist(t, targetB)
}
