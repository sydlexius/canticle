// Package titlerepair finds the queue rows that settled with lyrics before the
// instrumental title rule existed and that the rule would now settle as
// instrumental (#1470). This file is the read-only half: select, classify and
// count. A title, artist, album and path are private: nothing here logs or
// returns them, and a row is named by its work item id only.
package titlerepair

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sydlexius/canticle/internal/instrumentaltitle"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
)

// pageSize bounds one keyset page of the selector.
const pageSize = 500

// Dispositions of a Strong row. The first skip that matches wins, in this
// order; the rest are candidates. Unreadable is first because the later ones
// presume a verdict, and a failed read of the file's album leaves it unknown.
const (
	DispositionUnreadable     = "unreadable"
	DispositionInFlight       = "in-flight"
	DispositionHandMarked     = "hand-marked"
	DispositionArtistPrefixed = "artist-prefixed"
	DispositionCandidate      = "candidate"
)

// Options scopes one Run.
type Options struct {
	// LibraryID limits the selection to rows linked to that library. Nil means
	// every library.
	LibraryID *int64
	// FileAlbum returns the audio file's album tag, which the worker classifies
	// with where it re-reads it. ("", nil) is a file with no album tag: the row's
	// album stands. An error counts the row Unreadable. Nil means no re-read.
	FileAlbum func(sourcePath string) (album string, err error)
	// OnRow, when set, is called once per Strong row.
	OnRow func(Row)
}

// Row is one Strong row and what the repair makes of it. It carries no part of
// the title: a Verdict is closed constants.
type Row struct {
	ID          int64
	Verdict     instrumentaltitle.Verdict
	Disposition string
}

// StrongKey is one cell of the Strong breakdown.
type StrongKey struct {
	Family   instrumentaltitle.Family
	Position instrumentaltitle.Position
}

// Result is the count of one Run. Examined = NoSignal + Weak + Strong, and
// Strong = Unreadable + InFlight + HandMarked + ArtistPrefixed + Candidates.
type Result struct {
	Examined int
	// NoSignal includes the excluded a cappella and karaoke titles.
	NoSignal int
	Weak     int
	Strong   int
	StrongBy map[StrongKey]int
	// The detector's verdict on the Strong rows, from instrumental_result.
	DetectorInstrumental, DetectorNotInstrumental, DetectorUnscored int
	// Unreadable: the file's album could not be read. InFlight: held by a worker.
	// HandMarked: a hand edit or mark. ArtistPrefixed: the rule declines the title.
	Unreadable, InFlight, HandMarked, ArtistPrefixed int
	// Candidates are the rows the repair would act on.
	Candidates int
}

// Repairer runs the repair over one database.
type Repairer struct {
	db  *sql.DB
	log *slog.Logger
}

// New returns a Repairer over db. A nil log is slog.Default().
func New(db *sql.DB, log *slog.Logger) *Repairer {
	if log == nil {
		log = slog.Default()
	}
	return &Repairer{db: db, log: log}
}

// candidate is one selected row. The text fields never leave this package.
type candidate struct {
	id                                                int64
	artist, title, album, albumArtist, source, status string
	hand                                              bool
	detector                                          sql.NullInt64
}

// Run selects every row a provider lane settled with lyrics, classifies its
// title as the worker's title rule would, and counts the outcome. Whether the
// rule is switched on is the caller's check.
func (r *Repairer) Run(ctx context.Context, o Options) (Result, error) {
	res := Result{StrongBy: map[StrongKey]int{}}
	for after := int64(0); ; {
		page, err := r.page(ctx, o.LibraryID, after)
		if err != nil {
			return res, err
		}
		for _, c := range page {
			r.classify(c, o, &res)
		}
		if len(page) < pageSize {
			break
		}
		after = page[len(page)-1].id
	}
	r.log.Debug("titlerepair: classified", "examined", res.Examined, "strong", res.Strong, "candidates", res.Candidates)
	return res, nil
}

