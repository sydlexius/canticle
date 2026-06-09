package providers

import (
	"hash/fnv"
	"sort"
	"strings"
)

// Generation computes a deterministic integer generation from the given set of
// active provider names. The generation changes when the set changes (provider
// added or removed); reordering or duplicating entries does not change it,
// because names are lowercased, trimmed, de-duplicated, and sorted before
// hashing, so the result is a function of the provider set. The generation
// stamps work_queue rows at enqueue time; an item whose stored generation
// differs from the current generation is treated as a cache miss and re-fetched
// against the new set. FNV-64a is used over the canonical comma-joined names.
func Generation(names []string) int {
	normalized := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, n := range names {
		v := strings.ToLower(strings.TrimSpace(n))
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		normalized = append(normalized, v)
	}
	sort.Strings(normalized)
	key := strings.Join(normalized, ",")
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	// Mask to 31 bits so the value is always positive and round-trips safely
	// through SQLite INTEGER columns on both 32-bit and 64-bit platforms.
	return int(h.Sum64() & 0x7fff_ffff)
}
