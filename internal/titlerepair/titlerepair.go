// Package titlerepair finds the queue rows that settled with lyrics before the
// instrumental title rule existed and that the rule would now settle as
// instrumental (#1470). This file selects, classifies and counts; evidence.go
// vets and culls the candidates. A title, artist, album and path are private:
// nothing here logs them, a row is named by its work item id only, and
// Row.Files is the one place a path leaves the package.
package titlerepair

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/sydlexius/canticle/internal/instrumentaltitle"
	"github.com/sydlexius/canticle/internal/lyricblock"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
)

// pageSize bounds one keyset page of the selector.
const pageSize = 500

// Dispositions of a Strong row. The first skip that matches wins, in this
// order; the rest are candidates, and the cull names what became of each.
// Unreadable is first because the later ones presume a verdict, and a failed
// read of the file's album leaves it unknown.
const (
	DispositionUnreadable     = "unreadable"
	DispositionInFlight       = "in-flight"
	DispositionHandMarked     = "hand-marked"
	DispositionArtistPrefixed = "artist-prefixed"
	// ByAlbum: Strong by the album alone (PositionRelease) on a row that is not
	// provably the release's own: its edition is empty (the split left it shared
	// with a sung twin), or the audio file's album no longer names the release.
	DispositionByAlbum = "by-album"
	// A candidate's outcome. WouldRepair is the dry run's Repaired.
	DispositionSymlink         = "symlink"
	DispositionOutsideRoots    = "outside-roots"
	DispositionStemOccupied    = "stem-occupied"
	DispositionNoFile          = "no-file"
	DispositionSourceDisagrees = "source-disagrees"
	DispositionNotProvable     = "not-provable"
	DispositionNeedsManualMark = "needs-manual-mark"
	DispositionError           = "error"
	DispositionWouldRepair     = "would-repair"
	DispositionRepaired        = "repaired"
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
	// OnRow, when set, is called once per Strong row, after the row was acted on.
	OnRow func(Row)
	// Roots are the library roots a candidate's output directories must sit in.
	Roots []string
	// Apply repairs the candidates. Without it Run reads their files and vets
	// them and changes nothing.
	Apply bool
	// Report makes one backup record durable before its file is removed (see
	// lyricblock.MarkRequest.Report). Required with Apply, never called without.
	Report func(lyricblock.Backup) error
}

// Row is one Strong row and what the repair makes of it. It carries no part of
// the title: a Verdict is closed constants.
type Row struct {
	ID          int64
	Verdict     instrumentaltitle.Verdict
	Disposition string
	// Files are the lyric files the repair read for a candidate. They are private
	// paths, for the caller's --tail file only: never stdout, never a log.
	Files []string
}

// StrongKey is one cell of the Strong breakdown.
type StrongKey struct {
	Family   instrumentaltitle.Family
	Position instrumentaltitle.Position
}

