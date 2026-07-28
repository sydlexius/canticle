package scan_test

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/scan"
	"github.com/sydlexius/canticle/internal/timing"
)

// fakeTimingVerdicts stands in for the durable timing_outcome record (#440).
// Keyed by the same (artist, title) the enqueuer already has from a scan result.
type fakeTimingVerdicts struct {
	verdicts map[string]scan.TimingVerdict
	err      error
	lookups  int
}

func (f *fakeTimingVerdicts) LookupTiming(_ context.Context, artist, title string) (scan.TimingVerdict, bool, error) {
	f.lookups++
	if f.err != nil {
		return scan.TimingVerdict{}, false, f.err
	}
	v, ok := f.verdicts[artist+"\x00"+title]
	return v, ok, nil
}

// A Categorical verdict writes NO sidecar, so nothing on disk suppresses the
// next attempt: the row looks exactly like a track that was never fetched and is
// re-enqueued, re-fetched, re-judged and re-rejected on every single scan,
// forever. That is a self-inflicted read loop against the library disks (#679).
func TestEnqueuePendingSuppressesCategorical(t *testing.T) {
	ctx := context.Background()
	store := &fakePendingStore{results: []models.ScanResult{{
		ID:       1,
		FilePath: "/music/quarantined.flac",
		Track:    models.Track{ArtistName: "A", TrackName: "Quarantined"},
	}}}
	verdicts := &fakeTimingVerdicts{verdicts: map[string]scan.TimingVerdict{
		"A\x00Quarantined": {Outcome: timing.Categorical, ProvidersVersion: 3},
	}}
	work := &fakeWorkQueue{}

	e := scan.Enqueuer{
		Results: store, Cache: fakeLyricsCache{}, Queue: work,
		Timing: verdicts, ProvidersVersion: 3,
	}

	enqueued, _, err := e.EnqueuePending(ctx, models.Library{ID: 7})
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if enqueued != 0 || len(work.inputs) != 0 {
		t.Fatalf("enqueued=%d items=%d; want 0 -- a Categorical row must not be re-fetched on every scan",
			enqueued, len(work.inputs))
	}
}

// MisSynced is already safe: it WRITES a .txt, and #439's settled-sidecar check
// makes a later re-fetch a no-op. Suppressing it too would be scope creep that
// hides a recoverable track, so the suppression must be Categorical-only.
func TestEnqueuePendingSuppressesOnlyCategorical(t *testing.T) {
	ctx := context.Background()
	store := &fakePendingStore{results: []models.ScanResult{{
		ID:       1,
		FilePath: "/music/mistimed.flac",
		Track:    models.Track{ArtistName: "A", TrackName: "Mistimed"},
	}}}
	verdicts := &fakeTimingVerdicts{verdicts: map[string]scan.TimingVerdict{
		"A\x00Mistimed": {Outcome: timing.MisSynced, ProvidersVersion: 3},
	}}
	work := &fakeWorkQueue{}

	e := scan.Enqueuer{
		Results: store, Cache: fakeLyricsCache{}, Queue: work,
		Timing: verdicts, ProvidersVersion: 3,
	}

	enqueued, _, err := e.EnqueuePending(ctx, models.Library{ID: 7})
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if enqueued != 1 {
		t.Fatalf("enqueued=%d; want 1 -- MisSynced writes a .txt and is already safe, do not suppress it", enqueued)
	}
}

