package lyrics

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/selfwrite"
	"github.com/sydlexius/canticle/internal/timing"
)

// MaxEditOffsetMS bounds a hand-entered offset (10 minutes). A larger value
// is a typo, never a timing correction; callers refuse it before writing.
const MaxEditOffsetMS = 600_000

// ShiftLines returns a copy of lines with offsetMS added to every line start
// (#481 Stage 2). A start shifted below zero clamps to zero: intro lines pin to
// the start of the track rather than vanishing. Word timings are NOT shifted:
// the editor only accepts line-synced files, which carry none.
func ShiftLines(lines []TimedLine, offsetMS int) []TimedLine {
	out := make([]TimedLine, len(lines))
	for i, l := range lines {
		l.StartMS += offsetMS
		if l.StartMS < 0 {
			l.StartMS = 0
		}
		out[i] = l
	}
	return out
}

// EditOptions configures ApplyEdit.
type EditOptions struct {
	Roots           []string            // library roots; the .lrc must sit inside one
	ExpectMTime     time.Time           // the mtime the page loaded with; zero skips the check (tests only)
	DurationSeconds int                 // exact audio duration; <= 0 means unknown (fails open)
	SelfWrites      *selfwrite.Registry // nil-safe
}

// EditResult reports what ApplyEdit did.
type EditResult struct {
	CreatedOrig bool      // this call wrote <path>.orig
	NewMTime    time.Time // the rewritten file's mtime
}

var (
	// ErrEditChanged means the file's mtime differs from the one the editor loaded.
	ErrEditChanged = errors.New("lyrics: file changed since it was loaded")
	// ErrEditTiming means the edited lines fail the timing guard.
	ErrEditTiming = errors.New("lyrics: edited timing fails the timing guard")
	// ErrEditRefused means the path is not a regular, non-symlink .lrc inside a library root.
	ErrEditRefused = errors.New("lyrics: path is not an editable .lrc inside a library root")
)

// confineEdit resolves path beneath one of roots (lexically, over both the
// absolute and the symlink-resolved spelling of the root, as the preview
// player's openPreviewAudio does) and returns the canonical root plus the
// root-relative name. Every later filesystem touch goes through an os.Root on
// that directory, which refuses any escaping component.
func confineEdit(path string, roots []string) (canon, rel string, err error) {
	if !strings.EqualFold(filepath.Ext(path), ".lrc") || !filepath.IsAbs(path) {
		return "", "", ErrEditRefused
	}
	path = filepath.Clean(path)
	for _, root := range roots {
		if root == "" {
			continue
		}
		abs, c := pathutil.CanonicalRoot(root)
		for _, spelling := range []string{abs, c} {
			r, rerr := filepath.Rel(spelling, path)
			if rerr == nil && filepath.IsLocal(r) {
				return c, r, nil
			}
		}
	}
	return "", "", ErrEditRefused
}

// lstatRegular reports rel's Lstat info, requiring a regular file (so a
// symlink, FIFO or directory is refused, never followed).
func lstatRegular(root *os.Root, rel string) (fs.FileInfo, error) {
	fi, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, ErrEditRefused
	}
	return fi, nil
}

