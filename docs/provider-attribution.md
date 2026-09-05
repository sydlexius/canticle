# Design: per-track attribution for a multiplexing provider lane (issue #850)

Status: decision of record. Design only -- this document ships no Go code and no
schema change. The implementer for the InnerTube adapter (slice G, tracked
separately under the #849 epic) builds from the "What the implementer builds"
section at the end.

Scope: how Canticle represents the upstream lyric licensor for a result served by
a lane that MULTIPLEXES several upstreams, and how that representation stays
compatible with the three consumers that today assume one lane means one source.

Part of #849. Does not block slices C1-C4.

## Problem

Every lane Canticle has shipped so far is a straight line: one lane is one
provider is one `[source:]` token. The InnerTube lane breaks that line. The #848
spike measured, across four public reference tracks, that per-track attribution
alternates between two distinct upstream lyric providers -- two tracks each. The
lane is a transport in front of an editorial routing decision we do not make and
cannot predict from the request.

Three places in the current tree assume the straight line. Each was read at
current HEAD for this document rather than taken from the issue text:

1. **`internal/lyrics/writer.go:290-292`** writes the winning lane verbatim into
   the header tag:

   ```go
   if song.WinningLane != "" {
       tags = append(tags, fmt.Sprintf("[source:%s]", song.WinningLane))
   }
   ```

   The instrumental branch at `writer.go:326-331` does the same thing through a
   `src` local that the detector overrides.

2. **`internal/purgeprovenance/purgeprovenance.go:186-197`**, `provenanceAgrees`,
   compares that on-disk tag against `work_queue.provider_lane` and treats a
   mismatch as positive evidence of disagreement, which blocks deletion (#827).
   The only tolerated asymmetry is the detector's, spelled out explicitly:

   ```go
   if tag == lane { return true }
   return tag == lyrics.SourceDetector && lane == lyrics.DetectorLaneName
   ```

   The same package's `Filter.matches` (`purgeprovenance.go:59-64`) is a bare
   `source == f.Source` string equality, so the token is load-bearing for the
   `--source` selector as well as for the guard.

3. **`internal/reports/reports.go:281-289`** (`ProviderEffectiveness`) groups
   `lane_attempts` by `lane`, and **`internal/server/metrics.go:145-155`** emits
   `mxlrcgo_provider_hits_total{lane="..."}` / `mxlrcgo_provider_misses_total`
   keyed the same way. One lane means one row and one label, so a lane whose
   upstream varies shows an undifferentiated aggregate.

## The recommendation, argued

The issue proposed: keep the lane name `innertube` for breaker, pacing and
config purposes, and carry the per-result upstream separately on `models.Song`,
where it "feeds `[source:]` and `lane_attempts`". That proposal is **half
confirmed and half rejected**, and the split matters.

### Confirmed: the lane name stays `innertube`

The lane is the unit the rest of the system actually operates on, and every one
of those uses is about the transport, not the licensor:

- `internal/circuit` models one breaker per lane. There is one HTTP endpoint,
  one token bucket and one throttle response behind InnerTube, so a per-upstream
  breaker would be two breakers over one shared failure domain -- tripping either
  would be meaningless, since neither can be avoided independently.
- The `AdaptivePacer` and the provider-ordering config address a lane by name.
  An operator cannot order or disable an upstream they do not select.
- `work_queue.provider_lane` is stamped from the lane at completion
  (`internal/queue/queue.go:1879-1890`, and inside the settle transaction at
  `queue.go:736`). Its consumers -- the reports UI label
  (`internal/web/lanelabel.go`), the mark dispatch (`internal/web/lanemark.go`),
  the purge guard -- all want the operator-facing lane.

Nothing argues the other way. Confirmed as proposed.

### Rejected: the upstream must NOT feed `[source:]`

Writing the upstream into `[source:]` -- so an InnerTube result served by
upstream A lands as `[source:<A>]` while the row's lane is `innertube` -- is the
part of the recommendation this document rejects. Three concrete harms, in
increasing order of severity:

1. **It breaks the purge guard.** With `tag = "<A>"` and `lane = "innertube"`,
   `provenanceAgrees` falls through both of its true branches and returns
   `false`. Every InnerTube-served sidecar in the library becomes a permanent
   provenance disagreement, and `purge-provenance` refuses to delete any of them.
   The guard would need to grow a third asymmetry case, and that case is not a
   fixed pair like the detector's -- it is a set that grows every time the
   multiplexer adds an upstream.

2. **It makes an InnerTube result indistinguishable from a first-party one.**
   Upstream A is also a lane Canticle ships directly. `[source:<A>]` written by
   the InnerTube lane is byte-identical to `[source:<A>]` written by the direct
   lane. An operator running `--source <A>` to purge that provider's results
   would silently sweep in every file InnerTube happened to route through it --
   a deletion of files the operator did not target, which is precisely the
   class of bug #827 exists to prevent. `Filter.matches` is exact equality with
   no lane context available at the match site, so there is no way to
   disambiguate after the fact.

3. **It would attach a brand mark to a result we did not get from that brand.**
   `laneMark` dispatches on the persisted lane, so this specific harm does not
   land today -- but only because the mark reads the lane rather than the tag.
   Under a compound or upstream-valued `[source:]`, any future reader that keys
   the mark off the tag inherits the confusion.

A compound token (`innertube:<A>`) was considered as a middle path. It has harm 1
in full -- `tag != lane`, so the guard still fails -- and it converts every
existing string-equality site on the source token into a prefix-parse, each one a
fresh chance for a #827-class bug. Rejected.

### Rejected: the upstream must NOT feed `lane_attempts`

`lane_attempts` is the per-track hit/miss record whose whole purpose (#282) is a
TRUE per-track hit rate: one row per attempted lane, `hit=1` for the winner and
`hit=0` for every lane that was tried and lost. An upstream is never *attempted*.
We do not choose it, we cannot try it and fail, and it has no miss row to write.
Splitting `lane_attempts` by upstream would put rows in a table whose semantics
they do not satisfy, and would give `ProviderEffectiveness` a denominator that
lies: a per-upstream "hit rate" would measure the multiplexer's editorial routing,
not Canticle's provider selection, while sharing a column with numbers that mean
the latter.

`lane_attempts.lane` stays the lane. Unchanged, no migration.

## Decision

**The per-result upstream is a distinct concept from the lane, carried in a
distinct field and written to a distinct LRC header tag.**

### 1. In memory

A new string field on `models.Song`, sibling to `WinningLane`, set by the
InnerTube lane on the result it returns and empty for every other lane:

```go
// Upstream is the lyric licensor a MULTIPLEXING lane routed this result to,
// when the lane serves several and reports which one. Empty for a lane that
// is its own upstream, on cache hits, and on zero-value songs.
Upstream string `json:"-"`
```

`json:"-"` is not incidental. It matches `WinningLane` and is required for the
same reason: `encodeSong`/`decodeSong` (`internal/worker/worker.go:2208-2232`)
round-trip the song through the lyrics cache, and a serialized upstream would let
a cache hit resurrect an attribution that was true for a different fetch. The
cache is keyed on (artist, title, duration bucket) and knows nothing about which
upstream served the entry it stores.

### 2. On disk

A new LRC header tag, `[upstream:<token>]`, written immediately after
`[source:]` wherever `[source:]` is written and the upstream is known.

The tag is inert to every current reader, which was verified rather than assumed:
`parseTagLine` (`internal/lyrics/parser.go`) accepts any bracketed `key:value`
whose key does not begin with a digit, so `[upstream:...]` parses as a header tag
and does not terminate the header block; `ReadProvenanceTags` and
`ReadInstrumentalProvenance` both switch on a closed set of keys and ignore the
rest; `InjectProvenance` writes a fixed set and does not round-trip unknown keys
into a rewrite. Adding the tag changes no existing behavior.

### 3. Exact tokens

This is the part ambiguity turns into a purge bug, so it is specified
exhaustively. `<lane>` is the literal `innertube`. `<A>` and `<B>` are the two
upstream tokens, each a lowercase, trimmed constant declared in the innertube
adapter package (see "closed set" below):

| Placeholder | Literal token |
| --- | --- |
| `<A>` | `musixmatch` |
| `<B>` | `lyricfind` |

Lowercase and unpunctuated, matching the existing provider-token convention
(`petitlyrics`, `canticle-detector`). `<A>` is deliberately byte-identical to the
token the direct first-party lane already writes -- that collision is the subject
of harm 2 above, and it is why the upstream is never written into `[source:]`.

Any third value the API reports -- an upstream not in this set, a renamed one, an
empty or unparsed field -- maps to the empty string and writes NO `[upstream:]`
tag. There is no passthrough and no "other" token; the closed switch has exactly
these two arms plus a default that yields nothing.

| Situation | `[source:]` | `[upstream:]` |
| --- | --- | --- |
| Fresh fetch, InnerTube lane wins, upstream A reported | `[source:innertube]` | `[upstream:<A>]` |
| Fresh fetch, InnerTube lane wins, upstream B reported | `[source:innertube]` | `[upstream:<B>]` |
| Fresh fetch, InnerTube lane wins, no upstream reported | `[source:innertube]` | omitted |
| Fresh fetch, any other provider lane wins | `[source:<that lane>]` | omitted |
| Cache hit (any lane) | omitted | omitted |
| Detector-written instrumental marker | `[source:canticle-detector]` | omitted |
| InnerTube-sourced instrumental (provider-asserted) | `[source:innertube]` | `[upstream:<X>]` if known |

The cache-hit row is existing behavior, not a new rule: `WinningLane` is empty on
a decoded cache hit, so the writer's `song.WinningLane != ""` guard already omits
`[source:]` there. `Upstream` is empty on the same path for the same reason, so
the two tags appear and disappear together. A cache-hit sidecar has always been
part of purge's `NoSource` cohort and remains so.

An omitted `[upstream:]` asserts nothing. It is not "no upstream" and not
"unknown provider" -- it is the absence of a claim, matching how `[dv:]` is
handled for an unknown detector version.

**Closed set, never a passthrough.** The upstream token is mapped from the API
response through a fixed switch to a package constant, and an unrecognized value
writes NO tag rather than the raw string.

The rationale is narrower than "arbitrary text is dangerous", and the narrow
version is the one that holds. Executed against `internal/lyrics/parser.go`, only
a NEWLINE is an actual corruption vector:

- A newline splits the tag across two physical lines, and the FIRST fragment
  (`[upstream:<text-before-the-newline>`) has no closing `]`, so `parseTagLine`
  rejects it on the suffix check. The header-block loop in `ParseLRC`
  (`parser.go:93-113`) sets `inHeader = false` on the first non-tag, non-blank
  line, so that fragment and every real tag written after it are read as lyric
  lines rather than header tags.
- An interior `:` is harmless. `parseTagLine` (`parser.go:125-144`) takes the
  FIRST colon (`strings.IndexByte(inner, ':')`) as the key/value split and the
  whole remainder as the value, so `[upstream:a:b]` parses with value `a:b`.
- An interior `]` is harmless too. The function requires the `[`/`]` prefix and
  suffix and then slices off the last byte (`inner := s[1 : len(s)-1]`), which
  tolerates a `]` inside the value.

The sanitizer does not cover this write path, which is why the closed set carries
the weight. `sanitizeTagValue` (`parser.go:146-153`) strips `]`, CR and LF --
deliberately not `:` -- but its only callers are the four tag values in
`InjectProvenance` (`parser.go:205-208`). The fetch-time writer formats the token
straight into the header with `fmt.Sprintf("[source:%s]", ...)`
(`internal/lyrics/writer.go:290-292`, and the instrumental branch), unsanitized.
A closed set means the writer never has an unsanitized third-party string to
format in the first place.

A closed set also keeps the token stable if the upstream renames itself in the
API, which is the reason that survives independently of any parser detail.

### 4. `provenanceAgrees` holds, unchanged

Shown with the concrete strings on both sides. `tag` is the value read from the
sidecar's `[source:]`; `lane` is `work_queue.provider_lane`. Both are lowercased
and trimmed by the function before comparison.

| Sidecar | `tag` | `lane` | Branch taken | Result |
| --- | --- | --- | --- | --- |
| InnerTube, upstream A | `"innertube"` | `"innertube"` | `tag == lane` | agrees |
| InnerTube, upstream B | `"innertube"` | `"innertube"` | `tag == lane` | agrees |
| InnerTube, upstream unknown | `"innertube"` | `"innertube"` | `tag == lane` | agrees |
| InnerTube cache hit | `""` | `"innertube"` | empty tag | agrees |
| Any pre-existing sidecar | unchanged | unchanged | unchanged | unchanged |

The guard holds because **the `[source:]` token does not vary with the upstream**
-- that invariance is the entire reason the upstream was put in its own tag
rather than folded into this one. No new asymmetry case, no schema change, no
migration, and no backfill: existing sidecars carry no `[upstream:]` tag and are
correctly read as making no upstream claim.

The `--source` filter follows from the same invariance. `--source innertube`
matches every InnerTube-served sidecar regardless of upstream (exact equality on
an unchanged token), and `--source <A>` matches only sidecars the direct lane
wrote -- it does not sweep in InnerTube-routed files. That is harm 2 above,
avoided by construction rather than by a guard.

### 5. Reports and metrics do NOT split per upstream

Decided, not deferred: `ProviderEffectiveness` keeps grouping by `lane`, and
`mxlrcgo_provider_hits_total` / `_misses_total` keep exactly the `lane` label they
have. One row and one label for the InnerTube lane, aggregating both upstreams.

Cost of this choice: an operator reading `/metrics` or Report 3 cannot see the
upstream mix, and cannot detect a shift in the multiplexer's routing from
telemetry alone.

Cost of the alternative: a second dimension on a counter family operators already
scrape and alert on (label churn breaks existing recording rules); a
`lane_attempts` migration for rows whose semantics the upstream does not satisfy
(above); and a report whose headline number would be misleading -- the actionable
unit for a human reading provider effectiveness is the lane, because the lane is
what the breaker trips, what the pacer paces and what config can reorder or turn
off. Nobody can act on a per-upstream hit rate.

The trade is asymmetric, so this is a clear no. If demand for the split appears,
the cheapest reversible path is an ADDITIONAL label on the existing counter
(`{lane="innertube",upstream="<A>"}`) rather than a second lane or a
`lane_attempts` column -- that keeps one breaker, one config knob and one report
row while adding the dimension. That is a follow-up issue when there is a
measured need, not work for slice G.

### 6. No DB column in this slice

`work_queue` gets no `provider_upstream` column now. The reports and metrics
decision above is the only consumer that would have wanted one, and it was decided
against.

This is NOT the "nothing reads it" argument, because the accepted design has the
same property: the `Upstream` field and `case "upstream":` in the checklist below
give the tag a reader in `ReadProvenanceTags`, and nothing in the tree calls that
reader for the upstream either. Today only two non-test callers read provenance
tags at all -- `purgeprovenance.go:268` (which reads `.Source`) and
`realign.go:258` (ISRC, MBID, artist, title). Rejecting the column on "no reader"
while accepting a tag on the same terms would be incoherent.

The distinction that does hold is DURABILITY, and it is the whole argument. A tag
is a line of text in the user's own file: it is readable without Canticle, by a
person opening the sidecar, by another program, by a future version of this tool,
and it survives the database being deleted and rebuilt. A column is readable only
through code that queries it, in this tool, for as long as this schema lives. The
tag is a record; the column would be a cache of a record. So the tag is written
NOW, deliberately, for readers that do not exist yet, and the column is not.

The `[upstream:]` tag on disk is therefore the record of attribution, which is what
the acceptance criteria ask for. Adding the column
later is a nullable-TEXT migration exactly like the one that introduced
`provider_lane` (migration 018), and the trigger that would justify it is the
metrics split being reopened.

### 7. No mark for this lane

Settled and closed, per #601 and `docs/provider-terms.md`: no provider logo or
mark is vendored for InnerTube or for either upstream. `laneMark`
(`internal/web/lanemark.go`) returns `markNone` for an unmapped lane and the
template degrades to the display name alone, so a lane without a mark renders as
deliberate text rather than as a gap. Nothing to build.

## What the implementer builds (slice G)

Checklist form, for the adapter issue:

- [ ] `models.Song.Upstream string \`json:"-"\`` with a doc comment stating the
      cache-hit and no-multiplexer cases.
- [ ] The InnerTube lane maps its response's attribution field through a closed
      switch to a package constant -- `musixmatch` for `<A>` and `lyricfind` for
      `<B>`, lowercase and trimmed -- with a default arm yielding the empty
      string, so an unrecognized, renamed or absent attribution writes no
      `[upstream:]` tag at all.
- [ ] `internal/lyrics/writer.go` appends `[upstream:%s]` directly after the
      `[source:]` append, guarded on `song.Upstream != ""`, in both the tagged
      branch and the instrumental branch.
- [ ] `internal/lyrics/parser.go` `ProvenanceTags` gains an `Upstream` field and a
      `case "upstream":` in `ReadProvenanceTags`, so a reader can retrieve it.
      `InjectProvenance` is NOT extended -- backfill does not invent attribution
      it never had.
- [ ] A test asserting `provenanceAgrees("innertube", "innertube")` is true for
      each upstream's written sidecar, and that a `--source <A>` filter does not
      match an InnerTube-written sidecar.
- [ ] A test asserting a response carrying header-breaking characters in the
      attribution field writes no `[upstream:]` tag.
- [ ] No change to `lane_attempts`, `provider_outcomes`, `reports.go`,
      `metrics.go`, `lanemark.go`, or any migration.
