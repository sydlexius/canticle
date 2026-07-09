package lyrics

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// ProvenanceTags holds the tag key-value pairs to inject into an LRC header.
// A zero-value field means "skip this tag". Existing tags with the same key
// are never overwritten (S4 idempotency rule).
type ProvenanceTags struct {
	Source  string // [source:] tag
	Fetched string // [fetched:] tag (RFC3339)
	ISRC    string // [isrc:] tag
	MBID    string // [mbid:] tag
	// Artist and Title mirror the standard [ar:]/[ti:] LRC header tags. They are
	// read-only identity signals (populated by ReadProvenanceTags for the realign
	// name guard); InjectProvenance never writes them.
	Artist string // [ar:] tag
	Title  string // [ti:] tag
	// Ve is intentionally absent: [ve:] is skipped on backfilled files (DC4).
}

// ReadProvenanceTags reads the LRC header of the file at path and returns the
// identity-bearing tag values (ISRC, MBID) plus the standard [ar:]/[ti:] artist
// and title tags, using the same lowercase key convention as InjectProvenance.
// It works on any sidecar (.lrc or .txt); a file with no recognizable header
// yields a zero-value ProvenanceTags and a nil error. Later occurrences of a key
// win (matching how a rewrite would leave the final value in place).
func ReadProvenanceTags(path string) (ProvenanceTags, error) {
	tags, _, err := parseLRCHeader(path)
	if err != nil {
		return ProvenanceTags{}, err
	}
	var pt ProvenanceTags
	for _, t := range tags {
		if t.key == "" {
			continue
		}
		switch strings.ToLower(t.key) {
		case "isrc":
			pt.ISRC = strings.TrimSpace(t.value)
		case "mbid":
			pt.MBID = strings.TrimSpace(t.value)
		case "source":
			pt.Source = strings.TrimSpace(t.value)
		case "fetched":
			pt.Fetched = strings.TrimSpace(t.value)
		case "ar":
			pt.Artist = strings.TrimSpace(t.value)
		case "ti":
			pt.Title = strings.TrimSpace(t.value)
		}
	}
	return pt, nil
}

// lrcTag represents a parsed LRC header tag.
type lrcTag struct {
	key   string
	value string
	raw   string // original line as-is
}

// parseLRCHeader reads a file and returns (headerTags, lyricLines, error).
// headerTags holds parsed tag lines from the leading block; lyricLines holds
// the rest of the file verbatim. The split point is the first line that is NOT
// an LRC tag (i.e. does not match [key:value]). Blank lines before the first
// non-tag line are considered part of the header.
func parseLRCHeader(path string) ([]lrcTag, []string, error) {
	f, err := os.Open(path) //nolint:gosec // path comes from caller-controlled file enumeration
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var tags []lrcTag
	var lyrics []string
	inHeader := true

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if inHeader {
			if tag, ok := parseTagLine(line); ok {
				tags = append(tags, tag)
				continue
			}
			if strings.TrimSpace(line) == "" && len(lyrics) == 0 {
				// blank line in header block - treat as header
				tags = append(tags, lrcTag{raw: line})
				continue
			}
			inHeader = false
		}
		lyrics = append(lyrics, line)
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("scan %s: %w", path, err)
	}
	return tags, lyrics, nil
}

// parseTagLine parses a single LRC tag line of the form [key:value].
// Returns (tag, true) on success or (zero, false) if the line is not a tag.
func parseTagLine(line string) (lrcTag, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return lrcTag{}, false
	}
	inner := s[1 : len(s)-1]
	// Lyric lines like [01:23.45]text are NOT header tags. A header tag has
	// a non-numeric first character in the key portion.
	idx := strings.IndexByte(inner, ':')
	if idx <= 0 {
		return lrcTag{}, false
	}
	key := inner[:idx]
	// If key starts with a digit it is a timestamp line, not a tag.
	if key[0] >= '0' && key[0] <= '9' {
		return lrcTag{}, false
	}
	value := inner[idx+1:]
	return lrcTag{key: key, value: value, raw: line}, true
}

// sanitizeTagValue strips ']' and newline characters from a provenance tag
// value so a crafted value cannot break out of the [key:value] format.
func sanitizeTagValue(v string) string {
	v = strings.ReplaceAll(v, "]", "")
	v = strings.ReplaceAll(v, "\r", "")
	v = strings.ReplaceAll(v, "\n", "")
	return v
}

