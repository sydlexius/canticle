// Package sidecar owns the set of lyric sidecar file extensions canticle
// writes beside an audio file, and the Kind each one carries.
//
// It exists because that set was previously encoded as string literals at
// roughly two dozen decision sites across fourteen packages, plus three
// independent helpers (realign.isSidecar, lyrics.oppositeSidecar,
// lyrics.settledSidecar) that each re-derived it and imported none of the
// others. Adding a third extension against that surface is how a site gets
// missed SILENTLY -- a missed literal is not a compile error, and the
// consequence of a missed site is an orphaned or deleted user file.
//
// The shape deliberately mirrors internal/scanner's audio-extension accessors
// (supportedFileTypes / IsAudioFile / SupportedAudioExtensions): one private
// table as the sole data, exported accessors over it, and no way for a consumer
// to hand-copy the list.
//
// It is a LEAF package: it imports nothing else from internal/, so any package
// may depend on it without creating a cycle.
//
// ACTIVE vs DECLARED. A Kind can be declared here before the call sites know
// how to handle it. Extensions()/IsSidecar report only the ACTIVE set; KindOf
// and Ext cover every declared Kind. Every Kind declared today is active:
// KindWordSynced (".elrc", #986) was declared inactive first and switched on
// only once realign, revalidate, purgeprovenance, the scanner and the writer had
// learned to move, pair and settle it. A future Kind should follow the same
// order, because an active Kind the call sites cannot handle orphans files.
//
// CASE (#989). IsSidecar/KindOf have always been case-insensitive (they
// lowercase before comparing), matching realign's historical walk. Until
// #989, the writer's pairing (oppositeSidecar) and companion lookup
// (OwnedCompanionOf) disagreed: they were case-SENSITIVE, so an uppercase
// sidecar realign would collect and re-attach was invisible to the writer --
// never paired, never cleaned up, never suppressed at the watcher. #989
// resolved that by making the writer case-insensitive too, via
// Listing.Variants: a real deployment can see an uppercase extension (macOS or
// an SMB share upstream of a case-sensitive Linux/ext4 library), and losing
// re-attachability for it was judged the worse regression. This DOES widen a
// delete path (the writer now removes files it previously ignored), so
// Variants matches only the EXTENSION's case (ASCII), never the stem's, and
// returns REAL on-disk names -- so it cannot touch another track's file.
package sidecar

import (
	"os"
	"path/filepath"
	"strings"
)

// Kind is the flavor of lyric content a sidecar extension carries.
type Kind int

const (
	// KindUnknown is the zero value: not a lyric sidecar extension.
	KindUnknown Kind = iota
	// KindLineSynced is line-level synced lyrics (".lrc").
	KindLineSynced
	// KindUnsynced is plain, untimed lyric text (".txt").
	KindUnsynced
	// KindWordSynced is word-level synced lyrics (".elrc"): the companion the
	// writer puts beside a .lrc when output.word_sync_mode asks for one (#986).
	// It is a companion, not a sidecar in its own right: it never settles a
	// track, and it travels with (or goes with) the .lrc it describes.
	KindWordSynced
)

// Extension constants. Prefer these over a bare literal at a call site so a
// grep for the extension finds every decision that depends on it.
const (
	ExtLineSynced = ".lrc"
	ExtUnsynced   = ".txt"
	ExtWordSynced = ".elrc"
)

// entry is one declared sidecar extension.
type entry struct {
	kind Kind
	ext  string
	// active reports whether the call sites handle this extension TODAY.
	// See the package doc: declared-but-inactive is a deliberate state.
	active bool
}

// table is the single source of truth. Adding an extension is one line here
// (plus its Kind constant); enabling a declared one is a single false -> true.
var table = []entry{
	{kind: KindLineSynced, ext: ExtLineSynced, active: true},
	{kind: KindUnsynced, ext: ExtUnsynced, active: true},
	{kind: KindWordSynced, ext: ExtWordSynced, active: true},
}

// IsSidecar reports whether name (a file name or path) carries an ACTIVE lyric
// sidecar extension. The comparison is case-insensitive, matching the
// realign walk's historical behavior.
//
// A declared-but-inactive extension reports false.
func IsSidecar(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	for _, e := range table {
		if e.active && e.ext == ext {
			return true
		}
	}
	return false
}

