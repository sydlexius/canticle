# Multilingual Lyric Output Policy

## Decision

**Bilingual single `.lrc`** - original and translation lines share one timestamp,
interleaved as alternating lines under the same `[MM:SS.cc]` marker.

```text
[00:12.50]オリジナルの歌詞テキスト
[00:12.50]Translation of the lyric text
```

## Player compatibility

`song.<lang>.lrc` sidecar naming is not a recognized convention in Emby or
Jellyfin. Both servers match only `song.lrc` (or embedded `SYLT`/`USLT` tags).
Emitting separate per-language files means the secondary file is silently ignored
by every media server tested. The bilingual interleaved format is the established
convention for CJK lyrics players (e.g. Apple Music, Spotify) that display
dual-language lines.

Rejected options:
- **Primary-language only** - loses translation value entirely; rejected.
- **Emit both** (`song.lrc` + `song.<lang>.lrc`) - secondary file ignored by
  Emby/Jellyfin; adds file-management complexity for no player benefit; rejected.

## Future writer contract

These are the changes required in `models` and `lyrics` to support bilingual
output. No implementation yet - these are the specified shapes for when CJK
provider work lands.

**`models.Song`** gains two optional parallel fields:

```go
type Song struct {
    Track                Track
    Lyrics               Lyrics
    Subtitles            Synced  // existing: original synced track
    TranslationSubtitles Synced  // new: translation track (zero = absent)
    RomanizationSubtitles Synced // new: romanization track (zero = absent)
}
```

Zero-value `Synced` (empty `Lines` slice) means absent; no pointer indirection
needed. This is consistent with the existing `Subtitles Synced` and `Lyrics`
fields, which already signal absence by content rather than by a nil pointer.
Note: issue #149's coding plan proposes pointer fields (`Translation *Lyrics`,
`TranslationSynced *Synced`, `Romanization *Lyrics`); that shape is rejected
here in favor of the value-typed fields above, and #149 should adopt these.

**Default behavior: original-only.** When a provider returns a non-empty
`TranslationSubtitles`, `writeSyncedLRC` writes the original track only and
ignores the translation track unless bilingual output is explicitly enabled.
This keeps existing single-language output stable once providers begin returning
translation data.

**Bilingual interleaved output (opt-in).** When the config flag
`bilingual_output = true` is set AND `TranslationSubtitles` is non-empty,
`writeSyncedLRC` merges the tracks: each timestamp emits the original line
immediately followed by the translation line at the same timestamp (the format
in the Decision section above). The `Writer` interface signature is unchanged;
the merge logic is internal to `writeSyncedLRC`. The two-condition gate (flag
AND non-empty track) is what makes bilingual output opt-in.

## Forward compatibility with script guard

The `langguard` package (`internal/langguard`) provides `ScriptOf` (Unicode
script classifier) and `Guard` (script-allowlist filter over `models.Song`).
`Guard.Accept(song)` scores the concatenated original body (`Subtitles` lines
plus `Lyrics` body, credit lines stripped) against an `accepted_scripts`
allowlist and a foreign-script-share threshold, returning `(ok, reason)`. It
has no per-track concept today: it sees only the original track.

Default behavior when translation is present: the script guard operates on the
**original track only**. If the original passes the guard, only the original is
written (which, combined with the original-only writer default above, is the
conservative behavior). The guard never silently promotes a translation to
primary.

Translation-as-primary is an explicit opt-in config flag (e.g.
`prefer_translation = true` in `[provider]` TOML), separate from
`bilingual_output`. When enabled, if the original fails the guard but the
translation passes, the translation becomes the primary output line and the
original is dropped. This is a conscious user choice, not an automatic fallback.
Evaluating the translation track would require a per-track guard helper that
does not exist yet; it is future work.

**Unmet prerequisites.** This section describes intended behavior, not current
behavior. As of this writing the `langguard` package exists with tests but is
not wired in: there is no `accepted_scripts` (or threshold) TOML config, and
nothing in `internal/worker` or `cmd/` calls `Guard.Accept`. Likewise
`bilingual_output` and `prefer_translation` do not exist in the config layer.
Wiring `langguard` into config and the worker pipeline is a prerequisite that
must land before any of the opt-in behavior above can work (see the langguard
integration issue).

## Cross-references

- Issue [#146](https://github.com/sydlexius/mxlrcgo-svc/issues/146) - decision record
- Issue [#149](https://github.com/sydlexius/mxlrcgo-svc/issues/149) - CJK provider adapter (depends on this policy)
- `internal/langguard` - script classifier and guard
- `internal/lyrics/writer.go` - current writer (single-track, no translation)
- `internal/models/models.go` - `Song`, `Synced` types
