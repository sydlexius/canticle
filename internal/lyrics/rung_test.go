package lyrics

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/sidecar"
)

// Candidates, one per rung the writer can land.
func rungSongs() (instrumental, unsynced, line models.Song) {
	instrumental = models.Song{Track: models.Track{ArtistName: "A", TrackName: "T", Instrumental: 1}}
	unsynced = models.Song{Track: models.Track{ArtistName: "A", TrackName: "T"}, Lyrics: models.Lyrics{LyricsBody: "new words"}}
	line = models.Song{Track: models.Track{ArtistName: "A", TrackName: "T"}, Subtitles: models.Synced{Lines: []models.Lines{
		{Text: "new line", Time: models.Time{Total: 1, Seconds: 1}},
	}}}
	return instrumental, unsynced, line
}

func seed(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRungOnDisk pins the classifier to the artifact itself: no provenance
// header decides a rung, and a .lrc outranks a .txt beside it.
func TestRungOnDisk(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  Rung
	}{
		{"nothing", nil, RungNone},
		{"bare marker", map[string]string{"song.txt": InstrumentalMarker + "\n"}, RungInstrumental},
		{"provider marker with header", map[string]string{"song.txt": "[by:canticle]\n[source:musixmatch]\n" + InstrumentalMarker + "\n"}, RungInstrumental},
		{"unsynced text", map[string]string{"song.txt": "plain words\n"}, RungUnsynced},
		{"lrc without timestamps", map[string]string{"song.lrc": "plain words\n"}, RungUnsynced},
		{"line lrc", map[string]string{"song.lrc": "[00:01.00]line\n"}, RungLine},
		{"one word-marked line is enough", map[string]string{"song.lrc": "[00:01.00]line\n[00:02.00]<00:02.00>word <00:02.50>two\n"}, RungWord},
		{"owned companion makes it word", map[string]string{"song.lrc": "[00:01.00]line\n", "song.elrc": "[by:canticle]\n[00:01.00]<00:01.00>line\n"}, RungWord},
		{"lrc outranks txt", map[string]string{"song.lrc": "[00:01.00]line\n", "song.txt": "plain\n"}, RungLine},
		{"extension-case variant", map[string]string{"song.LRC": "[00:01.00]line\n"}, RungLine},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.files {
				seed(t, filepath.Join(dir, name), body)
			}
			if got := RungOnDisk(filepath.Join(dir, "song.txt"), sidecar.List(dir)); got != tc.want {
				t.Errorf("RungOnDisk = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestRungOnDisk_UnreadableKeepsTheFile: a sidecar the guard cannot judge is
// ranked as high as its extension allows, so doubt never licenses a delete.
func TestRungOnDisk_UnreadableKeepsTheFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	dir := t.TempDir()
	lrc := filepath.Join(dir, "song.lrc")
	seed(t, lrc, "[00:01.00]line\n")
	if err := os.Chmod(lrc, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lrc, 0o600) })
	if got := RungOnDisk(lrc, sidecar.List(dir)); got != RungWord {
		t.Errorf("RungOnDisk(unreadable .lrc) = %d, want RungWord", got)
	}
}

// TestWriteLRC_NoDowngrade is the #553 data-loss path: a lower-rung candidate
// must leave every file on disk untouched (no overwrite, no opposite removal,
// no companion removal) and report ErrKeptBetter so callers do not record it.
func TestWriteLRC_NoDowngrade(t *testing.T) {
	instrumental, unsynced, line := rungSongs()
	wordLRC := "[00:01.00]<00:01.00>alpha <00:02.00>beta\n"
	cases := []struct {
		name     string
		seedName string
		seedBody string
		song     models.Song
	}{
		{"unsynced over line lrc", "song.lrc", "[00:01.00]settled\n", unsynced},
		{"instrumental over line lrc", "song.lrc", "[00:01.00]settled\n", instrumental},
		{"instrumental over unsynced txt", "song.txt", "settled words\n", instrumental},
		{"line over word lrc", "song.lrc", wordLRC, line},
		// Qualifying words the writer cannot land (word sync off) write a line .lrc.
		{"unlandable words over word lrc", "song.lrc", wordLRC, a2Song()},
		{"unsynced over case-variant lrc", "song.LRC", "[00:01.00]settled\n", unsynced},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			settled := filepath.Join(dir, tc.seedName)
			seed(t, settled, tc.seedBody)
			err := NewLRCWriter().WriteLRC(tc.song, "song.lrc", dir)
			if !errors.Is(err, ErrKeptBetter) {
				t.Fatalf("WriteLRC err = %v, want ErrKeptBetter", err)
			}
			if got := readFileString(t, settled); got != tc.seedBody {
				t.Errorf("settled sidecar changed: %q", got)
			}
			if names := dirFiles(t, dir); len(names) != 1 {
				t.Errorf("dir = %v, want only the settled sidecar", names)
			}
		})
	}
}