// InjectProvenance injects the given provenance tags into the LRC file at path,
// skipping any tag whose key already exists in the file (idempotent per S4).
// The rewrite is atomic: a temp file in the same directory is written and
// renamed over path only on complete success. Only the header block is modified;
// lyric lines are preserved verbatim. The file must be a .lrc file.
//
// Returns (injected, skipped, error) where injected counts tags added and
// skipped counts tags that already existed and were left untouched.
func InjectProvenance(path string, pt ProvenanceTags) (injected, skipped int, err error) {
	if strings.ToLower(filepath.Ext(path)) != ".lrc" {
		return 0, 0, fmt.Errorf("not an LRC file: %s", path)
	}

	// Lstat to detect symlinks (MINOR-5) and capture original permissions (MAJOR-1).
	fi, err := os.Lstat(path) //nolint:gosec // path is caller-controlled
	if err != nil {
		return 0, 0, fmt.Errorf("lstat %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		slog.Warn("backfill: skipping symlink .lrc", "path", path)
		return 0, 0, nil
	}
	origMode := fi.Mode().Perm()

	// Detect original line endings and trailing-newline state (MINOR-6).
	rawContent, err := os.ReadFile(path) //nolint:gosec // path is caller-controlled
	if err != nil {
		return 0, 0, fmt.Errorf("read %s: %w", path, err)
	}
	eol := "\n"
	if bytes.Contains(rawContent, []byte("\r\n")) {
		eol = "\r\n"
	}
	hasTrailingNL := len(rawContent) > 0 && rawContent[len(rawContent)-1] == '\n'
	existingTags, lyricsLines, err := parseLRCHeader(path)
	if err != nil {
		return 0, 0, err
	}

	// Build a set of keys already present in the header (NIT-13: case-insensitive).
	existing := make(map[string]bool, len(existingTags))
	for _, t := range existingTags {
		if t.key != "" {
			existing[strings.ToLower(t.key)] = true
		}
	}

	// Build the list of candidate tags to inject (MINOR-10: sanitize values).
	type candidate struct{ key, value string }
	candidates := []candidate{
		{"source", sanitizeTagValue(pt.Source)},
		{"fetched", sanitizeTagValue(pt.Fetched)},
		{"isrc", sanitizeTagValue(pt.ISRC)},
		{"mbid", sanitizeTagValue(pt.MBID)},
	}

	var toAdd []lrcTag
	for _, c := range candidates {
		if c.value == "" {
			continue
		}
		if existing[c.key] {
			skipped++
			continue
		}
		toAdd = append(toAdd, lrcTag{
			key:   c.key,
			value: c.value,
			raw:   fmt.Sprintf("[%s:%s]", c.key, c.value),
		})
		injected++
	}

	if injected == 0 {
		return 0, skipped, nil
	}

	// MINOR-7: insert new tags before the first blank header entry so they land
	// within the tag block, not after the blank line that precedes lyric lines.
	insertIdx := len(existingTags)
	for i, t := range existingTags {
		if t.key == "" {
			insertIdx = i
			break
		}
	}

	// Build the ordered output line list.
	var outLines []string
	for _, t := range existingTags[:insertIdx] {
		outLines = append(outLines, t.raw)
	}
	for _, t := range toAdd {
		outLines = append(outLines, t.raw)
	}
	for _, t := range existingTags[insertIdx:] {
		outLines = append(outLines, t.raw)
	}
	outLines = append(outLines, lyricsLines...)

	// Atomic rewrite: write to a temp file, then rename.
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, base+".*.tmp") //nolint:gosec // path is caller-controlled
	if err != nil {
		return 0, 0, fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	tmpClosed := false
	defer func() {
		if !tmpClosed {
			_ = tmp.Close()
		}
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	buf := bufio.NewWriter(tmp)

	// Write output lines preserving original line endings (MINOR-6).
	for i, line := range outLines {
		isLast := i == len(outLines)-1
		var werr error
		if isLast && !hasTrailingNL {
			_, werr = fmt.Fprint(buf, line)
		} else {
			_, werr = fmt.Fprint(buf, line+eol)
		}
		if werr != nil {
			return 0, 0, fmt.Errorf("write line: %w", werr)
		}
	}

	if err = buf.Flush(); err != nil {
		return 0, 0, fmt.Errorf("flush %s: %w", tmpPath, err)
	}
	// MINOR-4: sync data to disk before rename for durability.
	if err = tmp.Sync(); err != nil {
		return 0, 0, fmt.Errorf("sync %s: %w", tmpPath, err)
	}
	if err = tmp.Close(); err != nil {
		return 0, 0, fmt.Errorf("close %s: %w", tmpPath, err)
	}
	tmpClosed = true

	// MAJOR-1: restore original file permissions instead of hardcoding 0666.
	if err = os.Chmod(tmpPath, origMode); err != nil { //nolint:gosec // mode copied from original
		return 0, 0, fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return 0, 0, fmt.Errorf("rename %s -> %s: %w", tmpPath, path, err)
	}
	// NEW-3: fsync the parent dir so the rename is durable across a hard crash.
	fsyncDir(dir)
	return injected, skipped, nil
}
