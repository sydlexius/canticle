# Encrypted-at-rest storage for secrets (Musixmatch token + webhook key)

Status: design of record - SIGNED OFF 2026-06-13 (A=c + first-run key hint, B=yes, C=yes); Decision A revised to (a) 2026-06-14 (#238)
Date: 2026-06-13
Issue: #223
Related: #204 (web-UI auth / onboarding / TLS - the broader "secure the serve
surface" docket; the onboarding UI is where these secrets get entered)

## Summary

Store the two recoverable runtime secrets - the Musixmatch API token
(`api.token`) and the serve-mode static webhook API key(s)
(`server.webhook_api_keys` / `MXLRC_WEBHOOK_API_KEY`) - **encrypted at rest in
the existing pure-Go SQLite DB** instead of as plaintext in TOML and environment
variables. Encryption uses **AES-256-GCM** (Go stdlib AEAD) with a managed
32-byte key. The key comes from one of two sources: an auto-generated `0600` key file under
the data dir (the universal zero-setup default, all platforms including Docker)
or an operator-supplied env master key (`MXLRC_MASTER_KEY`, opt-in for
key/data separation).

Existing env/TOML setups keep working unchanged: the DB is the **lowest**
precedence source. A new `secrets` CLI command performs an explicit one-time
import of the current plaintext secret into the encrypted store. Decrypted values
are held only in memory at use time and are never logged or echoed.

This is a design-first issue. Implementation is tracked in a separate issue that
references this document (see "Decomposition").

## Threat model and scope

At-rest encryption here defends a narrow, well-defined boundary. Being explicit
about what it does and does not protect is the whole point, because a key colocated
with the data it protects gives a false sense of security.

**Protected against (the secret stays confidential):**

- DB-only exfiltration: the `.db`/`.db-wal` is copied, backed up, attached to
  another tool, synced to cloud storage, or accidentally committed to git, while
  the encryption key is not.
- Casual inspection: someone with read access to the DB file cannot recover the
  token by opening it in a SQLite browser.
- A leaked TOML/compose file alone no longer contains the live secret once the
  operator has imported it and removed the plaintext.

**NOT protected against (out of scope, by design):**

- A compromised running process. The plaintext key and decrypted secrets are in
  memory at use time; an attacker with code execution or memory access on the live
  daemon recovers them. AES-at-rest is not process hardening.
- Whole-volume theft when the key file lives in the same bind-mount as the DB
  (the Docker default - see "Key management"). An attacker who copies the entire
  data volume gets both ciphertext and key. This is exactly why the env-master-key
  path exists and why it is the recommended Docker posture.
- External secrets managers (Vault, KMS, SOPS). Local-first only (a #223 non-goal).

**Why reversible AEAD, not hashing (the distinction from #204):** #204 stores
*login credentials* and *generated API keys* one-way (PBKDF2-SHA256 in
`api_key_metadata`) - you only ever verify a guess against the hash. These two
secrets are different: the daemon must send the **real** Musixmatch token upstream
on every request, and an operator must be able to read back / rotate the webhook
key they configured. Recoverable plaintext is a hard requirement, so the mechanism
is authenticated symmetric encryption with a managed key, not a one-way hash.
(The webhook-key comparison itself is constant-time; recoverability is needed for
operator round-trip and rotation, not for the comparison.)

**Scope of secrets.** Two secrets in v1: `musixmatch_token` and
`webhook_api_key`. The table and the encrypt/decrypt API are designed as a
**general secret store** (CR design choice 3, option 2) so future credentials
(additional provider keys, TLS material) reuse it without a schema change, but
only these two are wired in now (YAGNI for the rest).

Note the existing `api_key_metadata` table (generated keys, hashed) is a separate
mechanism and is **not** migrated or touched by this work.

## Encryption scheme

AES-256-GCM via `crypto/aes` + `crypto/cipher` from the Go standard library
(CR design choice 1, option 1).

- **Algorithm:** AES-256-GCM. GCM is an AEAD mode: it provides confidentiality
  *and* integrity/authenticity, so a tampered or truncated ciphertext fails
  decryption loudly rather than yielding garbage plaintext.
- **Key size:** 32 bytes (AES-256).
- **Nonce:** 12 bytes (`gcm.NonceSize()`), freshly generated per encryption via
  `crypto/rand.Read`. A unique nonce per write is mandatory for GCM security; with
  random 96-bit nonces and the tiny write volume here (a handful of secret writes
  over a deployment's life) reuse probability is negligible.
- **Stored blob layout:** `nonce (12 bytes) || ciphertext || GCM tag (16 bytes)`
  as one opaque BLOB. `gcm.Seal(nonce, nonce, plaintext, aad)` returns
  `nonce || ciphertext || tag` directly when the prefix is `nonce`; decryption
  splits the first 12 bytes off as the nonce and feeds the remainder to
  `gcm.Open`.
- **Associated data (AAD):** bind each ciphertext to its `name` by passing the
  secret name as GCM additional data. This prevents a swap attack (moving the
  encrypted `webhook_api_key` blob into the `musixmatch_token` row). Low cost,
  real benefit; recommended.

**Why AES-256-GCM over NaCl secretbox** (XSalsa20-Poly1305,
`golang.org/x/crypto/nacl/secretbox`): both are sound AEAD choices. AES-256-GCM
wins here because it is **pure standard library** - the repo already leans on
stdlib crypto (PBKDF2-SHA256, sha256 in `internal/auth`) and adds no new
dependency or supply-chain surface, and AES-NI hardware acceleration is
irrelevant at this volume but free. secretbox's nonce-misuse story is no better in
practice. No CGO is involved either way (a hard repo constraint); both are pure
Go.

## Schema

A new general `secrets` table. Migration **017** (016 is already taken by
`work_queue_detect_instrumental`), following the additive-migration and
`strftime` timestamp conventions of migration 007 (`api_key_metadata`).

```sql
-- +goose Up
-- +goose StatementBegin
-- Encrypted-at-rest secret store. Each row holds one secret as an AES-256-GCM
-- blob: nonce(12) || ciphertext || tag(16). The encryption key is NOT stored
-- here (see docs/design/2026-06-13-223-secrets-encryption.md). General store:
-- `name` is a stable identifier; v1 uses 'musixmatch_token' and 'webhook_api_key'.
CREATE TABLE secrets (
    name       TEXT PRIMARY KEY,
    ciphertext BLOB NOT NULL,
    updated_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS secrets;
-- +goose StatementEnd
```

- `name TEXT PRIMARY KEY` - stable secret identifier. Upsert via
  `INSERT ... ON CONFLICT(name) DO UPDATE`, so a re-import overwrites cleanly.
- `ciphertext BLOB NOT NULL` - the full `nonce || ciphertext || tag` blob. No
  separate nonce column (the nonce is the blob prefix), no plaintext column ever.
- `updated_at` - set on write; useful for the Config view and future rotation
  auditing. (The repo sets timestamps explicitly in SQL rather than via triggers;
  the repository's upsert writes `updated_at` on each store.)

A `secrets` repository in `internal/secrets` (repository-over-interface pattern,
like `internal/cache` and `internal/auth/store.go`) exposes `Set(ctx, name,
plaintext)`, `Get(ctx, name) (plaintext, ok, err)`, and `Delete(ctx, name)`,
performing encrypt-on-set / decrypt-on-get internally so callers never see
ciphertext. An in-memory store mirrors the `auth.MemoryStore` pattern for tests.

No CGO; `modernc.org/sqlite` + goose only.

## Key management (the central decision)

The encryption key is a 32-byte AES-256 key, resolved from these sources in
precedence order (first present wins):

1. **Key file** - 32 raw bytes, auto-generated on first use via `crypto/rand` and
   written `0600`. Default location `xdgDataPath("mxlrcgo-svc", ".mxlrcgo.key")`
   (hidden file alongside the DB). Overridable via the `secrets.key_file` config
   field / `MXLRC_SECRETS_KEY_FILE` env to point at a separate volume. This is the
   **universal zero-setup default on all platforms, including Docker**.
2. **`MXLRC_MASTER_KEY` env var** - base64-encoded 32 bytes. When set, it takes
   precedence and the key file is neither read nor written. This is the opt-in
   path for operators who want the key off the data volume entirely (the recommended
   Docker hardening posture when threat model includes whole-volume theft).

**Decision A is (a) - universal auto-generated key file (REVISED 2026-06-14, #238).**
The original signed-off decision was (c): native gets an auto key file, Docker
refuses to auto-create one and requires `MXLRC_MASTER_KEY`. Decision A has been
revised to (a): the auto-generated `0600` key file is the universal default on
all platforms including Docker. `MXLRC_MASTER_KEY` (and `MXLRC_SECRETS_KEY_FILE`)
are optional overrides. Rationale: (c) imposed Docker-specific code complexity and
a first-run print-and-exit flow; (a) eliminates that complexity while still
defending the primary threat - DB-only exfiltration (backups, accidental commits,
stray `.db` copies) - on all platforms. For operators who want stronger key/data
separation (whole-volume theft protection), setting `MXLRC_MASTER_KEY` or pointing
`MXLRC_SECRETS_KEY_FILE` at a separate mount is straightforward and fully supported;
it is documented as the recommended Docker hardening posture, not a requirement.

On read of an existing key file, if permissions are looser than `0600` the daemon
emits an `slog` **warning** (not fatal; permission bits are unreliable on
Windows/some bind-mounts). Per the no-silent-failure rule, a missing-but-required
key, a malformed `MXLRC_MASTER_KEY` (wrong length / bad base64), or an
unreadable/unwritable key file is a **loud, fatal** startup error - never a silent
fallback to "no encryption".

**OS keyring is rejected** (CR design choice 2): a headless container has no
keyring daemon, and it adds a CGO/dbus dependency surface the project avoids.

### The Docker co-exfiltration reality (honest documentation)

In Docker mode `xdgDataPath` and `xdgConfigPath` both resolve to **`/config/`**
(`internal/config/config.go`). So the default key file at `/config/.mxlrcgo.key`
sits in the **same bind-mount** as the DB at `/config/mxlrcgo.db`. An attacker
who copies that one volume gets both the ciphertext and the key - the at-rest
encryption then only protects against DB-*only* leakage (backups, accidental git
commit, a stray copy of just the `.db`), not whole-volume theft.

This is the accepted tradeoff under revised Decision A=(a): the auto-generated
colocated key file is the zero-setup default on all platforms. It defends the
DB-only-exfiltration threat, which is the most common real-world scenario.
Operators who want stronger protection (whole-volume-theft defense) can opt in:

```yaml
# docker-compose.yml - optional hardening: key separated from the data volume
services:
  mxlrcgo-svc:
    image: ghcr.io/sydlexius/canticle:latest
    environment:
      # Optional. base64 of 32 random bytes: openssl rand -base64 32
      # When set, takes precedence over the key file; keeps key off the data volume.
      MXLRC_MASTER_KEY: ${MXLRC_MASTER_KEY}   # from a .env NOT in the data volume
    volumes:
      - ./config:/config
```

```yaml
    # Alternative: key file on a separate, narrower mount
    environment:
      MXLRC_SECRETS_KEY_FILE: /run/secrets/mxlrcgo_key
    secrets:
      - mxlrcgo_key
```

The honest framing: the env-key or separate-mount-key path is what buys key/data
separation. The default colocated key file is still a significant improvement over
plaintext-in-config: it defends DB-only-exfiltration (backups, accidental commits,
stray `.db` copies). For whole-volume-theft protection, set `MXLRC_MASTER_KEY` or
point `MXLRC_SECRETS_KEY_FILE` at a separate mount. This is documented as the
recommended Docker hardening posture, not a requirement.

## Precedence and migration

### Precedence (DB is lowest, backward compatible)

The encrypted DB store slots in as the **lowest-priority** source, so every
existing env/TOML deployment behaves identically:

- Musixmatch token: `--token` CLI > `MUSIXMATCH_TOKEN` env > `MXLRC_API_TOKEN`
  env > TOML `api.token` > **DB `secrets`**.
- Webhook key: CLI flag > `MXLRC_WEBHOOK_API_KEY` env > TOML
  `server.webhook_api_keys` > **DB `secrets`**.

A higher-precedence source is used at runtime but is **never** auto-persisted to
the DB. Persistence is an explicit operator action (the import command). This
avoids surprise writes and keeps "what's encrypted at rest" something the operator
opted into.

### Migration (explicit import, never automatic)

A new CLI command moves the current effective plaintext secret into the encrypted
store:

```
mxlrcgo-svc secrets import            # imports the currently-resolved token + webhook key
mxlrcgo-svc secrets import --token    # just the Musixmatch token
mxlrcgo-svc secrets set webhook_api_key   # prompt-driven entry (no value on argv)
mxlrcgo-svc secrets list              # names + updated_at only, NEVER values
```

- `import` reads the currently effective secret(s) (resolving the precedence
  above, but skipping the DB tier as a source) and writes them encrypted.
- Idempotent: re-running upserts (overwrites) the existing row.
- After a successful import, the command prints a reminder to **remove the
  plaintext** from `config.toml` / compose env.
- **No automatic import on startup** - explicit user action only, to avoid
  silently copying a secret into a new store the operator did not ask for.
- `secrets set` accepts the value via interactive prompt or stdin, never as an
  argv argument (argv lands in shell history and `ps`).

### Logging and redaction

- Decrypted values never touch logs. The Musixmatch token is already redacted in
  the startup-config dump and the Config view (`IsSensitiveConfigKey` /
  `render.go`); extend the same treatment so a DB-sourced token/webhook key is
  redacted identically, and add `secrets.key_file` content (the key) to the
  never-log set.
- `secrets list` prints names and `updated_at` only.
- The `secrets` repository returns decrypted plaintext only to its direct callers
  (serve startup, fetch command); it never returns ciphertext or the key.

## Rotation

**Deferred to a follow-up issue, not v1.** Rationale: a correct rotation story
(re-encrypt every row under a new key, atomically, with rollback on failure, plus
a `secrets rekey` command and a documented key-loss recovery path) is a
self-contained chunk that should not gate the core store. v1 ships the import +
encrypt/decrypt + precedence path, which is the load-bearing functionality #204's
onboarding needs.

What v1 must do to keep rotation cheap later: store the full self-describing blob
(`nonce || ct || tag`) per row so a future rekey just decrypts-with-old /
encrypts-with-new per row; and reserve the option to add a `key_version` column
later without a rebuild (additive `ADD COLUMN`). v1 does **not** add `key_version`
now (YAGNI). The follow-up issue is filed when rotation is scheduled; this doc
flags it.

Key-loss note for the docs: losing the key (deleted `.key` file, lost
`MXLRC_MASTER_KEY`) makes the encrypted secrets unrecoverable by design - the
remedy is to re-enter them (re-run `secrets import`/`set` with the original
plaintext, or re-onboard via #204). This must be called out in operator docs.

## Onboarding tie-in (#204)

The #204 onboarding UI is the natural place to *enter* these secrets. The storage
format defined here (the `secrets` table + the `internal/secrets` repository API)
is exactly what that UI writes: the onboarding flow calls `secrets.Set(ctx,
"musixmatch_token", value)` / `secrets.Set(ctx, "webhook_api_key", value)` rather
than inventing its own persistence. The repository is the single write path for
both the CLI `secrets` command and the web onboarding form.

## Open questions resolved

Each Open Question from #223, with a recommended answer. Items tagged
**[SIGN-OFF]** need the maintainer's explicit decision before implementation.

- **Q: Key-management default - `.key` file vs env master key vs keyring?**
  A: Support env (`MXLRC_MASTER_KEY`) and key file; env wins when set. Keyring
  rejected (headless/CGO). Recommended *posture*: env-key for Docker, auto key
  file for native. **[SIGN-OFF]** - see options below.
- **Q: Where does the key file live vs the DB in a Docker bind-mount?**
  A: By default `/config/.mxlrcgo.key`, i.e. the same `/config` mount as the DB
  (co-exfiltration risk, documented). Recommendation is to *not* rely on that for
  Docker and use `MXLRC_MASTER_KEY` (or `MXLRC_SECRETS_KEY_FILE` on a separate
  mount) instead. **[SIGN-OFF]** on whether the colocated-key-file default is
  acceptable as the native default given the documented limitation.
- **Q: Only these two secrets, or a general store?**
  A: General `secrets` table + reusable repository API, only the two secrets wired
  now. (Resolved; matches CR design choice 3.)
- **Q: Key rotation / re-encrypt - v1 or follow-up?**
  A: Follow-up. v1 stores self-describing blobs so rotation stays a per-row
  decrypt/re-encrypt later. (Resolved; flag a follow-up issue when scheduled.)

### Maintainer sign-off decisions (DECIDED 2026-06-13; Decision A revised 2026-06-14 by #238)

- **Decision A - default key source. REVISED: (a). Original: (c) + first-run key hint.**
  - **(a) CHOSEN (revised 2026-06-14, #238).** Auto-generated `0600` key file as
    the universal zero-setup default on all platforms including Docker.
    `MXLRC_MASTER_KEY` is an optional override for key/data separation. Rationale:
    eliminates Docker-specific code complexity (no DockerMode field, no
    FirstRunError, no print-and-exit flow) while still defending the primary
    threat. For whole-volume-theft protection, `MXLRC_MASTER_KEY` or a separately-
    mounted key file is documented as the recommended Docker hardening posture.
  - (b) `MXLRC_MASTER_KEY` required (no auto key file at all). Not chosen.
  - (c) ~~PREVIOUSLY CHOSEN.~~ Auto key file native only; Docker refuses and requires
    `MXLRC_MASTER_KEY` with a first-run onboarding wizard. Revised to (a): the
    Docker-specific path added complexity without improving the common-case
    security posture (see "The Docker co-exfiltration reality" section).
- **Decision B - AAD binding. DECIDED: yes.** Bind each ciphertext to its `name`
  via GCM AAD (prevents row-swap). Low cost, real benefit.
- **Decision C - webhook-key model. DECIDED: yes.** The static configured webhook
  key(s) are stored *encrypted/recoverable* here (so they can be viewed/rotated),
  distinct from the existing *hashed* generated keys in `api_key_metadata`, which
  stay one-way and untouched (out of scope).

## Non-goals

- Not user-login auth or sessions (that is #204).
- Not an external secrets manager (Vault/KMS/SOPS).
- Not migrating the existing hashed `api_key_metadata` (generated keys stay
  one-way).
- Not key rotation in v1 (follow-up).

## Decomposition

This document is the design of record; implementation is one tracked issue that
references it (a separate issue is required per the design-doc rule). Suggested
implementation phases inside that issue:

1. **Crypto + store foundation**: `internal/secrets` package (AES-256-GCM
   encrypt/decrypt helpers), migration 017 `secrets` table, repository + in-memory
   store, key-resolution (`MXLRC_MASTER_KEY` > key file, loud failures). Universal
   key-file auto-create on all platforms (no Docker branch).
2. **Precedence wiring**: add DB as lowest-precedence source for the Musixmatch
   token and webhook key; redaction parity for DB-sourced values.
3. **CLI**: `secrets import` / `set` / `list` (no values in argv or logs).
4. **Docs**: README + compose examples (zero-setup key-file default; MXLRC_MASTER_KEY
   as optional hardening), key-loss recovery note.

## Testing

- **Crypto**: round-trip encrypt/decrypt; tampered blob (flip a byte) fails
  `gcm.Open`; wrong key fails; AAD mismatch (swap names) fails; nonce uniqueness
  across writes.
- **Store**: repository round-trip against real in-memory SQLite
  (`file::memory:?cache=shared`), upsert overwrites, `Get` of an absent name
  returns not-found, `updated_at` advances on re-set.
- **Key resolution**: env wins over file; malformed `MXLRC_MASTER_KEY` is fatal;
  auto-generate-on-first-use writes `0600`; loose-perms warning fires; auto-create
  works identically regardless of Docker env state (universal default, all platforms).
- **Precedence**: DB used only when all higher tiers absent; higher tier never
  auto-persists; redaction covers DB-sourced values.
- **CLI**: `import` is idempotent; `list` never prints values; `set` rejects a
  value passed on argv.
- Patch coverage >= the repo's 70% gate per lane; storage tests use real SQLite,
  no mocks (repo convention).
