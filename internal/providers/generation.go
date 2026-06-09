package providers

import (
	"hash/fnv"
	"sort"
	"strings"
)

// Generation computes a deterministic integer generation from the given set of
// active provider names. The generation changes when the set changes (provider
// added or removed); reordering the slice alone does not change the generation
// because names are sorted before hashing. The generation stamps work_queue rows
// at enqueue time; an item whose stored generation differs from the current
// generation is treated as a cache miss and re-fetched against the new set.
// FNV-64a is used over the sorted, lowercased, comma-joined provider names.
// Aliasing would only cause a spurious cache hit, never a correctness failure.
func Generation(names []string) int {
	normalized := make([]string, 0, len(names))
	for _, n := range names {
		if v := strings.ToLower(strings.TrimSpace(n)); v != "" {
			normalized = append(normalized, v)
		}
	}
	sort.Strings(normalized)
	key := strings.Join(normalized, ",")
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	// Mask to 31 bits so the value is always positive and round-trips safely
	// through SQLite INTEGER columns on both 32-bit and 64-bit platforms.
	return int(h.Sum64() & 0x7fff_ffff)
}
