package identityrepair

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sydlexius/canticle/internal/normalize"
)

// setScanIdentity directly rewrites a scan_results row's artist identity,
// standing in for what a real scan's baseUpsert does on conflict -- issue
// #963's premise is that scan_results can already be corrected while
// work_queue has not followed. album_artist is left untouched; the
// album-artist-only sync path is covered by TestRun_AlbumArtistOnly and its
// divergence-side equivalent does not need a second fixture here.
func setScanIdentity(t *testing.T, db *sql.DB, id int64, artist string) {
	t.Helper()
	if _, err := db.Exec(
		`UPDATE scan_results SET artist = ?, artist_key = ? WHERE id = ?`,
		artist, normalize.NormalizeKey(artist), id); err != nil {
		t.Fatalf("set scan identity %d: %v", id, err)
	}
}

func scanStatus(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var status string
	if err := db.QueryRow(`SELECT status FROM scan_results WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("read scan status %d: %v", id, err)
	}
	return status
}

func junctionLinked(t *testing.T, db *sql.DB, wqID, scanID int64) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM work_queue_scan_results WHERE work_queue_id = ? AND scan_result_id = ?`, wqID, scanID).Scan(&n); err != nil {
		t.Fatalf("count junction: %v", err)
	}
	return n > 0
}

func scalarLink(t *testing.T, db *sql.DB, wqID int64) sql.NullInt64 {
	t.Helper()
	var v sql.NullInt64
	if err := db.QueryRow(`SELECT scan_result_id FROM work_queue WHERE id = ?`, wqID).Scan(&v); err != nil {
		t.Fatalf("read scalar link %d: %v", wqID, err)
	}
	return v
}

// Reproduces #963's steps directly: scan_results is already corrected (as a
// real scan's baseUpsert would leave it) but the coupled work_queue row never
// followed. The old tag re-read Run pins "no change" (scan_results already
// agrees with the file), and RepairDivergence -- the new DB-only pass -- finds
// and re-keys it.
func TestRun_DoesNotSeeAlreadyCorrectedScanResults(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	wq := seedQueue(t, db, "AlphaBravo", "", "pending", sr)

	// A prior scan already corrected scan_results; work_queue is stale.
	setScanIdentity(t, db, sr, "Alpha; Bravo")

	// The tag re-read pass: the file itself already reads "Alpha; Bravo", which
	// now MATCHES scan_results, so Run reports no change (pinning the bug).
	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}}
	res, err := New(db, reader.read).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Changed != 0 {
		t.Fatalf("Run.Changed = %d; want 0 (scan_results already correct, Run cannot see the divergence)", res.Changed)
	}
	if a, _, status := queueIdentity(t, db, wq); a != "AlphaBravo" || status != "pending" {
		t.Fatalf("work_queue = (%q, status=%q); want stale AlphaBravo still stuck", a, status)
	}

	// RepairDivergence (#963) finds and fixes it from the database alone.
	divRes, err := New(db, reader.read).RepairDivergence(context.Background(), Options{})
	if err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if divRes.Rekeyed != 1 {
		t.Fatalf("DivergenceResult = %+v; want Rekeyed=1", divRes)
	}
	wantKey := normalize.NormalizeKey("Alpha; Bravo")
	if a, k, status := queueIdentity(t, db, wq); a != "Alpha; Bravo" || k != wantKey || status != "pending" {
		t.Errorf("work_queue = (%q,%q,%q); want (Alpha; Bravo,%q,pending)", a, k, status, wantKey)
	}
}

// A 'done' work_queue row re-keyed by the divergence pass is reopened to
// 'pending' with settle state cleared -- the #960 constraint applies here too.
func TestRepairDivergence_RekeyReopensDoneRow(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	wq := seedQueue(t, db, "AlphaBravo", "", "done", sr)
	stampSettleState(t, db, wq)
	setScanIdentity(t, db, sr, "Alpha; Bravo")

	res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{})
	if err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if res.Rekeyed != 1 {
		t.Fatalf("Result = %+v; want Rekeyed=1", res)
	}
	s := readSettleState(t, db, wq)
	if s.status != "pending" {
		t.Fatalf("status = %q; want pending (reopened)", s.status)
	}
	if s.outcomeType.Valid || s.providerLane.Valid || s.timingOutcome.Valid || s.completedAt.Valid {
		t.Errorf("settle state not cleared on reopen: %+v", s)
	}
}

