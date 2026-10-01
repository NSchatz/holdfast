# Hardware encoders

How holdfast decides whether a hardware encoder may run on this host, which device it opens,
and what it does where the hardware is missing. This document is that argument's single home:
`CLAUDE.md` names the rule and links here rather than restating it.

Nothing here relaxes a gate. A hardware encoder's output is held to exactly the gates a
libx265 output is held to (`docs/design/swap.md`, `docs/design/quality-gate.md`,
`docs/design/encode-plan.md#fidelity`); this document is only about whether the encoder is
asked to run at all.

## The rule

<a id="probe"></a>

**An encoder is used only after a real encode through the command line a job would run came
out faithful, at each bit depth a job asks for.** At start, every encoder the configuration can
reach is probed: a tiny lossless clip, once at 8 bits (4:2:0) and once at 10 bits (4:2:0), is
encoded by `engine.ProbeEncode`, which is the production encoder deriving the probe's plan
from the configuration with the encoder and pixel format set - so the probe's command line
carries the device options, the upload or the explicit pixel format, the Main 10 profile and
the quality option a job's carries, and nothing a job's does not. Each output must exist, be
non-empty, be of the encoder's target codec and be of the depth it was asked for
(`encoder.Available`). An encoder that passes neither probe refuses the start, naming why.

## Why the probe runs the job's command line

