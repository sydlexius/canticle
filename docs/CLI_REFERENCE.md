# CLI Reference

This page documents every subcommand and flag. For operational guidance (running the server, Docker/Unraid, the watcher), see the [User Guide](USER_GUIDE.md). For every setting, see [Configuration](CONFIGURATION.md).

## Usage

```text
Usage: canticle [fetch|serve|scan|library|keys|admin|secrets|config|queue|provenance|realign|revalidate|timing-accuracy|completion]

Commands:
  fetch       fetch lyrics once without HTTP server or DB queue
  serve       run HTTP server, worker, and library scheduler
  scan        scan configured libraries and enqueue missing lyrics
  library     manage library roots
  keys        manage API keys
  admin       manage the web-UI admin account
  secrets     manage encrypted-at-rest secrets
  config      inspect or update configuration
  queue       inspect or maintain the durable work queue
  provenance  embed or inspect provenance tags in .lrc files
  realign     re-attach orphaned .lrc/.txt sidecars (and their .elrc companions) to renamed audio files
  revalidate  re-check existing .lrc timing against audio duration and remediate the backlog
  timing-accuracy  measure per-provider line-start timing error against a local hand-verified reference set
  completion  output a shell completion script (bash, zsh, or fish)

Global flags:
  --version  print the build version and exit
  --help     show help for the program or a subcommand

Legacy flag-only invocation is still supported:
  canticle [--outdir OUTDIR] [--cooldown COOLDOWN] [--depth DEPTH] [--update] [--upgrade] [--bfs] [--serve] [--listen LISTEN] [--token TOKEN] [--config CONFIG] [SONG ...]
```

## Version

`canticle --version` prints the embedded build metadata, for example
`canticle v1.1.0 (commit 1a2b3c4, built 2026-06-05T00:00:00Z)`. Release
binaries and the published Docker images carry the real tag; a `go build` or
`go install` from source reports `dev` unless you inject the ldflags yourself.

## Fetch

One-shot lyric fetching without the HTTP server or DB queue.

### One song

```sh
canticle adele,hello
canticle fetch adele,hello
```

### Multiple songs and a custom output directory

```sh
canticle adele,hello "the killers,mr. brightside" -o some_directory
```

### With a text file and a custom cooldown time

```sh
canticle example_input.txt -c 20
```

### Directory mode (recursive)

```sh
canticle "Dream Theater"
```

> **_This option overrides the `-o/--outdir` argument which means the lyrics will be saved in the same directory as the given input._**
>
> **_The output extension depends on the lyric type: `.lrc` when synced lyrics are found, and `.txt` when only unsynced lyrics or an instrumental marker is written. When the provider also serves word timings, a `.elrc` companion is written beside the `.lrc` under the default `output.word_sync_mode = "both"` (see [CONFIGURATION.md](CONFIGURATION.md))._**
>
> **_The `-d/--depth` argument limits the depth of subdirectories to scan; use `-d 0` or `--depth 0` to only scan the specified directory._**

The `--upgrade` flag re-fetches tracks that previously produced a `.txt` - unsynced lyrics or an instrumental marker, whoever wrote it - to promote them when better lyrics become available. Results are ranked from lowest to highest as instrumental marker, unsynced, line-synced, word-synced, and a result is written only when it ranks at least as high as what is already on disk, so a re-fetch never replaces a `.lrc` with a `.txt` or lyrics with a marker. Only `--update` (full re-fetch) may write a lower-ranked result over a better one. A line-synced `.lrc` is not reopened by `--upgrade`; it reaches word sync only through the paced word-sync recheck sweep (`[word_sync_recheck]`, off by default).

