# Instrumental Detection

Canticle can optionally recognize that a track is **instrumental** (no lyrics to
find) by listening to the audio, so it writes an instrumental marker instead of
re-querying providers forever for lyrics that do not exist. This page is the
single reference for how that works, how to deploy it, and how to tune it.

It is **disabled by default**. Nothing in this page applies unless
a `classifier_url` is set under `[instrumental_detector]` and detection is enabled
globally (`enabled = true`) or for a library.

## What the detector is for

The detector covers a blind spot in the provider catalog. It does not compete
with provider lyrics: by default it is consulted only after every provider has
missed.

A provider row that reports no lyrics, no subtitles, and `Instrumental = 0` is an
**unannotated catalog entry**. It does *not* assert that the track has vocals. It
only says the provider never filled in that row. For game soundtracks, classical,
and similar collections, most of the catalog is in exactly that state. The
detector listens to the audio and settles those tracks, so Canticle stops asking
providers about lyrics that do not exist. Where a provider does return lyrics, the
detector does not run under the default ordering (see "Only on provider misses"
below). The opt-in `ordering = "front"` consults the detector first and skips the
providers when it is confident.

### Where it earns its keep

The figures below are from one live deployment of about 47,500 tracks. They show
the shape of the benefit, not a guarantee for your library.

| Library | Detector instrumental rate | Provider lyric coverage |
|---|---|---|
| VGM | 62.5% | 2.1% |
| Classical | 58.5% | 2.3% |
| Music | 21.4% | 82.9% |
| Kids Music | 4.2% | 91.9% |

In game-soundtrack and classical collections the provider resolves about 2% of
tracks and the detector resolves about 60%, so the detector does nearly all the
useful work. In a general pop library the provider resolves about 83% and the
detector is a modest supplement.

**Which number to quote.** The detector rates above are measured over rows where
detection actually ran. That is the honest denominator. The final outcome
distribution per library looks far more instrumental-heavy (about 98%), but that
is a selection artifact: a row settles as instrumental exactly when the detector
says so, and stays deferred when it does not. Quote the roughly 60%, not the 98%.

**When to enable it.** Turn it on for instrumental-heavy collections (soundtracks,
classical, ambient, library music), where providers have little to offer. It is
marginal for mainstream vocal libraries, where providers already cover most
tracks and each detector check costs an ffmpeg sample plus an inference call. You
can enable it per library (see the User Guide) instead of globally.

### The three gates, in operator terms

A track is marked instrumental only if all three checks pass. If any one fails,
the track stays a normal miss.

1. **Music:** the track is music throughout (averaged over the whole sample).
2. **No singing:** no sung vocal appears anywhere, even briefly (the loudest
   moment counts, not the average).
3. **No sustained speech:** spoken word does not run through the track (averaged,
   so a brief announcer or crowd sample is tolerated).

Thresholds and the reasoning for mean versus max are in "The decision model"
below.

### What you need to deploy

The detector needs the YAMNet classifier sidecar, which you build and run
yourself (see "Sidecar setup"), plus `ffmpeg` (and `ffprobe`, for spread
sampling; see "ffmpeg and ffprobe are both required" below). Detection runs
under both provider modes. Under `providers.mode = "parallel"` the detector
never joins the provider race: it is staged outside it, so with the default
`ordering = "demoted"` it runs only after at least one provider was asked and
every provider that was asked has answered without a usable lyric. When no
provider could be asked (every circuit open), the track waits. With
`ordering = "front"` the detector is asked before any provider: a track it
calls instrumental is settled with no provider request, and the providers race
only when it says not instrumental or is unavailable. Under either mode, a
track the detector calls not instrumental while no provider could be asked
(every provider circuit open) waits too: it is not counted as a miss, and no
provider that was not asked is charged one. It is set aside until a provider
may be tried again, so tracks queued behind it are still checked. With no
`classifier_url` set, no detector is built and Canticle behaves as if the feature
did not exist: providers only.

If the sidecar is configured but unreachable, Canticle never writes a marker
without it, so nothing is mislabeled. Until the first successful call after boot,
a refused connection is treated as the sidecar still starting: the item is
released with no penalty and retried. After 5 consecutive refused connections
with no success since boot, or if a sidecar that was up goes away, the detector
lane counts as an outage and its circuit breaker trips. Provider lookups carry on throughout.

## What it does and when it runs

