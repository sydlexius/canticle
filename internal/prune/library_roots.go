package prune

import (
	"context"
	"database/sql"
	"fmt"
)

// LibraryRoots lists the configured library roots. It reads the libraries table
// only, never the disk; the worker caches the list for a TTL so a drain does not
// re-read the table per row (#1430).
func (p *Pruner) LibraryRoots(ctx context.Context) ([]string, error) {
	var roots []string
	if err := queryRows(ctx, p.db, `SELECT path FROM libraries WHERE path != ''`, nil, func(rows *sql.Rows) error {
		var path string
		if err := rows.Scan(&path); err != nil {
			return err
		}
		roots = append(roots, path)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("prune: load library roots: %w", err)
	}
	return roots, nil
}

// RootOnline reports whether one library root is present and populated, the
// availableRoots test for a single root: the worker asks about the root a row
// lives in, never every root, so a spun-down array is not woken for the rest.
func (p *Pruner) RootOnline(root string) bool { return dirPopulated(root) }