// Extensions returns the ACTIVE sidecar extensions, lowercase and dotted, in
// declaration order. It returns a fresh slice, so a caller cannot mutate the
// package's own table.
func Extensions() []string {
	out := make([]string, 0, len(table))
	for _, e := range table {
		if e.active {
			out = append(out, e.ext)
		}
	}
	return out
}

// KindOf maps a file name or path to its Kind, case insensitively. It covers
// every DECLARED Kind, active or not: a caller asking "what is this?" gets a
// truthful answer even for an extension IsSidecar does not report.
//
// A bare dotted extension (".lrc") resolves, because filepath.Ext reads the
// suffix from the FINAL dot and a leading dot is a final dot. That is how the
// original realign walk behaved and it is preserved deliberately.
func KindOf(name string) Kind {
	ext := strings.ToLower(filepath.Ext(name))
	for _, e := range table {
		if e.ext == ext {
			return e.kind
		}
	}
	return KindUnknown
}

// Ext returns the dotted extension for k, or "" for KindUnknown or any Kind
// not in the table. It covers declared-but-inactive Kinds.
func Ext(k Kind) string {
	for _, e := range table {
		if e.kind == k {
			return e.ext
		}
	}
	return ""
}

// Active reports whether k is handled by the call sites today. A declared Kind
// whose call-site support has not landed yet reports false.
func Active(k Kind) bool {
	for _, e := range table {
		if e.kind == k {
			return e.active
		}
	}
	return false
}

// StemOf returns path with its extension removed, preserving any directory
// part. It is the strings.TrimSuffix(p, filepath.Ext(p)) idiom that is
// open-coded at a dozen sites; a caller wanting only the base name wraps
// filepath.Base itself.
func StemOf(path string) string {
	return strings.TrimSuffix(path, filepath.Ext(path))
}

// Listing is one directory's entry names, read ONCE and reused for every
// candidate a single write asks about (#989): a write resolves its stale
// opposite sidecar, its settled check and its companion from the same
// snapshot, so the name it records with the self-write registry and the name
// it removes can never come from two different reads.
type Listing struct {
	dir     string
	entries []os.DirEntry
}

// List reads dir once. An unreadable directory yields an empty Listing, so
// doubt resolves to "no case variant found" rather than guessing; the exact
// candidate name is still honored by Variants.
func List(dir string) Listing {
	entries, _ := os.ReadDir(dir)
	return Listing{dir: dir, entries: entries}
}

// Variants returns every path on disk that IS candidate's sidecar under a
// different extension case (#989), exact-case name first, the rest in name
// order. The rule is deliberately narrow because callers feed the result to
// os.Remove:
//
//   - The STEM must be byte-identical. Only the extension is compared
//     case-insensitively, so "Intro.lrc" is never a variant of "intro.lrc":
//     on a case-sensitive filesystem those are two different tracks' sidecars.
//   - The extension is folded with ASCII-only folding, never Unicode
//     (strings.EqualFold): sidecar extensions are ASCII, and Unicode simple
//     folding aliases unrelated names (the long s, the micro sign, final sigma).
//   - A case variant is accepted only when it is a REGULAR file: never a
//     directory, symlink, or special file. The exact-case candidate keeps the
//     pre-#989 behavior (it is included when it exists and is not a
//     directory, so a symlink there is unlinked, never followed, exactly as
//     before).
//
// On a case-insensitive filesystem the exact Lstat already matches, and a
// listing entry that is the same file (os.SameFile) is not reported twice.
func (l Listing) Variants(candidate string) []string {
	var out []string
	exact, err := os.Lstat(candidate)
	if err == nil && !exact.IsDir() {
		out = append(out, candidate)
	} else {
		exact = nil
	}
	dir, base := filepath.Dir(candidate), filepath.Base(candidate)
	if filepath.Clean(dir) != filepath.Clean(l.dir) {
		return out
	}
	stem, ext := StemOf(base), filepath.Ext(base)
	for _, e := range l.entries {
		name := e.Name()
		if name == base || StemOf(name) != stem || !asciiEqualFold(filepath.Ext(name), ext) || !e.Type().IsRegular() {
			continue
		}
		p := filepath.Join(dir, name)
		if exact != nil {
			if fi, err := os.Lstat(p); err == nil && os.SameFile(exact, fi) {
				continue
			}
		}
		out = append(out, p)
	}
	return out
}

// asciiEqualFold reports whether a and b are equal under ASCII-only case
// folding; any non-ASCII byte must match exactly.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
