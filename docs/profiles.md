# Saying something different for part of a library

The top-level `encoder`, `crf`, `preset`, `pixel_format`, `container_ext` and
`bitrate_kbps` are what every file in every library root is transcoded under. This page
is how you say something different for part of a library: different encode settings
(`encode_profiles`), a target bitrate instead of a quality target (`bitrate_kbps`), or
nothing at all for part of a root (`exclude_paths` and `include_paths`).

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

### What it is laid over: four layers, innermost last

1. the built-in default
2. the top-level configuration (the YAML file, then `HOLDFAST_*`)
3. the profile of the **library root** the file lives under (a `library_roots` entry
   spelled as a mapping - see `config.example.yaml`)
4. the **encode profile** whose pattern selected this file

The last layer to mention a key decides it. So an encode profile is laid over the ROOT's
resolved values and not over the top level: a root at `crf: 20` whose files match a
profile that sets only `encoder` encodes at that root's 20, not at the top level's.
`holdfast validate` prints layers 1 to 3 per root, knob by knob, with the layer each
value came from.

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
`container_ext` and `bitrate_kbps`. **Any other key is a startup refusal naming the
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

Every distinct encoder the configuration can reach - the top-level one, and each
profile's override - is tested against this host at startup, before anything is
encoded and before the job store is opened. A hardware encoder with no matching
device, or an ffmpeg build without the codec, **refuses the run** and the account
names the profile that asked for it. That is the same loud failure the top-level
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
encoder at all** - no `-crf`, `-cq`, `-global_quality`, `-qp` and no `-rc cqp`. A rate
control and a quality target are two different instructions, and passing both would
leave which one wins to the encoder rather than to you.

It is announced at startup as a **note**, not a warning: every no-loss gate still runs,
and an encode that misses one is still rejected with the source untouched. `0` (the
default, and what an absent key means) keeps the quality target, so a config written
before this setting existed produces byte-identical encoder arguments.

The value must be a whole number of kbps. `8000.5`, a quoted string with a unit, a
boolean, a list, a negative, or the key with no value at all is a startup refusal naming
the key and the value, never a silently truncated bitrate.

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

An excluded directory is still **listed**. The scan lists exactly the directories it
would have listed with no filter configured and records exactly the same evidence about
where it looked - because the ledger's retention pass may only remove a terminal row when
this run LISTED the directory the file should be in and the file was not there. A filter
that skipped the directory instead of its files would make every row beneath it read as a
file that had been deleted, which would discard audit history irreversibly and hand the
excluded subtree back to the encoder on the next scan.

So an excluded file that already has a terminal row keeps it, and `holdfast export` still
carries its record. Excluding a path is not a request to forget what was done to it.

Filtering is not a way to make a scan faster: the walk is unchanged, and only the set of
files offered to the pipeline is narrower.

`holdfast validate` prints, per library root, the patterns in force, which layer supplied
each list and how many patterns it holds. The count is a count OF PATTERNS - `validate`
describes a configuration and walks no library, so it never counts matching files.

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

Audio **transcoding is a non-goal**: this is selection and copy, and nothing here
re-encodes a track, downmixes one, or adds an AAC stereo companion. Transcoding audio
reopens the fidelity question for a second medium, and it would need its own gate argument
before this tool did it to somebody's only copy of a film.

`holdfast validate` prints, per library root, the resolved value of each of these four keys
and which layer supplied it - the same way it prints every other knob, and for the same
reason: the resolved value is the only thing that says what a root will actually do.

## Where the working file lives

`scratch_dir` is a separate question - it moves where the encode WORKS, not what it
produces. See [docs/scratch.md](scratch.md).
