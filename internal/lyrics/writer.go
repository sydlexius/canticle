package lyrics

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/pathutil"
)

// Writer abstracts LRC file output.
type Writer interface {
	WriteLRC(song models.Song, filename string, outdir string) error
}

// LRCWriter writes songs to .lrc files.
//
// When constructed with one or more confinement roots, a write whose output
// directory falls under a root is re-resolved and re-confined to that root
// immediately before the write (pathutil.ResolveWithinRoot, which follows
// symlinks with filepath.EvalSymlinks). This is the write-time half of the
// fix for #102 and closes the realistic write-time TOCTOU left open by the
// handler-side check in PR #98: a directory component swapped for a symlink
// that escapes the root between the handler check and the worker write is
// rejected here instead of redirecting the write outside the root, while
// legitimate in-root symlinks (e.g. a symlinked album directory) still resolve
// and write normally. Output outside every configured root, and writers built
// with no roots (e.g. directory mode), use the plain path.
//
// This re-confine-before-write narrows the exposure from the handler-to-worker
// queue latency to the microseconds between the resolve and the open; it is not
// a fully race-free guarantee. An open-time guard (os.Root / openat2) would be,
// but os.Root rejects every symlinked path component, including in-root ones,
// which would break symlinked library layouts -- so it is intentionally not used
// here.
type LRCWriter struct {
	roots []string
}

// NewLRCWriter creates a new LRCWriter. Any non-empty roots passed become
// write-time confinement boundaries (see LRCWriter); pass none for unconfined
// writes. Roots are cleaned once here so confinement checks need not re-derive
// them on every write.
func NewLRCWriter(roots ...string) *LRCWriter {
	cleaned := make([]string, 0, len(roots))
	for _, r := range roots {
		if r != "" {
			cleaned = append(cleaned, filepath.Clean(r))
		}
	}
	return &LRCWriter{roots: cleaned}
}

// WriteLRC writes the song lyrics to an LRC or TXT file in the given output directory.
// Only synced lyrics are written as .lrc; unsynced lyrics and instrumentals are
// written as .txt (the .lrc extension is reserved for timed/synced content).
func (w *LRCWriter) WriteLRC(song models.Song, filename string, outdir string) (retErr error) {
	// Eligibility gate -- determine content type before touching disk. synced
	// drives the file extension (.lrc only for synced lyrics, .txt otherwise);
	// writeTags drives whether the LRC metadata header is emitted.
	var writeContent func(*bufio.Writer) error
	var writeTags bool
	var synced bool
	switch {
	case len(song.Subtitles.Lines) > 0:
		slog.Info("saving synced lyrics")
		writeContent = func(buf *bufio.Writer) error { return writeSyncedLRC(song, buf) }
		writeTags = true
		synced = true
	case song.Lyrics.LyricsBody != "":
		slog.Info("saving unsynced lyrics")
		writeContent = func(buf *bufio.Writer) error { return writeUnsyncedLRC(song, buf) }
	case song.Track.Instrumental == 1:
		slog.Info("saving instrumental")
		writeContent = writeInstrumental
		// Instrumentals are a plain .txt marker: no timestamp, no tag headers.
	default:
		return fmt.Errorf("nothing to save for %s - %s", song.Track.ArtistName, song.Track.TrackName)
	}

	ext := ".txt"
	if synced {
		ext = ".lrc"
	}

	var fn string
	if filename != "" {
		// In dir mode the scanner sets an explicit .lrc filename. Swap the
		// extension to match the content type (.lrc only for synced lyrics).
		fn = strings.TrimSuffix(filename, filepath.Ext(filename)) + ext
	} else {
		fn = Slugify(fmt.Sprintf("%s - %s", song.Track.ArtistName, song.Track.TrackName)) + ext
	}

	// When the output directory falls under a confinement root, re-resolve and
	// re-confine it right before the write so a symlink swapped in since the
	// caller validated the path cannot redirect the write outside the root.
	if root, ok := w.matchRoot(outdir); ok {
		resolved, ok := pathutil.ResolveWithinRoot(root, outdir)
		if !ok {
			// ResolveWithinRoot fails (EvalSymlinks) both when the dir does not
			// exist and when it escapes the root via a symlink. Distinguish the
			// two so the error is not misleading: a missing dir is a plain setup
			// error, not a confinement violation. (No MkdirAll here -- behavior is
			// unchanged; os.CreateTemp below already requires the dir to exist.)
			if _, statErr := os.Stat(outdir); os.IsNotExist(statErr) {
				return fmt.Errorf("refusing to write: output dir %q does not exist", outdir)
			}
			return fmt.Errorf("refusing to write to %q: output dir escapes confinement root %q or is unresolvable", outdir, root)
		}
		outdir = resolved
	}
	fp := filepath.Join(outdir, fn)

	var tags []string
	if writeTags {
		tags = []string{
			"[by:mxlrcgo-svc]",
			fmt.Sprintf("[ar:%s]", song.Track.ArtistName),
			fmt.Sprintf("[ti:%s]", song.Track.TrackName),
		}
		if song.Track.AlbumName != "" {
			tags = append(tags, fmt.Sprintf("[al:%s]", song.Track.AlbumName))
		}
		if song.Track.TrackLength != 0 {
			tags = append(tags, fmt.Sprintf("[length:%02d:%02d]", song.Track.TrackLength/60, song.Track.TrackLength%60))
		}
	}

	// Write to a temp file in the same directory, then rename atomically so a
	// mid-write failure never leaves a partial .lrc at the final path.
	tmp, err := os.CreateTemp(outdir, fn+".*.tmp") //nolint:gosec // path is constructed from sanitized song metadata
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", outdir, err)
	}
	tmpPath := tmp.Name()
	tmpClosed := false
	defer func() {
		if !tmpClosed {
			if cerr := tmp.Close(); cerr != nil && retErr == nil {
				retErr = fmt.Errorf("closing %s: %w", tmpPath, cerr)
			}
		}
		if retErr != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	buffer := bufio.NewWriter(tmp)
	for _, tag := range tags {
		if _, err := buffer.WriteString(tag + "\n"); err != nil {
			return fmt.Errorf("writing tag: %w", err)
		}
	}
	if err := writeContent(buffer); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpPath, err)
	}
	tmpClosed = true
	// Restore typical output file permissions (0666, subject to umask).
	// os.CreateTemp creates files with mode 0600; chmod before rename so the
	// final .lrc has the same permissions as a file created with os.Create.
	if err := os.Chmod(tmpPath, 0o666); err != nil { //nolint:gosec // mode is a fixed constant, not user input
		return fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	// On Windows, os.Rename fails when the destination already exists.
	// Remove it first so overwrite semantics are preserved cross-platform.
	if err := os.Remove(fp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing existing %s: %w", fp, err)
	}
	if err := os.Rename(tmpPath, fp); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmpPath, fp, err)
	}
	// Remove the opposite sidecar so format transitions never leave both files on disk.
	// Writing .lrc removes a stale .txt (upgrade), writing .txt removes a stale .lrc (downgrade).
	switch filepath.Ext(fp) {
	case ".lrc":
		stale := strings.TrimSuffix(fp, ".lrc") + ".txt"
		if err := os.Remove(stale); err != nil && !os.IsNotExist(err) {
			slog.Warn("could not remove stale sidecar", "path", stale, "error", err)
		}
	case ".txt":
		stale := strings.TrimSuffix(fp, ".txt") + ".lrc"
		if err := os.Remove(stale); err != nil && !os.IsNotExist(err) {
			slog.Warn("could not remove stale sidecar", "path", stale, "error", err)
		}
	}
	slog.Info("lyrics saved", "path", fp)
	return nil
}

