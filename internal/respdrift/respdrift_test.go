package respdrift

import "testing"

// TestFiresOnDistinctQueriesReturningOneIdentity is the core assertion. On
// 2026-09-04 musixmatch returned ONE fixed track for every query, including a
// deliberately nonsensical artist/title (#838). Every response was HTTP 200
// with a valid envelope and real lyrics, so nothing in the error path could
// see it -- the fault was only visible by comparing responses ACROSS requests.
func TestFiresOnDistinctQueriesReturningOneIdentity(t *testing.T) {
	d := New(3)
	got := []bool{}
	for _, q := range []string{"q1", "q2", "q3"} {
		fired, _ := d.Observe(q, "fixed-track")
		got = append(got, fired)
	}
	if got[0] || got[1] {
		t.Errorf("fired before the threshold: %v", got)
	}
	if !got[2] {
		t.Error("did not fire on the 3rd distinct query returning the same identity")
	}
}

// TestRepeatedQueryIsNotEvidence is the counterweight that keeps the detector
// from becoming its own outage. The SAME query returning the SAME answer is
// correct provider behavior -- a retry, a re-scan, a duplicate row. Counting it
// would fire on a healthy provider serving a stable catalog, which is the
// mirror-image failure of the one this detects.
func TestRepeatedQueryIsNotEvidence(t *testing.T) {
	d := New(3)
	for i := 0; i < 10; i++ {
		if fired, _ := d.Observe("same-query", "same-track"); fired {
			t.Fatalf("fired on repeat %d of an IDENTICAL query; that is correct provider behavior", i)
		}
	}
}

// TestDistinctIdentitiesReset asserts a healthy provider never accumulates
// toward the threshold.
func TestDistinctIdentitiesReset(t *testing.T) {
	d := New(3)
	for i, tc := range []struct{ q, id string }{
		{"q1", "track-a"}, {"q2", "track-b"}, {"q3", "track-c"},
		{"q4", "track-d"}, {"q5", "track-e"},
	} {
		if fired, _ := d.Observe(tc.q, tc.id); fired {
			t.Fatalf("fired at %d on a provider returning DISTINCT identities", i)
		}
	}
}

// TestOneDifferentIdentityBreaksTheRun asserts the counter is CONSECUTIVE.
// A single correct answer mid-run proves the provider is discriminating, so the
// evidence for a canned response is gone and the count must start over.
func TestOneDifferentIdentityBreaksTheRun(t *testing.T) {
	d := New(3)
	_, _ = d.Observe("q1", "fixed")
	_, _ = d.Observe("q2", "fixed")
	if fired, _ := d.Observe("q3", "a-real-different-track"); fired {
		t.Fatal("fired on a DIFFERENT identity")
	}
	if fired, _ := d.Observe("q4", "fixed"); fired {
		t.Fatal("did not reset after a differing identity broke the run")
	}
}

// TestFiresOncePerRun: the caller logs/alerts on a true return, so firing on
// every subsequent observation would turn one fault into a log flood. This is
// the same latch discipline the lane breaker uses for its transition messages.
func TestFiresOncePerRun(t *testing.T) {
	d := New(2)
	_, _ = d.Observe("q1", "fixed")
	if fired, _ := d.Observe("q2", "fixed"); !fired {
		t.Fatal("did not fire at the threshold")
	}
	for i := 0; i < 5; i++ {
		if fired, _ := d.Observe("q"+string(rune('a'+i)), "fixed"); fired {
			t.Errorf("fired again at %d; the latch must hold until the run breaks", i)
		}
	}
	// A differing identity breaks the run and re-arms the latch.
	_, _ = d.Observe("qx", "different")
	_, _ = d.Observe("qy", "fixed")
	if fired, _ := d.Observe("qz", "fixed"); !fired {
		t.Error("did not re-arm after the run broke")
	}
}

// TestEmptyIdentityIsNotCounted: a response carrying no usable identity cannot
// be evidence that two responses are the SAME. Fails open rather than counting
// blanks as matches, which would fire on any provider omitting the field.
func TestEmptyIdentityIsNotCounted(t *testing.T) {
	d := New(2)
	for i := 0; i < 5; i++ {
		if fired, _ := d.Observe("q"+string(rune('a'+i)), ""); fired {
			t.Fatalf("fired at %d on EMPTY identities", i)
		}
	}
}

// TestThresholdBelowTwoIsRaised: a threshold of 1 would fire on the very first
// response, and 0 or negative is meaningless. Two distinct queries is the
// minimum that can evidence repetition at all.
func TestThresholdBelowTwoIsRaised(t *testing.T) {
	for _, n := range []int{-1, 0, 1} {
		d := New(n)
		if fired, _ := d.Observe("q1", "fixed"); fired {
			t.Errorf("New(%d) fired on the FIRST observation; one response cannot evidence repetition", n)
		}
		if fired, _ := d.Observe("q2", "fixed"); !fired {
			t.Errorf("New(%d) did not fire at the raised minimum of 2", n)
		}
	}
}