// Suppression EXPIRES with the provider generation. A Categorical verdict is a
// statement about what the providers served at ONE MOMENT; if the provider set
// changes, the track deserves another look. Tying expiry to the existing
// providers_version counter reuses the mechanism that already retires stale
// cache entries rather than inventing a parallel one.
func TestEnqueuePendingRetriesCategoricalAfterProviderGenerationChange(t *testing.T) {
	ctx := context.Background()
	store := &fakePendingStore{results: []models.ScanResult{{
		ID:       1,
		FilePath: "/music/quarantined.flac",
		Track:    models.Track{ArtistName: "A", TrackName: "Quarantined"},
	}}}
	verdicts := &fakeTimingVerdicts{verdicts: map[string]scan.TimingVerdict{
		"A\x00Quarantined": {Outcome: timing.Categorical, ProvidersVersion: 3},
	}}
	work := &fakeWorkQueue{}

	e := scan.Enqueuer{
		Results: store, Cache: fakeLyricsCache{}, Queue: work,
		Timing: verdicts, ProvidersVersion: 4, // provider set changed since the verdict
	}

	enqueued, _, err := e.EnqueuePending(ctx, models.Library{ID: 7})
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if enqueued != 1 {
		t.Fatalf("enqueued=%d; want 1 -- a new provider generation must re-examine a quarantined track", enqueued)
	}
}

// A nil Timing seam must preserve pre-#679 behavior exactly, so every non-serve
// caller (and any construction that predates this) keeps working unchanged.
func TestEnqueuePendingNilTimingSeamEnqueuesNormally(t *testing.T) {
	ctx := context.Background()
	store := &fakePendingStore{results: []models.ScanResult{{
		ID:       1,
		FilePath: "/music/song.flac",
		Track:    models.Track{ArtistName: "A", TrackName: "Song"},
	}}}
	work := &fakeWorkQueue{}

	e := scan.Enqueuer{Results: store, Cache: fakeLyricsCache{}, Queue: work}

	enqueued, _, err := e.EnqueuePending(ctx, models.Library{ID: 7})
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if enqueued != 1 {
		t.Fatalf("enqueued=%d; want 1 with no Timing seam wired", enqueued)
	}
}

// A lookup error must FAIL OPEN (enqueue anyway). Suppression is an
// optimization: a failed read of the verdict must never silently drop work,
// which would look exactly like a track that was fetched and lose it.
func TestEnqueuePendingFailsOpenOnTimingLookupError(t *testing.T) {
	ctx := context.Background()
	store := &fakePendingStore{results: []models.ScanResult{{
		ID:       1,
		FilePath: "/music/song.flac",
		Track:    models.Track{ArtistName: "A", TrackName: "Song"},
	}}}
	verdicts := &fakeTimingVerdicts{err: context.DeadlineExceeded}
	work := &fakeWorkQueue{}

	e := scan.Enqueuer{
		Results: store, Cache: fakeLyricsCache{}, Queue: work,
		Timing: verdicts, ProvidersVersion: 3,
	}

	enqueued, _, err := e.EnqueuePending(ctx, models.Library{ID: 7})
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if enqueued != 1 {
		t.Fatalf("enqueued=%d; want 1 -- a failed verdict lookup must not drop work", enqueued)
	}
}

// fakeVerdictReader is the raw primitive-shaped queue read the adapter wraps.
type fakeVerdictReader struct {
	outcome string
	version int
	found   bool
	err     error
	artist  string
	title   string
	lookups int
}

func (f *fakeVerdictReader) LookupTiming(_ context.Context, artist, title string) (string, int, bool, error) {
	f.lookups++
	f.artist, f.title = artist, title
	return f.outcome, f.version, f.found, f.err
}

// The adapter exists so internal/queue never has to import internal/scan (scan
// imports queue, and reversing that would be a cycle). It must convert the
// stored outcome STRING into the typed verdict without losing the generation.
func TestTimingVerdictsAdaptsStoredOutcome(t *testing.T) {
	r := &fakeVerdictReader{outcome: string(timing.Categorical), version: 9, found: true}

	v, found, err := scan.TimingVerdicts{Reader: r}.LookupTiming(context.Background(), "A", "T")
	if err != nil {
		t.Fatalf("LookupTiming: %v", err)
	}
	if !found {
		t.Fatal("found = false; want true when the reader reports a verdict")
	}
	if v.Outcome != timing.Categorical {
		t.Errorf("Outcome = %q; want %q", v.Outcome, timing.Categorical)
	}
	if v.ProvidersVersion != 9 {
		t.Errorf("ProvidersVersion = %d; want 9 -- losing it would break expiry", v.ProvidersVersion)
	}
	if r.artist != "A" || r.title != "T" {
		t.Errorf("reader saw (%q,%q); want the caller's artist/title verbatim", r.artist, r.title)
	}
}