// An 'unavailable' work_queue row's identity is corrected by the divergence
// pass but the row itself is never reopened -- RecheckRetired is the only
// designed revival path (#477/#960).
func TestRepairDivergence_UnavailableRowStaysRetired(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	wq := seedQueue(t, db, "AlphaBravo", "", "unavailable", sr)
	if _, err := db.Exec(`UPDATE work_queue SET last_error = 'miss limit reached', miss_count = 15 WHERE id = ?`, wq); err != nil {
		t.Fatalf("stamp sentinel: %v", err)
	}
	setScanIdentity(t, db, sr, "Alpha; Bravo")

	res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{})
	if err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if res.Rekeyed != 1 {
		t.Fatalf("Result = %+v; want Rekeyed=1", res)
	}
	a, k, status := queueIdentity(t, db, wq)
	wantKey := normalize.NormalizeKey("Alpha; Bravo")
	if a != "Alpha; Bravo" || k != wantKey {
		t.Errorf("identity = (%q,%q); want corrected to (Alpha; Bravo,%q)", a, k, wantKey)
	}
	if status != "unavailable" {
		t.Errorf("status = %q; want unavailable (never reopened)", status)
	}
}

// A 'processing' work_queue row is left entirely alone: the divergence
// persists and is retried on the next pass rather than disturbing in-flight
// work.
func TestRepairDivergence_ProcessingRowSkipped(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	wq := seedQueue(t, db, "AlphaBravo", "", "processing", sr)
	setScanIdentity(t, db, sr, "Alpha; Bravo")

	res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{})
	if err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if res.ProcessingSkips != 1 || res.Rekeyed != 0 {
		t.Fatalf("Result = %+v; want ProcessingSkips=1 Rekeyed=0", res)
	}
	if a, _, status := queueIdentity(t, db, wq); a != "AlphaBravo" || status != "processing" {
		t.Errorf("processing row mutated: (%q, %q)", a, status)
	}
}

// A key collision at the corrected identity merges the stale row into the
// existing one, exactly like Run's apply for a single scan_result.
func TestRepairDivergence_ConflictMerges(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srBad := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srGood := seedScan(t, db, lib, "/m/2.mp3", "Alpha; Bravo", "", "Song")
	wqBad := seedQueue(t, db, "AlphaBravo", "", "pending", srBad)
	wqGood := seedQueue(t, db, "Alpha; Bravo", "", "done", srGood)
	setScanIdentity(t, db, srBad, "Alpha; Bravo")

	res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{})
	if err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if res.Merged != 1 {
		t.Fatalf("Result = %+v; want Merged=1", res)
	}
	if n := queueCount(t, db); n != 1 {
		t.Fatalf("work_queue count = %d; want 1", n)
	}
	if err := db.QueryRow(`SELECT 1 FROM work_queue WHERE id = ?`, wqBad).Scan(new(int)); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("bad work_queue row still present; want deleted (err=%v)", err)
	}
	if _, _, status := queueIdentity(t, db, wqGood); status != "pending" {
		t.Errorf("survivor status = %q; want pending (reopened by merge)", status)
	}
}