// selector builds the query of one keyset page after id. Processing and
// hand-protected rows are selected on purpose, so Run can count them as
// skipped. The lanes are the provider lanes: a detector, hand or title-rule
// settle is not a fetched lyric. The unary plus on status is load-bearing:
// without it SQLite sorts every page in a temp b-tree (TestSelectorWalksRowid).
func selector(libraryID *int64, after int64) (string, []any) {
	lanes := reports.Lanes()
	args := make([]any, 0, len(lanes)+3)
	args = append(args, after)
	for _, l := range lanes {
		args = append(args, l)
	}
	query := `SELECT id, artist, title, album, album_artist, source_path, status, ` +
		queue.HandProtectedPredicate + `, instrumental_result
          FROM work_queue
         WHERE id > ? AND +status IN ('done', 'processing') AND outcome_type IN ('synced', 'unsynced')
           AND provider_lane IN (` + strings.TrimSuffix(strings.Repeat("?,", len(lanes)), ",") + `)`
	if libraryID != nil {
		query += ` AND ` + reports.LibraryPredicate
		args = append(args, *libraryID)
	}
	return query + ` ORDER BY id LIMIT ?`, append(args, pageSize)
}

// page reads the next keyset page after id and closes it before returning, so
// no statement is open while the caller works on the rows.
func (r *Repairer) page(ctx context.Context, libraryID *int64, after int64) ([]candidate, error) {
	query, args := selector(libraryID, after)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("titlerepair: select rows after id %d: %w", after, err)
	}
	defer func() { _ = rows.Close() }()
	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.artist, &c.title, &c.album, &c.albumArtist, &c.source, &c.status, &c.hand, &c.detector); err != nil {
			return nil, fmt.Errorf("titlerepair: scan row after id %d: %w", after, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("titlerepair: read rows after id %d: %w", after, err)
	}
	return out, nil
}

// classify counts one row. It decides on Actionable's ok alone; the verdict is
// for the breakdown.
func (r *Repairer) classify(c candidate, o Options, res *Result) {
	res.Examined++
	v, ok := instrumentaltitle.Actionable(c.title, c.album, c.artist, c.albumArtist)
	// The worker replaces the row's album with the audio file's where the file
	// has one. An album only withdraws a Strong verdict (a karaoke album), so
	// only a title that is Strong alone costs a read. The path test is the worker's.
	unreadable := false
	if alone := instrumentaltitle.Classify(c.title, ""); alone.Tier == instrumentaltitle.Strong &&
		o.FileAlbum != nil && strings.TrimSpace(c.source) != "" {
		switch album, err := o.FileAlbum(c.source); {
		case err != nil: // the error may name the path, so it is not logged
			r.log.Warn("titlerepair: audio file album unreadable, row skipped", "id", c.id)
			v, ok, unreadable = alone, false, true
		case album != "":
			v, ok = instrumentaltitle.Actionable(c.title, album, c.artist, c.albumArtist)
		}
	}
	switch v.Tier {
	case instrumentaltitle.None:
		res.NoSignal++
		return
	case instrumentaltitle.Weak:
		res.Weak++
		return
	case instrumentaltitle.Strong:
	}
	res.Strong++
	res.StrongBy[StrongKey{v.Family, v.Position}]++
	switch {
	case !c.detector.Valid:
		res.DetectorUnscored++
	case c.detector.Int64 == 1:
		res.DetectorInstrumental++
	default:
		res.DetectorNotInstrumental++
	}
	row := Row{ID: c.id, Verdict: v}
	switch {
	case unreadable:
		res.Unreadable++
		row.Disposition = DispositionUnreadable
	case c.status == "processing":
		res.InFlight++
		row.Disposition = DispositionInFlight
	case c.hand:
		res.HandMarked++
		row.Disposition = DispositionHandMarked
	case !ok:
		res.ArtistPrefixed++
		row.Disposition = DispositionArtistPrefixed
	default:
		res.Candidates++
		row.Disposition = DispositionCandidate
	}
	if o.OnRow != nil {
		o.OnRow(row)
	}
}
