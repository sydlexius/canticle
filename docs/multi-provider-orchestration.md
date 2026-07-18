# Multi-Provider Orchestration

## Status

This was originally a design record; the functionality it specified is now
shipped. Multi-provider orchestration runs in the worker today: the worker
dispatches each song over one or more provider lanes through
`internal/orchestrator`, with per-lane circuit breakers. Two dispatch modes are
selectable via `providers.mode`:

- **`ordered`** (the default) queries the configured lanes in priority order -
  the primary first, then each entry in `providers.fallback_order` - and returns
  the first suitable result.
- **`parallel`** dispatches every lane concurrently and races them; the first
  suitable result wins, and an unsynced winner is held for a bounded window
  (`providers.race_wait_seconds`, default 2) so a slower synced result can
  preempt it.

The petitlyrics adapter in `internal/petitlyrics` is wired as a fallback lane
(add it to `providers.fallback_order`) as well as being selectable as the
primary. A single-lane deployment (Musixmatch only, no fallback) behaves
identically under either mode: one lane, one call. The sections below describe
how the lanes run together without races, double-writes, or shared rate-limit
state.

This document is retained for the design rationale and the orchestration
contracts (cancellation, dedup, suitability/ranking, per-lane breakers); those
remain accurate. The original "this is unbuilt / out of scope" framing has been
corrected to reflect the shipped behavior. The implementation landed across
issues #173 (the `internal/circuit` extraction), #174 (the lane abstraction and
ordered dispatch), #175 (the petitlyrics fallback lane and `providers_version`
write-and-compare), and #176 (the parallel-race path with the
`race_wait_seconds` upgrade window).

## Topology

### Decision

**Hybrid, with ordered fallback as the default.** Query Musixmatch first; fall
back to a secondary provider (for example petitlyrics) only when the primary
returns a miss or an unsuitable result. Parallel race (dispatch every lane at
once, first-suitable-wins, cancel the rest) is the alternative mode, selectable
in config.

```text
[providers]
mode = "ordered"   # default; "parallel" opts into the race
```

### Rationale

Ordered fallback minimizes calls to rate-limited and grey-market upstreams. A
secondary provider exists to cover catalog gaps Musixmatch lacks (for example
CJK content), not to compete on latency: most tracks resolve on the primary, so
the secondary is only touched on a real miss. This keeps the call budget low and
the rate-limit reasoning simple: each lane is hit at most once per dispatch, and
the secondary is hit only when the primary already returned nothing.

The tradeoff is latency on a primary miss: the secondary is not consulted until
the primary round-trip completes (or its circuit is open). Parallel race trades
call efficiency for that latency: it dispatches all lanes concurrently and takes
the first suitable result, which is faster when a provider is slow but wastes
the calls that lose the race. Because Musixmatch is the lane we least want to
spend calls against, parallel race is opt-in and never the default.

In both modes the lane order is the configured fallback priority. A single-lane
deployment (Musixmatch only, with no `providers.fallback_order`) behaves
identically under either mode: one lane, one call.

## Cancellation model

A single `context.Context` is derived per song-dispatch from the worker's
run context. In **ordered** mode there is at most one in-flight fetch at a time,
so cancellation is only the normal propagation of the worker context (shutdown,
deadline); a satisfied dispatch simply returns and the next lane is never
started.

In **parallel** mode the orchestrator derives a child context with
`context.WithCancel`. It launches one goroutine per available lane, each calling
`lane.FindLyrics(childCtx, track)`. When the orchestrator commits a result (see
"Suitability and ranking") it calls `cancel()`, which propagates to every
sibling lane's in-flight HTTP request and pacer wait. The Musixmatch client
already honors `ctx.Err()` at its pacer and request boundaries, so a cancelled
lane returns promptly with a context error rather than completing a wasted call.

Cleanup contract (no leaked goroutines):

- Results funnel into a channel buffered to the number of launched goroutines,
  so a lane that returns after the orchestrator has already committed can always
  send without blocking, then exit.
- The orchestrator `defer cancel()`s on every return path, including the success
  path, so cancelling is unconditional once a winner is chosen.
- A cancelled lane's `ctx.Err()` return is not treated as a provider error; it
  is discarded by the collector. Only a lane that returns before cancellation
  contributes to the outcome.
- The orchestrator does not wait for the losing goroutines to drain before
  returning the winner; they observe the cancel and exit on their own. Because
  the result channel is buffered to the goroutine count, no late sender blocks
  and no goroutine is leaked.

## Race and dedup guarantee

