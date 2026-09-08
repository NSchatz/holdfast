# How the dashboard's figures are computed

This is where the dashboard's explanation lives. The page itself carries a short scope
label per block and one link per region to the section below that covers it; the
paragraphs that explain HOW a figure is derived, and what it does and does not license,
are here (frontend clause F8: "Explanation lives in docs ... The claims are not dropped;
they move").

Nothing here is optional reading for someone deciding whether to trust a figure. If a
claim was ever printed beside a number on that page, it is in this document, and
`TestDocs_EveryClaimMovedOffTheSurfaceIsInTheLinkedDocument` fails the build if one
is not.

## What this page is

holdfast, a config-as-code, data-safe media transcoder. The YAML config is the source of
truth; this page reads and controls. It reports what was measured, and only that.

Each of those three sentences is load-bearing and none of them is on the surface:

- **config-as-code, data-safe.** The library is declared in a YAML file that lives in
  git, and a source is never destroyed until its replacement has passed every gate.
- **reads and controls.** The page can start a scan and pause or resume the feeding of
  new files, and it can do nothing else. It cannot edit the configuration, cannot change
  a job's outcome and cannot touch a media file; the mutating endpoints are token-gated
  and are disabled outright when the server has no control token set.
- **only what was measured.** Every figure on the page came off the wire from the server,
  which computed it from the ledger. The page derives presentation - a percentage, a
  duration in words, an elapsed age - and never a measurement. A value nobody recorded
  reads as "not recorded" rather than as a zero, which is the same rule stated below.

Two more facts govern the whole page and are repeated nowhere on it:

- **A fact nobody recorded reads as "not recorded".** That is the page's ONE absence
  phrase, in every field that can carry one: a VMAF score, an encode duration, a
  progress figure, a file size, a worker name, a timestamp, an aggregate input. It is
  never rendered as `0`, never as a bare dash, and never as an estimate wearing a
  measurement's clothes. The store's numeric outcome columns are nullable precisely so
  that distinction survives to the screen, and a VMAF of `0.0` is a destroyed frame, not
  a missing measurement.
- **A figure that could not be computed reads as "unavailable".** That is a different
  fact from an absent measurement and gets a different word. The card stays on the page
  saying so, because a card that vanished would leave the page looking complete while a
  number was missing from it, and it draws no mark at all: there is nothing measured to
  draw.

## Right now

Everything in this region describes THIS run of `holdfast serve`. It is pushed by the
server over a Server-Sent Events stream as it changes, never polled by the page, and it
covers whether holdfast is running or paused, whether a scan is under way, how many files
stand in each state, and the work in flight.

What the two blocks of the region cover, in full:

- **The region.** The live state of this holdfast, pushed by the server as it changes.
  Nothing here is derived from the ledger, so nothing here survives a restart; the other
  region is where the durable figures are.
- **The queue table.** Files in flight, with in-state age and the encoder's own progress.
  The two are different measurements and neither substitutes for the other: elapsed is
  time in the current state, progress is the encoder's own position in the source.

### The badges and the counts

`running` / `paused` is the state of the pause flag the API controls; it DELAYS the
feeding of new files and never interrupts an in-flight encode or swap. `idle` /
`scanning` is whether a scan pass is walking the library.

The seven count chips are the server's own `GROUP BY status` over the jobs table. A
status the server does not name has no row in that state, so a `0` chip is a measured
zero and is rendered as one. A summary that is not readable at all is a different fact,
and the view says it could not be read rather than drawing seven zeroes nobody counted.

`reclaimed this run` is the in-process counter for this daemon's lifetime. The lifetime
figure in the other region is the durable one.

### Elapsed, and why it is recomputed rather than counted

Elapsed is how long the file has been in the state it is in now, recomputed from the
transition timestamp on each update rather than counted in this page. Two reasons, both
load-bearing: a background tab's timers are throttled by policies with no normative
guarantee, so a counter ticking in the page would silently drift while a value recomputed
from the transition timestamp cannot; and a client whose clock is skewed against the
server's would otherwise read a nonsensical (or negative) age straight off the timestamp.
The page therefore takes the offset between its own clock and the server's from each
snapshot's own `now` field, and derives every elapsed figure from it.

### Progress, and what it is measured against

Progress is read from the encoder's own progress stream and measured against the source
duration, so it is shown while a file is *encoding* and not in any other state: a file
being probed or verified has no encoder running and is covered by Elapsed alone. Where
the encoder has reported nothing yet, or the container reports no duration, the cell
reads "not recorded" - never an estimate, and never a stale figure carried over from an
earlier reading. There is no interpolation from elapsed time, including at the instant
the encode ends and the verify begins.

### The row caps

`/api/queue` and `/api/history` each ship at most a fixed number of rows, so a truncated
view could read as the whole ledger. When the summary's own totals exceed what the page
was handed, the page says so above the table. The count chips, not the tables, are the
authoritative totals.

### The controls, and what a refusal means

`Rescan`, `Pause` and `Resume` POST to the API with the control token. A refusal is
rendered as visible text that says the action did not happen, and nothing else on the
page moves: no badge flips, no count changes, and every control stays operable. The
mutating endpoints are disabled entirely when the server has no `server_auth_token` set,
which the refusal names.

## What it has done to your library

Everything in this region is derived from the ledger rather than from the run above, so
it survives restarts and describes the library as a whole. `reclaimed lifetime` is summed
over every done row that recorded both sizes; a pre-outcome-columns row that recorded
neither contributes nothing rather than reading a NULL as `0`.

In full, what the region covers: Derived from the ledger, so it survives restarts and
covers the whole library. That is the difference between the two regions, and it is why a
figure here can disagree with a count above without either being wrong - the counts above
describe one process, these describe every file holdfast has ever finished.

### The whole-ledger figures

Every figure here is computed by the server over *every* matching row in the ledger, not
over the capped rows the tables ship, and each states the set it covers. A row that
recorded no value is excluded and counted, never read as a zero: a fact nobody measured is
reported as missing, not as 0.

In full: Computed by the server over every matching row, never over the capped rows below.
The tables below this block are capped by the API, so a figure derived from them would be
a statistic about the last few hundred files wearing the clothes of a statistic about the
library. That is why these figures are computed server-side and never in the browser.

The six figures are the outcome distribution, the skips broken down by which guard fired,
and four spreads: the replacement size as a share of the original, the encode time, the
pooled VMAF mean and the VMAF worst frame. Each spread reports its minimum, its mean and
its maximum, and the count of files it is across.

The server computes only `COUNT`, `MIN`, `AVG` and `MAX`. There is no median and no
percentile: SQLite's `median()` and `percentile()` are gated on 3.51.0 built with
`-DSQLITE_ENABLE_PERCENTILE`, and the pinned build has neither.

### Why the figures are drawn as well as stated

Each figure is also DRAWN, from the same server-computed numbers and nothing else: the
bars of a distribution are in proportion to the counts beside them, and a spread puts its
minimum, mean and maximum on one scale. The drawing carries no value the text beside it
does not carry, so a reader using a screen reader, a keyboard or no colour vision loses
nothing by ignoring it, and no figure is readable only by pointing at it.

Four rules govern every drawing, and each is graded in a real browser against a document
deliberately mutated to defeat it:

- **Built, never fetched.** A drawing is a shell cloned from a `<template>` in the page's
  own markup, with one geometry attribute set per mark. No library, no font, no image, no
  `data:` URI: the response policy is `default-src 'none'`.
- **The drawing is never the sole carrier of a number.** Every value a figure encodes is
  rendered as text in the same card, in the order the marks are drawn. Remove every
  drawing from the rendered document and the cards still read.
- **Nothing means anything by colour alone.** Every mark of every figure is one token
  (`--mark`), and the distinctions are position, length, tick height and the label beside
  the mark.
- **3:1 against what is behind it.** Every mark, scale, tick, status dot and figure
  boundary clears WCAG 2.2's non-text contrast floor, measured from the browser's own
  computed styles, in BOTH themes.

### Recent history: the proof each swap was safe

Each finished file, with the proof its swap was safe. A skipped file shows which guard
held it back; a failed one shows why. The proof is the row itself: the two sizes and the
percentage reclaimed, both pooled VMAF statistics with the model that produced them, the
encoder, how long the encode took, and when the row was last written.

The size cell is the source size, the output size and the percentage reclaimed. The
strictly-smaller gate precludes an output larger than its source, and the figure is
clamped at zero anyway so a future bug there can never render a nonsensical negative.

### What a VMAF score on this page licenses, and what it does not

VMAF is a perceptual estimate under one viewing condition. Each score on this page was
measured against that file's OWN source, and both pooled statistics are shown: the
harmonic mean and the worst frame. The model that produced them is named with every
score.

- **The model is luma-only.** It is structurally blind to chroma damage, which only the
  structural gates (codec, duration and packet parity, stream-count parity, full decode
  integrity, strictly-smaller) catch.
- **Scores are never compared across files.** VMAF is not comparable between different
  sources, and this page never puts two files' scores on one scale for that reason. The
  whole-ledger VMAF figures pool per-file scores; they are a summary of this library's
  encodes, not a ranking of them.
- **A pooled mean hides local damage.** That is documented by the model's own authors,
  and it is why the worst-frame floor exists and why the worst frame is shown beside the
  mean rather than behind it.
- **There is no "visually lossless" score.** VMAF is a regression onto a subjective
  opinion scale; 100 is a label-normalisation anchor, not "identical to the source".

### The skip guards

A skipped row names the guard that held the file back, from a closed vocabulary: already
at the target codec, already efficient (low bitrate), hardlinked (would break a seed),
interlaced, Dolby Vision or HDR10+ (dynamic metadata a generic re-encode cannot preserve),
incomplete HDR metadata, an exotic pixel format, a target file that already exists, a
symlinked source, and a failure to retain the original inside the undo window. An unknown
token falls back to itself, so a guard added later is never hidden behind a blank.

## The three states every view shows

Every view on this page - the live counts, the queue, the whole-ledger figures and recent
history - says which of three states it is in, in words, and the three are always
distinct:

| state | what it means |
|---|---|
| loading | the page is connected and no snapshot has arrived yet |
| empty | a snapshot arrived and this view has nothing to show |
| unreadable | the payload could not be read at all, or the stream failed before one arrived |

Each view says it in its own words, so a reader always knows WHICH view is in which
state without reading the heading above it again:

| view | loading | empty | unreadable |
|---|---|---|---|
| the live counts | Loading the live counts. | No file recorded yet. | The live counts are unreadable. |
| the queue | Loading the work in hand. | No work in hand. | The work in hand is unreadable. |
| the whole-ledger figures | Loading these figures. | No figure has a contributing row. | These figures are unreadable. |
| recent history | Loading finished swaps. | No swap finished yet. | Finished swaps are unreadable. |

Those wordings deliberately avoid the words in the heading above them and in the column
headers beneath them: a state row sits INSIDE its own table, so naming the table again
there spends a reader's attention on a word they have just read.

**"Empty" is a fact about that view, not about the ledger.** A ledger with no rows leaves
the queue and recent history with nothing to show while the whole-ledger figures still
have six figures to draw, and a published aggregate set that is empty leaves the figures
with nothing while the counts still have their measured zeroes. Each view answers for
itself.

A stream that drops with rows already on screen is a different case again: the rows STAY,
and the connection state in the header alone says they are no longer live. A view never
silently freezes, and a figure never keeps reading as current once the feed is gone.

## Themes, type and motion

The page follows the operating system's colour-scheme preference and ships both a light
and a dark palette, each hand-authored value by value. An engine that reports no
preference gets the LIGHT palette, which is the named default. Every colour and every
length comes from one committed token file, `internal/webui/src/tokens.css`, and every
token pair that must clear a contrast floor carries its measured ratio in both themes
beside the value.

Monospace is used where column alignment carries meaning - paths, sizes, scores,
durations and counts in a column. Headings, explanatory text and controls use the system
UI face.

Motion on this page is decoration and affordance only; nothing is communicated by
movement. `prefers-reduced-motion: reduce` removes every transition, and the page still
shows every value, state change and status it showed with the motion in place.

## Where this is graded

`make webui-check` loads the SERVED document in a real browser engine and reads what it
rendered: computed style after the whole cascade, real layout geometry, the accessibility
tree, the text `innerText` says a reader can see, and the browser's own report of every
policy refusal. Nothing about what this page SHOWS is decided by matching HTML or CSS
source text. See `docs/webui.md` for the layout of the sources and the two suites.
