package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/sydlexius/canticle/internal/ffmpeg"
)

// durationTolerance is how far (in whole seconds) a generated file's measured
// duration may drift from the listed length. The accept-time timing guard judges
// a synced lyric against the AUDIO duration, so a fixture that is materially
// short would demote a correct .lrc; one second absorbs MP3 frame padding and
// the whole-second truncation of the duration reader.
const durationTolerance = 1

// Duration is a track length in whole seconds. In TOML it is either an integer
// number of seconds (225) or an "m:ss" string ("3:45").
type Duration int

// UnmarshalTOML implements toml.Unmarshaler.
func (d *Duration) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case int64:
		// Non-positive values are rejected by LoadTracks, the only caller.
		*d = Duration(x)
		return nil
	case string:
		secs, err := parseDuration(x)
		if err != nil {
			return err
		}
		*d = Duration(secs)
		return nil
	default:
		return fmt.Errorf("duration must be seconds (integer) or \"m:ss\" (string), got %T", v)
	}
}

// parseDuration parses "225" or "3:45" into whole seconds.
func parseDuration(s string) (int, error) {
	s = strings.TrimSpace(s)
	var total int
	if m, sec, ok := strings.Cut(s, ":"); ok {
		mins, err := strconv.Atoi(m)
		if err != nil || mins < 0 {
			return 0, fmt.Errorf("duration %q: bad minutes", s)
		}
		secs, err := strconv.Atoi(sec)
		if err != nil || len(sec) != 2 || secs < 0 || secs > 59 {
			return 0, fmt.Errorf("duration %q: seconds must be two digits 00-59", s)
		}
		total = mins*60 + secs
	} else {
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("duration %q: want seconds or m:ss", s)
		}
		total = n
	}
	if total <= 0 {
		return 0, fmt.Errorf("duration %q must be positive", s)
	}
	return total, nil
}

// Track is one fixture: the ID3 tags and the exact length of the silent file.
type Track struct {
	Artist   string   `toml:"artist"`
	Album    string   `toml:"album"`
	Title    string   `toml:"title"`
	Duration Duration `toml:"duration"`
}

type trackFile struct {
	Track []Track `toml:"track"`
}

// ControlTrack is the nonsense-tagged negative control. It must end as a miss,
// never a lyric: a result for it is the decoy-payload failure #939 fixed.
var ControlTrack = Track{
	Artist:   "Qzxv Vrmptlo",
	Album:    "Nonexistent Fixture Album",
	Title:    "Wqlbz Jhrvnak Control",
	Duration: 180,
}

// LoadTracks reads a [[track]] TOML list. Unknown keys and incomplete entries
// are errors, so a typo cannot silently produce an untagged fixture.
func LoadTracks(path string) ([]Track, error) {
	var tf trackFile
	meta, err := toml.DecodeFile(path, &tf)
	if err != nil {
		return nil, fmt.Errorf("read track list %s: %w", path, err)
	}
	if und := meta.Undecoded(); len(und) > 0 {
		return nil, fmt.Errorf("track list %s: unknown keys %v", path, und)
	}
	if len(tf.Track) == 0 {
		return nil, fmt.Errorf("track list %s: no [[track]] entries", path)
	}
	for i, t := range tf.Track {
		if strings.TrimSpace(t.Artist) == "" || strings.TrimSpace(t.Album) == "" || strings.TrimSpace(t.Title) == "" {
			return nil, fmt.Errorf("track list %s: entry %d needs artist, album and title", path, i+1)
		}
		if t.Duration <= 0 {
			return nil, fmt.Errorf("track list %s: entry %d needs a duration", path, i+1)
		}
		if isControlIdentity(t) {
			return nil, fmt.Errorf("track list %s: entry %d uses the negative control's artist and title (%q / %q), which are reserved; the control is always added by the tool, so remove the entry", path, i+1, ControlTrack.Artist, ControlTrack.Title)
		}
	}
	return tf.Track, nil
}

// isControlIdentity reports whether t carries the control's artist and title
// (case-insensitively). That identity is reserved: LoadTracks rejects it, so the
// one file bearing it is always the canonical ControlTrack.
func isControlIdentity(t Track) bool {
	return strings.EqualFold(strings.TrimSpace(t.Artist), ControlTrack.Artist) &&
		strings.EqualFold(strings.TrimSpace(t.Title), ControlTrack.Title)
}

// WithControl returns tracks plus the canonical ControlTrack, always appended
// (LoadTracks has already rejected any entry claiming the control's identity, so
// there is nothing to de-duplicate). The input slice is never modified.
func WithControl(tracks []Track) []Track {
	return append(append([]Track(nil), tracks...), ControlTrack)
}

// canticleModule is the module path whose go.mod marks a canticle checkout.
const canticleModule = "github.com/sydlexius/canticle"

