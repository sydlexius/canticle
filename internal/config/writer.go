package config

import (
	"context"
	"errors"
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
		// A missing config file is not an error on the write path: operators may
		// save settings before any config.toml exists. Treat it as an empty
		// document (create-on-save), mirroring SetValue's create-on-absent-section
		// behavior and WriteAtomic's ErrNotExist tolerance for the .bak step.
		if errors.Is(err, os.ErrNotExist) {
			return &tomledit.Document{}, nil
		}
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
// replaced in place; if it is absent it is inserted into its table, and if that
// [section] table is itself absent (operators hand-write minimal configs with
// only the sections they set) the table is created. Comments, blank lines, and
// ordering elsewhere are untouched.
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
	kv := &parser.KeyValue{Name: parser.Key{name}, Value: val}
	if tab := transform.FindTable(doc, table...); tab != nil && tab.Section != nil {
		transform.InsertMapping(tab.Section, kv, true)
		return nil
	}
	// The [section] table is absent. A registry field always lives under a
	// section, so create that table (appending it after the existing sections)
	// rather than failing the save. A truly global key would have no table to
	// create, which is not a real registry shape -- guard it explicitly.
	if len(table) == 0 {
		return fmt.Errorf("config: set %s: no table to insert into", path)
	}
	doc.Sections = append(doc.Sections, &tomledit.Section{
		Heading: &parser.Heading{Name: parser.Key(table)},
		Items:   []parser.Item{kv},
	})
	return nil
}

// RemoveKeyIfPresent removes a dotted key from the config file if it is present,
// preserving all other content (comments, ordering, other keys). It is a no-op
// (no write, no .bak churn) when the key is absent. The settings token save uses
// it to strip a cleartext api.token from the TOML after routing the secret to
// the encrypted store, so the store -- not a stale higher-precedence file value
// -- is authoritative on the next load (#288).
func RemoveKeyIfPresent(configPath, path string) error {
	doc, err := LoadDocument(configPath)
	if err != nil {
		return err
	}
	keys := strings.Split(path, ".")
	if doc.First(keys...) == nil {
		return nil // absent: nothing to remove
	}
	if err := transform.Remove(parser.Key(keys)).Apply(context.Background(), doc); err != nil {
		return fmt.Errorf("config: remove %s: %w", path, err)
	}
	return WriteAtomic(configPath, doc)
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
	// the rename, leaving the live file untouched. The .bak is a transient
	// crash-safety copy for the write window only: it is purged once the new
	// config is durably in place (see below), so no lingering plaintext copy of
	// a file-resident secret (server.webhook_api_keys) survives the write (#290).
	bakPath := path + ".bak"
	wroteBak := false
	mode := os.FileMode(0o600)
	orig, err := os.ReadFile(path) //nolint:gosec // G304: operator's own config file
	switch {
	case err == nil:
		if fi, statErr := os.Stat(path); statErr == nil {
			mode = fi.Mode()
		}
		if err := os.WriteFile(bakPath, orig, mode); err != nil { //nolint:gosec // G304: .bak sits beside the operator's own config file; path is app-resolved, not attacker-controlled
			return fmt.Errorf("config: write backup: %w", err)
		}
		wroteBak = true
	case errors.Is(err, os.ErrNotExist):
		// First write: there is nothing to back up.
	default:
		// A non-ENOENT read failure (permissions, I/O error) means we cannot
		// guarantee a backup; abort rather than overwrite an unrecoverable file.
		return fmt.Errorf("config: read existing config for backup: %w", err)
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
	// The new config is durably in place; the .bak has served its crash-safety
	// purpose for this write window. Purge it so no lingering plaintext copy of
	// a file-resident secret remains on disk. Best-effort: a removal failure does
	// not corrupt the (already-renamed) config, so it must not fail the write.
	if wroteBak {
		_ = os.Remove(bakPath)
	}
	return nil
}