The probe it replaced ran `ffmpeg -f lavfi -i testsrc2 -c:v <codec> out.mkv`. Two failures
followed from that, both measured before this change (`verify-hw-encode.md` claim 7, in the
program's research):

- For `hevc_vaapi` the generic command line fails in format negotiation, before any device is
  touched (no `hwupload`, no device), so VAAPI could never pass on a working host.
- A codec-only check passes a VAAPI encode that uploads 10-bit frames as 8-bit surfaces: the
  output is real HEVC, just not the file the plan described. Every VAAPI job was encoded that
  way until goal 4 of the program made the upload follow the plan.

A probe that builds its own command line proves that its own command line works. Only the
job's command line, built by the job's builder, proves anything about the job. The 10-bit
probe is there because every derived plan is 10-bit (`hdr.DerivePixFmt` floors the depth at
10); the 8-bit one because a forced `pixel_format` can ask for 8. The probes are 4:2:0 only:
a plan of another chroma subsampling is held to the depth its probe showed, and the output
fidelity gate stays the backstop for the layout (ASSUMED until a hardware report shows 4:2:2
and 4:4:4 on real devices).

The probes open no device in the gate or CI: every hardware path there is proven against a
stand-in ffmpeg that records the command line it was handed, and the one test that opens a
real device is behind the `hwlive` build tag, which neither the Makefile nor CI sets
(brief T9).

<a id="detection"></a>

## Which device an encoder opens

**VAAPI and QSV open the render node this host assigned them, and every VAAPI device is opened
with `connection_type=drm`.** At start `internal/hwdevice` lists `/dev/dri/renderD*`, reads each
node's PCI vendor from sysfs (`/sys/class/drm/<node>/device/vendor`: `0x8086` Intel, `0x1002`
AMD, `0x10de` NVIDIA, from https://pci-ids.ucw.cz/ read 2026-09-30) and tries to open it
read-write. VAAPI is assigned the first usable Intel or AMD node; QSV the first usable Intel
node. A node this process may not open is reported with the lever: the container's user is not
in the node's group, which `group_add` (compose) or `--group-add` (docker run) fixes, and the
message names the node's GID. The assignment and every node are logged once at start, and an
encoder refused for want of a node says why beside the probe's own reason.

NVENC is not told a node: it reaches the GPU through the CUDA driver the NVIDIA Container
Toolkit injects. AMF is not supported in the image (below).

`connection_type=drm` matters because of how the pinned ffmpeg loads its VAAPI libraries: it
opens libva, libdrm and the X11 libraries lazily through generated stubs, and a stub whose
library is missing aborts the process (exit 134). Without `connection_type=drm`, a node that
fails to open falls through to an X11 display (`libavutil/hwcontext_vaapi.c:1753-1880` at the
pinned revision `5d4d3bdc61`), and the image carries no libX11, so a missing node aborted
ffmpeg instead of saying the node was missing; with it, the same run ends in a clean "No VA
display found" (measured 2026-09-29, `verify-hw-encode.md` claim 1). QSV is handed a QSV device
derived from a VAAPI device opened the same way, because without one the encoder opens its own
VAAPI display with no node named, which is the same fallthrough.

<a id="amf"></a>

## AMF in the container image

**`encoder: amf` is refused at start in the container image, with the reason, and works on a
host install.** AMD's AMF runtime (`libamfrt64`, in `amf-amdgpu-pro`) is licensed under the
AMDGPU PRO EULA, which grants a licence "to a) install and use the Software solely in Object
Code form" and says "You have no other rights in the Software": there is no redistribution
grant, so a published image of this AGPL project cannot carry it
(https://repo.radeon.com/amf/copyright, read 2026-09-29). AMD's own advice for Linux is VA-API
through Mesa, which the image does carry (`radeonsi`), so on AMD hardware the image runs
`encoder: vaapi`. The owner approved this at Checkpoint T (proposal P3, option (a)).

The image's binary knows it is the image's: the Dockerfile builds it with the
`holdfast_image` build tag (`internal/version.Packaging`), and nothing else does. The refusal
happens before any probe, so no AMF library is ever looked for there. `amf` stays a valid key
(`holdfast validate` accepts it) and is never read as `vaapi`: they are different encoders with
different quality scales, and a configuration that says one is never run as the other.

<a id="auto"></a>

## `encoder: auto`

**`encoder: auto` chooses, per job, the first hardware HEVC encoder that passed this host's
start-time probe for the job's plan, in the order `nvenc`, `qsv`, `vaapi`, `amf`, and where none
does, `hw_fallback` decides.** It is opt-in, like every new transformation in this program: a
configuration that does not say `auto` runs exactly the encoder it names, as before.

- It chooses **per job**, at the pixel-format guard, because the answer depends on the job's
  own plan: an encoder whose 10-bit probe failed may still carry an 8-bit plan (a forced
  `pixel_format`), and a plan the encoder's pixel-format list cannot carry is not handed to it.
- It hands hardware **only 4:2:0 plans**, the layout the probe encodes. A 4:2:2 or 4:4:4 plan
  goes to the fallback: `auto` is a choice made on evidence, and there is none for those.
- Every encoder it may choose, and the software encoder it falls back to, writes **HEVC**, so a
  file's target codec does not depend on which hardware a host has: the already-at-target skip
  and the output codec check read the same answer on every host, and moving a library between
  an Intel and an NVIDIA host re-encodes nothing.
- The row records the encoder that ran (`nvenc`, `vaapi`, `cpu`, ...), never `auto`; the
  decision inputs record the configured `auto`, so an unchanged configuration re-opens nothing.
  An encoder named explicitly is recorded as written (an alias such as `hevc_vaapi` stays the
  alias), unless a fallback replaced it, in which case the row names the software encoder.
- `holdfast plan` runs the same probe where the configuration reaches `auto`, so it reports a
  file `auto` would hand to the hardware as one a run would transcode, and not as skipped.
- The order is ASSUMED, not measured: NVENC first; QSV before VAAPI on Intel because QSV is
  Intel's own runtime; VAAPI before AMF because AMD's advice on Linux is VA-API through Mesa.
  The hardware reports (brief T43) are what would change it.

<a id="fallback"></a>

## `hw_fallback`: what a job does without its hardware

**`hw_fallback` is set per library root (or at the top level), is `skip` or `software`, and
defaults to `skip`.**

- `skip`: no other encoder is used. A hardware encoder the start-time probe found unusable
  refuses the start, naming the root and the lever, as a configured encoder that does not work
  always has - it would skip every file it was asked to encode, and a run that says so at start
  is better than a run that says so per file. A job whose plan the probe did not show the
  encoder carrying (an 8-bit-only device and a 10-bit plan), or that `auto` finds no hardware
  for, is skipped `hardware-unavailable`, re-decided on every pass, and the source stays as it
  is. A hardware encode that fails at run time fails the job, as it always has: the source is
  untouched and `max_failures` decides the retries.
- `software`: the software encoder of the same codec runs instead - `cpu` (libx265) for the HEVC
  encoders and `auto`, `svtav1` for the AV1 ones, `x264` (libx264) for the H.264 ones. An unusable hardware encoder is stated at start
  and the run proceeds; a hardware encode that fails at run time is encoded once more in
  software, from a plan derived for that encoder, before any gate runs; the row records the
  software encoder.

**Why `skip` is the default.** Three reasons, in order of weight:

1. An existing configuration keeps its decisions and command lines as far as the probe allows
   (the program's rule for every new key, brief I5): under `skip`, a configuration that names
   an encoder gets that encoder or a refusal, and never another encoder. One decision does
   change, in the fail-safe direction: an encoder whose 8-bit probe passes and whose 10-bit
   probe fails used to start and attempt every 10-bit plan (the fidelity gate then rejected
   the output); it now starts and skips those plans `hardware-unavailable` before any encode.
2. A software encode is not a quiet substitute. libx265 at the configured preset can take many
   times the wall-clock of the hardware encoder the operator chose, on a host whose CPU is
   shared; an operator who wants that trade has one key to say so.
3. Nothing is lost by skipping: the source is never touched, the skip is logged, counted
   (`holdfast_skips_total{guard="hardware-unavailable"}`) and re-decided on every pass, so the
   file is encoded once the hardware is there or the fallback changes.

The fallback never relaxes a gate: a software output is held to exactly the gates a hardware
one is, against the plan it was encoded from, and the VMAF floors stay the root's whichever
encoder wrote the file.
