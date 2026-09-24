package lyrics

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sydlexius/canticle/internal/sidecar"
)

// ErrChangedDuringRewrite is returned by InjectEditorTag when path changed
// (or vanished) between the read this call started from and the rename that
// would apply its result -- a concurrent writer (the worker, a revalidate
// demotion/quarantine) won the race. A caller treats this as "skipped, retry
// next run", never as an error and never as stamped done (#483 finding 3).
var ErrChangedDuringRewrite = errors.New("lyrics: file changed during editor-tag rewrite")

// injectEditorTagPreRenameHook, non-nil only in tests, runs immediately
// before InjectEditorTag's pre-rename re-Lstat, to mutate the target
// deterministically inside the race window instead of racing a goroutine.
var injectEditorTagPreRenameHook func(path string)

// hasBOM reports whether raw begins with a UTF-8 byte-order mark.
func hasBOM(raw []byte) bool {
	return bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
}

// hasMixedLineEndings reports whether raw uses both CRLF and bare LF/CR.
// This package's backup is eol/hasTrailingNL bookkeeping, not the original
// bytes, so a mixed-style file cannot be restored byte-for-byte and is left
// untouched instead of normalized (#483 finding 4; canticle never writes one
// itself). Guarded on a CRLF actually being present first, since ContainsAny
// alone would misreport a pure-LF file (it matches either byte on its own).
func hasMixedLineEndings(raw []byte) bool {
	return bytes.Contains(raw, []byte("\r\n")) &&
		bytes.ContainsAny(bytes.ReplaceAll(raw, []byte("\r\n"), nil), "\r\n")
}

// canticleWrittenTags reports whether tags carry the pair WriteLRC's writeTags
// branch always emits together: [by:canticle] and a [ve:] version tag. That
// joint presence is the cheapest reliable "this tool wrote it" signal -- a
// bare [ve:] could be any tool's, and [source:] is not always present (a
// fetch-mode uncoupled write omits it). hasRe reports whether [re:] already
// exists; veIdx is [ve:]'s index in tags, or -1 if absent.
func canticleWrittenTags(tags []lrcTag) (canticleWritten, hasRe bool, veIdx int) {
	veIdx = -1
	var hasBy bool
	for i, t := range tags {
		switch strings.ToLower(t.key) {
		case "by":
			// v1.7.0 files carry [by:mxlrcgo-svc] (98bac8b renamed it to
			// canticle) and are canticle-written too (#483 finding 5).
			v := strings.TrimSpace(t.value)
			if strings.EqualFold(v, "canticle") || strings.EqualFold(v, "mxlrcgo-svc") {
				hasBy = true
			}
		case "ve":
			veIdx = i
		case "re":
			hasRe = true
		}
	}
	return hasBy && veIdx >= 0, hasRe, veIdx
}

