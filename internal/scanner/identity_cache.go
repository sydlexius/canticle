package scanner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
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

// IdentityStore is the scanner's read of the tag cache (#1574). It is declared
// here because internal/audiometa imports this package, so the scanner cannot
// name that store directly.
//
// Identity returns the cached identity for path when a row exists that is
// current for the given mtime and size AND was written by the current duration
// and tag readers. found is false otherwise; a miss is not an error. path is
// the canonical spelling audio_metadata is keyed by (see cachedIdentity).
//
// The scanner only READS through this seam. Nothing here writes a row.
type IdentityStore interface {
	Identity(ctx context.Context, path string, mtimeNano, size int64) (id CachedIdentity, found bool, err error)
}

// WithIdentityStore wires the tag cache so an unsettled file whose cached row
// is current is not opened (#1574). Nil disables it: every file is read.
func WithIdentityStore(s IdentityStore) Option {
	return func(sc *Scanner) { sc.identities = s }
}

// identityStats counts one scan's tag-cache outcomes. It is owned by a single
// ScanLibrary call and logged once at its end; a per-file line would carry a
// path, and the store's error text does too.
type identityStats struct {
	hits, misses, lookupErrors, bankErrors int
	firstError                             string
}

// fail counts a store error in n and keeps the scan's first one as a cause that
// cannot name a file: the INNERMOST error's text, as every wrapper quotes the
// path, or only its type if the chain holds a path error or the text the name.
func (st *identityStats) fail(n *int, err error, name string) {
	*n++
	if st.firstError != "" {
		return
	}
	inner := err
	for u := errors.Unwrap(inner); u != nil; u = errors.Unwrap(inner) {
		inner = u
	}
	st.firstError = inner.Error()
	if errors.As(err, new(*fs.PathError)) || errors.As(err, new(*os.LinkError)) || strings.Contains(st.firstError, name) {
		st.firstError = fmt.Sprintf("%T", inner)
	}
}

// log reports the scan's tag-cache counts: at Warn when a lookup or a duration
// bank failed, at Debug otherwise. Counts, plus the first error's path-free
// cause (see fail) on the Warn line.
func (st *identityStats) log(ctx context.Context) {
	level, args := slog.LevelDebug, []any{"hits", st.hits, "misses", st.misses,
		"lookup_errors", st.lookupErrors, "duration_bank_errors", st.bankErrors}
	if st.lookupErrors > 0 || st.bankErrors > 0 {
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
func (sc *Scanner) cachedIdentity(ctx context.Context, opts ScanOptions, file os.DirEntry, lstat os.FileInfo, fullPath, key string, stats *identityStats) (CachedIdentity, bool) {
	if sc.identities == nil || opts.embeddedLyricsOn() {
		return CachedIdentity{}, false
	}
	info, err := statFollowing(file, fullPath, lstat)
	if err != nil {
		stats.misses++
		return CachedIdentity{}, false
	}
	mtimeNano, size := info.ModTime().UnixNano(), info.Size()
	id, found, err := sc.identities.Identity(ctx, key, mtimeNano, size)
	if err != nil {
		stats.fail(&stats.lookupErrors, err, file.Name())
		return CachedIdentity{}, false
	}
	if !found {
		stats.misses++
		return CachedIdentity{}, false
	}
	stats.hits++
	if opts.EnrichRecording && sc.durations != nil && id.TrackLength > 0 {
		_, banked, err := sc.durations.Lookup(ctx, key, mtimeNano, size)
		if err == nil && !banked {
			err = sc.durations.Record(ctx, key, mtimeNano, size, id.TrackLength)
		}
		if err != nil {
			stats.fail(&stats.bankErrors, err, file.Name())
		}
	}
	return id, true
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