// RefuseInsideRepo returns an error when out resolves inside any git work tree
// enclosing cwd (every ancestor holding a .git entry, so a worktree nested in the
// main checkout guards both), or inside any ancestor of out holding canticle's
// go.mod (so a checkout is guarded even when cwd is elsewhere). Containment is
// judged by file identity (os.SameFile) as well as lexically, so a symlink or a
// case-only spelling difference on a case-insensitive filesystem cannot escape.
func RefuseInsideRepo(out, cwd string) error {
	if !filepath.IsAbs(out) {
		out = filepath.Join(cwd, out)
	}
	target := resolveExisting(filepath.Clean(out))
	for dir := resolveExisting(filepath.Clean(cwd)); ; dir = filepath.Dir(dir) {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil && sameOrUnder(target, dir) {
			return refusal(out, dir)
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	for dir := nearestExisting(target); ; dir = filepath.Dir(dir) {
		if isCanticleModule(dir) {
			return refusal(out, dir)
		}
		if filepath.Dir(dir) == dir {
			return nil
		}
	}
}

func refusal(out, dir string) error {
	return fmt.Errorf("refusing to write fixtures to %s: it is inside the repository %s; pick a directory outside it (default /tmp/canticle-smoke-fixtures)", out, dir)
}

// nearestExisting returns p or its closest ancestor that exists.
func nearestExisting(p string) string {
	for cur := p; ; cur = filepath.Dir(cur) {
		if _, err := os.Stat(cur); err == nil || filepath.Dir(cur) == cur {
			return cur
		}
	}
}

// sameOrUnder reports whether p is root or a descendant of it: lexically, or by
// file identity of any existing ancestor of p.
func sameOrUnder(p, root string) bool {
	if within(p, root) {
		return true
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false
	}
	for cur := nearestExisting(p); ; cur = filepath.Dir(cur) {
		if fi, err := os.Stat(cur); err == nil && os.SameFile(fi, rootInfo) {
			return true
		}
		if filepath.Dir(cur) == cur {
			return false
		}
	}
}

// isCanticleModule reports whether dir holds a go.mod declaring canticleModule.
func isCanticleModule(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "go.mod")) //nolint:gosec // reason: G304 -- reads only a go.mod in an ancestor of the operator's own output path, to refuse writing there
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "module" && strings.Trim(f[1], `"`) == canticleModule {
			return true
		}
	}
	return false
}

// ManifestName is the file Generator.Run keeps in the output directory listing
// the stem of every fixture it wrote. It is the ONLY ownership record -clean
// trusts: a file name that merely looks like a fixture proves nothing.
const ManifestName = ".smokefixtures-manifest.json"

// ownedExts are the same-stem files -clean removes for each manifest stem: the
// fixture itself and the sidecars serve writes for it.
var ownedExts = []string{".mp3", ".lrc", ".txt", ".lrc.orig"}

type manifest struct {
	Stems []string `json:"stems"`
}