**Exactly one success-write per song.** This is guaranteed by the existing
compare-and-set guard in `queue.Complete`, reused unchanged:

```sql
UPDATE work_queue
   SET status = 'done', completed_at = ?, last_error = ''
 WHERE id = ?
   AND status = 'processing'
```

The `AND status = 'processing'` predicate is the CAS. The first writer flips the
row to `done`; any second attempt for the same id matches zero rows,
`requireAffected` returns an error, and that late write is discarded. The same
transaction flips every linked `scan_results` row to `done`, so a successful
`Complete` leaves the work-queue row and all originating scan results atomically
in agreement. Crash or partial write between the two updates is impossible:
SQLite commits the whole transaction or rolls it back.

This backstop holds regardless of topology. In ordered mode there is only ever
one candidate writer, so the guard is never actually contended. In parallel
mode the orchestrator already collapses to a single winner before any write (a
mutex-guarded "best result so far" slot, committed once), so the CAS is a
second line of defense, not the primary serialization. The orchestrator is the
single writer per dispatch by construction; `queue.Complete` is the durable
guarantee that even a logic bug cannot produce two finalizations.

### Reconciliation with the busy-retry upserts

`Enqueue` uses `ON CONFLICT(artist_key, title_key) DO UPDATE` to coalesce
duplicate inputs, and its `CASE` arms already refuse to disturb a row whose
status is `processing` or `done` (artist, title, album, paths, status, and
`next_attempt_at` are all preserved for those two states). Orchestration changes
nothing here: a song is claimed exactly once via `Dequeue` (which atomically
flips `pending`/`failed`/`deferred` to `processing`), the orchestrator races
lanes only within that single claimed item, and `Complete` finalizes it under
the CAS. Concurrent enqueues of the same track collapse into the one row and
cannot create a second processing claim, so they cannot create a second writer.
No new upsert arm and no new column are required for dedup.

## Suitability and ranking

A result is **suitable** when it passes both gates:

1. **Script guard.** `langguard.Guard.Accept(song)` returns `(true, "")`. The
   guard scores the concatenated lyric body against the `accepted_scripts`
   allowlist and the foreign-script-share threshold. A disabled guard (empty
   allowlist) accepts everything. The guard is now wired into config and the
   worker pipeline (issue #163).
2. **Quality bar.** The result carries usable lyrics: synced is preferred over
   unsynced, and an instrumental marker or an empty body is not suitable on its
   own. The quality rank is: synced > unsynced > instrumental > none.

### Ranking rule

**First-suitable-wins, with a bounded upgrade wait in parallel mode.**

In **ordered** mode there is no ranking decision to make: lanes are queried one
at a time and the first suitable result is returned immediately. The next lane
is consulted only if the current one is unsuitable, so two suitable results are
never in hand at once.

In **parallel** mode a fast lane may return an unsynced result while a slower
lane is still fetching a synced one. To avoid committing the worse result, the
orchestrator accepts an unsynced result but holds the write for a bounded
window, during which a synced result (a strict quality upgrade) preempts it. A
synced result that arrives first wins immediately and cancels the window; if the
window elapses with no upgrade, the held unsynced result is committed.

This window is a configuration value, never a literal (see "Gap 2"). Its key is
`[providers] race_wait_seconds` with a default of `2`.

## Per-lane rate-limit and circuit-breaker composition

**Per-lane breakers, never a global pool against Musixmatch.** Each provider
lane owns its own circuit breaker, cooldown, and geometric backoff. Independent
upstreams have independent limits, so a lane that trips does not pause a sibling
lane. Musixmatch is never pooled: it stays one lane with one in-flight call at a
time, exactly as today.

Today the breaker state lives as fields on `Worker`
(`circuitOpenUntil`, `circuitBackoffBase`, `circuitOpenDuration`,
`consecutiveCircuitTrips`, `circuitProbing`, `everProviderSuccess`), driven by
`tripCircuitIfRateLimited` and the half-open probe gate in `RunOnce`. The
worker doc already flags that `everProviderSuccess` (and the rest of this
state) becomes a data race the moment per-provider concurrency lands, because
`RunOnce` is single-goroutine and the fields carry no mutex. Orchestration is
that moment, so the breaker must move out of `Worker` and into a per-lane
component before any second lane exists. The staging that keeps the tree out of
a two-breaker state is specified in "Gap 3".

Composition across lanes:

- A benign miss (`ErrNotFound`, `ErrNoLyrics`) does not trip a lane's breaker; a
  clean miss is a successful round-trip, not a throttle signal. This preserves
  the current behavior where a benign miss resets the circuit ramp.
- A rate-limit or auth error (`ErrRateLimited`, `ErrUnauthorized`,
  `ErrTokenRenewalRequired`) trips only the lane that saw it.
- If every available lane has its breaker open, the orchestrator reports that
  the whole dispatch is unavailable, and the worker releases the item back to
  `pending` with no failure increment (the existing `Release` semantics), rather
  than recording a miss. Cross-lane error precedence is specified in "Gap 4".

## Resolved gaps

The maintainer flagged four gaps that this design must close. Each is resolved
below.

### Gap 1: Per-provider cache ownership

**Resolved: no schema change now. The single-slot `lyrics_cache` plus the
`work_queue.providers_version` invalidation seam is sufficient for ordered
fallback.**

The cache (`internal/cache`) is single-slot: one lyrics blob keyed by
`(artist, title, album)`, with `LookupExact`, `LookupFallback`, and `Store`. In
ordered fallback the first provider to return a suitable result wins and is the
only result that exists, so there is nothing per-provider to retain: last (and
only) suitable writer wins, and that is the served blob. This is the same
single-slot contract the cache already enforces.

Changing the provider set (adding or removing a provider) is handled by the
`work_queue.providers_version` column, not by a cache schema change. The
mechanism (implemented in #175):

- The active provider set (primary plus `providers.fallback_order`) is hashed
  into a deterministic generation by `providers.Generation` - the sorted,
  lowercased, comma-joined provider names through FNV-64a, masked to 31 bits so
  it round-trips safely through a SQLite `INTEGER`. Sorting before hashing makes
  the generation a function of the provider *set*, so reordering alone does not
  change it; adding or removing a provider does.
- `DBQueue.Enqueue` stamps the current generation onto each new `work_queue` row
  (`DBQueue.SetProvidersVersion`, set once at startup). The stamp is written only
  on a fresh insert; the `ON CONFLICT` refresh and the `Defer` / `RecheckDeferred`
  paths leave it untouched, so an item keeps the generation it was first enqueued
  under.
- When the worker dequeues an item whose stored `providers_version` differs from
  the worker's configured generation (`Worker.SetProvidersVersion`), it bypasses
  the cache lookup and re-fetches against the current lanes, so a result cached
  under an older provider set is revalidated rather than served stale. A worker
  generation of `0` (no providers configured) honors the cache unconditionally,
  preserving single-provider behavior.

This is exactly the "reserved seam for the future multi-source re-check sweep"
the field was added for, with no `lyrics_cache` schema change.

**When a per-provider cache column would become necessary (explicit future
work, not added now):** add a `provider` column to `lyrics_cache` (making the
key `(artist, title, album, provider)`) only if we later want to

- retain and compare results from multiple providers for the same track (for
  example to pick the best at read time rather than at fetch time), or
- cache a losing provider's result so a future config change that demotes the
  current winner can fall back to it without a re-fetch, or
- attribute a cached blob to its source provider for diagnostics or
  per-provider invalidation finer than the global generation bump.

None of these are required for ordered fallback or for the opt-in parallel race,
both of which commit exactly one winning result per dispatch. The per-provider
column is therefore deferred until one of those needs is real.

### Gap 2: No hardcoded wait

**Resolved: the parallel-race bounded upgrade wait is a config value, not a
literal.** The key is `[providers] race_wait_seconds` (integer seconds), default
`2`. The orchestrator reads it into a duration once at construction; no
`2 * time.Second` literal appears in the dispatch path. The value is only
consulted in parallel mode; ordered mode ignores it.

### Gap 3: Single breaker, no dual-breaker intermediate

**Resolved: extract the breaker into `internal/circuit` and migrate the single
Musixmatch lane through it in one step, so there is exactly one breaker system
at every point in the migration.**

The hazard to avoid is leaving the old `Worker` breaker fields in place
alongside new per-lane breakers with a "migrate later" note: that two-breaker
state tends to become permanent and produces two disagreeing cooldowns.

Migration order (each step compiles and ships with one breaker system, never
two):

1. **Extract.** Create `internal/circuit` with a `Breaker` type holding the
   current fields (`openUntil`, `consecutiveTrips`, `backoffBase`,
   `openDuration`, and the probe/ever-success flags) and the trip and half-open
   logic ported verbatim from `tripCircuitIfRateLimited` and the `RunOnce` gate.
   In the same change, delete those fields and that method from `Worker` and
   have `Worker` drive its single Musixmatch path through one `circuit.Breaker`
   instance. There is no moment where both `Worker` and the extracted breaker
   own the state: it is one atomic move. The breaker becomes a guarded type
   (mutex or atomics) so it is safe under the concurrency the next step
   introduces, closing the `everProviderSuccess` data-race the worker doc flags.
2. **Wrap as a lane.** Introduce the provider-lane abstraction that embeds a
   `circuit.Breaker`, and rebuild the existing Musixmatch path as a single lane.
   The worker now dispatches through the orchestrator holding that one lane.
   Still one breaker, now owned by the lane instead of the worker.
3. **Add a second lane.** Each new lane instantiates its own `circuit.Breaker`
   with its own per-provider parameters. There are now N independent breakers,
   one per lane, and zero on the worker. The tree never passed through a state
   with a worker breaker and a lane breaker both live.

### Gap 4: Cross-lane error precedence

**Resolved: a rate-limit or auth error from any lane outranks a benign miss from
another, so the queue backs off rather than recording a stable miss.**

When lanes disagree, the orchestrator returns the highest-precedence outcome:

1. **Auth / rate-limit** (`ErrUnauthorized`, `ErrTokenRenewalRequired`,
   `ErrRateLimited`, HTTP 401/429-class) from any lane. Returned as that error,
   which drives the worker's release-and-back-off path (the item is retried, not
   retired). This is highest because it means the catalog answer is unknown: a
   throttled or unauthorized lane did not actually tell us the track is absent,
   so recording a miss would be a lie that suppresses a future successful fetch.
2. **Transport error** (timeout, connection failure, a non-benign HTTP error).
   A retriable failure; returned so the item is retried under the normal failure
   backoff rather than deferred as a clean miss.
3. **Benign miss** (`ErrNotFound`, `ErrNoLyrics`) from every lane that
   answered. Only when no lane reported a higher-precedence outcome is the
   dispatch a genuine miss: every reachable provider was asked and none had the
   track. Returned as a benign miss so the worker defers it (catalog may grow),
   without tripping any circuit.
4. **All-lanes-unavailable** (every available lane's breaker is open). Treated
   like a rate-limit for queue purposes: the item is released back to `pending`
   with no failure increment, because no lane was actually consulted.

The ordering rule is "least-certain-negative wins": any signal that we did not
truly learn the track is absent (auth, rate-limit, transport, open circuit)
outranks the only signal that the track is absent (a benign miss). A stable miss
is recorded only when every reachable lane returned a benign miss and nothing
higher.

## Implementation status

This document decided contracts; the implementation is now complete. The
`internal/circuit` extraction (#173), the lane abstraction and ordered dispatch
(#174), the petitlyrics fallback lane plus `providers_version`
write-and-compare (#175), and the parallel-race dispatch path with the
`race_wait_seconds` upgrade window (#176) have all landed. What shipped, against
the original out-of-scope notes:

- **The petitlyrics fallback lane runs.** A second provider (for example the
  CJK/petitlyrics lane) is configured via `providers.fallback_order` and runs
  alongside Musixmatch under ordered fallback or, in `parallel` mode, races it.
  The adapter is also still selectable as the primary.
- **The orchestrator is shipped.** The lane abstraction, the `internal/circuit`
  extraction, the ordered and parallel dispatch paths, the `race_wait_seconds`
  config, and the `providers_version` write-and-compare are all in the worker,
  sequenced per "Gap 3".
- **No schema change was needed.** The per-provider cache column remains
  deferred until one of the triggers in "Gap 1" is real; ordered fallback and
  the parallel race both commit exactly one winning result per dispatch.

## Cross-references

- Issue [#148](https://github.com/sydlexius/canticle/issues/148) - this design record
- Issue [#146](https://github.com/sydlexius/canticle/issues/146) - multilingual output policy (the suitability quality bar interacts with bilingual output)
- Issue [#149](https://github.com/sydlexius/canticle/issues/149) - second lyrics source; deferred per-provider cache handling to this design (resolved in Gap 1)
- Issue [#163](https://github.com/sydlexius/canticle/issues/163) - langguard wired into config and the worker pipeline (supplies the suitability script gate)
- `internal/queue/queue.go` - `Complete` CAS guard, `Enqueue` busy-retry upserts, `providers_version` column
- `internal/worker/worker.go` - current circuit-breaker fields and `tripCircuitIfRateLimited`
- `internal/langguard/guard.go` - `Guard.Accept` suitability decision
- `internal/cache/cache.go` - single-slot `lyrics_cache`
- `internal/providers/providers.go` - `Select` (single-provider today), provider names
- `internal/petitlyrics` - existing secondary adapter (single-select today)
