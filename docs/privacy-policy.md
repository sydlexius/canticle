# Privacy Policy

This page documents what data leaves your machine when you run `canticle`,
and what does not.

## Musixmatch (default provider)

**What is sent.** Each lyrics lookup sends an HTTPS request to the Musixmatch
API. The request includes the following as URL query parameters:

- Track name, artist name, and album name (always present; may be empty strings
  if the audio file lacks those tags).
- Your API token (`usertoken`).
- Fixed application parameters (`app_id`, `format`, `namespace`,
  `subtitle_format`) that identify the client type; these contain no personal
  data.
- Recording-level identifiers, sent only when available from the audio file's
  tags, to help Musixmatch match the exact recording rather than a cover or
  alternate version:
  - Track duration in seconds (`q_duration`) - sent when the file's tag
    reports a non-zero duration.
  - Recording ISRC (`track_isrc`) - sent when the file carries an ISRC tag.
  - Spotify track ID (`track_spotify_id`) - sent when the file carries a
    Spotify ID tag.

**Credentials scope.** The Musixmatch token is read from the CLI flag, the
`MUSIXMATCH_TOKEN` environment variable, or the TOML config file, in that order
of precedence. It is transmitted only to `apic.musixmatch.com`; it is never
sent anywhere else.

## PetitLyrics (optional provider)

**What is sent.** When PetitLyrics is enabled, each lookup sends track name and
artist name to the PetitLyrics API. No credentials are required or transmitted
for this provider.

## Network metadata

Like any application that makes internet requests, Canticle does not
control what standard network metadata a remote server or intermediary receives.
When the app contacts a lyrics provider, that provider - and any network
intermediary on the path - inherently receives the request's source IP address
alongside the query content. This is true of any internet request; it is not
additional data the app chooses to send.

## Local cache

**What stays local.** Lookup results are stored in a local SQLite database
(path resolved via XDG conventions). Nothing in the cache is transmitted
anywhere. Subsequent lookups for the same track are served from this local cache
without contacting any external API.

## Telemetry, analytics, and crash reporting

**None.** Canticle does not collect telemetry, analytics, or crash
reports. No data is ever sent to the project maintainers or any third party
other than the lyrics providers listed above.

## Summary

| Data                                      | Destination                |
|-------------------------------------------|----------------------------|
| Track name, artist name                   | Active lyrics provider API |
| Album name                                | Musixmatch API only        |
| Track duration (when tag present)         | Musixmatch API only        |
| Recording ISRC (when tag present)         | Musixmatch API only        |
| Spotify track ID (when tag present)       | Musixmatch API only        |
| Musixmatch token                          | Musixmatch API only        |
| Lookup results                            | Local SQLite cache         |
| Telemetry / analytics                     | Not collected              |

## Cross-references

- `internal/musixmatch/client.go` - Musixmatch HTTP client (query parameters and token handling)
- `internal/petitlyrics/client.go` - PetitLyrics HTTP client (no credentials transmitted)
- `internal/cache/` - local SQLite cache repository