// Shared queue row, DISAGREEING siblings: one linked scan_results row's
// identity was corrected, another still matches the queue row's stored key.
// The queue row must NOT be re-keyed (that would orphan the still-correct
// sibling) -- only the divergent sibling is unlinked and reset to pending so
// the next scan's enqueue creates it its own queue row at its own key.
func TestRepairDivergence_SharedRowDisagreementUnlinksOnlyDivergent(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srA := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srB := seedScan(t, db, lib, "/m/2.mp3", "AlphaBravo", "", "Song")
	// Force both onto ONE shared queue row (same key/title), as a dedup would.
	if _, err := db.Exec(`UPDATE scan_results SET artist_key = 'alphabravo' WHERE id IN (?, ?)`, srA, srB); err != nil {
		t.Fatalf("force shared key: %v", err)
	}
	wq := seedQueue(t, db, "AlphaBravo", "", "pending", srA)
	if _, err := db.Exec(`UPDATE work_queue SET artist_key = 'alphabravo' WHERE id = ?`, wq); err != nil {
		t.Fatalf("force queue key: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wq, srB); err != nil {
		t.Fatalf("link srB: %v", err)
	}

	// Only srA's identity gets corrected by a later scan; srB still matches wq.
	if _, err := db.Exec(`UPDATE scan_results SET artist = 'Alpha; Bravo', artist_key = ? WHERE id = ?`,
		normalize.NormalizeKey("Alpha; Bravo"), srA); err != nil {
		t.Fatalf("correct srA: %v", err)
	}

	res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{})
	if err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if res.Unlinked != 1 || res.Rekeyed != 0 || res.Merged != 0 {
		t.Fatalf("Result = %+v; want Unlinked=1 Rekeyed=0 Merged=0 (disagreement must not re-key)", res)
	}
	// wq itself is untouched: still the OLD identity, srB stays linked.
	if a, k, _ := queueIdentity(t, db, wq); a != "AlphaBravo" || k != "alphabravo" {
		t.Errorf("shared work_queue row mutated: (%q,%q); want left at the old identity", a, k)
	}
	if !junctionLinked(t, db, wq, srB) {
		t.Errorf("srB unlinked; want it to remain linked (still matches wq's key)")
	}
	if junctionLinked(t, db, wq, srA) {
		t.Errorf("srA still linked; want the divergent member unlinked")
	}
	if status := scanStatus(t, db, srA); status != "pending" {
		t.Errorf("srA status = %q; want pending (reset so it re-enqueues at its own corrected key)", status)
	}
	if v := scalarLink(t, db, wq); v.Valid && v.Int64 == srA {
		t.Errorf("scalar work_queue.scan_result_id still points at unlinked srA")
	}
}

// Shared queue row, BOTH members diverge to DIFFERENT new keys, neither
// matching the queue row's own stored key -- a disagreement (len(byKey) > 1),
// so the queue row is never re-keyed. Once both members are unlinked, nothing
// linked remains that could still be served under the stale identity, so the
// orphaned work_queue row itself must be deleted rather than left behind to
// fetch and write lyrics under a key nothing agrees with any more.
func TestRepairDivergence_DisagreementOrphansQueueRowDeletesIt(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srA := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srB := seedScan(t, db, lib, "/m/2.mp3", "AlphaBravo", "", "Song")
	// Force both onto ONE shared queue row (same key/title), as a dedup would.
	if _, err := db.Exec(`UPDATE scan_results SET artist_key = 'alphabravo' WHERE id IN (?, ?)`, srA, srB); err != nil {
		t.Fatalf("force shared key: %v", err)
	}
	wq := seedQueue(t, db, "AlphaBravo", "", "pending", srA)
	if _, err := db.Exec(`UPDATE work_queue SET artist_key = 'alphabravo' WHERE id = ?`, wq); err != nil {
		t.Fatalf("force queue key: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wq, srB); err != nil {
		t.Fatalf("link srB: %v", err)
	}

	// Both members are corrected, but to TWO DIFFERENT keys -- neither matches
	// wq's stored key, and they don't agree with each other either.
	if _, err := db.Exec(`UPDATE scan_results SET artist = 'Alpha; Bravo', artist_key = ? WHERE id = ?`,
		normalize.NormalizeKey("Alpha; Bravo"), srA); err != nil {
		t.Fatalf("correct srA: %v", err)
	}
	if _, err := db.Exec(`UPDATE scan_results SET artist = 'Charlie; Delta', artist_key = ? WHERE id = ?`,
		normalize.NormalizeKey("Charlie; Delta"), srB); err != nil {
		t.Fatalf("correct srB: %v", err)
	}

	res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{})
	if err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if res.Unlinked != 2 || res.Deleted != 1 || res.Rekeyed != 0 || res.Merged != 0 {
		t.Fatalf("Result = %+v; want Unlinked=2 Deleted=1 Rekeyed=0 Merged=0", res)
	}
	if err := db.QueryRow(`SELECT 1 FROM work_queue WHERE id = ?`, wq).Scan(new(int)); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("orphaned work_queue row still present; want deleted (err=%v)", err)
	}
	if status := scanStatus(t, db, srA); status != "pending" {
		t.Errorf("srA status = %q; want pending", status)
	}
	if status := scanStatus(t, db, srB); status != "pending" {
		t.Errorf("srB status = %q; want pending", status)
	}
}