// TestConcurrentObserveIsSafe: lanes are driven from the worker goroutine today,
// but a Lane is shared and nothing structurally prevents concurrent use. Run
// under -race.
func TestConcurrentObserveIsSafe(t *testing.T) {
	d := New(5)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				_, _ = d.Observe(string(rune('a'+n))+string(rune('0'+j%10)), "fixed")
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

// TestLegitimateEditionVariantsDoNotFire is the counterweight the original
// threshold argument lacked. The commit claimed "a healthy catalog answering
// five different questions with one answer is not something that happens."
// A pre-push review falsified that and it REPRODUCED: five distinct edition
// variants of one recording -- remaster, live, alternate punctuation -- that a
// provider legitimately canonicalizes to a single credited recording fired the
// detector at exactly run 5, with no provider fault.
//
// That is ordinary catalog structure, not a contrived case: a library holding a
// deluxe reissue, a live album, and a remaster of one song produces exactly this
// shape, and a directory-grouped scan can land them consecutively.
//
// The discriminator is RELATEDNESS, not repetition alone: a canonicalized
// edition variant stays similar to what was asked, while a canned unrelated
// track does not. Observe therefore counts a response only when the returned
// identity is UNLIKE the query.
func TestLegitimateEditionVariantsDoNotFire(t *testing.T) {
	d := New(5)
	const canonical = "aurora kestrel\x00marigold drift"
	for i, q := range []string{
		"aurora kestrel\x00marigold drift",
		"aurora kestrel\x00marigold drift - 2011 remaster",
		"aurora kestrel\x00marigold drift (live)",
		"aurora kestrel\x00marigold drift - live at the hall",
		"aurora kestrel\x00marigold drift (2011 remaster)",
	} {
		if fired, run := d.Observe(q, canonical); fired {
			t.Fatalf("fired at query %d (run %d) on legitimate edition variants of ONE recording; "+
				"the provider canonicalized them, it did not stop discriminating", i+1, run)
		}
	}
}

// TestUnrelatedIdentityStillFires pins that the relatedness guard did not
// disarm the detector: a fixed track unlike every query is the #838 fault and
// must still be caught.
func TestUnrelatedIdentityStillFires(t *testing.T) {
	d := New(5)
	queries := []string{
		"aurora kestrel\x00marigold drift",
		"bramblewood quintet\x00ninefold ascent",
		"cinder vale\x00hollow tide",
		"dovetail parade\x00glass orchard",
		"ember lyric\x00salt meridian",
	}
	fired := false
	for _, q := range queries {
		if f, _ := d.Observe(q, "unrelated performer\x00unrelated song"); f {
			fired = true
		}
	}
	if !fired {
		t.Error("did not fire on five distinct queries all answered with ONE unrelated track")
	}
}

// TestNonAdjacentRepeatedQueryIsNotEvidence pins the run as DISTINCT queries
// rather than merely non-adjacent ones. Asking q1, q2, q1 is two questions, not
// three, so a run built from it is one short of what the sequence looks like.
// The adjacent-only check let a retry interleaved with one other request inflate
// the run to the threshold, firing on a provider that never stopped
// discriminating (CodeRabbit, PR #844).
func TestNonAdjacentRepeatedQueryIsNotEvidence(t *testing.T) {
	d := New(3)
	for i, q := range []string{"q1", "q2", "q1"} {
		if fired, run := d.Observe(q, "fixed"); fired {
			t.Fatalf("fired at observation %d (run %d) on only TWO distinct queries; "+
				"q1 repeated non-adjacently must not advance the run", i+1, run)
		}
	}
}

// TestRelatedObservationDoesNotContributeToTheRun pins that a RELATED response
// contributes nothing. It is correct provider behavior -- a canonicalized
// edition -- so it is not evidence, and the run it leaves behind must be zero
// rather than one. Setting it to one let a single related observation stand in
// for a missing unrelated one, so the detector could fire having seen only
// threshold-1 unrelated queries (Copilot, PR #844).
func TestRelatedObservationDoesNotContributeToTheRun(t *testing.T) {
	const canonical = "aurora kestrel\x00marigold drift"
	d := New(3)

	// Related: the provider canonicalized an edition. Not evidence.
	if _, run := d.Observe("aurora kestrel\x00marigold drift (live)", canonical); run != 0 {
		t.Fatalf("a RELATED observation left run=%d; it is not evidence and must leave run=0", run)
	}

	// Two unrelated queries is one short of the threshold of three.
	for i, q := range []string{
		"bramblewood quintet\x00ninefold ascent",
		"cinder vale\x00hollow tide",
	} {
		if fired, run := d.Observe(q, canonical); fired {
			t.Fatalf("fired at unrelated query %d (run %d) with only TWO unrelated queries seen; "+
				"the related observation must not have counted toward the run", i+1, run)
		}
	}

	// The third unrelated query is the first legitimate firing point.
	if fired, _ := d.Observe("dovetail parade\x00glass orchard", canonical); !fired {
		t.Error("did not fire on the THIRD unrelated query; the guard must not disarm the detector")
	}
}
