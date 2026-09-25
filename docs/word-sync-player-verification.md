# A2 Player Verification

`output.word_sync_mode` can put per-word ("A2" / Enhanced LRC) timing markers into
a `.lrc` file (`replace`; see [Configuration](CONFIGURATION.md#output)).
Support for those markers is not universal, and the failure mode is three-way,
not two-way: a player can render them, silently ignore them, or display them as
literal text mixed into the lyrics. Only the third is visibly broken. This page
is the procedure for finding out which of the three your player does, before you
turn `replace` on for a whole library.

If you only want word timings kept without touching the `.lrc` at all, use the
default `both` mode instead: it writes the markers to a companion `.elrc`
file and leaves every `.lrc` exactly as it would be with word sync off. Nothing
in this procedure applies to `both` or `off` -- there is nothing in the `.lrc`
for a player to misinterpret. (`sidecar` and `inline` are deprecated aliases
for `both` and `replace`, #1072.)

## 1. Get a word-synced track

Word-level timings come from two lanes today: Musixmatch (via its bundled
richsync call, most commonly on synced results) and Petit Lyrics' word-synced
tier. Not every track that has line-synced lyrics also has word timings, so
picking a track at random from your library and getting no markers at all does
not mean anything is broken -- it may simply mean neither lane served word data
for that track.

Set `output.word_sync_mode` to `replace`, then fetch or re-fetch a single album. Confirm at least one resulting
`.lrc` actually carries `<mm:ss.cc>` markers before moving on -- that is your
positive sample.

Note that a line whose words all share one timestamp is refused for markers by
design (see `a2Words` in `internal/lyrics/a2.go`): that line falls back to a
plain line-level cue instead of claiming per-word detail the data does not
carry. On one verified word-synced track, 10 of 86 lines fell into this
category, so it is normal and expected for some lines in your sample `.lrc` to
have no markers even though the file overall is word-synced. Confirm those
lines still play as ordinary line-synced cues, not as broken output -- that
fallback path is part of what this procedure is verifying.

## 2. Play it in your target player and classify the result

Copy the `.lrc` (and its audio) to wherever your player reads from, play the
track, and watch the lyric display. First confirm the player shows lyrics at
all for an ordinary line-synced `.lrc`. If no lyrics appear, the test is
inconclusive: fix how the player finds the file before judging A2 support.
Once lyrics load, there are exactly three outcomes:

- **Highlights per word.** The player understands A2. This is the intent of
  `replace`, and you can enable it for that player's library.
- **Markers silently absent, plain line-sync shown.** The player does not
  understand A2 but degrades gracefully. Harmless: `replace` costs you
  nothing extra here, but it also does not do anything the default `both`
  mode's clean `.lrc` did not already do. You may as well use `both`.
- **Markers rendered as literal text**, e.g. `<00:12.34>word` showing up in the
  displayed lyric line. Visibly broken. Do not enable `replace` for this
  player. Use `both` or `off` instead.

## 3. Optional pre-check: lrcsong.com

[lrcsong.com](https://lrcsong.com) accepts A2-format `.lrc` files and will tell
you whether the file itself parses as well-formed A2. Use it only to separate
"our file is malformed" from "this player lacks support" before you spend time
blaming a player. It cannot substitute for step 2: by its own documentation it
highlights at line granularity, so a file that fails there is informative, but a
file that passes there tells you nothing about whether a given player renders
word-level detail.

## What is known so far

No player in this table is a verified positive control (confirmed to render A2
word-level highlighting). Confidence varies sharply between rows; read the
confidence column, not just the verdict.

| Player | Verdict | Confidence |
|---|---|---|
| Music Assistant | does not support | solid -- maintainer's own bug report, 2026-07-15 |
| Symfonium | strips markers (expect clean line-sync) | forum-sourced, ~3 years stale, unverified against current build |
| Emby | unknown | nothing found |
| foobar2000 | UNVERIFIED | see below |

foobar2000 has no built-in lyrics display; lyrics come from the third-party
`foo_openlyrics` component, whose README never mentions word-level, Enhanced
LRC, karaoke, or angle-bracket timestamps, and whose [component
page](https://www.foobar2000.org/components/view/foo_openlyrics) lists
supported systems as Windows 32-bit and Windows 64-bit only -- it is not an
option on foobar2000 for Mac at all. An earlier claim that foobar2000 was a
confirmed-supporting positive control was wrong: it traced back to a format
guide's general compatibility list, a secondary source about the LRC format in
general, not evidence about this specific plugin.

If you verify a player against this procedure, the result belongs in this
table (with its confidence noted) rather than assumed elsewhere.

## What this procedure does not cover

There is no automated (e.g. browser-driven) classifier for this, and none is
planned: the players above are native desktop/mobile/server clients, not
browser surfaces, and the one web tool available (lrcsong.com) is disqualified
as a positive control for the reason given in step 3. A synthetic audio
fixture built from the same timings a renderer emits was tried and discarded as
tautological -- it can only prove the renderer agrees with itself, not that a
real player supports A2. There is no shortcut around actually playing a
word-synced file in the target player.

## Privacy note

If you share a result from this procedure (an issue, a forum post, a
screen recording), use a public-domain or synthetic-text track. Do not post
`.lrc` content, screenshots, or descriptions that reveal titles from a private
library.
