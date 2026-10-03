# Saying something different for part of a library

The top-level `encoder`, `crf`, `preset`, `pixel_format`, `container_ext` and
`bitrate_kbps` are what every file in every library root is transcoded under. This page
is how you say something different for part of a library: different encode settings
(`encode_profiles`), a target bitrate instead of a quality target (`bitrate_kbps`),
different thresholds for a band of source resolutions (`rules`), or nothing at all for part
of a root (`exclude_paths` and `include_paths`). Two keys here are not about what is done to a
file: `watch` is about how it is FOUND (a filesystem watch for that root), and `priority` is
about how EARLY it is offered ([queue priority](#queue-priority)).

Everything here is reachable from the config file and its `HOLDFAST_*` environment
override, and from nowhere else: `run`, `serve` and `validate` gain no flags.

## `encode_profiles` - an ordered, first-match list

```yaml
crf: 22                    # everything else keeps today's HEVC at crf 22
encode_profiles:
  - name: 4k-av1
    match: "**/4K/**"      # everything under any directory named 4K
    encoder: svtav1
    crf: 32
  - name: bulk-tv
    match: "**/TV/**/*.mkv"
    bitrate_kbps: 3000     # this profile's jobs run at a TARGET BITRATE instead
```

The **first** profile whose `match` selects a source supplies that job's settings, laid
over what that file would otherwise be encoded at. A later matching profile has no effect
on that job; a setting the matching profile does not override keeps the value it
inherited; and a source no profile matches is transcoded under the settings it inherited
- never skipped, never failed. With no `encode_profiles` at all, every job resolves to
exactly what it inherited.

### What it is laid over: five layers, innermost last

1. the built-in default
2. the top-level configuration (the YAML file, then `HOLDFAST_*`)
3. the profile of the **library root** the file lives under (a `library_roots` entry
   spelled as a mapping - see `config.example.yaml`)
4. the first **resolution rule** of that root whose band admits the file, for the knobs it
   names - `encoder` among them (see [`rules`](#resolution-rules))
5. the **encode profile** whose pattern selected this file

The last layer to mention a key decides it. So an encode profile is laid over the ROOT's
resolved values and not over the top level: a root at `crf: 20` whose files match a
profile that sets only `encoder` encodes at that root's 20, not at the top level's. And an
encode profile that names an `encoder` beats a rule that names one, exactly as its `crf`
beats a rule's `crf`; one that names no encoder leaves the rule's.
`holdfast validate` prints layers 1 to 3 per root, knob by knob, with the layer each
value came from, and each root's rules beneath them.

An encode profile may only change what the encoder PRODUCES. The gates a library root
carries - the VMAF floors, the savings floor, the bitrate floor, the hardlink guard -
are never reachable from one, so no pattern over a filename can move a gate that decides
whether a source is destroyed.

### What `match` matches

A glob over the source's path:

- a pattern with **no `/`** is matched against the file's **basename** (`*.mkv`)
- a pattern **containing `/`** is matched against the **whole path**, segment by
  segment. `*`, `?` and `[...]` do not cross a separator; `**` matches zero or more
  whole segments (`**/4K/**`)
- an **empty** `match` selects every source: the catch-all, only ever reached by what no
  earlier profile matched

A pattern that cannot be parsed is an error at **every** path, never a silent non-match
for the files that happen to fall out of the scan early.

### What a profile may carry, and what it may not

A profile accepts exactly `name`, `match`, `encoder`, `crf`, `preset`, `pixel_format`,
`container_ext`, `bitrate_kbps` and `priority` (which orders the files it decides and is not an
encode setting - see [queue priority](#queue-priority)). **Any other key is a startup refusal naming the
profile and the key** - the unknown-key check bites inside a profile, not only at the
top level, because `encodr: svtav1` nested in one is the same typo with the same
consequence. So is an unknown encoder, a `crf` outside 0-51, a `container_ext` carrying
a dot or a slash, an empty or duplicate name, and a match pattern that cannot be parsed.
Each refusal names the profile and the offending key and value, exits non-zero, and
creates, renames or removes nothing under any library root.

A profile selects what the encoder **produces**, and nothing else. There is deliberately
no per-profile VMAF threshold, undo window or retention setting: a profile must never be
able to move a gate that decides whether a source is destroyed.

### A profile's encoder is checked before the run starts

Every distinct encoder the configuration can reach - the top-level one, each root's,
each profile's override, and each encoder a resolution rule names - is tested against
this host at startup, before anything is encoded and before the job store is opened. A
hardware encoder with no matching device, or an ffmpeg build without the codec,
**refuses the run** and the account names what asked for it: the profile, or the library
root and the rule's index. That is the same loud failure the top-level
encoder has always had, and it is never a silent fallback to `cpu`: some hardware
encoders exit 0 while writing nothing, so the alternative is a library's worth of
files failing one at a time, hours in.

### The skip decisions move with the profile

A source already in the **top-level** target codec whose matching profile targets a
**different** codec is transcoded rather than skipped, and its output is accepted only
if it carries the **profile's** target codec. Both questions are decided against the
codec that job's own encoder produces, not against a run-global one.

### What ran is on the record

The profile that decided a job is recorded on its **terminal ledger row** and on the
`holdfast export` NDJSON row (`profile`, empty for the top-level settings), for a skip
as well as for a swap. `encoder` alone stops answering "what ran" the moment two
encoders can run in one scan. `""` is a real value there and not a missing measurement,
so the key is always present - see
[docs/api-reference.md](api-reference.md#the-recorded-outcome---the-proof-a-swap-was-safe).

The re-derivation inputs beside the name are **this job's own settings**, resolved for that
file's path through the profile that selected it. So a terminal row is stale when, and only
when, a key that row recorded resolves to a different value under the configuration now in
force for the same path: editing a profile offers back the files whose recorded values it
moved, and leaves alone every row whose guard never read the key you changed. The rule and
what it costs are in [docs/requeue.md](requeue.md#what-a-row-records).

## `bitrate_kbps` - a target bitrate instead of a quality target

```yaml
bitrate_kbps: 0            # the default: keep the crf/quality target
```

A positive value, top-level or per-profile, selects a **target-bitrate rate control** at
that many kbps for the affected jobs, and then **no quality target is passed to the
encoder at all** - no `-crf`, `-cq`, `-global_quality`, `-qp`, `-qp_i`/`-qp_p` and no `-rc cqp`,
whatever `crf` or [`quality`](#quality---each-hardware-encoders-quality-on-its-own-scale) says. A rate
control and a quality target are two different instructions, and passing both would
leave which one wins to the encoder rather than to you.

It is announced at startup as a **note**, not a warning: every no-loss gate still runs,
and an encode that misses one is still rejected with the source untouched. `0` (the
default, and what an absent key means) keeps the quality target, so a config written
before this setting existed produces byte-identical encoder arguments.

The value must be a whole number of kbps. `8000.5`, a quoted string with a unit, a
boolean, a list, a negative, or the key with no value at all is a startup refusal naming
the key and the value, never a silently truncated bitrate.

## `quality` - each hardware encoder's quality on its own scale

```yaml
crf: 22                    # libx265 and libsvtav1, unchanged
quality:                   # optional, top-level only, keyed by registry key
  nvenc: 24                # -cq, 1-51
  av1_nvenc: 30            # -cq, 1-63
  qsv: 24                  # -global_quality (ICQ), 1-51
  vaapi: 24                # -qp (constant QP), 1-52
  amf: 24                  # -qp_i and -qp_p under -rc cqp, 0-51
  h264_nvenc: 24           # -cq, 1-51
  h264_qsv: 24             # -global_quality (ICQ), 1-51
  h264_vaapi: 24           # -qp (constant QP), 1-51
  h264_amf: 24             # -qp_i and -qp_p under -rc cqp, 0-51
  av1_qsv: 24              # -global_quality (ICQ), 1-51
  av1_vaapi: 100           # -global_quality under -rc_mode CQP (AV1 quantiser index), 1-255
  av1_amf: 100             # -qp_i and -qp_p under -rc cqp (AV1 q_index), 0-255
```

`crf` is a libx265 and libsvtav1 rate factor. Before this key, the same number was also
passed as NVENC's `-cq`, QSV's `-global_quality`, VAAPI's `-qp` and AMF's `-qp_i`/`-qp_p`:
five different instructions - a rate factor, a constant-quality target, an ICQ level and
two fixed quantisers - on scales that do not even share a range. `quality.<key>` sets a
hardware encoder's value on **that encoder's own scale**. The values above are placeholders,
not recommendations.

- **An absent key inherits the job's effective `crf` unchanged**, which is what the
  encoder was always handed, so a configuration written before this key produces
  byte-identical encoder arguments. That default is not measured to mean anything
  comparable on any hardware encoder (ASSUMED): it is kept only so nothing moves, and a
  hardware report per encoder is what will calibrate it.
- **Each scale is the encoder's own**, read from the pinned ffmpeg (`ffmpeg -h
  encoder=<name>`) and its source, and narrowed only past the values the encoder does not
  read as a target: NVENC's `-cq 0` means "automatic" and VAAPI's `-qp 0` means "unset", so
  both scales start at 1. `internal/encoder/quality.go` holds each scale with its sources.
- **`holdfast validate` refuses** a value off its encoder's scale naming the key and the
  scale (`quality.vaapi 53 is outside hevc_vaapi's scale (-qp 1-52)`), `quality.cpu` and
  `quality.svtav1` (those are set by `crf`), an ffmpeg codec name in place of the registry key
  (`quality.hevc_nvenc`), any other key, and a value that is not a whole number.
- **An inherited `crf` off a scale fails that job** when its encode plan is derived, naming
  `quality.<key>` and the scale: `crf: 0` under `encoder: nvenc` would be `-cq 0`, which is not
  a quality target, and nothing is encoded. Set the encoder's own key.
- **A job reads its own encoder's key.** An encode profile that switches a job to `nvenc`
  gets `quality.nvenc`; a job named by an ffmpeg alias (`encoder: hevc_nvenc`) reads the
  registry key's entry. A target bitrate (`bitrate_kbps`) passes no quality value at all, so
  none is read.
- **Top-level only.** A library root or an encode profile carrying `quality` is refused as
  the unknown key it is there. The environment sets one entry as `HOLDFAST_QUALITY_<KEY>`:
  `HOLDFAST_QUALITY_NVENC=24`, `HOLDFAST_QUALITY_AV1_NVENC=30`. It is validated exactly as the
  file's value is.

## H.264 and AV1 on every vendor - the T27 encoders

```yaml
encoder: x264              # libx264 (alias `libx264`) -> H.264, in software
# encoder: h264_nvenc      # H.264 on NVIDIA; h264_qsv, h264_vaapi, h264_amf likewise
# encoder: av1_qsv         # AV1 on Intel; av1_vaapi (Intel or AMD), av1_amf likewise
```

Each writes its codec through the same vendor path as its HEVC sibling: the same render node,
the same device options, the same start-time probe at 8 and at 10 bits, the same `hw_fallback`
(an H.264 hardware encoder falls back to `x264`, an AV1 one to `svtav1`), and in the container
image `h264_amf` and `av1_amf` are refused exactly as `amf` is, naming `h264_vaapi` and
`av1_vaapi`. Every output is held to the same gates as `cpu`'s; nothing is special-cased.

- **The codec families.** A source already in a family ranked above the target is skipped as
  `better-codec-family` rather than re-encoded down: H.264 < HEVC < AV1, so an H.264 target
  leaves HEVC and AV1 sources alone and an HEVC target leaves AV1 sources alone. A source in the
  target's own codec is skipped as `already-at-target-codec`, as before. A codec holdfast does
  not write (MPEG-2, VC-1, MPEG-4 Part 2, VP9, FFV1 and the rest) has no rank and is decided as
  it always was.
- **8-bit only on two of them.** `h264_qsv` lists no 10-bit format, and `h264_vaapi` uploads
  only `nv12` (ASSUMED: no VAAPI driver is documented taking a 10-bit H.264 surface), while the
  derived `pixel_format` of every source is at least 10-bit. With `pixel_format: auto` they skip
  every file as `exotic-pixel-format`; set `pixel_format: yuv420p` for them. Whether
  `h264_nvenc` and `h264_amf` encode 10-bit is the device's to say: the start-time probe finds
  out, and a 10-bit plan on a device that failed it is skipped as `hardware-unavailable`.
- **`encoder: auto` is unchanged**: it chooses among the HEVC hardware encoders only, so a
  file's target codec never depends on the host.

## `encoder: auto` and `hw_fallback` - hardware where it works

`encoder: auto` (at the top level, on a library root, or in an encode profile) chooses per job
the first hardware HEVC encoder - `nvenc`, `qsv`, `vaapi`, `amf`, in that order - that passed
this host's start-time probe for the job's pixel format; every choice writes HEVC.
`hw_fallback` decides what a job does where its hardware encoder (the one named, or every one
`auto` may choose) is missing or fails: `skip` (the default) encodes it with nothing else and
leaves the source as it is, and `software` encodes it with the software encoder of the same
codec (`cpu` for the HEVC ones, `svtav1` for the AV1 ones, `x264` for the H.264 ones). It is a
library root knob with a top-level default;
an encode profile carries none, since the root a file lives under decides it. Under `skip`, a
named hardware encoder that does not work refuses the start, as it always did, and a job
skipped for want of hardware records `hardware-unavailable` and is re-decided on every pass.
The reasoning, and why `skip` is the default, is in
[`docs/design/hardware.md`](design/hardware.md#fallback).


## `hw_decode` - decoding on the encoder's hardware

```yaml
hw_decode: software        # the default: every source is decoded in software, as always
# hw_decode: hardware      # decode on the job's hardware encoder's vendor hardware
```

Under `hardware`, a job whose encoder is a hardware one decodes its source on that vendor's
hardware: CUDA (`-hwaccel cuda`) for `nvenc`, `av1_nvenc` and `h264_nvenc`; VAAPI on the
encoder's own render node for the VAAPI and QSV encoders; VAAPI on the VAAPI node for the AMF
ones. Every decoded frame comes back to system memory before anything reads it, so the colour
stamp, a `deinterlace`, a `max_height` scale, the VAAPI upload and every gate see exactly the
frames a software decode would, at the source's own depth and with its HDR10 metadata: it is a
throughput option and changes no decision. A job encoded in software (a software encoder, or a
job `hw_fallback: software` handed to one) decodes in software. A source the device cannot
decode is decoded in software by ffmpeg itself; a decode device that cannot be opened fails the
job, which `hw_fallback` then decides. It is a library root knob with a top-level default, like
`hw_fallback`; the start-time probe runs under the top level's value. The reasoning, and the
exact command lines, are in
[docs/design/hardware.md](design/hardware.md#decode).
## `subtitle_sidecars` - text subtitles copied beside the replacement

<a id="subtitle-sidecars"></a>

```yaml
subtitle_sidecars: off        # the default: nothing is written beside a replacement
# subtitle_sidecars: text     # also copy each carried SubRip, ASS and WebVTT stream to a sidecar
```

Under `text`, once a job's swap has committed, every subtitle stream the replacement carries (after
`subtitle_languages` and `keep_commentary`) that is SubRip, ASS or WebVTT is also copied, in its own
format, to `<name>.<lang>[.forced].<ext>` beside the replacement: `film.eng.srt`,
`film.fre.forced.ass`, `film.und.vtt`. The embedded streams stay in the replacement exactly as
before. Picture-based subtitles (PGS, VobSub, DVB) and MP4 timed text (`mov_text`) get no sidecar
and are recorded with the reason; an existing file at a sidecar's name is never overwritten; a
sidecar is published only after it parses back with the source stream's event count, and one that
does not is recorded and does not stop the swap. It is a library root knob with a top-level
default, like `hw_decode`. The rules, the names and the gate, with their reasoning, are in
[docs/design/subtitles.md](design/subtitles.md#sidecars).

## `crop` - cutting black bars away

<a id="crop"></a>

```yaml
crop: off        # the default: every replacement keeps the source's whole frame
# crop: auto     # find each source's black bars and cut them away where that is provably safe
```

Under `auto`, each source is sampled by ffmpeg's `cropdetect` at ten points spread over the file
(skipping its first and last 5%), and its bars are cut away before encoding only where the
samples agree on them within 9 px, at least three samples were valid, the area removed is black on
every frame of the source, and - for a Dolby Vision source - its RPU's own active area (level 5)
names the same rectangle, which the replacement then carries zeroed. Anything else - samples that
disagree (a mixed aspect ratio), too few valid ones, bars with text or a logo in them, a Dolby
Vision RPU whose active area varies, is zero or disagrees with the picture - encodes the whole
frame, exactly as `off` would, and the row records the reason
(`crop` on the job row, [docs/api-reference.md](api-reference.md)). A crop runs after a
deinterlace and before a `max_height` scale, which then scales the cropped picture. The
perceptual gate scores the output against the source put through the same crop, and a crop gate
holds the output to the declared size and the removed area to black. Detection and the blackness
check each read the source once more, so a root under `auto` pays for them on every file it
encodes. It is a library root knob with a top-level default, like `subtitle_sidecars`; an encode
profile does not carry it. The rules, the constants and their sources are in
[docs/design/crop.md](design/crop.md#crop).
## `dolby_vision_p7` - Dolby Vision profile 7, converted on request

<a id="dolby-vision-p7"></a>

```yaml
dolby_vision_p7: skip         # the default: a profile 7 source is skipped (dolby-vision-profile-7)
# dolby_vision_p7: convert    # convert it to profile 8.1 with dovi_tool, discarding the enhancement layer
```

On the `cpu` encoder, Dolby Vision profile 8.1 and HDR10+ sources are carried by default, behind
gates of their own; profile 7 is not, because carrying it means converting it first, and the
conversion discards the enhancement layer. Under `convert`, a profile 7 source is rewritten to
profile 8.1 (`dovi_tool -m 2 convert --discard`, into a working file beside the job's, removed on
every way out) and encoded from that stream at the source's exact frame rate; a source whose frame
rate is not constant is skipped. Whether the layer was FEL or MEL is logged. Every other encoder
skips a profile 7 source whatever this says. A row skipped under `skip` records the key, so
switching to `convert` offers it back. It is a library root knob with a top-level default, like
`subtitle_sidecars`. The rules and their reasoning are in
[docs/design/dynamic-hdr.md](design/dynamic-hdr.md#profile-7).

## `exclude_paths` and `include_paths` - which paths this tool may touch

<a id="path-filters"></a>

```yaml
exclude_paths:
  - "**/Extras"            # every file under any directory named Extras, at any depth
  - "movies/4k"            # that directory, relative to the root, and everything in it
  - "/mnt/media/import"    # the same thing spelled absolutely
include_paths: []          # empty: every file under the root stays eligible
```

These two keys decide which files are **offered to the pipeline at all**. They decide
nothing about how a file is encoded, and they are the only keys here that can stop
holdfast touching part of a library root without making that part a root of its own.

Both are optional and **both default to empty**, so a configuration that names neither
offers exactly the files it always did. An empty list means the same thing an absent key
means: nothing is excluded, and with `include_paths` empty every file under the root
stays eligible. That is the behaviour of every configuration written before these keys
existed, and the reason a typo'd `include_paths` is the sharper of the two mistakes - an
exclude pattern that matches nothing protects nothing, while an include pattern that
matches nothing stops the whole library being scanned.

**Exclude wins.** A file matched by both lists is excluded, always. The fail-safe
direction for a tool that deletes sources is to touch fewer files, so the two keys are
not symmetrical and a file is offered only when no exclude pattern reaches it.

Both may be written at the top level and inside a `library_roots` entry. An entry that
names one of them uses **its** value for that root instead of the top-level value, empty
list included; an entry that does not name it inherits the top level. A filter is not a
profile knob: editing one moves no root's profile digest and re-opens no terminal row,
because a digest records what DECIDED a file and a filter decides whether a file is
looked at at all.

### The pattern language

Patterns are [doublestar](https://github.com/bmatcuk/doublestar) globs, and a pattern is
matched against the **whole** of a path and **never a substring** of it. `movies/tv`
therefore does not reach `movies/tv-archive`, which is exactly what a substring filter
(Tdarr's "file paths containing the entered input") gets wrong.

- `*` matches any run of characters that are not a path separator
- `**` matches any number of directories, and it must be **its own path component**:
  `Extras/**` is what you want, while a mid-pattern doublestar like `Extras**` behaves
  as `Extras*` and silently matches far less than it looks like it does
- `?` matches one character that is not a path separator, `[class]` one character of a
  class, and `{a,b}` either alternative

A pattern that does not begin with `/` is weighed against the path that is
**relative to the library root** containing the file (`movies/4k/x.mkv` under
`/mnt/media`). A pattern that **does begin with** `/` is weighed against the path as
configured (`/mnt/media/movies/4k/x.mkv`). A pattern naming a directory
**covers everything** beneath it: the pattern is weighed against the file's own path and
against every directory path containing it within the root, so `**/Extras` reaches the
files inside an `Extras` directory and not merely the directory itself.

A pattern that is not valid in this language is a **startup refusal** naming the key, the
pattern and, when it came from a `library_roots` entry, that root - never a silent
non-match for a directory the operator believes is protected. A pattern that is valid but
anchored outside every root it applies to is **reported** as covering nothing, at startup
and by `holdfast validate`, and refuses nothing: a filter legitimately guards a directory
that does not exist yet.

### What a filter does NOT change

A filter decides which files are offered, and nothing about how a file is judged. The one
thing it changes about the startup walk is visible, so it is stated here: the walk
**does not list** a directory an `exclude_paths` pattern reaches - a pattern that matches
the directory, or a directory containing it within its root - and nothing beneath it is
enumerated, swept for orphaned temp files or watched, by the first scan or by any later
one. That is the file rule asked of a directory: a pattern weighed against a file's own
path and every directory containing it reaches every file beneath such a directory, so
not listing it keeps out exactly the files the filter keeps out. Startup reports each one
at `debug` and their count at `info`, and never as a directory that could not be read.

A library root is always listed, whatever a pattern says about it, and `include_paths`
never keeps a directory from being listed: a directory no include pattern names may still
hold a file one does. The walk still inspects an excluded directory, so a mount point there
keeps its classification record and refuses a run exactly as it would with no filter
configured. A mount point BENEATH one is no longer a checked path, because nothing under it
is offered, swept, watched or swapped.

An excluded file that already has a terminal row **keeps it**, and `holdfast export` still
carries its record. The ledger's retention pass may only remove a terminal row when this run
LISTED the directory the file should be in and the file was not there, so it
draws no conclusion from a directory this run did not list: a row beneath an excluded
directory is kept whether or not its file is still there, which is retained audit history
and the safe direction. Excluding a path is not a request to forget what was done to it.

The same storage reached again under a path no pattern reaches - a symbolic link or a bind
mount of an excluded directory, or a later library root exposing it - is not descended
either, and startup names the excluded path that reached it first: walking it would offer,
under a second name, the files the first name keeps out. A bind mount exposing only a
SUBDIRECTORY of an excluded directory is a path to different storage and is walked like any
other, so exclude it by its own path as well.

`holdfast validate` prints, per library root, the patterns in force, which layer supplied
each list and how many patterns it holds. The count is a count OF PATTERNS - `validate`
describes a configuration and walks no library, so it never counts matching files.

`holdfast plan` is where the files are counted: per library root it publishes the patterns in
force, the source-named library under the root before any filter, and the part of it a path
filter kept out (`excluded_by_path_filter`), beside what is covered and eligible after them.

## `rules` - per-resolution-band overrides inside one library root

<a id="resolution-rules"></a>

```yaml
library_roots:
  - path: /mnt/tv
    min_bitrate_kbps: 2500
    crf: 22
    rules:
      - when:
          max_source_height: 576   # SD
        min_bitrate_kbps: 800
        crf: 24
      - when:
          min_source_height: 2160  # UHD
        min_bitrate_kbps: 12000
        min_savings_percent: 20
```

A threshold is one number for a 480p DVD rip and a 2160p remux alike. `min_bitrate_kbps:
2500` either excludes most of a 1080p library or lets SD files through that will bloat, and
the same is true of the quality target and the savings floor. A rule says "for sources in
this band of heights, these knobs are different".

`rules` lives INSIDE a `library_roots` entry, beside its `path`. There is no top-level
`rules` and no `HOLDFAST_RULES`: a rule is a per-library band, so there is nothing at the
top level for one to inherit from, and writing it there refuses to start with the entry form
named.

### Ordered, first match wins, nothing merges

The FIRST rule whose `when` admits the file supplies every knob it names. Every knob it does
not name comes from the root's own resolved profile. No later rule contributes anything -
not even a knob the first one is silent about.

That is a deliberate trade. Merging would mean a file guarded by a floor from one rule and
encoded at a quality target from another, with no line of your file saying so, on a tool
that deletes the source of every file it accepts. The cost is that an early broad rule
SHADOWS a later specific one, so:

- a rule that names no knob at all - and no `priority` - is **refused at start**: it would
  match, win, and change nothing, making every rule after it unreachable for the files it
  took. A rule naming only a `priority` is accepted, because it does something: it orders the
  files it takes (and, like any rule, takes them from every rule after it); and
- `holdfast validate` prints each root's rules **in list order**, with the band and the
  knobs each overrides, so a shadow is visible without running the library.

### `when` - which files a rule applies to

`min_source_height` and `max_source_height` are heights in pixels of the SOURCE. Both bounds
are INCLUSIVE (`max_source_height: 720` covers a 720p file), an absent bound leaves that side
unbounded, and a rule with no `when` at all matches every file under its root.

They are spelled `*_source_height` on purpose. A `when` bound selects which files a rule
applies TO, and it is a property of the SOURCE. `max_height` is a different thing entirely -
an output ceiling, a property of what is WRITTEN - and a rule may carry one, but beside the
`when` rather than inside it. Both `min_height` and `max_height` are refused inside a `when`
by name, with that distinction stated and with the place `max_height` does belong named,
rather than accepted as synonyms.

### What a rule may override

Exactly five knobs: `min_bitrate_kbps`, `crf`, `min_savings_percent`, `max_height` and
`encoder`. The first four are the ones whose right value depends on how many pixels the source
has. Anything else inside a rule refuses to start, naming the offending key and listing what a
rule may carry; a value the top level would refuse - a `crf` of 99, a `min_savings_percent` of
140 - is refused in the top level's own words, with the root and the rule's index in front of
them.

Beside the knobs a rule may name a `priority`, which is not a knob: it decides only how early
the band's files are offered, and is in no digest and no decision input
([queue priority](#queue-priority)).

`encoder` lets one band of a library go to a different encoder from the rest - a hardware
encoder for the band where encode time matters more than the last few percent of size, a
software one for the others - without splitting the library into roots:

```yaml
library_roots:
  - path: /mnt/tv
    encoder: cpu
    rules:
      - when:
          max_source_height: 720
        encoder: nvenc      # this band on the GPU; everything else stays on cpu
```

It is judged exactly as an encode profile's `encoder` is: a key or an ffmpeg codec alias this
build ships (`hevc_nvenc` behaves exactly as `nvenc`), never empty. It is refused beside a root
that resolves `remux_only: true`, since that root re-encodes nothing and the rule's encoder
could never run. An encode profile that matches the same file and names its own `encoder` still
wins (layer 5 above). The job's terminal row records the encoder that actually ran, the
already-at-target skip and the output-codec check read that encoder's target codec, and the
startup check above tests it against the host.

One more startup refusal belongs to it. A `max_height` makes a band's output shorter than its
source, so that output can fall in ANOTHER band on the next scan - for example a rule for
sources of 1081 lines and up, capped at 1080, whose 1080-line replacements the root's own
profile then decides. When the two bands target different codecs (the rule writes av1 and the
root hevc), that replacement would not be at its new band's target and would be encoded a
second time after its original was already deleted. So a root where a `max_height` in force
for a band writes that band's output into a band with a different target codec refuses to
start, naming both bands, the ceiling and both codecs. The output height is the one the encode
really writes.

`max_height` is an OUTPUT height ceiling: a source taller than it is scaled to it in the
source's own aspect ratio before it is encoded, and one at or below it is left alone. It is
the one knob here that makes a replacement a different picture from its source, so it is off
unless you set it and a root that sets it earns a startup notice saying what that means. With
`undo_window_hours: 0` - the default, under which a swap is final - a file it would scale is
SKIPPED unless the root also sets `downscale_acknowledged: true`. The README states the whole
posture under [its own anchor](../README.md#downscaling-posture), which is the one place it is
written down; a height that is not a positive EVEN whole number of pixels is refused at start,
because every pixel format this build encodes to is 4:2:0 and has no representation for an odd
dimension.

A rule may not move the VMAF floors or anything else about the perceptual gate: `vmaf_enable`,
`min_vmaf`, `vmaf_min_pool`, `vmaf_min_chroma`, `vmaf_subsample` and `vmaf_model` inside a rule
each refuse to start by name. A rule that could weaken the perceptual gate would be a rule that
could weaken the thing standing between a band of files and the deletion of their sources, and
a band moved to a faster encoder is exactly the band where that would be tempting. Every output
under a root is held to that root's floors, whichever encoder produced it. The per-encoder
`quality` map stays top-level too: a rule's encoder takes its `quality.<key>` entry from there,
as a root's does.

### A source whose height cannot be read

If a root carries any rule with a `when`, and ffprobe cannot establish a file's height, that
file is decided under NO rule and under no profile: it records a terminal skip
`undetermined-source-height`, warned about with its path. Reading an unreadable height as 0
would drop the file into whichever band admits zero, and ignoring the rules would judge it by
a threshold its operator wrote a band to avoid.

The skip records the rule list as what it read, so removing every `when`-carrying rule from
that root offers the file to the pipeline again on the next scan. `holdfast requeue --guard
undetermined-source-height` is the other lever, for a source that has since been remuxed.

### What a row records

Every terminal row carries the SOURCE's pixel dimensions, and the OUTPUT's where a file was
produced and measured (`source_width`, `source_height`, `output_width`, `output_height` in
`/api/history` and `holdfast export`) - so the band a file was judged in is readable in the
ledger rather than inferred. A dimension nothing measured is an explicit `null`, never a 0.

A row decided by a rule records the EFFECTIVE threshold the guard compared against, not the
root's. Editing that rule offers the file back to the pipeline on the next scan; editing a
key the guard never read does not. See [docs/requeue.md](requeue.md).

A root's `profile_digest` covers its rules: two roots that differ only in their rules carry
different digests, and a root with no `rules` key - or with an empty one - digests exactly as
it did before this existed.

## `audio_languages`, `subtitle_languages`, `keep_commentary` and `remux_only` - which streams survive

<a id="stream-selection"></a>

```yaml
audio_languages: [eng, jpn]    # keep these audio languages; empty (the default) keeps every one
subtitle_languages: [eng]      # the same, for subtitles
keep_commentary: false         # drop the tracks the CONTAINER marks as commentary
remux_only: true               # stream-copy the video too: drop streams, re-encode nothing
```

These four keys decide which of a source's streams the replacement carries. The defaults
are the behaviour this tool had before they existed:
both language lists default to empty, which keeps every stream of that type;
keep_commentary defaults to true, so dropping a commentary track is a choice and never a
side effect of a language filter; and
remux_only defaults to false, so the video is re-encoded as it always was.
A configuration that names none of them therefore carries exactly the streams it always
did and builds exactly the encode command it always built. Each may be written at the top
level and inside a `library_roots` entry, where the entry's value **replaces** the
inherited one for that root. They ARE profile knobs: editing one moves that root's profile
digest, because a digest records what decided a file and these decide what is done to it.

A language list is a list of ISO-639-2 codes, compared **case-insensitively** against a
stream's own `language` tag, and a code is validated by SHAPE - three alphabetic
characters - rather than against a registry, so `english` and `en` are refused at startup
and a code this build has never heard of is not. An empty list means the same thing an
absent key means: every stream of that type is carried.

A stream with no language tag, an empty one, or the undefined code und
**is kept whatever a list says**. Containers spell "unknown" both ways, and an untagged
track is more often the main audio than not - so this is the fail-safe direction, and it
is the reason a list can only ever drop tracks whose language you can see.

**If applying the audio selection would leave the file with no audio at all**, the
selection is not applied to audio: every audio stream is carried forward instead, the rest
of the selection still applies, and the fact is recorded on that file's row
(`selection_not_applied` in `holdfast export`). `audio_languages: [eng]` over a
foreign-language film therefore keeps its Japanese audio rather than producing a silent
file, which is a loss nothing recovers once the source is gone. Only AUDIO has this guard:
a file with no subtitle track is watchable, and extending the guard would make
`subtitle_languages: [eng]` silently a no-op on every foreign-language film.

`keep_commentary: false` drops only the streams the **container itself marks** as
commentary. A title, a stream name or a filename is never read as such a mark: inferring
one would drop a main track on a mislabelled file, on a configuration its operator
believed was conservative.

**`remux_only: true` stream-copies the video as well**, so nothing is re-encoded and the
job reclaims exactly the bytes of the streams it dropped. It is held to every structural
gate an encode is held to - length parity, the intended-stream check and the size floor in
force for that root, which is **not** waived or lowered for the mode - and it
**skips the VMAF gate**, recording on the row that the gate did not run and why
(`vmaf_skipped`), with no VMAF figure of any kind. That skip is paid for and not free:
before it is taken, every video stream the output carries is established to be
**identical to the source** stream it came from, and an output that is not - or whose
identity cannot be established at all - is **rejected and the source is kept**.
`remux_only` beside an `encoder` in the same layer is refused at startup: they are two
instructions about one job.

Two things `remux_only` does NOT change, and both decide how much a root actually reclaims.
It does not change which files are **offered**: the guard that skips a file already in the
target codec runs before this mode is consulted, so a file already in that codec is never
handed to a remux, whatever it carries. `remux_only` reclaims on the files this tool would
have re-encoded anyway and reclaims them without re-encoding - on a library already in the
target codec it reclaims nothing at all, and that is not a failure you will see reported,
because those files skip exactly as they always did. It also does not mark a file as
already thinned: the swap gives the file a new size and mtime, a ledger row is keyed to
both, so the next scan sees a file it has no row for and offers it again, and that second
pass has nothing left to drop. On a root you remux, set `min_savings_percent` above zero:
a pass that reclaims nothing is then rejected on the size floor rather than swapped for
whatever the container's own overhead happens to differ by.

Whatever a job drops, the row records: each dropped stream by its source index, its type
and its language as the source tagged it (`dropped_streams` in `holdfast export`). The
dropped bytes are not recoverable from the replacement, so that row is the only record
there is - and a row written before this build existed reads as **not recorded** rather
than as "dropped nothing".

These four keys select and copy: nothing here re-encodes a track, downmixes one or changes
its loudness. That is what [the audio keys](#audio) do, each off by default, on the tracks
these four keys carry.

`holdfast validate` prints, per library root, the resolved value of each of these four keys
and which layer supplied it - the same way it prints every other knob, and for the same
reason: the resolved value is the only thing that says what a root will actually do.

## The audio keys - re-encode, downmix and loudness

<a id="audio"></a>

```yaml
audio_reencode: on           # re-encode every carried lossless track; off (the default) copies it
audio_codec: eac3            # aac | ac3 | eac3 | opus; required by a re-encode or a downmix
audio_51_kbps: 640           # the bitrate per layout (also audio_mono_kbps, audio_stereo_kbps,
                             # audio_71_kbps); 0 (the default) takes the codec's own default
keep_original_audio: false   # true keeps the original beside its re-encode
audio_downmix: stereo        # add a stereo downmix of a surround track; off is the default
audio_loudness: ebu_r128     # normalise every re-encoded and added track; off is the default
```

Every key is a library root knob with a top-level default, and every one is **off** until
configured: a root that sets none of them copies every carried audio track, builds the command
line it always built and records nothing new. Each key away from its default moves the root's
profile digest.

- **`audio_reencode: on`** re-encodes each carried track in a lossless codec - TrueHD, DTS-HD
  Master Audio (by ffprobe's `DTS-HD MA` profile, never a lossy DTS core), any PCM, FLAC - to
  `audio_codec`, in its own place, **replacing** it. Every other track is copied.
- **`audio_codec`** is `aac` (ffmpeg's native encoder), `ac3`, `eac3` or `opus` (libopus).
  libfdk_aac is never used. The layout is named on the command line: mono, stereo, 5.1 and 7.1
  for `aac` and `opus`; mono, stereo and 5.1 for `ac3` and `eac3`, which cannot carry 7.1 - a 7.1
  track is then **copied**, with `layout-not-carried` on the row, never folded to 5.1. A layout
  outside those four (7.1(wide), 6.1, a bare channel count) is copied the same way. The sample
  rate is the source's where the codec takes it, and 48 kHz otherwise.
- **`audio_mono_kbps`, `audio_stereo_kbps`, `audio_51_kbps`, `audio_71_kbps`** are the bitrate
  per layout in kb/s. 0 takes the codec's default: Opus 64/128/256/450 (stereo, 5.1 and 7.1 from
  the Xiph recommendation), AAC 64/128/384/512, AC-3 and E-AC-3 96/192/640. An AC-3 bitrate
  must be one AC-3 carries (32 to 640); a bitrate past what the codec writes at the track's
  channels and rate copies that track with `bitrate-beyond-codec`.
- **`keep_original_audio: true`** keeps the original in place and adds its re-encode after
  every carried track, never the default track.
- **`audio_downmix: stereo`** adds one stereo track per language, from the first carried
  surround track of it that is not commentary, unless a stereo track of that language is
  already carried. It is folded by ffmpeg's default matrix (centre and surrounds at -3 dB, LFE
  left out), written in `audio_codec`, and never the default track.
- **`audio_loudness: ebu_r128`** normalises every re-encoded and added track to EBU R 128
  (-23 LUFS, -1 dBTP) in two passes: one measuring the source track, one applying it linearly
  where loudnorm can. The row records which mode the encoder reported (`linear`, `dynamic`,
  or `not-recorded`).

Every transformed track is held to its own gates before the swap - its decoded length, channel
count and layout, sample rate and, where normalised, loudness - and every output audio stream
must decode in full; the whole file must still pass every other gate, strictly-smaller included.
The row's `audio_tracks` lists what was done to each track and why. The reasoning, the cited
figures and the tolerances are in [docs/design/audio.md](design/audio.md).

## `priority` - which files are offered first

<a id="queue-priority"></a>

```yaml
library_roots:
  - path: /mnt/films
    priority: 10              # this library before the others
    rules:
      - when:
          max_source_height: 576
        priority: 50          # its SD rips before anything else
  - /mnt/tv
encode_profiles:
  - name: bulk-tv
    match: "**/TV/**/*.mkv"
    bitrate_kbps: 3000
    priority: -5              # after the rest of the library
```

A whole number from -1000 to 1000, default 0. **Higher is offered first**; files of equal
priority follow `queue_order`, then the full path. It may be written on a `library_roots`
entry (beside `path`), on a rule inside one, and on an `encode_profiles` entry, and a file's
priority is its matching rule's, else its matching encode profile's, else its root's, else 0.
First match wins for it as for every knob: a matching rule or encode profile that names no
priority passes the question on to the next PLACE, never to a later rule or profile.

It decides the SEQUENCE files are offered in and nothing else: never which files are offered,
never a guard, a gate, the encode's command line or the terminal row. It is in no profile
digest and no decision input, so adding, changing or removing a priority re-opens no row
([docs/requeue.md](requeue.md)). Adding a RULE is another matter, even one naming only a
priority: it takes its band's files from the rules after it, which is a change to the rule list. With no priority written anywhere every queue is exactly what
it was, and `queue_order: path` still hands the first file out while the library is being
listed; with one, the queue is held until the listing is finished.

A `priority` at the top level (or `HOLDFAST_PRIORITY`) refuses to start, saying where it goes;
so does a value out of range, a fraction, a word, or the key with no value, naming the key and
where it was written. A rule's priority under a banded `when` needs the source height, read
once per file per scan before the first file is offered; a file whose height cannot be read is
offered last, with a warning, never dropped. `holdfast validate` prints a root's priority in
its block, a rule's on its rule line and an encode profile's beside the queue order. The
design is [docs/design/queue-order.md](design/queue-order.md#priority).

## `watch` and `watch_settle_sec` - how a new file under this root is found

Every other key on this page decides what holdfast does to a file. These two decide when it
hears about one.

Without them a new file is discovered by a full library traversal: once at startup, and
again every `scan_interval_sec` seconds. That is the whole reason `scan_interval_sec` ships
at `0` and nobody sets it aggressively - enumerating a library to notice that one episode
arrived is the wrong shape of work - so in practice a new file waits for a manual rescan or
a restart.

```yaml
library_roots:
  - path: /mnt/tv
    watch: true              # default: absent, which is off
    watch_settle_sec: 60     # default: 60
  - path: /mnt/film          # says nothing, so it is not watched
```

**Off unless a root asks.** A configuration written before this existed carries neither key
and behaves exactly as it did. There is no top-level `watch` and no `HOLDFAST_WATCH`: a
top-level one would be inherited by every root that stayed silent, and a library still
filling over a network mount must not be watched because a different root asked to be.
Written at the top level, in the file or in the environment, either key refuses to start and
says where it goes.

**It accelerates the scan and never replaces it.** Events are lossy by construction: the
platform's queue overflows and drops them, a restart misses everything that happened while
the process was down, and a file moved in by a rename the watch never saw is simply there.
So the startup scan and the `scan_interval_sec` scan run exactly as they do with no watch
configured, and they remain the source of truth. The watch only ever makes a file's
discovery earlier.

**A watched file goes through the same door as a scanned one.** There is no second pipeline:
an offered path enters the same entry point a scan's worker uses, so every skip guard, every
gate, the same store claim and the same swap discipline apply to it unchanged, and a watch
offer racing a scan over one path results in exactly one claim.

### `watch_settle_sec` - why a watched file waits

A scan meets a file that has been sitting there. A watch meets one the moment a download
client created it, and probing a half-written 40 GB remux reads a duration and a packet
count that are not the finished file's. Every gate downstream is weighed against those
numbers, up to and including the decision that the replacement is faithful enough to delete
the source for.

So a watched file is offered only once its **size has held still** for `watch_settle_sec`
seconds. The period is measured from the last time the size CHANGED, not from the event: a
file that has been growing steadily for an hour has never been stable for a minute. The
default is 60. `0` offers a file the moment an event names it, and startup warns about that
root in as many words. A `watch_settle_sec` written without `watch: true` beside it is
refused rather than quietly read by nothing.

A path that is removed, renamed away or becomes unreadable before it settles is dropped: no
probe, no offer, and no record at `error`. A download client writing to a temporary name and
renaming it into place is routine, and a log that shouted about it would train you to ignore
the log you need.

### When a root cannot be watched

The watch either exists for a root or it does not, and a root nobody is watching is never
reported as watched. Each of these is decided **once, at startup**, recorded at `warn`
naming the root, the dependency that failed and what was tried, and leaves that root served
by the interval scan alone:

- this build carries no filesystem-event backend for the platform;
- the platform refused a watcher;
- the startup filesystem check could not positively identify the root's storage as local.
  NFS and SMB provide no file-notification support at all, and storage holdfast cannot
  identify is not evidence that it does - see [docs/filesystem.md](filesystem.md);
- the startup walk traversed nothing under that root, so there is no directory to register.

A root that IS watched gets one record naming the **event mechanism actually obtained** -
`inotify` on Linux, `kqueue` on the BSDs and macOS, `ReadDirectoryChangesW` on Windows,
`FEN` on illumos - beside the number of watch descriptors it is holding and its settle
period. There is no polling fallback anywhere in this, by design: a watch that had quietly
degraded to polling would be reporting something it is not doing.

### The descriptor count, and the host's own limit

The watch is not recursive: a descriptor is one per **directory** under the root, and the
count grows with the tree. The host caps it (`fs.inotify.max_user_watches` on Linux, whose
default differs per distribution and per available memory), and holdfast carries a bound of
its own so the ceiling is one it chose rather than whichever the host happens to have.

Reaching either is an announced degradation, never a watch that quietly sees half a library:
the record says the root is **no longer fully watched**, how many of its directories are
held, and that the interval scan covers the rest.

The directories registered are exactly the ones the startup walk traversed successfully - a
subtree that walk declined is one the watch touches in no way at all - and a directory
created later is found by the scan rather than by the watch.

### What it does not change

`holdfast validate` prints what the inheritance produced for each root and is unchanged by
these keys: they are not encode settings, and neither of them moves the resolved profile
digest a terminal row records its configuration by. Turning the watch on re-opens nothing
and re-offers nothing - it is a discovery accelerator, and the ledger cannot tell the
difference between a file the watch found and the same file found by a scan.

## `health_sweep_interval_hours` and `health_sweep_workers` - the library health sweep

<a id="health-sweep"></a>

```yaml
health_sweep_interval_hours: 0   # the default: no sweep. 168 = a sweep a week
health_sweep_workers: 1          # decodes at once (1-16; 0 means the default of 1)
```

Two daemon-wide keys, read by `holdfast serve` only. With `health_sweep_interval_hours` above 0,
the daemon fully decodes every source the enumeration offers - the set a scan would offer, path
filters included - every that many hours, measured from the end of the previous sweep, and
records each file as `ok`, `corrupt` or `unreadable` with the reason. It is **report only**: it
never moves, renames, deletes, repairs or touches a file, and nothing it records changes what the
encode pipeline does. The results are served at `GET /api/health`
([docs/api-reference.md](api-reference.md#health)), counted by the `holdfast_health_sweep_*`
metrics, and summarised in one notification per sweep that found a problem.

The sweep starts no decode while the daemon is paused or the run window, `max_load` or the
Tautulli pause says no (a decode already running finishes), runs its decodes at the lowest CPU
priority, and resumes after a restart without decoding again a file it already checked unless
the file changed. A decode still adds to the load average and reads every byte of a file, so on a
host near its `max_load` keep `health_sweep_workers` at 1. Accepted values:
`health_sweep_interval_hours` 0 to 87660 (ten years), `health_sweep_workers` 0 to 16; anything
else refuses to start, naming the key. Neither is a library root knob or an encode setting, so
neither moves a profile digest. The reasoning, and what each result means, is in
[docs/design/health-sweep.md](design/health-sweep.md#health-sweep).

## The media-server keys - Radarr, Sonarr and Plex

<a id="media-clients"></a>

```yaml
radarr_url: ""        # with radarr_api_key: RescanMovie for the owning movie after a swap
sonarr_url: ""        # with sonarr_api_key: RescanSeries for the owning series after a swap
plex_url: ""          # with plex_token: a partial scan after a swap, and the play hold
```

Nine daemon-wide keys, all off by default: `radarr_url`, `radarr_api_key`, `radarr_path_map`,
`sonarr_url`, `sonarr_api_key`, `sonarr_path_map`, `plex_url`, `plex_token` and `plex_path_map`.
A target is on when its address and its credential reference are both set. None of them is a
profile knob: a library root cannot carry one, none moves a profile digest, and none re-opens a
terminal row. They are read by `run` and `serve` alike. What each sends, the path maps, the play
hold and the warning to read before enabling an arr target are in
[docs/post-swap-hook.md](post-swap-hook.md).

`webhook_token` is a tenth daemon-wide key on the same terms, used by `serve` only: a credential
reference that turns on the Sonarr and Radarr webhook intake, which reads `sonarr_path_map` and
`radarr_path_map` in reverse ([docs/docker.md](docker.md#telling-holdfast-about-one-file-sonarr--radarr)).

## The worker-node keys

<a id="worker-nodes"></a>

```yaml
node_token: ""              # a secret reference; unset, worker nodes are off
node_lease_ttl_sec: 60      # ASSUMED; a node heartbeats every quarter of it
node_max_leases: 4          # ASSUMED; live leases across every node
node_max_leases_per_node: 1 # ASSUMED; live leases one node may hold
node_max_transfers: 2       # ASSUMED; source streams and uploads in flight across every node
node_gate_slots: 1          # ASSUMED; node outputs the server gates at once
```

Six daemon-wide keys read by `serve`. With `node_token` unset - the default - no lease is granted,
the endpoints under `/api/node/v1` answer 403, and the other five are not read. `node_token` is a
credential reference and must be a secret of its own: the same reference as `server_auth_token`,
`server_read_token` or `webhook_token` refuses to start. `node_lease_ttl_sec` accepts 4 to 86400
and the four counts 1 to 256; 0 means the default, and anything else refuses naming the key. The
five defaults are **ASSUMED**: nobody has measured a node deployment.

```yaml
server_tls_cert: ""   # a plain path to the PEM certificate chain `serve` presents
server_tls_key: ""    # a secret reference to its PEM private key; both or neither
```

Two daemon-wide keys read by `serve`. With both set the listener on `server_addr` speaks TLS
(1.2 at least) for the whole surface; with neither it listens exactly as before. One without the
other refuses to start naming the missing key, a literal `server_tls_key` refuses as every
literal credential does, and a pair that does not parse or does not match refuses without
printing the key. `server_tls_cert` is an absolute path. A probe of a TLS listener must speak
`https://` and trust the certificate. There is no client certificate and no user login.

```yaml
worker_server: ""     # the server a `holdfast worker` leases from
worker_name: ""       # this node's name
worker_slots: 1       # encodes this worker runs at once
worker_work_dir: ""   # where the worker writes an encode (and, in http mode, the source)
worker_mode: mapped   # mapped: read the source through a mount; http: download it on the lease
worker_path_map: []   # mapped mode: the server's view of the library to this worker's mounts
worker_insecure_http: false  # true accepts plain http:// to a server that is not loopback
worker_tls_ca: ""     # a PEM bundle trusted beside the system roots
```

Eight keys a `holdfast worker` reads; `run` and `serve` read none of them. `worker_server` is an
absolute http or https URL with no userinfo, query or fragment; `worker_name` is 1 to 64 characters
from letters, digits, `.`, `_` and `-`; `worker_slots` accepts 1 to 256; `worker_work_dir` is an
absolute path. `worker_path_map` has the form and the rules of the media-server path maps, with no
environment form, and one difference in how it is used: a source path no entry covers is refused
rather than passed through.

`worker_mode` is `mapped` (the default: a worker configured before the key existed behaves as it
did) or `http`. In http mode the worker has no mount: its file names **no** `library_roots` and
no `worker_path_map`, and one that names either is refused by name; it downloads each leased
source into `worker_work_dir`, which must have room for the source and its output for every
slot at once.
`holdfast validate` accepts such a file; `run` and `serve` refuse it. `worker_insecure_http`
defaults to `false`: plain `http://` to a server that is not loopback refuses to start. Written
`true` it starts, and says at warn level at every start that the node credential - and in http
mode the media - cross the network in cleartext; with `https://` or a loopback server it does
nothing. `worker_tls_ca` is an absolute path to a PEM bundle the worker trusts in addition to
the system roots; an unreadable or empty bundle refuses to start. No key switches certificate
verification off.

None of the sixteen is a profile knob: a library root cannot carry one and none moves a profile
digest. Every one has a `HOLDFAST_*` form except `worker_path_map`. The rules they serve are in
[docs/design/nodes.md](design/nodes.md#leases): the lease, [the source stream](design/nodes.md#http-mode)
and [the transport](design/nodes.md#transport), with the warnings to read before a worker speaks
plain HTTP.

## Where the working file lives

`scratch_dir` is a separate question - it moves where the encode WORKS, not what it
produces. See [docs/scratch.md](scratch.md).