// Result is the count of one Run. Examined = NoSignal + Weak + Strong,
// Strong = StrongByTitle + StrongByAlbum = Unreadable + InFlight + HandMarked +
// ArtistPrefixed + ByAlbum + Candidates, and Candidates = Symlink +
// OutsideRoots + StemOccupied + NoFile + SourceDisagrees + NotProvable +
// NeedsManualMark + Errors + WouldRepair + Repaired.
type Result struct {
	Examined int
	// NoSignal includes the excluded a cappella, karaoke and sing-along titles.
	NoSignal int
	Weak     int
	Strong   int
	StrongBy map[StrongKey]int
	// StrongByAlbum: the Strong rows the album alone decides, a plain title on an
	// instrumental release (instrumentaltitle.Settles, PositionRelease), from the
	// stored album and artists. StrongByTitle: the rest.
	StrongByTitle, StrongByAlbum int
	// The detector's verdict on the Strong rows, from instrumental_result.
	DetectorInstrumental, DetectorNotInstrumental, DetectorUnscored int
	// The same split for the Weak rows, read from instrumental_result and never
	// scored again. They sum to Weak, and inform whether a weak tier is worth building.
	WeakDetectorInstrumental, WeakDetectorNotInstrumental, WeakDetectorUnscored int
	// WeakAlbumInstrumental: the Weak rows whose verdict is the instrumental
	// family in the album position: an album naming the word that the release
	// rule declined. Counts only.
	WeakAlbumInstrumental int
	// Unreadable: the file's album could not be read. InFlight: held by a worker.
	// HandMarked: a hand edit or mark. ArtistPrefixed: the rule declines the title.
	// InFlight and HandMarked include a row found so only when the cull reached it.
	// ByAlbum: Strong by album and none of those, on a row not the release's own
	// (see DispositionByAlbum). The rest Strong by album are candidates, culled
	// with no block (#1564): a block would refuse the sung twin's words too.
	Unreadable, InFlight, HandMarked, ArtistPrefixed, ByAlbum int
	// Candidates are the rows handed to the cull that it did not find in flight or
	// hand-marked. Each lands in exactly one of the counters below.
	Candidates int
	// Skips that change nothing. Symlink: a lyric file is a symlink. OutsideRoots:
	// an output directory is in no root, or is gone. StemOccupied: the cull would
	// leave a file on the lyric stem, so no marker could land. NoFile: no lyric
	// file, or none with words. SourceDisagrees: a .lrc names another lane in
	// [source:]. NotProvable: a file not dated within two minutes of the row's settle.
	Symlink, OutsideRoots, StemOccupied, NoFile, SourceDisagrees, NotProvable int
	// NeedsManualMark: a file appeared or changed mid-repair and failed the vet.
	// The row stays done with its words blocked and its cache entry gone, nothing
	// was removed, and a rerun refuses it again: it needs a mark-wrong by hand.
	NeedsManualMark int
	// Errors: any other failure, logged with the work item id. A rerun retries it,
	// unless the row changed under the repair: then the rerun counts it as it is now.
	Errors int
	// WouldRepair (dry run) or Repaired (Apply): the rows that pass every check.
	WouldRepair, Repaired int
	// BackedUp is the lyric files of those rows, one that appeared mid-repair too:
	// backed up under Apply, what a dry run would back up. The rest are Apply only.
	BackedUp, Removed, NewBlocks, CacheInvalidated, Reopened int
}

// Repairer runs the repair over one database.
type Repairer struct {
	db  *sql.DB
	svc *lyricblock.Service
	log *slog.Logger
}

// New returns a Repairer over db. A nil log is slog.Default().
func New(db *sql.DB, log *slog.Logger) *Repairer {
	if log == nil {
		log = slog.Default()
	}
	// No self-write registry: this runs in a CLI process (see lyricblock.New).
	return &Repairer{db: db, svc: lyricblock.New(db, log, nil), log: log}
}

// candidate is one selected row. The text fields never leave this package.
type candidate struct {
	id                                                         int64
	artist, title, album, albumArtist, source, status, edition string
	// What the vet reads: the lane that settled the row and when.
	lane, completedAt string
	hand              bool
	detector          sql.NullInt64
}