// readManifest loads dir's manifest. A missing manifest returns fs.ErrNotExist
// (wrapped). Every stem must be a plain file name, so a hand-edited or corrupt
// manifest cannot direct a removal outside dir.
func readManifest(dir string) ([]string, error) {
	p := filepath.Join(dir, ManifestName)
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, fmt.Errorf("manifest %s: %w", p, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("manifest %s is not a regular file", p)
	}
	b, err := os.ReadFile(p) //nolint:gosec // reason: G304 -- reads the tool's own manifest in the operator-chosen output dir, confirmed a regular file by Lstat above
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", p, err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", p, err)
	}
	for _, st := range m.Stems {
		if st == "" || st == "." || st == ".." || strings.ContainsAny(st, `/\`) || filepath.Base(st) != st {
			return nil, fmt.Errorf("manifest %s: invalid stem %q", p, st)
		}
	}
	return m.Stems, nil
}

// writeManifest atomically replaces dir's manifest with stems (sorted, unique):
// a temp file in dir, fsynced, then renamed over the old one, so a crash leaves
// either the previous manifest or the new one, never a torn file.
func writeManifest(dir string, stems []string) error {
	uniq := map[string]bool{}
	for _, st := range stems {
		uniq[st] = true
	}
	m := manifest{Stems: make([]string, 0, len(uniq))}
	for st := range uniq {
		m.Stems = append(m.Stems, st)
	}
	sort.Strings(m.Stems)
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	f, err := os.CreateTemp(dir, ManifestName+".tmp-*")
	if err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	tmp := f.Name()
	_, werr := f.Write(append(b, '\n'))
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, filepath.Join(dir, ManifestName))
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write manifest: %w", werr)
	}
	return nil
}

// PrepareOut makes dir safe to generate into. A missing or empty dir is fine. A
// non-empty one is refused unless clean, because a stale .lrc there makes the
// scanner skip its track and the smoke pass on old results. With clean, ownership
// comes only from the manifest a previous run wrote: for each listed stem the
// .mp3 and every same-stem sidecar in ownedExts that exists as a regular file is
// removed, whether or not the .mp3 is still there (an orphaned sidecar is exactly
// the stale file that would fake a pass), and then the manifest itself. Nothing
// unlisted is touched, symlinks are never followed and subdirectories never
// entered. A non-empty dir with no manifest is refused: nothing in it is provably
// this tool's.
func PrepareOut(dir string, clean bool) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	if len(entries) == 0 {
		return nil
	}
	if !clean {
		return fmt.Errorf("%s is not empty: stale sidecars there would make the smoke pass on old results; pass -clean (make smoke-fixtures CLEAN=1) to replace the fixtures this tool wrote, or pick an empty directory", dir)
	}
	stems, err := readManifest(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s is not empty and has no %s: nothing there is provably a fixture this tool wrote, so -clean will not guess; empty it yourself or pick an empty directory", dir, ManifestName)
	}
	if err != nil {
		return err
	}
	for _, stem := range stems {
		for _, ext := range ownedExts {
			p := filepath.Join(dir, stem+ext)
			if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
				continue
			}
			if err := os.Remove(p); err != nil {
				return fmt.Errorf("clean %s: %w", p, err)
			}
		}
	}
	if err := os.Remove(filepath.Join(dir, ManifestName)); err != nil {
		return fmt.Errorf("clean manifest: %w", err)
	}
	return nil
}

// resolveExisting evaluates symlinks on the longest existing prefix of p and
// re-attaches the rest.
func resolveExisting(p string) string {
	var tail []string
	for cur := p; ; cur = filepath.Dir(cur) {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			parts := append([]string{r}, tail...)
			return filepath.Join(parts...)
		}
		if filepath.Dir(cur) == cur {
			return p
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
	}
}

// within reports whether p is root or a descendant of it.
func within(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Generator writes and verifies fixtures. Encode and ReadDuration are seams so
// the verification logic is testable without ffmpeg.
type Generator struct {
	Encode       func(ctx context.Context, t Track, path string) error
	ReadDuration func(path string) (int, error)
}

// Run writes one file per track into outDir and verifies each duration,
// returning the written paths. A mismatch beyond durationTolerance is an error.
//
// Each stem is recorded in the manifest (ManifestName) BEFORE its file is
// encoded, rewritten atomically every time, so even a run that crashes or fails
// verification mid-way leaves every file it may have created listed and a later
// -clean can remove it. Stems from a manifest already in outDir are kept, so a
// file this tool owns never drops out of the record.
func (g Generator) Run(ctx context.Context, tracks []Track, outDir string) ([]string, error) {
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return nil, fmt.Errorf("create %s: %w", outDir, err)
	}
	stems, err := readManifest(outDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	paths := make([]string, 0, len(tracks))
	for i, t := range tracks {
		stem := fmt.Sprintf("%02d - %s - %s", i+1, safeName(t.Artist), safeName(t.Title))
		p := filepath.Join(outDir, stem+".mp3")
		stems = append(stems, stem)
		if err := writeManifest(outDir, stems); err != nil {
			return paths, err
		}
		if err := g.Encode(ctx, t, p); err != nil {
			return paths, fmt.Errorf("encode %s: %w", p, err)
		}
		got, err := g.ReadDuration(p)
		if err != nil {
			return paths, fmt.Errorf("verify %s: %w", p, err)
		}
		if diff := got - int(t.Duration); diff > durationTolerance || diff < -durationTolerance {
			return paths, fmt.Errorf("verify %s: measured duration %ds, want %ds (tolerance %ds)", p, got, int(t.Duration), durationTolerance)
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// safeName makes a tag value usable as a filename component.
func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(s))
	return strings.Trim(s, ". ")
}

// ffmpegEncoder returns an Encode func writing a silent mono 32 kbps CBR MP3 of
// exactly the track's length, with ID3v2.3 artist/album/title tags.
func ffmpegEncoder(bin string) func(ctx context.Context, t Track, path string) error {
	return func(ctx context.Context, t Track, path string) error {
		args := []string{
			"-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "anullsrc=r=44100:cl=mono",
			"-t", strconv.Itoa(int(t.Duration)),
			"-c:a", "libmp3lame", "-b:a", "32k",
			"-id3v2_version", "3",
			"-metadata", "artist=" + t.Artist,
			"-metadata", "album=" + t.Album,
			"-metadata", "title=" + t.Title,
			path,
		}
		cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // reason: G204 -- bin comes from ffmpeg.Resolve (flag override, pinned cache, or PATH) and args are a fixed argv with tag values passed as single arguments, never through a shell
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("ffmpeg: %w: %s", err, ffmpeg.BoundOutput(string(out)))
		}
		return nil
	}
}

// errNoTracks is returned when the default track list is missing, with a hint.
var errNoTracks = errors.New("track list not found; copy scripts/smoke-fixtures.example.toml to smoke-fixtures.local.toml (gitignored) and fill in real tracks")
