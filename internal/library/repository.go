package library

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/sydlexius/canticle/internal/models"
)

// Repo provides CRUD access to configured library roots.
type Repo struct {
	db *sql.DB
}

// New creates a library repository backed by db.
func New(db *sql.DB) *Repo {
	return &Repo{db: db}
}

// Add creates a new library root. Path must be unique. A nil settings field is
// stored as NULL (inherit the global default).
func (r *Repo) Add(ctx context.Context, path, name string, settings models.LibrarySettings) (models.Library, error) {
	path, name, err := validate(path, name)
	if err != nil {
		return models.Library{}, err
	}
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO libraries (path, name, enrich_recording, detect_instrumental) VALUES (?, ?, ?, ?)`,
		path,
		name,
		boolToNullableInt(settings.EnrichRecording),
		boolToNullableInt(settings.DetectInstrumental),
	)
	if err != nil {
		return models.Library{}, fmt.Errorf("library: add: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return models.Library{}, fmt.Errorf("library: add id: %w", err)
	}
	return r.Get(ctx, id)
}

// List returns all configured library roots in stable ID order.
func (r *Repo) List(ctx context.Context) (libs []models.Library, retErr error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, path, name, created_at, updated_at, enrich_recording, detect_instrumental FROM libraries ORDER BY id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("library: list: %w", err)
	}
	defer func() {
		if err := rows.Close(); err != nil && retErr == nil {
			retErr = fmt.Errorf("library: close list rows: %w", err)
		}
	}()

	for rows.Next() {
		var lib models.Library
		var enrich, detect sql.NullInt64
		if err := rows.Scan(&lib.ID, &lib.Path, &lib.Name, &lib.CreatedAt, &lib.UpdatedAt, &enrich, &detect); err != nil {
			return nil, fmt.Errorf("library: list scan: %w", err)
		}
		lib.EnrichRecording = nullableIntToBool(enrich)
		lib.DetectInstrumental = nullableIntToBool(detect)
		libs = append(libs, lib)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("library: list rows: %w", err)
	}
	return libs, nil
}

// Get returns the library root with id. It returns sql.ErrNoRows when not found.
func (r *Repo) Get(ctx context.Context, id int64) (models.Library, error) {
	var lib models.Library
	var enrich, detect sql.NullInt64
	err := r.db.QueryRowContext(ctx,
		`SELECT id, path, name, created_at, updated_at, enrich_recording, detect_instrumental FROM libraries WHERE id = ?`,
		id,
	).Scan(&lib.ID, &lib.Path, &lib.Name, &lib.CreatedAt, &lib.UpdatedAt, &enrich, &detect)
	if err != nil {
		return models.Library{}, fmt.Errorf("library: get: %w", err)
	}
	lib.EnrichRecording = nullableIntToBool(enrich)
	lib.DetectInstrumental = nullableIntToBool(detect)
	return lib, nil
}

// ErrAmbiguousLibraryName is returned when a name lookup matches more than
// one library row. The schema does not enforce uniqueness on name (only path),
// so callers must disambiguate by ID rather than have the lookup silently pick
// an arbitrary row.
var ErrAmbiguousLibraryName = errors.New("library: ambiguous name (multiple rows match)")

// GetByName returns the library root whose name matches name. It returns
// sql.ErrNoRows when not found and ErrAmbiguousLibraryName when more than one
// row shares the same name (schema only enforces unique path).
func (r *Repo) GetByName(ctx context.Context, name string) (models.Library, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return models.Library{}, fmt.Errorf("library: name must not be empty")
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, path, name, created_at, updated_at, enrich_recording, detect_instrumental FROM libraries WHERE name = ? LIMIT 2`,
		name,
	)
	if err != nil {
		return models.Library{}, fmt.Errorf("library: get by name: %w", err)
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return models.Library{}, fmt.Errorf("library: get by name: %w", err)
		}
		return models.Library{}, fmt.Errorf("library: get by name: %w", sql.ErrNoRows)
	}
	var lib models.Library
	var enrich, detect sql.NullInt64
	if err := rows.Scan(&lib.ID, &lib.Path, &lib.Name, &lib.CreatedAt, &lib.UpdatedAt, &enrich, &detect); err != nil {
		return models.Library{}, fmt.Errorf("library: get by name: %w", err)
	}
	lib.EnrichRecording = nullableIntToBool(enrich)
	lib.DetectInstrumental = nullableIntToBool(detect)
	if rows.Next() {
		return models.Library{}, fmt.Errorf("library: get by name %q: %w", name, ErrAmbiguousLibraryName)
	}
	if err := rows.Err(); err != nil {
		return models.Library{}, fmt.Errorf("library: get by name: %w", err)
	}
	return lib, nil
}

// Update changes the path and name for an existing library root. A nil settings
// field leaves that column unchanged; a non-nil field writes the explicit value
// (COALESCE keeps the existing value when the parameter is NULL).
func (r *Repo) Update(ctx context.Context, id int64, path, name string, settings models.LibrarySettings) (models.Library, error) {
	path, name, err := validate(path, name)
	if err != nil {
		return models.Library{}, err
	}
	res, err := r.db.ExecContext(ctx,
		`UPDATE libraries
		   SET path = ?,
		       name = ?,
		       enrich_recording = COALESCE(?, enrich_recording),
		       detect_instrumental = COALESCE(?, detect_instrumental)
		 WHERE id = ?`,
		path,
		name,
		boolToNullableInt(settings.EnrichRecording),
		boolToNullableInt(settings.DetectInstrumental),
		id,
	)
	if err != nil {
		return models.Library{}, fmt.Errorf("library: update: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return models.Library{}, fmt.Errorf("library: update rows affected: %w", err)
	}
	if affected == 0 {
		return models.Library{}, fmt.Errorf("library: update missing: %w", sql.ErrNoRows)
	}
	return r.Get(ctx, id)
}

// Remove deletes the library root with id.
func (r *Repo) Remove(ctx context.Context, id int64) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM libraries WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("library: remove: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("library: remove rows affected: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("library: remove missing: %w", sql.ErrNoRows)
	}
	return nil
}

// boolToNullableInt maps a tri-state *bool to a nullable SQL INTEGER parameter:
// nil -> NULL, false -> 0, true -> 1.
func boolToNullableInt(v *bool) any {
	if v == nil {
		return nil
	}
	if *v {
		return 1
	}
	return 0
}

// nullableIntToBool maps a scanned nullable INTEGER back to a tri-state *bool:
// NULL -> nil, 0 -> false, non-zero -> true.
func nullableIntToBool(v sql.NullInt64) *bool {
	if !v.Valid {
		return nil
	}
	b := v.Int64 != 0
	return &b
}

func validate(path, name string) (string, string, error) {
	path = strings.TrimSpace(path)
	name = strings.TrimSpace(name)
	if path == "" {
		return "", "", fmt.Errorf("library: path must not be empty")
	}
	if name == "" {
		return "", "", fmt.Errorf("library: name must not be empty")
	}
	return path, name, nil
}
