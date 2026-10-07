package lyrics

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/lrcnormalize"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/selfwrite"
	"github.com/sydlexius/canticle/internal/sidecar"
	"github.com/sydlexius/canticle/internal/timing"
)

// MaxEditOffsetMS bounds a hand-entered offset (10 minutes). A larger value
// is a typo, never a timing correction; callers refuse it before writing.
const MaxEditOffsetMS = 600_000

// maxEditFileSize caps what the editor reads (16 MiB, the lrcbackfill bound):
// a request-triggered read must never pull an arbitrarily large file into memory.
const maxEditFileSize = 16 * 1024 * 1024

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

// isOffsetTag reports whether a header line is an [offset:] ID tag, classified
// by lrcnormalize like every other tag (no second parser).
func isOffsetTag(line string) (value string, ok bool) {
	for _, tg := range lrcnormalize.ParseBody(line).Tags {
		if strings.EqualFold(strings.TrimSpace(tg.Key), "offset") {
			return tg.Value, true
		}
	}
	return "", false
}

// WithOffsetTag returns headerTags with an [offset:<ms>] tag recording shiftMS,
// the shift the editor REQUESTED (#1385), summed with any value the original
// already carried. It REPLACES an existing [offset:] in place, never stacking a
// second; with none it appends one. A zero shiftMS returns the tags untouched.
//
// The header is the requested shift, not a measurement of what moved: when
// ShiftLines clamps a line at zero (a large negative shift) that line moved
// less than the header says, so a clamped save is a lossy edit and the header
// then matches no single line's movement.
//
// SIGN CONVENTION: positive means the timestamps were moved LATER (the same
// sign as ShiftLines and the editor's offset field), so the header is exactly
// the number applied, with no inversion to get wrong. The LRC convention most
// players follow is the opposite (positive = lyrics appear sooner), Symfonium
// may read it the other way again, and Emby is unverified (#1240).
//
// RESIDUAL DOUBLE-APPLY RISK: this is a record, not an instruction. The stamps
// are already shifted, so a player that also honors [offset:] shifts a second
// time (or, under the common convention, undoes the edit). That cannot be
// prevented from inside the file; it is accepted because the alternative
// (omitting the record) leaves the applied correction invisible to a reader.
func WithOffsetTag(headerTags []string, shiftMS int) []string {
	if shiftMS == 0 {
		return headerTags
	}
	out := make([]string, 0, len(headerTags)+1)
	at, total := -1, shiftMS
	for _, tag := range headerTags {
		v, ok := isOffsetTag(tag)
		if !ok {
			out = append(out, tag)
			continue
		}
		if at < 0 {
			at = len(out)
			out = append(out, "")
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				total += n
			}
		}
	}
	tagLine := "[offset:" + strconv.Itoa(total) + "]"
	if at < 0 {
		return append(out, tagLine)
	}
	out[at] = tagLine
	return out
}

// dropOffsetTags removes every [offset:] tag: a retimed file's stamps no longer
// relate to the recorded shift.
func dropOffsetTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if _, ok := isOffsetTag(t); !ok {
			out = append(out, t)
		}
	}
	return out
}

// EditOptions configures ApplyEdit.
type EditOptions struct {
	Roots           []string            // library roots; the .lrc must sit inside one
	ExpectMTime     time.Time           // the mtime the page loaded with; zero skips the check (tests only)
	DurationSeconds int                 // exact audio duration; <= 0 means unknown (fails open)
	SelfWrites      *selfwrite.Registry // nil-safe
	// Generated marks an accepted aligner suggestion (#1008); nil is the hand
	// edit, byte for byte.
	Generated *GeneratedEdit
}

// TimingAligner is the [timing:] header value of a .lrc whose stamps came from
// the aligner sidecar (#1008). [source:] names the lane that served the WORDS
// and purge compares it to the row's lane, so it is never rewritten.
const TimingAligner = "canticle-aligner"

// maxRetimeMS bounds a generated stamp (24 hours); beyond it is not a track.
const maxRetimeMS = 24 * 60 * 60 * 1000