// The dry-run form of the same scenario: nothing is deleted, but Deleted is
// still reported so a preview reflects what --yes would do.
func TestRepairDivergence_DisagreementOrphansQueueRowDryRun(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srA := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srB := seedScan(t, db, lib, "/m/2.mp3", "AlphaBravo", "", "Song")
	if _, err := db.Exec(`UPDATE scan_results SET artist_key = 'alphabravo' WHERE id IN (?, ?)`, srA, srB); err != nil {
		t.Fatalf("force shared key: %v", err)
	}
	wq := seedQueue(t, db, "AlphaBravo", "", "pending", srA)
	if _, err := db.Exec(`UPDATE work_queue SET artist_key = 'alphabravo' WHERE id = ?`, wq); err != nil {
		t.Fatalf("force queue key: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wq, srB); err != nil {
		t.Fatalf("link srB: %v", err)
	}
	if _, err := db.Exec(`UPDATE scan_results SET artist = 'Alpha; Bravo', artist_key = ? WHERE id = ?`,
		normalize.NormalizeKey("Alpha; Bravo"), srA); err != nil {
		t.Fatalf("correct srA: %v", err)
	}
	if _, err := db.Exec(`UPDATE scan_results SET artist = 'Charlie; Delta', artist_key = ? WHERE id = ?`,
		normalize.NormalizeKey("Charlie; Delta"), srB); err != nil {
		t.Fatalf("correct srB: %v", err)
	}

	res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{DryRun: true})
	if err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if res.Unlinked != 2 || res.Deleted != 1 {
		t.Fatalf("Result = %+v; want Unlinked=2 Deleted=1 (dry-run still reports the count)", res)
	}
	if err := db.QueryRow(`SELECT 1 FROM work_queue WHERE id = ?`, wq).Scan(new(int)); err != nil {
		t.Errorf("dry run deleted work_queue row (err=%v); want left in place", err)
	}
	if !junctionLinked(t, db, wq, srA) || !junctionLinked(t, db, wq, srB) {
		t.Errorf("dry run unlinked a junction row; want both left in place")
	}
}

// A queue row can also carry a junction link whose scan_results title_key does
// not match its own (prune's identity relink does not check title_key). That
// link is outside the divergence group, but it still keeps the queue row alive
// on apply, so a dry run must not predict a delete that --yes will not perform.
func TestRepairDivergence_DryRunDeleteMatchesApplyWithMismatchedTitleLink(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srA := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srB := seedScan(t, db, lib, "/m/2.mp3", "AlphaBravo", "", "Song")
	srOther := seedScan(t, db, lib, "/m/3.mp3", "AlphaBravo", "", "Other")
	if _, err := db.Exec(`UPDATE scan_results SET artist_key = 'alphabravo' WHERE id IN (?, ?)`, srA, srB); err != nil {
		t.Fatalf("force shared key: %v", err)
	}
	wq := seedQueue(t, db, "AlphaBravo", "", "pending", srA)
	if _, err := db.Exec(`UPDATE work_queue SET artist_key = 'alphabravo' WHERE id = ?`, wq); err != nil {
		t.Fatalf("force queue key: %v", err)
	}
	for _, sr := range []int64{srB, srOther} {
		if _, err := db.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wq, sr); err != nil {
			t.Fatalf("link %d: %v", sr, err)
		}
	}
	if _, err := db.Exec(`UPDATE scan_results SET artist = 'Alpha; Bravo', artist_key = ? WHERE id = ?`,
		normalize.NormalizeKey("Alpha; Bravo"), srA); err != nil {
		t.Fatalf("correct srA: %v", err)
	}
	if _, err := db.Exec(`UPDATE scan_results SET artist = 'Charlie; Delta', artist_key = ? WHERE id = ?`,
		normalize.NormalizeKey("Charlie; Delta"), srB); err != nil {
		t.Fatalf("correct srB: %v", err)
	}

	r := New(db, fakeReader{}.read)
	dry, err := r.RepairDivergence(context.Background(), Options{DryRun: true})
	if err != nil {
		t.Fatalf("dry-run RepairDivergence: %v", err)
	}
	applied, err := r.RepairDivergence(context.Background(), Options{})
	if err != nil {
		t.Fatalf("apply RepairDivergence: %v", err)
	}
	if applied.Deleted != 0 {
		t.Fatalf("apply Result = %+v; want Deleted=0 (the mismatched-title link keeps the row)", applied)
	}
	if dry.Deleted != applied.Deleted {
		t.Errorf("dry-run Deleted = %d, apply Deleted = %d; the preview must predict what --yes does", dry.Deleted, applied.Deleted)
	}
	if err := db.QueryRow(`SELECT 1 FROM work_queue WHERE id = ?`, wq).Scan(new(int)); err != nil {
		t.Errorf("work_queue row gone (err=%v); want kept by its remaining link", err)
	}
}

