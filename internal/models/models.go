package models

import "time"

// Track represents a song's metadata from the Musixmatch API.
type Track struct {
	TrackName    string `json:"track_name,omitempty"`
	ArtistName   string `json:"artist_name,omitempty"`
	AlbumName    string `json:"album_name,omitempty"`
	AlbumArtist  string `json:"album_artist,omitempty"`
	TrackLength  int    `json:"track_length,omitempty"`
	Instrumental int    `json:"instrumental,omitempty"`
	HasLyrics    int    `json:"has_lyrics,omitempty"`
	HasSubtitles int    `json:"has_subtitles,omitempty"`
	// ISRC, SpotifyID, and RecordingMBID are recording-level identifiers fed to
	// the matcher when present (see internal/musixmatch). ISRC and RecordingMBID
	// are populated from audio tags during library scans; SpotifyID is populated
	// only by the fetch --probe diagnostic flags.
	ISRC          string `json:"isrc,omitempty"`
	SpotifyID     string `json:"spotify_id,omitempty"`
	RecordingMBID string `json:"recording_mbid,omitempty"`
}

// Lyrics holds unsynced lyrics text.
type Lyrics struct {
	LyricsBody string `json:"lyrics_body,omitempty"`
}

// Synced holds time-synced subtitle lines.
type Synced struct {
	Lines []Lines
}

// Lines represents a single synced lyrics line with text and timestamp.
type Lines struct {
	Text string `json:"text,omitempty"`
	Time Time   `json:"time,omitempty"`
}

// Time represents a timestamp for a synced lyrics line.
type Time struct {
	Total      float64 `json:"total,omitempty"`
	Minutes    int     `json:"minutes,omitempty"`
	Seconds    int     `json:"seconds,omitempty"`
	Hundredths int     `json:"hundredths,omitempty"`
}

// Song represents the complete result from a lyrics lookup.
type Song struct {
	Track     Track
	Lyrics    Lyrics
	Subtitles Synced
	// TranslationSubtitles holds an optional translation track parallel to
	// Subtitles. Zero value (empty Lines) means absent, matching the Subtitles
	// convention. Used for opt-in bilingual interleaved output (see #146).
	TranslationSubtitles Synced
	// RomanizationSubtitles holds an optional romanization track parallel to
	// Subtitles. Zero value (empty Lines) means absent. Not interleaved by
	// default; reserved for a future romanization output flag.
	RomanizationSubtitles Synced
	// WinningLane is the provider lane name that returned this song. It is set by
	// the orchestrator for both suitable results and best-available fallbacks.
	// Empty on cache hits and zero-value songs. Used by the worker write-path to
	// record per-lane hit counters without changing the Fetcher interface.
	WinningLane string `json:"-"`
	// LaneAttempts carries the per-lane hit/miss attribution for THIS track out of
	// the orchestrator so the worker can persist a true per-track hit-rate (issue
	// #282). One entry per ATTEMPTED lane: Hit is true for the winning lane and
	// false for every other attempted lane (including lanes that lost to a later
	// winner). Empty on cache hits and zero-value songs. Transient: not persisted
	// as part of the song and never serialized.
	LaneAttempts []LaneAttempt `json:"-"`
	// FetchedAt is the time the song was fetched from the provider. Set by the
	// worker immediately after a successful fetch and before any write path so
	// all output formats share one consistent timestamp. Zero on cache hits
	// (timestamp not available without a live fetch). Transient: not persisted.
	FetchedAt time.Time `json:"-"`
	// DetectorVersion is the audio detector's version string when this song is an
	// instrumental verdict written by the detector (Track.Instrumental == 1 via the
	// detector path). Empty for provider-sourced instrumentals and all other songs.
	// The writer uses it to stamp [source:canticle-detector] + [dv:] provenance so
	// a detector marker is later distinguishable from an editorial provider marker.
	// Transient: not persisted, not serialized.
	DetectorVersion string `json:"-"`
	// Detector telemetry accompanying a detector-sourced instrumental verdict.
	// Transient (never serialized): the worker reads them off the orchestrator
	// result to stamp queue.InstrumentalTelemetry. Zero on non-detector songs.
	DetectorMusicSum   float64 `json:"-"`
	DetectorVocalPeak  float64 `json:"-"`
	DetectorSpeechMean float64 `json:"-"`
	DetectorVocalClass string  `json:"-"`
}

// LaneAttempt is one provider lane's outcome for a single track: the lane name
// and whether it served the track (Hit) or was attempted and lost (miss). The
// orchestrator emits one per attempted lane on Song.LaneAttempts; the worker
// persists them to lane_attempts (migration 022) for a true per-track hit-rate.
type LaneAttempt struct {
	Lane string
	Hit  bool
	// Local reports whether the lane resolved the track without an outbound
	// provider request (the detector lane today). The worker uses this to decide
	// whether an item consumed the provider-request pacing budget: an item whose
	// every attempted lane is Local made no provider request and must not be
	// throttled at the provider rate (#534).
	//
	// Deliberately per-attempt rather than looked up from a lane registry, so a
	// caller that is not orchestrator-driven still carries the attribution with
	// the song, and a lane rename cannot silently change the classification.
	Local bool
}

// Inputs represents a single work item in the processing queue.
type Inputs struct {
	Track       Track
	Outdir      string
	Filename    string
	SourcePath  string
	OutputPaths []OutputPath
	// ScanResultID links this work item back to its originating scan_results row.
	// Zero means the item did not originate from a library scan (e.g. ad-hoc fetch).
	ScanResultID int64
	// DetectInstrumental carries the per-item instrumental-detection decision
	// resolved at enqueue time (CLI > per-library > global). It is stamped onto the
	// work_queue row on initial insert. nil means "no decision" -> the worker falls
	// back to the global config default. Set by the scan enqueuer; other enqueue
	// paths (e.g. webhook) leave it nil.
	DetectInstrumental *bool
}

// OutputPath represents one lyrics output destination.
type OutputPath struct {
	Outdir   string `json:"outdir,omitempty"`
	Filename string `json:"filename,omitempty"`
}

// Library represents a configured music library root.
type Library struct {
	ID        int64  `json:"id,omitempty"`
	Path      string `json:"path,omitempty"`
	Name      string `json:"name,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`

	// EnrichRecording and DetectInstrumental are tri-state per-library toggles.
	// A nil pointer means "inherit the global default"; a non-nil pointer is an
	// explicit on/off. They map to nullable INTEGER columns (NULL/0/1).
	EnrichRecording    *bool `json:"enrich_recording,omitempty"`
	DetectInstrumental *bool `json:"detect_instrumental,omitempty"`
}

// LibrarySettings carries the per-library tri-state toggles for create/update
// operations. A nil field means "inherit" on Add and "leave unchanged" on
// Update; a non-nil field is an explicit on/off.
type LibrarySettings struct {
	EnrichRecording    *bool
	DetectInstrumental *bool
}

// ScanResult represents an audio file discovered during a library scan.
type ScanResult struct {
	ID        int64  `json:"id,omitempty"`
	LibraryID int64  `json:"library_id,omitempty"`
	FilePath  string `json:"file_path,omitempty"`
	Track     Track  `json:"track,omitempty"`
	Outdir    string `json:"outdir,omitempty"`
	Filename  string `json:"filename,omitempty"`
	Status    string `json:"status,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}
