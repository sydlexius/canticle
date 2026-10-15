package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sydlexius/canticle/internal/models"
)

// Editions a work_queue row can carry (#1525, migration 075). The row identity
// is (artist_key, title_key, edition): an instrumental-release copy and its sung
// twin are separate rows, while two sung copies still collapse into one.
const (
	// EditionNone is every row's edition unless a rule says otherwise, and every
	// row that predates the column.
	EditionNone = ""
	// EditionInstrumental is a copy on an instrumental release.
	EditionInstrumental = "instrumental"
)

// EditionRule returns the edition of an incoming track from its tags. It must
// return EditionNone or EditionInstrumental; anything else fails the enqueue.
type EditionRule func(models.Track) string

// SetEditionRule configures the rule Enqueue and the adoption inserts use to
// compute a new row's edition, in the style of SetProvidersVersion. nil (the
// default) gives every row EditionNone, which is the identity before #1525.
func (q *DBQueue) SetEditionRule(rule EditionRule) {
	q.editionRule = rule
}

// resolveEditionTx returns the edition a row for track at sourcePath is stored
// under. With no rule it is always EditionNone and nothing is read.
//
// With a rule, the rule's answer is the edition, except that a row with these
// keys that already names sourcePath keeps that file: its edition is used, so
// a row made before the column existed (an empty edition) or under a different rule
// answer is found again instead of a second row being made for the same file.
// When rows of both editions name it, the rule's answer wins.
func (q *DBQueue) resolveEditionTx(ctx context.Context, tx *sql.Tx, artistKey, titleKey string, track models.Track, sourcePath string) (string, error) {
	if q.editionRule == nil {
		return EditionNone, nil
	}
	edition := q.editionRule(track)
	if edition != EditionNone && edition != EditionInstrumental {
		return "", fmt.Errorf("queue: edition rule returned unknown edition %q", edition)
	}
	if sourcePath == "" {
		return edition, nil
	}
	var held string
	err := tx.QueryRowContext(ctx,
		`SELECT edition FROM work_queue
         WHERE artist_key = ? AND title_key = ? AND source_path = ?
         ORDER BY edition = ? DESC, id LIMIT 1`,
		artistKey, titleKey, sourcePath, edition).Scan(&held)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return edition, nil
	case err != nil:
		return "", fmt.Errorf("queue: read edition of the row naming the incoming file: %w", err)
	}
	return held, nil
}
