# Re-opening a decision: recorded inputs and `holdfast requeue`

A terminal `jobs` row holds that file out of the encoder for as long as it exists. It is a
**decision**, not only a record - and the decision was computed from ordinary editable YAML
keys, so it is only an answer for the configuration it was computed under. This page is how
that is kept true: what each row records, when a scan re-opens one, and the command for the
rows a configuration change cannot reason about.

## What a row records

Beside its proof (see [api-reference.md](api-reference.md)), every terminal row records the
configuration values **the decision that wrote it actually read**, and no others.

| Row | What it records |
|---|---|
| `skipped / low-bitrate` | `min_bitrate_kbps` - the threshold the source was compared against |
| `skipped / already-at-target-codec` | `target_codec` - what `encoder` resolves to (`cpu` -> `hevc`, `svtav1` -> `av1`) |
| `skipped / exotic-pixel-format` | `pixel_format` |
| `skipped / target-already-exists` | `container_ext` |
| `done` | `target_codec`, `encoder`, `crf`, `preset` - the settings the encode was taken under, resolved for the **replacement's** path |
| `skipped / undetermined-source-height` | `rules` - the band list its root carries. That guard read no threshold at all: what it read was that the root selects thresholds by a height nobody could establish |
| a guard that read no configuration (interlaced, Dolby Vision, a symlinked source) | nothing, recorded **as** nothing read: a verdict no key can move |

