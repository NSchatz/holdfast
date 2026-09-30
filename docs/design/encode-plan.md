# The encode plan

What one job's encode does to its source, and the argument behind the rule `CLAUDE.md`
states about it. This document is that argument's single home: `CLAUDE.md` names the rule
and links here rather than restating it.

## The rule

<a id="encode-plan"></a>

**Every job's encode is declared once, as one plan, before anything is encoded, and the
command line and every acceptance gate read that plan and derive nothing of their own.** The
plan is derived in one place (`deriveEncodePlan` in `internal/engine/encodeplan.go`), from the
profile that decides the file, the job's effective encode settings (resolved once per job,
before the guards read them, and handed to the derivation), the working path, the intended
stream map and the probe snapshot the guards already read. The encoder takes every
job-specific argument of its command line from it; the gates take from it the source and the
output they measure and every job-specific expectation they hold the output to; the terminal row records
the picture operations it declares. None of them resolves any part of that answer for itself.

## Why one plan

holdfast deletes a source on the strength of its gates. A gate is only worth that trust if
what it checks is what was encoded, and "what was encoded" is decided by the same handful of
inputs every time: the encoder and its quality value, the pixel format, the streams carried,
the deinterlace, the `max_height` scale, the colour description. Before the plan, the encoder
resolved all of those for itself, and the engine resolved the ones its gates check - the target
codec, the deinterlace, the scale - again, through the same helpers from the same inputs. Two
resolutions that agree today are two answers waiting to
disagree, and the disagreement lands where it costs most: a gate that checks an output
against something other than what was encoded, passes it, and licenses the deletion of the
source.

The intended stream map already followed this rule: it was derived once and handed to both
the command line and the stream-parity gate, and a test proves the two read one derivation
rather than two that happen to agree. The plan extends that rule from which streams are
carried to everything the encode does.

A plan is also where later transformations are meant to land. A hardware decode path, a
device chosen at run time, an audio re-encode, a subtitle sidecar, a crop and dynamic HDR
metadata would each add a declaration to the plan, to be read by the command line and by the
gate that proves it happened, from the same value. This build declares each such slot with the
one value it derives, and refuses a plan carrying any other (below).

## What the plan declares

