package revalidate

// Benchmark comparing the two candidate fixes for #691 (companionAudio's
// per-sidecar os.ReadDir): a bounded stem-probe (os.Stat per audio
// extension, the SHIPPED companionAudio below -- called directly, not
// reimplemented, so this benchmark cannot drift from production behavior)
// versus a cached directory listing (one os.ReadDir per directory, reused
// for every sidecar in it, reproduced here standalone purely for
// measurement since it was NOT the option chosen).
//
// The distinguishing case named in #691 is a FLAT library: many sidecars in
// one large directory. These benchmarks sweep directory size (10..5000
// entries) and measure resolving ALL sidecars in the directory once, which
// mirrors walkRoot's actual access pattern (many companionAudio calls
// against the same directory during one pass).
//
// READ THE RESULT HONESTLY. Option 2 appears TWICE on purpose:
//
//   - indexedReadDirCompanion is Option 2 done WELL (index the listing once,
//     O(1) per sidecar after). It is the comparator that counts, and it comes
//     back CPU-COMPARABLE to the shipped probe -- each wins at two of the four
//     sizes, non-monotonically, which at this sample count is noise, not a
//     trend. The probe is NOT chosen because it is faster here; it is chosen
//     because it issues no directory read at all (see companionAudio's comment).
//   - cachedReadDirCompanion is Option 2 done NAIVELY (listing cached but
//     rescanned linearly per sidecar, still O(N^2)). It loses badly, and it is
//     kept only as a labeled worst case.
//
// An earlier revision shipped ONLY the naive variant, and the doc comment drew
// a "faster at every size" conclusion from it. That conclusion did not survive
// adding the fair comparator. If a future change adds another alternative here,
// implement its BEST form -- a benchmark that only beats a strawman measures
// nothing.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var benchAudioExts = []string{".mp3", ".m4a", ".m4b", ".m4p", ".alac", ".flac", ".ogg", ".dsf"}

// uncachedReadDirCompanion reproduces the ORIGINAL #691 bug: one os.ReadDir
// per sidecar, the baseline both candidate fixes are measured against.
func uncachedReadDirCompanion(lrcPath string) (string, bool) {
	dir := filepath.Dir(lrcPath)
	stem := strings.TrimSuffix(lrcPath, filepath.Ext(lrcPath))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		matched := false
		for _, ext := range benchAudioExts {
			if strings.EqualFold(filepath.Ext(e.Name()), ext) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if strings.TrimSuffix(p, filepath.Ext(p)) == stem {
			return p, true
		}
	}
	return "", false
}

// indexedReadDirCompanion is Option 2 implemented WELL: one os.ReadDir per
// directory, and the listing is turned into a stem -> path INDEX once, so each
// subsequent sidecar in that directory is an O(1) map lookup.
//
// This is the honest comparator, and it is the one to beat. Its naive sibling
// below caches the listing but then rescans all N entries per sidecar, which is
// still O(N^2) over a directory -- benchmarking only against that would flatter
// the stem probe by comparing it to a strawman rather than to the alternative
// #691 actually proposed. Both are kept so the benchmark shows the difference
// between the option and a careless implementation of it.
func indexedReadDirCompanion(cache map[string]map[string]string, lrcPath string) (string, bool) {
	dir := filepath.Dir(lrcPath)
	stem := strings.TrimSuffix(lrcPath, filepath.Ext(lrcPath))
	index, ok := cache[dir]
	if !ok {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return "", false
		}
		index = make(map[string]string, len(entries))
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			matched := false
			for _, ext := range benchAudioExts {
				if strings.EqualFold(filepath.Ext(name), ext) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			p := filepath.Join(dir, name)
			index[strings.TrimSuffix(p, filepath.Ext(p))] = p
		}
		cache[dir] = index
	}
	p, found := index[stem]
	return p, found
}

// cachedReadDirCompanion is Option 2 implemented NAIVELY: the listing is cached
// but rescanned linearly for every sidecar, so the repeated syscall goes away
// while the per-sidecar linear scan does not. Kept as a labeled worst case, NOT
// as the comparator any conclusion rests on -- see indexedReadDirCompanion.
func cachedReadDirCompanion(cache map[string][]os.DirEntry, lrcPath string) (string, bool) {
	dir := filepath.Dir(lrcPath)
	stem := strings.TrimSuffix(lrcPath, filepath.Ext(lrcPath))
	entries, ok := cache[dir]
	if !ok {
		var err error
		entries, err = os.ReadDir(dir)
		if err != nil {
			return "", false
		}
		cache[dir] = entries
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		matched := false
		for _, ext := range benchAudioExts {
			if strings.EqualFold(filepath.Ext(e.Name()), ext) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if strings.TrimSuffix(p, filepath.Ext(p)) == stem {
			return p, true
		}
	}
	return "", false
}

// buildFlatLibrary creates n sidecar+audio pairs directly in one directory
// (the flat-library, worst-case-for-ReadDir shape #691 names).
func buildFlatLibrary(b *testing.B, n int) (dir string, lrcPaths []string) {
	b.Helper()
	dir = b.TempDir()
	for i := 0; i < n; i++ {
		base := "track" + strconv.Itoa(i)
		if err := os.WriteFile(filepath.Join(dir, base+".mp3"), []byte("x"), 0o600); err != nil {
			b.Fatalf("write audio: %v", err)
		}
		lrc := filepath.Join(dir, base+".lrc")
		if err := os.WriteFile(lrc, []byte("x"), 0o600); err != nil {
			b.Fatalf("write lrc: %v", err)
		}
		lrcPaths = append(lrcPaths, lrc)
	}
	return dir, lrcPaths
}

func BenchmarkCompanionAudio(b *testing.B) {
	for _, n := range []int{10, 100, 1000, 5000} {
		_, lrcPaths := buildFlatLibrary(b, n)

		b.Run("StemProbe(shipped)/n="+strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, p := range lrcPaths {
					if _, ok := companionAudio(p); !ok {
						b.Fatal("expected a match")
					}
				}
			}
		})

		b.Run("IndexedReadDir(fair-option-2)/n="+strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				cache := make(map[string]map[string]string)
				for _, p := range lrcPaths {
					if _, ok := indexedReadDirCompanion(cache, p); !ok {
						b.Fatal("expected a match")
					}
				}
			}
		})

		b.Run("CachedReadDir(naive-option-2)/n="+strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				cache := make(map[string][]os.DirEntry)
				for _, p := range lrcPaths {
					if _, ok := cachedReadDirCompanion(cache, p); !ok {
						b.Fatal("expected a match")
					}
				}
			}
		})

		b.Run("UncachedReadDir(baseline)/n="+strconv.Itoa(n), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, p := range lrcPaths {
					if _, ok := uncachedReadDirCompanion(p); !ok {
						b.Fatal("expected a match")
					}
				}
			}
		})
	}
}