Where a library root carries [resolution rules](profiles.md#resolution-rules), the value a
row records is the **effective** one the guard compared against - the floor the file's own
band supplied, never the root's. So editing that rule offers the file back, and editing a
band the file does not fall in leaves it exactly where it is.

It is **never a digest or a copy of the whole configuration.** That would tie every row to
every key, so correcting a notification URL or adding a library root would offer an entire
library back to the encoder - which is not a re-derivation, it is a re-scan of everything,
and you would learn to distrust it.

**The rule, in one sentence: a terminal row is stale when, and only when, a key that row
recorded resolves to a different value under the configuration now in force FOR THAT ROW'S
OWN PATH.** Each value is resolved through the whole layering that decided that path -
the built-in default, then the top level, then the profile of the library root the file
lives under, then the [encode profile](profiles.md) whose match selects it - and a later
scan resolves the same keys the same way for the same path before it compares. So an
encode profile is a first-class decision input:

- the row says which one ran. A terminal row carries the encode profile's **name** beside
  its inputs (`profile` in `holdfast export`), so a reader holding the row and the
  configuration can resolve exactly which settings decided the file;
- and editing that profile's `encoder`, `crf`, `preset`, `pixel_format` or `container_ext`
  offers back exactly the files whose recorded values it moved - the ones the profile
  selects, and only where the row's own guard read the key you changed. Editing a
  profile's `crf` does not disturb a file skipped `already-at-target-codec`: that guard
  read the target codec and nothing else.

`bitrate_kbps` is the sixth key an encode profile can set, and no row records it, so editing
it offers no file back. It is the rate the encoder is *told to aim at*, never a value a guard
compares a source against; the threshold a guard does read is `min_bitrate_kbps`, which an
encode profile cannot set at all - a gate deciding whether a source may be destroyed stays on
the library root's profile, where the guard that reads it reads it.

A `done` row is keyed on the file the swap produced, so its values are the ones the
**replacement's** path resolves to. That matters in one corner: where an encode profile
moves `container_ext`, `film.mkv` becomes `film.mp4` and a profile matching `*.mkv` no
longer selects the row's path, so the row records the library root's settings beside that
profile's name - the name says which profile ran, the values say what the path in front of
you resolves to now. Recording the source's resolution instead would record a value later
scans could never resolve again, which is a row offered back for ever.

Two properties keep that safe, and neither is traded for the other. **Record and compare
are one resolution**, of one key, for one path, so a row written under a configuration
matches under that configuration - a recorded value the comparison could not reach again
would be a row offered back on every scan for the life of the row. And **only the keys a
decision actually read are recorded**, so an edit that moves none of them changes nothing
about that row, whatever else it changed. `holdfast requeue --guard <token>` is still the
lever for a verdict no key can re-derive.

A row that records **nothing at all** - every row written before holdfast recorded this -
reads as "cannot be re-derived" and is re-opened **once**, after which the decision it
reaches records what it read. Nothing is backfilled: claiming those rows were taken under
whatever is configured now would make them match, and they would stay excluded for ever.

## When a scan re-opens one

At claim time, a `done` or `skipped` row whose recorded values no longer match the
configuration in force - or which records none - is offered to the pipeline exactly as a
file holdfast has never seen. A row whose values still match holds the file out exactly as
it always did.

**Re-opening is not re-encoding.** The guards run again, so a file that reaches the same
verdict reaches it in microseconds, with nothing encoded, nothing written beside the source
and the source itself untouched - and the new verdict records the values in force, so the
next scan leaves it alone.

`run` and `serve` report, before the scan re-opens anything, how many rows were taken under
a configuration that has since moved and how many record no inputs at all. `validate`
reports the same two counts - as numbers, including when both are zero, because "nothing has
moved" is the answer worth being able to trust - and reads the ledger **read-only**, because
a validate that migrated your store as a side effect of describing it would leave that file
unopenable by the daemon still running against it. With no ledger yet, `validate` says so and
still passes.

Both counts are taken **per row, against that row's own path**, which is the same reading
the scan will apply to it - a figure measured against one configuration for the whole
ledger would describe a rule the scan does not use. A row whose path lies under **no
configured library root** is reported separately and named: the scan walks the configured
roots, so it never reaches that file and never re-opens it, whatever the row records.
Neither condition can fail a run or a `validate`; a ledger that could not be read at all
is reported beside a configuration that is still valid.

A ledger an **earlier holdfast** wrote is reported too, and that is the upgrade you most
want the figures for: no row in it records anything, so the first scan under the new build
re-opens every terminal row it holds. Reading it needs no migration - a schema with no column
for the inputs is one in which no row can have recorded any - so `validate` counts those rows
and still leaves the file exactly as the running daemon left it.

The ledger a build **just before this one** wrote reads differently, and its figure is worth
expecting. Those rows do record inputs, but they record what the library root resolved,
because that build resolved each key for the root and stopped there. So for a file an encode profile
overrides, the recorded value and the value now in force for that path differ, the row
counts as **moved**, and the first scan after the upgrade offers it back **once**: the guards
run again, the decision they reach records the profile-resolved values, and the scan after
that leaves it alone. That is the defect this rule exists to fix, showing up as a number.

## `holdfast requeue`

Re-opening answers "the value this verdict was computed from has moved" and nothing else. A
row parked at `max_failures` records an **error text**, not a decision any key determines,
so no configuration change can tell whether a new value would alter it; and a verdict you
simply want taken again - after replacing a source, after upgrading ffmpeg - is not a
configuration question at all. This is the lever for both:

```bash
holdfast requeue --config config.yaml /media/tv/ep.mkv    # one file
holdfast requeue --config config.yaml --guard low-bitrate # every row that guard skipped
holdfast requeue --config config.yaml --failed            # every row parked at max_failures
```

Each prints what it re-opened and the count, and takes **one selector per run**: a path,
`--guard` and `--failed` name three different sets, so two of them together is a refusal
rather than a guess at which one you meant.

It re-opens by the same mechanism a configuration change does - clearing what the row
recorded - rather than by deleting the row, which would take a `done` row's contribution to
the lifetime reclaimed total with it and a failed row's attempt accounting with it.

A row parked at `max_failures` is the exception, because what holds it is its **attempt
count** and not a verdict: re-opening one resets that count, whether `--failed` or the
file's own path named it. So a parked file you requeue by name really does come back, and
whatever failed that encode will be attempted again - which is the one case where
re-opening does lead to an encode.

It **refuses**, non-zero and having changed nothing, when:

- a path, a guard token or the `--failed` set matches no row - naming what it looked for. A
  command that reported success against an empty set would leave you believing a file is
  back in the pipeline when nothing about it has changed;
- a `--guard` token is not one this build recognises - naming the ones it does, because a
  typo and an empty match set are different problems;
- there is no selector at all. Re-opening the whole ledger is not something anybody types
  by accident;
- there is more than one selector. Acting on one of them would hand you a different set from
  the one you asked for and report it as a success;
- the state directory holds no ledger. It reports that and creates neither.

It is a **local** command and never an HTTP endpoint, for the reason `restore` is not one:
it changes what the engine will do to a media file, and a mutating endpoint opens an
authorization question the read-and-control API does not answer.

## The three rows nothing re-opens

Not by a configuration change, not by `requeue`. `requeue` names each one it leaves alone
and why:

- **a job parked `indeterminate`** - whether the swap was applied is exactly what is
  unknown, so re-encoding that path would overwrite contents nobody can describe.
  `holdfast resolve` is the way out;
- **a swap recorded `applied-despite-error`** - the rename took effect, so the file at that
  path **is** the replacement and the row describes an attempt that is over;
- **a `skipped / restored-original` row** - an operator deliberately put that original back
  through the undo window, and re-opening it would feed their rescued bytes to the very
  gates that passed the encode they rejected.

Changing the file itself is the only way one of those re-enters the pipeline, because that
is a new fingerprint and therefore a new row.
