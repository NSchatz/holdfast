#!/usr/bin/env bash
# Install the PINNED dynamic-HDR tools, dovi_tool and hdr10plus_tool, that the image
# bundles - the same builds, because the pin is read straight out of the Dockerfile.
#
#   ./scripts/install-dynhdr-tools.sh [dest]     # default: /opt/dynhdr-tools
#
# Both land in <dest>/bin/. The Dockerfile's DOVI_TOOL_* and HDR10PLUS_TOOL_* ARGs are
# the SINGLE SOURCE OF TRUTH, exactly as its FFMPEG_* ARGs are for scripts/install-ffmpeg.sh,
# and for the same reason: a pin restated here and kept in step by a comment is a pin
# that drifts, and then the fixture suite proves the Dolby Vision and HDR10+ carry against
# tools that are NOT the ones in the shipped image.
#
# It is modelled on scripts/install-ffmpeg.sh and fails the same ways, with the same exit
# codes, each naming the tool, the pin and the URL:
#
#   2  bad usage / a missing tool / an unreadable pin
#   3  the destination cannot be created or written (nothing is fetched)
#   4  the pinned release is GONE upstream - the pin has EXPIRED (404/410)
#   5  upstream could not be reached after bounded retries (transient)
#   6  the archive downloaded but its SHA-256 is not the pinned digest
#   7  the archive verified but does not unpack into exactly the one binary
#   8  the binary does not run here, or `--version` does not print the pinned version
#
# It installs BOTH or NEITHER. Each tool is fetched, verified and unpacked into a staging
# directory and run from there; only when both have passed is either published. There is
# no cargo build, no distro package and no floating "latest" fallback: these tools decide
# whether a Dolby Vision RPU or HDR10+ metadata survived an encode, so a substituted tool
# is a substituted measuring instrument.
set -euo pipefail

EX_USAGE=2
EX_DEST=3
EX_EXPIRED=4
EX_UNREACHABLE=5
EX_DIGEST=6
EX_ARCHIVE=7
EX_CAPABILITY=8

# Bounded and not configurable, for the reason scripts/install-ffmpeg.sh gives.
ATTEMPTS=3

DEST="${1:-/opt/dynhdr-tools}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dockerfile="$here/Dockerfile"

# Upstream's GitHub owner. Not the pin - the pin is the version and the digests, read from
# the Dockerfile below. Deliberately not overridable: an origin knob on a provenance
# script is a supply-chain hole with a helpful name.
RELEASE_OWNER="https://github.com/quietvoid"

TOOLS=(dovi_tool hdr10plus_tool)

die() { local code="$1"; shift; printf '::error::%s\n' "$*" >&2; exit "$code"; }

arg() {
  local name="$1" value
  value="$(sed -n "s/^ARG ${name}=\\(.*\\)$/\\1/p" "$dockerfile" | head -1)"
  [ -n "$value" ] || { echo "::error::no 'ARG ${name}=' in $dockerfile" >&2; exit "$EX_USAGE"; }
  printf '%s' "$value"
}

for t in curl sha256sum tar; do
  command -v "$t" >/dev/null 2>&1 \
    || die "$EX_USAGE" "required tool '$t' is not on PATH - refusing to guess at an install"
done

case "$(uname -m)" in
  x86_64)        triple=x86_64-unknown-linux-musl;  archsuffix=AMD64 ;;
  aarch64|arm64) triple=aarch64-unknown-linux-musl; archsuffix=ARM64 ;;
  *) die "$EX_USAGE" "unsupported arch $(uname -m)" ;;
esac

declare -A version=() sha=() url=()
for tool in "${TOOLS[@]}"; do
  prefix="$(printf '%s' "$tool" | tr '[:lower:]' '[:upper:]')"
  version[$tool]="$(arg "${prefix}_VERSION")"
  sha[$tool]="$(arg "${prefix}_SHA256_${archsuffix}")"
  url[$tool]="${RELEASE_OWNER}/${tool}/releases/download/${version[$tool]}/${tool}-${version[$tool]}-${triple}.tar.gz"
done