// GeneratedEdit describes an accepted aligner suggestion. Words, Inline and
// Companion are validated but NOT written yet: this slice writes line stamps
// only, and word marks are written by the word-mark accept slice of #1008.
type GeneratedEdit struct {
	Words []models.WordTiming // per-word starts, ordered by Line then time
	// WordsFor, when set, supplies Words from the current file's lines.
	WordsFor  func(cur []TimedLine) ([]models.WordTiming, bool)
	Inline    bool // word marks belong inline in the .lrc
	Companion bool // word marks belong in an .elrc companion
}

// validate checks Words against the lines being written.
func (g *GeneratedEdit) validate(lines []TimedLine) error {
	for i, w := range g.Words {
		switch {
		case w.Line < 0 || w.Line >= len(lines):
			return fmt.Errorf("%w: word %d names no line", ErrEditInvalid, i)
		case w.Text == "" || w.StartMS < 0 || w.StartMS > maxRetimeMS || (w.EndMS != 0 && w.EndMS < w.StartMS):
			return fmt.Errorf("%w: word %d is out of bounds", ErrEditInvalid, i)
		case i > 0 && (w.Line < g.Words[i-1].Line || (w.Line == g.Words[i-1].Line && w.StartMS < g.Words[i-1].StartMS)):
			return fmt.Errorf("%w: word %d is out of order", ErrEditInvalid, i)
		}
	}
	return nil
}

// RetimeLines returns a copy of orig (from CurrentLines) with each start
// replaced by its startsMS value. It refuses a count that is not one per line,
// a negative or absurd value, a decreasing sequence, and a same-stamp group
// (consecutive lines sharing a start) whose members would stop sharing one.
func RetimeLines(orig []TimedLine, startsMS []int) ([]TimedLine, error) {
	if len(startsMS) != len(orig) || len(orig) == 0 {
		return nil, fmt.Errorf("%w: %d stamps for %d lines", ErrEditInvalid, len(startsMS), len(orig))
	}
	out := make([]TimedLine, len(orig))
	for i, l := range orig {
		ms := startsMS[i]
		switch {
		case ms < 0 || ms > maxRetimeMS:
			return nil, fmt.Errorf("%w: stamp %d is out of bounds", ErrEditInvalid, i)
		case i > 0 && ms < startsMS[i-1]:
			return nil, fmt.Errorf("%w: stamp %d goes backwards", ErrEditInvalid, i)
		case i > 0 && l.StartMS == orig[i-1].StartMS && ms != startsMS[i-1]:
			return nil, fmt.Errorf("%w: stamp %d splits a same-stamp group", ErrEditInvalid, i)
		}
		l.StartMS = ms
		out[i] = l
	}
	return out, nil
}

// isWordCompanionName reports whether directory entry name is the word-synced
// companion of the sidecar with base stem: that stem plus ".elrc", any case.
func isWordCompanionName(name, stem string) bool {
	return sidecar.StemOf(name) == stem && sidecar.KindOf(name) == sidecar.KindWordSynced
}

// timingMarker is the header line an accepted generated edit carries.
const timingMarker = "[timing:" + TimingAligner + "]"

// looseWordMarkRe matches an inline word mark in any spelling, including ones
// the parser does not report as words (any fraction after '.', ':' or ',', or
// none; inner padding). Conservative: lyric text like "<16:9>" refuses too.
var looseWordMarkRe = regexp.MustCompile(`<\s*\d+:\d+([.:,]\d+)?\s*>`)

