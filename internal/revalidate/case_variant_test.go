package revalidate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/realign"
)

// caseSensitiveFS is defined in revalidate_test.go and reused here.

// TestJudgeCandidateResolvesUppercaseLRC is the revalidate half of #1051: a
// track whose sidecar is spelled "track.LRC" must be judged, not read as
// no_sidecar, or the row never leaves the timing backlog on a case-sensitive
// deployment.
func TestJudgeCandidateResolvesUppercaseLRC(t *testing.T) {
	root, lrc := lib(t, overrunBody)
	dir := filepath.Dir(lrc)
	if !caseSensitiveFS(t, dir) {
		t.Skip(`filesystem is case-insensitive; os.Lstat("track.lrc") already finds "track.LRC" without the fix`)
	}
	upper := filepath.Join(dir, "track.LRC")
	if err := os.Rename(lrc, upper); err != nil {
		t.Fatalf("rename to uppercase: %v", err)
	}

	r, _ := newRevalidator(t, root, fixedDuration(), func(o *Options) {
		o.MisSyncedAction = ActionOff
	})
	plan, err := r.PlanCandidates(context.Background(), []Candidate{candidateFor(1, root, lrc)})
	if err != nil {
		t.Fatalf("PlanCandidates: %v", err)
	}
	if len(plan.Findings) != 1 {
		t.Fatalf("len(Findings) = %d, want 1", len(plan.Findings))
	}
	if got := plan.Findings[0].Outcome; got != "mis_synced" {
		t.Errorf("Outcome = %q, want mis_synced -- an uppercase .LRC must be judged, not treated as absent", got)
	}
	if plan.Counts.NoSidecar != 0 {
		t.Errorf("NoSidecar = %d, want 0", plan.Counts.NoSidecar)
	}
	if plan.Findings[0].Path != upper {
		t.Errorf("Path = %q, want the REAL on-disk name %q", plan.Findings[0].Path, upper)
	}
}

