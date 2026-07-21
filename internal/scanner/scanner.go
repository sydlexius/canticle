package scanner

import (
	"bufio"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/dhowden/tag"
	"github.com/dhowden/tag/mbz"
	"github.com/lizc2003/audioduration"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

// supportedFileTypes lists audio file extensions that can have metadata read.
var supportedFileTypes = []string{".mp3", ".m4a", ".m4b", ".m4p", ".alac", ".flac", ".ogg", ".dsf"}

// IsAudioFile reports whether name (a file name or path) carries a supported
// audio extension. It is the single exported accessor over supportedFileTypes so
// consumers outside the scan loop (e.g. the realign command) classify files
// identically to scanDir and cannot drift from the scanner's own extension list.
func IsAudioFile(name string) bool {
	return slices.Contains(supportedFileTypes, strings.ToLower(filepath.Ext(name)))
}

// ReadAudioProvenance reads the ISRC and MusicBrainz recording MBID embedded in
// the audio file at path, plus its artist/title tags, for a single file outside
// the full-library scan loop. It wraps the same extractISRC/extractRecordingMBID
// and tag readers scanDir uses, so realign's exact-match tier sees exactly the
// identity signals a scan would. A missing tag yields an empty string, not an
// error; only an open/parse failure returns an error.
func ReadAudioProvenance(path string) (isrc, mbid, artist, title string, err error) {
	f, oerr := os.Open(path) //nolint:gosec // G304: path is derived from a configured library root + scanned filename, confined via pathutil upstream by the caller
	if oerr != nil {
		return "", "", "", "", fmt.Errorf("open %s: %w", path, oerr)
	}
	defer func() { _ = f.Close() }()
	m, terr := tag.ReadFrom(f)
	if terr != nil {
		return "", "", "", "", fmt.Errorf("read metadata %s: %w", path, terr)
	}
	return extractISRC(m), extractRecordingMBID(m), extractArtist(m), m.Title(), nil
}

// ReadArtistIdentity opens the audio file at path and returns its corrected
// artist and album-artist, applying the same multi-value recovery a scan does
// (extractArtist / extractAlbumArtist -- issue #466). It is the seam the
// reconcile-identity backfill uses to re-derive a row's identity from disk,
// where the mangled boundaries the DB already stored cannot be recovered. An
// open or parse failure returns a non-empty error so the caller can skip the
// row rather than mistake a read failure for a genuinely empty artist.
func ReadArtistIdentity(path string) (artist, albumArtist string, err error) {
	f, oerr := os.Open(path) //nolint:gosec // G304: path originates from a scan_results row whose file_path was written from a configured library root
	if oerr != nil {
		return "", "", fmt.Errorf("open %s: %w", path, oerr)
	}
	defer func() { _ = f.Close() }()
	m, terr := tag.ReadFrom(f)
	if terr != nil {
		return "", "", fmt.Errorf("read metadata %s: %w", path, terr)
	}
	return extractArtist(m), extractAlbumArtist(m), nil
}

// AudioMetadata carries the recording disambiguators a provider query needs from
// an audio file's tags. The zero values are the documented "absent" sentinels:
// TrackLength 0 is the unknown-duration sentinel normalize.DurationBucket keys
// on, and an empty string means the tag is absent.
type AudioMetadata struct {
	TrackLength int
	ISRC        string
	AlbumName   string
}

// ReadAudioMetadata opens the audio file at path and returns the recording
// disambiguators a provider query needs: duration, ISRC, and album. It is the
// seam serve mode uses to reach fetch-mode parity (#584): work_queue persists
// neither duration nor ISRC, so the worker re-reads them from disk at fetch time
// through exactly the readers a scan uses, which makes the two paths identical by
// construction rather than by agreement.
//
// A missing tag, an unparsable duration, or an unsupported extension is not an
// error and yields the zero-value sentinel; only an open or parse failure returns
// an error, matching ReadAudioProvenance and ReadArtistIdentity.
func ReadAudioMetadata(path string) (AudioMetadata, error) {
	f, oerr := os.Open(path) //nolint:gosec // reason: G304 - path is a work_queue source_path, written from a configured library root and confined via pathutil upstream
	if oerr != nil {
		return AudioMetadata{}, fmt.Errorf("open %s: %w", path, oerr)
	}
	defer func() { _ = f.Close() }()
	m, terr := tag.ReadFrom(f)
	if terr != nil {
		return AudioMetadata{}, fmt.Errorf("read metadata %s: %w", path, terr)
	}
	// audioduration seeks to 0 internally, so f may sit at any offset after
	// tag.ReadFrom; no rewind is needed here. A parse failure degrades to the
	// unknown-duration sentinel exactly as scanDir does.
	dur, derr := audioDuration(f, strings.ToLower(filepath.Ext(path)))
	if derr != nil {
		slog.Debug("duration parse failed; using 0", "file", path, "error", derr)
		dur = 0
	}
	return AudioMetadata{TrackLength: dur, ISRC: extractISRC(m), AlbumName: m.Album()}, nil
}

// audioFileTypeForExt returns the audioduration type constant for a lower-case
// audio file extension. Extensions not recognized return (0, false); callers
// degrade to TrackLength=0 (the "unknown duration" sentinel).
func audioFileTypeForExt(ext string) (int, bool) {
	switch ext {
	case ".flac":
		return audioduration.TypeFlac, true
	case ".mp3":
		return audioduration.TypeMp3, true
	case ".m4a", ".m4b", ".m4p", ".alac":
		return audioduration.TypeMp4, true
	case ".ogg":
		return audioduration.TypeOgg, true
	case ".dsf":
		return audioduration.TypeDsd, true
	default:
		return 0, false
	}
}

// MetadataFailureStore remembers files that consistently fail audio metadata
// read so the scanner can skip re-reading (and re-warning about) malformed files
// until they change on disk. A nil store disables the feature: every file is
// read and every failure warns, the historical behavior. See issue #376.
type MetadataFailureStore interface {
	// ShouldSkip reports whether path previously failed metadata read at the same
	// mtime and size, meaning a re-read would fail identically and can be skipped.
	// mtimeNano is ModTime().UnixNano() (nanosecond precision so a same-second
	// rewrite to the same size is still detected as changed).
	ShouldSkip(ctx context.Context, path string, mtimeNano, size int64) (bool, error)
	// RecordFailure remembers that path failed metadata read at the given mtime
	// and size, so subsequent scans skip it until the file changes.
	RecordFailure(ctx context.Context, path string, mtimeNano, size int64, readErr error) error
}

// Scanner handles parsing input sources and populating the work queue.
type Scanner struct {
	// probeFunc, when set, is used as the duration probe instead of audioduration
	// (tests only -- lets tests inject a known duration without real audio fixtures).
	probeFunc func(string) (int, error)
	// failures, when set, lets the scanner skip files that previously failed
	// metadata read (and warn only once per file-version). Nil disables it.
	failures MetadataFailureStore
}

// Option configures a Scanner at construction.
type Option func(*Scanner)

// WithMetadataFailureStore wires a store so the scanner skips files that
// consistently fail metadata read until they change on disk (issue #376).
func WithMetadataFailureStore(s MetadataFailureStore) Option {
	return func(sc *Scanner) { sc.failures = s }
}

// ScanOptions controls library directory traversal and queue eligibility.
type ScanOptions struct {
	Update   bool
	Upgrade  bool
	MaxDepth int
	BFS      bool
	// EmbeddedLyrics controls handling of lyrics embedded in tags: "off"
	// (default) ignores them; "respect" skips fetching a file that already
	// carries embedded lyrics; "extract" writes them to a sidecar (and then skips
	// fetching). A Vorbis SYNCEDLYRICS comment carrying timestamped LRC text is
	// extracted as a synced .lrc and takes precedence; unsynced lyrics (the ID3
	// USLT frame) become a .txt. ID3 SYLT and MP4 synced atoms are intentionally
	// not handled.
	EmbeddedLyrics string
	// EnrichRecording controls recording enrichment: reading ISRC, MusicBrainz
	// recording ID, and duration from audio tags into the Track. When false, all
	// three are skipped (the duration prober is not even invoked) and the track
	// keeps the duration_bucket=0 fallback. Callers resolve this per library via
	// config.ResolveBool; the scheduler stamps it before each ScanLibrary call.
	// The zero value is false, so direct callers that want the historical
	// always-on behavior must set it true explicitly.
	EnrichRecording bool
	// DetectorVersion is the current audio-detector version. When set (serve mode
	// with the detector enabled), a provisional (detector-written) instrumental
	// marker whose stored [dv:] differs is invalidated and re-checked, mirroring
	// providers_version cache retirement. Empty (dir/CLI mode, or detector off)
	// disables version invalidation. See #502.
	DetectorVersion string
}

// NewScanner creates a new Scanner with the supplied options.
func NewScanner(opts ...Option) *Scanner {
	sc := &Scanner{}
	for _, o := range opts {
		o(sc)
	}
	return sc
}

// audioDuration reads the header of r to determine duration in seconds.
// Returns 0 and a wrapped error for unknown extension or parse failure;
// callers treat 0 as the "unknown duration" sentinel (duration_bucket=0).
func audioDuration(r io.ReadSeeker, ext string) (int, error) {
	ft, ok := audioFileTypeForExt(ext)
	if !ok {
		return 0, fmt.Errorf("no duration parser for %s", ext)
	}
	secs, err := audioduration.Duration(r, ft)
	if err != nil {
		return 0, fmt.Errorf("duration %s: %w", ext, err)
	}
	return int(secs), nil
}

// probeDuration returns the duration in seconds for the file at f.
// Uses probeFunc when set (tests), otherwise calls audioDuration.
func (sc *Scanner) probeDuration(f *os.File, ext string) (int, error) {
	if sc.probeFunc != nil {
		return sc.probeFunc(f.Name())
	}
	return audioDuration(f, ext)
}

// extractISRC returns the ISRC from audio tag metadata, or "" if absent.
// Checks format-specific raw keys in priority order: TSRC (ID3v2.3/v2.4),
// TRC (ID3v2.2), isrc/ISRC (Vorbis/FLAC), iTunes freeform atom (MP4).
func extractISRC(m tag.Metadata) string {
	raw := m.Raw()
	for _, k := range []string{"TSRC", "TRC", "isrc", "ISRC", "----:com.apple.iTunes:ISRC"} {
		if v, ok := raw[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// extractRecordingMBID returns the MusicBrainz recording ID from audio tag metadata,
// or "" if absent.
func extractRecordingMBID(m tag.Metadata) string {
	return mbz.Extract(m).Get(mbz.Recording)
}

// artistValueSep joins the discrete values of a multi-value artist frame for
// display and for the stored identity. Semicolon-space is the de facto
// MusicBrainz/Picard multi-value separator and round-trips cleanly through
// normalize.NormalizeKey.
const artistValueSep = "; "

// extractArtist returns the track artist, preferring a correctly-delimited
// multi-value form when the file carries one.
//
// github.com/dhowden/tag mangles multi-value ID3v2.4 text frames: readTFrame
// joins the NUL-separated values with an empty string, so m.Artist() returns
// the discrete values run together ("ABC" for values A, B, C) with the value
// boundaries destroyed at read time (issue #466). The parallel TXXX "ARTISTS"
// frame that standard taggers (e.g. Picard) write alongside TPE1 is NOT mangled
// -- readTextWithDescrFrame preserves its embedded NULs -- so we recover the
// real boundaries from it when present and join them with a human-readable
// separator. Files carrying only a multi-value TPE1 and no ARTISTS frame cannot
// be recovered (the boundaries are already gone by the time m.Artist() runs);
// they keep the run-together fallback.
func extractArtist(m tag.Metadata) string {
	if v := multiValueTag(m, "ARTISTS"); v != "" {
		return v
	}
	return m.Artist()
}

// extractAlbumArtist mirrors extractArtist for the album-artist frame (TPE2 /
// TXXX "ALBUMARTISTS").
func extractAlbumArtist(m tag.Metadata) string {
	if v := multiValueTag(m, "ALBUMARTISTS"); v != "" {
		return v
	}
	return m.AlbumArtist()
}

// multiValueTag scans the raw ID3 frames for a TXXX frame whose description
// matches desc (case-insensitively) and, when its value holds two or more
// NUL-separated discrete values, returns them joined by artistValueSep.
// Surrounding whitespace is trimmed and empty values dropped (real Picard output
// terminates the frame with a trailing NUL, which would otherwise yield a
// spurious empty value). Returns "" unless at least two values are present, so a
// single-value frame falls through to the standard accessor -- this keeps the
// fix scoped to the genuine multi-value case (issue #466) and never overrides a
// correctly-read single-value TPE1/TPE2 with a differing single ARTISTS value.
//
// TXXX frames are keyed "TXXX", "TXXX_0", ... in Raw() when several are present,
// so the whole map is scanned; other *tag.Comm frames (COMM, USLT) never carry
// these descriptions and are skipped by the description match.
func multiValueTag(m tag.Metadata, desc string) string {
	raw := m.Raw()
	if raw == nil {
		return ""
	}
	for _, v := range raw {
		c, ok := v.(*tag.Comm)
		if !ok || !strings.EqualFold(c.Description, desc) {
			continue
		}
		parts := strings.Split(c.Text, "\x00")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		if len(out) >= 2 {
			return strings.Join(out, artistValueSep)
		}
	}
	return ""
}

// isInstrumentalTxt reports whether the file at path contains the instrumental
// marker. Uses substring match rather than exact equality because files renamed
// from .lrc carry LRC tag headers before the marker line. Returns false on any
// read error so a scan failure never silently drops a track.
func isInstrumentalTxt(path string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // path is constructed from directory scan within a validated root
	if err != nil {
		return false
	}
	return strings.Contains(string(data), lyrics.InstrumentalMarker)
}

// AssertInput validates a "artist,title" string and returns a Track, or nil if invalid.
// Uses csv.NewReader so that fields containing commas (RFC-4180 quoting) are handled
// correctly -- this matches the csv.Writer used in app.handleFailed.
func AssertInput(song string) *models.Track {
	r := csv.NewReader(strings.NewReader(song))
	fields, err := r.Read()
	if err != nil || len(fields) != 2 {
		return nil
	}
	return &models.Track{
		ArtistName: strings.TrimSpace(fields[0]),
		TrackName:  strings.TrimSpace(fields[1]),
	}
}

// GetSongMulti processes multiple "artist,title" pairs into the work queue.
func (sc *Scanner) GetSongMulti(songList []string, savePath string, songs *queue.InputsQueue) {
	for _, song := range songList {
		track := AssertInput(song)
		if track == nil {
			slog.Warn("invalid input", "song", song)
			continue
		}
		songs.Push(models.Inputs{Track: *track, Outdir: savePath, Filename: ""})
	}
}

// GetSongText reads a text file with "artist,title" lines and populates the queue.
func (sc *Scanner) GetSongText(textFn string, savePath string, songs *queue.InputsQueue) error {
	f, err := os.Open(textFn) //nolint:gosec // path comes from user CLI argument
	if err != nil {
		return fmt.Errorf("opening text file %s: %w", textFn, err)
	}
	s := bufio.NewScanner(f)
	s.Split(bufio.ScanLines)
	var songList []string
	for s.Scan() {
		songList = append(songList, s.Text())
	}
	_ = f.Close()
	if err := s.Err(); err != nil {
		return fmt.Errorf("reading text file %s: %w", textFn, err)
	}
	sc.GetSongMulti(songList, savePath, songs)
	return nil
}

// ScanLibrary scans a root directory for audio files and returns structured results.
func (sc *Scanner) ScanLibrary(ctx context.Context, root string, opts ScanOptions) ([]models.ScanResult, error) {
	if opts.MaxDepth < 0 {
		opts.MaxDepth = 0
	}
	var results []models.ScanResult
	if err := sc.scanDir(ctx, root, opts, 0, &results); err != nil {
		return nil, err
	}
	return results, nil
}

func (sc *Scanner) scanDir(ctx context.Context, dir string, opts ScanOptions, depth int, results *[]models.ScanResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	slog.Debug("scanning directory", "path", dir)
	files, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading directory %s: %w", dir, err)
	}

	sort.Slice(files, func(i int, j int) bool {
		id1, id2 := files[i].IsDir(), files[j].IsDir()
		if id1 == id2 {
			return files[i].Name() < files[j].Name()
		}
		return opts.BFS != id1
	})

	// reopen is loop-invariant for this directory scan (opts is fixed), so compute
	// it once rather than per file.
	reopen := reopenClassesFor(opts)
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if file.IsDir() {
			if depth < opts.MaxDepth {
				if err := sc.scanDir(ctx, filepath.Join(dir, file.Name()), opts, depth+1, results); err != nil {
					return err
				}
			}
			continue
		}

		ext := strings.ToLower(filepath.Ext(file.Name()))

		// Skip lyrics files themselves -- they are not audio sources.
		if ext == ".lrc" || ext == ".txt" {
			continue
		}

		stem := strings.TrimSuffix(file.Name(), filepath.Ext(file.Name()))
		lrcFile := stem + ".lrc"
		txtFile := stem + ".txt"

		lrcExists := false
		if _, err := os.Stat(filepath.Join(dir, lrcFile)); err == nil {
			lrcExists = true
		}
		txtExists := false
		if _, err := os.Stat(filepath.Join(dir, txtFile)); err == nil {
			txtExists = true
		}

		switch {
		case lrcExists && !reopen.Synced:
			// Synced lyrics already present and not asked to update -- skip.
			// Gated on the Synced class rather than opts.Update directly so this
			// and the embedded-lyrics hasSynced skip below cannot drift apart.
			slog.Debug("skipping file, lyrics exist", "file", file.Name())
			continue
		case txtExists && !lrcExists && isInstrumentalTxt(filepath.Join(dir, txtFile)):
			// Instrumental markers are re-checkable by provenance (#502): a provider
			// marker is authoritative (terminal), a detector marker is provisional and
			// reopens on --upgrade or a detector-version bump.
			prov, _, provErr := lyrics.ReadInstrumentalProvenance(filepath.Join(dir, txtFile))
			if provErr != nil {
				// Treat an unreadable header as terminal: fail conservatively toward terminal so a transient read error never reopens a settled marker.
				slog.Warn("could not read instrumental provenance; treating marker as terminal", "file", file.Name(), "error", provErr)
			}
			if !instrumentalReopenable(prov, reopen, opts.DetectorVersion) {
				slog.Debug("skipping file, instrumental marker (terminal)", "file", file.Name())
				continue
			}
			// Provisional marker eligible for re-check: fall through to enqueue.
		case txtExists && !lrcExists && !reopen.Unsynced:
			// Unsynced .txt present and not asked to upgrade or update -- skip.
			slog.Debug("skipping file, unsynced lyrics exist", "file", file.Name())
			continue
		}

		if !slices.Contains(supportedFileTypes, ext) {
			slog.Debug("skipping file, unsupported format", "file", file.Name())
			continue
		}

		fullPath := filepath.Join(dir, file.Name())

		// Identity for the metadata-failure skip list: a malformed file reads
		// identically until its mtime or size changes, so a prior failure at the
		// same (mtime, size) means re-reading is wasted work and repeat log noise.
		// info() can fail (race with a delete) -- when it does we skip the feature
		// for this file and read as usual rather than guessing an identity.
		var mtimeNano, size int64
		haveIdentity := false
		if sc.failures != nil {
			if info, ierr := file.Info(); ierr == nil {
				mtimeNano, size, haveIdentity = info.ModTime().UnixNano(), info.Size(), true
				skip, serr := sc.failures.ShouldSkip(ctx, fullPath, mtimeNano, size)
				if serr != nil {
					slog.Warn("metadata-failure lookup failed; reading file anyway", "file", file.Name(), "error", serr)
				} else if skip {
					slog.Debug("skipping file, metadata read previously failed (unchanged)", "file", file.Name())
					continue
				}
			}
		}

		f, err := os.Open(fullPath) //nolint:gosec // path from directory scan
		if err != nil {
			slog.Warn("error reading file", "error", err)
			continue
		}

		m, err := tag.ReadFrom(f)
		if err != nil {
			_ = f.Close()
			slog.Warn("error reading metadata", "file", file.Name(), "error", err)
			// Remember this failure so later scans skip the file (and do not
			// re-warn) until it changes on disk. A record/store error is logged
			// but never fatal -- the scan continues exactly as before.
			if sc.failures != nil && haveIdentity {
				if rerr := sc.failures.RecordFailure(ctx, fullPath, mtimeNano, size, err); rerr != nil {
					slog.Warn("failed to record metadata-read failure", "file", file.Name(), "error", rerr)
				}
			}
			continue
		}

		// Embedded lyrics handling. After sidecar checks and metadata load:
		// "respect" skips a file that already carries embedded lyrics; "extract"
		// writes them to a sidecar and then skips. Synced lyrics (Vorbis
		// SYNCEDLYRICS, stored as LRC text) take precedence and write a .lrc;
		// unsynced lyrics (m.Lyrics(), the ID3 USLT frame) write a .txt. ID3 SYLT
		// and MP4 synced atoms are intentionally not handled. "off" is a no-op.
		if opts.EmbeddedLyrics != "" && opts.EmbeddedLyrics != "off" {
			synced := strings.TrimSpace(syncedLyrics(m))
			unsynced := strings.TrimSpace(m.Lyrics())
			hasSynced := synced != "" && looksLikeLRC(synced)
			if synced != "" && !hasSynced {
				// SYNCEDLYRICS present but not timestamped LRC: never write a bad
				// .lrc -- warn (no silent failure) and fall through to the
				// unsynced/fetch path.
				slog.Warn("embedded SYNCEDLYRICS has no LRC timestamps; ignoring", "file", file.Name())
			}
			if hasSynced || unsynced != "" {
				switch opts.EmbeddedLyrics {
				case "extract":
					// Prefer synced .lrc; else unsynced .txt. On a write failure do
					// NOT skip the track -- fall through and enqueue so a normal
					// fetch is still attempted (never silently dropped).
					if hasSynced {
						if err := extractEmbeddedSyncedLyrics(dir, stem, synced); err != nil {
							slog.Warn("failed to extract embedded synced lyrics; enqueuing for fetch instead", "file", file.Name(), "error", err)
						} else if reopen.Synced {
							// Extraction yielded a synced .lrc, but --update is a full
							// re-fetch: embedded SYNCEDLYRICS is whatever previous
							// tooling happened to write and is frequently worse than a
							// provider result. Skipping here pinned the track to it with
							// no supported way to refresh short of deleting the sidecar
							// by hand (#575). The sidecar is already written -- the
							// extraction above ran first -- so falling through costs
							// nothing and lets a provider fetch replace it.
							//
							// Deliberately NOT gated on lrcExists (captured before
							// extraction): that made the first --update pass write-and-
							// skip while the second re-queued, so two identical passes
							// disagreed. Mirrors the reopen.Unsynced branch below, which
							// is likewise unconditional on an existing sidecar.
							//
							// f is deliberately NOT closed here, for the same reason as
							// the reopen.Unsynced branch below: this falls through to the
							// straight-line region that ends in a single unconditional
							// f.Close(), and probeDuration still reads from the handle
							// when EnrichRecording is set.
							slog.Debug("extracted synced .lrc present; still enqueuing for update re-fetch", "file", file.Name())
						} else {
							_ = f.Close()
							slog.Debug("extracted embedded synced lyrics to .lrc sidecar; skipping fetch", "file", file.Name())
							continue
						}
					} else if err := extractEmbeddedLyrics(dir, stem, unsynced); err != nil {
						slog.Warn("failed to extract embedded lyrics; enqueuing for fetch instead", "file", file.Name(), "error", err)
					} else if reopen.Unsynced {
						// Extraction yielded the UNSYNCED form only, which is not a
						// terminal result. When an upgrade/update pass asked for
						// unsynced work to be reconsidered, the sidecar is written but
						// the fetch must still run so a provider can promote the track
						// to synced. Skipping here froze such tracks permanently at
						// unsynced -- worse than an ordinary unsynced fetch, which
						// upgrade does revisit (#538). Mirrors the txtExists case above,
						// which already defers to reopen.Unsynced.
						//
						// f is deliberately NOT closed here. Unlike the skip branches,
						// which close and continue, this one falls through to the
						// straight-line region below that ends in a single
						// unconditional f.Close(). Closing here would double-close, and
						// would be a use-after-close whenever EnrichRecording is set,
						// since probeDuration still reads from the handle first.
						slog.Debug("extracted embedded lyrics to .txt sidecar; still enqueuing for synced upgrade", "file", file.Name())
					} else {
						_ = f.Close()
						slog.Debug("extracted embedded lyrics to .txt sidecar; skipping fetch", "file", file.Name())
						continue
					}
				default: // "respect"
					_ = f.Close()
					slog.Debug("respecting embedded lyrics; skipping fetch", "file", file.Name())
					continue
				}
			}
		}

		// Recording enrichment (ISRC, MusicBrainz recording ID, duration) is a
		// single switch (#217). When off, skip all three: do not probe duration
		// (avoids the header read entirely), leave ISRC/MBID empty, and let the
		// track keep the duration_bucket=0 fallback.
		filePath := filepath.Join(dir, file.Name())
		var dur int
		var isrc, recordingMBID string
		if opts.EnrichRecording {
			// Duration: audioduration reads only the file header (no audio decode).
			// The library seeks to 0 internally, so f may be at any position from
			// tag.ReadFrom above. Parse errors degrade gracefully to TrackLength=0
			// (duration_bucket sentinel for "unknown").
			var durErr error
			dur, durErr = sc.probeDuration(f, ext)
			if durErr != nil {
				slog.Debug("duration parse failed; using 0", "file", file.Name(), "error", durErr)
				dur = 0
			}
			isrc = extractISRC(m)
			recordingMBID = extractRecordingMBID(m)
		}
		_ = f.Close()

		slog.Debug("adding file", "file", file.Name(), "enrich", opts.EnrichRecording)
		*results = append(*results, models.ScanResult{
			FilePath: filePath,
			Track: models.Track{
				ArtistName:    extractArtist(m),
				TrackName:     m.Title(),
				AlbumName:     m.Album(),
				AlbumArtist:   extractAlbumArtist(m),
				TrackLength:   dur,
				ISRC:          isrc,
				RecordingMBID: recordingMBID,
			},
			Outdir:   dir,
			Filename: stem + ".lrc",
			Status:   "pending",
		})
	}
	return nil
}

// lrcTimestampRe matches a line beginning with an LRC timestamp, e.g.
// "[00:12.34]". Used to vet that embedded SYNCEDLYRICS text is real synced LRC
// before writing it as a .lrc sidecar -- writing prose as .lrc would win over a
// real fetch forever via sidecar precedence.
var lrcTimestampRe = regexp.MustCompile(`(?m)^\[\d{1,2}:\d{2}(?:[.:]\d{1,3})?\]`)

// looksLikeLRC reports whether s carries at least one LRC-timestamped line.
func looksLikeLRC(s string) bool { return lrcTimestampRe.MatchString(s) }

// syncedLyrics returns the embedded Vorbis SYNCEDLYRICS comment (FLAC/OGG) if
// present, else "". dhowden/tag lowercases comment keys, so the raw key is
// "syncedlyrics"; other formats (MP3/MP4) do not surface this key.
func syncedLyrics(m tag.Metadata) string {
	if raw := m.Raw(); raw != nil {
		if s, ok := raw["syncedlyrics"].(string); ok {
			return s
		}
	}
	return ""
}

// linkSidecar writes content to a "<stem>.<ext>" sidecar next to the audio file
// using exclusive-create semantics so an existing sidecar is never overwritten.
// It writes to a temp file in the same directory, then hard-links it into place:
// os.Link fails if the target already exists (atomic never-overwrite), and a
// partial or flush-failed write can never become the canonical sidecar because
// the temp file is always removed and only a fully written, closed file is
// linked. Close errors (where buffered-write failures surface) are returned
// rather than swallowed. An already-present sidecar is treated as success.
func linkSidecar(dir, stem, ext, content string) error {
	path := filepath.Join(dir, stem+"."+ext)
	tmp, err := os.CreateTemp(dir, stem+".*."+ext+".tmp")
	if err != nil {
		return fmt.Errorf("scanner: create temp lyrics sidecar in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("scanner: write lyrics sidecar %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("scanner: flush lyrics sidecar %s: %w", tmpName, err)
	}
	if err := os.Link(tmpName, path); err != nil {
		if os.IsExist(err) {
			return nil // a sidecar already exists; never overwrite it
		}
		return fmt.Errorf("scanner: link lyrics sidecar %s: %w", path, err)
	}
	return nil
}

// extractEmbeddedLyrics writes embedded unsynced lyrics to a "<stem>.txt"
// sidecar (never overwriting an existing one).
func extractEmbeddedLyrics(dir, stem, lyrics string) error {
	return linkSidecar(dir, stem, "txt", lyrics)
}

// extractEmbeddedSyncedLyrics writes embedded synced (LRC) lyrics to a
// "<stem>.lrc" sidecar (never overwriting an existing one).
func extractEmbeddedSyncedLyrics(dir, stem, lyrics string) error {
	return linkSidecar(dir, stem, "lrc", lyrics)
}

// GetSongDir scans a directory for audio files and populates the queue with metadata.
// update causes existing .lrc files to be re-queued (overwrite synced lyrics).
// upgrade causes existing .txt files (previously saved as unsynced) to be re-queued
// so that the tool can check whether synced lyrics are now available and promote them to .lrc.
func (sc *Scanner) GetSongDir(dir string, songs *queue.InputsQueue, update bool, upgrade bool, limit int, depth int, bfs bool) error {
	results, err := sc.ScanLibrary(context.Background(), dir, ScanOptions{
		Update:   update,
		Upgrade:  upgrade,
		MaxDepth: limit - depth,
		BFS:      bfs,
		// Directory mode (legacy one-shot fetch) has no library row to carry a
		// per-library setting, so it preserves the historical always-on
		// enrichment behavior. Library scans resolve this per-library upstream.
		EnrichRecording: true,
	})
	if err != nil {
		return err
	}
	for _, res := range results {
		songs.Push(models.Inputs{
			Track:      res.Track,
			Outdir:     res.Outdir,
			Filename:   res.Filename,
			SourcePath: res.FilePath,
		})
	}
	return nil
}

// ParseInput determines the input mode and populates the work queue accordingly.
func (sc *Scanner) ParseInput(songs []string, outdir string, update bool, upgrade bool, depth int, bfs bool, inputs *queue.InputsQueue) (string, error) {
	if len(songs) == 1 {
		fi, err := os.Stat(songs[0])
		if err == nil {
			if !fi.IsDir() {
				if err := sc.GetSongText(songs[0], outdir, inputs); err != nil {
					return "", err
				}
				return "text", nil
			}
			if err := sc.GetSongDir(songs[0], inputs, update, upgrade, depth, 0, bfs); err != nil {
				return "", err
			}
			return "dir", nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("checking input path %s: %w", songs[0], err)
		}
	}
	sc.GetSongMulti(songs, outdir, inputs)
	return "cli", nil
}
