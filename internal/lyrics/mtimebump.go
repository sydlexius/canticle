package lyrics

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

// Opt-in audio mtime bump (#505). Music Assistant keys change detection on the
// AUDIO file's mtime and never re-reads a rewritten sidecar. With
// output.bump_audio_mtime on, the writer touches the audio file's mtime
// (os.Chtimes: metadata only, content never opened) after it REPLACES an
// existing sidecar with different lyrics. A first-ever write, a refused
// (ErrKeptBetter) write and a rewrite with an unchanged lyric body never bump.
// With the key off, or no AudioPath on the song, the snapshot is a zero value
// and nothing here performs any I/O.

// maxPriorSidecarBytes caps the prior-body read; a lyric sidecar is a few KiB.
const maxPriorSidecarBytes = 4 << 20

// idTagLine matches an LRC ID-tag header ([ar:...], [fetched:...]); a timed cue
// starts with a digit and never matches. The value may itself contain ']'
// ([al:Album [Deluxe]]), so it runs greedily to the line's final bracket.
// lrcnormalize.ParseBody is not reused: it drops plain-text lines, which a
// .txt sidecar's whole body consists of.
var idTagLine = regexp.MustCompile(`^\[([A-Za-z][A-Za-z0-9_-]*):.*\]\s*$`)

// txtHeaderKeys are the ID tags the writer puts on a .txt (instrumental marker
// provenance). In a .txt only these are headers: any other bracketed line, such
// as a "[Chorus: X]" annotation, is lyric text and a change to it is a
// correction. A .lrc strips every tag-shaped line, since its lyrics are cues.
var txtHeaderKeys = map[string]bool{
	"by": true, "source": true, "upstream": true, "dv": true, "fetched": true,
	"re": true, "ve": true, "ar": true, "ti": true, "al": true, "length": true,
	"isrc": true, "mbid": true,
}

// SetAudioMtimeBump enables the #505 audio mtime bump. Not goroutine-safe; call
// before sharing the writer.
func (w *LRCWriter) SetAudioMtimeBump(enabled bool) {
	w.bumpAudioMtime = enabled
}

// priorSidecar is what WriteLRC captured about the sidecar it replaces; the
// zero value (active false) means "not tracking".
type priorSidecar struct {
	active        bool
	existed       bool   // the exact target already existed
	replacedOther bool   // an opposite-extension sidecar existed (txt<->lrc)
	bodyKnown     bool   // body holds the prior lyric body
	body          string // prior body, ID-tag headers stripped
}

func (w *LRCWriter) snapshotPrior(song models.Song, fp string, stale []string) priorSidecar {
	if !w.bumpAudioMtime || song.AudioPath == "" {
		return priorSidecar{}
	}
	p := priorSidecar{active: true, replacedOther: len(stale) > 0}
	fi, err := os.Lstat(fp)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		p.existed = true
		if err == nil && fi.Mode().IsRegular() {
			if b, ok := readCapped(fp); ok {
				p.bodyKnown, p.body = true, lyricBody(fp, b)
			}
		}
	}
	return p
}

// bumpIfCorrected bumps the audio mtime when the sidecar just written replaced
// an existing one with different content. Every failure is a Warn: the sidecar
// write already succeeded and is never rolled back.
func (w *LRCWriter) bumpIfCorrected(audio, fp string, p priorSidecar) {
	if !p.active || (!p.existed && !p.replacedOther) {
		return
	}
	// Only the sidecar that belongs to THIS audio file: a multi-output row
	// (identityrepair merge) carries one AudioPath for several sidecars, and
	// bumping it for another file's correction is a write to the wrong file.
	// Stem compare is case-insensitive because sidecar resolution already is.
	if !sidecarBelongsTo(audio, fp) {
		slog.Debug("audio mtime bump skipped: sidecar not beside its audio file", "audio", audio, "sidecar", fp)
		return
	}
	changed := p.replacedOther
	if !changed {
		nb, ok := readCapped(fp)
		changed = !p.bodyKnown || !ok || lyricBody(fp, nb) != p.body
	}
	if !changed {
		return
	}
	if fi, err := os.Stat(audio); err != nil || !fi.Mode().IsRegular() {
		slog.Warn("audio mtime bump skipped: audio file not found", "path", audio, "error", err)
		return
	}
	// Recorded BEFORE the touch, like every self-write: the attribute event can
	// reach the watcher before this call returns.
	w.selfWrites.Record(audio)
	chtimes := os.Chtimes
	if w.chtimes != nil {
		chtimes = w.chtimes
	}
	// A zero atime leaves the access time unchanged (os.Chtimes contract).
	if err := chtimes(audio, time.Time{}, time.Now()); err != nil {
		slog.Warn("audio mtime bump failed; sidecar kept", "path", audio, "error", err)
		return
	}
	slog.Info("audio mtime bumped after lyric correction", "path", audio)
}

// sidecarBelongsTo reports whether sidecar sits in audio's directory and shares
// its stem (extension excluded, case-insensitive).
func sidecarBelongsTo(audio, sidecar string) bool {
	if !sameDir(filepath.Dir(audio), filepath.Dir(sidecar)) {
		return false
	}
	stem := func(p string) string {
		b := filepath.Base(p)
		return strings.ToLower(strings.TrimSuffix(b, filepath.Ext(b)))
	}
	return stem(audio) == stem(sidecar)
}

// sameDir reports whether two directories are the same place. The writer hands
// over a sidecar path whose directory has had symlinks resolved, while the
// audio path is as the library registered it, so a library reached through a
// symlink (macOS /tmp, a symlinked mount) differs textually. Both sides are
// resolved; when either fails to resolve the literal cleaned comparison is kept.
func sameDir(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// readCapped reads a regular, non-symlink file through readRegularNoFollow (no
// FIFO block, no link follow), refusing one over maxPriorSidecarBytes.
func readCapped(path string) ([]byte, bool) {
	b, err := readRegularNoFollow(path, maxPriorSidecarBytes)
	return b, err == nil
}

// lyricBody is a sidecar's content minus its ID-tag headers, so a rewrite that
// only refreshes [fetched:]/[ve:] is a no-op rather than a correction. path
// selects the rule: a .txt drops only txtHeaderKeys (see there).
func lyricBody(path string, b []byte) string {
	txt := strings.EqualFold(filepath.Ext(path), ".txt")
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		m := idTagLine.FindStringSubmatch(line)
		if m == nil || (txt && !txtHeaderKeys[strings.ToLower(m[1])]) {
			out = append(out, strings.TrimRight(line, " \t"))
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