// readRegular reads rel through root, verifying the opened handle is the file
// Lstat saw (a swap between the two is refused).
func readRegular(root *os.Root, rel string, want fs.FileInfo) ([]byte, error) {
	f, err := root.Open(rel)
	if err != nil {
		return nil, fmt.Errorf("opening lyrics file: %w", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !os.SameFile(fi, want) {
		return nil, ErrEditRefused
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("reading lyrics file: %w", err)
	}
	return b, nil
}

// OriginalLines returns the lines an edit applies to: <path>.orig when it is a
// regular file, else <path>, parsed through ParseTimedLRC (which expands
// stacked [t1][t2] timestamps), plus the raw header tag lines. Same
// confinement as ApplyEdit.
func OriginalLines(path string, roots []string) ([]TimedLine, []string, error) {
	canon, rel, err := confineEdit(path, roots)
	if err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(canon)
	if err != nil {
		return nil, nil, fmt.Errorf("opening library root: %w", err)
	}
	defer func() { _ = root.Close() }()
	cur, err := lstatRegular(root, rel)
	if err != nil {
		return nil, nil, refuseOrWrap(err)
	}
	src, srcFI := rel, cur
	if ofi, oerr := lstatRegular(root, rel+".orig"); oerr == nil {
		src, srcFI = rel+".orig", ofi
	}
	body, err := readRegular(root, src, srcFI)
	if err != nil {
		return nil, nil, err
	}
	doc := ParseTimedLRC(string(body))
	tags := make([]string, 0, len(doc.Tags))
	for _, tg := range doc.Tags {
		tags = append(tags, tg.Raw)
	}
	return doc.Lines, tags, nil
}

func refuseOrWrap(err error) error {
	if errors.Is(err, ErrEditRefused) || errors.Is(err, fs.ErrNotExist) {
		return ErrEditRefused
	}
	return err
}

// ApplyEdit rewrites the .lrc at path with lines (#481 Stage 2), backup-first:
// the pre-edit bytes are copied to <path>.orig exactly once, and an existing
// .orig is never touched. It refuses a symlink or out-of-root path, a file
// whose mtime moved since load, and edited timing the guard rejects. The
// rewrite is atomic and recorded with SelfWrites so the watcher drops it.
func ApplyEdit(path string, lines []TimedLine, headerTags []string, opts EditOptions) (EditResult, error) {
	canon, rel, err := confineEdit(path, opts.Roots)
	if err != nil {
		return EditResult{}, err
	}
	root, err := os.OpenRoot(canon)
	if err != nil {
		return EditResult{}, fmt.Errorf("opening library root: %w", err)
	}
	defer func() { _ = root.Close() }()
	fi, err := lstatRegular(root, rel)
	if err != nil {
		return EditResult{}, refuseOrWrap(err)
	}
	if !opts.ExpectMTime.IsZero() && !fi.ModTime().Equal(opts.ExpectMTime) {
		return EditResult{}, ErrEditChanged
	}
	song := models.Song{}
	for _, l := range lines {
		song.Subtitles.Lines = append(song.Subtitles.Lines, models.Lines{Text: l.Text, Time: models.MsToTime(l.StartMS)})
	}
	if out, _ := timing.Evaluate(song, opts.DurationSeconds); out != timing.Ok && out != timing.UnknownDuration {
		return EditResult{}, fmt.Errorf("%w: %v", ErrEditTiming, out)
	}

	dir := filepath.Join(canon, filepath.Dir(rel))
	base := filepath.Base(rel)
	var res EditResult
	if _, oerr := root.Lstat(rel + ".orig"); errors.Is(oerr, fs.ErrNotExist) {
		cur, rerr := readRegular(root, rel, fi)
		if rerr != nil {
			return EditResult{}, rerr
		}
		if werr := writeAtomic(dir, base+".orig", nil, func(w *bufio.Writer) error {
			if _, e := w.Write(cur); e != nil {
				return e
			}
			return w.Flush()
		}); werr != nil {
			return EditResult{}, fmt.Errorf("writing .orig backup: %w", werr)
		}
		res.CreatedOrig = true
		opts.SelfWrites.Record(path + ".orig")
	} else if oerr != nil {
		return EditResult{}, fmt.Errorf("checking .orig backup: %w", oerr)
	}

	opts.SelfWrites.Record(path)
	if err := writeAtomic(dir, base, headerTags, func(w *bufio.Writer) error {
		for _, l := range song.Subtitles.Lines {
			text := l.Text
			if text == "" {
				text = "♪"
			}
			if _, e := w.WriteString("[" + l.Time.Stamp() + "]" + text + "\n"); e != nil {
				return e
			}
		}
		return w.Flush()
	}); err != nil {
		return EditResult{}, fmt.Errorf("writing edited lyrics: %w", err)
	}
	nfi, err := root.Lstat(rel)
	if err != nil {
		return EditResult{}, fmt.Errorf("stat edited lyrics: %w", err)
	}
	res.NewMTime = nfi.ModTime()
	return res, nil
}