When enabled, the detector samples a track's audio with `ffmpeg`, sends the
sample to an external [AudioSet](https://research.google.com/audioset/)
classifier (a YAMNet sidecar, vendored at `deploy/yamnet-detector/`), and decides
whether the track is instrumental. If it is, the worker writes the usual
instrumental marker (`.txt`) for that track.

Two rules bound when it can act:

- **Only on provider misses.** By default (`ordering = "demoted"`), the detector
  runs *after* every lyrics provider has come back empty. If a provider returns
  lyrics, the track is never classified - the audio model never overrides
  provider-supplied data.
- **Never destructive.** It only writes a marker where there would otherwise be a
  miss. A marker is not a final state: `--upgrade` re-checks every instrumental
  marker, provider- or detector-written alike, and a result at least as good as
  the marker replaces it (another marker included, which can change its
  provenance and mtime). A strictly worse result never replaces anything on
  disk, so a re-check cannot turn lyrics into a marker (only `--update` may go
  down).

## Title rule

Separate from the audio detector, and on by default: `instrumental_title.mode`
defaults to `strong` (see [CONFIGURATION.md](CONFIGURATION.md)). It needs no
sidecar. With it on, the worker settles a track whose title carries a strong
instrumental marker (for example "(Instrumental)") with an instrumental marker
file and asks no lyrics provider. Set it to `off` to disable.

**Upgrading.** After upgrading, rows not yet settled whose title is a strong
match get a marker file where before they would have been sent to the
providers. Rows already settled are not changed. The repair command `scan
reconcile-instrumental-titles` handles those (dry run by default).

**The problem it solves.** A track titled `Glass Harbor (Instrumental)` is the
instrumental cut of a sung track. Asked for it, a lyrics provider answers with
the sung version's words: its search is fuzzy and the qualifier does not
survive it. That answer ends the lookup before the detector is consulted (under
the default ordering it only runs after every provider misses), and a cached
answer never reaches a provider at all. The track ends up with lyrics for
singing that is not in the file.

**The policy.** With the mode at `strong` (or `all`), serve mode settles
a track whose title carries a strong instrumental qualifier with an
instrumental marker, without asking any provider, the cache, or the detector.
The title alone decides. Requiring the detector to agree was weighed and
declined: the rule would then silently turn back into today's behavior whenever
the sidecar is absent or its breaker is open.

A qualifier is strong only when it stands alone in brackets or after a dash,
beside nothing but packaging words (`Version`, `Mix`, a year), and qualifies a
real title: `(Instrumental)`, `- Off Vocal`, `[Backing Track]`, `(Inst)`,
`(BGM)`, `(No Vocals)`. These are deliberately NOT acted on:

- a track that is only the word (`Instrumental`), or uses it inline: that is a
  song named with the word;
- a dash-suffix title whose text before the dash could be the track's own
  artist, such as `Artist - Off Vocal`: that is a title tag carrying the
  artist, not a qualified title. The check is deliberately broad (see "The
  artist check" below). A bracketed qualifier is not subject to it;
- any a cappella title: vocals only, so its lyrics are correct;
- any karaoke title, or any track on a karaoke album: a karaoke track is for
  singing along, so it gets its lyrics (see "Karaoke and the detector" below);
- any sing-along product, named by the title or by the album, exactly as
  karaoke is (see "Sing-along products" below);
- weaker signals (an album named Instrumental, `Piano Version`, `Score`),
  unless the mode is `all` (see "Weaker signals" below).

**Sing-along products.** A recording sold for singing or playing over is
treated exactly like karaoke: no verdict, in the title or in the album, so the
track is looked up and keeps its lyrics. The phrases are:

- `Accompaniment`, `Performance` or `Split`, directly followed by `Track`,
  `Tracks` or `Trax`;
- `Minus One`, `Minus 1`;
- `Sing` or `Play` followed by `Along` or `Alongs`, or by `A Long` or `A Longs`
  (`Sing-A-Long`, `Sing A Long`);
- the karaoke-label phrases `Originally Performed By` and `In the Style Of`;
- in the album name only: `Backing` directly followed by `Track`, `Tracks` or
  `Trax`.

`Backing Track` means two things, and where it stands tells them apart. In a
track title it is an instrumental qualifier. In practice it labels a studio
outtake, such as `(Backing Track)`, and that recording really has no vocal. A
qualifier listed after a slash with other words, as in `(Take 14 / Instrumental
Backing Track)`, is a weaker signal and is not acted on. On a release name, as in `Low Tide (Backing Tracks)`, it
marks a sing-along product. Every track on that release keeps its lyrics, a
track titled `(Backing Track)` included. Only the singular is the title
qualifier: `Backing Tracks` in a title is no signal either way.

`Minus One` is no longer an instrumental qualifier. It was, and a title
carrying it was settled with a marker. Such a track is made for singing along,
so its lyrics are wanted. A track already settled as instrumental under the old
rule keeps its marker file. `scan purge-provenance --source title-rule` removes
the markers the rule wrote (see "Provenance and undo" below), and the tracks are
then looked up again. A title that carried the old `Minus One` term in an
unusual spelling can now be judged by its other qualifier instead.

The words of a phrase count only when they are separated by whitespace, by a
a single dash of any kind with nothing around it, or written as one word (`SplitTrax`,
`Singalong`, `Minus1`). Any other punctuation between them means no match:
`Live Performance - Track 3` and `Performance, Track 2 (Instrumental)` are not
excluded. `Performance`, `Backing` or `Split` alone excludes nothing.

The over-match is deliberate. A song genuinely titled `Minus One` or `In the
Style of ...` gets no verdict either. That costs one normal lookup, where the
opposite mistake costs a track its lyrics.

**The artist check.** A dash-suffix title is acted on only when the text before
the last dash is known not to be the artist. It is compared against the track
artist and the album artist, ignoring case, width, diacritics, dotted initials,
a leading `The`, track numbering, and the spelling of `&` / `and`. The rule does
not act when the two are equal, when either contains the other (a multi-artist
tag, or a title that credits a guest the tag does not), or when no real artist
is known: an empty tag and a placeholder (`Various Artists`, `VA`, `Unknown
Artist`, `Unknown`) both count as unknown. Every doubtful case resolves toward
not acting. The cost is stated plainly: a genuine `Artist - Title -
Instrumental` title is declined too, and that track is simply looked up like any
other.

**Weaker signals (`all`).** A weak signal is the word used inline or
as the whole title, a qualifier beside other words, `Piano Version` or
`Orchestral Version`, `Score`, or an album that carries the signal over a clean
title. None of these is reliable enough to act on alone, so under `all`
such a track is put to the audio detector before the cache and the providers.
That order holds in the worker only: the scan's own title check is for strong
titles, so a weak-titled track with a cached lyric can still be settled from the
cache at scan time. In the worker:

- the detector says instrumental: the track is settled exactly as any detector
  verdict is (the detector's marker and lane, not `title-rule`), with no
  provider asked;
- the detector says not instrumental: the track is looked up as usual, and the
  detector is not run again for it in that pass, whatever
  `instrumental_detector.ordering` is.

Everything else is left on the ordinary path, unchanged: no detector
configured, detection off for the track's library, the detector's breaker open,
an upgrade re-fetch, and any track that already has a lyric or a marker on
disk. `providers.mode` makes no difference: the weak tier asks the detector
first under ordered and parallel dispatch alike. A detector that fails or is
still starting settles nothing and the track is looked up as usual; the early
ask still counts as a detector call, so a failing detector can open its breaker
sooner.
`strong` and `off` never ask the detector early. The tier inherits the
detector's accuracy: a sung track with a weak title that the detector hears as
instrumental gets a marker and no lyrics. With the upgrade sweep on and
`instrumental_detector.ordering = "demoted"`, a marker settled this way is in
the sweep's population like any detector marker, and a later provider lyric can
replace it.

**Instrumental releases (counted, not acted on).** Some releases are
instrumental as a whole and say so in the album name, not in each title:
`Low Tide (Instrumental)`. Canticle recognizes a plain-titled track on such a
release, and `scan reconcile-instrumental-titles` counts those tracks on two
lines of its dry run:

```
weak, album named as instrumental: N
weak, plain title on an instrumental release (counted only, a lower bound): N
```

Both lines count rows inside `weak`. The first is the upper bound: every weak
row whose own title carries no signal and whose stored album name says
instrumental in any shape, the unrecognized shapes below included. The second
is the lower bound: the rows of the first line that pass every check in this
section. The second is never above the first, and the first is never above
`weak`. Among the weak rows, the true number on instrumental releases is
between the two. Rows outside `weak` are on neither line, so neither count
bounds the library as a whole.

Nothing acts on either count yet. The track is looked up like any other, keeps
the lyrics it has, and its album stays the weaker signal it always was. The
reason: the queue row, the lyric cache and the lyric block identify a track by
artist and title, with no album. A library holding both the sung album and its
instrumental edition has one row per artist and title, carrying the album of
whichever copy settled first, so such a track is counted or not by scan order,
and acting on it would affect the sung copy. That is why nothing acts yet
(#1525).

The album name must end in the marker. The marker's whole content is one of
`Instrumental`, `Instrumentals`, `Instrumental Version`, `Instrumental Edition`
or `Instrumental Album`, in any case. Two shapes count:

- in round or square brackets at the end: `Low Tide (Instrumental)`,
  `Low Tide [Instrumentals]`. Full-width forms of those brackets count too;
- after a dash at the end: `Low Tide - Instrumental`. The dash needs a space
  before it. A tilde is not a dash here.

Every doubt withholds the answer. These common shapes are NOT recognized. All
but the last are still counted on the first line:

- the bare trailing word: `Low Tide Instrumentals`;
- `(The Instrumentals)`;
- a suffix after the marker: `(Instrumental) - Single`;
- a second bracket pair anywhere in the name;
- a comma, `&`, `and` or `with` in the album base;
- `(Instrumental Versions)`;
- `(Inst.)`. This one is on neither line.

The full list of albums that are not counted on the second line:

- the bare trailing word or the word inline;
- other words inside the marker: `(Instrumental Demos)`, `(Deluxe /
  Instrumental)`, `(Instrumental Mix)`, `(The Instrumentals)`,
  `(Instrumental Versions)`;
- a marker that is not last, any second bracket pair, or any other kind of
  bracket;
- a name that is only the word, or the word after a track or disc number;
- a name containing `score`: a score album can be sung;
- a name that reads as a list before the marker: it holds `/`, `+`, `&`, `;`,
  `,`, `|` or `、`, or the word `and`, `plus`, `bw`, `vs` or `with`. A single
  such as `Quiet Shelf / Quiet Shelf (Instrumental)` is one sung track and its
  instrumental. `Salt and Stone (Instrumental)` is withheld too, by choice;
- a name holding a sing-over product: `made famous by`, `tribute to`,
  `accompaniment`, `playback`, `trax`, `for singers`, `cast recording`,
  `cover version`;
- a name with a voice word before the marker (the title list below);
- karaoke, sing-along and a cappella, in the album or the title.

The track title is checked in two ways. A bracket pair or a dash-suffix is
where a sung version is named, so it may hold only these words:

- a year, meaning 1900 to 2099;
- `remaster`, `remastered`, `remasters`, `reprise`, `album`, `interlude`,
  `intro`, `outro`, `prelude`, `mono`, `stereo`;
- `pt`, `part` or `parts`, alone or followed by numbers of up to three digits
  or short roman numerals (`i`, `v` and `x`, up to four letters);
- `version`, only directly after `album`.

Any other word there withholds the answer: `(Radio Edit)`, `(Extended)`,
`(English)`, `(Version)`, `- Paper Lantern`. An empty bracket pair withholds it
too. The whole title must also hold none of these:

- a word containing `vocal`, `lyric`, `voice`, `choir`, `choral`, `chorus`,
  `duet`, `narrat`, `speech`, `spoken` or `skit`, or starting with `sing`,
  `sang` or `sung`. `Single` and `Invoice` are withheld by this, by choice;
- the words `voc`, `live`, `demo`, `demos`, `remix`, `cover`, `acoustic`,
  `bonus`, `rap`, `raps`, `rapped`, `rapper`;
- the vocal words of the title rule, which also withhold here: `vocal`,
  `vocals`, `vox`, `sung`, `guide`, `chorus`, `sing`, `singalong`, `lyric`,
  `lyrics`, `hook`, `voice`, `singer`, `rap`, and the Japanese words that rule
  knows for a vocal, a chorus, a guide and a sung version;
- an `&` anywhere in the title;
- a credit: the words `feat`, `featuring`, `ft`, `f` or `w`; `with`, `con`,
  `avec` or `mit` followed by a word; `x` or `vs` between two words;
- `made famous by` or `tribute to`;
- `歌`, `唱`, `보컬` or `라이브` anywhere in the text. This list holds no kana
  word. So a title that says live or demo in Japanese is withheld only inside a
  bracket pair or a dash-suffix, by the allowed-word rule above.

A title with an instrumental signal of its own, strong or weak, is on neither
line: it is already counted on its own line. An empty title is not counted.
Invisible format characters are ignored, so they cannot hide a word.

The artist tags are checked last. A guest credit in the track artist or the
album artist withholds the answer: the words `feat`, `featuring`, `ft`, `f` or
`w`; `with`, `con`, `avec` or `mit` followed by a word; `x` or `vs` between two
words. A band named with `&` or `and` is not a credit. An album
tagged `Artist - Instrumental` is an album named with the word, so the text
before a dash is compared with both artists as in "The artist check" above.
When the marker itself follows a dash and no real artist is known, the answer
is withheld. The bracket form needs no artist.

**Karaoke and the detector.** Under `instrumental_detector.ordering = "front"`
the detector answers before any provider, and a karaoke backing track sounds
instrumental to it. So while the rule is on, a karaoke or sing-along track is not sent
to the detector first: the providers are asked, and if none has lyrics the track
is deferred like any other miss. It is not marked instrumental at that point.
With the rule off, `front` behaves as it always has. Other orderings are
unaffected either way.

**What it never does.** It never replaces a file. A lyric already on disk stays
byte for byte, on an ordinary pass and on an upgrade re-fetch alike, and so does
any instrumental marker already there, whoever wrote it: the detector, a
provider, or a person. The rule changes only tracks with nothing on disk yet.
A track written to several output directories counts as one track: a lyric or
marker in any of them means no marker is written in any.
One-shot `fetch` ignores the rule: naming a track there is an explicit request
for it.

**Turning it on and off.** The mode is read once at startup, so a change takes
effect on the next restart of `serve`.

**Provenance and undo.** The marker carries `[source:title-rule]` and the row's
source is `title-rule` (shown as "Title Rule"). The verdict is not cached and is
not recorded as a detector result. To undo it in bulk, set the mode to `off`,
restart, and run:

```sh
canticle scan purge-provenance --source title-rule        # dry run: what would go
canticle scan purge-provenance --source title-rule --yes  # remove the markers
```

Only markers the rule wrote match, since it never restamps a marker that was
already there. The tracks are then looked up again on the next scan. This does
not undo a weak-tier marker: the detector wrote it and it carries the detector's
source, so it cannot be told apart from any other detector marker.

**Residual risk.** A sung recording whose title carries a strong qualifier gets
a marker and no lyrics while the mode is on, and nothing corrects it
automatically. The artist check cannot recognize an artist written in a
different script from the tag (a katakana prefix against a romanized artist
tag): such a title is treated as qualified. The mitigations are the switch
itself, the purge above, and the fact that a file already on disk is never
displaced. There is no per-track override.

### Repairing rows already settled (`scan reconcile-instrumental-titles`)

Turning the rule on changes nothing for a track that already has a lyric on
disk. `scan reconcile-instrumental-titles` finds the tracks a provider settled
with lyrics whose title the rule would now settle as instrumental, and repairs
the ones it can prove it is safe to touch: the lyric file is saved to a backup
and removed, its words are blocked for that track, and the track is queued
again. The running worker then settles it with a `[source:title-rule]` marker.

```sh
canticle scan reconcile-instrumental-titles                                # dry run: counts only
canticle scan reconcile-instrumental-titles --tail rows.tsv                # dry run, plus one line per row
canticle scan reconcile-instrumental-titles --yes --backup repair.jsonl    # repair
```

**Dry run is the default.** Without `--yes` it reads and counts and changes no
row and no file. Like every `scan` command it creates the database and applies
pending migrations if needed. Read the counts first.

**It reads audio tags.** With `enrichment.enabled` on, a strong-titled row's
album comes from the metadata index when the index is current for the file, and
from the audio file's tags otherwise. That read can log the existing
duration-parse warning, which names a file path in the log (never on stdout).

**Flags.**

- `--yes` repairs. It needs `--backup`, and it is refused while
  `instrumental_title.mode` is `off`: the reopened tracks would only be fetched
  again. A dry run works in any mode and says when the mode is off. The command
  reads `instrumental_title.mode` and `enrichment.enabled` from its own config:
  if `serve` runs with the mode off, or with a different `enrichment.enabled`,
  the reopened tracks are fetched again instead of settled with a marker.
- `--backup <file>` is the JSONL file each lyric file is appended to, and
  synced to disk, before it is removed. It is appended to if it exists. It may
  not be a lyric file, sit inside a library, or be the database file. `--tail`
  has the same limits and must be a different file.
- `--tail <file>` appends one line per strong-titled row: the work item id, a
  tab, and what became of the row (`repaired`, `would-repair`, `not-provable`
  and so on). It holds no path, artist or title. With `--yes` it is always
  written; the default is `<backup>.rows.tsv`.
- `--library <name|id>` limits the rows examined to one library.

**Output.** Counts only: no path, artist, title or lyric text is printed. Paths
appear only inside the backup file.

| Line | Meaning |
|---|---|
| `rows examined` | Tracks whose lyrics came from a provider: settled ones, and ones a worker holds right now (`processing`). Equals no signal + weak + strong. |
| `no signal or excluded (a cappella, karaoke, sing-along)` | The title carries no qualifier, or is a cappella, or the title or album names karaoke or a sing-along product. Never touched. |
| `weak` | A weak signal only. Never touched. |
| `weak, detector says instrumental` / `weak, detector says not instrumental` / `weak, never scored` | The weak rows by the verdict the detector already stored for them; the three add up to `weak`. Nothing is run to get it. It sizes a possible weak-tier repair. |
| `weak, album named as instrumental` | The weak rows whose own title carries no signal and whose stored album name says instrumental in any shape. An upper bound on the weak rows that sit on instrumental releases, not on the library as a whole; never above `weak`. A count only. |
| `weak, plain title on an instrumental release (counted only, a lower bound)` | The rows of the line above whose title is plain and whose stored album name ends in an instrumental marker, with every check passed (see "Instrumental releases" above). A lower bound on the same weak rows, never above the line above; the truth for the weak rows is between the two. A count only: no such row is repaired, skipped or reported differently because of it, with or without `--yes`. The album and artists are the ones stored for the row; no audio file is read for these two lines. |
| `strong` | A strong qualifier. The `strong <family> <position>` lines and the three detector lines break this number down. It equals the four `strong, skipped` lines plus `candidates`. |
| `strong, skipped, audio file unreadable` | The audio file's album tag could not be read, so the title could not be judged the way the worker judges it. |
| `strong, skipped, in flight` | A worker holds the row right now. |
| `strong, skipped, hand-marked` | The lyrics were edited by hand, or the track was marked instrumental by hand. |
| `strong, skipped, artist before the dash or no artist known` | The rule declines the title (see "The artist check"): the text before the dash could be the artist, or no real artist is known. |
| `candidates` | Rows the repair examined on disk. Equals every line from here down to the repaired line. |
| `skipped, symlinked lyric file` | A lyric file is a symlink. |
| `skipped, outside library roots` | An output directory is in no configured library, or is gone. |
| `skipped, a file the repair would leave on the stem` | Another file would stay where the marker goes (a hand-placed marker, a word-timing file Canticle does not own), so no marker could be written. |
| `skipped, no lyric file on disk` | Nothing to remove: no lyric file, or one with no words in it. |
| `skipped, source tag names another lane` | The `.lrc` says a different provider wrote it than the row records. |
| `skipped, not provable` | See below. |
| `needs a manual mark` | See below. |
| `errors` | The row failed part-way or could not be handled. See the exit status. |
| `rows that would be repaired` / `rows repaired` | Rows that passed every check. |
| `lyric files that would be backed up` / `lyric files backed up` | Their lyric files. |
| `lyric files removed`, `new blocks`, `cache entries invalidated`, `rows reopened` | Applied runs only: what the repair did. |

**Not provable.** A lyric file is removed only when its modification time is
within two minutes of the moment the row settled, before or after. That date is
the only proof that the file is the one the provider wrote; the words matching
are not proof, because a hand-edited file can still match. Expect this count to
be large on an old library. The usual causes are:

- the track was fetched again later and the file on disk was kept, which moves
  the row's settle time and leaves the file's date alone;
- the file was rewritten, edited, copied or restored with another date (a
  library moved between disks, a sync tool, a tag editor);
- the row's settle time is missing or unreadable;
- the `.lrc` file's header cannot be parsed.

These tracks keep their lyrics. To replace one by hand, use `scan mark-wrong`.

**Needs a manual mark.** A file appeared or changed while its row was being
repaired. The row is left settled with its files in place, but its words are
already blocked. A rerun skips it again. Find the ids in the `--tail` file and
run `canticle scan mark-wrong --id <id> --yes` for each.

**One pass is not exhaustive.** A track waiting on an upgrade re-fetch or a
word-timing re-check is not settled at that moment, so the command does not see
it. Run the dry run again later to pick those up.

**Restoring.** The backup has the same records as `scan mark-wrong` (`op` is
`reconcile-instrumental-titles`; see [Mark a track's lyrics as wrong](USER_GUIDE.md#mark-a-tracks-lyrics-as-wrong)), one
for every file that was about to be removed. That includes rows that were then
refused or failed, whose files are still on disk. So restore only the records
whose `work_item_id` has a `repaired` line in the `--tail` file:

```sh
awk -F'\t' '$2 == "repaired" { print $1 }' repair.jsonl.rows.tsv
```

A rerun appends to the same backup, so one path can have several records; the
last one is the file as it was removed. A record whose id has no line at all in
the `--tail` file belongs to the last row of a run that stopped because the
tail could not be written. Stdout names that row (`row <id> (<disposition>) has
no line in the --tail file`), and its file is gone if the disposition printed
is `repaired`.

Putting a file back does not undo the block or the marker: `scan unblock
--work-item <id>` lifts the block, and the purge under "Provenance and undo"
removes the markers.

**Exit status.** 0 when the run finished and no row errored; skipped rows are
not failures. 1 when a row errored, a row needs a manual mark, the run was
interrupted (the counts printed are then partial, and the last row handled is
named), the `--tail` file or a backup record could not be written (the run
stops at that row), `--yes` was refused because the mode is off, no library
root is configured, or the config, database or `--library` could not be used.
2 for `--yes` without `--backup`, or an unsafe `--backup` or `--tail` path. A row that
errored may be part-way: its files removed and the row not yet reopened. A rerun
does not promise to finish it; it reports where each row stands now.

## The decision model (the core)

A track is marked instrumental only when **all three** gates pass. Any gate
failing means "not instrumental", and the track is left as a normal miss.

| Gate | Condition | Default |
|------|-----------|---------|
| **Music gate** | The **mean** over frames of the summed `instrumental_classes` probabilities is at least `min_confidence`. | `min_confidence = 0.90`, `instrumental_classes = ["Music", "Musical instrument"]` |
| **Sung-vocal gate** (#384) | The **peak** (max over frames) of *every* `vocal_classes` score stays **below** `vocal_max_confidence`. | `vocal_max_confidence = 0.015`, `vocal_classes` = the singing/vocal set below |
| **Speech gate** (#403) | The summed frame **mean** of the `speech_classes` stays **below** `speech_max_confidence`. | `speech_max_confidence = 0.20`, `speech_classes = ["Speech"]` |

The default `vocal_classes` (sung-vocal) set is:

```
Singing, Vocal music, Choir, A capella, Chant, Rapping,
Child singing, Synthetic singing, Yodeling, Humming
```

`Speech` is **no longer** in `vocal_classes`: as of #403 it is governed by its own
**speech gate** on sustained presence. A track with brief incidental speech (a
crowd sample, an announcer, a single line of dialogue over a bed) should still be
markable instrumental, whereas a *sustained* spoken-word track should not. The
peak-based sung-vocal gate could not tell those apart (both produce a high single
peak); the mean-based speech gate can. Any legacy config that still lists
`Speech` in `vocal_classes` is **de-duplicated** at detector construction - the
class is removed from the effective peak set so it is governed solely by the
speech (mean) gate, delivering the fix without requiring you to edit your config.

### Why mean for music, max for sung vocals, and mean for speech

This asymmetry is the heart of the design:

- **Music** is gated on the frame **mean**, because instrumental backing is
  present throughout the track - a sustained, track-wide property.
- **Sung vocals** are gated on the frame **max**, because singing can be *brief*.
  A short sung passage is diluted to near-nothing by the mean but preserved by
  the max. Gating sung vocals on their loudest single moment is what stops an
  otherwise-instrumental aria from slipping through.
- **Speech** is gated on the frame **mean**, because it models *sustained*
  presence. A brief announcer or crowd transient has a high single peak but a
  near-zero mean; a spoken-word track (monologue, narration over a bed) has a
  high mean. Mean is robust to a single loud transient where a raised peak
  threshold would not be, so brief incidental speech no longer wrongly blocks an
  instrumental marking while sustained spoken word still does.

### Spread sampling

Late-entering vocals (an aria after a long instrumental intro, a vocal that
arrives two minutes in) would be missed by sampling only the start of the track.
So the detector builds **one** sample from `spread_samples` short windows
(default **6**) distributed evenly across the **whole** track, concatenated into a
single `sample_duration_seconds` clip, and runs one inference on it. The vocal
peak is then taken across that whole spread.

This requires the sidecar to return both reductions - `{"mean": {...}, "max":
{...}}` - on the one forward pass. A value of `spread_samples < 2` disables
spreading and uses a single contiguous window from the start.

### Conservative by construction

Any doubt resolves to **not instrumental**. A false instrumental is strictly
worse than a missed one: it permanently suppresses a real lyrics fetch, whereas a
missed instrumental just gets retried. Every threshold default is chosen to fail
safe in that direction.

### Calibration evidence

The `vocal_max_confidence = 0.015` default was calibrated on 2026-07-19 against a
296-track labeled sample scored by the live sidecar: 146 tracks that provider
lyrics prove are vocal, and 150 provider-labeled instrumentals.

**The two classes overlap; there is no clean separating margin.** An earlier
6-track sample suggested instrumentals topped out near `0.021` while vocals
floored near `0.059`, implying a comfortable 3x gap. The larger sample refutes
that: known-vocal `vocal_peak` ranges from `0.0019` to `0.8861` (median `0.0959`,
p5 `0.0257`), so genuinely-vocal tracks reach well into what looks like
instrumental territory. Any threshold trades one error against the other.

| `vocal_max_confidence` | False instrumental (of 146 known-vocal) | True instrumental recovered (of 150) |
|---|---|---|
| `0.005` | 0.68% | 24.0% |
| `0.010` | 1.37% | 42.7% |
| **`0.015`** (default) | **1.37%** | **54.0%** |
| `0.020` | 2.05% | 58.7% |
| `0.030` (prior default) | 4.79% | 66.0% |

During this sweep the music gate was held at `0.90` and the speech gate at
`0.20`.

`0.015` is chosen because it recovers the most true instrumentals available at
its error level: it costs no additional false instrumentals over `0.010`, and
the prior `0.030` misclassified 4.79% of known-vocal tracks.

Note that `cmd/vocalcalib -mode sweep` returns `0.007589` at its default 1%
error ceiling. That output is **not** the default, deliberately. It is pinned
exactly to one sample's `vocal_peak` (moving it by `1e-5` doubles the error
rate), and it is dominated by `0.015` - same real error count, 22 points less
recovery. At n=146 a single track is 0.68%, so a 1% ceiling is finer than the
sample can resolve and pushes the sweep into a strictly worse operating point.
Treat the sweep as evidence, not as the decision.

**The music gate is the binding constraint on recovery.** The `min_confidence =
0.90` default was measured against the same 296-track sample with vocal gate held
at the calibrated 0.015 and speech gate at 0.20:

| `min_confidence` | False instrumental (of 146 known-vocal) | True instrumental recovered (of 150) |
|---|---|---|
| 0.95 | 1.37% | 49.3% |
| **0.90** (default) | **1.37%** | **54.0%** |
| 0.85 | 1.37% | 57.3% |
| 0.80 | 2.05% | 60.0% |
| 0.70 | 2.05% | 60.0% |
| 0.50 | 2.05% | 60.7% |

Total headroom is about 7 percentage points, and recovery plateaus hard below
0.80. Lowering `min_confidence` to 0.85 is free in error rate (identical 1.37%
false-positive rate) and recovers 3.3 additional percentage points, but that is
5 tracks out of 150 and is marginal at this sample size. **Do not change the
default.** The value in this measurement is knowing the recovery ceiling, not
moving the constant.

An earlier analysis claimed the music gate capped recovery at roughly 79%. That
figure was inferred from 32 of 150 negatives failing the `music_sum >= 0.90`
gate, not directly measured. The sweep shows the real ceiling is about 61%:
most of those 32 rows do not become recoverable when the gate lowers, because
they fail on other grounds too. The speech gate blocks 2 of 150 and has little
effect on measured instrumental recovery (positive-class `speech_mean` median
0.0006, orders of magnitude below the 0.20 threshold).

### Applying a threshold change to rows already decided

Changing `vocal_max_confidence` only affects future detections. Rows the old
value already decided are never revisited on their own, in either direction, so
a recalibration is finished only once the backlog is re-decided:

```sh
# Thresholds LOOSENED: promote rows the old, tighter gate buried.
canticle scan reconcile-instrumental-recalibrate

# Thresholds TIGHTENED: revert rows the old, looser gate wrongly settled.
canticle scan reconcile-instrumental-recalibrate --reverse
```

Both re-decide from the telemetry already stored on each row
(`music_sum`/`vocal_peak`/`speech_mean`), so neither re-reads audio or calls the
sidecar - they are arithmetic passes, not detection passes. Both are dry-run by
default, print what they would change, and need `--yes` to apply; applying
writes a JSONL backup of every changed row first.

The `--reverse` direction removes marker sidecars, so it is guarded: a marker is
deleted only when its `[source:]` header proves the **detector** wrote it. A
provider-declared instrumental, or a legacy bare marker with no header (which is
indistinguishable from one), is left untouched and counted as
`skipped(provider-owned=N)`. Reverted rows return to the queue at normal scan
priority for a normal provider fetch - deliberately not the miss-backoff tier,
because a reversed instrumental is a track with real vocals that is unusually
likely to resolve on its next attempt.

Rows with **no** stored telemetry (written before migration 025) are invisible to
both directions - there are no scores to re-decide from. Those need a real
re-scan via `scan reconcile`.

The speech gate's `speech_max_confidence = 0.20` default (#403) is a different
kind of value: it was chosen conservatively low (biased toward "not
instrumental", preserving lyric protection) and has since been measured against
the audit set (see the music-gate sweep above): it has little effect on
measured instrumental recovery, blocking 2 of 150 labeled instrumentals, so
there is nothing to pin. Because the
key is configurable, it can still be changed without a code change.

## Sidecar setup

The classifier is a small YAMNet HTTP service. Canticle does not publish an image
for it; you build it on the host from the vendored source.

- **Source:** `deploy/yamnet-detector/` in this repo (Dockerfile + FastAPI app).
- **Response contract:** `POST /classify` returns
  `{"mean": {<class>: <prob>, ...}, "max": {<class>: <prob>, ...}}` - both the
  mean and the max-over-frames reduction for **every** AudioSet class (a full map,
  no thresholding or top-N). The vocal gate needs the `max` map; a legacy mean-only
  sidecar degrades safely to never-instrumental rather than producing wrong
  markers. The full-map guarantee matters: Canticle records the vocal classes a
  healthy response carries and, on every later decision, treats a configured vocal
  class **missing** from a non-empty `max` map as a partial/contract-violating
  response and fails safe to not-instrumental (see Operations below). A custom
  sidecar that omits zero-scored classes would trip this on normal tracks, so any
  replacement must honor the full-map contract.
- **Wire it up:** point `classifier_url` at the service, e.g.
  `http://yamnet:8080`.

### ffmpeg and ffprobe are both required

The app container needs **both** `ffmpeg` and `ffprobe` on PATH:

- `ffmpeg` extracts and concatenates the audio sample.
- `ffprobe` reads each track's **duration** so spread sampling can place its
  windows.

If `ffmpeg` was auto-provisioned by Canticle it ships **no** `ffprobe`; set
`ffprobe_path` (or install `ffprobe` on PATH) or the detector silently falls back
to a single window from the start and loses late-vocal protection. See
[Configuration -> ffmpeg resolution](CONFIGURATION.md#ffmpeg-resolution) for how
`ffmpeg` is located.

### Upgrade ordering (important)

When upgrading a running deployment, **upgrade Canticle before the sidecar.** The
new Canticle tolerates an old flat-map (mean-only) sidecar response and degrades
safely; an old Canticle **cannot** parse the new `{mean, max}` response. Upgrading
the sidecar first can break classification until the app catches up.

## Which setting drains which backlog

A row the detector settles can cost a provider request and a provider pacing
wait, or neither. Which one depends on the settings below. These rules hold
under both `providers.mode` values, `"ordered"` and `"parallel"`.

- **`ordering = "demoted"` (the default).** The providers are asked first. A row
  the detector ends up settling has already cost a provider request, and the
  worker then waits out the provider pacing before the next row. This is by
  design: a provider lyric is never overridden by the audio model, so the
  detector only sees a row once every provider that was asked has missed.
- **`ordering = "front"`.** The detector is asked first. A row it settles as
  instrumental costs no provider request and no pacing wait. A row it calls not
  instrumental goes on to the providers and pays their pacing as usual. The cost
  is twofold. A detector false positive can now override a provider lyric on a
  fresh row. And the audio is read before any provider is asked, for each row
  that reaches the detector with no stored scores to answer from. A row never
  reaches it while detection is off or the detector's breaker is open.
- **A Weak instrumental title.** With `instrumental_title.mode = "all"`,
  a row whose title carries only a weak instrumental signal is put to the
  detector first under either ordering, with the same costs as `front` for that
  row. See "Title rule".
- **The serve-mode backfill sweep.** It drains rows the detector has never
  scored (`instrumental_result` is empty, status deferred, a source path on
  file, and detection on for the row). It makes no provider request, so provider
  pacing does not slow it. Its rate is `backfill.batch_size` rows (default
  `100`) every `backfill.interval_minutes` minutes (default `60`). See "The
  background backfill sweep" below.
- **A row deferred while the detector was on.** It is usually already scored. The
  first full not-instrumental answer is stored on the row, so asking again reads
  the stored scores and gives the same answer without reading the audio. The
  answer changes only if the thresholds change (they are applied to the stored
  scores) or the sidecar's model version changes (the audio is then scored
  again).
- **A Strong instrumental title.** With `instrumental_title.mode` set to
  `strong` or `all`, such a title settles with no lookup at all: no
  provider, no cache and no detector. See "Title rule".

## Configuration reference and tuning

All keys live under `[instrumental_detector]`; each has an
`MXLRC_INSTRUMENTAL_DETECTOR_*` environment equivalent (see the full table in
[Configuration](CONFIGURATION.md)).

| Key | Default | Purpose |
|-----|---------|---------|
| `enabled` | `false` | Master switch. |
| `classifier_url` | (none) | Sidecar base URL. Required when enabled. |
| `ffmpeg_path` | `ffmpeg` | ffmpeg binary (PATH or auto-provisioned). |
| `ffprobe_path` | (auto-discover) | ffprobe binary for duration probing. Sibling of ffmpeg, then PATH. |
| `sample_duration_seconds` | `30` | Total sample length, clamped to [30, 60]. |
| `spread_samples` | `6` | Windows spread across the track. `< 2` disables spreading. |
| `min_confidence` | `0.90` | Music-gate threshold (mean). Values outside (0, 1] reset to `0.90`. |
| `instrumental_classes` | `["Music", "Musical instrument"]` | Classes summed for the music gate. |
| `vocal_max_confidence` | `0.015` | Sung-vocal-gate threshold (peak). Values outside (0, 1] reset to `0.015`. |
| `vocal_classes` | (the singing/vocal set above) | Sung-vocal classes whose peak blocks an instrumental marking. |
| `speech_max_confidence` | `0.20` | Speech-gate threshold (summed mean). Values outside (0, 1] reset to `0.20`. |
| `speech_classes` | `["Speech"]` | Speech classes gated on sustained mean (not peak). |
| `cooldown_seconds` | `5` | Minimum gap between inference calls. `0` disables. |

**The defaults are the calibrated values - do not change the thresholds without a
specific reason.** They were tuned against real audio (#384) to maximize the
margin between instrumentals and vocals.

**Recovery is ceiling-bound by the detector, not by the thresholds.** The three
gates trade against each other in a narrow band (7pp headroom), and the recovery
ceiling is a property of the acoustic model and feature set rather than any
constant. The goal of recovering sustained melodic instrumentals that read faintly
as vocals (#510) is not reachable by threshold tuning alone; it requires a
different model or different input features.

If you do tune:

- **`vocal_max_confidence`** is the dial that matters. **Raising** it lets more
  tracks pass as instrumental (more false instrumentals - the dangerous
  direction). **Lowering** it marks fewer tracks instrumental (safer, but you may
  re-query genuine instrumentals).
- **`speech_max_confidence`** controls the speech gate the same way, on the
  summed Speech **mean**. Its `0.20` default is has little effect on measured
  instrumental recovery (see Calibration evidence): raising it tolerates more sustained speech
  as instrumental; lowering it blocks instrumental marking on less speech.
- **`min_confidence`** rarely needs changing; lowering it admits non-music audio
  (field recordings, spoken word) as "instrumental".
- **`spread_samples`** trades inference cost for coverage. Fewer windows risks
  missing a late vocal entry; more windows dilutes each one's audio share within
  the fixed `sample_duration_seconds`.

### The background backfill sweep

Under the default `demoted` ordering, detection is otherwise reached only after
a provider miss, which is paced to the provider's rate limit. The one exception
is a Weak instrumental title under `instrumental_title.mode = "all"`,
which is put to the detector first (see "Which setting drains which backlog"). A
track the detector has never scored therefore waits on a throttle it does not
use, and an install where nobody runs `scan reconcile-instrumental` drifts
forever - on one real library that produced a backlog of over 7,000 never-scored
tracks.

Serve mode runs a bounded periodic sweep to fix that. It is **on by default**: the
work is local, bounded, and makes no provider request.

| Key | Default | Purpose |
|-----|---------|---------|
| `backfill.enabled` | `true` | Master switch for the sweep. The CLI works either way. |
| `backfill.batch_size` | `100` | Tracks classified per cycle. |
| `backfill.interval_minutes` | `60` | Gap between cycles. |
| `backfill.cooldown_seconds` | `0` | Gap between the sweep's own detector calls. |

All four are under `[instrumental_detector.backfill]` with
`MXLRC_INSTRUMENTAL_DETECTOR_BACKFILL_*` environment equivalents.

The sweep uses **its own detector instance**, so its cooldown is independent of
`instrumental_detector.cooldown_seconds`. That separation is deliberate rather
than cosmetic: a detector holds an internal lock across its cooldown sleep, so a
shared instance would park the worker's interactive detection behind a whole
sweep cycle.

### Taming it on a low-wattage or shared host

Size against the **real** per-track cost, which is not inference. Measured on a
live install (Canticle 1.30.1):

| Stage | Cost per track |
|-------|----------------|
| ffmpeg sample extraction (6 windows) | ~0.10s |
| Classifier inference (30s sample) | 0.07-0.11s |
| Classifier inference (60s sample, the longest sent) | 0.13-0.24s |

So a track costs roughly **0.2 seconds of real work**, and a default 100-track
cycle is about a 20-second burst once an hour.

**Bound the sweep between cycles, not within one.** A disk spindown timer measures
the longest *contiguous* quiet window, not the average rate, so a short burst
followed by a long silence is what lets an array sleep. Dribbling the same 100
tracks out across the hour keeps the disks awake far longer to do exactly the same
work - strictly worse for idle time and for power.

Recipes:

- **Quieter** - lower `batch_size` (fewer tracks per burst) or raise
  `interval_minutes` (longer silence between bursts). Prefer these two.
- **Drain a large backlog faster** - raise `batch_size`, or run the CLI directly
  with a large `--limit` (`canticle scan reconcile-instrumental --yes --limit 5000`).
- **Spread a cycle out** - raise `backfill.cooldown_seconds`. This is the knob
  that actually governs how long a cycle *takes*, but it works against the
  spindown goal; reach for it only when the burst competes with something else on
  the host.
- **Cut the per-track cost** - lower `spread_samples`, which multiplies the ffmpeg
  work per track. This changes detection accuracy, so treat it as a last resort.
- **Off entirely** - `backfill.enabled = false`. The `scan reconcile-instrumental`
  CLI still works, so an operator can drain on their own schedule.

The `cpus:` limit on the sidecar container is an **isolation** knob, not a
throughput one - it bounds how much of the host TensorFlow grabs during a burst,
but it governs only ~0.2s of a multi-second per-track cost. Do not size detector
throughput with it.

## Operations and troubleshooting

The decision is logged. Look for the detector decision line, which reports the
computed `music_sum`, the `vocal_peak`, the `speech_mean`, and the resulting
verdict, so you can see *why* a track was or was not marked.

- **The borderline band.** A `vocal_peak` of roughly `0.015-0.05` is genuinely
  mixed material (quiet backing vocals, sparse vocal samples). The default
  `0.015` deliberately keeps these on the "not instrumental" side.
- **A false instrumental** (a vocal track wrongly marked) usually means a vocal
  that the sample under-weighted - check that `ffprobe` is present and spread
  sampling is actually running (a single-window fallback is the common culprit).
- **Speech vs sung vocals are distinct failure modes.** A spoken-word track
  wrongly *not* marked, or an incidental-speech instrumental wrongly *blocked*,
  is the **speech gate** (`speech_mean` vs `speech_max_confidence`), not the
  sung-vocal gate (`vocal_peak`). Read `speech_mean` in the decision line: a high
  value is sustained speech (correctly blocked); a near-zero value with a high
  Speech peak is brief incidental speech (correctly allowed through the now-split
  gate). Tune `speech_max_confidence`, not `vocal_max_confidence`, for these.
- **Partial classifier responses.** A non-empty `max` map that omits a vocal class
  the sidecar normally returns is a contract violation (a truncated/corrupt
  response): the absent class would silently contribute 0 to `vocal_peak` and
  weaken the gate, so the detector treats that decision as **not instrumental** and
  logs `detector: vocal classes missing from a non-empty classifier max map` at
  `Error` on **every** occurrence (not once per process). A configured vocal class
  the sidecar *never* returns is instead a permanent config/contract mismatch: it
  is logged once and dropped from the baseline so the gate keeps running on the
  classes the sidecar does emit. Note the deliberate severity split: a *fully*
  absent `max` map is the expected legacy mean-only degradation and stays at
  `Warn`, while a present-but-partial map is the unexpected violation at `Error`.
  This fail-safe is scoped to the partial-response case only - it does not explain
  the conservative `0.015-0.05` borderline band (by design) or other separately
  tracked refinements (cross-version model drift). `Speech` activation on
  non-lyrical audio is now handled by the dedicated sustained-mean speech gate
  (#403). Per-decision telemetry (the scores, the winning vocal class, and the
  detector version) is persisted on the `work_queue` row - see *Decision
  telemetry* below.
- **Re-classifying / clearing stale markers.** After changing thresholds or
  fixing the sidecar, re-validate existing markers with `scan reconcile` (see
  below) rather than a blanket `--update`. `--upgrade` also re-checks every
  marker (a lookup per marker per run, a provider request or more on a cache miss), and
  a result at least as good replaces it (another marker included); a strictly
  worse one never does (#553).

### Decision telemetry on `work_queue`

When the detector renders a verdict it stamps the decision inputs onto the
`work_queue` row (migration 025), alongside the boolean `instrumental_result`, in
a single atomic update:

| column | meaning |
| --- | --- |
| `music_sum` | summed music-gate score (`Result.Confidence`) |
| `vocal_peak` | peak sung-vocal score (`Result.VocalConfidence`) |
| `speech_mean` | summed speech-gate score (`Result.SpeechConfidence`) |
| `vocal_class` | the configured vocal class that produced `vocal_peak` (empty when none scored) |
| `detector_version` | the sidecar's loaded model identity (its pinned `YAMNET_SHA256`, read from `GET /health`) |

All five are **nullable**: `NULL` means detection did not run for that row
(detection disabled, no source path, or a row written before migration 025). They
make borderline review and drift detection a query rather than a 40-minute
re-inference pass:

```sql
-- borderline sung-vocal tracks worth a human look
SELECT id, source_path, vocal_peak FROM work_queue
WHERE instrumental_result = 1 AND vocal_peak BETWEEN 0.015 AND 0.05;

-- markers written against a different model (model drift)
SELECT id, source_path, detector_version FROM work_queue
WHERE instrumental_result = 1 AND detector_version <> '<current-model-sha>';
```

> **Why the MODEL, not the app version (#684).** This column used to hold the
> Canticle app version, and `detector_version` is what gates verdict reuse: the
> worker re-infers whenever a stored value differs from the detector's current
> one. So every Canticle release invalidated every stored verdict library-wide
> even though the classifier had not changed, and each affected row re-ran a
> YAMNet inference -- an ffmpeg decode of the audio file. Measured in prod before
> the fix: 26 distinct values across the queue, only 6.7% of telemetry-bearing
> rows reusable. The library disks never got an idle window long enough to spin
> down. Migration 038 re-keys existing rows; the model archive's sha256 has held
> a single value for the project's whole history, so no stored score changes
> meaning.
>
> An earlier revision of this doc suggested deriving model identity from the
> build arg or image tag "rather than a new `/health` field". That was the wrong
> call: neither is visible to a running Canticle process, which must ask the
> sidecar it is actually talking to -- an operator can point it at any image.
> `/health` now reports `model_version`, and an older sidecar that omits it
> yields UNKNOWN, which fails closed to re-inference.
>
> Gate/threshold drift is handled separately and needs no version bump:
> `DecideStored` re-applies the CURRENT thresholds to stored scores, so a
> recalibration re-decides every row with no re-inference.

### Reconciling stale markers (`scan reconcile`)

`scan reconcile` re-runs the detector over instrumental-tagged tracks and, for any
the current detector no longer classifies as instrumental, deletes the exact
instrumental `.txt` marker and re-queues the row so the scheduler re-fetches it.

- **Dry-run by default.** It prints what would change; pass `--yes` to apply.
- **Cheap by default.** Using the telemetry above, it re-infers only the
  *candidate* set - borderline `vocal_peak`/`speech_mean`, cross-version, or
  un-scored (pre-telemetry) rows - instead of every tagged track. `--all` forces a
  full re-inference of every `instrumental_result = 1` row.
- **Safe deletes.** A `.txt` is removed only when its content is *exactly* the
  instrumental marker, so genuine unsynced `.txt` lyrics are never touched. Every
  cleared row is written to a JSONL backup first (`--backup <path>`, default
  `<db-dir>/reconcile-backup-<timestamp>.jsonl`).
- **No starvation.** Re-queued rows are deferred at `priority = -100`, so a bulk
  reconcile is dequeued strictly behind foreground work.
- `--library <name|id>` scopes the run; `--limit <n>` caps it.

```sh
mxlrcgo-svc scan reconcile                 # dry run, narrowed candidate set
mxlrcgo-svc scan reconcile --yes           # apply
mxlrcgo-svc scan reconcile --all --yes     # re-infer every tagged track
```
