// Package respdrift detects a provider that has stopped discriminating between
// requests: many DISTINCT queries answered with the SAME track identity.
//
// It exists because of a fault class that no error path can see. On 2026-09-04
// musixmatch returned ONE fixed, unrelated track for every query -- including a
// deliberately nonsensical artist/title -- while answering HTTP 200 with a
// valid envelope and real lyrics for a real song (#838). Every individual
// response was well-formed, so nothing examining a single response in isolation
// could tell anything was wrong. The fault was visible only ACROSS requests.
//
// That is the general shape worth naming: a semantic failure cannot be found by
// checking for errors, because there are none. It can only be found by checking
// an answer against something already known -- here, the other answers.
//
// The detector is deliberately narrow. It reports the condition and nothing
// else: it does not classify an outcome, trip a breaker, or suppress a write.
// A provider answering wrongly is not a transport failure, and #838
// deliberately classifies the resulting mismatch as a benign miss so it cannot
// ratchet the pacer or march rows toward retirement. Escalation policy needs
// evidence this detector has not yet produced.
package respdrift

import (
	"sync"

	"github.com/sydlexius/canticle/internal/normalize"
)

// minThreshold is the smallest meaningful threshold. One response cannot
// evidence repetition, so a configured 1 (or less) is raised to 2.
const minThreshold = 2

// relatedFloor is the Jaro-Winkler similarity at or above which a returned
// identity is treated as RELATED to the query, and therefore not evidence of a
// non-discriminating provider.
//
// Repetition alone is not the signal. A provider that CANONICALIZES editions --
// a remaster, a live cut, alternate punctuation -- legitimately answers several
// distinct queries with one credited recording, and a pre-push review reproduced
// exactly that firing the detector at run 5 with no fault present (#839). The
// original threshold argument asserted this "does not happen"; it does, from
// ordinary catalog structure.
//
// What separates the two cases is RELATEDNESS: a canonicalized variant stays
// close to what was asked, while a canned unrelated track does not. 0.75 matches
// the floor the musixmatch match guard already uses for the same judgment
// (#838), so the two agree about what "corresponds to the request" means.
const relatedFloor = 0.75

// Detector counts consecutive DISTINCT queries that returned the same identity.
// The zero value is not usable; call New.
type Detector struct {
	mu sync.Mutex

	threshold int

	// lastIdentity is the identity of the previous observation, and lastQuery
	// the query that produced it. Both are needed: the counter advances only
	// when the identity REPEATS while the query CHANGES.
	lastIdentity string
	lastQuery    string

	// run counts consecutive distinct queries sharing lastIdentity, including
	// the observation that first established it.
	run int

	// seen holds the DISTINCT query keys counted into the current run. A run is
	// defined by distinctness, not adjacency: tracking only the previous query
	// let q1, q2, q1 advance the run to 3 on two questions, so an ordinary retry
	// interleaved with one other request could fire the detector on a provider
	// that never stopped discriminating (CodeRabbit, PR #844). Cleared whenever
	// the run resets, and not grown once fired latches -- see Observe.
	seen map[string]struct{}

	// fired latches at the threshold so one fault produces one report rather
	// than one per subsequent request. Cleared when the run breaks, matching the
	// transition-reporting discipline the circuit breaker uses.
	fired bool
}

// New returns a Detector that reports once a run of `threshold` consecutive
// distinct queries has returned the same identity. A threshold below 2 is
// raised to 2 rather than rejected: the caller is wiring a diagnostic, and a
// misconfigured one should degrade to the minimum useful value, not fire on
// every first response.
func New(threshold int) *Detector {
	if threshold < minThreshold {
		threshold = minThreshold
	}
	return &Detector{threshold: threshold}
}

// Observe records one provider response and reports whether THIS observation
// completes a run long enough to evidence a non-discriminating provider.
//
// query is a stable key for what was ASKED (normalized artist+title); identity
// is a stable key for what came BACK (the returned artist+title). Both are
// opaque here: this package never parses the content, and never logs or exposes
// it, so a caller may pass normalized private metadata without it reaching a log
// line. It does HOLD the keys in memory for the life of a run -- comparing an
// answer against the other answers is the whole mechanism, and that requires
// remembering them (Copilot, PR #844). Nothing is persisted.
//
// Returns (fired, run). The run length is returned WITH the verdict rather than
// read back through Run(), because a caller doing Observe() then Run() takes the
// lock twice and can report a length that grew between the two calls -- the
// reported number would then describe a different run than the one that fired
// (PR review, #839). The worker drives one goroutine today, so this was latent
// rather than live, but the mutex and the concurrency test both assert this type
// tolerates concurrent callers, and a report that lies is worse than no report.
//
// Fires at most once per run. Two guards keep it from firing on healthy
// behavior:
//
//   - An EMPTY identity is never counted. A response with no usable identity
//     cannot evidence that two responses are the same, and counting blanks as
//     matches would fire on any provider that omits the field.
//   - A REPEATED query is never counted. The same question returning the same
//     answer is correct: a retry, a re-scan, or a duplicate queue row. Counting
//     it would fire on a healthy provider serving a stable catalog -- the
//     mirror-image outage of the one this detects.
func (d *Detector) Observe(query, identity string) (fired bool, run int) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if identity == "" {
		return false, d.run
	}

	// A response RELATED to its query is not evidence of anything, even repeated:
	// that is a provider canonicalizing editions of one recording, which is
	// correct behavior. Only an identity unlike the query can evidence a provider
	// that stopped discriminating.
	if normalize.MatchConfidence(query, identity) >= relatedFloor {
		// run 0, NOT 1: a related observation is not evidence, so it must not
		// stand in for a missing unrelated one. Setting it to 1 let the detector
		// fire having seen only threshold-1 unrelated queries, because one related
		// answer silently supplied the remaining count (Copilot, PR #844).
		d.reset(identity, query, 0)
		return false, d.run
	}

	// A different identity proves the provider IS discriminating, so whatever
	// evidence had accumulated is gone. Start over and re-arm the latch.
	if identity != d.lastIdentity {
		d.reset(identity, query, 1)
		return false, d.run
	}

	// Same identity for a question already counted is not evidence of anything.
	// Checked against the whole run, not just the previous query: a repeat is a
	// repeat whether or not something else was asked in between.
	if _, dup := d.seen[query]; dup {
		return false, d.run
	}

	// Once fired, the run is frozen. The latch holds until the identity changes,
	// so no further decision reads the length, and continuing to grow the set
	// would be unbounded during exactly the sustained fault this detects -- a
	// provider answering every distinct query identically never breaks the run.
	if d.fired {
		return false, d.run
	}

	d.lastQuery = query
	d.seen[query] = struct{}{}
	d.run++
	if d.run >= d.threshold {
		d.fired = true
		return true, d.run
	}
	return false, d.run
}

// reset starts a new run at the given length, seeding the distinct-query set to
// match. run is 1 when the observation itself counts as evidence (an unrelated
// identity establishing the run) and 0 when it does not (a related identity,
// which is correct provider behavior). Callers hold d.mu.
func (d *Detector) reset(identity, query string, run int) {
	d.lastIdentity = identity
	d.lastQuery = query
	d.run = run
	d.fired = false
	d.seen = make(map[string]struct{}, d.threshold)
	if run > 0 {
		d.seen[query] = struct{}{}
	}
}

// Run reports the current consecutive-distinct-query run length. It is for TESTS
// only: a report path must use the run returned by Observe, which is read under
// the same lock as the verdict it accompanies.
func (d *Detector) Run() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.run
}
