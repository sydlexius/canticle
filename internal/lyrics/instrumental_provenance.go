package lyrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SourceDetector is the [source:] token stamped into an instrumental marker that
// the audio detector wrote. Any other source token (a provider lane name) marks
// a provider-written, editorial-authoritative instrumental. Kept here so the
// writer and the scanner agree on one spelling.
const SourceDetector = "canticle-detector"

// InstrumentalProvenance is the provenance read from an instrumental .txt marker.
// A detector marker carries Source == SourceDetector and a DetectorVersion; a
// provider marker carries the provider lane name and an empty DetectorVersion.
type InstrumentalProvenance struct {
	Source          string
	DetectorVersion string
}

// IsDetector reports whether the marker was written by the audio detector, i.e.
// whether it is provisional (re-checkable) rather than editorially terminal.
func (p InstrumentalProvenance) IsDetector() bool {
	return p.Source == SourceDetector
}

// ReadInstrumentalProvenance reads the file at path and reports whether it is an
// instrumental marker and, if so, its provenance ([source:]/[dv:]). A legacy bare
// marker (no header) returns a zero-value InstrumentalProvenance. A file with no
// marker line returns isMarker=false. Any read error is returned.
func ReadInstrumentalProvenance(path string) (prov InstrumentalProvenance, isMarker bool, err error) {
	tags, lyricLines, err := parseLRCHeader(path)
	if err != nil {
		return InstrumentalProvenance{}, false, err
	}
	for _, t := range tags {
		switch strings.ToLower(t.key) {
		case "source":
			prov.Source = strings.TrimSpace(t.value)
		case "dv":
			prov.DetectorVersion = strings.TrimSpace(t.value)
		}
		if strings.Contains(t.raw, InstrumentalMarker) {
			isMarker = true
		}
	}
	for _, l := range lyricLines {
		if strings.Contains(l, InstrumentalMarker) {
			isMarker = true
			break
		}
	}
	if !isMarker {
		return InstrumentalProvenance{}, false, nil
	}
	return prov, true, nil
}

// WriteMarkerProvenance stamps a provenance header onto a bare instrumental
// marker .txt so a detector-written marker becomes distinguishable on disk (and
// re-checkable by the scanner). It prepends [by:canticle], [source:<prov.Source>],
// and (when set) [dv:<prov.DetectorVersion>] ahead of the existing content,
// preserving the marker line. It is a no-op (changed=false) when the file is not
// a marker, already carries a [source:] header (idempotent), or is a symlink.
// The rewrite is atomic (temp file + rename + parent-dir fsync) and preserves the
// original file mode.
func WriteMarkerProvenance(path string, prov InstrumentalProvenance) (changed bool, err error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return false, fmt.Errorf("lstat %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	existing, isMarker, err := ReadInstrumentalProvenance(path)
	if err != nil {
		return false, fmt.Errorf("read marker provenance %s: %w", path, err)
	}
	if !isMarker || existing.Source != "" {
		return false, nil
	}

	raw, err := os.ReadFile(path) //nolint:gosec // path is a caller-controlled marker file
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}

	var hdr strings.Builder
	hdr.WriteString("[by:canticle]\n")
	fmt.Fprintf(&hdr, "[source:%s]\n", prov.Source)
	if prov.DetectorVersion != "" {
		fmt.Fprintf(&hdr, "[dv:%s]\n", prov.DetectorVersion)
	}
	newContent := append([]byte(hdr.String()), raw...)

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp") //nolint:gosec // dir is the marker's own directory
	if err != nil {
		return false, fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, werr := tmp.Write(newContent); werr != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("write %s: %w", tmpPath, werr)
	}
	if serr := tmp.Sync(); serr != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("sync %s: %w", tmpPath, serr)
	}
	if cerr := tmp.Close(); cerr != nil {
		return false, fmt.Errorf("close %s: %w", tmpPath, cerr)
	}
	if cherr := os.Chmod(tmpPath, fi.Mode().Perm()); cherr != nil { //nolint:gosec // mode copied from the original file
		return false, fmt.Errorf("chmod %s: %w", tmpPath, cherr)
	}
	if rerr := os.Rename(tmpPath, path); rerr != nil {
		return false, fmt.Errorf("rename %s -> %s: %w", tmpPath, path, rerr)
	}
	fsyncDir(dir)
	return true, nil
}