// TestWriteLRC_NoDowngrade_KeepsCompanion: a line-only rewrite of a .lrc whose
// word timing lives in an owned companion would remove the companion; the
// on-disk rung is word, so the write is refused and both files survive.
func TestWriteLRC_NoDowngrade_KeepsCompanion(t *testing.T) {
	_, _, line := rungSongs()
	dir := t.TempDir()
	lrc := filepath.Join(dir, "song.lrc")
	seed(t, lrc, "[00:01.00]settled\n")
	companion := seedCompanion(t, dir)
	if err := modeWriter(false, true).WriteLRC(line, "song.lrc", dir); !errors.Is(err, ErrKeptBetter) {
		t.Fatalf("WriteLRC err = %v, want ErrKeptBetter", err)
	}
	mustExist(t, companion)
	if got := readFileString(t, lrc); got != "[00:01.00]settled\n" {
		t.Errorf(".lrc changed: %q", got)
	}
}

// TestWriteLRC_NoDowngrade_AllowsEqualOrHigher: the guard only refuses a
// strictly lower rung. Same-rung refreshes and upgrades write as before, and a
// forced (--update) write may go down.
func TestWriteLRC_NoDowngrade_AllowsEqualOrHigher(t *testing.T) {
	instrumental, unsynced, line := rungSongs()
	cases := []struct {
		name, seedName, seedBody string
		song                     models.Song
		force                    bool
		want, gone               string
	}{
		{"line over txt upgrades", "song.txt", "old words\n", line, false, "song.lrc", "song.txt"},
		{"unsynced over marker upgrades", "song.txt", InstrumentalMarker + "\n", unsynced, false, "song.txt", ""},
		{"line over line refreshes", "song.lrc", "[00:01.00]old\n", line, false, "song.lrc", ""},
		{"marker over marker refreshes", "song.txt", InstrumentalMarker + "\n", instrumental, false, "song.txt", ""},
		{"forced unsynced over lrc", "song.lrc", "[00:01.00]old\n", unsynced, true, "song.txt", "song.lrc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			seed(t, filepath.Join(dir, tc.seedName), tc.seedBody)
			w := NewLRCWriter()
			w.SetForceOverwrite(tc.force)
			if err := w.WriteLRC(tc.song, "song.lrc", dir); err != nil {
				t.Fatalf("WriteLRC: %v", err)
			}
			if got := readFileString(t, filepath.Join(dir, tc.want)); got == tc.seedBody {
				t.Errorf("%s was not rewritten", tc.want)
			}
			if tc.gone != "" {
				mustNotExist(t, filepath.Join(dir, tc.gone))
			}
		})
	}
}

// TestWriteLRC_NoDowngrade_JudgesEveryCaseVariant: on a case-sensitive
// filesystem a .txt write removes EVERY .lrc variant, so the guard must judge
// every one, not only the first. An untimed "song.lrc" ties an unsynced
// candidate; the word-synced "song.LRC" beside it must still block the write.
func TestWriteLRC_NoDowngrade_JudgesEveryCaseVariant(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveFS(t, dir) {
		t.Skip("filesystem is case-insensitive; song.lrc and song.LRC are one file there")
	}
	_, unsynced, _ := rungSongs()
	untimed, word := filepath.Join(dir, "song.lrc"), filepath.Join(dir, "song.LRC")
	seed(t, untimed, "plain words\n")
	seed(t, word, "[00:01.00]<00:01.00>alpha <00:02.00>beta\n")
	if got := RungOnDisk(filepath.Join(dir, "song.txt"), sidecar.List(dir)); got != RungWord {
		t.Errorf("RungOnDisk = %d, want RungWord (the best variant)", got)
	}
	if err := NewLRCWriter().WriteLRC(unsynced, "song.lrc", dir); !errors.Is(err, ErrKeptBetter) {
		t.Fatalf("WriteLRC err = %v, want ErrKeptBetter", err)
	}
	mustExist(t, untimed)
	mustExist(t, word)
	mustNotExist(t, filepath.Join(dir, "song.txt"))
}