**Cost of `--upgrade`.** In fetch mode every run does at least one lookup per reopened `.txt`, instrumental markers included, and a cache miss costs one or more provider requests (more with fallback lanes or parallel mode); a marker is re-checked on every `--upgrade` run, not once. Requests are paced only by the cooldown (`-c`/`api.cooldown`), so on a library with many markers a routine `--upgrade` is mostly re-asking questions whose answer rarely changes. To narrow a run, scope the directory you pass. `scan --upgrade --unsynced-before <cutoff>` (below) is the scan-side equivalent, but under `scan`/`serve` a sidecar whose queue row is already `done` is not re-queued by the run itself (so it may issue no requests for it); the serve-mode [`[upgrade_sweep]`](CONFIGURATION.md#upgrade_sweep) re-queues such rows, paced and at most once a week.

A Canticle-written `.elrc` follows its `.lrc`: an `--update` re-fetch that replaces the `.lrc` also replaces the companion, or removes it when the new result has no word timings or `word_sync_mode` is `off`/`replace`, so a rewrite never leaves word timings beside a different `.lrc`. A result that is refused for a timing mismatch leaves both files as they were. A `.elrc` Canticle did not write is never touched.

### Scoping an upgrade to an older cohort

`scan --unsynced-before <cutoff>` narrows a single run's `.txt` re-fetch to sidecars last modified before a cutoff. It exists for a one-time repair: when an identifiable batch of sidecars was written by an older, buggier version, a plain `--upgrade` would re-fetch the entire unsynced population, including files that are already correct.

**It pairs with `--upgrade`, and is refused with `--update`.** The cutoff applies only to `.txt` sidecars. `--update` also reopens settled `.lrc` files, and those would be re-fetched regardless of the cutoff - so the pairing would present as a scoped repair while sweeping every synced track in the library, rewriting exactly the files a repair is trying not to disturb. Rather than warn about that, Canticle rejects the combination outright.

```sh
# Re-fetch only sidecars written before 2026-04-01
canticle scan --upgrade --unsynced-before 2026-04-01

# An exact instant, when a bare date is too coarse
canticle scan --upgrade --unsynced-before 2026-04-01T12:00:00Z
```

Why bother narrowing, rather than just re-fetching everything:

- **Provider traffic.** Re-fetching a correct file spends a request to learn nothing. Requests are paced by [`api.cooldown`](CONFIGURATION.md) (15s by default), and the adaptive pacer multiplies that base by up to 8x while a provider is throttling, so a few thousand unnecessary tracks can mean many hours of wall-clock.
- **It destroys the evidence.** Re-fetching rewrites the sidecar and bumps its mtime. Where a repair cohort is identified *by* mtime - which is the case for any batch predating the database - a full re-fetch erases the only signal distinguishing the damaged files from the healthy ones, and the run cannot be scoped again.

Behavior worth knowing before you rely on it:

- **It only ever subtracts from one run.** The flag narrows a re-fetch that was already going to happen; it can never reopen something the reopen rules exclude, and it writes no state. A file skipped by a dated run is fully eligible under the next ordinary scan.
- **It covers both `.txt` classes `--upgrade` reopens** - unsynced sidecars and instrumental markers of any provenance - for the evidence reason above. It does **not** cover `.lrc` files, which is why it is refused with `--update`.
- **It requires `--upgrade`,** and is rejected without it rather than silently matching nothing.
- **The comparison is strict.** A sidecar stamped exactly at the cutoff is excluded.
- **A bare date is read as midnight UTC.** Use the RFC3339 form when you need a different zone.
- **An unreadable sidecar is skipped**, not swept in: a bulk repair should touch only files positively identified as belonging to it.
- **`scan` only.** Serve mode's scheduler never applies a cutoff, so ongoing upgrades are unaffected.

Choosing a cutoff is an evidence question, not a guess. Sidecar mtime is a filesystem attribute, and a copy or restore can rewrite it, so confirm it still reflects write time before trusting it: Canticle never writes audio files, so if the sidecars in a suspected cohort carry timestamps that their sibling audio files do not share, no bulk filesystem event produced them. A genuine write cohort also spreads across time at roughly the provider's pace, where a bulk copy compresses into seconds.

In directory mode, when audio tags carry ISRC, MusicBrainz recording ID, or duration, those values are read and passed to Musixmatch to improve match precision - for example, distinguishing two recordings of the same title.

## Serve

Run the HTTP server, worker, and library scheduler. See the [User Guide](USER_GUIDE.md#lidarr-webhook-server) for full operational detail.

```sh
canticle serve --listen 127.0.0.1:3876
canticle serve --config path/to/config.toml
```

Relevant serve flags: `--listen` (overrides `MXLRC_SERVER_ADDR`), `--scan-interval` (deprecated; prefer `[server.scan_schedule]`), `--work-interval`, and `--config`.

## Library and key management

```sh
canticle library add /data/media/music --name Music
canticle library list
canticle scan
canticle keys create --name lidarr --scope webhook
canticle keys list
canticle keys revoke <raw-api-key>
```

`keys` has three subcommands: `create` (`--name`, repeatable `--scope` of `webhook` or `admin`; prints the raw key once), `list` (tab-separated public ID, name, scopes, revoked-at), and `revoke <raw-api-key>`. All accept `--config`. See [Webhook API keys](USER_GUIDE.md#webhook-api-keys) for the full workflow and the web UI equivalent.

## Web UI admin password

```sh
canticle admin set-password --user admin < newpass.txt
docker exec -i canticle canticle admin set-password --user admin < newpass.txt
```

`admin set-password` changes an existing web-UI admin's password. It is the only supported way to do so: there is no password-change screen in the web UI yet (#545), and editing `MXLRC_WEBAUTH_ADMIN_PASSWORD` does nothing once an admin exists, because the environment bootstrap never overwrites an existing account.

The password is read from **standard input, never a flag**, so it stays out of the host process list where any other user could read it. Read it from a file or a secret manager; never embed it in the command itself, including inside a `docker exec ... sh -c '...'` wrapper, since anything on the command line is visible in `ps` and recorded in shell history. This matches `canticle secrets set`, which rejects a value passed on the command line for the same reason. One trailing newline is stripped; leading and trailing spaces are preserved.

The update and the revocation of that user's existing sessions happen in a single transaction, so a rotation cannot half-apply. Everyone signed in with the old password is signed out immediately, including you. No restart is required. Accepts `--config`.

This also works when you are locked out, since it acts on the database rather than requiring a login. See [Changing or resetting the admin password](USER_GUIDE.md#changing-or-resetting-the-admin-password).

## Secrets

The Musixmatch token and the webhook API key can be stored encrypted at rest in the database instead of as plaintext in `config.toml` or environment variables. The encrypted store is the lowest-precedence source, so CLI flags, env vars, and TOML still win over it.

```sh
# Encrypt the currently-effective secret(s) into the DB store.
canticle secrets import                 # both token and webhook key
canticle secrets import --token         # only the Musixmatch token
canticle secrets import --webhook       # only the webhook API key

# Set one secret by name. The value is read from stdin (prompt or pipe),
# never from argv. Valid names: musixmatch_token, webhook_api_key.
canticle secrets set musixmatch_token             # prompts for the value
printf '%s' "$TOKEN" | canticle secrets set musixmatch_token

# List stored secret names and their updated_at (never the values).
canticle secrets list
```

`secrets set` rejects a value passed on the command line (it would land in shell history and `ps`); supply it on stdin. All three subcommands accept `--config`. See [Encrypted secrets](USER_GUIDE.md#encrypted-secrets) for the precedence model and key-loss recovery.

## Config

Inspect or update the configuration file from the CLI.

```sh
canticle config get db.path        # print one value by dotted key
canticle config set api.cooldown 30   # update one key, then write the config file
canticle config list               # print every known key as key=value
```

`config` has three subcommands: `get <key>` (prints the single value, exit 2 on an unknown key), `set <key> <value>` (applies the change to the effective config and writes the whole file back, creating it at the default path if absent), and `list` (prints every known key as `key=value`). All accept `--config` to target a non-default config file.

## Queue and scan inspection

The `queue` and `scan` subcommands expose the durable work queue and persisted scan results. See [Inspection commands](USER_GUIDE.md#inspection-commands) in the User Guide for the full command set (`queue list`/`failed`/`deferred`/`retry`/`clear`/`recheck`/`mark-instrumental`/`unmark-instrumental`, and `scan results`/`clear`/`reconcile`).

## Provenance

Synced `.lrc` files written by `canticle` carry provenance tags in the header block that identify where and when each file came from. These tags appear after the standard metadata tags (`[by:]`, `[ar:]`, `[ti:]`, etc.) and before the first timestamped lyric line:

```text
[source:musixmatch]
[fetched:2026-06-15T12:00:00Z]
[ve:v1.2.0]
[isrc:USRC17607834]
[mbid:9f2a2b4c-1234-5678-abcd-000000000000]
```

| Tag | Value | Notes |
|---|---|---|
| `[source:]` | provider lane name | e.g. `musixmatch`, `petitlyrics`, `innertube`. Always the lane, never the upstream licensor. |
| `[upstream:]` | per-result licensor | Only written by a multiplexing lane (today: `innertube`), which routes each result to one of several upstream lyric licensors. Omitted when no upstream was reported. See [Provider Attribution](provider-attribution.md). |
| `[fetched:]` | ISO 8601 fetch timestamp | UTC; absent on cache hits |
| `[ve:]` | generating Canticle version | e.g. `v1.2.0`; `dev` on local builds |
| `[isrc:]` | ISRC recording identifier | when available from the audio file or API response |
| `[mbid:]` | MusicBrainz recording ID | when available from the audio file |

### Provenance backfill

Existing `.lrc` files that predate this feature can have provenance tags injected retroactively from the work queue database:

```sh
# Preview what would change (dry run)
canticle provenance backfill

# Target specific paths or directories
canticle provenance backfill /data/music/Artist

# Apply the changes
canticle provenance backfill --yes

# Apply to specific paths
canticle provenance backfill --yes /data/music/Artist/Album
```

The backfill is idempotent: tags that already exist in a file are skipped; only genuinely absent tags are injected. The `[ve:]` tag is never injected on backfill (the originating version is not recorded in the database). Files for which the database has no matching row, or with only partial metadata, are reported as `partial` rather than `seeded`.

**Cache-hit writes and missing `[source:]`/`[fetched:]` tags:** when a lyric fetch is served from the in-memory cache, `[ve:]` is written inline but `[source:]` and `[fetched:]` are absent because those fields are transient (not persisted alongside the cached result). Run `provenance backfill --yes` after a cache-hit write to pull the source lane and fetch timestamp from the work queue database and inject them retroactively.

## Realign

When an audio file is renamed but its `.lrc` / `.txt` lyric sidecar is not, the sidecar is orphaned: it no longer shares a stem with any audio file, so a later scan re-fetches lyrics that already exist on disk. `canticle realign` re-attaches those orphaned sidecars to their audio using a confidence resolver, and only ever changes a sidecar's stem, never its extension (a synced `.lrc` stays `.lrc`, an instrumental `.txt` marker stays `.txt`).

The tiers:

- **exact** - the orphan's `[isrc:]` / `[mbid:]` header uniquely matches one audio file's embedded ISRC/MBID. Matched in `identity_keys` order (default `mbid`, then `isrc`).
- **heuristic** - exactly one orphaned sidecar and exactly one audio file missing its sidecar in the same directory, and their names match closely enough (a Jaro-Winkler name guard at `min_confidence`) **and** distinguishably: the pair must also beat the orphan's best score against every other audio file in the directory by `min_margin`, so a name that fits the whole album equally well is reported ambiguous rather than guessed.
- **heuristic-nm** - opt-in (`name_match = true`), for the common case a single-candidate heuristic can't resolve: a directory with *multiple* orphaned sidecars and *multiple* sidecar-less audio files (a folder of renamed tracks). Every orphan is scored against every remaining candidate; a pairing is only accepted when it clears `min_confidence` **and** the orphan's best score beats its runner-up by at least `min_margin`. Anything closer than that is reported ambiguous, never guessed.
- **ambiguous** - zero or multiple candidates on either side, or a pairing too close to call. Both name tiers can report a near-tie: `heuristic` when the pair fails to beat the orphan's best rival in the directory by `min_margin`, `heuristic-nm` when the best score fails to beat its runner-up by the same. Reported and skipped, never guessed.
- **conflict** - contradictory signals (multiple exact matches, or the destination sidecar already exists). Reported and skipped, never clobbered.

```sh
# Preview what would change across all libraries (dry run, the default)
canticle realign

# Apply the moves
canticle realign --yes

# Limit to a single library (name or numeric id)
canticle realign --library "Main Music" --yes

# Write the JSONL backup of applied moves to a chosen path
canticle realign --yes --backup /data/realign-undo.jsonl
```

Every applied move is recorded (before the rename) as a JSONL line - `{"old_path","new_path","library_id","method"}` - in `<db-dir>/realign-backup-<timestamp>.jsonl` (or the `--backup` path); swap `old_path`/`new_path` to undo. Behavior is gated by the [`[realign]` config section](CONFIGURATION.md#realign): `require_provenance = true` restricts applied moves to the exact tier (heuristic and heuristic-nm candidates are reported but skipped), `cross_directory = true` lets an exact match move a sidecar into a different directory within the same library, `min_confidence` sets the heuristic (and heuristic-nm) name-guard floor, `name_match` enables the N:M matcher, and `min_margin` sets the ambiguity-rejection threshold for both name-similarity tiers.

**Note:** the exact tier requires ISRC/MBID-tagged audio. Libraries whose files carry no such tags fall back to the heuristic tier (single-candidate-per-directory + name guard).

## Revalidate

`canticle revalidate` re-checks `.lrc` files that are **already on disk** against the duration of the audio they sit beside, and remediates the ones whose cues run past the end of the track. It is the backlog counterpart to the accept-time timing guard, which only ever sees new fetches.

**Not the same as `realign`.** `realign` is about a sidecar's *location* - it re-attaches an orphan to renamed or moved audio and never looks at timing. `revalidate` is about a sidecar's *content* - the file is beside the right audio, but its timestamps do not fit it. A file can need both; they are independent passes.

The buckets, from the shared timing predicate:

- **ok** - the last text-bearing cue lands within the audio (a 2s tolerance absorbs rounding). Decorative music-note markers are excluded from the comparison, so a lyric that parks a `♪` past the end is not flagged.
- **MisSynced** - the lyric overruns by more than the tolerance but stays under 1.5x the duration. The words are content-correct, only the timing is wrong, so the default keeps them.
- **categorical** - the lyric runs at or past 1.5x the duration, i.e. it is almost certainly timed to a different, longer recording. Its words are not trusted either.
- **unknown-duration** - no exact duration is cached for the audio file. **Always fails open**: counted, reported, never remediated. Run a scan to populate the duration cache.
- **no-audio** - a `.lrc` with no companion audio file. That is `realign`'s problem, not this command's.

```sh
# Report the distribution across all libraries (dry run, the default: writes nothing)
canticle revalidate

# Remediate: MisSynced files keep their words as .txt, categorical files are quarantined
canticle revalidate --apply

# Scan specific roots instead of the configured libraries
canticle revalidate /music/albums --apply

# Drop the words of a MisSynced file instead of demoting them
canticle revalidate --on-fail=delete --apply

# Hard-delete instead of quarantining (NOT reversible)
canticle revalidate --apply --purge

# Record the per-file offenders locally for your own inspection
canticle revalidate --tail ./offenders.tsv
```

**Reversible by default.** A removed `.lrc` is *moved* under `<db-dir>/quarantine` (or `--quarantine-dir`), preserving its path relative to the library root, not deleted - move it back to undo. A demotion writes the plain words to a `.txt` beside the audio **first**, then moves the `.lrc` aside, so a failed write leaves the original untouched; an already-settled `.txt` or `.lrc` is never overwritten. Every applied action is recorded as a JSONL line in `<db-dir>/revalidate-backup-<timestamp>.jsonl` (or `--backup`) before it happens. `--purge` is the opt-in escape hatch and is genuinely irreversible.

**Output is aggregate-only.** Only counts are printed; no path, artist, title, or lyric text ever reaches stdout, so the report is safe to paste into an issue. Per-file detail goes only to the local file you name with `--tail`.

## Reconcile word sync

`scan reconcile-word-sync` queues tracks that already have a line-synced `.lrc` for a word-timing re-check. Turning on `output.word_sync_mode` only changes new fetches, so without this pass an established library keeps its line-only output. The command itself fetches nothing. It marks the selected queue rows, and a running `canticle serve` worker re-asks only the word-capable providers (Musixmatch, Petit Lyrics) for them, behind all fresh work. A track whose providers have no word data is marked as such and not asked again, unless the provider set changes or you pass `--recheck-absent-before`.

```sh
# Count the candidates and the minimum drain time (dry run, the default: writes nothing)
canticle scan reconcile-word-sync

# Size the run: the 500 oldest tracks settled before 2026-06-01, in one library
canticle scan reconcile-word-sync --completed-before 2026-06-01 --limit 500 --library music

# Apply
canticle scan reconcile-word-sync --completed-before 2026-06-01 --limit 500 --library music --yes

# Also re-check tracks judged "no word data" before a date (catalogs gain word timings over time)
canticle scan reconcile-word-sync --recheck-absent-before 2026-01-01 --yes
```

- **Output is aggregate-only:** `candidates=N selected=M already-queued=K estimated-minimum-drain=<duration>`. No path, artist, or title is printed.
- **Cutoffs are strict** and take a date (midnight UTC) or an RFC3339 instant, like `--unsynced-before`. `--library` takes a name or numeric id and can be repeated. A track shared (deduplicated) with a library outside the filter is skipped, since its re-check rewrites every copy; an unscoped run covers it.
- **Refused when `output.word_sync_mode = "off"`.** Under `off` a re-check could not write any word timings.
- **Reversible.** Each row's prior queue state is written as a JSONL line in `<db-dir>/reconcile-word-sync-backup-<timestamp>.jsonl` (or `--backup`, appended to if it exists) and fsynced before its batch commits. If that write fails, the batch is rolled back and the command names how many trailing records belong to it.
- **Cost.** See [Word-timing re-check cost](USER_GUIDE.md#word-timing-re-check-cost). Read the dry-run count first.

## Reconcile remediated

`scan reconcile-remediated` (#1143) re-describes completed "synced" rows that the dashboard counts as "Synced (tier unknown)" because their file is gone or was demoted, using the files actually on disk. Dry run by default; `--yes` applies.

```sh
canticle scan reconcile-remediated
canticle scan reconcile-remediated --yes
```

- **No `.lrc` and no `.txt`:** the row is reset for re-fetch and its cache entry is dropped in the same transaction, so the next fetch is real, not served from cache.
- **Only a `.txt`:** the row becomes `unsynced` (the upgrade sweep can then offer it).
- **An `.lrc` (or case variant) is present:** a word or line tier is recorded. An `.lrc` that classifies as unsynced is left untouched and counted as `unsynced_lrc`.
- **Left unchanged and counted on the `skipped:` line:** `kept_remediation_verdict` (a timing verdict beside a present `.lrc` is history, so the row stays tier-unknown even though its tier is recorded), `retired` (audio gone), in-flight rows (`processing`, `word_recheck`, `upgrade_armed`), `already_recorded`, `no_audio`, `audio_gone` (audio moved; left to prune), `unreadable` and `raced`. `scanned` is a superset of the dashboard count because it includes in-flight rows.
- **Aggregate-only output,** and each applied row's prior outcome, tier, timing verdict and status are written to `<db-dir>/reconcile-remediated-backup-<timestamp>.jsonl` (or `--backup`) and fsynced before the row commits. That is enough to undo an `unsynced` or tier change by hand; for a reset it is an audit record only, since the reset also clears word-timing and upgrade state it does not save, and restoring it would bring back a row claiming a missing synced file.
- **Busy database:** a row that hits `SQLITE_BUSY` is counted as `write_failed` (exit 1) and is not retried, so the backup never gets a duplicate record; rerun the command, which is idempotent.

## Reconcile upstream

`scan reconcile-upstream` (#1298) fills `work_queue.upstream` (the licensor an `innertube` result was served from) for rows settled before the column existed, by reading the `[upstream:]` tag from each row's sidecar. Dry run by default; `--yes` applies. On demand only: there is no serve-startup pass or sweep.

```sh
canticle scan reconcile-upstream
canticle scan reconcile-upstream --yes
```

- **Candidates come from the database,** never a directory walk: done rows with a recorded lane that can report an upstream (`innertube`) and no upstream yet. The sidecar is derived from the row's audio path (`.lrc`, else the Canticle-owned `.elrc` (a foreign one is skipped), else `.txt`, each with an extension-case fallback); a symlink is never read.
- **A row is filled only when the sidecar's `[source:]` equals the row's lane.** Everything else stays NULL and is counted on the `skipped:` line: `source_mismatch`, `no_sidecar`, `unreadable`, `no_tags` (no tag block, e.g. a file written before the tags existed; never guessed), `no_upstream` (a `[source:]` with no `[upstream:]`), `unknown_upstream` (an `[upstream:]` token Canticle never writes; only `musixmatch` and `lyricfind` are accepted), `processing` (in flight), and `raced` (the row changed between plan and apply; the write is guarded in SQL on still-done, same lane, upstream still NULL).
- **Aggregate-only output** (no library path, artist or title; the one path printed is the backup file's, beside the database, and only when a row was filled), and each filled row's id, lane and value are written to `<db-dir>/reconcile-upstream-backup-<timestamp>.jsonl` (or `--backup`, 0600) and fsynced before the row commits. Restoring is setting `upstream` back to NULL. On a quiescent database a dry run reports the same counts the apply then produces; `raced` and `write_failed` can only be non-zero on apply.

## Purge provenance

`scan purge-provenance` selects sidecars by a header tag. Dry run by default; `--yes` applies. Exactly one selector is required: `--source <name>`, `--no-source` or `--generated`. `--library` limits the run to one library.

```sh
canticle scan purge-provenance --source <name>
canticle scan purge-provenance --generated
canticle scan purge-provenance --generated --yes
```

- **`--source <name>` / `--no-source`** (#474) delete each matching `.lrc`/`.txt` (and an owned `.elrc` companion), drop its cache entry and requeue the track for re-fetch.
- **`--generated`** (#1008) undoes accepted Auto alignment. It selects each `.lrc` whose header carries `[timing:canticle-aligner]` and puts `<name>.lrc.orig` back over it, which restores the provider's own timing. The `.orig` is consumed, an `.elrc` companion is removed only when Canticle owns it (`[by:canticle]`) and it carries the same marker, and the row's edit mark is cleared with its tier recorded as line-synced. Nothing is re-fetched: the queue status and the cache are left as they were. The row's timing verdict is cleared; the serve timing sweep, when enabled, judges the restored file. `[source:]`, `[fetched:]`, `[isrc:]` and `[mbid:]` tags the retimed file had gained since the backup are added to the restored file (`[upstream:]` is not). A hand offset made before the alignment was accepted is not preserved.
- **Backup no longer matches:** a `.lrc.orig` is saved once, by the first edit, and a later re-fetch does not refresh it. A file is restored only when its backup has the same lines of text in the same order and no differing `[source:]`, `[fetched:]`, `[upstream:]`, `[isrc:]` or `[mbid:]` tag; otherwise it is left untouched and counted as `skipped original differs`.
- **No queue row:** a restored file with no queue row has no edit mark to clear and is counted as `without a queue row`.
- **With `--library`:** a file whose queue row is found only by its audio path is restored only when that row is linked to the chosen library and to no other; otherwise it is left untouched and reported in a note, and a run without `--library` restores it.
- **Tag re-add failures:** if a tag cannot be added back after a restore, the restore stands and the run counts an error (non-zero exit).
- **Skipped as well:** a file with no `.lrc.orig` (left untouched, never deleted, counted as `skipped without an original`), symlinked sidecars, and files whose row is in flight (`processing`); rerun later for those. The dry run's `would restore up to` count is an upper bound.
- **Backup first.** Every file a run replaces or deletes is written, bytes included, to `<db-dir>/purge-provenance-backup-<timestamp>.jsonl` (or `--backup`) and fsynced before it is touched; a failed backup leaves the file alone.
- **`--generated` output is aggregate-only on stdout:** no track path, only counts and the backup path. A failure warning on stderr names the file's path, as it does for the other selectors.

## Index Metadata

`scan index-metadata` walks a library's audio files and records the complete tag set into the `audio_metadata` table. This populates audio metadata coverage independently of fetch history - a library that has never had lyrics fetched can be indexed to record ISRC, MBID, duration, and other technical metadata from the audio files themselves.

The command is designed to be cheap on repeat runs. A file whose (path, mtime, size) still matches its existing row is skipped without being opened, so re-running over an unchanged library performs almost no I/O and also doubles as a coverage check.

```sh
# Preview what would be indexed across all libraries (dry run, the default)
canticle scan index-metadata

# Index a single library (name or numeric id)
canticle scan index-metadata --library "Main Music"

# Apply the indexing
canticle scan index-metadata --yes

# Pilot run: index no more than 1000 files before stopping
canticle scan index-metadata --limit 1000 --yes

# Re-run to verify coverage on unchanged files (prints file count and total coverage)
canticle scan index-metadata --yes
```

- **Dry-run by default.** It prints what would be indexed; pass `--yes` to actually write metadata rows.
- **Cheap by default.** Files are checked against their (path, mtime, size) key first. A match means the file is already indexed and unchanged; it is skipped without opening. A second pass over an unchanged library is therefore nearly free and serves as a coverage spot-check.
- **No backup.** Unlike the `reconcile-*` commands, `index-metadata` is purely additive and destroys nothing, so there is no JSONL backup to restore.
- **Scale context.** A full library of approximately 84,000 files requires one header read per file, not a full-file read. The operation is I/O-bound on the filesystem, not the audio metadata parser.
- `--library <name|id>` scopes the run to a single library; default indexes every configured library root.
- `--limit <n>` caps the number of files newly read and recorded (0 = no limit); already-indexed files are still walked and skipped for free, and do not count against the budget. The cap is shared across every library root in one run, not applied per root. Useful for a pilot run before applying to the full library.

See the Rollout section of issue #646 for operational guidance: capture row counts before starting, dry-run on the smallest library first to verify field coverage and error rates, then live-run and immediately re-run to confirm the skip logic is working.

## Shell completion

```sh
canticle completion <bash|zsh|fish>
```

See [Shell completion](USER_GUIDE.md#shell-completion) for installation snippets.

## Timing accuracy

`canticle timing-accuracy <RefDir> [--lanes a,b] [--config P]` reports how far each lane's line starts and word starts sit from a hand-verified reference (#1117). It asks each lane for every reference track, compares, and writes nothing: no library, queue, cache, or database.

`RefDir` is **local only** and should live outside any git tree (a directory that itself holds a `.git` entry is refused; parents are not checked). It holds `manifest.toml` plus the reference `.lrc`/`.elrc` files. Each `[[track]]` has `id` (unique, never printed), `file` (relative, inside `RefDir`), `artist`, `title` (required, not blank), `album` (optional), `duration_seconds` (optional, not negative), `line_residual_ms` (required, not negative: the reference's own declared line-start error), and `word_residual_ms` (not negative; required when that reference file carries word timings). An unknown manifest key is refused; the error names the key, never a value or a path. The reference `.lrc` is plain synced LRC (`[mm:ss.xx]text`); to carry word timings it uses Enhanced LRC (A2) inline markers, `[00:01.00]<00:01.00>one <00:01.40>two`, read by the same parser as the `.elrc` companion (`lyrics.ParseTimedLRC`). The reference set is local only and is never published or committed.

Output is aggregate-only: one row per lane and per `lane/upstream`, with sample sizes, line MAE, share within 0.3 s, and the declared residual. When any reference file carries word timings, each row is followed by a word-start row (`word-tracks`, `words(ref matched)` (`ref` counts the reference words on every matched line that has them, so it shows partial coverage), `word-MAE`, share within 0.3 s, the declared word residual, and the matched-word sample size `n`). Words pair only inside line pairs that already matched, and only where both the reference and the served result carry word timings for that line; a reworded word stays unmatched (it lowers the matched count, not the error). Leading and trailing punctuation is ignored when pairing words. A word-timed reference line must not stack timestamps; such lines are measured for line starts only, and the header counts them. A served line whose words all share one start is line-level data and is not measured as word timing. With a repeated word the word error is a lower bound. A lane with no matched words prints `word-MAE=n/a`. Decorative cues are dropped on both sides and counted per side (`reference=` and `provider=`). A lane with no matched cues prints `n/a`, never 0. Cues pair only when their normalized text is equal, so a reworded or re-punctuated line stays unmatched (it lowers the matched count, not the error). When a track repeats a line (a chorus) and the two sides disagree on how many times, the reported error is a lower bound. Lanes run one after another at each lane's own pacing (`api.cooldown` below 1 is raised to 1 second, then each lane's floor applies; InnerTube costs three requests per track). A lane that reports a rate limit or a provider outage is not asked again (its row says which), and the tracks it was not asked are counted separately from `not_found` and `failed`. Supply the Musixmatch token through `MUSIXMATCH_TOKEN` / `MXLRC_API_TOKEN` or the config file. `--token` is an optional override that wins over both, but a token passed as an argument can be exposed through shell history and process listings, so prefer the other two. The command reads no token from stdin and does not consult the encrypted store. An interrupted run prints partial rows and exits 1. A run in which no lane matched a single line (every lane skipped, unserved, or throttled) prints `no lane produced a measurement` and exits 1.