// refuseIfWordTimed is the #1008 predicate "provider word timings are never
// replaced": ErrEditHasWords when the current .lrc carries inline word marks
// (in any spelling) or anything beside it is its .elrc companion (owned or
// foreign, regular file or not). The volume is asked by name, so a companion
// it resolves under another case or normalization refuses too, and the
// sidecar's own directory is listed once through root for an extension-case
// variant on a case-sensitive volume. No entry is followed.
func refuseIfWordTimed(root *os.Root, rel, body string, hasWords bool) error {
	if hasWords {
		return fmt.Errorf("%w: inline word marks", ErrEditHasWords)
	}
	for _, cue := range lrcnormalize.ParseBody(strings.TrimPrefix(body, utf8BOM)).Cues {
		if looseWordMarkRe.MatchString(cue.Text) {
			return fmt.Errorf("%w: inline word marks", ErrEditHasWords)
		}
	}
	if _, err := root.Lstat(sidecar.StemOf(rel) + sidecar.ExtWordSynced); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: a word-synced companion exists", ErrEditHasWords)
	}
	stem := sidecar.StemOf(filepath.Base(rel))
	d, err := root.Open(filepath.Dir(rel))
	if err != nil {
		return fmt.Errorf("opening the sidecar directory: %w", err)
	}
	defer func() { _ = d.Close() }()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("listing the sidecar directory: %w", err)
	}
	for _, name := range names {
		if isWordCompanionName(name, stem) {
			return fmt.Errorf("%w: a word-synced companion exists", ErrEditHasWords)
		}
	}
	return nil
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
	// ErrEditHasWords means a generated edit met word timings it must not replace.
	ErrEditHasWords = errors.New("lyrics: word timings already exist for this file")
	// ErrEditInvalid means generated timings do not fit the file's lines.
	ErrEditInvalid = errors.New("lyrics: generated timings do not fit the lines")
	// ErrEditStaleOrig means the .orig beside the .lrc is not the original of the
	// file on disk, so a rewrite would destroy text, tags or lines no copy holds (#1313).
	ErrEditStaleOrig = errors.New("lyrics: the .orig backup is not the original of the current file")
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
// Lstat saw and unchanged since (a swap is ErrEditRefused, an in-place rewrite
// before or during the read is ErrEditChanged). Outside writers cannot be fully
// serialized without OS file locks: a rewrite after the post-read stat and
// before the caller's atomic rename is not caught. A file over maxEditFileSize is
// refused before anything parses it.
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
	if !sameStamp(fi, want) {
		return nil, ErrEditChanged
	}
	b, err := io.ReadAll(io.LimitReader(f, maxEditFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("reading lyrics file: %w", err)
	}
	if after, serr := f.Stat(); serr != nil || !sameStamp(after, want) {
		return nil, ErrEditChanged
	}
	if len(b) > maxEditFileSize {
		return nil, fmt.Errorf("%w: file exceeds %d bytes", ErrEditRefused, maxEditFileSize)
	}
	return b, nil
}

func sameStamp(a, b fs.FileInfo) bool {
	return a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size()
}

// OriginalLines returns the lines an edit applies to: <path>.orig when it is a
// regular file, else <path>, parsed through ParseTimedLRC (which expands
// stacked [t1][t2] timestamps), plus the raw header tag lines. Same
// confinement as ApplyEdit.
func OriginalLines(path string, roots []string) ([]TimedLine, []string, error) {
	return editLines(path, roots, false, time.Time{})
}

// CurrentLines returns the lines of <path> itself, never its .orig backup, and
// only while the file's mtime is expect (else ErrEditChanged; a zero expect
// never matches). A generated accept validates against these: the mtime binds
// the posted starts to the cues they were computed for, position by position.
func CurrentLines(path string, roots []string, expect time.Time) ([]TimedLine, error) {
	lines, _, err := editLines(path, roots, true, expect)
	return lines, err
}

func editLines(path string, roots []string, current bool, expect time.Time) ([]TimedLine, []string, error) {
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
	if current && !cur.ModTime().Equal(expect) {
		return nil, nil, ErrEditChanged
	}
	src, srcFI := rel, cur
	ofi, oerr := lstatRegular(root, rel+".orig")
	switch {
	case current:
		// The backup plays no part in what a generated accept validates.
	case oerr == nil:
		src, srcFI = rel+".orig", ofi
	case errors.Is(oerr, fs.ErrNotExist):
		// No backup yet: the current file is the original.
	default:
		// A .orig that exists but is not a regular file (symlink, directory,
		// FIFO) is not a usable original; never silently edit the .lrc instead.
		return nil, nil, refuseOrWrap(oerr)
	}
	body, err := readRegular(root, src, srcFI)
	if err != nil {
		return nil, nil, err
	}
	if src != rel {
		if err := requireOrigBacksCurrent(root, rel, cur, body, false); err != nil {
			return nil, nil, err
		}
	}
	doc := ParseTimedLRC(string(body))
	tags := make([]string, 0, len(doc.Tags))
	for _, tg := range doc.Tags {
		tags = append(tags, tg.Raw)
	}
	return doc.Lines, tags, nil
}

