package scanner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/sidecar"
)

// CachedIdentity is the identity a scan row is built from: the models.Track
// fields scanDir fills from a file's tags. Both sources of a row produce one,
// the tag read and the tag cache, and pendingRow turns either into the row, so
// the two cannot build it differently.
//
// The fields mirror audiometa.Identity one for one, in the same order: the
// adapter in internal/commands converts between the two with a plain struct
// conversion, which stops compiling if either side drifts.
type CachedIdentity struct {
	Artist      string
	AlbumArtist string
	Title       string
	Album       string
	ISRC        string
	MBID        string
	TrackLength int
}

// IdentityStore is the scanner's view of the tag cache: the identity read
// (#1574) and the write of a row from the scan's own read (#1575). It is
// declared here because internal/audiometa imports this package, so the scanner
// cannot name that store directly.
//
// Identity returns the cached identity for path when a row exists that is
// current for the given mtime and size AND was written by the current duration
// and tag readers. found is false otherwise; a miss is not an error. path is
// the canonical spelling audio_metadata is keyed by (see cachedIdentity).
//
// Record stores facts as the row for path, replacing EVERY column and stamping
// the row current for both readers. The scanner therefore only ever passes the
// complete audioFactsFromHandle output, never a subset (see bankFacts).
type IdentityStore interface {
	Identity(ctx context.Context, path string, mtimeNano, size int64) (id CachedIdentity, found bool, err error)
	Record(ctx context.Context, path string, facts AudioFacts) error
}

// WithIdentityStore wires the tag cache so an unsettled file whose cached row
// is current is not opened (#1574), and so a file the scan does read leaves a
// row behind (#1575). Nil disables both: every file is read, nothing is written.
func WithIdentityStore(s IdentityStore) Option {
	return func(sc *Scanner) { sc.identities = s }
}

// identityStats counts one scan's tag-cache outcomes. It is owned by a single
// ScanLibrary call and logged once at its end; a per-file line would carry a
// path, and the store's error text does too.
type identityStats struct {
	hits, misses, lookupErrors, bankErrors  int
	recorded, recordErrors, multiValueSkips int
	firstError                              string
}

// cacheMiss is the stat a tag-cache lookup CLEANLY missed on: a store is
// wired, the lookup ran without error and found no current row. It is the zero
// value in every other case, and the zero value never equals a read handle's
// stamp, so only a clean miss lets the read that follows be banked.
type cacheMiss struct{ mtimeNano, size int64 }

// fail counts a store error in n and keeps the scan's first one as a cause that
// cannot name a file: the INNERMOST error's text, as every wrapper quotes the
// path, or only its type if the chain holds a path error or the text contains
// the file's base name, its directory or the key's directory (never a vacuous
// "", "." or root candidate, which would match text that names nothing).
func (st *identityStats) fail(n *int, err error, fullPath, key string) {
	*n++
	if st.firstError != "" {
		return
	}
	inner := err
	for u := errors.Unwrap(inner); u != nil; u = errors.Unwrap(inner) {
		inner = u
	}
	st.firstError = inner.Error()
	redact := errors.As(err, new(*fs.PathError)) || errors.As(err, new(*os.LinkError))
	for _, s := range []string{filepath.Base(fullPath), filepath.Dir(fullPath), filepath.Dir(key)} {
		redact = redact || (s != "" && s != "." && s != string(filepath.Separator) && strings.Contains(st.firstError, s))
	}
	if redact {
		st.firstError = fmt.Sprintf("%T", inner)
	}
}

// log reports the scan's tag-cache counts: at Warn when a lookup, a duration
// bank or a row write failed, at Debug otherwise. Counts, plus the first error's path-free
// cause (see fail) on the Warn line.
func (st *identityStats) log(ctx context.Context) {
	level, args := slog.LevelDebug, []any{"hits", st.hits, "misses", st.misses,
		"lookup_errors", st.lookupErrors, "duration_bank_errors", st.bankErrors,
		"recorded", st.recorded, "record_errors", st.recordErrors, "multivalue_skips", st.multiValueSkips}
	if st.lookupErrors > 0 || st.bankErrors > 0 || st.recordErrors > 0 {
		level, args = slog.LevelWarn, append(args, "first_error", st.firstError)
	}
	slog.Log(ctx, level, "scan identity cache", args...)
}

// embeddedLyricsOn reports whether the scan must look at a file's embedded
// lyrics. Those are deliberately not cached, so such a scan reads every
// unsettled file.
func (o ScanOptions) embeddedLyricsOn() bool {
	return o.EmbeddedLyrics != "" && o.EmbeddedLyrics != "off"
}

// statFollowing stats the file a directory entry names, following a symlink.
// DirEntry.Info is an lstat, which for a symlinked audio file describes the
// link; the cache row is stamped from the open handle of the TARGET, so the
// link's own mtime and size would never match it. The entry's type bit comes
// from the directory read, so telling the two apart costs nothing. lstat is the
// entry's Info when the caller already has it, saving a second lstat.
func statFollowing(file os.DirEntry, fullPath string, lstat os.FileInfo) (os.FileInfo, error) {
	if file.Type()&fs.ModeSymlink != 0 {
		return os.Stat(fullPath)
	}
	if lstat != nil {
		return lstat, nil
	}
	return file.Info()
}

