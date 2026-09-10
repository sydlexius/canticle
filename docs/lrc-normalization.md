# LRC Normalization

Canticle writes and maintains **expanded** LRC: one timestamp per line. This
page explains what that means, why it matters, and what to do if the
dashboard or a startup log tells you stacked files exist.

## The problem: stacked timestamps

The LRC format allows one line to carry several timestamps when the same text
repeats at multiple points in a song (a repeated chorus, for example):

```
[00:30.00][01:45.00][02:50.00]Chorus line here
```

This is valid LRC, but many simple player parsers only read the **first**
timestamp on a line. The rest of the bracketed stamps on that line are then
rendered as literal text in the lyric display instead of triggering their own
highlighted cue - a chorus line looks fine the first time it plays, then shows
up with stray `[01:45.00]` characters glued to the front on every repeat.

The expanded form gives each timestamp its own line:

```
[00:30.00]Chorus line here
[01:45.00]Chorus line here
[02:50.00]Chorus line here
```

Every LRC parser, simple or sophisticated, reads this correctly. Expanded LRC
is a strict superset of what any parser understands - there is no format a
stacked line works with that an expanded one does not.

## What Canticle does now

Canticle always writes the expanded form. The LRC-text provider lanes expand
stacked timestamps at parse time, so every file Canticle writes from here on
is already one-cue-per-line - there is no setting to turn this on or off, and
no reason to want the compressed form.

That covers new fetches. It does not, by itself, touch `.lrc` files that
already existed on disk before this behavior shipped - an inherited library
can still have compressed lines in it. Fixing those is a separate,
explicit step.

## The detect/apply split

This is the part most worth understanding, because the two halves look
similar and are not the same:

- **The serve-mode startup check detects and reports. It never rewrites
  anything.** Once per database, Canticle walks your configured library roots
  looking for `.lrc` sidecars that still carry a stacked line, and logs how
  many it found. That is the entire scope of the startup pass - it calls the
  same walk the CLI uses, but in report-only mode, so it never writes a
  single byte to your library.
- **The CLI applies.** `canticle scan reconcile-lrc --yes` is the only thing
  that ever rewrites a file. Run it without `--yes` first to see what it
  would do:

  ```sh
  canticle scan reconcile-lrc
  ```

  and with `--yes` to actually rewrite the stacked files it found:

  ```sh
  canticle scan reconcile-lrc --yes
  ```

If a startup log or the dashboard summary line tells you "N stacked files"
were found, that count is a notification, not a completed action. Nothing on
disk has changed yet - you still need to run the CLI with `--yes` to fix it.

Because the fix is gated on a needs-work check (a file with no stacked line is
left alone), `scan reconcile-lrc --yes` is safe to run repeatedly: an
already-clean file costs a read and nothing else, and a rewritten file will
not be rewritten again.

## The `.lrc.orig` backup

Before rewriting a stacked `.lrc`, Canticle writes the pristine original
alongside it as `<file>.lrc.orig` - for example, rewriting
`Artist/Album/Track.lrc` also produces `Artist/Album/Track.lrc.orig`. That
backup is written and fsynced to disk *before* the rewrite happens, so a
crash mid-run can never leave a rewritten file without its undo copy.

A few things worth knowing about `.orig` files:

- They are never overwritten. Once a `.lrc.orig` exists for a file, Canticle
  will not touch it again on a later pass.
- They are yours. Nothing in Canticle reads them back automatically. Keep
  them if you want an undo trail, or delete them once you have confirmed the
  rewritten `.lrc` looks right - either is safe.
- If you see a `.lrc.orig` next to your lyrics, it is evidence of a
  deliberate, backed-up rewrite, not debris left behind by something going
  wrong.

## Why the startup check never names files

The startup detection pass logs a count, never a path. This is deliberate: a
sidecar's path encodes `<library root>/<Artist>/<Album>/<Title>.lrc`, which
is private library metadata about what you listen to. An unattended server
log is not a safe place for that, so the startup check reports only
aggregate numbers.

If you need to know *which* files are affected, run the CLI without `--yes`.
Unlike the startup check, the CLI is something you invoked yourself to look
at your own library, so it prints per-file detail as it works.

## Skipped, blocked, and errored files

Not every stacked file the walk finds is rewritten cleanly. Three states are
worth knowing:

- **Skipped** - the sidecar is a symlink. Canticle never follows or rewrites
  a symlinked `.lrc`; it is left untouched and counted separately so a
  nonzero skip count does not silently masquerade as "nothing to do."
- **Blocked** - the file is still stacked, but a `.lrc.orig` already exists
  next to it. Canticle cannot tell whether that pre-existing backup is a
  trustworthy pristine copy, so it declines to overwrite the `.lrc` rather
  than risk losing data. This usually means a concurrent run or manual
  interference; compare the two files, then remove or rename the stray
  `.orig` and re-run to clear it.
- **Errored** - the file could not be read or rewritten. The one bounded case
  worth naming: a `.lrc` larger than 16 MiB is refused outright rather than
  read into memory. A real lyric sidecar is a few KB, so a file that size is
  corrupt, wrongly named, or not a lyric file at all, and this pass may be
  running inside a long-lived server that should not try to load it. Other
  read or write failures (a permissions problem, a file that vanishes
  mid-run, a full disk) land here too. The run continues past any of them;
  the count tells you how many files were left unjudged.

`scan reconcile-lrc` (dry run, without `--yes`) is how to see exactly which
files fall into any of these categories - it prints the path and reason for
each one it could not simply expand and leave clean.

## Reading the dashboard summary

The web dashboard shows a line summarizing the most recent *applied*
`scan reconcile-lrc --yes` pass: how many files it rewrote, and when.
Because it only tracks applied passes, a deployment that has only ever
booted (and had the startup check merely detect stacked files, never fix
them) reads as "no LRC normalization pass has run yet" - even if the startup
log already told you stacked files exist. That is expected: the dashboard
line is asking "when did the fix last run," not "was the problem last seen."
Running `canticle scan reconcile-lrc --yes` is what moves it out of that
state.
