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
	"github.com/sydlexius/canticle/internal/sidecar"
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
	kind := sidecar.KindUnsynced
	if synced {
		kind = sidecar.KindLineSynced
	}
	return SidecarNameFor(artist, track, filename, kind)
}

// SidecarNameFor is SidecarName with the extension selected by a sidecar.Kind
// rather than a two-valued bool, so a third kind (the word-synced companion,
// #986) can be named without changing SidecarName's exported signature. It
// errors for a Kind with no declared extension.
func SidecarNameFor(artist, track, filename string, kind sidecar.Kind) (string, error) {
	ext := sidecar.Ext(kind)
	if ext == "" {
		return "", fmt.Errorf("refusing to write: no sidecar extension for kind %d", kind)
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
	// wordSync enables opt-in Enhanced-LRC (A2) inline word markers on synced
	// output (#480). When false (the default), a word-synced result writes the
	// same line-level .lrc it always did, so an existing library never changes
	// shape because a provider served richer data.
	wordSync bool
	// wordSyncCompanion enables the word-synced companion sidecar (#986): a
	// separate file (sidecar.ExtWordSynced) carrying the A2 body beside a .lrc
	// that is left exactly as it would be without it. Independent of wordSync,
	// which controls markers INSIDE the .lrc; output.word_sync_mode maps onto
	// the pair (sidecar = companion only, inline = wordSync only, both = both).
	wordSyncCompanion bool
	// companionGate reports whether canticle may touch companion files at all.
	// Nil (production) reads sidecar.Active(sidecar.KindWordSynced), true since
	// #986 slice 4c once the realign/scan/purge/revalidate paths learned the
	// extension: a companion those paths cannot see must never reach a
	// library. It is a TEST-ONLY seam; nothing outside _test files sets it.
	companionGate func() bool
	// companionWrite, when non-nil, replaces writeAtomic for the companion
	// only. A TEST-ONLY seam, like companionGate: it is the one way to fail the
	// companion write after the .lrc has landed, since any filesystem obstacle
	// at the companion path is classified foreign and skipped before that.
	companionWrite func(outdir, fn string, tags []string, body func(*bufio.Writer) error) error
	// companionRemove, when non-nil, replaces os.Remove for a stale companion.
	// TEST-ONLY, for the same reason: it fails the removal without also making
	// the directory unwritable for the .lrc.
	companionRemove func(path string) error
	// selfWrites, when non-nil, records every path this writer touches so the
	// filesystem watcher can drop the events its own writes generate (#685).
	// Nil (the default, and every non-serve caller) is a no-op.
	selfWrites *selfwrite.Registry
}

// SetWordSync enables or disables Enhanced-LRC (A2) word markers on synced
// output. When enabled AND a Song carries WordTimings that pass a2Words' checks,
// each cue keeps its leading line-level [mm:ss.xx] and gains inline <mm:ss.xx>
// markers per word.
//
// Default false. Player support for A2 is not universal and the failure mode is
// three-way -- render, silently strip, or show the markers as literal text --
// so this stays opt-in rather than something a library inherits silently. Not
// goroutine-safe; call before sharing the writer, alongside SetBilingual.
func (w *LRCWriter) SetWordSync(enabled bool) {
	w.wordSync = enabled
}

// SetWordSyncCompanion enables or disables the word-synced companion sidecar
// (#986). When enabled, a synced write whose word timings qualify on at least
// one line ALSO writes the A2 body to a companion file beside the .lrc. The
// companion is gated on the sidecar table activating its Kind; while inactive
// this setting changes nothing. Not goroutine-safe; call before sharing the
// writer, alongside SetBilingual.
func (w *LRCWriter) SetWordSyncCompanion(enabled bool) {
	w.wordSyncCompanion = enabled
}

// WordSyncCompanion reports the SetWordSyncCompanion setting.
func (w *LRCWriter) WordSyncCompanion() bool {
	return w.wordSyncCompanion
}

// companionActive reports whether companion files are canticle's to write and
// remove. See the companionGate field.
func (w *LRCWriter) companionActive() bool {
	if w.companionGate != nil {
		return w.companionGate()
	}
	return sidecar.Active(sidecar.KindWordSynced)
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
// When the word-synced companion is active (#986, see planCompanion) it also
// writes or removes <stem>.elrc, and can then return an error AFTER the .lrc
// has already landed (a failed companion write).
func (w *LRCWriter) WriteLRC(song models.Song, filename string, outdir string) error {
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
		writeContent = func(buf *bufio.Writer) error { return writeSyncedLRC(song, buf, w.bilingual, w.wordSync) }
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
		// That includes a word-synced companion (#986): the verdict judges this
		// CANDIDATE, not what is on disk, and a companion there belongs to the
		// .lrc being kept.
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

	outdir, err = w.resolveOutdir(outdir)
	if err != nil {
		return err
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
		// [upstream:] names the LICENSOR a multiplexing lane routed this result
		// to; [source:] stays the LANE and never varies with it. Keeping them
		// separate is what makes purgeprovenance.provenanceAgrees hold unchanged
		// (it compares the source tag against work_queue.provider_lane) and what
		// stops `--source musixmatch` sweeping in files that a multiplexing lane
		// merely ROUTED through Musixmatch. See docs/provider-attribution.md.
		//
		// [upstream:] IS NESTED INSIDE THE [source:] GUARD, NOT A SIBLING OF IT,
		// and that nesting is load-bearing rather than tidiness. The two fields
		// are populated by DIFFERENT layers: WinningLane only by the orchestrator
		// (serve mode), Upstream by the provider itself, one layer below. One-shot
		// `fetch` mode never runs the orchestrator -- providers.New returns a bare
		// namedProvider that forwards to the client and stamps no lane -- so with
		// providers.primary = "innertube" an uncoupled write emitted
		// [upstream:<licensor>] with NO [source:] at all.
		//
		// That file is a contradiction on disk: purge's --no-source cohort is
		// documented as the inherited/foreign files canticle never wrote, and it
		// would match a sidecar that positively names the licensor canticle
		// fetched from. Measured through Purger.Run: matched=1, deleted=1.
		//
		// So the licensor is written only where the lane is. An attribution with
		// nothing to attribute it TO is not a weaker record, it is a misleading
		// one -- and fetch mode recording no attribution matches what it already
		// does for [source:], which has always been absent there.
		//
		// Omitted when empty, deliberately: an absent tag asserts nothing, which
		// is the honest reading when no licensor was named. Same rule as [dv:]
		// below.
		if song.WinningLane != "" {
			tags = append(tags, fmt.Sprintf("[source:%s]", song.WinningLane))
			if song.Upstream != "" {
				tags = append(tags, fmt.Sprintf("[upstream:%s]", song.Upstream))
			}
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
		// NESTED for the same reason as the tagged branch above: an [upstream:]
		// with no [source:] beside it is an attribution with nothing to attribute
		// it to, and lands the file in purge's "canticle never wrote this"
		// cohort while naming the licensor canticle fetched from. src is empty on
		// exactly the fetch-mode path that motivates the nesting there.
		//
		// The inner guard is on src, not on song.Upstream alone. src is
		// SourceDetector whenever canticle's own detector decided, and a detector
		// verdict is canticle's, never a licensor's -- carrying a provider's
		// upstream onto it would credit that provider for a call it did not make.
		// Note src covers BOTH independent detector signals (the lane and the
		// version), so a directly-built detector Song is caught too.
		if src != "" {
			tags = append(tags, fmt.Sprintf("[source:%s]", src))
			if src != SourceDetector && song.Upstream != "" {
				tags = append(tags, fmt.Sprintf("[upstream:%s]", song.Upstream))
			}
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
	//
	// The word-synced companion (#986) is decided HERE, before any file is
	// opened: a2Words' per-line decision is otherwise made mid-stream, and a
	// companion none of whose lines qualified would be a byte copy of the .lrc
	// claiming word timing it does not carry. It is recorded alongside fp for
	// the same watcher reason, but ONLY when this write touches it: recording
	// an untouched path would make the watcher drop a third party's change.
	companion := w.planCompanion(song, fp, synced)
	w.selfWrites.Record(fp, oppositeSidecar(fp), companion.path)

	// Any existing companion is removed BEFORE the .lrc/.txt is replaced, even
	// when a fresh one is about to be written. The two renames cannot be atomic
	// together, so this picks the failure: a crash or a failed companion write
	// leaves a line-synced file and NO companion, never an older companion
	// describing the lyric this write replaced.
	if companion.remove {
		remove := os.Remove
		if w.companionRemove != nil {
			remove = w.companionRemove
		}
		if err := remove(companion.path); err != nil && !os.IsNotExist(err) {
			// Abort BEFORE the .lrc/.txt is replaced: continuing would leave
			// the old word timing beside new line timing.
			return fmt.Errorf("removing stale word-synced companion: %w", err)
		}
	}

	if err := writeAtomic(outdir, fn, tags, writeContent); err != nil {
		return err
	}
	// Remove the opposite sidecar so format transitions never leave both files on disk.
	// Writing .lrc removes a stale .txt (upgrade), writing .txt removes a stale .lrc (downgrade).
	if stale := oppositeSidecar(fp); stale != "" {
		if err := os.Remove(stale); err != nil && !os.IsNotExist(err) {
			slog.Warn("could not remove stale sidecar", "path", stale, "error", err)
		}
	}
	slog.Info("lyrics saved", "path", fp, "kind", kind,
		"artist", song.Track.ArtistName, "track", song.Track.TrackName)

	// Companion LAST, strictly after the .lrc/.txt is durable, so it can never
	// sit beside a missing or stale .lrc. The old one is already gone (above).
	if companion.write {
		cfn := filepath.Base(companion.path)
		body := func(buf *bufio.Writer) error { return writeSyncedLRC(song, buf, w.bilingual, true) }
		write := writeAtomic
		if w.companionWrite != nil {
			write = w.companionWrite
		}
		if err := write(outdir, cfn, tags, body); err != nil {
			return fmt.Errorf("writing word-synced companion: %w", err)
		}
		slog.Info("lyrics saved", "path", companion.path, "kind", "word-synced companion",
			"artist", song.Track.ArtistName, "track", song.Track.TrackName)
	}
	return nil
}

// companionPlan is planCompanion's verdict. path is the companion this write
// touches ("" when it touches none); remove says an existing canticle-owned
// companion is deleted before the .lrc/.txt is replaced; write says a fresh
// one is written after it. Every path in a plan is canticle's to touch, so the
// plan is also exactly what is recorded with selfwrite.
type companionPlan struct {
	path   string
	remove bool
	write  bool
}

// companionOwnership is what is on disk at the companion path.
type companionOwnership int

const (
	companionAbsent  companionOwnership = iota // nothing there
	companionOwned                             // a regular file tagged [by:canticle]
	companionForeign                           // anything else: never touched
)

// planCompanion decides what this write does to the word-synced companion
// beside fp (#986). A companion is NOT an opposite (see oppositeSidecar): it
// follows the .lrc rather than excluding it. The invariant: a companion on disk
// that canticle wrote always describes the .lrc beside it, never an earlier one.
//
//   - Gate closed: nothing. With sidecar.KindWordSynced inactive a .elrc on
//     disk is not canticle's, so it is neither written nor removed.
//   - A FOREIGN file at the path (not a regular file, unreadable, or without
//     [by:canticle]): nothing, not even a write over it. It belongs to another
//     tool or to the operator, and no mode may cost them a file.
//   - Otherwise an owned companion is removed, and a fresh one is written when
//     this is a .lrc write with the companion enabled and at least one
//     qualifying line. A .txt write (unsynced, instrumental, or a MisSynced
//     demotion) and any other .lrc write therefore leave no companion: word
//     timing must not outlive the line timing it was aligned to.
//
// A write that touches nothing (quarantine, or a demotion that keeps a settled
// sidecar) never reaches here, so the companion of a kept .lrc is kept too.
func (w *LRCWriter) planCompanion(song models.Song, fp string, synced bool) companionPlan {
	if !w.companionActive() {
		return companionPlan{}
	}
	path := sidecar.StemOf(fp) + sidecar.ExtWordSynced
	own := companionOwnershipOf(path)
	if own == companionForeign {
		slog.Info("leaving a word-synced companion canticle did not write", "path", path)
		return companionPlan{}
	}
	plan := companionPlan{path: path, remove: own == companionOwned}
	plan.write = synced && w.wordSyncCompanion && hasA2Line(song)
	if !plan.remove && !plan.write {
		return companionPlan{}
	}
	return plan
}

// IsOwnedCompanion reports whether path is a word-synced companion canticle
// itself wrote: a regular file (never followed) whose header carries
// [by:canticle]. It is THE ownership predicate for every path that moves or
// removes a companion outside the writer (realign, revalidate through
// realign.Apply, purgeprovenance), so none of them can disagree with the writer
// about which files are canticle's to touch. Absent and foreign both report
// false: either way there is nothing the caller may touch.
func IsOwnedCompanion(path string) bool {
	return companionOwnershipOf(path) == companionOwned
}

// OwnedCompanionOf returns the word-synced companion that must travel with (or
// go with) the line-synced sidecar lrc, or "" when there is nothing to touch:
// the word-synced Kind is inactive, lrc is not a .lrc, or the file beside it is
// absent or FOREIGN. The gate is the same one the writer consults, so flipping
// sidecar.KindWordSynced is the single switch for every mutation path.
func OwnedCompanionOf(lrc string) string {
	if !sidecar.Active(sidecar.KindWordSynced) || sidecar.KindOf(lrc) != sidecar.KindLineSynced {
		return ""
	}
	c := sidecar.StemOf(lrc) + sidecar.ExtWordSynced
	if !IsOwnedCompanion(c) {
		return ""
	}
	return c
}

// companionOwnershipOf classifies path WITHOUT following it. Lstat comes first
// so a symlink, FIFO, or device is never opened: opening a FIFO blocks until a
// writer appears, which would hang the fetch. Any error other than not-exist,
// and a regular file whose header cannot be read, count as foreign, so doubt
// always resolves to leaving the file alone.
func companionOwnershipOf(path string) companionOwnership {
	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return companionAbsent
	}
	if err != nil || !fi.Mode().IsRegular() {
		return companionForeign
	}
	tags, _, err := parseLRCHeader(path)
	if err != nil {
		return companionForeign
	}
	for _, t := range tags {
		if strings.EqualFold(t.key, "by") && strings.TrimSpace(t.value) == "canticle" {
			return companionOwned
		}
	}
	return companionForeign
}

// resolveOutdir re-resolves and re-confines outdir when it falls under a
// confinement root, so a symlink swapped in since the caller validated the path
// cannot redirect the write outside the root. Outside every root it returns
// outdir unchanged.
func (w *LRCWriter) resolveOutdir(outdir string) (string, error) {
	root, ok := w.matchRoot(outdir)
	if !ok {
		return outdir, nil
	}
	resolved, ok := pathutil.ResolveWithinRoot(root, outdir)
	if !ok {
		// ResolveWithinRoot fails (EvalSymlinks) both when the dir does not
		// exist and when it escapes the root via a symlink. Distinguish the
		// two so the error is not misleading: a missing dir is a plain setup
		// error, not a confinement violation. (No MkdirAll here -- behavior is
		// unchanged; os.CreateTemp already requires the dir to exist.)
		if _, statErr := os.Stat(outdir); os.IsNotExist(statErr) {
			return "", fmt.Errorf("refusing to write: output dir %q does not exist", outdir)
		}
		return "", fmt.Errorf("refusing to write to %q: output dir escapes confinement root %q or is unresolvable", outdir, root)
	}
	return resolved, nil
}

// writeAtomic writes tags then writeContent to outdir/fn through a temp file in
// the same directory, renamed into place only on complete success, so a
// mid-write failure never leaves a partial file at the final path.
func writeAtomic(outdir, fn string, tags []string, writeContent func(*bufio.Writer) error) (retErr error) {
	fp := filepath.Join(outdir, fn)
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
	return nil
}

// oppositeSidecar returns the other-extension sidecar path for fp -- the .txt
// for a .lrc and the .lrc for a .txt -- or "" when fp is neither. It is the one
// definition of that pairing, shared by the write path (which removes the
// opposite sidecar so a format transition never leaves both on disk) and the
// self-write recording that keeps the resulting Remove event from waking the
// watcher.
//
// It is a strict PAIRING, deliberately NOT "every other sidecar extension"
// (#986). Its result is fed to os.Remove, so widening it to the whole
// sidecar.Extensions() set would silently delete a file the write path never
// intended to replace. The extensions come from internal/sidecar so the
// literals live in one place, but the two-way mapping stays hand-written here
// and a third extension has to be reasoned about rather than picked up for
// free.
//
// The extension comparison is case-SENSITIVE (plain filepath.Ext, no
// ToLower), unlike sidecar.IsSidecar: a ".LRC" on disk is not paired, and is
// therefore not removed. That asymmetry is pre-existing and preserved.
func oppositeSidecar(fp string) string {
	switch filepath.Ext(fp) {
	case sidecar.ExtLineSynced:
		return sidecar.StemOf(fp) + sidecar.ExtUnsynced
	case sidecar.ExtUnsynced:
		return sidecar.StemOf(fp) + sidecar.ExtLineSynced
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
//
// When wordSync is true AND a cue's word timings pass a2Words' fidelity and
// distinctness checks, that cue's text is replaced by inline <mm:ss.xx> word
// markers (#480). The leading line-level [mm:ss.xx] is kept either way, so a
// player without A2 support reads the file exactly as before. The decision is
// PER LINE: a cue whose timings do not reconstruct it writes its plain text, so
// a partially-timed track degrades line by line rather than losing word sync
// wholesale or, worse, losing words.
//
// A translation line never gains markers even when its original does: the word
// timings describe the ORIGINAL track's words, and mapping them onto translated
// text would attach one language's timing to another's words.
func writeSyncedLRC(song models.Song, buff *bufio.Writer, bilingual bool, wordSync bool) error {
	interleave := bilingual && len(song.TranslationSubtitles.Lines) > 0
	translations := song.TranslationSubtitles.Lines
	// Left nil unless word sync is on. This is the default write path for every
	// synced lyric in the library, and wordSync is opt-in, so allocating a map
	// the common case never reads is pure waste. A nil map is safe to read --
	// byLine[i] yields nil, which a2Words treats as "no timings" and refuses --
	// so the lookup below needs no guard of its own.
	var byLine map[int][]models.WordTiming
	if wordSync && len(song.WordTimings) > 0 {
		byLine = wordTimingsByLine(song)
	}

	for i, line := range song.Subtitles.Lines {
		text := line.Text
		if text == "" {
			text = "\u266a"
		}
		// Word markers replace the cue TEXT only; the line-level stamp below is
		// emitted either way. a2Words returns false whenever the markers would be
		// dishonest or lossy, and that fallback keeps `text` as-is.
		if wordSync {
			if marked, ok := a2Words(line.Text, byLine[i]); ok {
				text = marked
			}
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
// The probe ORDER is load-bearing and therefore written out rather than taken
// from sidecar.Extensions(): unsynced is checked first, so when both sidecars
// somehow exist the .txt is the one reported.
func settledSidecar(fp string) (string, bool) {
	stem := sidecar.StemOf(fp)
	for _, kind := range []sidecar.Kind{sidecar.KindUnsynced, sidecar.KindLineSynced} {
		candidate := stem + sidecar.Ext(kind)
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
