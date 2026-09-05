#!/usr/bin/env bash
# Install the PINNED ffmpeg that the safety proof runs against - the same build the
# image bundles, because the pin is read straight out of the Dockerfile.
#
#   ./scripts/install-ffmpeg.sh [dest]     # default: /opt/ffmpeg
#
# The Dockerfile's FFMPEG_* ARGs are the SINGLE SOURCE OF TRUTH. They used to be
# duplicated into the CI workflow with a comment asking the next person to keep the two
# in step - but a pin enforced by prose is not a pin. If the two drifted, both files
# would still be internally consistent, every checksum would still verify, CI would go
# green, and the fixture suite would be proving the no-loss contract against an ffmpeg
# that is NOT the one in the shipped image. That is precisely the failure the pin exists
# to prevent, so the pin is parsed, never restated.
#
# WHY THE FAILURE PATHS BELOW ARE AS LOUD AS THEY ARE (S0022). Upstream's retention
# policy deletes builds: month-end builds live two years, the last 14 dailies live about
# a fortnight, and the previous pin was a mid-month daily. It 404ed on schedule, and the
# only thing anybody saw was a red `install ffmpeg` step on unrelated pull requests, in
# three different specs, for days. An expiry that presents as "some CI job is red" costs
# more than the expiry does. So each failure mode now says WHICH failure it is, in its
# own words, and they are distinguishable by exit code as well as by message:
#
#   2  bad usage / a missing tool / an unreadable pin
#   3  the destination cannot be created or written (nothing is fetched)
#   4  the pinned release is GONE upstream - the pin has EXPIRED (404/410)
#   5  upstream could not be reached after bounded retries (transient)
#   6  the archive downloaded but its SHA-256 is not the pinned digest
#   7  the archive verified but does not unpack into a usable installation
#   8  the installed build lacks a capability the safety gate depends on
#
# THE ONE THING THIS SCRIPT WILL NEVER DO IS INSTALL A DIFFERENT FFMPEG. There is no
# distro-package fallback, no floating "latest" alias, no "well, it downloaded, ship it".
# Every path out of a failure leaves the destination with no ffmpeg in it, because a
# substituted encoder is also a substituted MEASURING INSTRUMENT: libvmaf is what decides
# whether a replacement is faithful before the source is deleted. An availability
# incident must never be paid for with the verdict that guards the data.
set -euo pipefail

EX_USAGE=2
EX_DEST=3
EX_EXPIRED=4
EX_UNREACHABLE=5
EX_DIGEST=6
EX_ARCHIVE=7
EX_CAPABILITY=8

# Bounded, and deliberately not configurable: a knob here is a knob that gets turned to
# 0 the first time CI is slow, and "retried forever" and "did not retry" are both ways of
# never learning that the pin died.
ATTEMPTS=3

DEST="${1:-/opt/ffmpeg}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dockerfile="$here/Dockerfile"

# Upstream's release download root. Not the pin - the pin is the tag and the digests, and
# they come from the Dockerfile below. Deliberately not overridable: an origin knob on a
# script whose whole job is provenance is a supply-chain hole with a helpful name.
RELEASE_BASE="https://github.com/BtbN/FFmpeg-Builds/releases/download"

die() { local code="$1"; shift; printf '::error::%s\n' "$*" >&2; exit "$code"; }

arg() {
  local name="$1" value
  value="$(sed -n "s/^ARG ${name}=\\(.*\\)$/\\1/p" "$dockerfile" | head -1)"
  [ -n "$value" ] || { echo "::error::no 'ARG ${name}=' in $dockerfile" >&2; exit "$EX_USAGE"; }
  printf '%s' "$value"
}

for tool in curl sha256sum tar; do
  command -v "$tool" >/dev/null 2>&1 \
    || die "$EX_USAGE" "required tool '$tool' is not on PATH - refusing to guess at an install"
done

build="$(arg FFMPEG_BUILD)"
version="$(arg FFMPEG_VERSION)"

case "$(uname -m)" in
  x86_64)         slug=linux64;    sha="$(arg FFMPEG_SHA256_AMD64)" ;;
  aarch64|arm64)  slug=linuxarm64; sha="$(arg FFMPEG_SHA256_ARM64)" ;;
  *) die "$EX_USAGE" "unsupported arch $(uname -m)" ;;
esac

tarball="ffmpeg-${version}-${slug}-gpl.tar.xz"
url="${RELEASE_BASE}/${build}/${tarball}"

