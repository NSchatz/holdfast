# Hardware reports

`scripts/hw-report.sh` is how a result from a real GPU reaches this repository. The gate and CI
have no GPU, so every hardware path there is proven on stand-ins
([`docs/design/hardware.md`](design/hardware.md)); a report is the evidence from a real device,
run by the owner on the host that has it and committed under `testdata/hw-reports/`.

## What it does

1. Builds two small lossless clips with ffmpeg: `sdr8`, 8-bit 4:2:0 BT.709, and `hdr10`, 10-bit
   4:2:0 BT.2020 PQ carrying a mastering-display block and a content-light block, which the
   script reads back with ffprobe before going on. Both are FFV1 in Matroska, a source no
   holdfast target skips as already encoded. Their length, size and rate are set in one block at
   the top of the script (2 seconds, 640x360, 24 fps), so a report takes minutes.
2. Puts them in a throwaway library under `$HOME` (holdfast refuses a state directory on tmpfs,
   and `/tmp` is tmpfs on a Debian 13 host) and writes a configuration naming the encoder with
   `hw_fallback: skip`, so a missing device is a refusal and never a quiet software encode.
3. Runs `holdfast validate`, then `holdfast run --file` once per clip, so every gate runs and
   each clip has its own wall-clock, then reads the ledger back with `holdfast export`.
4. Writes `testdata/hw-reports/<encoder>-<date>.json` (UTC date), or `--out`. An existing report
   is never overwritten, and the throwaway library is removed whatever happens.

An encoder the start-time probe cannot use ends the script with `encoder '<key>' is unavailable`,
holdfast's own reason, a non-zero exit and no report. So does an encoder key `holdfast validate`
rejects, a missing tool (`jq` always; `docker` in image mode; `ffmpeg` and `ffprobe` in host mode),
or a report in which the final check still finds a forbidden token.

## What a report records

- `encoder`: the key asked for, the encoder each ledger row says ran, and the start-time probe's
  8-bit and 10-bit answers for a hardware encoder.
- `holdfast_version` (`holdfast version`) and `ffmpeg` (the first line of `ffmpeg -version`).
- `device`: `nvidia-smi --query-gpu=driver_version,name --format=csv,noheader` when `nvidia-smi` is
  on the host, the `Driver version:` line of `vainfo --display drm` when `vainfo` is, and for each
  render node its PCI vendor and kernel driver from sysfs and, on an Intel or AMD node, the
  `VAAPI driver:` line ffmpeg prints when it opens the node (no encode). Render node paths are not
  recorded.
- `clips`: for each clip, the source's codec, pixel format, colour tags and HDR10 blocks and its
  size; the ledger row's outcome, skip or failure reason, the encoder that ran, VMAF mean and
  worst-frame pool, the model, the chroma figure, source and output bytes, the saving, the output
  dimensions and `encode_ms`; the output file's codec, pixel format, colour tags and HDR10 blocks;
  and the wall-clock of the clip's `holdfast run`, start-time probe included.

## What it removes

A report is built from the allow-list of fields above, never from a copy of a log or a ledger row.
Then every string in it passes through a redaction that replaces, with `<redacted>`:

- this host's names (`hostname`, `hostname -f`, `uname -n`, `/proc/sys/kernel/hostname`, and each
  one's first label), the user (`$USER`, `$LOGNAME`, `id -un`), the home directory (`$HOME` and the
  passwd entry's), the work directory, the repository and the report's directory;
- any UUID, NVIDIA `GPU-` or `MIG-` identity, MAC address, PCI bus address, run of ten or more
  digits (a board serial) or sixteen or more hex digits;
- any absolute filesystem path.

A token shorter than three characters is not replaced. After the redaction the script checks the
file again, byte for byte for the literal tokens and string by string for the shapes, and refuses
to write it if anything is still there. `scripts/hw-report.sh --verify <file>` runs that check
alone, on the host that wrote the report, before it is committed. `scripts/hwreport` proves it: a
report written through stand-in `hostname`, `nvidia-smi` and `vainfo` tools that print planted
host names, users, home paths, GPU UUIDs, serials and MAC addresses in the very fields the script
copies carries none of them, nor this machine's own host name, user, home or temporary paths.

## Running it

Image mode runs holdfast, ffmpeg and ffprobe in the image as your user
(`docker run --rm -u "$(id -u):$(id -g)"`); `--docker-arg` passes one `docker run` argument per
use. Host mode runs a holdfast binary with the host's ffmpeg and ffprobe. From a clone:

```bash
docker build -t holdfast:hw .
# NVIDIA (NVENC), with the NVIDIA Container Toolkit
scripts/hw-report.sh --encoder nvenc --image holdfast:hw --docker-arg=--gpus --docker-arg=all
# Intel (QSV, then VAAPI) and AMD (VAAPI): the render node and its group
g="$(stat -c %g /dev/dri/renderD128)"
scripts/hw-report.sh --encoder qsv --image holdfast:hw --docker-arg=--device=/dev/dri --docker-arg=--group-add="$g"
scripts/hw-report.sh --encoder vaapi --image holdfast:hw --docker-arg=--device=/dev/dri --docker-arg=--group-add="$g"
# AMD AMF, on a host install with AMD's runtime (the image cannot carry it)
make build && scripts/hw-report.sh --encoder amf --holdfast ./holdfast
```

Any key `holdfast validate` accepts can be named; the script keeps no list of its own.

## Sources

Read 2026-09-30:

- `vainfo --display drm` and its `<name>: Driver version: <string>` line: libva-utils,
  [`common/va_display.c`](https://github.com/intel/libva-utils/blob/master/common/va_display.c)
  and [`vainfo/vainfo.c`](https://github.com/intel/libva-utils/blob/master/vainfo/vainfo.c).
- ffmpeg's `VAAPI driver: <vendor string>.` line, logged at verbose level when a VAAPI device is
  opened: [`libavutil/hwcontext_vaapi.c`](https://github.com/FFmpeg/FFmpeg/blob/master/libavutil/hwcontext_vaapi.c).
- `nvidia-smi --format=csv,noheader`, and the GPU UUID and board serial being attributes
  `nvidia-smi` can print (which is why the redaction covers both):
  [the nvidia-smi documentation](https://docs.nvidia.com/deploy/nvidia-smi/index.html). The query
  field names `driver_version` and `name` are ASSUMED from `nvidia-smi --help-query-gpu`, which
  was not run here (no goal runs a real GPU); a report whose `device.nvidia` is null on an NVIDIA
  host says they are wrong.