// A DryRun computes and reports the same decision without writing anything.
func TestRepairDivergence_DryRunWritesNothing(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	wq := seedQueue(t, db, "AlphaBravo", "", "pending", sr)
	setScanIdentity(t, db, sr, "Alpha; Bravo")

	var reported []Change
	res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{
		DryRun: true,
		Report: func(c Change) error { reported = append(reported, c); return nil },
	})
	if err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if res.Rekeyed != 1 {
		t.Fatalf("Result = %+v; want Rekeyed=1", res)
	}
	if len(reported) != 1 || reported[0].NewArtist != "Alpha; Bravo" {
		t.Fatalf("reported = %+v; want one change to Alpha; Bravo", reported)
	}
	if a, _, status := queueIdentity(t, db, wq); a != "AlphaBravo" || status != "pending" {
		t.Errorf("dry run mutated work_queue: (%q, %q)", a, status)
	}
}

// PathPrefix scopes the divergence pass to one subtree (#963 follow-up
// finding 3): a divergent row outside the rescanned subtree is excluded, and
// one inside it is still found and repaired.
func TestRepairDivergence_PathPrefixScope(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srIn := seedScan(t, db, lib, "/music/artistA/1.mp3", "AlphaBravo", "", "Song")
	srOut := seedScan(t, db, lib, "/music/artistB/2.mp3", "CharlieDelta", "", "Song")
	wqIn := seedQueue(t, db, "AlphaBravo", "", "pending", srIn)
	wqOut := seedQueue(t, db, "CharlieDelta", "", "pending", srOut)
	setScanIdentity(t, db, srIn, "Alpha; Bravo")
	setScanIdentity(t, db, srOut, "Charlie; Delta")

	res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{
		PathPrefix: "/music/artistA",
	})
	if err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if res.Scanned != 1 || res.Rekeyed != 1 {
		t.Fatalf("Result = %+v; want Scanned=1 Rekeyed=1 (only the in-subtree row)", res)
	}
	if a, _, _ := queueIdentity(t, db, wqIn); a != "Alpha; Bravo" {
		t.Errorf("in-subtree queue row not corrected: %q", a)
	}
	if a, _, _ := queueIdentity(t, db, wqOut); a != "CharlieDelta" {
		t.Errorf("out-of-subtree queue row was touched: %q; want left at CharlieDelta", a)
	}
}

// A sibling directory sharing a name PREFIX (not a path-separator boundary)
// must not be matched: "/a/b" scoping must not pull in "/a/bc/...".
func TestRepairDivergence_PathPrefixScopeDoesNotMatchSiblingPrefix(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srSibling := seedScan(t, db, lib, "/a/bc/2.mp3", "CharlieDelta", "", "Song")
	wqSibling := seedQueue(t, db, "CharlieDelta", "", "pending", srSibling)
	setScanIdentity(t, db, srSibling, "Charlie; Delta")

	res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{
		PathPrefix: "/a/b",
	})
	if err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if res.Scanned != 0 {
		t.Fatalf("Result = %+v; want Scanned=0 (sibling dir /a/bc must not match scope /a/b)", res)
	}
	if a, _, _ := queueIdentity(t, db, wqSibling); a != "CharlieDelta" {
		t.Errorf("sibling-dir queue row was touched: %q; want left at CharlieDelta", a)
	}
}

