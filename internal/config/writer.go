package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/creachadair/tomledit"
	"github.com/creachadair/tomledit/parser"
	"github.com/creachadair/tomledit/transform"
)

// LoadDocument parses a TOML file into an editable tomledit document, preserving
// comments, blank lines, and key ordering. This is the write-path loader;
// BurntSushi/toml (via LoadWithSources) remains the decode/read path. The two
// coexist: tomledit is write-only.
func LoadDocument(path string) (*tomledit.Document, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is the operator's own config file, resolved by the app, not attacker-controlled
	if err != nil {
		return nil, fmt.Errorf("config: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	doc, err := tomledit.Parse(f)
	if err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return doc, nil
}

// SetValue sets the value of a known dotted key in the document, preserving all
// other content. The field's type drives serialization (strings are quoted,
// slices become inline arrays, etc.). If the key already exists its value is
// replaced in place; if it is absent it is inserted into its (existing)
// section. Comments, blank lines, and ordering elsewhere are untouched.
func SetValue(doc *tomledit.Document, path string, ftype FieldType, value string) error {
	keys := strings.Split(path, ".")
	val, err := tomlValue(ftype, value)
	if err != nil {
		return fmt.Errorf("config: set %s: %w", path, err)
	}
	if entry := doc.First(keys...); entry != nil && entry.KeyValue != nil {
		entry.Value = val
		return nil
	}
	// Key absent: insert into its table (the last element is the leaf key).
	name := keys[len(keys)-1]
	table := keys[:len(keys)-1]
	tab := transform.FindTable(doc, table...)
	if tab == nil || tab.Section == nil {
		return fmt.Errorf("config: set %s: section %q is not present in the file", path, strings.Join(table, "."))
	}
	kv := &parser.KeyValue{Name: parser.Key{name}, Value: val}
	transform.InsertMapping(tab.Section, kv, true)
	return nil
}

// tomlValue converts a string value of the given field type into a parsed TOML
// value, rejecting input that does not match the type.
func tomlValue(ftype FieldType, value string) (parser.Value, error) {
	var lit string
	switch ftype {
	case TypeString:
		lit = strconv.Quote(value)
	case TypeInt:
		if _, err := strconv.Atoi(value); err != nil {
			return parser.Value{}, fmt.Errorf("invalid integer %q", value)
		}
		lit = value
	case TypeBool:
		b, err := strconv.ParseBool(value)
		if err != nil {
			return parser.Value{}, fmt.Errorf("invalid boolean %q", value)
		}
		lit = strconv.FormatBool(b)
	case TypeFloat64:
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			return parser.Value{}, fmt.Errorf("invalid number %q", value)
		}
		lit = value
	case TypeStringSlice:
		parts := splitCSV(value)
		quoted := make([]string, len(parts))
		for i, p := range parts {
			quoted[i] = strconv.Quote(p)
		}
		lit = "[" + strings.Join(quoted, ", ") + "]"
	default:
		return parser.Value{}, fmt.Errorf("unknown field type %d", ftype)
	}
	return parser.ParseValue(lit)
}

// WriteAtomic renders the document to a temp file in the same directory, fsyncs
// it, backs up the existing file to path+".bak", and renames the temp file over
// the original. A failure leaves the original byte-identical.
func WriteAtomic(path string, doc *tomledit.Document) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".mxlrc-config-*.toml.tmp")
	if err != nil {
		return fmt.Errorf("config: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed away

	var fmtr tomledit.Formatter
	if err := fmtr.Format(tmp, doc); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: format document: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: fsync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: close temp file: %w", err)
	}

	// Mirror the original file's mode and keep a single .bak of the prior
	// content. Reading the original up front means a failure here aborts before
	// the rename, leaving the live file untouched.
	mode := os.FileMode(0o600)
	if orig, err := os.ReadFile(path); err == nil { //nolint:gosec // G304: operator's own config file
		if fi, statErr := os.Stat(path); statErr == nil {
			mode = fi.Mode()
		}
		if err := os.WriteFile(path+".bak", orig, mode); err != nil { //nolint:gosec // G304: .bak sits beside the operator's own config file; path is app-resolved, not attacker-controlled
			return fmt.Errorf("config: write backup: %w", err)
		}
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("config: chmod temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("config: rename temp file over config: %w", err)
	}
	// Fsync the containing directory so the rename itself is persisted, not just
	// the file contents -- guards against losing the rename on a power failure
	// right after WriteAtomic returns. Best-effort: some filesystems do not
	// support directory fsync, and the data is already durably on disk.
	if d, err := os.Open(dir); err == nil { //nolint:gosec // G304: dir is filepath.Dir of the operator's own config path
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
