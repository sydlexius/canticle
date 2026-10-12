package scanner

// TagReaderVersion identifies the code that turns a file's tags into the
// identity values a scan row is built from. internal/audiometa stamps it on
// every audio_metadata row it writes (migration 072) and its identity read
// requires it to match.
//
// BUMP IT whenever a change alters what the tag read returns for artist,
// title, album, album artist, ISRC or recording ID for an UNCHANGED file: a
// change to extractArtist, extractAlbumArtist, extractISRC or
// extractRecordingMBID, to the multi-value recovery in vorbis_multivalue.go or
// mp4_multivalue.go, or a tag-library upgrade that moves any of those values.
// The file's mtime and size do not change when the reader does, so without the
// bump a row cached by the old reader keeps reading as current.
//
// Those values key scan_results and work_queue (artist + title), so serving a
// stale one re-keys a track. Err toward bumping when unsure: a miss costs one
// re-read of the file, never a remediation. A bump makes every existing row a
// miss for that read until the row is re-recorded (see audiometa.Identity).
//
// It is separate from DurationReaderVersion on purpose. That one names the
// duration parser and is shared with audio_durations; bumping it for a tag
// change would invalidate every cached duration too (the reason migration 071
// cleared stamps instead). Hand-set for the same reason that one is: nothing
// cheap computes "the tag read changed".
const TagReaderVersion = 1