# --- 1. The destination, settled BEFORE a single byte is fetched --------------------
# Ordering is the point: a script that downloads 80MB and then discovers it cannot write
# has already spent the time and still has nothing to show, and on a shared runner it has
# also left the operator guessing which of the two things failed.
mkdir_err="$(mkdir -p -- "$DEST" 2>&1)" || \
  die "$EX_DEST" "destination '$DEST' could not be created: ${mkdir_err:-mkdir failed}. Nothing was downloaded."
[ -d "$DEST" ] \
  || die "$EX_DEST" "destination '$DEST' exists but is not a directory. Nothing was downloaded."

probe="$DEST/.hf-write-probe.$$"
probe_err="$( { : >"$probe"; } 2>&1 )" || \
  die "$EX_DEST" "destination '$DEST' is not writable by $(id -un 2>/dev/null || id -u) (uid $(id -u)): ${probe_err:-permission denied}. Nothing was downloaded."
rm -f "$probe"

stage=""
tmp="$(mktemp -d)" || die "$EX_USAGE" "could not create a temporary directory"
cleanup() { rm -rf "$tmp"; [ -z "$stage" ] || rm -rf "$stage"; }
trap cleanup EXIT

archive="$tmp/ffmpeg.tar.xz"
curl_err="$tmp/curl.err"

# --- 2. Fetch, classifying WHY it failed --------------------------------------------
# "The release is gone" and "the network is having a moment" want opposite responses:
# one is a code change, the other is a retry. Conflating them is how a dead pin spends a
# week being read as flakiness. Note the deliberate absence of `curl -f`: -f collapses
# every HTTP error into exit 22, which is exactly the distinction being made here.
echo "fetching pinned ffmpeg ${version} (${build}, ${slug})"
echo "  from ${url}"

fetch_attempt() {  # 0 = ok | 1 = gone | 2 = transient | 3 = other hard HTTP error
  local code rc=0
  code="$(curl -sSL --connect-timeout 20 --max-time 1800 \
            -o "$archive" -w '%{http_code}' "$url" 2>"$curl_err")" || rc=$?
  http_code="$code"
  if [ "$rc" -eq 0 ] && [ "$code" = "200" ]; then return 0; fi
  case "$code" in
    404|410)     return 1 ;;
    5??|000|"")  return 2 ;;
  esac
  [ "$rc" -eq 0 ] || return 2
  return 3
}

delay=2
got=""
for attempt in $(seq 1 "$ATTEMPTS"); do
  rc=0
  fetch_attempt || rc=$?
  case "$rc" in
    0) got=ok; break ;;
    1)
      die "$EX_EXPIRED" "the pinned ffmpeg release has EXPIRED - upstream no longer serves it (HTTP ${http_code}).
       pinned build tag : ${build}
       pinned version   : ${version}
       url attempted    : ${url}
       This is NOT a network problem and retrying will not fix it. BtbN/FFmpeg-Builds
       keeps the last build of each MONTH for two years and only the last 14 DAILY
       builds, so a pin ages out on a published schedule. Repin the Dockerfile's FFMPEG_*
       ARGs to a month-end build (and its published SHA-256 digests) and update NOTICE.
       No ffmpeg was installed and none was substituted."
      ;;
    2)
      if [ "$attempt" -ge "$ATTEMPTS" ]; then
        die "$EX_UNREACHABLE" "could not REACH upstream after ${ATTEMPTS} attempts (last HTTP status '${http_code}').
       url attempted : ${url}
       pinned build  : ${build}
       The pinned release is not known to be missing - this looks like a transient
       network or upstream fault (DNS, timeout, connection reset, or a 5xx), NOT an
       expired pin. curl said: $(tr '\n' ' ' <"$curl_err" 2>/dev/null || true)
       No ffmpeg was installed and none was substituted."
      fi
      echo "  attempt ${attempt}/${ATTEMPTS} failed (HTTP '${http_code}') - transient, retrying in ${delay}s"
      sleep "$delay"
      delay=$((delay * 2))
      ;;
    *)
      die "$EX_UNREACHABLE" "unexpected HTTP status '${http_code}' fetching the pinned ffmpeg.
       url attempted : ${url}
       pinned build  : ${build}
       This is neither a 404/410 (an expired pin) nor a recognised transient fault, so it
       is reported rather than retried. No ffmpeg was installed and none was substituted."
      ;;
  esac
done
[ "$got" = "ok" ] || die "$EX_UNREACHABLE" "the fetch loop ended without a downloaded archive - refusing to continue"