// TestPlanCandidatesReadsNoDirectoryForAnUppercaseLRC pins the I/O shape of the
// #1051 fix: resolving a case-variant sidecar must stay a bounded set of Lstat
// calls, never a directory read, or a batch containing several such rows would
// wake a spun-down library array (#684/#685) exactly as
// TestPlanCandidatesReadsNoDirectoryForTheCommonCases already guards for the
// ordinary no-sidecar and sidecar-present cases.
//
// dirListingCache.Reads() only counts calls THROUGH the cache (its own list
// method), and resolveSidecarCaseVariant never goes through it -- it Lstats
// candidates directly, by design (see its own comment). So a Reads()==0
// assertion alone cannot tell "the resolver used bounded Lstats, as intended"
// apart from "the resolver silently called os.ReadDir directly, bypassing the
// cache entirely" -- both read zero on the cache's own counter. Earlier this
// test asserted only Reads()==0 against a normally-readable directory, which a
// mutant os.ReadDir(dir) call inserted into resolveSidecarCaseVariant would
// pass (ReadDir works fine there and is never routed through the cache), so it
// caught nothing. The directory is therefore made TRAVERSABLE BUT UNLISTABLE
// (0o311: execute+write, no read) before judging: os.Lstat on a named entry
// only needs execute (search) permission on its parent, so the bounded-Lstat
// path still succeeds and the verdict still resolves, while any os.ReadDir on
// the directory -- cached or not -- fails with EACCES and the file would be
// missed. Permissions are restored in Cleanup so t.TempDir's own removal can
// still walk the tree.
func TestPlanCandidatesReadsNoDirectoryForAnUppercaseLRC(t *testing.T) {
	root, lrc := lib(t, overrunBody)
	dir := filepath.Dir(lrc)
	if !caseSensitiveFS(t, dir) {
		t.Skip(`filesystem is case-insensitive; os.Lstat("track.lrc") already finds "track.LRC" without the fix`)
	}
	if err := os.Rename(lrc, filepath.Join(dir, "track.LRC")); err != nil {
		t.Fatalf("rename to uppercase: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("a root process can list a 0o311 directory, so the fixture cannot deny ReadDir")
	}
	if err := os.Chmod(dir, 0o311); err != nil {
		t.Fatalf("chmod dir traversable-but-unlistable: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })

	cache := newDirListingCache()
	r, _ := newRevalidator(t, root, fixedDuration(), func(o *Options) {
		o.MisSyncedAction = ActionOff
	})
	var plan Plan
	if err := r.judgeCandidate(context.Background(), candidateFor(1, root, lrc), &plan, cache, map[string]bool{}); err != nil {
		t.Fatalf("judgeCandidate: %v", err)
	}
	if got := cache.Reads(); got != 0 {
		t.Errorf("issued %d directory read(s) resolving an extension-case variant; a batch of these would wake the library array every cycle", got)
	}
	if len(plan.Findings) != 1 {
		t.Fatalf("len(Findings) = %d, want 1", len(plan.Findings))
	}
	if got := plan.Findings[0].Outcome; got != "mis_synced" {
		t.Errorf("Outcome = %q, want mis_synced -- resolving the variant must not depend on the directory being listable", got)
	}
}

// TestDemotionReusesAnExistingUppercaseTxt is the demotion-target half of
// #1051: when a MisSynced "track.lrc" is demoted and the directory already
// holds a real "track.TXT" (written under a case-variant path, or synced from
// a case-insensitive source), the demotion must land on that SAME file rather
// than creating a second, lowercase "track.txt" beside it.
func TestDemotionReusesAnExistingUppercaseTxt(t *testing.T) {
	root, lrc := lib(t, overrunBody)
	dir := filepath.Dir(lrc)
	if !caseSensitiveFS(t, dir) {
		t.Skip(`filesystem is case-insensitive; "track.txt" and "track.TXT" would collide as one file`)
	}
	upperTxt := filepath.Join(dir, "track.TXT")
	if err := os.WriteFile(upperTxt, []byte("settled words"), 0o600); err != nil {
		t.Fatalf("write pre-existing settled .TXT: %v", err)
	}

	r, _ := newRevalidator(t, root, fixedDuration(), func(o *Options) {
		o.MisSyncedAction = ActionDemote
	})
	plan, err := r.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Moves) != 1 {
		t.Fatalf("len(Moves) = %d, want 1", len(plan.Moves))
	}
	mv := plan.Moves[0]
	if mv.Kind != realign.KindDemote {
		t.Fatalf("Kind = %q, want %q", mv.Kind, realign.KindDemote)
	}
	if mv.TextPath != upperTxt {
		t.Errorf("TextPath = %q, want the REAL on-disk name %q -- demoting onto a new lowercase name would leave two unsynced sidecars", mv.TextPath, upperTxt)
	}

	applied := applyPlan(t, plan)
	if len(applied) != 1 {
		t.Fatalf("len(applied) = %d, want 1", len(applied))
	}

	// The settled "track.TXT" must be untouched (writeDemotedText's O_EXCL
	// create is a no-op against an existing file): settled content on disk wins.
	got, rerr := os.ReadFile(upperTxt)
	if rerr != nil {
		t.Fatalf("read %q after apply: %v", upperTxt, rerr)
	}
	if string(got) != "settled words" {
		t.Errorf("track.TXT body = %q, want unchanged %q (settled content must win over a demotion)", got, "settled words")
	}
	// No second, lowercase .txt was created beside it.
	if _, err := os.Stat(filepath.Join(dir, "track.txt")); !os.IsNotExist(err) {
		t.Error("a second, lowercase track.txt was created; want the existing track.TXT reused")
	}
	// The .lrc was quarantined (moved aside), never left on disk.
	if _, err := os.Stat(lrc); !os.IsNotExist(err) {
		t.Error("track.lrc still exists after demote+quarantine")
	}
}

// TestJudgeCandidateStemCaseNeverMatches is the C1 guard reprised for
// revalidate: "Track.LRC" (a differing STEM case) must never be treated as
// "track"'s sidecar, even though it is one Lstat away and would satisfy a
// naive case-insensitive lookup.
func TestJudgeCandidateStemCaseNeverMatches(t *testing.T) {
	root, lrc := lib(t, overrunBody)
	dir := filepath.Dir(lrc)
	if !caseSensitiveFS(t, dir) {
		t.Skip(`filesystem is case-insensitive; "Track.LRC" and "track.lrc" would collide as one file`)
	}
	// Remove the real sidecar and write one under a DIFFERING stem case
	// instead -- a foreign sidecar that happens to sit in the same directory.
	if err := os.Remove(lrc); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Track.LRC"), []byte(overrunBody), 0o600); err != nil {
		t.Fatalf("write foreign sidecar: %v", err)
	}

	r, _ := newRevalidator(t, root, fixedDuration(), func(o *Options) {
		o.MisSyncedAction = ActionOff
	})
	plan, err := r.PlanCandidates(context.Background(), []Candidate{candidateFor(1, root, lrc)})
	if err != nil {
		t.Fatalf("PlanCandidates: %v", err)
	}
	if len(plan.Findings) != 1 {
		t.Fatalf("len(Findings) = %d, want 1", len(plan.Findings))
	}
	if got := plan.Findings[0].Outcome; got != "no_sidecar" {
		t.Errorf("Outcome = %q, want no_sidecar -- \"Track.LRC\" must never be treated as \"track\"'s sidecar", got)
	}
}
