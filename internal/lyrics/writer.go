package lyrics

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/selfwrite"
	"github.com/sydlexius/canticle/internal/version"
)

// InstrumentalMarker is the human-readable marker embedded in an instrumental .txt
// sidecar (without trailing newline). Exported so consumers can detect the marker
// via substring search -- e.g. files renamed from .lrc may carry LRC tag headers
// before the marker line. The writer appends "\n" when writing the bare .txt form.
const InstrumentalMarker = "♪ Instrumental ♪"

// SidecarName derives the base file name WriteLRC writes for a track, applying the
// same extension selection and base-name safety checks the writer uses. synced
// selects the extension: ".lrc" for synced lyrics, ".txt" otherwise (unsynced
// lyrics or the instrumental marker). When filename is non-empty it must be a
// single path component (its extension is swapped to match the content type);
// otherwise the name is derived from "artist - track" via Slugify. It returns an
// error if the provided or derived name is not a safe base name (rejecting ".",
// "..", any path separator, or an absolute path so a crafted name cannot traverse
// out of the output dir). Reconcile (#405) uses this to locate the exact .txt
// sidecar an instrumental marker would occupy, guaranteeing its path logic matches
// the writer's rather than re-implementing it.
func SidecarName(artist, track, filename string, synced bool) (string, error) {
	ext := ".txt"
	if synced {
		ext = ".lrc"
	}
	var fn string
	if filename != "" {
		if filename == "." || filename == ".." || filename != filepath.Base(filename) ||
			isUnsafeBaseName(filename) {
			return "", fmt.Errorf("refusing to write: output filename %q is not a base name", filename)
		}
		fn = strings.TrimSuffix(filename, filepath.Ext(filename)) + ext
	} else {
		fn = Slugify(fmt.Sprintf("%s - %s", artist, track)) + ext
	}
	// Defense in depth: re-check the derived name after the extension swap or
	// Slugify, keeping the base-name invariant local and failing closed if either
	// branch ever regresses.
	if isUnsafeBaseName(fn) {
		return "", fmt.Errorf("refusing to write: derived output name %q is not a base name", fn)
	}
	return fn, nil
}

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
	// bilingual enables opt-in interleaved original+translation output (see
	// docs/multilingual-output-policy.md). When false (the default), only the
	// original track is written even if a translation track is present.
	bilingual bool
	// selfWrites, when non-nil, records every path this writer touches so the
	// filesystem watcher can drop the events its own writes generate (#685).
	// Nil (the default, and every non-serve caller) is a no-op.
	selfWrites *selfwrite.Registry
}

// SetSelfWriteRegistry attaches the registry the watcher consults to recognize
// this process's own writes (#685). Not goroutine-safe; call before sharing the
// writer, alongside SetBilingual.
func (w *LRCWriter) SetSelfWriteRegistry(r *selfwrite.Registry) {
	w.selfWrites = r
}

