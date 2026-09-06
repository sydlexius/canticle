package innertube

import (
	"context"
	"fmt"
	"strings"

	"github.com/sydlexius/canticle/internal/models"
)

// FindLyrics looks up timed lyrics for a track, composing the three-call chain
// documented in doc.go: search for candidates, verify one corresponds to the
// requested track, follow it to a lyrics-tab browseId, and decode the timed
// cues that browse returns.
//
// PACING IS PER OUTBOUND REQUEST, NOT PER FindLyrics, and that is a decision
// this slice owed an answer to rather than a detail. The interval exists to
// bound the rate at which canticle draws on someone else's gateway, and a
// gateway counts REQUESTS -- it has no notion of our lookups. Pacing per
// FindLyrics would satisfy the configured interval on paper while firing three
// back-to-back requests for every hit: a 3x burst, which is the precise shape
// the floor exists to prevent. So the wait lives in postJSON, the single point
// all three calls funnel through, and no call site can opt out of it.
//
// The consequence is asymmetric cost, and it is the honest one. A MISS costs
// one interval: selection rejects the candidate before next() or browse() is
// ever issued, so a track this provider has nothing for is paid for once. A HIT
// costs three, because a hit genuinely consumes three times as much of the
// shared resource. That asymmetry is the right way round for a fallback lane,
// whose traffic is mostly misses.
//
// Every step is ctx-cancellable: the pacer's wait selects on ctx.Done, and each
// HTTP call is built with NewRequestWithContext, so a canceled context stops
// the chain at whichever of the two it is sitting in.
func (c *Client) FindLyrics(ctx context.Context, track models.Track) (models.Song, error) {
	// REFUSED BEFORE ANY I/O. With both fields blank the query would be empty
	// and the answer undecidable: SelectCandidate's gate rejects on "no
	// comparable field to verify the candidate against", a verdict reachable
	// from the input alone. Issuing the search anyway spends a request AND a
	// full pacing interval to be told something already known.
	//
	// This takes up the flag checkCorresponds left for "whoever lands the query
	// builder" -- it reasoned that an all-blank request could not produce a
	// meaningful query, but could not verify it because no caller existed yet.
	// FindLyrics is that caller, and the reasoning holds.
	//
	// Deliberately requires BOTH to be blank. One blank field is still
	// verifiable: the gate skips it as non-comparable and holds the other to
	// the floor alone, which is a legitimate lookup.
	if strings.TrimSpace(track.ArtistName) == "" && strings.TrimSpace(track.TrackName) == "" {
		return models.Song{}, fmt.Errorf(
			"innertube: track has neither artist nor title to search on: %w", ErrNotFound)
	}

	candidates, err := c.Search(ctx, track.ArtistName, track.TrackName)
	if err != nil {
		return models.Song{}, err
	}

	// THE GATE RUNS BEFORE ANY FURTHER REQUEST, which is both a correctness and
	// a cost property. Correctness: search never signals "no match" (doc.go), so
	// an unverified candidate would send us fetching a confident, fully-timed,
	// unrelated lyric. Cost: a rejection returns here having spent exactly one
	// request.
	candidate, err := SelectCandidate(candidates, track)
	if err != nil {
		return models.Song{}, err
	}

	browseID, err := c.Next(ctx, candidate.VideoID)
	if err != nil {
		return models.Song{}, err
	}

	raw, err := c.Browse(ctx, browseID)
	if err != nil {
		return models.Song{}, err
	}

	song, err := Decode(raw)
	if err != nil {
		return models.Song{}, err
	}

	// Decode is pure and returns only Subtitles; the identity is stamped here,
	// where the request and the winning candidate are both in scope.
	song.Track = trackFromCandidate(candidate, track)
	return song, nil
}

// trackFromCandidate fills a models.Track from the winning candidate, keeping
// the requested track's values wherever the candidate has none. Mirrors
// internal/petitlyrics's function of the same name.
//
// Song.AudioDurationSeconds is deliberately NOT set here, and cannot be: it
// lives on Song, not Track, and it must come from the AUDIO FILE rather than
// from a provider's catalog. Stamping the candidate's duration there would make
// the accept-time timing guard compare a lyric against the very length it was
// timed against -- near-circular, and biased toward "fine" (see the field's own
// comment in internal/models). The caller that holds the file stamps it.
func trackFromCandidate(c SearchCandidate, local models.Track) models.Track {
	t := local
	if c.Title != "" {
		t.TrackName = c.Title
	}
	if c.Artist != "" {
		t.ArtistName = c.Artist
	}
	if c.DurationSeconds > 0 {
		t.TrackLength = c.DurationSeconds
	}
	// A song reaching this line came back from Decode with at least one
	// non-empty timed cue -- ExtractCues rejects the zero-cue and all-empty-text
	// payloads as misses -- so both flags are facts about this result, not
	// optimism.
	t.HasLyrics = 1
	t.HasSubtitles = 1
	return t
}