// Run selects every row a provider lane settled with lyrics, classifies its
// title as the worker's title rule would, and hands each candidate to the cull
// (evidence.go), which changes nothing unless o.Apply. A row that fails is
// counted and the run goes on. Whether the rule is switched on is the caller's
// check.
func (r *Repairer) Run(ctx context.Context, o Options) (Result, error) {
	res := Result{StrongBy: map[StrongKey]int{}}
	if o.Apply && o.Report == nil {
		return res, errors.New("titlerepair: Apply needs a Report to back the lyric files up")
	}
	for after := int64(0); ; {
		page, err := r.page(ctx, o.LibraryID, after)
		if err != nil {
			return res, err
		}
		for _, c := range page {
			r.classify(ctx, c, o, &res)
			after = c.id
			// After the row, so a cancel during the last row is not a clean finish.
			if err := ctx.Err(); err != nil {
				return res, fmt.Errorf("titlerepair: stopped after id %d: %w", after, err)
			}
		}
		if len(page) < pageSize {
			break
		}
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
	query := `SELECT id, artist, title, album, album_artist, source_path, status,
                 provider_lane, COALESCE(completed_at, ''), edition, ` +
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
		if err := rows.Scan(&c.id, &c.artist, &c.title, &c.album, &c.albumArtist, &c.source, &c.status,
			&c.lane, &c.completedAt, &c.edition, &c.hand, &c.detector); err != nil {
			return nil, fmt.Errorf("titlerepair: scan row after id %d: %w", after, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("titlerepair: read rows after id %d: %w", after, err)
	}
	return out, nil
}

// classify counts one row and hands a candidate to the cull. It decides on
// Settles' ok alone, which for a verdict from the title is Actionable's; the
// verdict is for the breakdown.
func (r *Repairer) classify(ctx context.Context, c candidate, o Options, res *Result) {
	res.Examined++
	v, ok := instrumentaltitle.Settles(c.title, c.album, c.artist, c.albumArtist)
	// The worker replaces the row's album with the audio file's where the file
	// has one. An album only withdraws a Strong verdict (a karaoke album), so
	// only a title that is Strong alone costs a read here (releaseOwn reads it
	// for a row Strong by album). The path test is the worker's.
	unreadable := false
	if alone := instrumentaltitle.Classify(c.title, ""); alone.Tier == instrumentaltitle.Strong &&
		o.FileAlbum != nil && strings.TrimSpace(c.source) != "" {
		switch album, err := o.FileAlbum(c.source); {
		case err != nil: // the error may name the path, so it is not logged
			r.log.Warn("titlerepair: audio file album unreadable, row skipped", "id", c.id)
			v, ok, unreadable = alone, false, true
		case album != "":
			v, ok = instrumentaltitle.Settles(c.title, album, c.artist, c.albumArtist)
		}
	}
	switch v.Tier {
	case instrumentaltitle.None:
		res.NoSignal++
		return
	case instrumentaltitle.Weak:
		res.Weak++
		switch {
		case !c.detector.Valid:
			res.WeakDetectorUnscored++
		case c.detector.Int64 == 1:
			res.WeakDetectorInstrumental++
		default:
			res.WeakDetectorNotInstrumental++
		}
		if v.Family == instrumentaltitle.FamilyInstrumental && v.Position == instrumentaltitle.PositionAlbum {
			res.WeakAlbumInstrumental++
		}
		return
	case instrumentaltitle.Strong:
	}
	res.Strong++
	res.StrongBy[StrongKey{v.Family, v.Position}]++
	byAlbum := v.Position == instrumentaltitle.PositionRelease
	if byAlbum {
		res.StrongByAlbum++
	} else {
		res.StrongByTitle++
	}
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
	case byAlbum:
		switch own, unread := r.releaseOwn(ctx, c, o); {
		case unread:
			res.Unreadable++
			row.Disposition = DispositionUnreadable
		case !own:
			res.ByAlbum++
			row.Disposition = DispositionByAlbum
		default:
			res.Candidates++
			row.Disposition, row.Files = r.cull(ctx, c, o, res, true)
		}
	default:
		res.Candidates++
		row.Disposition, row.Files = r.cull(ctx, c, o, res, false)
	}
	if o.OnRow != nil {
		o.OnRow(row)
	}
}

// releaseOwn reports whether a row Strong by album is the release's own, so the
// cull may act on it (the safety rule of #1525): its edition is instrumental or,
// in a dry run, the split would re-key it (Split runs first under Apply, so an
// empty edition then is a row it left shared). And the audio file's album, where
// it has one, still names the release, as the worker re-reads it; a failed read
// is unread. A row Strong by album is Strong by its stored tags, which is the
// split's re-key test, so a dry run needs only the split's skips.
//
// The audio file must exist even with no reader (FileAlbum is nil with
// enrichment off; a read of a missing file fails). With no block to prove an
// interrupted cull, Mark reopens a settled row whose lyric file is gone on the
// row's own record (MarkRequest.NoBlock), so a track deleted with its lyric
// file would be reopened and get a marker. A missing audio file is unread.
func (r *Repairer) releaseOwn(ctx context.Context, c candidate, o Options) (own, unread bool) {
	if c.edition != queue.EditionInstrumental {
		if o.Apply {
			return false, false
		}
		p, err := plan(ctx, r.db, c.id, &SplitResult{})
		if err != nil || p.skip != nil {
			return false, false
		}
	}
	if strings.TrimSpace(c.source) == "" {
		return true, false
	}
	if o.FileAlbum == nil {
		if _, err := os.Stat(c.source); err != nil { // the error names the path, so it is not logged
			r.log.Warn("titlerepair: audio file missing, row skipped", "id", c.id)
			return false, true
		}
		return true, false
	}
	album, err := o.FileAlbum(c.source)
	if err != nil { // the error may name the path, so it is not logged
		r.log.Warn("titlerepair: audio file album unreadable, row skipped", "id", c.id)
		return false, true
	}
	return album == "" || instrumentaltitle.InstrumentalRelease(c.title, album, c.artist, c.albumArtist), false
}