# --- 1. The destination, settled BEFORE a single byte is fetched --------------------
mkdir_err="$(mkdir -p -- "$DEST" 2>&1)" || \
  die "$EX_DEST" "destination '$DEST' could not be created: ${mkdir_err:-mkdir failed}. Nothing was downloaded."
[ -d "$DEST" ] \
  || die "$EX_DEST" "destination '$DEST' exists but is not a directory. Nothing was downloaded."

probe="$DEST/.hf-write-probe.$$"
probe_err="$( { : >"$probe"; } 2>&1 )" || \
  die "$EX_DEST" "destination '$DEST' is not writable by $(id -un 2>/dev/null || id -u) (uid $(id -u)): ${probe_err:-permission denied}. Nothing was downloaded."
rm -f "$probe"

tmp="$(mktemp -d)" || die "$EX_USAGE" "could not create a temporary directory"
# The stage lives inside $DEST so the publish is a same-filesystem rename.
stage="$DEST/.hf-install-stage.$$"
cleanup() { rm -rf "$tmp"; [ -z "$stage" ] || rm -rf "$stage"; }
trap cleanup EXIT
rm -rf "$stage"
mkdir -p "$stage" || die "$EX_DEST" "could not create the staging directory '$stage'"

curl_err="$tmp/curl.err"

fetch_attempt() {  # <url> <out>: 0 = ok | 1 = gone | 2 = transient | 3 = other hard HTTP error
  local code rc=0
  code="$(curl -sSL --connect-timeout 20 --max-time 600 \
            -o "$2" -w '%{http_code}' "$1" 2>"$curl_err")" || rc=$?
  http_code="$code"
  if [ "$rc" -eq 0 ] && [ "$code" = "200" ]; then return 0; fi
  case "$code" in
    404|410)     return 1 ;;
    5??|000|"")  return 2 ;;
  esac
  [ "$rc" -eq 0 ] || return 2
  return 3
}

