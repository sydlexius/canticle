package titlerepair

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
)

// The split of legacy shared rows (#1525). A row made before the edition column
// has edition '' even when it describes a copy on an instrumental release, and
// one row may link the scan results of both an instrumental release and its sung
// twin. Split re-keys a row that describes the release copy to
// queue.EditionInstrumental and detaches the other edition's scan results,
// returning them to pending so the next scan offers them again. It is
// database-only: no file, lyric cache entry or block is touched, and no row is
// reopened. A hand-protected row is re-keyed like any other: nothing it records
// changes.

// splitOp labels a split's backup records.
const splitOp = "reconcile-instrumental-titles-split"

// SplitOptions scopes one Split.
type SplitOptions struct {
	// LibraryID limits the walk to rows linked to that library; nil is every row.
	LibraryID *int64
	// Apply changes the rows. Without it Split only counts.
	Apply bool
	// Report makes one record durable before its row changes. Required with
	// Apply, never called without.
	Report func(SplitRecord) error
}

// SplitRecord is one row's change, written before it is made. It names ids and
// fixed words only. Restoring is setting work_queue.edition back to empty, putting
// back each Detached link, its status and the row's scan_result_id. Once a scan
// has run after the split, the sung twin may have a new row with an empty
// edition and the same keys, and setting edition back to empty then collides
// with it: that row must be merged or removed first. Migration 075's Down
// deletes every row whose edition is not empty, re-keyed legacy rows included,
// so once a split has run, roll back by restoring a copy of the database taken
// before the split (canticle does not make one), not by migrating down.
type SplitRecord struct {
	Op         string `json:"op"`
	WorkItemID int64  `json:"work_item_id"`
	// Edition is the row's new edition; '' when only scan results are detached.
	Edition  string     `json:"edition"`
	Detached []Detached `json:"detached,omitempty"`
	// ScanResultID is the row's scan_result_id before, when it named a detached one.
	ScanResultID int64 `json:"scan_result_id,omitempty"`
}

// Detached is one scan result a split unlinks, with its status before.
type Detached struct {
	ScanResultID int64  `json:"scan_result_id"`
	Status       string `json:"status"`
}

// SplitResult counts one Split. Rows = Split + InFlight + BothFolders +
// Collision + Errors; Split is what a dry run would split, or what Apply split.
type SplitResult struct {
	Rows, Split, ReKeyed, Detached int
	// Pending: detached scan results set back to pending (Apply only); one
	// another row still links keeps its status.
	Pending int
	// InFlight: a worker holds the row. BothFolders: an output sits in a folder
	// of the other edition's copy, so the row cannot be said to be one edition's.
	// Collision: an instrumental row with the same keys exists already.
	InFlight, BothFolders, Collision, Errors int
}

// splitLink is one scan result linked to a row.
type splitLink struct {
	id                                        int64
	artist, title, album, albumArtist, outdir string
	status                                    string
}

// splitPlan is what one row needs. skip names the counter that stops it.
type splitPlan struct {
	rekey      bool
	twins      []splitLink
	columnTwin int64 // the row's scan_result_id, when it is a twin
	skip       *int
}

func (p splitPlan) needed() bool { return p.rekey || len(p.twins) > 0 }

// querier is a *sql.DB or the *sql.Tx of an apply.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// edition is the edition a copy with these tags belongs to: the album-level
// verdict, from the stored tags.
func edition(title, album, artist, albumArtist string) string {
	return queue.ReleaseEdition(models.Track{TrackName: title, AlbumName: album, ArtistName: artist, AlbumArtist: albumArtist})
}

// Split walks every row with an empty edition and splits the ones that need
// it. A row that fails is counted and the walk goes on; a cancel stops it.
func (r *Repairer) Split(ctx context.Context, o SplitOptions) (SplitResult, error) {
	var res SplitResult
	if o.Apply && o.Report == nil {
		return res, errors.New("titlerepair: a split needs a Report to record the rows first")
	}
	for after := int64(0); ; {
		ids, err := r.splitPage(ctx, o.LibraryID, after)
		if err != nil {
			return res, err
		}
		for _, id := range ids {
			after = id
			r.splitRow(ctx, id, o, &res)
			if err := ctx.Err(); err != nil {
				return res, fmt.Errorf("titlerepair: split stopped after id %d: %w", id, err)
			}
		}
		if len(ids) < pageSize {
			return res, nil
		}
	}
}

