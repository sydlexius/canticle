# Provider terms and attribution requirements

Per-provider record of whether the lyrics sources canticle consumes require attribution
as a condition of use, and what else their terms impose. Tracked by #600.

**This file exists so the question is not re-derived from memory.** Record the finding
even when the answer is "not required", with the date it was checked and a link to the
document that says so. Terms change; a finding is only as good as its date.

**Status: BOTH PROVIDERS READ, OBLIGATIONS OUTSTANDING.** Musixmatch's terms require a
prescribed credit and linkback that canticle does **not** currently satisfy (#600).
petitlyrics imposes no attribution requirement but does constrain use. Do not read this
file as a compliance sign-off -- it is a record of what the terms say, not a claim that
canticle complies with them.

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

**Checked:** 2026-08-16
**Source:** <https://www.musixmatch.com/apiterms/>, which redirects to
<https://about.musixmatch.com/apiterms> (Musixmatch API Terms of Service)

An earlier revision of this file recorded these terms as unreachable and therefore
UNKNOWN. That was a tooling limitation, not a property of the terms: the page is served
from `about.musixmatch.com`, which the first attempts never tried. Surfaced by CodeRabbit
on PR #766.

### Attribution IS required, with prescribed wording and placement

| Clause | Obligation |
|---|---|
| **2.1.5** | Credit Musixmatch and link to the Site **each time** you use Musixmatch Data, by linking one of the "powered by Musixmatch" buttons published at <https://www.musixmatch.com/resources> |
| **2.1.6** | Comply with the Musixmatch Brand Guidelines (<https://brand.musixmatch.com>) |
| **2.1.11** | Allow Musixmatch to track your access to the API and the Data |
| **3.2** | Musixmatch trade marks, logos and graphics may be used **only** to inform third parties that the Data originates from Musixmatch |

Three further terms constrain the deployment rather than the mark, and are recorded
because they are easy to trip over:

- **1.1** grants a non-exclusive, non-sublicensable license for **non-commercial** use only.
- **2.1.4** requires **prior written approval** before making web pages containing
  Musixmatch Data publicly accessible. canticle's serve-mode UI displays lyrics; an
  operator exposing it publicly is squarely in scope.
- **2.2.4** prohibits scraping or harvesting other than by accessing the API with the
  User Authentication Key.

### Does this apply to the desktop endpoint? Assume yes.

canticle does not call the documented developer API. It calls
`apic-desktop.musixmatch.com/ws/1.1/macro.subtitles.get`
(`internal/musixmatch/client.go:21`) with a token minted from `.../ws/1.1/token.get`
(`token.go:17`) -- the endpoint the Musixmatch desktop application uses internally.

An earlier revision leaned on that distinction to argue the published terms "govern an
API canticle does not use". **That argument does not survive reading the definitions.**
Clause 8.1 defines `API` functionally -- "the Musixmatch application programming
interface that supports requests for Musixmatch Data made of it by computer programs" --
and `Musixmatch Data` as any data or content made available by Musixmatch, explicitly
including time-synced lyrics. Neither definition is scoped to a documented product,
a particular hostname, or a registration tier. The desktop endpoint satisfies both.

So the terms are the governing document unless Musixmatch says otherwise in writing.
Written confirmation would still be worth having, but its absence is not a reason to
treat the obligations as inapplicable -- that reads the ambiguity in our own favor,
which is the wrong default for a compliance question.

### What is NOT satisfied today

The lane mark added in #601 is a **static, non-linking `<img>`**. Clause 2.1.5 asks for
a linked "powered by Musixmatch" button from the published resources page; clause 3.2
permits the logo only to indicate data origin. A silent logo in a lane column arguably
indicates origin, but it is not the prescribed asset and carries no link.

Nothing here is a reason to delay a UI change: **the obligation attaches to using the
Data, which canticle already does today**, and it existed before the mark and would
survive its removal. Tracked in #600.

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

**The brand page suggests linking the badge to musixmatch.com, and the API Terms turn
that suggestion into a requirement** (clause 2.1.5 -- see above). canticle's lane mark is
currently a static, non-linking `<img>`, so it does not satisfy 2.1.5 and is not one of
the prescribed "powered by Musixmatch" buttons.

An earlier revision of this section called the linkback "a suggestion, not a
requirement", on the strength of the brand page alone while the Terms were unread. That
was wrong, and it is left visible here rather than quietly corrected: the brand page is
not the governing document.

The remedy is to add the prescribed credit surface, not to remove the mark -- the
obligation attaches to using the Data, which canticle already does. Tracked in #600.

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
