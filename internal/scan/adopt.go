package scan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/sidecar"
)

// DefaultAdoptBudget caps how many lyric files one scan reads to classify
// (#1409). The scan has just walked these directories, so the disks are awake;
// adopted rows leave the query, so later scans continue the backlog. Link-only
// adoptions and rows with no lyric file read nothing and do not count.
const DefaultAdoptBudget = 2000

const adoptPageSize = 500 // keyset page size of the unlinked-row walk

// AdoptStore is the queue surface adoption needs. *queue.DBQueue satisfies it.
type AdoptStore interface {
	ListUnlinkedSettled(ctx context.Context, libraryID int64, pathPrefix string, afterID int64, limit int) ([]queue.AdoptCandidate, error)
	LinkSettled(ctx context.Context, c queue.AdoptCandidate, now time.Time, backup func(queue.AdoptRecord) error) (queue.AdoptResult, error)
	InsertSettledDone(ctx context.Context, c queue.AdoptCandidate, cls queue.AdoptClass, now time.Time, backup func(queue.AdoptRecord) error) (queue.AdoptResult, error)
}

// AdoptCounts is the whole report of a pass: counts only, never a path or title.
type AdoptCounts struct {
	Inserted      int // new 'done' rows adopted from a lyric file
	Linked        int // linked to an existing row, nothing read
	NotSettled    int // identity row exists but is not done; left unlinked for a later scan
	AlreadyLinked int // the link already existed (another writer won); nothing written, nothing read
	Unexpected    int // the store returned a result this adopter does not expect; nothing assumed
	NoSidecar     int // done index row, no lyric file beside the audio; not adopted
	Unreadable    int // lyric file present but could not be classified
	NoIdentity    int // row lacks an artist or a title key (half-empty pairs collide)
	WriteFailed   int // adoption write or backup failed; database unchanged
	BudgetHit     bool
}

// Adopter gives every settled scan_results row of a scanned library a
// work_queue row (#1409), so queue-only readers (upgrade sweep, timing recheck,
// reports) can see files that already have lyrics beside them.
//
// A done scan row with NO lyric file beside the audio is not adopted: it is
// counted (NoSidecar) and never content-read. Likely cause: EnqueuePending's
// cache-hit path marks a scan row done with no Queue.Enqueue and no sidecar
// write. There is nothing on disk to classify, and a row invented without
// lyrics would mislead every queue-only reader.
type Adopter struct {
	Store AdoptStore
	// Backup is REQUIRED: it must make the record durable before it returns.
	Backup func(queue.AdoptRecord) error
	Now    func() time.Time
	Budget int // 0 means DefaultAdoptBudget

	// Seams; nil means the production helpers.
	List        func(dir string) sidecar.Listing
	ClassifyLRC func(path string) (lyrics.SyncTier, error)
	RungOnDisk  func(fp string, l sidecar.Listing) lyrics.Rung
}