// TestWriteLRC_NoDowngrade_CaseVariantOfSameRungProceeds: judging every
// variant must not turn a stale case variant into a blocker for a legitimate
// same-rung refresh or an upgrade (the guard refuses strictly lower only).
func TestWriteLRC_NoDowngrade_CaseVariantOfSameRungProceeds(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveFS(t, dir) {
		t.Skip("filesystem is case-insensitive; the variants alias one file")
	}
	_, unsynced, line := rungSongs()
	seed(t, filepath.Join(dir, "song.lrc"), "[00:01.00]old\n")
	seed(t, filepath.Join(dir, "song.LRC"), "[00:01.00]stale variant\n")
	if err := NewLRCWriter().WriteLRC(line, "song.lrc", dir); err != nil {
		t.Fatalf("line refresh beside a line variant: %v", err)
	}
	if got := readFileString(t, filepath.Join(dir, "song.lrc")); !strings.Contains(got, "new line") {
		t.Errorf("song.lrc not rewritten: %q", got)
	}

	dir = t.TempDir()
	seed(t, filepath.Join(dir, "song.txt"), "old words\n")
	seed(t, filepath.Join(dir, "song.TXT"), "stale words\n")
	if err := NewLRCWriter().WriteLRC(unsynced, "song.lrc", dir); err != nil {
		t.Fatalf("unsynced refresh beside an unsynced variant: %v", err)
	}
}

// TestRungOnDisk_SymlinkNeverFollowed: an exact-name symlink (which
// Listing.Variants reports) is never followed; it is kept as high as its
// extension allows, whatever its target says.
func TestRungOnDisk_SymlinkNeverFollowed(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	lrcTarget, txtTarget := filepath.Join(outside, "t.lrc"), filepath.Join(outside, "t.txt")
	seed(t, lrcTarget, "plain words\n")         // RungUnsynced if followed
	seed(t, txtTarget, InstrumentalMarker+"\n") // RungInstrumental if followed
	cases := []struct {
		name, link, target string
		want               Rung
	}{
		{"lrc", "song.lrc", lrcTarget, RungWord},
		{"txt", "song.txt", txtTarget, RungUnsynced},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := filepath.Join(dir, tc.name)
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(tc.target, filepath.Join(d, tc.link)); err != nil {
				t.Skipf("symlink unsupported: %v", err)
			}
			if got := RungOnDisk(filepath.Join(d, "song.txt"), sidecar.List(d)); got != tc.want {
				t.Errorf("RungOnDisk = %d, want %d (symlink kept unjudged, not followed)", got, tc.want)
			}
		})
	}
}

// TestWriteLRC_NoDowngrade_SameExtensionVariantNotJudged: the guard judges
// exactly what the write removes. A line write to "song.lrc" overwrites
// song.lrc and removes .txt variants, never "song.LRC", so a word-synced
// song.LRC must neither block the refresh nor be touched by it.
func TestWriteLRC_NoDowngrade_SameExtensionVariantNotJudged(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveFS(t, dir) {
		t.Skip("filesystem is case-insensitive; song.lrc and song.LRC are one file there")
	}
	_, _, line := rungSongs()
	target, variant := filepath.Join(dir, "song.lrc"), filepath.Join(dir, "song.LRC")
	wordBody := "[00:01.00]<00:01.00>alpha <00:02.00>beta\n"
	seed(t, target, "[00:01.00]old\n")
	seed(t, variant, wordBody)
	if err := NewLRCWriter().WriteLRC(line, "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRC err = %v, want nil (song.LRC is not in the removal set)", err)
	}
	if got := readFileString(t, target); !strings.Contains(got, "new line") {
		t.Errorf("song.lrc not rewritten: %q", got)
	}
	if got := readFileString(t, variant); got != wordBody {
		t.Errorf("song.LRC changed: %q", got)
	}
}