// queueAlbumArtist returns a work_queue row's stored album_artist.
func queueAlbumArtist(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var aa string
	if err := db.QueryRow(`SELECT album_artist FROM work_queue WHERE id = ?`, id).Scan(&aa); err != nil {
		t.Fatalf("read album_artist %d: %v", id, err)
	}
	return aa
}

// A shared queue row (#967): two scan_results members with the SAME
// artist_key but different album_artist -- e.g. the same song on two
// different releases. The queue row can only hold one album_artist, and
// neither member is "wrong", so this is not a divergence at all: it must not
// be selected as a candidate, on a dry run or on apply, and a repeated pass
// must still find nothing (the pre-#967 query re-selected such a row on
// every run because it also compared album_artist).
func TestRepairDivergence_SharedRowAlbumArtistOnlyIsNoOp(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srA := seedScan(t, db, lib, "/m/1.mp3", "Alpha", "Release One", "Song")
	srB := seedScan(t, db, lib, "/m/2.mp3", "Alpha", "Release Two", "Song")
	wq := seedQueue(t, db, "Alpha", "Release One", "pending", srA)
	if _, err := db.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wq, srB); err != nil {
		t.Fatalf("link srB: %v", err)
	}

	res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{})
	if err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if res.Scanned != 0 {
		t.Fatalf("Result = %+v; want Scanned=0 (an album_artist-only difference on a shared row is not a divergence)", res)
	}
	if a, _, _ := queueIdentity(t, db, wq); a != "Alpha" {
		t.Errorf("queue row artist mutated: %q; want left at Alpha", a)
	}
	if aa := queueAlbumArtist(t, db, wq); aa != "Release One" {
		t.Errorf("queue row album_artist mutated: %q; want left at Release One", aa)
	}

	// Dry run agrees.
	dry, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{DryRun: true})
	if err != nil {
		t.Fatalf("dry-run RepairDivergence: %v", err)
	}
	if dry.Scanned != 0 {
		t.Fatalf("dry-run Result = %+v; want Scanned=0", dry)
	}

	// A second pass is still a no-op -- the pre-#967 bug re-selected such a row
	// on every run.
	res2, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{})
	if err != nil {
		t.Fatalf("second RepairDivergence: %v", err)
	}
	if res2.Scanned != 0 {
		t.Fatalf("second pass Result = %+v; want Scanned=0", res2)
	}
}

// A shared queue row where the divergent-looking difference is only the
// artist DISPLAY string's case (artist_key -- the lookup key -- already
// agrees): also not a divergence. Distinct from the album_artist case above,
// this exercises the sr.artist != wq.artist leg of the pre-#967 query.
func TestRepairDivergence_ArtistDisplayCaseOnlyIsNoOp(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "alpha", "", "Song")
	wq := seedQueue(t, db, "Alpha", "", "pending", sr)

	res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{})
	if err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if res.Scanned != 0 {
		t.Fatalf("Result = %+v; want Scanned=0 (artist_key agrees; a display-only case difference is not a divergence)", res)
	}
	if a, _, _ := queueIdentity(t, db, wq); a != "Alpha" {
		t.Errorf("queue row artist mutated: %q; want left at Alpha", a)
	}
}

