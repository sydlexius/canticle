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

	// Decode is pure and returns only what the RESPONSE carries -- the cues and,
	// when the payload names one, the upstream licensor. The identity is stamped
	// here instead, where the request and the winning candidate are both in
	// scope. Assigning the field (rather than rebuilding the struct) is what
	// carries Upstream through to the writer; a literal here would silently drop
	// it and no test upstream of the write path would notice.
	song.Track = trackFromCandidate(candidate, track, song)
	return song, nil
}

// trackFromCandidate fills a models.Track from the winning candidate, keeping
// the requested track's values wherever the candidate has none. Mirrors
// internal/petitlyrics's function of the same name.
//
// It takes the decoded song because the two content flags are facts about the
// RESULT, not about the candidate: this lane can return either a timed or an
// untimed payload for the same query (see errors.go, ErrUntimedLyrics), so
// HasSubtitles has to be read off what actually arrived rather than assumed.
//
// Song.AudioDurationSeconds is deliberately NOT set here, and cannot be: it
// lives on Song, not Track, and it must come from the AUDIO FILE rather than
// from a provider's catalog. Stamping the candidate's duration there would make
// the accept-time timing guard compare a lyric against the very length it was
// timed against -- near-circular, and biased toward "fine" (see the field's own
// comment in internal/models). The caller that holds the file stamps it.
func trackFromCandidate(c SearchCandidate, local models.Track, song models.Song) models.Track {
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
	// A song reaching this line carries content: Decode rejects the zero-cue
	// and blank-text payloads as misses on BOTH its paths.
	//
	// "Blank" is precisely what strings.TrimSpace calls blank, which is narrower
	// than "renders as nothing". TrimSpace strips Unicode WHITESPACE and does not
	// strip zero-width characters, so a body of U+200B alone passes both guards
	// and is stamped HasLyrics=1 (measured). That hole predates this change and
	// is equally reachable on the timed path; it is named here rather than
	// claimed away, because an absolute "this is a fact" would be false.
	//
	// HasSubtitles is read off the song rather than assumed. The premise the
	// unconditional version rested on -- "at least one non-empty TIMED cue" --
	// stopped holding once Decode learned to return an unsynced song for an
	// untimed payload, and stamping 1 there would contradict the very Song the
	// flag travels with.
	t.HasLyrics = 1
	// ASSIGNED unconditionally, never only set. `t := local` copies whatever the
	// caller arrived with, so a conditional that can only ever set the flag to 1
	// lets an inbound HasSubtitles=1 survive onto an unsynced result -- leaving
	// the exact defect this reads the song to avoid. Both flags describe THIS
	// result; neither inherits.
	t.HasSubtitles = 0
	if len(song.Subtitles.Lines) > 0 {
		t.HasSubtitles = 1
	}
	return t
}
