package orchestrator

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/respdrift"
)

// escalationThreshold is the number of consecutive circuit trips (with zero
// intervening provider successes, after at least one earlier success) after
// which the throttle log escalates from a steady throttling note back to a Warn
// that the token, valid earlier this session, may now have expired. It mirrors
// the worker's prior constant of the same name (the classification moved here).
const escalationThreshold = 5

// Lane wraps a single resolve func with its own circuit breaker. It owns the
// breaker interaction (open-gate short-circuit, half-open probe note, the
// error classification+trip, benign-miss reset, success recording). The
// error->breaker policy is injected as classifyErr so a provider lane and a
// detector lane can bring different semantics over the same breaker machinery.
type Lane struct {
	name        string
	resolve     ResolveFunc
	breaker     *circuit.Breaker
	classifyErr func(l *Lane, err error) error
	pacer       providers.AdaptivePacer // optional; nil when the lane has no pacer
	// local marks a lane that resolves without an outbound provider request (the
	// detector lane today). It is surfaced per-attempt on models.LaneAttempt so
	// the worker can tell whether an item spent the provider-request pacing
	// budget (#534). The zero value is false, so a lane is treated as remote
	// unless it opts in -- a new lane cannot accidentally suppress pacing.
	local bool
	// drift detects a provider that has stopped discriminating between requests
	// (#839). Optional and nil by default, so a lane that does not opt in behaves
	// exactly as before. See WithResponseDrift.
	drift   *respdrift.Detector
	onDrift func(lane, run string)
}

// Name reports the lane's name.
func (l *Lane) Name() string { return l.name }

// Local reports whether the lane resolves without an outbound provider request.
func (l *Lane) Local() bool { return l.local }

// Breaker exposes the lane's breaker (construction + tests asserting ramp state).
func (l *Lane) Breaker() *circuit.Breaker { return l.breaker }

// Pacer exposes the lane's optional adaptive pacer (nil when the lane has
// none), so a caller building a second lane can share the SAME pacer instance
// -- e.g. wiring the primary provider lane's pacer into a local lane
// (NewDetectorLane) so that lane's settles can credit ratchet-down decay
// without themselves spending the provider-request pacing budget (#550).
func (l *Lane) Pacer() providers.AdaptivePacer { return l.pacer }

// FindLyrics drives the lane's breaker around a resolve call. An open breaker
// returns ErrLaneUnavailable without calling resolve. Errors run through the
// lane's injected classifier; success records recovery and pacer stabilization.
func (l *Lane) FindLyrics(ctx context.Context, track models.Track, sourcePath string) (models.Song, error) {
	switch l.breaker.Allow() {
	case circuit.StateOpen:
		return models.Song{}, ErrLaneUnavailable
	case circuit.StateHalfOpen:
		slog.Debug("lane circuit half-open; probing", "lane", l.Name())
	case circuit.StateClosed:
	}

	song, err := l.resolve(ctx, track, sourcePath)
	if err != nil {
		return models.Song{}, l.classifyErr(l, err)
	}

	if l.breaker.RecordSuccess() {
		slog.Info("lane circuit closed; recovered", "lane", l.Name())
	}
	l.notifySuccess()
	l.observeDrift(track, song)
	return song, nil
}

// WithResponseDrift opts this lane into non-discriminating-provider detection
// (#839). report is called AT MOST ONCE per run, with the lane name and the run
// length -- never with the identity that repeated, which is a track title from
// the user's library and must not reach a log line or a metric label.
//
// Opt-in rather than automatic: a detector on every lane would change behavior
// for lanes nobody has reasoned about, and the detector's own cost is only
// justified where a silent wrong answer is plausible.
func (l *Lane) WithResponseDrift(d *respdrift.Detector, report func(lane, run string)) *Lane {
	l.drift = d
	l.onDrift = report
	return l
}

// DriftWired reports whether this lane opted into response-drift detection.
// It exists so a caller's wiring can be ASSERTED rather than assumed: an
// unwired detector passes every test its own package has, which is exactly how
// a feature ships dead.
func (l *Lane) DriftWired() bool { return l.drift != nil }

// observeDrift feeds one successful response to the detector, if wired.
//
// The keys are NORMALIZED artist+title on both sides -- what was asked, and what
// came back. Normalization matters: without it, ordinary case or spacing
// variation between the request and the response would read as a different
// identity and mask a genuinely repeating one.
func (l *Lane) observeDrift(requested models.Track, got models.Song) {
	if l.drift == nil {
		return
	}
	query := normalize.NormalizeKey(requested.ArtistName) + "\x00" + normalize.NormalizeKey(requested.TrackName)
	identity := normalize.NormalizeKey(got.Track.ArtistName) + "\x00" + normalize.NormalizeKey(got.Track.TrackName)
	// A response carrying no identity at all cannot evidence repetition; the
	// detector drops it, but check here too so the separator alone never counts.
	if identity == "\x00" {
		return
	}
	// The run length comes back WITH the verdict: reading it back via Run() takes
	// the lock a second time, so a concurrent observation could grow the run
	// between the two calls and the report would name a different run than the
	// one that fired (PR review, #839).
	if fired, run := l.drift.Observe(query, identity); fired && l.onDrift != nil {
		l.onDrift(l.Name(), strconv.Itoa(run))
	}
}

// notifyThrottle forwards a throttle notification to the lane's pacer, if any.
func (l *Lane) notifyThrottle() {
	if l.pacer != nil {
		l.pacer.OnThrottle()
	}
}

// notifySuccess forwards a success notification to the lane's pacer, if any.
func (l *Lane) notifySuccess() {
	if l.pacer != nil {
		l.pacer.OnSuccess()
	}
}