// A nil Reader must be inert rather than panic: the adapter is constructed from
// wiring that a non-serve caller may legitimately leave unset.
func TestTimingVerdictsNilReaderIsInert(t *testing.T) {
	v, found, err := scan.TimingVerdicts{}.LookupTiming(context.Background(), "A", "T")
	if err != nil || found || v.Outcome != "" {
		t.Fatalf("nil reader = (%+v, %v, %v); want a zero verdict, not found, no error", v, found, err)
	}
}

// A reader error propagates rather than being reported as "no verdict". The
// caller distinguishes them: an error fails open (enqueue anyway), while
// not-found is the ordinary never-fetched case.
func TestTimingVerdictsPropagatesReaderError(t *testing.T) {
	r := &fakeVerdictReader{err: context.DeadlineExceeded}

	_, found, err := scan.TimingVerdicts{Reader: r}.LookupTiming(context.Background(), "A", "T")
	if err == nil {
		t.Fatal("err = nil; want the reader's error surfaced so the caller can fail open deliberately")
	}
	if found {
		t.Error("found = true on an errored lookup; want false")
	}
}

// An unrecognized stored value must not suppress. The column holds a verbatim
// timing.TimingOutcome, but a future value (or a hand-edited row) must degrade
// to "not Categorical" rather than matching something by accident.
func TestTimingVerdictsUnknownOutcomeDoesNotSuppress(t *testing.T) {
	ctx := context.Background()
	store := &fakePendingStore{results: []models.ScanResult{{
		ID:       1,
		FilePath: "/music/song.flac",
		Track:    models.Track{ArtistName: "A", TrackName: "Song"},
	}}}
	verdicts := &fakeTimingVerdicts{verdicts: map[string]scan.TimingVerdict{
		"A\x00Song": {Outcome: timing.TimingOutcome("something_else"), ProvidersVersion: 3},
	}}
	work := &fakeWorkQueue{}

	e := scan.Enqueuer{
		Results: store, Cache: fakeLyricsCache{}, Queue: work,
		Timing: verdicts, ProvidersVersion: 3,
	}

	enqueued, _, err := e.EnqueuePending(ctx, models.Library{ID: 7})
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if enqueued != 1 {
		t.Fatalf("enqueued=%d; want 1 -- only Categorical suppresses", enqueued)
	}
}

// A zero current generation (an unknown provider set, as in a one-shot CLI scan)
// must never suppress: without a trustworthy comparison the safe direction is to
// re-examine, since the cost is one refetch versus losing the track indefinitely.
func TestEnqueuePendingUnknownGenerationNeverSuppresses(t *testing.T) {
	ctx := context.Background()
	store := &fakePendingStore{results: []models.ScanResult{{
		ID:       1,
		FilePath: "/music/quarantined.flac",
		Track:    models.Track{ArtistName: "A", TrackName: "Quarantined"},
	}}}
	verdicts := &fakeTimingVerdicts{verdicts: map[string]scan.TimingVerdict{
		"A\x00Quarantined": {Outcome: timing.Categorical, ProvidersVersion: 0},
	}}
	work := &fakeWorkQueue{}

	e := scan.Enqueuer{
		Results: store, Cache: fakeLyricsCache{}, Queue: work,
		Timing: verdicts, ProvidersVersion: 0,
	}

	enqueued, _, err := e.EnqueuePending(ctx, models.Library{ID: 7})
	if err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if enqueued != 1 {
		t.Fatalf("enqueued=%d; want 1 -- an unknown generation must not suppress", enqueued)
	}
}