# --- 3. The digest gate, BEFORE anything is unpacked --------------------------------
# An archive that answers 200 is not evidence of anything. This is the only step that
# turns "a file arrived" into "the pinned build arrived", so it runs before tar does.
actual="$(sha256sum "$archive" | awk '{print $1}')"
if [ "$actual" != "$sha" ]; then
  die "$EX_DIGEST" "SHA-256 MISMATCH - the downloaded archive is NOT the pinned build. Nothing was extracted.
       expected : ${sha}
       actual   : ${actual}
       url      : ${url}
       build    : ${build} (${slug})
       Either upstream re-cut this tag, something is rewriting the download, or the
       Dockerfile's FFMPEG_SHA256_* is wrong. The archive has been discarded; '${DEST}'
       has no ffmpeg in it and nothing was substituted for the pinned build."
fi
echo "  sha256 ok: ${actual}"

# --- 4. Unpack into a STAGING dir, never straight into the destination ---------------
# Unpacking into $DEST directly means a truncated or wrong-shaped archive leaves a
# half-populated tree that the next step reads as an installation. Stage, prove, then
# publish. The stage lives inside $DEST so the publish is a same-filesystem rename and
# cannot fail halfway across a device boundary.
stage="$DEST/.hf-install-stage.$$"
rm -rf "$stage"
mkdir -p "$stage" || die "$EX_DEST" "could not create the staging directory '$stage'"

if ! tar_err="$(tar -C "$stage" --strip-components=1 -xf "$archive" 2>&1)"; then
  die "$EX_ARCHIVE" "the archive verified but could NOT BE UNPACKED. Nothing was installed.
       url   : ${url}
       build : ${build} (${slug})
       tar   : ${tar_err}
       '${DEST}' has no ffmpeg in it: the unpack happened in a staging directory that has
       been removed, so there is no partial install for a later step to mistake for a
       good one, and nothing was substituted."
fi

for bin in ffmpeg ffprobe; do
  if [ ! -f "$stage/bin/$bin" ]; then
    die "$EX_ARCHIVE" "the archive unpacked but 'bin/${bin}' is MISSING - this is not a usable ffmpeg installation. Nothing was installed.
       url   : ${url}
       build : ${build} (${slug})
       '${DEST}' has no ffmpeg in it and nothing was substituted."
  fi
  if [ ! -x "$stage/bin/$bin" ]; then
    die "$EX_ARCHIVE" "the archive unpacked but 'bin/${bin}' is NOT EXECUTABLE - this is not a usable ffmpeg installation. Nothing was installed.
       url   : ${url}
       build : ${build} (${slug})
       '${DEST}' has no ffmpeg in it and nothing was substituted."
  fi
done

# --- 5. The capabilities the safety gate DEPENDS on, checked while still staged ------
# Without libvmaf the perceptual gate is unmeasurable, and an unmeasured output is
# REJECTED - so a wrong ffmpeg does not weaken the contract, it stops the tool. Checking
# here rather than after publishing means a build that lacks them never becomes the thing
# on disk that a later step would pick up and use.
#
# Captured, not piped: `ffmpeg -filters | grep -q libvmaf` lets grep exit on the first
# match and SIGPIPE ffmpeg, and under `set -o pipefail` that fails the check on a build
# that HAS libvmaf. (Observed, not theorised - it did exactly this.)
filters="$("$stage/bin/ffmpeg" -hide_banner -filters)" \
  || die "$EX_CAPABILITY" "the staged ffmpeg could not list its filters - it does not run here. Nothing was installed."
grep -q libvmaf <<<"$filters" \
  || die "$EX_CAPABILITY" "the pinned ffmpeg lacks libvmaf. Nothing was installed and nothing was substituted."
encoders="$("$stage/bin/ffmpeg" -hide_banner -encoders)" \
  || die "$EX_CAPABILITY" "the staged ffmpeg could not list its encoders - it does not run here. Nothing was installed."
grep -q libx265 <<<"$encoders" \
  || die "$EX_CAPABILITY" "the pinned ffmpeg lacks libx265. Nothing was installed and nothing was substituted."

# --- 6. Publish ---------------------------------------------------------------------
published=()
shopt -s dotglob
for entry in "$stage"/*; do
  [ -e "$entry" ] || continue
  name="$(basename "$entry")"
  rm -rf "${DEST:?}/$name"
  mv -f "$entry" "$DEST/$name"
  published+=("$name")
done
shopt -u dotglob
rmdir "$stage" 2>/dev/null || true
stage=""

for bin in ffmpeg ffprobe; do
  if [ ! -x "$DEST/bin/$bin" ]; then
    for name in ${published+"${published[@]}"}; do rm -rf "${DEST:?}/$name"; done
    die "$EX_ARCHIVE" "'bin/${bin}' is not present and executable in '${DEST}' after publishing - the install is not usable, so it was REMOVED rather than left half-done for a later step to trust."
  fi
done

echo "ffmpeg ${version} installed to ${DEST} (checksum verified; libx265 + libvmaf present)"
