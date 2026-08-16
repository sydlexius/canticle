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

---

## LRCLIB

Not yet consumed. Check if #472 lands.