// TestWriteLRC_NoDowngrade_CompanionOfSurvivingVariant: a same-extension
// variant is not judged as a file, but when the write removes the owned
// companion that makes it word-synced ("song.elrc" beside a line "song.LRC"),
// that removal is a downgrade of the variant, and it is refused.
func TestWriteLRC_NoDowngrade_CompanionOfSurvivingVariant(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveFS(t, dir) {
		t.Skip("filesystem is case-insensitive; song.lrc and song.LRC are one file there")
	}
	if !sidecar.Active(sidecar.KindWordSynced) {
		t.Skip("companion inactive")
	}
	_, _, line := rungSongs()
	elrc := filepath.Join(dir, "song.elrc")
	seed(t, filepath.Join(dir, "song.LRC"), "[00:01.00]line\n")
	seed(t, elrc, "[by:canticle]\n[00:01.00]<00:01.00>line\n")
	if err := NewLRCWriter().WriteLRC(line, "song.lrc", dir); !errors.Is(err, ErrKeptBetter) {
		t.Fatalf("WriteLRC err = %v, want ErrKeptBetter (the companion removal downgrades song.LRC)", err)
	}
	mustExist(t, elrc)
	mustNotExist(t, filepath.Join(dir, "song.lrc"))
}

// TestWriteLRCIfAbsent: the write lands only on an empty stem. Anything already
// there stays byte for byte, including a file on the candidate's OWN rung, which
// is the one case WriteLRCNoDowngrade replaces; a forced writer changes nothing.
func TestWriteLRCIfAbsent(t *testing.T) {
	instrumental, _, _ := rungSongs()
	instrumental.WinningLane = TitleRuleLaneName
	const detectorMarker = "[by:canticle]\n[source:canticle-detector]\n[dv:9.9.9]\n" + InstrumentalMarker + "\n"
	for name, tc := range map[string]struct {
		file, body string
		onDisk     Rung
	}{
		"tagged marker":   {"song.txt", detectorMarker, RungInstrumental},
		"untagged marker": {"song.txt", InstrumentalMarker + "\n", RungInstrumental},
		"plain words":     {"song.txt", "words\n", RungUnsynced},
		"synced lyric":    {"song.lrc", "[00:01.00]line\n", RungLine},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, tc.file)
			seed(t, p, tc.body)
			w := NewLRCWriter()
			w.SetForceOverwrite(true)
			var k *KeptError
			if err := w.WriteLRCIfAbsent(instrumental, "song.lrc", dir); !errors.As(err, &k) || k.OnDisk != tc.onDisk {
				t.Fatalf("err = %v, want a *KeptError on rung %d", err, tc.onDisk)
			}
			if got := readText(t, p); got != tc.body {
				t.Errorf("%s changed: %q", tc.file, got)
			}
		})
	}
	dir := t.TempDir()
	if err := NewLRCWriter().WriteLRCIfAbsent(instrumental, "song.lrc", dir); err != nil {
		t.Fatalf("empty stem: %v", err)
	}
	if got := readText(t, filepath.Join(dir, "song.txt")); !strings.Contains(got, "[source:title-rule]") {
		t.Errorf("empty stem: marker = %q", got)
	}
}