// lyricIdentityKeys are the tags that say WHICH fetch of which lyric a sidecar
// holds ([fetched:] differs after a re-fetch); the rest ([re:], [ve:], [by:], [timing:]) canticle may add in place.
var lyricIdentityKeys = map[string]bool{"source": true, "upstream": true, "isrc": true, "mbid": true, "fetched": true}

// ReadEditable reads the sidecar (or .orig backup) at path the way SameLyric
// expects it read: no-follow, regular-checked on the handle, and capped at
// maxEditFileSize. It also returns that handle's FileInfo, so a caller that
// later acts on the path can check that the entry is still the file it read:
// a regular file with the same size and mtime as well as os.SameFile, since a
// freed inode number can be reused by the entry created next. An unreadable
// or non-regular entry is an error.
func ReadEditable(path string) ([]byte, os.FileInfo, error) {
	b, fi, err := readRegularNoFollowInfo(path, maxEditFileSize)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return b, fi, nil
}

// SameLyric reports whether the sidecar bodies a and b hold the same words: as
// many cues (at least one), the same text in order, and no identity tag both carry with
// different values. Stamps are not compared. ParseTimedLRC makes a BOM, CRLF,
// stacked stamps and word marks no difference, and an empty cue equals the "♪"
// ApplyEdit writes for it. It judges bytes, not paths, so a caller that read
// them through ReadEditable acts on exactly the bytes that were judged.
func SameLyric(a, b []byte) bool {
	docs := [2]TimedLRC{ParseTimedLRC(string(a)), ParseTimedLRC(string(b))}
	if len(docs[0].Lines) != len(docs[1].Lines) || len(docs[0].Lines) == 0 {
		return false
	}
	empty := map[string]bool{"": true, "♪": true}
	for i, l := range docs[0].Lines {
		o := docs[1].Lines[i].Text //nolint:gosec // reason: the lengths were compared equal just above
		if l.Text != o && (!empty[l.Text] || !empty[o]) {
			return false
		}
	}
	seen := map[string]string{}
	for i, d := range docs {
		for _, tg := range d.Tags {
			k, v := strings.ToLower(strings.TrimSpace(tg.Key)), strings.TrimSpace(tg.Value)
			if w, ok := seen[k]; i == 1 && ok && !strings.EqualFold(v, w) {
				return false
			}
			if i == 0 && lyricIdentityKeys[k] {
				seen[k] = v
			}
		}
	}
	return true
}

// editSkeleton is what a rewrite by ApplyEdit keeps or discards of a body: its
// header tags (trimmed), its cue texts in order, and every other non-blank
// line (an unstamped credit line, say), which the writer never carries over.
type editSkeleton struct {
	tags, cues, orphans []string
}

func skeletonOf(body []byte) editSkeleton {
	var sk editSkeleton
	type stamped struct {
		total float64
		text  string
	}
	var cues []stamped
	for _, raw := range strings.Split(strings.TrimPrefix(string(body), utf8BOM), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		doc := lrcnormalize.ParseBody(line)
		switch {
		case len(doc.Cues) > 0:
			for _, c := range doc.Cues {
				text := strings.TrimSpace(timing.StripWordMarkers(c.Text))
				if text == "" {
					text = "♪" // ApplyEdit writes an empty cue as a note
				}
				cues = append(cues, stamped{c.Time.Total, text})
			}
		case len(doc.Tags) > 0:
			// The aligner marker and the [offset:] record are canticle's own additions.
			if _, isOff := isOffsetTag(line); line != timingMarker && !isOff {
				sk.tags = append(sk.tags, line)
			}
		default:
			sk.orphans = append(sk.orphans, line)
		}
	}
	// The writer emits cues sorted by stamp (ParseBody, stable), so an original
	// listing them out of order must compare in that order too.
	sort.SliceStable(cues, func(i, j int) bool { return cues[i].total < cues[j].total })
	for _, c := range cues {
		sk.cues = append(sk.cues, c.text)
	}
	return sk
}