// cachedIdentity returns the tag-cache identity for an unsettled file, and
// whether it may be used INSTEAD of opening the file.
//
// The caller runs the metadata-failure skip first, so a file that skip covers
// never reaches this. A hit then needs all of: a store is wired, embedded
// lyrics are off, the stat succeeded, and the store reports a current row.
// Anything else returns false and the caller reads the file. In particular a
// lookup ERROR is not evidence of anything: the file is read, the track is
// kept, and no failure is recorded against it.
//
// key is the audio_metadata key: the path rebased onto the canonical library
// root, the same spelling the duration bank in scanDir uses and the one
// `scan index-metadata` writes.
//
// On a hit with recording enrichment on, the cached duration is banked into
// the duration cache against the same (mtime, size), which is what the read
// path does from its open handle, but only when ABSENT (the gate
// bankDurationForSkippedFile uses): this runs for every cached file on every
// scan, and an unchanged scan must not write. Enrichment off banks nothing.
//
// The third result is the stat a CLEAN miss was looked up on, for bankFacts.
// It is zero for a hit, a failed stat, a lookup error, and a scan that
// made no lookup at all.
func (sc *Scanner) cachedIdentity(ctx context.Context, opts ScanOptions, file os.DirEntry, lstat os.FileInfo, fullPath, key string, stats *identityStats) (CachedIdentity, bool, cacheMiss) {
	if sc.identities == nil || opts.embeddedLyricsOn() {
		return CachedIdentity{}, false, cacheMiss{}
	}
	info, err := statFollowing(file, fullPath, lstat)
	if err != nil {
		stats.misses++
		return CachedIdentity{}, false, cacheMiss{}
	}
	mtimeNano, size := info.ModTime().UnixNano(), info.Size()
	id, found, err := sc.identities.Identity(ctx, key, mtimeNano, size)
	if err != nil {
		stats.fail(&stats.lookupErrors, err, fullPath, key)
		return CachedIdentity{}, false, cacheMiss{}
	}
	if !found {
		stats.misses++
		return CachedIdentity{}, false, cacheMiss{mtimeNano: mtimeNano, size: size}
	}
	stats.hits++
	if opts.EnrichRecording && sc.durations != nil && id.TrackLength > 0 {
		_, banked, err := sc.durations.Lookup(ctx, key, mtimeNano, size)
		if err == nil && !banked {
			err = sc.durations.Record(ctx, key, mtimeNano, size, id.TrackLength)
		}
		if err != nil {
			stats.fail(&stats.bankErrors, err, fullPath, key)
		}
	}
	return id, true, cacheMiss{}
}

// bankFacts writes the tag-cache row for a file the scan just read, from that
// same read, so the next scan (and the worker) is answered from the cache with
// no `scan index-metadata` run in between (#1575). It never opens the file.
//
// A row is written only when ALL of these hold; otherwise the file is simply
// read again next time, which is what happened before this write existed:
//
//   - Recording enrichment is on. Off, the duration was never probed, and a row
//     with duration 0 stamped current reads as "duration genuinely absent" to
//     the worker and to revalidate.
//   - A store is wired, and the duration parse did not fail. That failure can
//     be transient: the row would pin duration 0 until the file itself changed.
//   - The Vorbis multi-value re-read hit no I/O error (FLAC/OGG only). That is
//     transient too, and the row would pin the single-value fallback artist,
//     half of the queue's unique key. Such a read is counted, never logged.
//   - The open handle's own stamp is non-zero and EQUALS the stat the lookup
//     CLEANLY missed on. A difference means the file changed between that stat
//     and the end of the read, so the facts may mix two versions of it. miss is
//     zero, so nothing is written, after a lookup error (which is not evidence
//     the row is absent) and on an embedded-lyrics scan (which makes no lookup);
//     a hit never reaches here, so an unchanged warm scan writes nothing.
//
// facts is always the complete audioFactsFromHandle output: Record replaces
// every column, so a subset would blank the rest while stamping them current.
//
// A failed write is counted and never fails or drops the track: the scan row is
// already built. A write a canceled context cut short is shutdown, not a fault,
// and is not counted.
func (sc *Scanner) bankFacts(ctx context.Context, opts ScanOptions, key, fullPath string, facts AudioFacts, miss cacheMiss, stats *identityStats) {
	if sc.identities == nil || !opts.EnrichRecording || facts.DurationErr != nil {
		return
	}
	if facts.MultiValueErr != nil {
		stats.multiValueSkips++
		return
	}
	stamped := facts.MTimeNano != 0 || facts.SizeBytes != 0
	if !stamped || facts.MTimeNano != miss.mtimeNano || facts.SizeBytes != miss.size {
		return
	}
	err := sc.identities.Record(ctx, key, facts)
	switch {
	case err == nil:
		stats.recorded++
	case !errors.Is(err, context.Canceled):
		stats.fail(&stats.recordErrors, err, fullPath, key)
	}
}

// pendingRow builds the scan row for an unsettled file from its identity,
// whichever source that came from.
//
// Recording enrichment is one switch (#217): when it is off the row carries no
// ISRC, recording ID or duration. The read path never extracts them in that
// case; a cached row always holds them, so the switch is applied here, once,
// for both.
func pendingRow(dir, filePath, stem string, id CachedIdentity, opts ScanOptions) models.ScanResult {
	if !opts.EnrichRecording {
		id.ISRC, id.MBID, id.TrackLength = "", "", 0
	}
	return models.ScanResult{
		FilePath: filePath,
		Track: models.Track{
			ArtistName:    id.Artist,
			TrackName:     id.Title,
			AlbumName:     id.Album,
			AlbumArtist:   id.AlbumArtist,
			TrackLength:   id.TrackLength,
			ISRC:          id.ISRC,
			RecordingMBID: id.MBID,
		},
		Outdir:   dir,
		Filename: stem + sidecar.ExtLineSynced,
		Status:   "pending",
	}
}
