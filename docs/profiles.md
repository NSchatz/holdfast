# Per-job encode settings - `encode_profiles` and `bitrate_kbps`

The top-level `encoder`, `crf`, `preset`, `pixel_format`, `container_ext` and
`bitrate_kbps` are what every file in every library root is transcoded under. This page
is how you say something different for part of a library, and how you ask for a target
bitrate instead of a quality target.

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
over the top-level ones. A later matching profile has no effect on that job; a setting
the matching profile does not override keeps its top-level value; and a source no
profile matches is transcoded under the top-level settings - never skipped, never
failed. With no `encode_profiles` at all, every job resolves to exactly the top-level
settings.

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

## Where the working file lives

`scratch_dir` is a separate question - it moves where the encode WORKS, not what it
produces. See [docs/scratch.md](scratch.md).