// requireOrigBacksCurrent returns ErrEditStaleOrig unless the .orig body is the
// original of the current .lrc: the same tags and cue texts in order, and
// every unstamped line of the current file also present in the .orig. Only
// stamps may differ, which is all an edit changes. With orphansOnly (a
// generated edit, which writes the current file's own text and tags and so
// cannot lose them) only the unstamped-line condition is checked. The current file is read
// through root, pinned to the Lstat info the caller already holds.
func requireOrigBacksCurrent(root *os.Root, rel string, cur fs.FileInfo, orig []byte, orphansOnly bool) error {
	cb, err := readRegular(root, rel, cur)
	if err != nil {
		return err
	}
	c, o := skeletonOf(cb), skeletonOf(orig)
	// A multiset: a line the current file repeats more often than the .orig
	// holds it would lose the extra occurrences with no copy.
	inOrig := map[string]int{}
	for _, l := range o.orphans {
		inOrig[l]++
	}
	same := orphansOnly || (reflect.DeepEqual(c.tags, o.tags) && reflect.DeepEqual(c.cues, o.cues))
	for _, l := range c.orphans {
		inOrig[l]--
		same = same && inOrig[l] >= 0
	}
	if !same {
		return ErrEditStaleOrig
	}
	return nil
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
//
// With opts.Generated set (#1008) it also refuses, before writing anything, a
// file that already has word timings (ErrEditHasWords) or words that do not
// fit lines (ErrEditInvalid), and writes [timing:canticle-aligner] once after
// the other header tags, which are kept in order. Only the STARTS of lines are
// used: the text and header tags written are the current file's own, so a stale
// .orig cannot reach the output. A current file with another cue count than
// lines is refused with ErrEditChanged. Same caller-held locks.
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
	if g := opts.Generated; g != nil {
		cur, rerr := readRegular(root, rel, fi)
		if rerr != nil {
			return EditResult{}, rerr
		}
		doc := ParseTimedLRC(string(cur))
		if err := refuseIfWordTimed(root, rel, string(cur), doc.HasWords); err != nil {
			return EditResult{}, err
		}
		if len(doc.Lines) != len(lines) {
			return EditResult{}, ErrEditChanged
		}
		for i := range doc.Lines {
			doc.Lines[i].StartMS = lines[i].StartMS //nolint:gosec // reason: the lengths were compared equal just above
		}
		lines, headerTags = doc.Lines, nil
		for _, tg := range doc.Tags {
			headerTags = append(headerTags, tg.Raw)
		}
		headerTags = dropOffsetTags(headerTags) // the retime supersedes a recorded shift (#1385)
		ge := *g
		if g.WordsFor != nil {
			var ok bool
			if ge.Words, ok = g.WordsFor(lines); !ok {
				return EditResult{}, fmt.Errorf("%w: words do not fit the file's text", ErrEditInvalid)
			}
		}
		if err := ge.validate(lines); err != nil {
			return EditResult{}, err
		}
	}
	// The writer below emits line stamps only, so a line carrying word timings
	// would silently lose them (a downgrade). Word-preserving edits are a later
	// phase; until then such input is refused, never written.
	for _, l := range lines {
		if len(l.Words) > 0 {
			return EditResult{}, ErrEditRefused
		}
	}
	song := models.Song{}
	for _, l := range lines {
		song.Subtitles.Lines = append(song.Subtitles.Lines, models.Lines{Text: l.Text, Time: models.MsToTime(l.StartMS)})
	}
	if out, _ := timing.Evaluate(song, opts.DurationSeconds); out != timing.Ok && out != timing.UnknownDuration {
		return EditResult{}, fmt.Errorf("%w: %v", ErrEditTiming, out)
	}

	var res EditResult
	switch _, oerr := lstatRegular(root, rel+".orig"); {
	case errors.Is(oerr, fs.ErrNotExist):
		cur, rerr := readRegular(root, rel, fi)
		if rerr != nil {
			return EditResult{}, rerr
		}
		// Recorded BEFORE the write so the watcher drops the create event.
		opts.SelfWrites.Record(path + ".orig")
		werr := rootWriteAtomic(root, rel+".orig", cur, fi.Mode().Perm(), true)
		switch {
		case werr == nil:
			res.CreatedOrig = true
		case errors.Is(werr, fs.ErrExist):
			// A .orig appeared since the Lstat. A regular one IS the original by
			// spec (it was written before any edit), so keep it and proceed; any
			// other kind is no usable backup, so refuse.
			if _, err := lstatRegular(root, rel+".orig"); err != nil {
				return EditResult{}, refuseOrWrap(err)
			}
		default:
			return EditResult{}, fmt.Errorf("writing .orig backup: %w", werr)
		}
	case oerr != nil:
		// Present but not a regular file: no usable backup exists, so refuse
		// rather than rewrite the only copy of the original.
		return EditResult{}, refuseOrWrap(oerr)
	default:
		// A backup exists and is never touched, so it must really back THIS
		// file: otherwise the rewrite destroys bytes no copy holds (#1313).
		ofi, _ := lstatRegular(root, rel+".orig")
		ob, rerr := readRegular(root, rel+".orig", ofi)
		if rerr != nil {
			return EditResult{}, rerr
		}
		if err := requireOrigBacksCurrent(root, rel, fi, ob, opts.Generated != nil); err != nil {
			return EditResult{}, err
		}
	}

	var body bytes.Buffer
	for _, tag := range headerTags {
		if opts.Generated != nil && strings.TrimSpace(tag) == timingMarker {
			continue // written once, below
		}
		body.WriteString(tag + "\n")
	}
	if opts.Generated != nil {
		body.WriteString(timingMarker + "\n")
	}
	for _, l := range song.Subtitles.Lines {
		text := l.Text
		if text == "" {
			text = "♪"
		}
		body.WriteString("[" + l.Time.Stamp() + "]" + text + "\n")
	}
	opts.SelfWrites.Record(path)
	if err := rootWriteAtomic(root, rel, body.Bytes(), fi.Mode().Perm(), false); err != nil {
		return EditResult{}, fmt.Errorf("writing edited lyrics: %w", err)
	}
	nfi, err := root.Lstat(rel)
	if err != nil {
		return EditResult{}, fmt.Errorf("stat edited lyrics: %w", err)
	}
	res.NewMTime = nfi.ModTime()
	return res, nil
}