// LibraryID scopes the divergence pass to one library.
func TestRepairDivergence_LibraryScope(t *testing.T) {
	db := openDB(t)
	lib1 := seedLibrary(t, db)
	lib2 := seedLibrary(t, db)
	sr1 := seedScan(t, db, lib1, "/m/1.mp3", "AlphaBravo", "", "Song")
	sr2 := seedScan(t, db, lib2, "/m/2.mp3", "CharlieDelta", "", "Song")
	wq1 := seedQueue(t, db, "AlphaBravo", "", "pending", sr1)
	seedQueue(t, db, "CharlieDelta", "", "pending", sr2)
	setScanIdentity(t, db, sr1, "Alpha; Bravo")
	setScanIdentity(t, db, sr2, "Charlie; Delta")

	res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{LibraryID: &lib1})
	if err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if res.Scanned != 1 || res.Rekeyed != 1 {
		t.Fatalf("Result = %+v; want Scanned=1 Rekeyed=1 (lib1 only)", res)
	}
	if a, _, _ := queueIdentity(t, db, wq1); a != "Alpha; Bravo" {
		t.Errorf("lib1 queue row not corrected: %q", a)
	}
}

// Shared queue row (#967 finding 1): srA is IN the scoped library and
// diverges to one key, srB is in a DIFFERENT library and diverges to a
// DIFFERENT key. An unscoped pass sees both as a disagreement (neither
// matches wq's own key, and they disagree with each other), unlinks both, and
// deletes the now-orphaned queue row. A pass scoped to srA's library must not
// do any of that on srB's behalf: srB was never examined by this run, its
// library gets neither credit nor a backup record, and unlinking it (or
// deleting the shared queue row) would silently corrupt state for a library
// this run was not asked to touch. The whole candidate row is skipped instead
// (ScopeSkips), leaving every column and every link exactly as found.
func TestRepairDivergence_LibraryScopeSkipsRowWithOutOfScopeDivergentMember(t *testing.T) {
	db := openDB(t)
	lib1 := seedLibrary(t, db)
	lib2 := seedLibrary(t, db)
	srA := seedScan(t, db, lib1, "/m1/1.mp3", "AlphaBravo", "", "Song")
	srB := seedScan(t, db, lib2, "/m2/2.mp3", "AlphaBravo", "", "Song")
	// Force both onto ONE shared queue row (same key/title), as a dedup would.
	if _, err := db.Exec(`UPDATE scan_results SET artist_key = 'alphabravo' WHERE id IN (?, ?)`, srA, srB); err != nil {
		t.Fatalf("force shared key: %v", err)
	}
	wq := seedQueue(t, db, "AlphaBravo", "", "pending", srA)
	if _, err := db.Exec(`UPDATE work_queue SET artist_key = 'alphabravo' WHERE id = ?`, wq); err != nil {
		t.Fatalf("force queue key: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wq, srB); err != nil {
		t.Fatalf("link srB: %v", err)
	}

	// srA (in-scope) and srB (out-of-scope) diverge to DIFFERENT keys -- a
	// disagreement, and srB's key differs from wq's own.
	if _, err := db.Exec(`UPDATE scan_results SET artist = 'Alpha; Bravo', artist_key = ? WHERE id = ?`,
		normalize.NormalizeKey("Alpha; Bravo"), srA); err != nil {
		t.Fatalf("correct srA: %v", err)
	}
	if _, err := db.Exec(`UPDATE scan_results SET artist = 'Charlie; Delta', artist_key = ? WHERE id = ?`,
		normalize.NormalizeKey("Charlie; Delta"), srB); err != nil {
		t.Fatalf("correct srB: %v", err)
	}

	res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{LibraryID: &lib1})
	if err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if res.ScopeSkips != 1 || res.Rekeyed != 0 || res.Merged != 0 || res.Unlinked != 0 || res.Deleted != 0 {
		t.Fatalf("Result = %+v; want ScopeSkips=1 and every other write-tally 0", res)
	}
	if a, k, status := queueIdentity(t, db, wq); a != "AlphaBravo" || k != "alphabravo" || status != "pending" {
		t.Errorf("shared work_queue row mutated: (%q,%q,%q); want left at the old identity", a, k, status)
	}
	if !junctionLinked(t, db, wq, srA) || !junctionLinked(t, db, wq, srB) {
		t.Errorf("a junction link was dropped; want both left in place (scope-skipped)")
	}
	if status := scanStatus(t, db, srA); status != "pending" {
		t.Errorf("srA status = %q; want its pre-existing pending status untouched", status)
	}
	if status := scanStatus(t, db, srB); status != "pending" {
		t.Errorf("srB (out-of-scope) status = %q; want untouched", status)
	}

	// An unscoped pass over the same fixture repairs it normally: disagreement,
	// both unlinked, row deleted once nothing correct remains linked.
	all, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{})
	if err != nil {
		t.Fatalf("unscoped RepairDivergence: %v", err)
	}
	if all.Unlinked != 2 || all.Deleted != 1 || all.ScopeSkips != 0 {
		t.Fatalf("unscoped Result = %+v; want Unlinked=2 Deleted=1 ScopeSkips=0", all)
	}
}