// TestStemOccupant_SimulatedCaseSensitive drives the absence check through a
// constructed listing, so it does not depend on the host filesystem: the
// entries name an extension-case variant while the directory the paths resolve
// in is EMPTY, which is what a case-sensitive directory looks like to an exact
// os.Lstat (a case-insensitive host would find "song.TXT" under "song.txt" and
// hide the difference). The variant cannot be read there, so its rung is the
// unjudged keep-it one; what is pinned is seen-or-not. classifyOnDisk is pinned
// beside it: the ordinary guard judges only what a write destroys, and a write
// to song.txt does not destroy song.TXT.
func TestStemOccupant_SimulatedCaseSensitive(t *testing.T) {
	for name, tc := range map[string]struct {
		onDisk, target string
		want           Rung // stemOccupant
		ordinary       Rung // classifyOnDisk, unchanged by #1468
	}{
		"same extension, txt":     {"song.TXT", "song.txt", RungUnsynced, RungNone},
		"same extension, lrc":     {"song.LRC", "song.lrc", RungWord, RungNone},
		"opposite extension, lrc": {"song.LRC", "song.txt", RungWord, RungWord},
		"opposite extension, txt": {"song.Txt", "song.lrc", RungUnsynced, RungUnsynced},
		"companion":               {"song.ELRC", "song.txt", RungWord, RungNone},
		"another stem":            {"Song.TXT", "song.txt", RungNone, RungNone},
		"not a sidecar":           {"song.TXT.bak", "song.txt", RungNone, RungNone},
	} {
		t.Run(name, func(t *testing.T) {
			src, dir := t.TempDir(), t.TempDir()
			seed(t, filepath.Join(src, tc.onDisk), "words\n")
			entries, err := os.ReadDir(src)
			if err != nil {
				t.Fatal(err)
			}
			l, fp := sidecar.ListEntries(dir, entries), filepath.Join(dir, tc.target)
			if got := stemOccupant(fp, l); got.OnDisk != tc.want {
				t.Errorf("stemOccupant = rung %d, want %d", got.OnDisk, tc.want)
			}
			if got := classifyOnDisk(fp, l, nil); got.OnDisk != tc.ordinary {
				t.Errorf("classifyOnDisk = rung %d, want %d (the ordinary guard must not change)", got.OnDisk, tc.ordinary)
			}
		})
	}
}

// TestWriteLRCIfAbsent_OrphanCompanion: a word-timing companion with no .lrc
// beside it occupies the stem. The ordinary guard ranks it RungNone (an owned
// one lifts nothing and is removed by the write, a foreign one is left alone),
// so only the absence check keeps the write off the stem.
func TestWriteLRCIfAbsent_OrphanCompanion(t *testing.T) {
	if !sidecar.Active(sidecar.KindWordSynced) {
		t.Skip("companion inactive")
	}
	instrumental, _, _ := rungSongs()
	for name, body := range map[string]string{
		"owned":   "[by:canticle]\n[00:01.00]<00:01.00>line\n",
		"foreign": "[00:01.00]<00:01.00>line\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			elrc := filepath.Join(dir, "song.elrc")
			seed(t, elrc, body)
			var k *KeptError
			if err := NewLRCWriter().WriteLRCIfAbsent(instrumental, "song.lrc", dir); !errors.As(err, &k) || k.OnDisk == RungNone {
				t.Fatalf("err = %v, want a *KeptError above RungNone", err)
			}
			if got := readText(t, elrc); got != body {
				t.Errorf("companion changed: %q", got)
			}
			mustNotExist(t, filepath.Join(dir, "song.txt"))
		})
	}
}

// TestWriteLRCIfAbsent_CaseVariant is the end-to-end form on a filesystem where
// "song.TXT" and "song.txt" are two files (it cannot fail on a case-insensitive
// one, hence the skip; TestStemOccupant_SimulatedCaseSensitive is the
// host-independent half). Every extension-case variant of the stem refuses the
// write and stays byte for byte, and no second lyric file appears beside it.
// The last block pins the ordinary guard: WriteLRCNoDowngrade still lands a
// marker at song.txt beside an untouched song.TXT, exactly as before #1468.
func TestWriteLRCIfAbsent_CaseVariant(t *testing.T) {
	if !caseSensitiveFS(t, t.TempDir()) {
		t.Skip("filesystem is case-insensitive; song.txt and song.TXT are one file there")
	}
	instrumental, _, line := rungSongs()
	for name, tc := range map[string]struct {
		file, body string
		song       models.Song
		target     string // what the write would have created
		onDisk     Rung
	}{
		"same extension, words":     {"song.TXT", "words\n", instrumental, "song.txt", RungUnsynced},
		"same extension, marker":    {"song.TXT", InstrumentalMarker + "\n", instrumental, "song.txt", RungInstrumental},
		"opposite extension, lyric": {"song.LRC", "[00:01.00]line\n", instrumental, "song.txt", RungLine},
		"same extension, lyric":     {"song.LRC", "[00:01.00]line\n", line, "song.lrc", RungLine},
		"opposite extension, words": {"song.TXT", "words\n", line, "song.lrc", RungUnsynced},
		"foreign companion":         {"song.ELRC", "[00:01.00]<00:01.00>line\n", instrumental, "song.txt", RungWord},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, tc.file)
			seed(t, p, tc.body)
			var k *KeptError
			if err := NewLRCWriter().WriteLRCIfAbsent(tc.song, "song.lrc", dir); !errors.As(err, &k) || k.OnDisk != tc.onDisk {
				t.Fatalf("err = %v, want a *KeptError on rung %d", err, tc.onDisk)
			}
			if got := readText(t, p); got != tc.body {
				t.Errorf("%s changed: %q", tc.file, got)
			}
			mustNotExist(t, filepath.Join(dir, tc.target))
		})
	}

	dir := t.TempDir()
	upper := filepath.Join(dir, "song.TXT")
	seed(t, upper, "words\n")
	if err := NewLRCWriter().WriteLRCNoDowngrade(instrumental, "song.lrc", dir); err != nil {
		t.Fatalf("WriteLRCNoDowngrade beside song.TXT: %v", err)
	}
	if got := readText(t, upper); got != "words\n" {
		t.Errorf("song.TXT changed: %q", got)
	}
	mustExist(t, filepath.Join(dir, "song.txt"))
}

