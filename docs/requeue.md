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
| `done` | `target_codec`, `encoder`, `crf`, `preset` - what the encode was taken under |
| a guard that read no configuration (interlaced, Dolby Vision, a symlinked source) | nothing, recorded **as** nothing read: a verdict no key can move |

It is **never a digest or a copy of the whole configuration.** That would tie every row to
every key, so correcting a notification URL or adding a library root would offer an entire
library back to the encoder - which is not a re-derivation, it is a re-scan of everything,
and you would learn to distrust it.

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
reports the same two counts - reading the ledger **read-only**, because a validate that
migrated your store as a side effect of describing it would leave that file unopenable by
the daemon still running against it. With no ledger yet, `validate` says so and still passes.

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

Each prints what it re-opened and the count. It re-opens by the same mechanism a
configuration change does - clearing what the row recorded - rather than by deleting the
row, which would take a `done` row's contribution to the lifetime reclaimed total with it
and a failed row's attempt accounting with it. `--failed` additionally resets that count,
because the count is the only thing holding a parked row.

It **refuses**, non-zero and having changed nothing, when:

- a path, a guard token or the `--failed` set matches no row - naming what it looked for. A
  command that reported success against an empty set would leave you believing a file is
  back in the pipeline when nothing about it has changed;
- a `--guard` token is not one this build recognises - naming the ones it does, because a
  typo and an empty match set are different problems;
- there is no selector at all. Re-opening the whole ledger is not something anybody types
  by accident;
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