// Same shape as the library-scope test above, but scoped by PathPrefix
// instead of LibraryID: srA sits under the rescanned subtree, srB sits under
// an entirely different subtree of the same library. The PathPrefix-scoped
// pass must skip the shared row rather than act on srB's behalf.
func TestRepairDivergence_PathPrefixScopeSkipsRowWithOutOfScopeDivergentMember(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srA := seedScan(t, db, lib, "/music/artistA/1.mp3", "AlphaBravo", "", "Song")
	srB := seedScan(t, db, lib, "/music/artistB/2.mp3", "AlphaBravo", "", "Song")
	if _, err := db.Exec(`UPDATE scan_results SET artist_key = 'alphabravo' WHERE id IN (?, ?)`, srA, srB); err != nil {
		t.Fatalf("force shared key: %v", err)
	}
	wq := seedQueue(t, db, "AlphaBravo", "", "pending", srA)
	if _, err := db.Exec(`UPDATE work_queue SET artist_key = 'alphabravo' WHERE id = ?`, wq); err != nil {
		t.Fatalf("force queue key: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wq, srB); err != nil {
		t.Fatalf("link srB: %v", err)
	}
	if _, err := db.Exec(`UPDATE scan_results SET artist = 'Alpha; Bravo', artist_key = ? WHERE id = ?`,
		normalize.NormalizeKey("Alpha; Bravo"), srA); err != nil {
		t.Fatalf("correct srA: %v", err)
	}
	if _, err := db.Exec(`UPDATE scan_results SET artist = 'Charlie; Delta', artist_key = ? WHERE id = ?`,
		normalize.NormalizeKey("Charlie; Delta"), srB); err != nil {
		t.Fatalf("correct srB: %v", err)
	}

	res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{
		PathPrefix: "/music/artistA",
	})
	if err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if res.ScopeSkips != 1 || res.Rekeyed != 0 || res.Merged != 0 || res.Unlinked != 0 || res.Deleted != 0 {
		t.Fatalf("Result = %+v; want ScopeSkips=1 and every other write-tally 0", res)
	}
	if a, k, status := queueIdentity(t, db, wq); a != "AlphaBravo" || k != "alphabravo" || status != "pending" {
		t.Errorf("shared work_queue row mutated: (%q,%q,%q); want left at the old identity", a, k, status)
	}
	if !junctionLinked(t, db, wq, srA) || !junctionLinked(t, db, wq, srB) {
		t.Errorf("a junction link was dropped; want both left in place (scope-skipped)")
	}
}

// #970 finding 2: PathPrefix "/" (a filesystem root) must include a normal
// descendant like "/lib/a.mp3" -- the naive range-seek form (always appending
// a fresh separator to the prefix) doubles up on a prefix that already ends
// in the separator and excludes every ordinary child.
func TestRepairDivergence_PathPrefixRootIncludesDescendants(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/lib/a.mp3", "AlphaBravo", "", "Song")
	wq := seedQueue(t, db, "AlphaBravo", "", "pending", sr)
	setScanIdentity(t, db, sr, "Alpha; Bravo")

	res, err := New(db, fakeReader{}.read).RepairDivergence(context.Background(), Options{
		PathPrefix: "/",
	})
	if err != nil {
		t.Fatalf("RepairDivergence: %v", err)
	}
	if res.Scanned != 1 || res.Rekeyed != 1 {
		t.Fatalf("Result = %+v; want Scanned=1 Rekeyed=1 (root prefix must include /lib/a.mp3)", res)
	}
	if a, _, _ := queueIdentity(t, db, wq); a != "Alpha; Bravo" {
		t.Errorf("root-scoped queue row not corrected: %q", a)
	}
}