// EditorTagEligible reports whether path is canticle-written (canticleWrittenTags)
// and does not yet carry [re:]. Read-only: usable as a dry-run preview or a
// gate before InjectEditorTag with no risk of mutation.
func EditorTagEligible(path string) (bool, error) {
	if k := sidecar.KindOf(path); k != sidecar.KindLineSynced && k != sidecar.KindWordSynced {
		return false, nil
	}
	fi, err := os.Lstat(path) //nolint:gosec // reason: path is caller-controlled library enumeration
	if err != nil {
		return false, fmt.Errorf("lstat %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // reason: path is caller-controlled library enumeration
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	if hasBOM(raw) || hasMixedLineEndings(raw) {
		return false, nil
	}
	tags, _, err := parseLRCHeader(path)
	if err != nil {
		return false, err
	}
	canticleWritten, hasRe, _ := canticleWrittenTags(tags)
	return canticleWritten && !hasRe, nil
}

// InjectEditorTag adds [re:canticle] to path's header immediately before
// [ve:], matching the adjacency WriteLRC writes on every fresh synced result
// (#483). Only acts when canticleWrittenTags reports both [by:canticle] and
// [ve:] present; idempotent -- an existing [re:] (any value) is left as-is,
// never duplicated or overwritten. Symlinks are Lstat-detected, never
// followed. The rewrite is atomic (temp file, fsync, rename), mirroring
// InjectProvenance in parser.go. injected=false with a nil error means
// "nothing to do".
func InjectEditorTag(path string) (injected bool, err error) {
	if k := sidecar.KindOf(path); k != sidecar.KindLineSynced && k != sidecar.KindWordSynced {
		return false, fmt.Errorf("not an LRC file: %s", path)
	}
	fi, err := os.Lstat(path) //nolint:gosec // reason: path is caller-controlled library enumeration
	if err != nil {
		return false, fmt.Errorf("lstat %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	origMode := fi.Mode().Perm()

	raw, err := os.ReadFile(path) //nolint:gosec // reason: path is caller-controlled
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	if hasBOM(raw) || hasMixedLineEndings(raw) {
		return false, nil
	}
	eol := "\n"
	if bytes.Contains(raw, []byte("\r\n")) {
		eol = "\r\n"
	}
	hasTrailingNL := len(raw) > 0 && raw[len(raw)-1] == '\n'

	tags, lyricsLines, err := parseLRCHeader(path)
	if err != nil {
		return false, err
	}
	canticleWritten, hasRe, veIdx := canticleWrittenTags(tags)
	if !canticleWritten || hasRe {
		return false, nil
	}

	outTags := make([]lrcTag, 0, len(tags)+1)
	outTags = append(outTags, tags[:veIdx]...)
	outTags = append(outTags, lrcTag{key: "re", value: "canticle", raw: "[re:canticle]"})
	outTags = append(outTags, tags[veIdx:]...)

	outLines := make([]string, 0, len(outTags)+len(lyricsLines))
	for _, t := range outTags {
		outLines = append(outLines, t.raw)
	}
	outLines = append(outLines, lyricsLines...)

	// preRename guards the unattended backfill racing the worker's own
	// atomic write or a revalidate demotion/quarantine (#483 finding 3):
	// identity/size/mtime must still match what this call started from, or
	// another writer touched (or removed) the target in the window.
	preRename := func() error {
		if injectEditorTagPreRenameHook != nil {
			injectEditorTagPreRenameHook(path)
		}
		newFi, statErr := os.Lstat(path) //nolint:gosec // reason: path is caller-controlled library enumeration
		if statErr != nil {
			if errors.Is(statErr, fs.ErrNotExist) {
				return ErrChangedDuringRewrite
			}
			return fmt.Errorf("lstat %s before rename: %w", path, statErr)
		}
		if !os.SameFile(fi, newFi) || newFi.Size() != fi.Size() || !newFi.ModTime().Equal(fi.ModTime()) {
			return ErrChangedDuringRewrite
		}
		return nil
	}
	if err := atomicWriteLines(path, outLines, eol, hasTrailingNL, origMode, preRename); err != nil {
		return false, err
	}
	fsyncDir(filepath.Dir(path))
	return true, nil
}

// atomicWriteLines writes lines to path via a same-directory temp file (sync,
// chmod, rename), preserving eol/hasTrailingNL, plus a preRename hook run
// after the chmod and before the rename (InjectEditorTag's concurrency
// guard). #483 finding 7 asked this be shared with InjectProvenance
// (parser.go, no import-cycle risk -- same package): REBUTTED on measured
// size, not merit -- unifying moves ~60 diff lines this slice's hard 600-line
// cap has no room for once finding 3's guard (IMPORTANT) is included. Left as
// its own small, deliberate third copy of the atomic-rewrite shape
// InjectProvenance and lrcbackfill.atomicWrite each already carry.
func atomicWriteLines(path string, lines []string, eol string, hasTrailingNL bool, mode os.FileMode, preRename func() error) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp") //nolint:gosec // reason: path is caller-controlled
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
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
	for i, line := range lines {
		isLast := i == len(lines)-1
		var werr error
		if isLast && !hasTrailingNL {
			_, werr = fmt.Fprint(buf, line)
		} else {
			_, werr = fmt.Fprint(buf, line+eol)
		}
		if werr != nil {
			return fmt.Errorf("write line: %w", werr)
		}
	}
	if err = buf.Flush(); err != nil {
		return fmt.Errorf("flush %s: %w", tmpPath, err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", tmpPath, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpPath, err)
	}
	tmpClosed = true
	if err = os.Chmod(tmpPath, mode); err != nil { //nolint:gosec // reason: mode copied from the original file
		return fmt.Errorf("chmod %s: %w", tmpPath, err)
	}
	if preRename != nil {
		if err = preRename(); err != nil {
			return err
		}
	}
	if err = os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpPath, path, err)
	}
	return nil
}
