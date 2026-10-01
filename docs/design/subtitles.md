# Subtitle sidecars

What `subtitle_sidecars: text` writes beside a replacement, the gate a sidecar passes before it
is written, and why a refused sidecar does not stop a swap. This document is that argument's
single home: `CLAUDE.md` names the rule and links here rather than restating it. The key itself
is described in [`docs/profiles.md`](../profiles.md#subtitle-sidecars); the code is
`internal/subtitle`.

Nothing here relaxes a gate. The replacement is held to exactly the gates it always was
([`docs/design/swap.md`](swap.md)), and still carries every subtitle stream its intended stream
map carries, stream-copied, as before. A sidecar is an added file, not a change to the
replacement.

## The rule

<a id="sidecars"></a>

**Under `subtitle_sidecars: text`, every carried SubRip, ASS and WebVTT stream is also copied,
unconverted, to `<name>.<lang>[.forced].<ext>` beside the replacement, only after the swap has
committed, and never over an existing file.** With the key at `off` (the default) none of this
runs: no subtitle is probed for it, nothing is written, and the profile digest is the one a
build without the key computed.

- **Which streams.** The carried ones: the subtitle streams of the job's intended stream map,
  after `subtitle_languages` and `keep_commentary` have been applied. A dropped stream gets no
  sidecar.
- **Which formats.** SubRip goes to `.srt`, ASS to `.ass`, WebVTT to `.vtt`, each by a stream
  copy (`ffmpeg -i <source> -map 0:<index> -c copy -f <srt|ass|webvtt> <temp>`) into its own
  native format. A copy across formats is refused by ffmpeg's muxers, and a conversion is not a
  copy (the citations are in `internal/subtitle`).
- **Skipped with a reason.** Picture-based subtitles (`hdmv_pgs_subtitle`, `dvd_subtitle`,
  `dvb_subtitle`, `xsub`) have no text to copy: `bitmap-subtitle`. MP4 timed text (`mov_text`)
  has no native text sidecar format, and converting it is not what this key does:
  `mov-text-not-converted`. Any other codec: `subtitle-codec-not-supported`.
- **The name.** `<name>` is the replacement's own name without its extension (so a source whose
  container changes gets sidecars named after the file that replaced it). `<lang>` is the
  stream's language tag as the source wrote it, lowercased, and `und` where it wrote none; a tag
  that is not a 2-3 letter code with optional hyphenated subtags is skipped
  (`language-tag-not-a-name`) rather than cleaned into a language nobody wrote. `.forced` is
  present exactly when the stream carries the forced disposition. Sonarr and Radarr parse this
  form (`SubtitleLanguageRegex`, `src/NzbDrone.Core/Parser/LanguageParser.cs` line 26 on
  `develop`,
  https://raw.githubusercontent.com/Sonarr/Sonarr/develop/src/NzbDrone.Core/Parser/LanguageParser.cs,
  read 2026-10-01), and so does Jellyfin (`[filename].[flag].[language].[flag].[extension]`,
  example `Film.default.en.forced.ass`,
  https://raw.githubusercontent.com/jellyfin/jellyfin.org/master/docs/general/server/media/_video-external-streams.md,
  read 2026-10-01). Plex's acceptance of a three-letter code is `ASSUMED` (its article was not
  readable when the research was done).
- **Never overwritten.** A name already on disk is skipped (`sidecar-exists-never-overwritten`)
  and its bytes are not touched. A name an earlier stream of the same job already took (two
  English SubRip tracks, say) is skipped for the later stream
  (`sidecar-name-taken-by-earlier-stream`). The final name is only ever created by `link(2)`
  from the gated temp, which fails on an existing target, so a file that appears at the name
  between the check and the publish is not overwritten either.
- **Fonts.** An ASS track in Matroska usually leans on the file's font attachments, which a
  sidecar cannot carry. Where the source carries a font attachment, an ASS sidecar is written,
  logged at `warn` as losing its fonts, and recorded with `fonts_lost`. The embedded track in the
  replacement keeps them.

### Sidecars rename, move and delete nothing

The README's library-manager non-goal is about renaming, moving and deleting files, and a
sidecar does none of those: it is a new file at a name nothing occupies. That is why the key is
not a reversal of that non-goal (brief I3). It is still a write into a library this tool
otherwise only replaces files in, so it gets the protections a write there needs: its own gate
(below), exclusive creation, and the swap's ordering.

### Crash safety

Each stream is extracted to a temp in the source's own directory, named after the job's working
file with a `.subtitle<n>` suffix (`<stem>.__transcoding__.<ext>.holdfast-part.subtitle<n>`).
That name carries the temp marker, so a run killed at any point leaves only files the stale-temp
sweep removes, and it ends in no extension a media server reads as a subtitle or a video. The
temp is written, parsed back and `fsync`ed before it is linked to its final name, and the
directory is synced after, so no partial file ever appears under a sidecar's final name. Every
way out of a job that does not reach a committed swap removes the temps.

## The gate

<a id="sidecar-gate"></a>

**A sidecar is published only when the written file parses back with ffprobe as exactly one
stream of its own format and its event count equals the source stream's.** The source's counts
are read once per job (`ffprobe -count_packets -select_streams s`); the sidecar's by reading the
written file with the format's own demuxer. A file that does not parse, parses as another codec,
or carries a different number of events is not published (`sidecar-parse-back-failed`,
`sidecar-event-count-mismatch`), and neither is one whose source count could not be read
(`source-event-count-unreadable`). The count is cheap and it is what catches the failure that
matters here: a truncated write.

## Why a refused sidecar does not block the swap

A sidecar that fails its gate, or cannot be extracted, made durable or linked, is not published,
is logged at `warn` with its reason, and is recorded on the job's row. The job still swaps. The
swap's gates are about the replacement, and the replacement still carries the very stream the
sidecar would have copied, stream-copied and checked by stream-count parity like every other
carried stream. Nothing is lost by the missing sidecar: it is an added convenience copy, and
refusing the replacement over it would trade the space the job reclaims for a file that would
not have held anything the replacement lacks.

## What the row records

A terminal `done` row under `subtitle_sidecars: text` carries `subtitle_sidecars`: per carried
subtitle stream, its source index, codec, language, forced flag, and either the published path or
the reason token (with the error text for a failure), the event count the gate compared, and
`fonts_lost`. With the key off it is `null` (not recorded), and `[]` is a job whose source
carried no subtitle stream. The field is in `docs/api-reference.md`.

Sidecars are written only by a job that encodes, so files already replaced before the key was
turned on get none.
