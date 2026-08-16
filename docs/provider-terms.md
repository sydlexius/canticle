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
| **Asset** | `Musixmatch-Icon-White-BG.svg`, from the **Musixmatch For Brands** set |
| **Source** | <https://about.musixmatch.com/brand-resources> -> "For Brands" -> Icon -> SVG |
| **Retrieved** | 2026-08-16 |
| **Modified** | Adobe metadata stripped. Artwork untouched -- see below. |

This is the provider's own brand kit, and the "For Brands" set is the one the page
describes as designed for third-party use ("these badges are designed to work with almost
any design"). That makes it the correct asset for a lane mark, rather than the site-header
icon on the Musixmatch CDN.

**The page suggests linking the badge to musixmatch.com.** It is phrased as a suggestion,
not a requirement, and canticle's lane mark is currently not a link. Recorded so the
decision is visible: if the API terms (unread, see above) turn a suggestion into a
condition, the mark should become a link rather than be removed.

### What "modified" means here, precisely

The asset as served is **396,427 bytes**, of which **394,960** are a single
`<metadata>` blob of Adobe Illustrator private data. The artwork is one `<rect>` and one
`<path>`. Embedding 396 KB into the binary to draw a 16px mark is not defensible, so the
metadata element, the generator comment, and the now-unused `xmlns:i` declaration were
removed. The result is **1,313 bytes**.

Nothing about the drawing was touched, and that is verified rather than assumed:

- the `<rect>`, the `<path>`, and the `viewBox` are **byte-identical** before and after
- both files were rendered at 256x256 in chromium and the resulting bitmaps hash
  identically (`ec1cc496...`), so the strip is provably lossless at the pixel level

If this asset is ever refreshed, redo both checks. Markup equality alone would not have
licensed the claim.

### Rendering note

The For Brands icon is a solid tile carrying its own background (`#fc532e` with the mark
in `#fff`), which is exactly why it works on any surface -- including this UI's dark
`#0b1120`. Do not add a background plate behind it, do not pad it, and do not recolor it;
the asset already solves the contrast problem the earlier site-header icon had.

---

## LRCLIB

Not yet consumed. Check if #472 lands.
