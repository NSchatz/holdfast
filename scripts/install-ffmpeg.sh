#!/usr/bin/env bash
# Install the PINNED ffmpeg that the safety proof runs against — the same build the
# image bundles, because the pin is read straight out of the Dockerfile.
#
#   ./scripts/install-ffmpeg.sh [dest]     # default: /opt/ffmpeg
#
# The Dockerfile's FFMPEG_* ARGs are the SINGLE SOURCE OF TRUTH. They used to be
# duplicated into the CI workflow with a comment asking the next person to keep the two
# in step — but a pin enforced by prose is not a pin. If the two drifted, both files
# would still be internally consistent, every checksum would still verify, CI would go
# green, and the fixture suite would be proving the no-loss contract against an ffmpeg
# that is NOT the one in the shipped image. That is precisely the failure the pin exists
# to prevent, so the pin is parsed, never restated.
set -euo pipefail

DEST="${1:-/opt/ffmpeg}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dockerfile="$here/Dockerfile"

arg() {
  local name="$1" value
  value="$(sed -n "s/^ARG ${name}=\\(.*\\)$/\\1/p" "$dockerfile" | head -1)"
  [ -n "$value" ] || { echo "::error::no 'ARG ${name}=' in $dockerfile" >&2; exit 1; }
  printf '%s' "$value"
}

build="$(arg FFMPEG_BUILD)"
version="$(arg FFMPEG_VERSION)"

case "$(uname -m)" in
  x86_64)         slug=linux64;    sha="$(arg FFMPEG_SHA256_AMD64)" ;;
  aarch64|arm64)  slug=linuxarm64; sha="$(arg FFMPEG_SHA256_ARM64)" ;;
  *) echo "::error::unsupported arch $(uname -m)" >&2; exit 1 ;;
esac

tarball="ffmpeg-${version}-${slug}-gpl.tar.xz"
url="https://github.com/BtbN/FFmpeg-Builds/releases/download/${build}/${tarball}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "fetching pinned ffmpeg ${version} (${build}, ${slug})"
curl -fsSL -o "$tmp/ffmpeg.tar.xz" "$url"
printf '%s  %s\n' "$sha" "$tmp/ffmpeg.tar.xz" > "$tmp/ffmpeg.sha256"
sha256sum -c "$tmp/ffmpeg.sha256"

mkdir -p "$DEST"
tar -C "$DEST" --strip-components=1 -xf "$tmp/ffmpeg.tar.xz"

# The two capabilities the safety gate DEPENDS on. Without libvmaf the perceptual gate
# is unmeasurable, and an unmeasured output is rejected — so a wrong ffmpeg does not
# weaken the contract, it stops the tool. Fail here instead, loudly.
#
# Captured, not piped: `ffmpeg -filters | grep -q libvmaf` lets grep exit on the first
# match and SIGPIPE ffmpeg, and under `set -o pipefail` that fails the check on a build
# that HAS libvmaf. (Observed, not theorised — it did exactly this.)
filters="$("$DEST/bin/ffmpeg" -hide_banner -filters)"
grep -q libvmaf <<<"$filters" \
  || { echo "::error::the pinned ffmpeg lacks libvmaf"; exit 1; }
encoders="$("$DEST/bin/ffmpeg" -hide_banner -encoders)"
grep -q libx265 <<<"$encoders" \
  || { echo "::error::the pinned ffmpeg lacks libx265"; exit 1; }

echo "ffmpeg ${version} installed to ${DEST} (checksum verified; libx265 + libvmaf present)"
