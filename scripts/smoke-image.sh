#!/usr/bin/env bash
# Smoke-test a built holdfast image: does the thing we are about to ship actually
# reclaim space without destroying a source, INSIDE the container?
#
# This is not a "did the image build" check — that proves nothing about the engine. It
# drives a real oneshot encode with the image's own bundled ffmpeg and asserts the
# no-loss contract held: the source was replaced by a smaller HEVC file, and no
# work-in-progress temp was left behind.
#
#   ./scripts/smoke-image.sh holdfast:ci            # native
#   ./scripts/smoke-image.sh holdfast:ci linux/arm64 --no-encode   # exec-only (QEMU)
#
# The fixture is a 320x240 H.264 clip forced to a REAL ~7.6 Mbps by CBR padding, and the
# config leaves every guard at its shipped default — so this drives the same engine a
# stranger gets: the low-bitrate skip guard has to LET THE FILE THROUGH, and then the
# full gate has to accept the encode (correct codec, duration/packet parity, strictly
# smaller, stream-count parity, decode integrity, VMAF >= 95).
#
# The CBR padding is load-bearing, not incidental. x264 in ABR mode does not pad, and
# testsrc2 is trivially compressible, so a plain `-b:v 8M` lands at ~873 kbps — under
# the default min_bitrate_kbps of 2500. The engine would then SKIP the file (correctly),
# `holdfast run` would exit 0 having done nothing, and this script would be asserting
# against a file the encoder never touched. A fixture that never reaches the encoder is
# not a smoke test.
set -euo pipefail

IMAGE=""
PLATFORM=""
MODE=""
# --no-encode is parsed as a FLAG, in any position. Binding it positionally means the
# obvious `smoke-image.sh holdfast:dev --no-encode` silently becomes
# `docker run --platform --no-encode`, and this script is advertised as the gate a human
# runs by hand.
for arg in "$@"; do
  case "$arg" in
    --no-encode) MODE=--no-encode ;;
    -*) echo "unknown flag: $arg" >&2
        echo "usage: smoke-image.sh <image-ref> [platform] [--no-encode]" >&2; exit 2 ;;
    *)  if [ -z "$IMAGE" ]; then IMAGE="$arg"; else PLATFORM="$arg"; fi ;;
  esac
done
[ -n "$IMAGE" ] || { echo "usage: smoke-image.sh <image-ref> [platform] [--no-encode]" >&2; exit 2; }

PLATFORM_ARGS=()
[ -n "$PLATFORM" ] && PLATFORM_ARGS=(--platform "$PLATFORM")

fail() { echo "::error::smoke: $*" >&2; exit 1; }
ok()   { echo "  ok: $*"; }

run_in_image() { docker run --rm "${PLATFORM_ARGS[@]}" "$@"; }

echo "== smoke: $IMAGE ${PLATFORM:+($PLATFORM)}"

# 1. The binary runs, and is the version we stamped.
version_out="$(run_in_image "$IMAGE" version)" || fail "'holdfast version' did not run"
echo "$version_out" | grep -qi holdfast || fail "unexpected 'version' output: $version_out"
ok "holdfast version runs: $(echo "$version_out" | head -1)"

# 2. The bundled ffmpeg carries the codecs the safety gate DEPENDS on. Without libvmaf
#    the perceptual gate is unmeasurable — and an unmeasured output is rejected, not
#    accepted — so an ffmpeg missing it would not silently weaken the contract, it
#    would stop the tool. Fail here instead, loudly, before anyone ships it.
for want in libvmaf:filters libx265:encoders libsvtav1:encoders; do
  lib="${want%%:*}"; kind="${want##*:}"
  # Captured, not piped: `ffmpeg ... | grep -q` lets grep exit on first match and
  # SIGPIPE ffmpeg, which under `set -o pipefail` fails the whole check for the wrong
  # reason the moment the capability list outgrows the pipe buffer.
  caps="$(run_in_image --entrypoint /usr/local/bin/ffmpeg "$IMAGE" -hide_banner "-${kind}")" \
    || fail "could not list the bundled ffmpeg's ${kind}"
  grep -q "$lib" <<<"$caps" || fail "bundled ffmpeg lacks $lib (checked -${kind})"
  ok "bundled ffmpeg has $lib"
done

