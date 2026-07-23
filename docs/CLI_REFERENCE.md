# CLI Reference

This page documents every subcommand and flag. For operational guidance (running the server, Docker/Unraid, the watcher), see the [User Guide](USER_GUIDE.md). For every setting, see [Configuration](CONFIGURATION.md).

## Usage

```text
Usage: canticle [fetch|serve|scan|library|keys|admin|secrets|config|queue|provenance|realign|completion]

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
  realign     re-attach orphaned .lrc/.txt sidecars to renamed audio files
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
> **_The output extension depends on the lyric type: `.lrc` when synced lyrics are found, and `.txt` when only unsynced lyrics or an instrumental marker is written._**
>
> **_The `-d/--depth` argument limits the depth of subdirectories to scan; use `-d 0` or `--depth 0` to only scan the specified directory._**

The `--upgrade` flag re-fetches tracks that previously produced a `.txt` (unsynced) file, to promote them to `.lrc` when synced lyrics later become available. Instrumental tracks are always written as `.txt` and are excluded from upgrade - only `--update` (full re-fetch) overrides them.

### Scoping an upgrade to an older cohort

`scan --unsynced-before <cutoff>` narrows a single run's `.txt` re-fetch to sidecars last modified before a cutoff. It exists for a one-time repair: when an identifiable batch of sidecars was written by an older, buggier version, a plain `--upgrade` would re-fetch the entire unsynced population, including files that are already correct.

**Pair it with `--upgrade`, not `--update`.** The cutoff applies only to `.txt` sidecars. `--update` also reopens settled `.lrc` files, and those are re-fetched regardless of the cutoff - so `--update --unsynced-before` still sweeps every synced track in the library. That is the opposite of a scoped repair, and it rewrites exactly the files a repair is trying not to disturb.

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
- **It covers every reopenable `.txt` class** - unsynced sidecars, provisional (detector-written) instrumental markers, and the authoritative markers that only `--update` reopens - for the evidence reason above. It does **not** cover `.lrc` files, which is why it pairs with `--upgrade`.
- **It requires `--upgrade` or `--update`,** and is rejected without one rather than silently matching nothing. One reopen path is outside its reach: a detector-version bump re-checks provisional markers on its own, with no flag set. The cutoff still narrows that re-check when a scan supplies one, but it cannot be requested on its own.
- **The comparison is strict.** A sidecar stamped exactly at the cutoff is excluded.
- **A bare date is read as midnight UTC.** Use the RFC3339 form when you need a different zone.
- **An unreadable sidecar is skipped**, not swept in: a bulk repair should touch only files positively identified as belonging to it.
- **`scan` only.** Serve mode's scheduler never applies a cutoff, so ongoing upgrades are unaffected.

Choosing a cutoff is an evidence question, not a guess. Sidecar mtime is a filesystem attribute, and a copy or restore can rewrite it, so confirm it still reflects write time before trusting it: canticle never writes audio files, so if the sidecars in a suspected cohort carry timestamps that their sibling audio files do not share, no bulk filesystem event produced them. A genuine write cohort also spreads across time at roughly the provider's pace, where a bulk copy compresses into seconds.

In directory mode, when audio tags carry ISRC, MusicBrainz recording ID, or duration, those values are read and passed to Musixmatch to improve match precision - for example, distinguishing two recordings of the same title.

## Serve

Run the HTTP server, worker, and library scheduler. See the [User Guide](USER_GUIDE.md#lidarr-webhook-server) for full operational detail.

```sh
canticle serve --listen 127.0.0.1:3876
canticle serve --config path/to/config.toml
```

Relevant serve flags: `--listen` (overrides `MXLRC_SERVER_ADDR`), `--scan-interval`, `--work-interval`, and `--config`.

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

The `queue` and `scan` subcommands expose the durable work queue and persisted scan results. See [Inspection commands](USER_GUIDE.md#inspection-commands) in the User Guide for the full command set (`queue list`/`failed`/`deferred`/`retry`/`clear`/`recheck`, and `scan results`/`clear`/`reconcile`).

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
| `[source:]` | provider lane name | e.g. `musixmatch`, `petitlyrics` |
| `[fetched:]` | ISO 8601 fetch timestamp | UTC; absent on cache hits |
| `[ve:]` | generating canticle version | e.g. `v1.2.0`; `dev` on local builds |
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

When an audio file is renamed but its `.lrc` / `.txt` lyric sidecar is not, the sidecar is orphaned: it no longer shares a stem with any audio file, so a later scan re-fetches lyrics that already exist on disk. `canticle realign` re-attaches those orphaned sidecars to their audio using a four-tier confidence resolver, and only ever changes a sidecar's stem, never its extension (a synced `.lrc` stays `.lrc`, an instrumental `.txt` marker stays `.txt`).

The four tiers:

- **exact** - the orphan's `[isrc:]` / `[mbid:]` header uniquely matches one audio file's embedded ISRC/MBID. Matched in `identity_keys` order (default `mbid`, then `isrc`).
- **heuristic** - exactly one orphaned sidecar and exactly one audio file missing its sidecar in the same directory, and their names match closely enough (a Jaro-Winkler name guard at `min_confidence`).
- **ambiguous** - zero or multiple candidates on either side. Reported and skipped, never guessed.
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

Every applied move is recorded (before the rename) as a JSONL line - `{"old_path","new_path","library_id","method"}` - in `<db-dir>/realign-backup-<timestamp>.jsonl` (or the `--backup` path); swap `old_path`/`new_path` to undo. Behavior is gated by the [`[realign]` config section](CONFIGURATION.md#realign): `require_provenance = true` restricts applied moves to the exact tier (heuristic candidates are reported but skipped), `cross_directory = true` lets an exact match move a sidecar into a different directory within the same library, and `min_confidence` sets the heuristic name-guard floor.

**Note:** the exact tier requires ISRC/MBID-tagged audio. Libraries whose files carry no such tags fall back to the heuristic tier (single-candidate-per-directory + name guard).

## Shell completion

```sh
canticle completion <bash|zsh|fish>
```

See [Shell completion](USER_GUIDE.md#shell-completion) for installation snippets.
