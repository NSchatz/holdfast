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
output they measure and every expectation they hold the output to; the terminal row records
the picture operations it declares. None of them resolves any part of that answer for itself.

## Why one plan

holdfast deletes a source on the strength of its gates. A gate is only worth that trust if
what it checks is what was encoded, and "what was encoded" is decided by the same handful of
inputs every time: the encoder and its quality value, the pixel format, the streams carried,
the deinterlace, the `max_height` scale, the colour description. Before the plan, the encoder
resolved those for itself and the engine resolved them again for the gates, through the same
helpers from the same inputs. Two resolutions that agree today are two answers waiting to
disagree, and the disagreement lands where it costs most: a gate that checks an output
against something other than what was encoded, passes it, and licenses the deletion of the
source.

The intended stream map already followed this rule: it was derived once and handed to both
the command line and the stream-parity gate, and a test proves the two read one derivation
rather than two that happen to agree. The plan extends that rule from which streams are
carried to everything the encode does.

A plan is also where every later transformation lands. A hardware decode path, a device
chosen at run time, an audio re-encode, a subtitle sidecar, a crop and dynamic HDR metadata
each add a declaration to the plan, and each is then read by the command line and by the gate
that proves it happened, from the same value.

## What the plan declares

| Part | What this build declares | Read by |
|---|---|---|
| `Video` | the registry encoder and the device it opens (the render node for `vaapi`, none for every other encoder); the decode path (software, `DecodeSoftware`); the pixel format (forced, or derived from the source's); the quality value (`crf`, or `bitrate_kbps`, and the preset); the codec the output must be in, or a copy of the source's video on a remux-only root | the command line; the codec gate; on a copy, the video-identity check |
| `Audio`, `Subtitles` | every carried stream is copied (`CopyStreams`) | the command line |
| `Streams` | the intended stream map: which source streams the output carries (the same value the row's dropped streams are recorded from) | the command line; the stream-parity gate |
| attached pictures | how the attached pictures the map carries travel: pinned to copy in the map, or copied out of the source and carried as Matroska attachments | the command line |
| `Picture` | the deinterlace applied, the `max_height` scale applied, and the crop (none in this build) | the filter chain; the reference and the scale the perceptual gate builds; the row's provenance |
| `Metadata` | the colour description (`hdr.Color`): the source's tags and, for HDR10, its mastering display and content light level; HDR10+ and Dolby Vision are not carried, because the guards skip a source that has either before a plan is derived | the command line (`-color_*` and the libx265 parameters) |
| `Profile`, `Settings` | the effective library profile, whose floors every gate applies, and the job's effective encode settings with the encode profile that supplied them | every gate's floors (`Profile`); the quality value (`Settings`) |
| container | the muxer the output is written in, named from the working file's name | the command line |
| `Source`, `Output` | the file the encode reads and the working file it writes: the two paths the plan was derived for | the command line (`-i` and the output); every gate, for the files it measures |

## Declared, then built or refused

The plan declares; the command-line builder (`EncodePlan.args`) performs what it declares
and refuses what it cannot. A plan declaring an operation this build has no way to perform -
an audio action other than copy, a crop, a hardware decode path, a device an encoder does not
open, dynamic HDR metadata carried - is refused with an `UnbuildablePlanError` before any
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

## What the plan does not declare

The plan says what the output is. How the encode is scheduled on this machine is not part of
that: the libx265 thread pools, the resident-memory watchdog, the progress channel, the mux
queue bounds and the ffmpeg binary are execution parameters of the run, joined to the command
line by the encoder and read by no gate. The length gate's tolerance (`duration_tolerance_sec`)
is not on the plan either: it is a run-wide setting, read from the engine's configuration
exactly as the stray-temp sweep that asks the same question reads it. The guards are not part of it either: they decide
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
and the refusals; through a whole pass of the engine, each of those combinations for the
default encoder and the core ones for every registry encoder, with the row each pass
recorded. The tests written against the builder and the
gates before the plan still grade them, through adapters that assemble the facts they hand in
into a plan; no assertion was removed or relaxed. The identity of the plan the command line
was built from and the plan the gates read is checked on a real job
(`TestEncodePlan_TheCommandLineAndTheGatesReadOneDerivation`), and each gate's verdict is
shown to move with the plan it is handed (`TestEncodePlan_EveryGateReadsThePlan`).

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
