package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sydlexius/canticle/internal/instrumentaltitle"
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

// ReleaseEdition is the one edition rule (#1562): a copy whose album-level
// verdict holds (instrumentaltitle.Settles at PositionRelease) is an
// instrumental-edition copy. The queues, the worker's tests and the title
// repair's split share it, so they cannot drift.
func ReleaseEdition(t models.Track) string {
	if v, ok := instrumentaltitle.Settles(t.TrackName, t.AlbumName, t.ArtistName, t.AlbumArtist); ok && v.Position == instrumentaltitle.PositionRelease {
		return EditionInstrumental
	}
	return EditionNone
}

// SetEditionRule configures the rule Enqueue and the adoption inserts use to
// compute a new row's edition, in the style of SetProvidersVersion. nil (the
// default) gives every row EditionNone, which is the identity before #1525.
func (q *DBQueue) SetEditionRule(rule EditionRule) {
	q.editionRule = rule
}

// EditionOf returns the edition Enqueue would store a copy of track at
// sourcePath under, without writing anything: the scan enqueuer's cache door
// asks it so an edition copy never reads the lyric cache (#1562). With no rule
// it is EditionNone and nothing is read.
func (q *DBQueue) EditionOf(ctx context.Context, track models.Track, sourcePath string) (string, error) {
	if q.editionRule == nil {
		return EditionNone, nil
	}
	tx, err := q.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", fmt.Errorf("queue: begin edition read: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	artistKey, titleKey := IdentityKeys(track)
	return q.resolveEditionTx(ctx, tx, artistKey, titleKey, track, sourcePath)
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

// SungRowMissing reports whether track's keys have an instrumental-edition row
// and no row with an empty edition, read-only (#1604). That is the state the
// repair's split leaves when it re-keys a legacy row: its sung twin's words
// are still cached, but no row of the twin's edition exists to settle onto, so
// the scan enqueuer's cache door must send the twin to Enqueue instead of
// marking it done. Without a split or a rule there is never an
// instrumental-edition row, so the answer is false. It is also false for the
// copy at sourcePath when an instrumental-edition row names that file: that
// copy is the release itself (offered with the mode off, after a split), and
// must not take the empty-edition row its sung twin needs.
func (q *DBQueue) SungRowMissing(ctx context.Context, track models.Track, sourcePath string) (bool, error) {
	artistKey, titleKey := IdentityKeys(track)
	var missing bool
	err := q.db.QueryRowContext(ctx, `SELECT
	    EXISTS (SELECT 1 FROM work_queue WHERE artist_key = ? AND title_key = ? AND edition = ?)
	    AND NOT EXISTS (SELECT 1 FROM work_queue WHERE artist_key = ? AND title_key = ? AND edition = ?)
	    AND NOT EXISTS (SELECT 1 FROM work_queue WHERE artist_key = ? AND title_key = ? AND edition = ? AND source_path = ?)`,
		artistKey, titleKey, EditionInstrumental, artistKey, titleKey, EditionNone,
		artistKey, titleKey, EditionInstrumental, sourcePath).Scan(&missing)
	if err != nil {
		return false, fmt.Errorf("queue: probe for a row of the empty edition: %w", err)
	}
	return missing, nil
}
