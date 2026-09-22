package providers

import (
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
)

// WordCapabilityRevision versions word extraction across the word-capable
// lanes (#982). Bump it deliberately when a lane's word extraction changes (a
// new richsync request shape, a coverage-threshold decision, a new word-capable
// provider): the bump changes WordGeneration, which re-opens every "no word
// data" verdict stamped under the old value, without touching Generation and
// its library-wide cache bypass.
const WordCapabilityRevision = 1

// WordCapable reports whether the named lane can serve word-level timings:
// Musixmatch (the bundled richsync sub-call) and PetitLyrics (its word-sync
// tier). InnerTube serves line cues only, and the detector lane serves no
// lyrics at all, so both -- and any unknown name -- are false.
func WordCapable(name string) bool {
	switch NormalizeName(name) {
	case Musixmatch, PetitLyrics:
		return true
	}
	return false
}

// WordGeneration fingerprints the word-capable subset of the active lane set
// together with WordCapabilityRevision. A "no word data" verdict is valid only
// under the generation it was reached with, so adding, removing or disabling a
// word-capable lane, or bumping the revision, expires it. Lanes that are not
// word-capable are ignored, so enabling InnerTube expires nothing. Like
// Generation, names are normalized, de-duplicated and sorted, so order does
// not matter, and the FNV-64a hash is masked to 31 bits so it round-trips
// through a SQLite INTEGER on any platform.
func WordGeneration(laneNames []string) int64 {
	return wordGeneration(WordCapabilityRevision, laneNames)
}

func wordGeneration(revision int, laneNames []string) int64 {
	seen := make(map[string]struct{}, len(laneNames))
	names := make([]string, 0, len(laneNames))
	for _, n := range laneNames {
		v := NormalizeName(n)
		if _, dup := seen[v]; dup || !WordCapable(v) {
			continue
		}
		seen[v] = struct{}{}
		names = append(names, v)
	}
	sort.Strings(names)
	h := fnv.New64a()
	_, _ = h.Write([]byte("wordcap:v" + strconv.Itoa(revision) + "|" + strings.Join(names, ",")))
	return int64(h.Sum64() & 0x7fff_ffff)
}
