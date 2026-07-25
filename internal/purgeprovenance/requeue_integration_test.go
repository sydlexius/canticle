// Integration tests covering the promise purge-provenance actually makes: a
// purged sidecar is not merely deleted, it is genuinely RE-FETCHED by the next
// scan. These drive the real scan.Enqueuer against a real SQLite database
// rather than a fake, because the defect they lock out lives in the seam
// between the two packages, not inside either one.
//
// Origin: the pre-push hostile review of #474 (c6c0c30) left these as
// reproductions. The cache-hit case failed and was a real data-loss defect
// (file deleted, never rewritten); it is now a passing regression lock. The
// double-enqueue pair proved to be a fixture artifact and is kept as a matched
// control/faithful pair documenting the trap.
package purgeprovenance

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scan"
)

// newEnqueuer builds the real scan.Enqueuer over a real DBQueue, cache, and
// scan repository -- the exact wiring serve mode uses -- so "the next scan" in
// these tests is the production path, not a stand-in.
func newEnqueuer(sqlDB *sql.DB) *scan.Enqueuer {
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	return &scan.Enqueuer{
		Results:  scan.New(sqlDB),
		Queue:    q,
		Cache:    cache.New(sqlDB),
		Priority: queue.PriorityScan,
	}
}

// TestRun_PurgeInvalidatesCacheSoNextScanRefetches is the regression lock for
// the data-loss defect this package's cache invalidation exists to prevent.
//
// scan.Enqueuer.EnqueuePending consults Cache.Lookup BEFORE enqueueing. Before
// the fix, a purge deleted the sidecar and requeued the row, but left the
// purged provider's lyrics sitting in lyrics_cache; the next scan therefore
// marked the row 'done' as a cache hit, enqueued nothing, and never rewrote the
// file. The user's lyric file was deleted and never restored.
//
// The purge must now invalidate the cache entry, so the next scan enqueues real
// work.
func TestRun_PurgeInvalidatesCacheSoNextScanRefetches(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dir := filepath.Join(root, "A")
	path := filepath.Join(dir, "song.lrc")
	writeSidecar(t, path, "musixmatch")
	srID, wqID := seedTrack(t, ctx, sqlDB, libID, dir, "song.lrc", "done")

	// scan_results carries artist='Artist' title='Title' (see seedTrack), and
	// ListPendingByLibrary -> EnqueuePending looks the cache up on THOSE values.
	c := cache.New(sqlDB)
	if err := c.Store(ctx, "Artist", "Title", normalize.DurationBucket(0), "[00:01.00]cached\n"); err != nil {
		t.Fatalf("cache store: %v", err)
	}

	res, err := New(sqlDB).Run(ctx, Options{
		Roots: []string{root}, LibraryID: &libID,
		Filter: Filter{Source: "musixmatch"}, DryRun: false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.CacheInvalidated != 1 {
		t.Errorf("CacheInvalidated = %d, want 1", res.CacheInvalidated)
	}
	if _, serr := os.Stat(path); serr == nil {
		t.Fatal("sidecar should be gone")
	}
	if got := rowStatus(t, ctx, sqlDB, "scan_results", srID); got != "pending" {
		t.Errorf("scan_results status = %q, want pending", got)
	}
	if got := rowStatus(t, ctx, sqlDB, "work_queue", wqID); got != "deferred" {
		t.Errorf("work_queue status = %q, want deferred", got)
	}
	if _, lerr := c.Lookup(ctx, "Artist", "Title", normalize.DurationBucket(0)); !errors.Is(lerr, sql.ErrNoRows) {
		t.Errorf("cache entry survived the purge: err = %v, want sql.ErrNoRows", lerr)
	}

	// The "next scan" the command's summary promises will re-fetch.
	enq, hits, err := newEnqueuer(sqlDB).EnqueuePending(ctx, models.Library{ID: libID, Path: root})
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if hits != 0 {
		t.Errorf("purged track was satisfied from cache (hits=%d); the deleted sidecar would never be rewritten", hits)
	}
	if enq != 1 {
		t.Errorf("enqueued = %d, want 1 (the purged track must be queued for re-fetch)", enq)
	}
}

// TestRun_PurgeInvalidatesEveryDurationBucket locks the bucket-0 fallback.
//
// cache.Lookup falls back to the bucket-0 unknown-duration sentinel row when an
// exact-bucket lookup misses. Invalidating only the bucket the purged fetch was
// stored under would therefore leave a sibling row that still satisfies the very
// lookup EnqueuePending performs, and the fix would silently do nothing in
// exactly the case it matters. Every bucket for the key must go.
func TestRun_PurgeInvalidatesEveryDurationBucket(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dir := filepath.Join(root, "A")
	writeSidecar(t, filepath.Join(dir, "song.lrc"), "musixmatch")
	seedTrack(t, ctx, sqlDB, libID, dir, "song.lrc", "done")

	c := cache.New(sqlDB)
	// Two rows under the same (artist, title): a real duration bucket and the
	// bucket-0 sentinel Lookup falls back to.
	for _, bucket := range []int{0, normalize.DurationBucket(213)} {
		if err := c.Store(ctx, "Artist", "Title", bucket, "[00:01.00]cached\n"); err != nil {
			t.Fatalf("cache store bucket %d: %v", bucket, err)
		}
	}

	res, err := New(sqlDB).Run(ctx, Options{
		Roots: []string{root}, LibraryID: &libID,
		Filter: Filter{Source: "musixmatch"}, DryRun: false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.CacheInvalidated != 2 {
		t.Errorf("CacheInvalidated = %d, want 2 (both duration buckets)", res.CacheInvalidated)
	}
	// A lookup at the real bucket must miss AND find no bucket-0 fallback.
	if _, lerr := c.Lookup(ctx, "Artist", "Title", normalize.DurationBucket(213)); !errors.Is(lerr, sql.ErrNoRows) {
		t.Errorf("bucket-0 fallback still satisfies the lookup after purge: err = %v, want sql.ErrNoRows", lerr)
	}

	enq, hits, err := newEnqueuer(sqlDB).EnqueuePending(ctx, models.Library{ID: libID, Path: root})
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if hits != 0 || enq != 1 {
		t.Errorf("next scan: enqueued=%d hits=%d, want 1/0", enq, hits)
	}
}

// TestRun_PurgeLeavesOtherTracksCacheIntact: invalidation is scoped to the
// purged track's key. A neighboring track's cache entry must survive, or the
// purge would trigger a library-wide re-fetch wave.
func TestRun_PurgeLeavesOtherTracksCacheIntact(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dir := filepath.Join(root, "A")
	writeSidecar(t, filepath.Join(dir, "song.lrc"), "musixmatch")
	seedTrack(t, ctx, sqlDB, libID, dir, "song.lrc", "done")

	c := cache.New(sqlDB)
	if err := c.Store(ctx, "Artist", "Title", 0, "[00:01.00]purged\n"); err != nil {
		t.Fatalf("cache store: %v", err)
	}
	if err := c.Store(ctx, "Other Artist", "Other Title", 0, "[00:01.00]kept\n"); err != nil {
		t.Fatalf("cache store other: %v", err)
	}

	res, err := New(sqlDB).Run(ctx, Options{
		Roots: []string{root}, LibraryID: &libID,
		Filter: Filter{Source: "musixmatch"}, DryRun: false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.CacheInvalidated != 1 {
		t.Errorf("CacheInvalidated = %d, want 1 (only the purged track)", res.CacheInvalidated)
	}
	if _, lerr := c.Lookup(ctx, "Other Artist", "Other Title", 0); lerr != nil {
		t.Errorf("unrelated cache entry was destroyed: %v", lerr)
	}
}

// TestRun_DryRunDoesNotInvalidateCache: a dry run previews and mutates nothing,
// including the cache.
func TestRun_DryRunDoesNotInvalidateCache(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dir := filepath.Join(root, "A")
	writeSidecar(t, filepath.Join(dir, "song.lrc"), "musixmatch")
	seedTrack(t, ctx, sqlDB, libID, dir, "song.lrc", "done")

	c := cache.New(sqlDB)
	if err := c.Store(ctx, "Artist", "Title", 0, "[00:01.00]cached\n"); err != nil {
		t.Fatalf("cache store: %v", err)
	}

	res, err := New(sqlDB).Run(ctx, Options{
		Roots: []string{root}, LibraryID: &libID,
		Filter: Filter{Source: "musixmatch"}, DryRun: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.CacheInvalidated != 0 {
		t.Errorf("dry run invalidated %d cache entries; want 0", res.CacheInvalidated)
	}
	if _, lerr := c.Lookup(ctx, "Artist", "Title", 0); lerr != nil {
		t.Errorf("dry run destroyed the cache entry: %v", lerr)
	}
}

// TestRun_BackupFailureLeavesCacheAndSidecarIntact: backup-first discipline
// extends to the cache. If the caller's Report (which writes and fsyncs the
// restorable record) fails, nothing at all happens to that sidecar -- not the
// file, not the rows, not the cache entry.
func TestRun_BackupFailureLeavesCacheAndSidecarIntact(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dir := filepath.Join(root, "A")
	path := filepath.Join(dir, "song.lrc")
	writeSidecar(t, path, "musixmatch")
	srID, _ := seedTrack(t, ctx, sqlDB, libID, dir, "song.lrc", "done")

	c := cache.New(sqlDB)
	if err := c.Store(ctx, "Artist", "Title", 0, "[00:01.00]cached\n"); err != nil {
		t.Fatalf("cache store: %v", err)
	}

	res, err := New(sqlDB).Run(ctx, Options{
		Roots: []string{root}, LibraryID: &libID,
		Filter: Filter{Source: "musixmatch"}, DryRun: false,
		Report: func(Record) error { return errors.New("disk full") },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Errors != 1 || res.Deleted != 0 || res.CacheInvalidated != 0 {
		t.Errorf("got errors=%d deleted=%d invalidated=%d, want 1/0/0", res.Errors, res.Deleted, res.CacheInvalidated)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("sidecar deleted despite backup failure: %v", serr)
	}
	if _, lerr := c.Lookup(ctx, "Artist", "Title", 0); lerr != nil {
		t.Errorf("cache invalidated despite backup failure: %v", lerr)
	}
	if got := rowStatus(t, ctx, sqlDB, "scan_results", srID); got != "done" {
		t.Errorf("scan_results mutated despite backup failure; status = %q", got)
	}
}

// TestRun_UnlinkedSidecarCountedNotSilentlyAssumed: a matched sidecar that no
// scan_results row claims has no derivable cache key. It is still deleted (the
// operator asked for that), but the run must SAY it could not requeue or
// invalidate for it rather than let the summary imply a re-fetch guarantee it
// does not have.
func TestRun_UnlinkedSidecarCountedNotSilentlyAssumed(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dir := filepath.Join(root, "A")
	path := filepath.Join(dir, "orphan.lrc")
	writeSidecar(t, path, "musixmatch") // no seedTrack: nothing links to it

	res, err := New(sqlDB).Run(ctx, Options{
		Roots: []string{root}, LibraryID: &libID,
		Filter: Filter{Source: "musixmatch"}, DryRun: false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1", res.Deleted)
	}
	if res.UnlinkedNoCacheKey != 1 {
		t.Errorf("UnlinkedNoCacheKey = %d, want 1", res.UnlinkedNoCacheKey)
	}
	if res.CacheInvalidated != 0 {
		t.Errorf("CacheInvalidated = %d, want 0 (no key derivable)", res.CacheInvalidated)
	}
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Errorf("orphan sidecar should still be deleted")
	}
}

// ============================================================================
// SEEDING ARTIFACT, NOT A DEFECT -- the control half of a matched pair.
//
// This "reproduces" a duplicate work_queue row, but the duplicate is caused by
// the SEED, not by purgeprovenance. seedTrack enqueues with
// models.Track{TrackName: filename}, so work_queue.title = "song.lrc" and
// title_key = "song.lrc"; but the scan_results row it links to carries
// title = 'Title'. Those two rows disagree about the track's identity, which a
// real scan never produces (a real work_queue row is built FROM its
// scan_results row via scanInputs, internal/scan/enqueuer.go).
//
// On re-enqueue, scanInputs derives title_key='title' from scan_results, which
// does not match the seeded work_queue row's title_key='song.lrc', so
// Enqueue's ON CONFLICT(artist_key, title_key) target finds nothing to update
// and performs an INSERT instead. Duplicate = 1 -> 2.
//
// Kept ONLY as the control for the faithful-seed test below, so a future reader
// who reproduces a "duplicate row" recognizes the fixture trap rather than
// filing it again. It asserts the divergence, not a product behavior.
// ============================================================================
func TestRun_DualResetDoubleEnqueue_MisseededControl(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dir := filepath.Join(root, "A")
	writeSidecar(t, filepath.Join(dir, "song.lrc"), "musixmatch")
	_, wqID := seedTrack(t, ctx, sqlDB, libID, dir, "song.lrc", "done")

	var seededTitleKey string
	if err := sqlDB.QueryRowContext(ctx, `SELECT title_key FROM work_queue WHERE id=?`, wqID).Scan(&seededTitleKey); err != nil {
		t.Fatalf("read seeded title_key: %v", err)
	}
	var srTitle string
	if err := sqlDB.QueryRowContext(ctx, `SELECT title FROM scan_results WHERE library_id=?`, libID).Scan(&srTitle); err != nil {
		t.Fatalf("read scan_results title: %v", err)
	}
	// The premise of this control: the fixture's two rows disagree on identity.
	if seededTitleKey == normalize.NormalizeKey(srTitle) {
		t.Fatalf("control is no longer misseeded (title_key=%q matches scan_results title=%q); "+
			"this test only has meaning as the divergent control for the faithful-seed case",
			seededTitleKey, srTitle)
	}

	if _, err := New(sqlDB).Run(ctx, Options{
		Roots: []string{root}, LibraryID: &libID,
		Filter: Filter{Source: "musixmatch"}, DryRun: false,
	}); err != nil {
		t.Fatal(err)
	}
	before := countWQ(t, ctx, sqlDB)
	if _, _, err := newEnqueuer(sqlDB).EnqueuePending(ctx, models.Library{ID: libID, Path: root}); err != nil {
		t.Fatal(err)
	}
	after := countWQ(t, ctx, sqlDB)
	t.Logf("work_queue rows before=%d after=%d (any duplicate here is the SEED's fault, not the purge's)", before, after)
}

// ============================================================================
// REGRESSION LOCK -- the faithful half of the pair.
//
// Same scenario, but seeded as a real scan produces it: work_queue and
// scan_results agree on the track identity. The re-enqueue then hits Enqueue's
// ON CONFLICT(artist_key, title_key) target, updates in place, and creates NO
// duplicate. It also PRESERVES purge's deliberate priority=-100.
//
// This is the honest answer to "does purge-provenance's reset double-enqueue?"
// -- it does not.
// ============================================================================
func TestRun_DualResetNoDoubleEnqueue_FaithfulSeed(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dir := filepath.Join(root, "A")
	writeSidecar(t, filepath.Join(dir, "song.lrc"), "musixmatch")
	srID, wqID := seedTrack(t, ctx, sqlDB, libID, dir, "song.lrc", "done")
	// Align work_queue's identity with its scan_results row, as a real scan does.
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE work_queue SET title='Title', title_key='title' WHERE id=?`, wqID); err != nil {
		t.Fatal(err)
	}

	if _, err := New(sqlDB).Run(ctx, Options{
		Roots: []string{root}, LibraryID: &libID,
		Filter: Filter{Source: "musixmatch"}, DryRun: false,
	}); err != nil {
		t.Fatal(err)
	}
	before := countWQ(t, ctx, sqlDB)

	enq, hits, err := newEnqueuer(sqlDB).EnqueuePending(ctx, models.Library{ID: libID, Path: root})
	if err != nil {
		t.Fatal(err)
	}
	after := countWQ(t, ctx, sqlDB)

	var prio int
	var status string
	if err := sqlDB.QueryRowContext(ctx, `SELECT priority, status FROM work_queue WHERE id=?`, wqID).Scan(&prio, &status); err != nil {
		t.Fatalf("read work_queue row: %v", err)
	}
	t.Logf("enqueued=%d hits=%d rows before=%d after=%d; wq %d: status=%s priority=%d; scan_results %d: %s",
		enq, hits, before, after, wqID, status, prio, srID, rowStatus(t, ctx, sqlDB, "scan_results", srID))

	if after != before {
		t.Errorf("duplicate work_queue row created: %d -> %d", before, after)
	}
	if prio != -100 {
		t.Errorf("re-enqueue clobbered purge's deliberate priority=-100 with %d", prio)
	}
}

// TestReadProvenanceTagsAdversarial documents how lyrics.ReadProvenanceTags
// classifies malformed sidecars, because purge-provenance now runs it against
// EVERY file in a library rather than a handful, and each classification is a
// deletion decision.
//
// Known finding (tracked separately, NOT fixed here): a UTF-8 BOM makes the
// header parser read the key as "\xef\xbb\xbf[source" rather than "source", so
// a genuinely canticle-written sidecar lands in the --no-source cohort and
// would be DELETED by that filter. This test asserts the current behavior so a
// future fix to the parser is a deliberate, visible change rather than a silent
// one; the assertion is written to fail loudly either way.
func TestReadProvenanceTagsAdversarial(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dir := filepath.Join(root, "A")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"bom.lrc":     "\xef\xbb\xbf[source:musixmatch]\n[00:01.00]x\n", // BOM -> misparsed
		"crlf.lrc":    "[source:musixmatch]\r\n[00:01.00]x\r\n",         // parses fine
		"bracket.lrc": "[source:musix]match]\n[00:01.00]x\n",
		"blanks.lrc":  "\n\n\n[source:musixmatch]\n[00:01.00]x\n", // parses fine
		"late.lrc":    "[00:01.00]x\n[source:musixmatch]\n",       // tag after lyrics -> not a header
		"binary.lrc":  "\x00\x01\x02\xff\xfe",
	}
	for name, body := range cases {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	preview := func(f Filter) map[string]bool {
		t.Helper()
		got := map[string]bool{}
		if _, err := New(sqlDB).Run(ctx, Options{
			Roots: []string{root}, LibraryID: &libID,
			Filter: f, DryRun: true,
			Report: func(r Record) error { got[filepath.Base(r.Path)] = true; return nil },
		}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		return got
	}

	bySource := preview(Filter{Source: "musixmatch"})
	for _, name := range []string{"crlf.lrc", "blanks.lrc"} {
		if !bySource[name] {
			t.Errorf("--source musixmatch did not match %s; the header parser regressed", name)
		}
	}
	for _, name := range []string{"late.lrc", "binary.lrc"} {
		if bySource[name] {
			t.Errorf("--source musixmatch matched %s, which carries no readable header tag", name)
		}
	}

	noSrc := preview(Filter{NoSource: true})
	// Known defect, asserted so a parser fix shows up here as a failing
	// expectation to update rather than passing silently.
	if !noSrc["bom.lrc"] {
		t.Errorf("bom.lrc no longer falls into the --no-source cohort; the BOM misparse appears FIXED -- " +
			"delete this expectation and move bom.lrc to the --source assertions above")
	}
	if noSrc["crlf.lrc"] || noSrc["blanks.lrc"] {
		t.Errorf("--no-source matched a validly tagged sidecar: %v", noSrc)
	}
}

// countWQ returns the total number of work_queue rows.
func countWQ(t *testing.T, ctx context.Context, sqlDB *sql.DB) int {
	t.Helper()
	var n int
	if err := sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_queue`).Scan(&n); err != nil {
		t.Fatalf("count work_queue: %v", err)
	}
	return n
}

// TestRun_InvalidatesEveryLinkedIdentity: one output path can be claimed by more
// than one scan_results row (two audio formats of the same track landing on the
// same sidecar), and those rows can carry DIFFERENT artist/title tags. Each row
// is a separate cache key and each is separately reset to pending, so each must
// be separately invalidated -- invalidating only the first leaves the second row
// to be satisfied from cache on the next scan, with no sidecar on disk.
func TestRun_InvalidatesEveryLinkedIdentity(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dir := filepath.Join(root, "A")
	path := filepath.Join(dir, "song.lrc")
	writeSidecar(t, path, "musixmatch")
	seedTrack(t, ctx, sqlDB, libID, dir, "song.lrc", "done")

	// A second scan_results row for the same output path under a different
	// identity (e.g. the FLAC and the MP3 carry divergent tags).
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, outdir, filename, status)
         VALUES (?, ?, 'Alt Artist', 'Alt Title', ?, 'song.lrc', 'done')`,
		libID, filepath.Join(dir, "song.flac"), dir); err != nil {
		t.Fatalf("insert second scan_result: %v", err)
	}

	c := cache.New(sqlDB)
	if err := c.Store(ctx, "Artist", "Title", 0, "[00:01.00]a\n"); err != nil {
		t.Fatalf("cache store first identity: %v", err)
	}
	if err := c.Store(ctx, "Alt Artist", "Alt Title", 0, "[00:01.00]b\n"); err != nil {
		t.Fatalf("cache store second identity: %v", err)
	}

	res, err := New(sqlDB).Run(ctx, Options{
		Roots: []string{root}, LibraryID: &libID,
		Filter: Filter{Source: "musixmatch"}, DryRun: false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.CacheInvalidated != 2 {
		t.Errorf("CacheInvalidated = %d, want 2 (one per linked identity)", res.CacheInvalidated)
	}
	for _, id := range [][2]string{{"Artist", "Title"}, {"Alt Artist", "Alt Title"}} {
		if _, lerr := c.Lookup(ctx, id[0], id[1], 0); !errors.Is(lerr, sql.ErrNoRows) {
			t.Errorf("cache entry for a reset row survived the purge (%q/%q): err = %v", id[0], id[1], lerr)
		}
	}

	enq, hits, err := newEnqueuer(sqlDB).EnqueuePending(ctx, models.Library{ID: libID, Path: root})
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if hits != 0 {
		t.Errorf("a reset row was satisfied from cache (hits=%d); its sidecar is gone and would never be rewritten", hits)
	}
	if enq == 0 {
		t.Errorf("enqueued = 0; the purged track must be queued for re-fetch")
	}
}

// TestRun_InvalidationFailureAbortsTheDelete pins the failure semantics of the
// invalidation half. A purge that deletes the file but fails to invalidate is
// strictly worse than doing nothing: the user loses the sidecar AND the next
// scan is still served from cache, so nothing rewrites it, while the summary
// claims a requeue. The invalidation therefore runs (and commits) BEFORE the
// unlink, and its failure aborts that sidecar entirely -- file intact, rows
// untouched, error counted.
//
// The failure is induced by removing the lyrics_cache table out from under the
// run, which is the only way to make a plain DELETE fail against a live SQLite
// handle without mocking the seam away.
func TestRun_InvalidationFailureAbortsTheDelete(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	dir := filepath.Join(root, "A")
	path := filepath.Join(dir, "song.lrc")
	writeSidecar(t, path, "musixmatch")
	srID, wqID := seedTrack(t, ctx, sqlDB, libID, dir, "song.lrc", "done")

	if _, err := sqlDB.ExecContext(ctx, `DROP TABLE lyrics_cache`); err != nil {
		t.Fatalf("drop lyrics_cache: %v", err)
	}

	res, err := New(sqlDB).Run(ctx, Options{
		Roots: []string{root}, LibraryID: &libID,
		Filter: Filter{Source: "musixmatch"}, DryRun: false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Errors != 1 {
		t.Errorf("Errors = %d, want 1 (the invalidation failure must be surfaced, not swallowed)", res.Errors)
	}
	if res.Deleted != 0 {
		t.Errorf("Deleted = %d, want 0: the sidecar must not be unlinked when its cache entry could not be invalidated", res.Deleted)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Errorf("sidecar deleted despite a failed invalidation: %v", serr)
	}
	if got := rowStatus(t, ctx, sqlDB, "scan_results", srID); got != "done" {
		t.Errorf("scan_results reset committed despite a failed invalidation; status = %q", got)
	}
	if got := rowStatus(t, ctx, sqlDB, "work_queue", wqID); got != "done" {
		t.Errorf("work_queue reset committed despite a failed invalidation; status = %q", got)
	}
}