// matchRoot returns the longest configured confinement root that outdir is
// lexically under, or ok=false when outdir is under none. Roots are already
// cleaned by NewLRCWriter.
func (w *LRCWriter) matchRoot(outdir string) (string, bool) {
	best := ""
	for _, r := range w.roots {
		if pathutil.WithinRoot(r, outdir) && len(r) > len(best) {
			best = r
		}
	}
	return best, best != ""
}

func writeSyncedLRC(song models.Song, buff *bufio.Writer) error {
	var text string
	var fLine string
	for _, line := range song.Subtitles.Lines {
		if text = line.Text; line.Text == "" {
			text = "\u266a"
		}
		fLine = fmt.Sprintf("[%02d:%02d.%02d]%s", line.Time.Minutes, line.Time.Seconds, line.Time.Hundredths, text)
		if _, err := buff.WriteString(fLine + "\n"); err != nil {
			return fmt.Errorf("writing synced line: %w", err)
		}
	}

	if err := buff.Flush(); err != nil {
		return fmt.Errorf("flushing synced lyrics: %w", err)
	}
	return nil
}

func writeUnsyncedLRC(song models.Song, buff *bufio.Writer) error {
	if _, err := buff.WriteString(song.Lyrics.LyricsBody); err != nil {
		return fmt.Errorf("writing unsynced lyrics: %w", err)
	}
	if err := buff.Flush(); err != nil {
		return fmt.Errorf("flushing unsynced lyrics: %w", err)
	}
	return nil
}

// writeInstrumental emits a plain instrumental marker (no [00:00.00] timestamp,
// no tag headers) so the .txt output carries only the single marker line.
func writeInstrumental(buff *bufio.Writer) error {
	line := "\u266a Instrumental \u266a"
	if _, err := buff.WriteString(line + "\n"); err != nil {
		return fmt.Errorf("writing instrumental line: %w", err)
	}
	if err := buff.Flush(); err != nil {
		return fmt.Errorf("flushing instrumental lyrics: %w", err)
	}
	return nil
}