// TestOccupantAgreesWithWriteLRCIfAbsent: Occupant is the read-only twin of
// WriteLRCIfAbsent, so for every state of the stem it must name exactly the
// file that write would keep (or nothing where the write would land), and it
// must leave the directory untouched. The worker probes every output with it
// before the first write (#1468); a disagreement would let a marker land beside
// a lyric at another output.
func TestOccupantAgreesWithWriteLRCIfAbsent(t *testing.T) {
	instrumental, _, _ := rungSongs()
	instrumental.WinningLane = TitleRuleLaneName
	manual := instrumental
	manual.WinningLane = ManualLaneName
	for name, files := range map[string]map[string]string{
		"empty stem":       {},
		"another stem":     {"other.txt": "words\n"},
		"marker":           {"song.txt": InstrumentalMarker + "\n"},
		"plain words":      {"song.txt": "words\n"},
		"synced lyric":     {"song.lrc": "[00:01.00]line\n"},
		"lyric and words":  {"song.lrc": "[00:01.00]line\n", "song.txt": "words\n"},
		"orphan companion": {"song.elrc": "[by:canticle]\n[00:01.00]<00:01.00>line\n"},
		"manual marker":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			if name == "orphan companion" && !sidecar.Active(sidecar.KindWordSynced) {
				t.Skip("companion inactive")
			}
			dir := t.TempDir()
			w := NewLRCWriter()
			for f, body := range files {
				seed(t, filepath.Join(dir, f), body)
			}
			if files == nil {
				if err := w.WriteManualMarker(manual, "song.lrc", dir); err != nil {
					t.Fatalf("seed manual marker: %v", err)
				}
			}
			before := dirSnapshot(t, dir)
			got, err := w.Occupant(instrumental, "song.lrc", dir)
			if err != nil {
				t.Fatalf("Occupant: %v", err)
			}
			if after := dirSnapshot(t, dir); after != before {
				t.Fatalf("Occupant changed the directory:\n%s\nwant:\n%s", after, before)
			}
			var want *KeptError
			if werr := w.WriteLRCIfAbsent(instrumental, "song.lrc", dir); werr != nil && !errors.As(werr, &want) {
				t.Fatalf("WriteLRCIfAbsent: %v", werr)
			}
			if (got == nil) != (want == nil) || (got != nil && *got != *want) {
				t.Fatalf("Occupant = %#v; WriteLRCIfAbsent kept %#v", got, want)
			}
			if wantOccupied := name != "empty stem" && name != "another stem"; (got != nil) != wantOccupied {
				t.Errorf("occupied = %v; want %v", got != nil, wantOccupied)
			}
		})
	}
	// A directory the write would refuse as missing is the same error here, not
	// "empty": the caller's missing-directory handling depends on telling them apart.
	root := t.TempDir()
	if k, err := NewLRCWriter(root).Occupant(instrumental, "song.lrc", filepath.Join(root, "gone")); k != nil || !errors.Is(err, ErrOutputDirMissing) {
		t.Errorf("missing directory: Occupant = %#v, %v; want nil, ErrOutputDirMissing", k, err)
	}
}

// dirSnapshot is every entry of dir with its content, in name order.
func dirSnapshot(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.Name() + "\n" + readText(t, filepath.Join(dir, e.Name())) + "\n")
	}
	return b.String()
}