// rootWriteAtomic writes data to rel through root: a sibling temp file named
// "<name>.<random>.tmp" (the shape selfwrite already suppresses), given perm
// and synced before it is published at rel, then the parent directory is
// synced. Every step goes through the os.Root, so a directory swapped for a
// symlink after validation cannot redirect the write outside the library root.
//
// exclusive publishes with a hard link, which fails with fs.ErrExist rather
// than ever replacing rel (the .orig backup); a filesystem without hard links
// fails the write instead of falling back to an overwriting rename. Otherwise
// the temp is renamed over rel, removing rel first on Windows, where a rename
// cannot replace an existing file.
func rootWriteAtomic(root *os.Root, rel string, data []byte, perm fs.FileMode, exclusive bool) (retErr error) {
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return fmt.Errorf("temp name: %w", err)
	}
	tmp := rel + "." + hex.EncodeToString(rnd[:]) + selfwrite.TempExt
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	published := false
	defer func() {
		if retErr != nil {
			_ = f.Close()
		}
		if !published {
			_ = root.Remove(tmp)
		}
	}()
	if err := f.Chmod(perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if exclusive {
		if err := root.Link(tmp, rel); err != nil {
			return fmt.Errorf("publishing temp file: %w", err)
		}
		// The temp name is removed by the deferred cleanup; rel stays.
	} else {
		if runtime.GOOS == "windows" {
			if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("removing existing file: %w", err)
			}
		}
		if err := root.Rename(tmp, rel); err != nil {
			return fmt.Errorf("renaming temp file: %w", err)
		}
		published = true
	}
	if d, err := root.Open(filepath.Dir(rel)); err == nil {
		_ = d.Sync() // durability only; the rename already happened
		_ = d.Close()
	}
	return nil
}