# 2b. The bundled dynamic-HDR tools run inside the image and ARE the pinned versions. They
#     are static musl binaries, so this needs nothing from the base; it runs on every
#     architecture this script is pointed at, including arm64 under QEMU, which is the one
#     place an arm64 binary is ever executed (the Dockerfile's fetch stage runs on the
#     build platform and can only check that one's ELF header). The expected versions are
#     parsed from the Dockerfile's ARGs, never restated here.
dockerfile="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/Dockerfile"
for tool in dovi_tool hdr10plus_tool; do
  prefix="$(printf '%s' "$tool" | tr '[:lower:]' '[:upper:]')"
  want="$(sed -n "s/^ARG ${prefix}_VERSION=\\(.*\\)$/\\1/p" "$dockerfile" | head -1)"
  [ -n "$want" ] || fail "could not read ${prefix}_VERSION from $dockerfile"
  got="$(run_in_image --entrypoint "/usr/local/bin/$tool" "$IMAGE" --version 2>&1)" \
    || fail "the bundled $tool does not run inside the image:
$got"
  got="$(head -1 <<<"$got")"
  [ "$got" = "$tool $want" ] || fail "the bundled $tool reports '$got', the Dockerfile pins '$tool $want'"
  ok "bundled $got runs"
done

# 2c. The owner's live-check command (scripts/client-report.sh --image) is in the image and
#     runs. Only its usage text is asked for: nothing here names a service, and no check in
#     any gate contacts one.
got="$(run_in_image --entrypoint /usr/local/bin/holdfast-client-report "$IMAGE" --help 2>&1)" \
  || fail "the bundled holdfast-client-report does not run inside the image:
$got"
grep -q -- '--refresh-dir DIR' <<<"$got" || fail "the bundled holdfast-client-report printed no usage text"
ok "bundled holdfast-client-report runs"

# 3. It does not run as root by default.
user="$(docker inspect -f '{{.Config.User}}' "$IMAGE")"
[ -n "$user" ] && [ "$user" != "root" ] && [ "$user" != "0" ] \
  || fail "image default user is root ('$user')"
ok "image default user is non-root ($user)"

# 4. The hardware runtime (amd64 only). The image carries the VAAPI and QSV userspace
#    (libva, libva-drm, libdrm, Intel iHD, libmfx-gen, Mesa radeonsi VA) as pinned Debian
#    packages. The bundled ffmpeg dlopens libva, libva-drm and libdrm through implib-gen
#    shims and ABORTS (exit 134) when one is missing, and a VA driver whose closure is
#    incomplete fails only on the host that has the hardware, where nobody can see why.
#    CI has no GPU, so this proves the half that can be proven without one: every object
#    resolves every dependency inside the image, and a VAAPI init against a missing
#    render node ends in a device error rather than an abort.
#
#    arm64 carries no hardware runtime (its pinned ffmpeg has no VAAPI and no libvpl; see
#    the Dockerfile's hwruntime stage), so there is nothing there to prove.
arch="$(docker image inspect -f '{{.Architecture}}' "$IMAGE")" || fail "could not read the image's architecture"
if [ "$arch" = "amd64" ]; then
  hwlib=/usr/lib/x86_64-linux-gnu
  # The dynamic loader's list mode is the entrypoint: the image has no shell and no ldd,
  # and this is what ldd itself runs. Its answer is the image's own, with no
  # LD_LIBRARY_PATH: the default search path of the base's glibc, and no ld.so.cache.
  for obj in dri/iHD_drv_video.so dri/radeonsi_drv_video.so libmfx-gen.so.1.2 \
             libva.so.2 libva-drm.so.2 libdrm.so.2; do
    deps="$(run_in_image --entrypoint /lib64/ld-linux-x86-64.so.2 "$IMAGE" --list "$hwlib/$obj" 2>&1)" \
      || fail "the dynamic loader could not load $hwlib/$obj inside the image:
$deps"
    if grep -q 'not found' <<<"$deps"; then
      fail "$hwlib/$obj has an unresolved dependency inside the image:
