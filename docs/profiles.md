# Saying something different for part of a library

The top-level `encoder`, `crf`, `preset`, `pixel_format`, `container_ext` and
`bitrate_kbps` are what every file in every library root is transcoded under. This page
is how you say something different for part of a library: different encode settings
(`encode_profiles`), a target bitrate instead of a quality target (`bitrate_kbps`),
different thresholds for a band of source resolutions (`rules`), or nothing at all for part
of a root (`exclude_paths` and `include_paths`). One key here is not about what is done to a
file but about how it is FOUND: `watch` turns on a filesystem watch for that root.

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

- a rule that names no knob at all is **refused at start**: it would match, win, and change
  nothing, making every rule after it unreachable for the files it took; and
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

Exactly four knobs: `min_bitrate_kbps`, `crf`, `min_savings_percent`, `max_height`. These are
the ones whose right value depends on how many pixels the source has. Anything else inside a
rule refuses to start, naming the offending key and listing what a rule may carry; a value the
top level would refuse - a `crf` of 99, a `min_savings_percent` of 140 - is refused in the
top level's own words, with the root and the rule's index in front of them.

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

A rule may not move the VMAF floors, the encoder, or anything else. A rule that could weaken
the perceptual gate would be a rule that could weaken the thing standing between a band of
files and the deletion of their sources.

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

Audio **transcoding is a non-goal**: this is selection and copy, and nothing here
re-encodes a track, downmixes one, or adds an AAC stereo companion. Transcoding audio
reopens the fidelity question for a second medium, and it would need its own gate argument
before this tool did it to somebody's only copy of a film.

`holdfast validate` prints, per library root, the resolved value of each of these four keys
and which layer supplied it - the same way it prints every other knob, and for the same
reason: the resolved value is the only thing that says what a root will actually do.

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

## Where the working file lives

`scratch_dir` is a separate question - it moves where the encode WORKS, not what it
produces. See [docs/scratch.md](scratch.md).
