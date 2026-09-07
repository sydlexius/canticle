# Provider terms and attribution requirements

Per-provider record of whether the lyrics sources Canticle consumes require attribution
as a condition of use, and what else their terms impose. Tracked by #600.

**This file exists so the question is not re-derived from memory.** Record the finding
even when the answer is "not required", with the date it was checked and a link to the
document that says so. Terms change; a finding is only as good as its date.

**Status: THREE PROVIDERS RECORDED. CREDIT IMPLEMENTED FOR MUSIXMATCH'S DIRECT API, SOME
OBLIGATIONS OUTSTANDING. BOTH INNERTUBE UPSTREAMS NOW READ.**
Musixmatch's required credit and linkback now render across the authenticated serve UI, using the
official brand mark rather than the exact prescribed button (which is not published in a
usable format -- see below). Clauses 2.1.11, 2.1.4 and 1.1 remain unaddressed.
petitlyrics imposes no attribution requirement but does constrain use.
InnerTube multiplexes between two upstream licensors per track (LyricFind and Musixmatch reached
through Google's own license). BOTH were read in full on 2026-09-07. Each binds on ACCESS to that
company's own site, application or services, which this lane never touches, and neither imposes an
attribution requirement. Silence is the finding, not permission: what authorizes YouTube Music to
serve the text is a Google-to-licensor arrangement Canticle cannot read. See the InnerTube section
below.

Do not read this file as a compliance sign-off -- it is a record of what the terms say and
what Canticle does about them, reviewed by an agent rather than by counsel.

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

Three of those touch what Canticle actually does and should be assessed deliberately
rather than assumed benign:

- **Writing `.lrc` / `.txt` sidecars** is a reproduction to durable storage. A
  personal-use reading likely covers a user's own library; that reading has not been
  confirmed.
- **The tier-2 (LSY) payload is obfuscated**, and #602 shipped a decoder for it. Whether
  that engages the anti-circumvention clause is a question for the maintainer, not for
  this file. Recording it because it is exactly the kind of thing that is cheap to note
  now and expensive to discover later.
- **Non-commercial** is a condition on the deployment, not on the software. It does not
  constrain Canticle as a project, but it does constrain how an operator may use it.

### Endpoint

Canticle uses the synced-lyrics API. A `clientAppId` is required (see #607, which exists
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
  Musixmatch Data publicly accessible. Canticle's serve-mode UI displays lyrics; an
  operator exposing it publicly is squarely in scope.
- **2.2.4** prohibits scraping or harvesting other than by accessing the API with the
  User Authentication Key.

### Does this apply to the desktop endpoint? Assume yes.

Canticle does not call the documented developer API. It calls
`apic-desktop.musixmatch.com/ws/1.1/macro.subtitles.get`
(`internal/musixmatch/client.go:21`) with a token minted from `.../ws/1.1/token.get`
(`token.go:17`) -- the endpoint the Musixmatch desktop application uses internally.

An earlier revision leaned on that distinction to argue the published terms "govern an
API Canticle does not use". **That argument does not survive reading the definitions.**
Clause 8.1 defines `API` functionally -- "the Musixmatch application programming
interface that supports requests for Musixmatch Data made of it by computer programs" --
and `Musixmatch Data` as any data or content made available by Musixmatch, explicitly
including time-synced lyrics. Neither definition is scoped to a documented product,
a particular hostname, or a registration tier. The desktop endpoint satisfies both.

So the terms are the governing document unless Musixmatch says otherwise in writing.
Written confirmation would still be worth having, but its absence is not a reason to
treat the obligations as inapplicable -- that reads the ambiguity in our own favor,
which is the wrong default for a compliance question.

### Credit surface: IMPLEMENTED, with one gap

A credit footer renders on every page of the authenticated serve UI -- Dashboard,
Reports, Settings and Keys, i.e. every page built on the shared `Layout`
(`musixmatchCredit` in `web/templates/layout.templ`): the official For Brands mark, the
text "Lyrics powered by Musixmatch", and a link to <https://www.musixmatch.com>. It sits
OUTSIDE `#mx-main` so an htmx report-rail swap cannot remove it -- a credit that survives
only until the first navigation would not be an each-time-you-use-the-Data credit.

**The login and setup pages carry no credit, deliberately.** They build their own shell
rather than using `Layout`, and they display no Musixmatch Data -- they are auth forms.
The obligation attaches to USING the Data, so a credit there would assert an attribution
on a page where nothing is attributable. Recorded because "every page" is the intuitive
reading of the requirement and the exception is easy to mistake for an oversight.

It is **hidden when Musixmatch is inactive** (no usable token). Crediting Musixmatch for
results another provider served would be a misattribution, so the absence is load-bearing
too, and is asserted by its own test.

Verified on the rendered page rather than from the stylesheet: computed color
`rgb(148,163,184)` on `rgb(11,17,32)` gives a **7.34:1** contrast ratio (clears WCAG AA).
A required credit that is present in the DOM but visually unreadable would not discharge
the obligation, which is why this is measured rather than assumed.

**The remaining gap.** Clause 2.1.5 names a specific "powered by Musixmatch" button
published at <https://www.musixmatch.com/resources>. That URL **redirects** to the
brand-resources page, and the only badge set there ("For Brands" -> With Claim) ships
**PNG only** -- no SVG, and no literal "powered by" wordmark asset was reachable. So this
implements the clause's SUBSTANCE (credit + mark + link to the Site) using the official
For Brands mark, not the exact prescribed button.

To close it fully: obtain the prescribed badge from Musixmatch and swap the `<img>` in
`musixmatchCredit`. The link target, the placement, and the tests are already correct, so
it is an asset swap rather than a redesign.

### Still outstanding

- **2.1.11** (permit Musixmatch to track API access) is not implemented. Whether the
  desktop endpoint already satisfies this by construction is unexamined.
- **2.1.4** (prior written approval before exposing pages carrying the Data publicly)
  binds the OPERATOR, not the software. Worth surfacing in user-facing docs so an
  operator publishing their instance knows it applies to them.
- **1.1** limits use to non-commercial.

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
that suggestion into a requirement** (clause 2.1.5 -- see above). Canticle's lane mark is
currently a static, non-linking `<img>`, so it does not satisfy 2.1.5 and is not one of
the prescribed "powered by Musixmatch" buttons.

An earlier revision of this section called the linkback "a suggestion, not a
requirement", on the strength of the brand page alone while the Terms were unread. That
was wrong, and it is left visible here rather than quietly corrected: the brand page is
not the governing document.

The remedy is to add the prescribed credit surface, not to remove the mark -- the
obligation attaches to using the Data, which Canticle already does. Tracked in #600.

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

## InnerTube (YouTube Music internal API)

**Checked:** 2026-09-07
**Source:** Google publishes no terms specific to the innertube API itself -- no
documentation, no rate-limit response headers, and no `robots.txt` covering it (see the
policy note in `internal/innertube/pacer.go`). This section instead records the terms
position for the two upstream lyric licensors the API multiplexes between, per
`docs/provider-attribution.md` (#850).

Canticle never calls either upstream directly through this lane; it only calls the
InnerTube endpoint, which internally routes each track to one of at least two upstream
providers and returns whichever it chose. That routing decision belongs to Google /
YouTube Music, not to Canticle, and it varies per track: the #848 spike measured, across
four reference tracks, an even split between the two upstreams. The terms below describe
what governs the content reaching Canticle through this lane, even though Canticle never
reaches those upstreams' own endpoints or agrees to their developer terms directly.

### Upstream A: LyricFind

**Checked:** 2026-09-07
**Source:** LyricFind Terms and Conditions, `https://www.lyricfind.com/terms-and-conditions`.
Read in full on the date above. The page carries NO last-updated or effective date, so
this reading cannot be pinned to a document version -- only to the date it was read. The
terms do provide for amendment on 30 days' posted notice, so a later reader should
re-check rather than assume this still holds.

**SCOPE, AND IT IS THE WHOLE FINDING.** The terms open by defining themselves as
governing use of "this website, www.lyricfind.com", and they bind by ACCESS: the user
agrees by accessing the Site on a device. They are a website agreement, front to back.

**Canticle never accesses that website.** This lane calls Google's InnerTube endpoint;
LyricFind's servers are never contacted, no LyricFind page is loaded, and no Canticle
code path touches their domain. On the terms' own trigger, they do not attach to
Canticle at all. That is a stronger and more precise statement than the earlier draft's
"appear not to reach this use", and it is what the document actually says.

What the terms contain, relevant to this lane:

- **Intellectual property**: material on the website -- design, layout, look, appearance,
  and graphics -- is owned by or licensed to LyricFind, and reproduction is prohibited
  unless otherwise stated. The enumeration is explicitly non-exhaustive.
- **Termination**: LyricFind reserves the right to block IP addresses and prevent access
  to the Site at its discretion. Not applicable while Canticle does not contact them, but
  worth knowing the remedy they name.
- **Governing law**: Canada (Ontario), with binding arbitration through the Canadian
  Arbitration Association in Toronto and a liability cap of US $100.

What the terms do NOT contain, checked for specifically:

- **No attribution or crediting requirement**, in any form, for lyrics or otherwise.
  Absent entirely rather than waived.
- **No third-party application, API, or developer terms.** Nothing addresses a consumer
  reaching their catalog indirectly through a platform that licenses from them.
- **No redistribution or caching language for lyric TEXT.** The IP clause enumerates site
  presentation material and does not name lyrics.

**DO NOT READ THAT LAST ABSENCE AS PERMISSION.** Lyric text is copyrighted by music
publishers, and LyricFind is a licensor of it rather than its owner; a website's terms
being silent on redistribution says nothing about the underlying copyright, which is not
LyricFind's to waive in this document and is not addressed here. The finding is narrow
and should stay narrow: these particular terms impose no attribution duty on Canticle and
do not reach this access pattern. Whether writing a lyric to a user's own disk is
permissible rests on copyright and on Google's license, neither of which this file
assesses.

**This is why no mark or icon is vendored for LyricFind anywhere in Canticle**, following
the same policy already applied to petitlyrics (#601). The IP clause covers the site's
graphics, and no third-party developer terms authorizing use of their branding exist to
grant an exception, so `laneMark` renders this lane as text only regardless of which
upstream served a given result -- never a LyricFind icon. See "No new mark for this lane"
in `docs/provider-attribution.md`.

### Upstream B: Musixmatch, reached through Google's license

**Checked:** 2026-09-07

Canticle already has a direct Musixmatch integration (`internal/musixmatch`), and the
Musixmatch section above analyzes that integration's terms against the direct desktop-API
endpoint it calls. When InnerTube's routing sends a track to Musixmatch as the upstream
licensor, Canticle reaches Musixmatch's catalog through Google's own licensing
relationship with Musixmatch, not through Canticle's Musixmatch integration at all -- no
Musixmatch endpoint is called, no Musixmatch token is used, and no Musixmatch code path
executes for that fetch.

**READ 2026-09-07, and the answer is SILENCE.** Source: the Musixmatch EULA page at
`https://about.musixmatch.com/eula`, which carries TWO agreements, both read in full:

- **Consumer EULA, last updated February 2025.** By its own words it applies where you
  access and use their Website, Application or Services "on a consumer basis", and the
  examples it gives are all direct use of Musixmatch's own properties: searching and
  viewing lyrics there, creating an account, buying premium, contributing to their
  catalog.
- **Business-to-business terms, last updated July 2025.** These apply to publishers,
  labels and distributors who SUPPLY lyrics and metadata TO Musixmatch, which is the
  opposite direction from this lane.

Neither one addresses a third party reaching Musixmatch content through a partner
platform's license. Searched for specifically and ABSENT from both: any clause about
content reached indirectly, via a licensee, or through a partner service. The documents'
"third party" clauses run the OTHER WAY -- clauses 3.2 and 3.3 disclaim Musixmatch's
responsibility for third-party content reached THROUGH Musixmatch, not the reverse.

Both agreements bind on ACCESS to Musixmatch's own Website, Application or Services. On
this lane Canticle accesses none of them: no Musixmatch endpoint is called, no account
exists, and no Musixmatch code path runs. As with LyricFind, the agreement does not attach
on its own trigger.

The restrictions that WOULD bite are scoped to that same access. Clause 6.6 grants a
limited, revocable license to display Musixmatch-provided content, lyrics included, for
personal non-commercial use and forbids copying, reproducing or distributing it "as part
of the Services"; clause 1.5(a) forbids using a robot, scraper or crawler against their
Website or Application; clause 1.8 reserves text-and-data-mining rights. Each is a
restriction on using MUSIXMATCH'S properties, and none reaches a fetch that never touches
them.

**No attribution requirement appears in either agreement.** That is worth stating plainly
because it differs from the direct-API terms recorded above, which DO impose a credit and
linkback (clause 2.1.5). Those API terms govern Canticle's own relationship with
Musixmatch as an API consumer, and that relationship is not what this lane uses.

**SILENCE IS THE FINDING, NOT PERMISSION.** These documents do not address this path, so
they impose nothing on it; that is a verified negative rather than an unknown. What it
does NOT establish is that the path is unencumbered. Lyric text is copyrighted by
publishers, Musixmatch is a licensor rather than the owner, and whatever authorizes
YouTube Music to serve that text is a Google-to-Musixmatch agreement Canticle is not party
to and cannot read. The compliance question rests there and on copyright, neither of which
this file assesses.

**Historical note, superseded:** before the reading above, this section recorded the
posture as unassessed and genuinely unknown. It is no longer unknown. The distinction
matters and is the reason the note is kept: "we read them and they do not cover this" is a
finding, where "we could not check" is a gap, and the two should never be confused in a
file whose purpose is recording what was actually verified.

What is known, and is a separate question from the terms question: `[upstream:musixmatch]`
is the tag written to the sidecar in this case, never `[source:musixmatch]` -- the
`[source:]` tag stays `innertube` regardless of which upstream served the result (see
`docs/provider-attribution.md`). The on-disk record is honest about the routing even
though the compliance question above remains open.

### Combined status

**Both upstreams' published terms were read in full on 2026-09-07, and neither reaches
this lane.** Each binds on ACCESS to that company's own website, application or services,
and this lane accesses neither: it calls Google's InnerTube endpoint and nothing else.
Neither imposes an attribution requirement. That is a verified negative, not an
assumption, and it is a change in kind from the earlier record, which had both marked
unassessed.

Do NOT extrapolate from it. In particular, do not assume the direct-API Musixmatch clauses
recorded above (2.1.5 credit, 1.1 non-commercial) apply here -- they govern a relationship
this lane does not use -- and do not read either silence as permission. What authorizes
YouTube Music to serve this text is a Google-to-Musixmatch and Google-to-LyricFind
arrangement that Canticle is not party to and cannot read, and the lyrics themselves are
publishers' copyright. **The unread agreement is the one that matters, and it remains
unread.** This file settles what the two licensors' PUBLISHED terms require of Canticle;
it does not settle whether the path is unencumbered.

No mark is vendored for either upstream through this lane (`laneMark` returns `markNone`
for `innertube`); the lane always renders as plain text, regardless of which upstream
served a given track.

---

## LRCLIB

Not yet consumed. Check if #472 lands.