// Run adopts the unlinked settled rows of libraryID at or under pathPrefix
// ("" = the whole library).
//
// A row with no lyric file is not remembered (a marker needs a schema change),
// so it is looked at again every scan: one cached directory listing, never a
// content read.
func (a *Adopter) Run(ctx context.Context, libraryID int64, pathPrefix string) (AdoptCounts, error) {
	var n AdoptCounts
	if a.Store == nil || a.Backup == nil {
		return n, errors.New("scan: adopt: store and backup are required")
	}
	budget := a.Budget
	if budget <= 0 {
		budget = DefaultAdoptBudget
	}
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	list, classifyLRC, rungOnDisk := a.List, a.ClassifyLRC, a.RungOnDisk
	if list == nil {
		list = sidecar.List
	}
	if classifyLRC == nil {
		classifyLRC = lyrics.ClassifyLRCFile
	}
	if rungOnDisk == nil {
		rungOnDisk = lyrics.RungOnDisk
	}

	var lastDir string
	var listing sidecar.Listing
	haveListing := false
	var after int64
	for {
		if err := ctx.Err(); err != nil {
			return n, fmt.Errorf("scan: adopt: %w", err)
		}
		page, err := a.Store.ListUnlinkedSettled(ctx, libraryID, pathPrefix, after, adoptPageSize)
		if err != nil {
			return n, fmt.Errorf("scan: adopt: %w", err)
		}
		if len(page) == 0 {
			return n, nil
		}
		for _, c := range page {
			after = c.ScanResultID
			if err := ctx.Err(); err != nil {
				return n, fmt.Errorf("scan: adopt: %w", err)
			}
			if c.ArtistKey == "" || c.TitleKey == "" {
				n.NoIdentity++
				continue
			}
			// Existing row for this song: link only, no read.
			res, err := a.Store.LinkSettled(ctx, c, now(), a.Backup)
			if err != nil {
				n.WriteFailed++
				if errors.Is(err, queue.ErrAdoptBackup) {
					return n, fmt.Errorf("scan: adopt: stopping, backup cannot be written: %w", err)
				}
				continue
			}
			switch res {
			case queue.AdoptLinked:
				n.Linked++
				continue
			case queue.AdoptNotSettled:
				n.NotSettled++
				continue
			case queue.AdoptAlreadyLinked:
				n.AlreadyLinked++
				continue
			case queue.AdoptNoRow:
				// No row for this song: fall through to classify and insert.
			default:
				n.Unexpected++
				continue
			}
			dir := filepath.Dir(c.FilePath)
			if !haveListing || dir != lastDir {
				listing, lastDir, haveListing = list(dir), dir, true
			}
			path, kind := pickSidecar(listing, c.FilePath)
			if kind == sidecarNone {
				n.NoSidecar++
				continue
			}
			if n.Inserted+n.Unreadable+n.WriteFailed >= budget {
				n.BudgetHit = true
				return n, nil
			}
			cls, ok := classifySidecar(path, kind, listing, classifyLRC, rungOnDisk)
			if !ok {
				n.Unreadable++
				continue
			}
			res, err = a.Store.InsertSettledDone(ctx, c, cls, now(), a.Backup)
			switch {
			case err != nil:
				n.WriteFailed++
				if errors.Is(err, queue.ErrAdoptBackup) {
					return n, fmt.Errorf("scan: adopt: stopping, backup cannot be written: %w", err)
				}
			case res == queue.AdoptInserted:
				n.Inserted++
			case res == queue.AdoptLinked:
				n.Linked++
			case res == queue.AdoptNotSettled:
				n.NotSettled++
			case res == queue.AdoptAlreadyLinked:
				n.AlreadyLinked++
			default:
				n.Unexpected++
			}
		}
	}
}

type sidecarKind int

const (
	sidecarNone sidecarKind = iota
	sidecarLRC
	sidecarTXT
)

// pickSidecar finds the lyric file beside audio: a .lrc first, else a .txt, any
// extension case, regular files only (a symlink is never followed).
func pickSidecar(l sidecar.Listing, audio string) (string, sidecarKind) {
	stem := sidecar.StemOf(audio)
	for _, k := range []struct {
		ext  string
		kind sidecarKind
	}{{sidecar.ExtLineSynced, sidecarLRC}, {sidecar.ExtUnsynced, sidecarTXT}} {
		for _, v := range l.Variants(stem + k.ext) {
			if fi, err := os.Lstat(v); err == nil && fi.Mode().IsRegular() {
				return v, k.kind
			}
		}
	}
	return "", sidecarNone
}

// classifySidecar reads the one file picked and returns its queue record, with
// the helpers reconcile-sync-tier uses. ok=false: could not be classified.
func classifySidecar(path string, kind sidecarKind, l sidecar.Listing, classifyLRC func(string) (lyrics.SyncTier, error), rungOnDisk func(string, sidecar.Listing) lyrics.Rung) (queue.AdoptClass, bool) {
	if kind == sidecarLRC {
		tier, err := classifyLRC(path)
		if err != nil {
			return queue.AdoptClass{}, false
		}
		return queue.AdoptClass{OutcomeType: "synced", SyncTier: string(tier)}, true
	}
	switch rungOnDisk(path, l) {
	case lyrics.RungInstrumental:
		return queue.AdoptClass{OutcomeType: "instrumental"}, true
	case lyrics.RungUnsynced:
		return queue.AdoptClass{OutcomeType: "unsynced"}, true
	default:
		return queue.AdoptClass{}, false
	}
}