// splitPage reads the ids of the next page of rows with an empty edition.
func (r *Repairer) splitPage(ctx context.Context, libraryID *int64, after int64) ([]int64, error) {
	query, args := `SELECT id FROM work_queue WHERE id > ? AND edition = ''`, []any{after}
	if libraryID != nil {
		query += ` AND ` + reports.LibraryPredicate
		args = append(args, *libraryID)
	}
	rows, err := r.db.QueryContext(ctx, query+` ORDER BY id LIMIT ?`, append(args, pageSize)...)
	if err != nil {
		return nil, fmt.Errorf("titlerepair: select split rows after id %d: %w", after, err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("titlerepair: scan split row after id %d: %w", after, err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// splitRow plans one row from a plain read and, under Apply, plans it again
// inside the transaction that changes it, so the change rests on what the row
// is at that moment.
func (r *Repairer) splitRow(ctx context.Context, id int64, o SplitOptions, res *SplitResult) {
	p, err := plan(ctx, r.db, id, res)
	if err == nil && p.needed() && p.skip == nil && o.Apply {
		p, err = r.applySplit(ctx, id, o, res)
	}
	switch {
	case err != nil:
		res.Rows++
		res.Errors++
		r.log.Error("titlerepair: split of a row failed; a rerun reports where it stands", "id", id, "error", err)
	case !p.needed():
	case p.skip != nil:
		res.Rows++
		*p.skip++
	default:
		res.Rows++
		res.Split++
		res.Detached += len(p.twins)
		if p.rekey {
			res.ReKeyed++
		}
	}
}

// plan reads row id and its linked scan results through q and decides what the
// row needs. A row whose edition is no longer empty needs nothing.
func plan(ctx context.Context, q querier, id int64, res *SplitResult) (splitPlan, error) {
	var artist, title, album, albumArtist, artistKey, titleKey, status, outdir, filename, outputs string
	var column sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT artist, title, album, album_artist, artist_key, title_key, status,
	        outdir, filename, COALESCE(output_paths, ''), scan_result_id
	   FROM work_queue WHERE id = ? AND edition = ''`, id).Scan(&artist, &title, &album, &albumArtist,
		&artistKey, &titleKey, &status, &outdir, &filename, &outputs, &column)
	if errors.Is(err, sql.ErrNoRows) {
		return splitPlan{}, nil
	} else if err != nil {
		return splitPlan{}, fmt.Errorf("titlerepair: read row %d: %w", id, err)
	}
	links, err := linked(ctx, q, id, column)
	if err != nil {
		return splitPlan{}, err
	}
	own := edition(title, album, artist, albumArtist)
	p := splitPlan{rekey: own == queue.EditionInstrumental}
	twinDirs := map[string]bool{}
	for _, l := range links {
		if edition(l.title, l.album, l.artist, l.albumArtist) != own {
			p.twins = append(p.twins, l)
			twinDirs[filepath.Clean(l.outdir)] = true
			if column.Valid && column.Int64 == l.id {
				p.columnTwin = l.id
			}
		}
	}
	if !p.needed() {
		return p, nil
	}
	if status == "processing" {
		p.skip = &res.InFlight
		return p, nil
	}
	// The outputs as the queue reads them: the JSON list, else outdir and filename.
	paths := []models.OutputPath{{Outdir: outdir, Filename: filename}}
	if outputs != "" {
		var list []models.OutputPath
		if err := json.Unmarshal([]byte(outputs), &list); err != nil {
			return p, fmt.Errorf("titlerepair: read the outputs of row %d: %w", id, err)
		}
		if len(list) > 0 {
			paths = list
		}
	}
	for _, out := range paths {
		if twinDirs[filepath.Clean(out.Outdir)] {
			p.skip = &res.BothFolders
			return p, nil
		}
	}
	if p.rekey {
		var taken bool
		if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM work_queue
		     WHERE artist_key = ? AND title_key = ? AND edition = ?)`,
			artistKey, titleKey, queue.EditionInstrumental).Scan(&taken); err != nil {
			return p, fmt.Errorf("titlerepair: probe the edition of row %d: %w", id, err)
		}
		if taken {
			p.skip = &res.Collision
		}
	}
	return p, nil
}

// linked reads the scan results row id links: the junction and its own
// scan_result_id.
func linked(ctx context.Context, q querier, id int64, column sql.NullInt64) ([]splitLink, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, artist, title, album, album_artist, outdir, status
	    FROM scan_results
	   WHERE id IN (SELECT scan_result_id FROM work_queue_scan_results WHERE work_queue_id = ?) OR id = ?
	   ORDER BY id`, id, column)
	if err != nil {
		return nil, fmt.Errorf("titlerepair: read the scan results of row %d: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []splitLink
	for rows.Next() {
		var l splitLink
		if err := rows.Scan(&l.id, &l.artist, &l.title, &l.album, &l.albumArtist, &l.outdir, &l.status); err != nil {
			return nil, fmt.Errorf("titlerepair: scan a scan result of row %d: %w", id, err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// applySplit plans row id again inside one transaction and, if it still needs a
// split, records it through o.Report and then changes it. A Report failure
// changes nothing.
func (r *Repairer) applySplit(ctx context.Context, id int64, o SplitOptions, res *SplitResult) (splitPlan, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return splitPlan{}, fmt.Errorf("titlerepair: begin the split of row %d: %w", id, err)
	}
	defer func() { _ = tx.Rollback() }()
	p, err := plan(ctx, tx, id, res)
	if err != nil || !p.needed() || p.skip != nil {
		return p, err
	}
	rec := SplitRecord{Op: splitOp, WorkItemID: id, ScanResultID: p.columnTwin}
	if p.rekey {
		rec.Edition = queue.EditionInstrumental
	}
	for _, l := range p.twins {
		rec.Detached = append(rec.Detached, Detached{l.id, l.status})
	}
	if err := o.Report(rec); err != nil {
		return p, fmt.Errorf("titlerepair: record the split of row %d: %w", id, err)
	}
	if p.rekey {
		if err := reKey(ctx, tx, id); err != nil {
			return p, err
		}
	}
	if p.columnTwin != 0 {
		if err := detachColumn(ctx, tx, id); err != nil {
			return p, err
		}
	}
	pending := 0
	for _, l := range p.twins {
		if _, err := tx.ExecContext(ctx, `DELETE FROM work_queue_scan_results WHERE work_queue_id = ? AND scan_result_id = ?`,
			id, l.id); err != nil {
			return p, fmt.Errorf("titlerepair: detach a scan result of row %d: %w", id, err)
		}
		// Back to pending unless another row still links it: that row owns it.
		n, err := tx.ExecContext(ctx, `UPDATE scan_results SET status = 'pending'
		  WHERE id = ? AND status != 'pending'
		    AND NOT EXISTS (SELECT 1 FROM work_queue_scan_results WHERE scan_result_id = ?)
		    AND NOT EXISTS (SELECT 1 FROM work_queue WHERE scan_result_id = ?)`, l.id, l.id, l.id)
		if err != nil {
			return p, fmt.Errorf("titlerepair: return a scan result of row %d to pending: %w", id, err)
		}
		if k, _ := n.RowsAffected(); k > 0 {
			pending++
		}
	}
	if err := tx.Commit(); err != nil {
		return p, fmt.Errorf("titlerepair: commit the split of row %d: %w", id, err)
	}
	res.Pending += pending
	return p, nil
}

// execer is a *sql.DB or the *sql.Tx of an apply.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// reKey sets row id's edition to instrumental. The statement itself refuses a
// row a worker holds or one already re-keyed; changing no row is an error, so
// nothing after it runs on a row that did not change.
func reKey(ctx context.Context, ex execer, id int64) error {
	n, err := ex.ExecContext(ctx, `UPDATE work_queue SET edition = ?
		WHERE id = ? AND edition = '' AND status != 'processing'`, queue.EditionInstrumental, id)
	if err != nil {
		return fmt.Errorf("titlerepair: re-key row %d: %w", id, err)
	}
	return oneRow(n, "re-key", id)
}

// detachColumn clears row id's scan_result_id, refusing a row a worker holds.
func detachColumn(ctx context.Context, ex execer, id int64) error {
	n, err := ex.ExecContext(ctx, `UPDATE work_queue SET scan_result_id = NULL
		WHERE id = ? AND status != 'processing'`, id)
	if err != nil {
		return fmt.Errorf("titlerepair: detach the scan result of row %d: %w", id, err)
	}
	return oneRow(n, "detach the scan result of", id)
}

// oneRow is an error unless n changed exactly one row.
func oneRow(n sql.Result, what string, id int64) error {
	k, err := n.RowsAffected()
	if err != nil {
		return fmt.Errorf("titlerepair: %s row %d: %w", what, id, err)
	}
	if k != 1 {
		return fmt.Errorf("titlerepair: %s row %d changed %d rows; it changed under the split", what, id, k)
	}
	return nil
}
