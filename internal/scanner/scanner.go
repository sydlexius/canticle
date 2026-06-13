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
	"slices"
	"sort"
	"strings"

	"github.com/dhowden/tag"
	"github.com/dhowden/tag/mbz"
	"github.com/lizc2003/audioduration"
	"github.com/sydlexius/mxlrcgo-svc/internal/lyrics"
	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/queue"
)

// supportedFileTypes lists audio file extensions that can have metadata read.
var supportedFileTypes = []string{".mp3", ".m4a", ".m4b", ".m4p", ".alac", ".flac", ".ogg", ".dsf"}

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

// Scanner handles parsing input sources and populating the work queue.
type Scanner struct {
	// probeFunc, when set, is used as the duration probe instead of audioduration
	// (tests only -- lets tests inject a known duration without real audio fixtures).
	probeFunc func(string) (int, error)
}

// ScanOptions controls library directory traversal and queue eligibility.
type ScanOptions struct {
	Update   bool
	Upgrade  bool
	MaxDepth int
	BFS      bool
	// EmbeddedLyrics controls handling of unsynced lyrics embedded in tags:
	// "off" (default) ignores them; "respect" skips fetching a file that already
	// carries embedded lyrics; "extract" writes them to a .txt sidecar (and then
	// skips fetching). Synced (SYLT) tags are intentionally not handled. "off"
	// (default) is a no-op.
	EmbeddedLyrics string
	// EnrichRecording controls recording enrichment: reading ISRC, MusicBrainz
	// recording ID, and duration from audio tags into the Track. When false, all
	// three are skipped (the duration prober is not even invoked) and the track
	// keeps the duration_bucket=0 fallback. Callers resolve this per library via
	// config.ResolveBool; the scheduler stamps it before each ScanLibrary call.
	// The zero value is false, so direct callers that want the historical
	// always-on behavior must set it true explicitly.
	EnrichRecording bool
}

// NewScanner creates a new Scanner.
func NewScanner() *Scanner {
	return &Scanner{}
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
		case lrcExists && !opts.Update:
			// Synced lyrics already present and not asked to update -- skip.
			slog.Debug("skipping file, lyrics exist", "file", file.Name())
			continue
		case txtExists && !lrcExists && !opts.Update && isInstrumentalTxt(filepath.Join(dir, txtFile)):
			// Instrumental markers are terminal -- re-fetching would produce the same result.
			// Skip regardless of --upgrade; only --update (explicit full re-fetch) overrides.
			slog.Debug("skipping file, instrumental marker", "file", file.Name())
			continue
		case txtExists && !opts.Upgrade && !opts.Update && !lrcExists:
			// Unsynced .txt present and not asked to upgrade or update -- skip.
			slog.Debug("skipping file, unsynced lyrics exist", "file", file.Name())
			continue
		}

		if !slices.Contains(supportedFileTypes, ext) {
			slog.Debug("skipping file, unsupported format", "file", file.Name())
			continue
		}

		f, err := os.Open(filepath.Join(dir, file.Name())) //nolint:gosec // path from directory scan
		if err != nil {
			slog.Warn("error reading file", "error", err)
			continue
		}

		m, err := tag.ReadFrom(f)
		if err != nil {
			_ = f.Close()
			slog.Warn("error reading metadata", "file", file.Name(), "error", err)
			continue
		}

		// Embedded (unsynced) lyrics handling. After sidecar checks and metadata
		// load: in "respect" mode, a file that already carries embedded lyrics is
		// skipped (the user has lyrics); in "extract" mode, those lyrics are
		// written to a .txt sidecar and the file is then skipped. SYLT (synced)
		// is intentionally not handled. "off" (default) is a no-op.
		if opts.EmbeddedLyrics != "" && opts.EmbeddedLyrics != "off" {
			if embedded := strings.TrimSpace(m.Lyrics()); embedded != "" {
				switch opts.EmbeddedLyrics {
				case "extract":
					if err := extractEmbeddedLyrics(dir, stem, m.Lyrics()); err != nil {
						// Extraction failed: do NOT skip the track -- fall through
						// and enqueue it so a normal fetch is still attempted
						// (rather than silently dropping it from the pipeline).
						slog.Warn("failed to extract embedded lyrics; enqueuing for fetch instead", "file", file.Name(), "error", err)
					} else {
						_ = f.Close()
						slog.Debug("extracted embedded lyrics to sidecar; skipping fetch", "file", file.Name())
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
				ArtistName:    m.Artist(),
				TrackName:     m.Title(),
				AlbumName:     m.Album(),
				AlbumArtist:   m.AlbumArtist(),
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

// extractEmbeddedLyrics writes embedded unsynced lyrics to a "<stem>.txt"
// sidecar next to the audio file using exclusive-create semantics so an existing
// sidecar is never overwritten. An already-present sidecar is treated as success.
func extractEmbeddedLyrics(dir, stem, lyrics string) error {
	path := filepath.Join(dir, stem+".txt")
	// Write to a temp file in the same directory, then hard-link it into place.
	// os.Link fails if the target already exists (atomic never-overwrite), and a
	// partial or flush-failed write can never become the canonical sidecar
	// because the temp file is always removed and only a fully written, closed
	// file is linked. Close errors (where buffered-write failures surface) are
	// returned rather than swallowed.
	tmp, err := os.CreateTemp(dir, stem+".*.txt.tmp")
	if err != nil {
		return fmt.Errorf("scanner: create temp lyrics sidecar in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.WriteString(lyrics); err != nil {
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
