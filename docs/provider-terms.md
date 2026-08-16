# Provider terms and attribution requirements

Per-provider record of whether the lyrics sources canticle consumes require attribution
as a condition of use, and what else their terms impose. Tracked by #600.

**This file exists so the question is not re-derived from memory.** Record the finding
even when the answer is "not required", with the date it was checked and a link to the
document that says so. Terms change; a finding is only as good as its date.

**Status: INCOMPLETE.** The petitlyrics half is recorded below. The Musixmatch half is
not established -- see the gap note. Do not read this file as a compliance sign-off.

---

## petitlyrics (SyncPower Corporation)

**Checked:** 2026-08-16
**Source:** <https://petitlyrics.com/contents/kiyaku> (Terms of Use, Japanese)

### Attribution

**No attribution, credit, or linkback requirement was found** for third-party
applications. The terms do not address third-party developer obligations at all -- there
is no developer-terms section, no prescribed credit wording, and no logo requirement.

Note what that does and does not mean: the absence of a stated requirement is not a
grant of permission. The terms are silent on third-party applications generally, which is
a different position from a public API whose terms contemplate developers and impose
conditions on them.

### Other conditions found, and they matter more than the attribution question

The terms limit use to **personal, non-commercial purposes**, and prohibit reproduction,
publication, transmission, distribution, public display, modification, derivative works,
and sale of lyric data. They separately prohibit transferring or providing lyrics to
third parties, and circumventing protection mechanisms.

Three of those touch what canticle actually does and should be assessed deliberately
rather than assumed benign:

- **Writing `.lrc` / `.txt` sidecars** is a reproduction to durable storage. A
  personal-use reading likely covers a user's own library; that reading has not been
  confirmed.
- **The tier-2 (LSY) payload is obfuscated**, and #602 shipped a decoder for it. Whether
  that engages the anti-circumvention clause is a question for the maintainer, not for
  this file. Recording it because it is exactly the kind of thing that is cheap to note
  now and expensive to discover later.
- **Non-commercial** is a condition on the deployment, not on the software. It does not
  constrain canticle as a project, but it does constrain how an operator may use it.

### Endpoint

canticle uses the synced-lyrics API. A `clientAppId` is required (see #607, which exists
because a revoked one fails silently rather than with a 401), which implies a
registration relationship whose terms may be separate from the public site terms above
and were not located.

### Lane mark: NONE, deliberately (#601)

petitlyrics publishes no brand kit and no SVG mark. Its site serves only a raster
site-header logo (`/images/logo/logo.png`, 233x120 PNG), and no third-party usage terms
for it were located.

**No mark is vendored, and the lane renders as text.** General icon sites do host
petitlyrics logos, but those are unauthorized redraws -- vendoring one would put an
asset of unknown provenance into the binary and ship it to every user. #601's degrade
path (name alone, no placeholder, no gap) covers this case exactly, which is what makes
declining the correct action rather than a missing feature.

To revisit: a mark could be sourced by asking SyncPower directly for a usable asset and
its usage terms. Do not resolve it by picking one off an icon site.

---

## Musixmatch

**Checked:** 2026-08-16 -- **NOT ESTABLISHED**

### The gap

Both `developer.musixmatch.com` and `www.musixmatch.com` are unreachable from this
environment, so the API terms at <https://www.musixmatch.com/apiterms/> could not be
read directly. Nothing here should be inferred from that failure.

Secondary sources indicate the documented developer API requires displaying a
**copyright notice and a tracking script** returned alongside the lyrics body. That is
second-hand and is recorded as a lead to verify, **not as a finding**.

### Why the published API terms may not be the governing document anyway

canticle does not use the documented developer API. It calls
`apic-desktop.musixmatch.com/ws/1.1/macro.subtitles.get`
(`internal/musixmatch/client.go:21`) with a token minted from
`.../ws/1.1/token.get` (`token.go:17`) -- the endpoint the Musixmatch desktop
application uses internally.

That distinction is material to #600 and was not visible from the issue body. The
published API terms govern the published API. What governs this endpoint is a separate
question, and the honest answer today is that it is unknown. It is worth resolving
before designing any credit surface, because the answer determines *which document* the
credit would be satisfying.

### To resolve

Read <https://www.musixmatch.com/apiterms/> from an environment that can reach it, and
record: whether a copyright notice is required, its prescribed wording and placement,
whether the tracking script is mandatory, and whether the terms address non-API endpoints.

### Lane mark: VENDORED (#601)

| | |
|---|---|
| **File** | `web/static/img/lanes/musixmatch.svg` |
| **Source** | <https://s.mxmcdn.net/site/images/mxm_icon.svg> (Musixmatch's own CDN, referenced by musixmatch.com) |
| **Retrieved** | 2026-08-16 |
| **Modified** | No. Used byte-for-byte as served. |

Taken from the provider's first-party CDN rather than from an icon site, per #601's
requirement to prefer the official asset. Note that Musixmatch **does** publish a brand
resources page at <https://about.musixmatch.com/brand-resources>, including a "For
Brands" badge set intended for third-party use with a suggested link to musixmatch.com.
Those assets sit behind Google Drive folders that need a browser to enumerate, so they
could not be retrieved here; the CDN mark above is the same company's own asset and is
the correct interim source.

**Worth checking when the brand kit is reachable:** the "For Brands" set may be the
intended asset for exactly this use, and the suggested musixmatch.com linkback may be a
condition attached to it. Neither the CDN file nor the brand page states explicit
third-party usage terms, so this is recorded as unverified rather than cleared.

**Rendering constraint this creates.** The mark is a single fixed fill (`#131313`) on a
`#0b1120` UI background -- near-black on near-black, unreadable as-is. It is NOT
recolored, because the guidelines that require an official asset generally forbid
altering it. The `.mx-lane-chip` treatment reconciles this instead by placing the mark on
a light plate, so it renders on the background it was drawn for. Do not "fix" this with a
CSS filter or by editing the file.

---

## LRCLIB

Not yet consumed. Check if #472 lands.