| Part | What this build declares | Read by |
|---|---|---|
| `Video` | the registry encoder and the device it opens (the render node for `vaapi`, none for every other encoder); the decode path (software, `DecodeSoftware`); the pixel format (forced, or derived from the source's); the quality value (`crf`, or `bitrate_kbps`, and the preset); the codec the output must be in, or a copy of the source's video on a remux-only root | the command line; the codec gate; on a copy, the video-identity check |
| `Audio`, `Subtitles` | every carried stream is copied (`CopyStreams`) | the command line |
| `Streams` | the intended stream map: which source streams the output carries (the same value the row's dropped streams are recorded from) | the command line; the stream-parity gate |
| attached pictures | how the attached pictures the map carries travel: pinned to copy in the map, or copied out of the source and carried as Matroska attachments | the command line |
| `Picture` | the deinterlace applied, the `max_height` scale applied, and the crop (none in this build) | the filter chain; the reference and the scale the perceptual gate builds; the row's provenance |
| `Metadata` | the colour description (`hdr.Color`): the source's tags and, for HDR10, its mastering display and content light level; HDR10+ and Dolby Vision are not carried, because the guards skip a source that has either before a plan is derived; and the fidelity declaration (`hdr.Fidelity`, [below](#fidelity)): the bit depth and chroma subsampling of the pixel format, the colour tags, and the HDR10 static-metadata blocks the source carries | the command line (`-color_*` and the libx265 parameters); the output fidelity gate |
| `Profile`, `Settings` | the effective library profile, whose floors the size and perceptual gates apply, and the job's effective encode settings with the encode profile that supplied them | the size and perceptual gates (`Profile`); `Settings` is the record the plan's quality value (`Video.Quality`) was taken from, and nothing reads it after the derivation |
| container | the muxer the output is written in, named from the working file's name | the command line |
| `Source`, `Output` | the file the encode reads and the working file it writes: the two paths the plan was derived for | the command line (`-i` and the output); every gate, for the files it measures |

## Declared, then built or refused

The plan declares; the command-line builder (`EncodePlan.args`) performs what it declares
and refuses what it cannot. A plan declaring an operation this build has no way to perform -
an audio or subtitle action other than copy, a crop, a hardware decode path, a device an
encoder does not open, dynamic HDR metadata carried - is refused with an `UnbuildablePlanError` before any
subprocess runs. An operation silently left out would be an output that is not what its plan
says, checked by gates that believe the plan.

A plan is also refused when it claims a picture operation on a stream copy, which re-encodes
nothing, and when no derivation produced it.

Two refusals exist twice on purpose: a source pixel format with no faithful derivation, and a
deinterlace that would emit one frame per field. The guards refuse both before a plan is
derived, and the derivation refuses them again, so that neither can reach a command line when
a check in front of it is bypassed.

A plan that cannot be derived - an output container this build cannot name, an attached
picture Matroska cannot carry, an unknown encoder - fails the job with the gate and the reason
the encoder's own refusal always recorded, and the encoder is then never called.

## The explicit pixel format and the per-encoder quality

<a id="explicit-pixel-format"></a>

**The plan names the format the encoder is handed, from the encoder's own list, and a plan
the encoder cannot carry exactly is never derived.** `Video.PixelFormat` is what the output
is meant to be (the source's chroma subsampling, depth floored at 10, or a forced
`pixel_format`); `Video.InputFormat` is the format the encoder receives: that format where
the encoder lists it, else the planar or semi-planar spelling of the same chroma and depth
that it lists (`yuv420p10le` is `p010le` on NVENC, QSV and AMF), chosen once by
`encoder.Spec.InputFormat` from the registry's copy of the pinned binary's "Supported pixel
formats" line. The command line names it explicitly: `-pix_fmt` for every encoder but VAAPI,
whose encoder takes only hardware surfaces, so its format is the software layout uploaded
(`format=p010le,hwupload` with the Main 10 profile, or `nv12`) and it gets no `-pix_fmt`.

Before this, the command line said `-pix_fmt <plan format>` to every encoder. An encoder that
does not list it makes ffmpeg pick the least-lossy format it does list, with a warning that
`-loglevel error` hides; that is harmless where a listed format carries the same chroma and
depth, and a silent subsample or depth cut where none does (4:2:2 into libsvtav1 becomes
4:2:0). VAAPI uploaded 8-bit `nv12` whatever the plan said. Now the engine's pixel-format
guard skips such a file (`exotic-pixel-format`, recording `pixel_format` and `encoder`) before
a plan is derived, the derivation refuses it again as a backstop, and the builder refuses a
plan whose input format is not the one the encoder's list gives for its pixel format.

<a id="encoder-quality"></a>

**The plan's quality value is on the encoder's own scale.** `Video.Quality.Value` is the
job's `quality.<key>` for a hardware encoder that has one, and its `crf` otherwise;
`Video.Quality.CRF` stays the job's `crf`. Each scale - its option, its range and the key
that sets it - lives on the registry's `encoder.Spec.Quality`, and the derivation refuses a
value off it naming `quality.<key>` and the scale (an inherited `crf: 0` is NVENC's
"automatic" and VAAPI's "unset", neither a quality target). An absent key keeps the value
the command line always carried, so its argv does not move.

Both are recorded argv changes, not refactors: the golden command lines of the hardware
encoders moved to name their input format (and VAAPI to upload `p010le`), and every one is
held by `TestGoldenArgv_EveryArgvNamesItsPixelFormat` to name a format its encoder lists.
libx265 and libsvtav1 are handed every format they list unchanged.

## The output fidelity gate

<a id="fidelity"></a>

**An output replaces its source only when it carries what its plan declares: the bit depth and
chroma subsampling of the pixel format it was encoded to, every colour tag the plan writes,
and every HDR10 static-metadata block the source carries, with the same values.** The
declaration is part of the plan (`MetadataPlan.Fidelity`, derived once by `hdr.FidelityOf`
from the plan's pixel format, its colour description and the source's side data), and the gate
(`Engine.outputFidelity`, gate 5b of `verifyAgainst`, `GateFidelity`) holds the output to it
with `hdr.Fidelity.Check`, naming every field that differs. A mismatch is a deterministic
rejection, and the source is kept.

Why a gate of its own. None of these properties is visible to the perceptual gate: the VMAF
model extracts luma features only, and it scores pictures, not metadata
([quality-gate](quality-gate.md#vmaf-pooling)). An output encoded 8-bit where the plan said
10, subsampled to 4:2:0 from a 4:2:2 source, tagged bt709 over PQ samples, or stripped of its
mastering-display block can score like a faithful one, pass every other gate, and replace a
source this tool then deletes. Two ways it happens were known before this gate existed: every
VAAPI job uploaded `nv12` (8-bit) whatever its plan said, and an encoder whose pixel format list
cannot carry a plan has ffmpeg auto-select another with only a warning, which `-loglevel error`
hides (`fftools/ffmpeg_mux_init.c` at the pinned revision,
https://github.com/FFmpeg/FFmpeg/blob/5d4d3bdc61/fftools/ffmpeg_mux_init.c , read 2026-09-29 by
proposal P2). The gate does not depend on the command line being right: it measures the file.

What the plan declares it changes is part of the declaration, so it is never a mismatch: the
bit-depth floor (8 to 10), a configured `pixel_format`, and the HDR10 tag defaults a source
carrying HDR10 metadata but under-signalling its tags is given. A tag the plan writes nothing
for - the source signals none - has nothing to lose and is not compared.

How the output is read. Two probes (`probe.OutputFacts`): the stream level - the pixel
format, the colour tags and the side data - and the first decoded frame - its colour tags and
side data. The side data is read in the flat form, and the frame-then-stream order, the guards
read a source's in, by the same HDR10 readers. The stream-level tags of a Matroska file are the
container's colour elements, and the decoded frame's are the bitstream's. The two can disagree, and a player may trust either one, so a declared tag is
carried when some level of the output signals it and no level signals anything else; an
output that signals it at neither level has lost it. A level that signals nothing is not a
contradiction, because on the pinned ffmpeg it is ordinary: the encoder takes its colour
primaries and transfer from the decoded frames rather than from `-color_primaries` and
`-color_trc` (measured here, 2026-09-30, with the pinned `N-125875-g5d4d3bdc61`: those two
options change neither level where the frames carry the tag, and leave the Matroska elements
unset where they do not, while `-colorspace` and `-color_range` take effect), and libx265
writes them into the bitstream from its own parameters. Anything the gate cannot establish -
an output pixel format it cannot take apart, a plan's format it cannot, a block present on the
source whose values could not be read - is a mismatch, never a pass. The declaration itself is
held to the same rule: a source whose side-data probe did not answer is not declared to carry
no HDR10 block (which would hold the output to nothing); the plan is refused and the source
kept (`probe.VideoProps.SideDataAnswered`,
`TestFidelityGate_AnUnreadSourceSideDataIsRefusedNotDeclaredAbsent`).

The same measurement is why the command line stamps the declared primaries and transfer
onto the frames (`hdr.Color.SetParams`, a `setparams` filter at the head of the chain, after
the picture operations and before any upload to a hardware surface) for every encoder but
libx265, which writes them from its own parameters and whose command line does not change.
Before it, an `svtav1` or hardware encode of an HDR10 source whose bitstream under-signals
those two tags - the plan declares the HDR10 defaults - wrote PQ samples tagged with neither:
a silent loss this gate now rejects, and the stamp now prevents
(`TestFidelityGate_EveryEncoderCarriesTheDeclaredPrimariesAndTransfer`).

A remux is not held to this gate: it re-encodes nothing, and the video-identity check holds
its video to bit-identity, which is stronger.

The proofs, one fixture per field, are in `internal/engine/fidelity_gate_test.go`: a 4:2:2
10-bit HDR10 source is encoded by an encoder faithful in every field but one - bit depth,
chroma subsampling, primaries, transfer, matrix, range, the mastering-display block, the
content-light block - and each is rejected by this gate naming exactly that field, with the
source byte-identical; holdfast's own encoder over the same source passes, and a stand-in for a
hardware encoder's ffmpeg that writes 8-bit for a 10-bit plan is rejected by bit depth.
`TestEncodePlan_EveryGateReadsThePlan` shows the verdict moving with the plan's declaration
alone.

## What the plan does not declare

The plan says what the output is. How the encode is scheduled on this machine is not part of
that. The libx265 thread pools, the progress channel and the mux queue bounds are execution
parameters of the run: the encoder joins them to the command line, and no gate reads them. The
resident-memory watchdog and the ffmpeg binary the encoder runs are the run's too, and are not
on the plan; the gates take their measurements through the prober, whatever the encoder ran.
The length gate's two thresholds
are not on the plan either: its tolerance (`duration_tolerance_sec`) is a run-wide setting,
read from the engine's configuration exactly as the stray-temp sweep that asks the same
question reads it, and its packet-count bound is a constant. The guards are not part of it either: they decide
whether a file is encoded at all, run before a plan exists, and read the same probe snapshot
the derivation reads.

A working file a killed run left behind has no plan to read: the job that derived one is gone.
The startup sweep that finds such a file asks the length gate's own question of it
(`lengthParity`, the same function) only to decide whether to keep it, and it never licenses a
swap.

The plan is not the `holdfast plan` command, which predicts what a pass would do to a library
and encodes nothing.

## How the refactor kept behaviour

The plan changed how a command line is derived and nothing about what it says. The golden
command lines under `internal/engine/testdata/golden-argv` were written by the build before
the refactor and are read back unchanged after it (`TestGoldenArgv`): through the encoder
directly, every registry encoder over every option combination the existing fixtures drive,
and the refusals; through a whole pass of the engine, 76 combinations the engine resolves
(root and band profiles, encode profiles, the guards' snapshot, the intended stream map, the
working path, scratch, progress) for the default encoder and 16 core ones for every registry
encoder, with the terminal row's status and reason each pass recorded. The tests written against the builder and the
gates before the plan still grade them, through adapters that assemble the facts they hand in
into a plan; no assertion was removed or relaxed. The identity of the plan the command line
was built from and the plan the gates read is checked on a real job
(`TestEncodePlan_TheCommandLineAndTheGatesReadOneDerivation`). Every gate's verdict is shown
to move with the plan it is handed (`TestEncodePlan_EveryGateReadsThePlan`): the exists, codec,
length, size, stream-parity and decode gates, the perceptual gate's three floors, and the
video-identity check a stream copy is held to. The perceptual gate is also shown to build its
comparison through the plan's picture operations.

One recorded fact changed, deliberately, and it is not a decision or a command line. A
remux-only job under a `max_height` below its source used to record that its picture was
scaled, naming a scaler, for a stream copy that scaled nothing: the row's scale was resolved
apart from the encode, without asking whether anything re-encoded the video. It now records
the plan's answer, which is no scale, as the deinterlace beside it always did
(`TestEncodePlan_ARemuxRecordsNoPictureOperationItDidNotApply`).

Three edges move with the derivation's new place, and none is reachable from a running
holdfast, whose one encoder is the production one, built with the engine's own configuration
and a prober. A job whose plan cannot be derived never reaches its encoder, so a test's
stand-in encoder that ignores its configuration no longer runs for it. An encoder the engine
hands a plan does not consult the configuration it was built with. And it needs no prober,
because it takes no snapshot of its own.
