// Package innertube implements a lyrics provider adapter for YouTube Music's
// unauthenticated internal ("innertube") API.
//
// The flow is three calls, none of them requiring credentials: search
// (artist+title -> videoId), next (videoId -> a lyrics-tab browseId prefixed
// "MPLY"), and browse (browseId -> the lyric content itself).
//
// THE CLIENT STRING SENT WITH EACH REQUEST IS THE WHOLE TRICK. The innertube
// API serves different content for the identical browseId depending on which
// client the request claims to be:
//
//   - "WEB_REMIX" returns plain, unsynced lyric text with no timings at all.
//   - "ANDROID_MUSIC" (version 7.03.52) and "IOS_MUSIC" (version 7.04.2) both
//     return timed cues carrying BOTH a start and an end time per line.
//   - "ANDROID_MUSIC" version 5.16.51 returns HTTP 400.
//
// This is the single most important and least obvious fact about this
// provider: an earlier probe reached the wrong conclusion ("this API only
// offers plain lyrics") purely by using the wrong client string. Anything
// that builds a browse request MUST pick its client deliberately rather than
// defaulting to whatever a generic innertube example uses.
//
// A second trap: the search call never signals "no match". A nonsense query
// still returns a confident, fully-timed, unrelated result -- there is no
// reliable field in the search response that distinguishes a real hit from a
// confident miss (measured during the #848 spike: the one candidate field,
// showingResultsForRenderer, scored 1/4 against nonsense queries). Any caller
// that accepts a search result must independently verify it corresponds to
// the track it asked for.
package innertube