$(grep 'not found' <<<"$deps")"
    fi
    ok "$obj resolves all $(grep -c '=>' <<<"$deps") of its dependencies inside the image"
  done

  # No device is passed, so the render node is missing. connection_type=drm keeps the
  # failure on the DRM path: without it a node that fails to open falls through to
  # XOpenDisplay, and libX11 is not in the image, so that path aborts in the shim. What
  # must come back is ffmpeg's own device error, never exit 134 and never a shim line.
  rc=0
  va_out="$(run_in_image --entrypoint /usr/local/bin/ffmpeg "$IMAGE" -hide_banner \
    -init_hw_device vaapi=va:/dev/dri/renderD128,connection_type=drm \
    -f lavfi -i nullsrc -frames:v 1 -f null - 2>&1)" || rc=$?
  [ "$rc" -ne 0 ] || fail "a VAAPI init with no render node exited 0:
$va_out"
  [ "$rc" -ne 134 ] || fail "a VAAPI init with no render node ABORTED (exit 134): a library the shim loads is missing:
$va_out"
  if grep -q 'implib-gen' <<<"$va_out"; then
    fail "a VAAPI init with no render node reached an implib-gen shim failure (exit $rc):
$va_out"
  fi
  grep -qE 'Failed to open /dev/dri/renderD128|No VA display found' <<<"$va_out" \
    || fail "a VAAPI init with no render node failed (exit $rc) without naming the device:
$va_out"
  ok "a VAAPI init with no render node ends in a device error (exit $rc), not an abort"

  # The missing node fails at open(), BEFORE ffmpeg touches a shim, so the check above
  # passes on an image with no libva at all. /dev/null opens, so this one really calls
  # vaGetDisplayDRM: libva-drm, libva and libdrm are loaded, and it is the libva-drm call
  # that refuses a node that is not a DRM device. On an image missing any of the three
  # this is the shim abort (exit 134) the runtime exists to prevent.
  rc=0
  va_out="$(run_in_image --entrypoint /usr/local/bin/ffmpeg "$IMAGE" -hide_banner -v verbose \
    -init_hw_device vaapi=va:/dev/null,connection_type=drm \
    -f lavfi -i nullsrc -frames:v 1 -f null - 2>&1)" || rc=$?
  [ "$rc" -ne 0 ] && [ "$rc" -ne 134 ] && ! grep -q 'implib-gen' <<<"$va_out" \
    || fail "a VAAPI init on a node that is not a DRM device did not end in a clean refusal (exit $rc):
$va_out"
  grep -q 'Cannot open a VA display from DRM device /dev/null' <<<"$va_out" \
    || fail "a VAAPI init on /dev/null failed (exit $rc) without reaching libva-drm:
$va_out"
  ok "libva-drm, libva and libdrm load inside the image and refuse a node that is not a DRM device (exit $rc)"
else
  ok "no hardware runtime to check on $arch (the image carries none there, by design)"
fi

# 4b. `encoder: amf` in the image, on every architecture: `holdfast validate` accepts the
#     key, and `holdfast run` refuses it at start with its reason (AMD's EULA grants no
#     redistribution of the AMF runtime, so the image cannot carry it; use vaapi) before
#     anything is probed or opened (docs/design/hardware.md#amf). This is the image's own
#     binary answering: it is built with the holdfast_image tag, and a binary without it
#     would probe amf instead, fail for want of a device, and say nothing about the licence.
amfdir="$(mktemp -d)"
mkdir -p "$amfdir/media" "$amfdir/state"
printf 'library_roots:\n  - /media\nstate_dir: /state\nencoder: amf\n' >"$amfdir/config.yaml"
amf_mounts=(-u "$(id -u):$(id -g)" -v "$amfdir/media:/media" -v "$amfdir/state:/state"
  -v "$amfdir/config.yaml:/config/config.yaml:ro")
run_in_image "${amf_mounts[@]}" "$IMAGE" validate --config /config/config.yaml >/dev/null \
  || { rm -rf "$amfdir"; fail "'holdfast validate' refused encoder: amf (the key must stay valid)"; }
rc=0
amf_out="$(run_in_image "${amf_mounts[@]}" "$IMAGE" run --config /config/config.yaml 2>&1)" || rc=$?
amf_store=no; [ -f "$amfdir/state/jobs.db" ] && amf_store=yes
rm -rf "$amfdir"
[ "$rc" -ne 0 ] || fail "'holdfast run' started with encoder: amf in the image:
$amf_out"
grep -q 'AMDGPU PRO EULA' <<<"$amf_out" && grep -q 'encoder: vaapi' <<<"$amf_out" \
  || fail "'holdfast run' refused encoder: amf (exit $rc) without naming the licence and vaapi:
$amf_out"
[ "$amf_store" = no ] || fail "the amf refusal left a job store behind"
ok "encoder: amf is valid, and refused at start in the image with its reason (exit $rc)"

if [ "$MODE" = "--no-encode" ]; then
  echo "== smoke: exec-only mode (skipping the encode) — image runs on ${PLATFORM:-native}"
  exit 0
fi

# 5. The real thing: a oneshot encode inside the image, on a real file.
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/media" "$work/state"
uid="$(id -u)"; gid="$(id -g)"

cat >"$work/config.yaml" <<'YAML'
library_roots:
  - /media
state_dir: /state
log_level: debug
YAML

# The fixture is built with the IMAGE's ffmpeg — so this also proves the bundled ffmpeg
# can encode, not just report its capabilities. nal-hrd=cbr + filler forces x264 to
# actually HIT the requested bitrate (see the header): the source must be genuinely
# bloated, or the default low-bitrate guard skips it and this test proves nothing.
run_in_image -u "$uid:$gid" -v "$work/media:/media" \
  --entrypoint /usr/local/bin/ffmpeg "$IMAGE" \
  -hide_banner -loglevel error -y -f lavfi \
  -i testsrc2=duration=2:size=320x240:rate=10 \
  -c:v libx264 -preset ultrafast \
  -b:v 8M -minrate 8M -maxrate 8M -bufsize 8M -x264-params nal-hrd=cbr:filler=1 \
  -pix_fmt yuv420p -- /media/sample.mkv \
  || fail "could not build the fixture with the image's ffmpeg"

before_size="$(stat -c %s "$work/media/sample.mkv")"
before_kbps=$(( before_size * 8 / 2 / 1000 ))   # 2-second clip
# 2500 mirrors the shipped default of min_bitrate_kbps (internal/config). It is a copy,
# and copies drift — but this one cannot cause a FALSE GREEN: if the default ever rises
# above the fixture's real bitrate the engine skips the file, the `hevc` assertion below
# reds, and the run fails loudly. This pre-check exists only so that failure says WHY.
[ "$before_kbps" -gt 2500 ] \
  || fail "fixture is only ${before_kbps} kbps — at or under the default min_bitrate_kbps (2500), so the engine will SKIP it and this test would assert against a file the encoder never touched"
ok "fixture: 320x240 H.264, ~${before_kbps} kbps, ${before_size} bytes (above the skip guard)"

# The config must survive the real validator before it drives an encode.
run_in_image -u "$uid:$gid" -v "$work/config.yaml:/config/config.yaml:ro" \
  "$IMAGE" validate --config /config/config.yaml >/dev/null \
  || fail "'holdfast validate' rejected the smoke config"
ok "holdfast validate accepts the smoke config"

run_in_image -u "$uid:$gid" \
  -v "$work/media:/media" -v "$work/state:/state" \
  -v "$work/config.yaml:/config/config.yaml:ro" \
  "$IMAGE" run --config /config/config.yaml \
  || fail "'holdfast run' exited non-zero inside the image"

# --- assert the no-loss contract held ----------------------------------------------
probe() {
  run_in_image -v "$work/media:/media" --entrypoint /usr/local/bin/ffprobe "$IMAGE" \
    -v error -select_streams v:0 -show_entries stream=codec_name \
    -of default=nw=1:nk=1 "/media/$1"
}

[ -f "$work/media/sample.mkv" ] || fail "the source is GONE — it was not replaced, it was lost"
ok "the source path still exists"

codec="$(probe sample.mkv | tr -d '[:space:]')"
[ "$codec" = "hevc" ] || fail "expected the file to be hevc after the run, got '$codec'"
ok "the file at the source path is now HEVC"

after_size="$(stat -c %s "$work/media/sample.mkv")"
[ "$after_size" -lt "$before_size" ] \
  || fail "output is not smaller ($after_size >= $before_size) — it should have been rejected"
ok "reclaimed $(( (before_size - after_size) * 100 / before_size ))% ($before_size -> $after_size bytes)"

leftovers="$(find "$work/media" -type f ! -name sample.mkv)"
[ -z "$leftovers" ] || fail "work-in-progress temp files left behind: $leftovers"
ok "no temp files left behind"

[ -f "$work/state/jobs.db" ] || fail "no job store written to the mounted state dir"
ok "the job store persisted to the mounted state volume"

echo "== smoke PASSED: the image encoded a real file, verified it, and swapped it safely"