// SetBilingual enables or disables interleaved bilingual output. When enabled
// AND a Song carries a non-empty TranslationSubtitles track, writeSyncedLRC
// emits each original line followed by its translation under the original
// line's timestamp. Default false (original-only). Not goroutine-safe; call
// before sharing the writer.
func (w *LRCWriter) SetBilingual(enabled bool) {
	w.bilingual = enabled
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

// isUnsafeBaseName reports whether name would escape its directory when joined
// into an output path: an absolute path, or any string containing a path
// separator. Shared by the raw caller-provided-filename guard and the
// defense-in-depth post-compute guard on the derived output name.
func isUnsafeBaseName(name string) bool {
	return filepath.IsAbs(name) || strings.ContainsAny(name, `/\`)
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
	var instrumental bool
	// kind labels the per-track outcome on the "lyrics saved" log so instrumental
	// writes are visible at the default Info level, not just under Debug.
	var kind string
	switch {
	case song.Track.Instrumental == 1:
		// Instrumental is authoritative: MM delivers a synced subtitle line alongside
		// the flag, so this case must precede the subtitles check.
		kind = "instrumental"
		writeContent = writeInstrumental
		instrumental = true
	case len(song.Subtitles.Lines) > 0:
		kind = "synced"
		writeContent = func(buf *bufio.Writer) error { return writeSyncedLRC(song, buf, w.bilingual) }
		writeTags = true
		synced = true
	case song.Lyrics.LyricsBody != "":
		kind = "unsynced"
		writeContent = func(buf *bufio.Writer) error { return writeUnsyncedLRC(song, buf) }
	default:
		return fmt.Errorf("nothing to save for %s - %s", song.Track.ArtistName, song.Track.TrackName)
	}

	// Accept-time timing guard (#439). A synced result is promoted to .lrc only
	// once internal/timing agrees its cues fit the audio. This sits between the
	// content-type gate and the write so BOTH accept paths (fetch mode and the
	// serve worker) inherit it from the one place that owns promotion, with no
	// per-caller opt-in and no provider-specific behavior.
	//
	// The verdict is DELEGATED, never recomputed here: see DecidePromotion.
	decision, verdict, _ := DecidePromotion(song)
	demoted := false
	switch decision {
	case Quarantine:
		// Timed to a different, longer recording. These words are not
		// trustworthy content for this file, so nothing is written and nothing
		// already on disk is touched. Not an error: the fetch worked, and the
		// caller settles the row rather than retrying something a retry cannot
		// fix.
		slog.Warn("refusing to write lyrics: timing indicates a different recording",
			"artist", song.Track.ArtistName, "track", song.Track.TrackName,
			"outcome", string(verdict), "decision", decision.String())
		return nil
	case DemoteToUnsynced:
		// Content-safe demotion (Investigation-0 on #438): the words are the
		// right song's, only the timing is wrong. Refuse the .lrc and keep the
		// words as .txt.
		body := unsyncedFallbackBody(song)
		if body == "" {
			slog.Warn("refusing to write lyrics: timing overruns the audio and no plain words survive demotion",
				"artist", song.Track.ArtistName, "track", song.Track.TrackName,
				"outcome", string(verdict))
			return nil
		}
		kind = "unsynced (demoted)"
		writeContent = func(buf *bufio.Writer) error { return writeText(body, buf) }
		writeTags = false
		synced = false
		demoted = true
	case PromoteAsIs:
		// Compliant, not judgeable, or not a synced result at all: unchanged.
	}

	// Derive the sidecar base name (extension selection + base-name safety) via
	// the shared helper so reconcile (#405) locates the exact same .txt path.
	fn, err := SidecarName(song.Track.ArtistName, song.Track.TrackName, filename, synced)
	if err != nil {
		return err
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

	// A demotion must never destroy settled content. Both sidecar forms count as
	// settled: an existing .txt is the upgrade scenario the issue calls out (the
	// candidate that would have promoted it did not qualify, so the .txt stays
	// exactly as it was), and an existing .lrc is a previously-accepted synced
	// result that a demoted candidate has no standing to replace. Only when
	// neither exists is this a fresh fetch, where writing the words as .txt is
	// what keeps them (AC #3). The check is deliberately here, after root
	// re-confinement resolved outdir, so it stats the same path the write would
	// use.
	//
	// This is how WriteLRC learns fresh-fetch from upgrade: from the disk it is
	// about to write to, not from a caller-supplied flag. The alternative -- a
	// parameter on the Writer interface -- would change a signature with three
	// callers plus their mocks so each could re-derive a fact the writer can see
	// directly, and any caller that got it wrong would silently truncate a
	// settled sidecar.
	if demoted {
		if settled, ok := settledSidecar(fp); ok {
			slog.Info("keeping settled lyrics: candidate timing overruns the audio",
				"path", settled, "artist", song.Track.ArtistName, "track", song.Track.TrackName,
				"outcome", string(verdict))
			return nil
		}
	}

	var tags []string
	if writeTags {
		tags = []string{
			"[by:canticle]",
			fmt.Sprintf("[ar:%s]", song.Track.ArtistName),
			fmt.Sprintf("[ti:%s]", song.Track.TrackName),
		}
		if song.Track.AlbumName != "" {
			tags = append(tags, fmt.Sprintf("[al:%s]", song.Track.AlbumName))
		}
		if song.Track.TrackLength != 0 {
			tags = append(tags, fmt.Sprintf("[length:%02d:%02d]", song.Track.TrackLength/60, song.Track.TrackLength%60))
		}
		if song.WinningLane != "" {
			tags = append(tags, fmt.Sprintf("[source:%s]", song.WinningLane))
		}
		if !song.FetchedAt.IsZero() {
			tags = append(tags, fmt.Sprintf("[fetched:%s]", song.FetchedAt.Format(time.RFC3339)))
		}
		tags = append(tags, fmt.Sprintf("[ve:%s]", version.Version))
		if song.Track.ISRC != "" {
			tags = append(tags, fmt.Sprintf("[isrc:%s]", song.Track.ISRC))
		}
		if song.Track.RecordingMBID != "" {
			tags = append(tags, fmt.Sprintf("[mbid:%s]", song.Track.RecordingMBID))
		}
	}

	if instrumental {
		src := song.WinningLane
		// EITHER signal marks a detector-written marker. This used to key on
		// DetectorVersion alone, which meant an unknown model version silently
		// wrote [source:<lane>] instead of [source:canticle-detector]: IsDetector()
		// then read false and scanner.instrumentalReopenable treated a provisional
		// detector verdict as editorially terminal -- reopenable only by a full
		// --update, never by --upgrade.
		//
		// That was structurally unreachable while DetectorVersion was the app
		// version (a build constant, never empty). Keying it to the sidecar model
		// (#684) makes empty routine: every process start before /health answers,
		// and permanently against a sidecar too old to report a version. The
		// provenance must not degrade just because the version is unknown.
		//
		// The version check is KEPT alongside the lane check rather than replaced
		// by it: callers that build a detector Song directly, without going through
		// the orchestrator that stamps WinningLane, carry the version and no lane.
		// Requiring the lane alone would silently relabel those as provider-written
		// -- the same defect, moved.
		if song.WinningLane == DetectorLaneName || song.DetectorVersion != "" {
			src = SourceDetector
		}
		tags = append(tags, "[by:canticle]")
		if src != "" {
			tags = append(tags, fmt.Sprintf("[source:%s]", src))
		}
		// [dv:] is omitted when unknown -- it records WHICH model decided, and an
		// empty tag would assert a version that was never established. Absent is
		// honest; the [source:] token above already carries the provenance.
		if song.DetectorVersion != "" {
			tags = append(tags, fmt.Sprintf("[dv:%s]", song.DetectorVersion))
		}
	}

	// Record every path the atomic-write sequence below will touch BEFORE it
	// touches any of them (#685). The sequence emits five filesystem events --
	// Create/Write/Rename on the temp file, Create on fp, Remove on fp's existing
	// copy, Remove on the opposite-extension sidecar -- and the watcher cannot
	// otherwise tell them from an external change, so it rescans the directory it
	// was just written to. Recording fp covers its temp file by derivation
	// (selfwrite.Suppress), which is what removes the race: os.CreateTemp picks
	// the random suffix, so the temp file's Create event can reach the watcher
	// before the name is known here.
	//
	// Recorded before the write rather than after, because an event can be
	// delivered while the write is still in flight. Entries expire on their own,
	// so recording a path a failed write never produces costs nothing.
	w.selfWrites.Record(fp, oppositeSidecar(fp))

	// Write to a temp file in the same directory, then rename atomically so a
	// mid-write failure never leaves a partial .lrc at the final path.
	tmp, err := os.CreateTemp(outdir, selfwrite.TempPattern(fn)) //nolint:gosec // path is constructed from sanitized song metadata
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
	// NEW-3: fsync the parent dir so the rename is durable across a hard crash.
	fsyncDir(outdir)
	// Remove the opposite sidecar so format transitions never leave both files on disk.
	// Writing .lrc removes a stale .txt (upgrade), writing .txt removes a stale .lrc (downgrade).
	if stale := oppositeSidecar(fp); stale != "" {
		if err := os.Remove(stale); err != nil && !os.IsNotExist(err) {
			slog.Warn("could not remove stale sidecar", "path", stale, "error", err)
		}
	}
	slog.Info("lyrics saved", "path", fp, "kind", kind,
		"artist", song.Track.ArtistName, "track", song.Track.TrackName)
	return nil
}

// oppositeSidecar returns the other-extension sidecar path for fp -- the .txt
// for a .lrc and the .lrc for a .txt -- or "" when fp is neither. It is the one
// definition of that pairing, shared by the write path (which removes the
// opposite sidecar so a format transition never leaves both on disk) and the
// self-write recording that keeps the resulting Remove event from waking the
// watcher.
func oppositeSidecar(fp string) string {
	switch filepath.Ext(fp) {
	case ".lrc":
		return strings.TrimSuffix(fp, ".lrc") + ".txt"
	case ".txt":
		return strings.TrimSuffix(fp, ".txt") + ".lrc"
	default:
		return ""
	}
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

// writeSyncedLRC writes the synced original track. When bilingual is true AND
// the song carries a non-empty translation track, each original line is
// followed immediately by its index-matched translation line under the
// ORIGINAL line's timestamp (the interleaved format in
// docs/multilingual-output-policy.md). Mismatched line counts are handled
// gracefully: an original line with no translation counterpart is emitted
// alone, and surplus translation lines (beyond the original count) are dropped.
func writeSyncedLRC(song models.Song, buff *bufio.Writer, bilingual bool) error {
	interleave := bilingual && len(song.TranslationSubtitles.Lines) > 0
	translations := song.TranslationSubtitles.Lines

	for i, line := range song.Subtitles.Lines {
		text := line.Text
		if text == "" {
			text = "\u266a"
		}
		fLine := fmt.Sprintf("[%02d:%02d.%02d]%s", line.Time.Minutes, line.Time.Seconds, line.Time.Hundredths, text)
		if _, err := buff.WriteString(fLine + "\n"); err != nil {
			return fmt.Errorf("writing synced line: %w", err)
		}
		if interleave && i < len(translations) {
			tText := translations[i].Text
			if tText == "" {
				tText = "\u266a"
			}
			// Use the ORIGINAL line's timestamp so the pair shares one marker.
			tLine := fmt.Sprintf("[%02d:%02d.%02d]%s", line.Time.Minutes, line.Time.Seconds, line.Time.Hundredths, tText)
			if _, err := buff.WriteString(tLine + "\n"); err != nil {
				return fmt.Errorf("writing translation line: %w", err)
			}
		}
	}

	if err := buff.Flush(); err != nil {
		return fmt.Errorf("flushing synced lyrics: %w", err)
	}
	return nil
}

func writeUnsyncedLRC(song models.Song, buff *bufio.Writer) error {
	return writeText(song.Lyrics.LyricsBody, buff)
}

// writeText emits body verbatim. Shared by the ordinary unsynced path and the
// timing guard's demotion (#439) so a demoted .txt is byte-identical to one the
// provider's own unsynced result would have produced.
func writeText(body string, buff *bufio.Writer) error {
	if _, err := buff.WriteString(body); err != nil {
		return fmt.Errorf("writing unsynced lyrics: %w", err)
	}
	if err := buff.Flush(); err != nil {
		return fmt.Errorf("flushing unsynced lyrics: %w", err)
	}
	return nil
}

// settledSidecar returns the path of an already-written sidecar for fp's stem
// (either extension), and whether one exists. Used only by the timing guard's
// demotion path, which must not overwrite settled content.
//
// A stat error other than not-exist is treated as PRESENT: the guard's job here
// is to avoid destroying a file, so an unreadable path is assumed occupied
// rather than assumed free.
func settledSidecar(fp string) (string, bool) {
	stem := strings.TrimSuffix(fp, filepath.Ext(fp))
	for _, ext := range []string{".txt", ".lrc"} {
		candidate := stem + ext
		if _, err := os.Stat(candidate); err == nil || !os.IsNotExist(err) {
			return candidate, true
		}
	}
	return "", false
}

// writeInstrumental emits a plain instrumental marker (no [00:00.00] timestamp,
// no tag headers) so the .txt output carries only the single marker line.
func writeInstrumental(buff *bufio.Writer) error {
	if _, err := buff.WriteString(InstrumentalMarker + "\n"); err != nil {
		return fmt.Errorf("writing instrumental line: %w", err)
	}
	if err := buff.Flush(); err != nil {
		return fmt.Errorf("flushing instrumental lyrics: %w", err)
	}
	return nil
}