for tool in "${TOOLS[@]}"; do
  v="${version[$tool]}"; u="${url[$tool]}"; want="${sha[$tool]}"
  archive="$tmp/${tool}.tar.gz"

  # --- 2. Fetch, classifying WHY it failed ------------------------------------------
  echo "fetching pinned ${tool} ${v} (${triple})"
  echo "  from ${u}"
  delay=2
  got=""
  for attempt in $(seq 1 "$ATTEMPTS"); do
    rc=0
    fetch_attempt "$u" "$archive" || rc=$?
    case "$rc" in
      0) got=ok; break ;;
      1)
        die "$EX_EXPIRED" "the pinned ${tool} release has EXPIRED - upstream no longer serves it (HTTP ${http_code}).
       tool          : ${tool}
       pinned version: ${v}
       url attempted : ${u}
       This is NOT a network problem and retrying will not fix it: the release or its
       asset was removed upstream. Repin the Dockerfile's ${tool^^}_* ARGs to a release
       upstream serves (with its published SHA-256 digests) and update NOTICE.
       No tool was installed and none was substituted."
        ;;
      2)
        if [ "$attempt" -ge "$ATTEMPTS" ]; then
          die "$EX_UNREACHABLE" "could not REACH upstream for ${tool} after ${ATTEMPTS} attempts (last HTTP status '${http_code}').
       url attempted : ${u}
       pinned version: ${v}
       The pinned release is not known to be missing - this looks like a transient
       network or upstream fault (DNS, timeout, connection reset, or a 5xx), NOT an
       expired pin. curl said: $(tr '\n' ' ' <"$curl_err" 2>/dev/null || true)
       No tool was installed and none was substituted."
        fi
        echo "  attempt ${attempt}/${ATTEMPTS} failed (HTTP '${http_code}') - transient, retrying in ${delay}s"
        sleep "$delay"
        delay=$((delay * 2))
        ;;
      *)
        die "$EX_UNREACHABLE" "unexpected HTTP status '${http_code}' fetching the pinned ${tool}.
       url attempted : ${u}
       pinned version: ${v}
       This is neither a 404/410 (an expired pin) nor a recognised transient fault, so it
       is reported rather than retried. No tool was installed and none was substituted."
        ;;
    esac
  done
  [ "$got" = "ok" ] || die "$EX_UNREACHABLE" "the fetch loop for ${tool} ended without a downloaded archive - refusing to continue"

  # --- 3. The digest gate, BEFORE anything is unpacked ------------------------------
  actual="$(sha256sum "$archive" | awk '{print $1}')"
  if [ "$actual" != "$want" ]; then
    die "$EX_DIGEST" "SHA-256 MISMATCH - the downloaded ${tool} archive is NOT the pinned build. Nothing was extracted.
       expected : ${want}
       actual   : ${actual}
       url      : ${u}
       version  : ${v} (${triple})
       Either upstream re-cut this release, something is rewriting the download, or the
       Dockerfile's ${tool^^}_SHA256_${archsuffix} is wrong. The archive has been
       discarded; '${DEST}' has no ${tool} in it and nothing was substituted."
  fi
  echo "  sha256 ok: ${actual}"

  # --- 4. Unpack into the STAGING dir: exactly one entry, the binary ----------------
  if ! listing="$(tar -tzf "$archive" 2>&1)"; then
    die "$EX_ARCHIVE" "the ${tool} archive verified but could NOT BE UNPACKED. Nothing was installed.
       url : ${u}
       tar : ${listing}
       '${DEST}' has no ${tool} in it and nothing was substituted."
  fi
  case "$(printf '%s' "$listing" | tr '\n' ' ')" in
    "./${tool}"|"${tool}") ;;
    *)
      die "$EX_ARCHIVE" "the ${tool} archive verified but does not hold exactly one entry, '${tool}' - it holds: $(printf '%s' "$listing" | tr '\n' ' ')
       url : ${u}
       Nothing was installed and nothing was substituted."
      ;;
  esac
  mkdir -p "$stage/$tool.d"
  if ! tar_err="$(tar -C "$stage/$tool.d" -xzf "$archive" 2>&1)"; then
    die "$EX_ARCHIVE" "the ${tool} archive verified but could NOT BE UNPACKED. Nothing was installed.
       url : ${u}
       tar : ${tar_err}
       '${DEST}' has no ${tool} in it and nothing was substituted."
  fi
  bin="$stage/$tool.d/$tool"
  [ -f "$bin" ] || die "$EX_ARCHIVE" "the ${tool} archive unpacked but '${tool}' is MISSING. Nothing was installed.
       url : ${u}"
  [ -x "$bin" ] || die "$EX_ARCHIVE" "the ${tool} archive unpacked but '${tool}' is NOT EXECUTABLE. Nothing was installed.
       url : ${u}"

  # --- 5. It runs here and IS the pinned version, checked while still staged --------
  ver_out=""
  ver_out="$("$bin" --version 2>&1)" \
    || die "$EX_CAPABILITY" "the staged ${tool} does not run here ('--version' failed: $(printf '%s' "$ver_out" | head -3 | tr '\n' ' ')). Nothing was installed."
  ver_out="$(printf '%s' "$ver_out" | head -1)"
  [ "$ver_out" = "${tool} ${v}" ] \
    || die "$EX_CAPABILITY" "the staged ${tool} reports '${ver_out}', not the pinned '${tool} ${v}'. Nothing was installed and nothing was substituted."
  echo "  ${ver_out} runs"
done

# --- 6. Publish both, only now that both passed -------------------------------------
mkdir -p "$DEST/bin" || die "$EX_DEST" "could not create '$DEST/bin'"
published=()
for tool in "${TOOLS[@]}"; do
  rm -f "$DEST/bin/$tool"
  mv -f "$stage/$tool.d/$tool" "$DEST/bin/$tool"
  published+=("$DEST/bin/$tool")
done
rm -rf "$stage"
stage=""

for tool in "${TOOLS[@]}"; do
  if [ ! -x "$DEST/bin/$tool" ]; then
    rm -f "${published[@]}"
    die "$EX_ARCHIVE" "'bin/${tool}' is not present and executable in '${DEST}' after publishing - the install is not usable, so it was REMOVED rather than left half-done."
  fi
done

echo "dovi_tool ${version[dovi_tool]} and hdr10plus_tool ${version[hdr10plus_tool]} installed to ${DEST}/bin (checksums verified; versions confirmed)"
