#!/usr/bin/env bash
# Smoke-test a built transcode image: does the thing we are about to ship actually
# reclaim space without destroying a source, INSIDE the container?
#
# This is not a "did the image build" check — that proves nothing about the engine. It
# drives a real oneshot encode with the image's own bundled ffmpeg and asserts the
# no-loss contract held: the source was replaced by a smaller HEVC file, and no
# work-in-progress temp was left behind.
#
#   ./scripts/smoke-image.sh transcode:ci            # native
#   ./scripts/smoke-image.sh transcode:ci linux/arm64 --no-encode   # exec-only (QEMU)
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
# `transcode run` would exit 0 having done nothing, and this script would be asserting
# against a file the encoder never touched. A fixture that never reaches the encoder is
# not a smoke test.
set -euo pipefail

IMAGE="${1:?usage: smoke-image.sh <image-ref> [platform] [--no-encode]}"
PLATFORM="${2:-}"
MODE="${3:-}"

PLATFORM_ARGS=()
[ -n "$PLATFORM" ] && PLATFORM_ARGS=(--platform "$PLATFORM")

fail() { echo "::error::smoke: $*" >&2; exit 1; }
ok()   { echo "  ok: $*"; }

run_in_image() { docker run --rm "${PLATFORM_ARGS[@]}" "$@"; }

echo "== smoke: $IMAGE ${PLATFORM:+($PLATFORM)}"

# 1. The binary runs, and is the version we stamped.
version_out="$(run_in_image "$IMAGE" version)" || fail "'transcode version' did not run"
echo "$version_out" | grep -qi transcode || fail "unexpected 'version' output: $version_out"
ok "transcode version runs: $(echo "$version_out" | head -1)"

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

# 3. It does not run as root by default.
user="$(docker inspect -f '{{.Config.User}}' "$IMAGE")"
[ -n "$user" ] && [ "$user" != "root" ] && [ "$user" != "0" ] \
  || fail "image default user is root ('$user')"
ok "image default user is non-root ($user)"

if [ "$MODE" = "--no-encode" ]; then
  echo "== smoke: exec-only mode (skipping the encode) — image runs on ${PLATFORM:-native}"
  exit 0
fi

# 4. The real thing: a oneshot encode inside the image, on a real file.
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
[ "$before_kbps" -gt 2500 ] \
  || fail "fixture is only ${before_kbps} kbps — under the default min_bitrate_kbps (2500), so the engine would SKIP it and this test would assert nothing"
ok "fixture: 320x240 H.264, ~${before_kbps} kbps, ${before_size} bytes (above the skip guard)"

# The config must survive the real validator before it drives an encode.
run_in_image -u "$uid:$gid" -v "$work/config.yaml:/config/config.yaml:ro" \
  "$IMAGE" validate --config /config/config.yaml >/dev/null \
  || fail "'transcode validate' rejected the smoke config"
ok "transcode validate accepts the smoke config"

run_in_image -u "$uid:$gid" \
  -v "$work/media:/media" -v "$work/state:/state" \
  -v "$work/config.yaml:/config/config.yaml:ro" \
  "$IMAGE" run --config /config/config.yaml \
  || fail "'transcode run' exited non-zero inside the image"

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
