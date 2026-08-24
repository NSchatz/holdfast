#!/usr/bin/env bash
# Ask upstream, out loud, whether the pinned ffmpeg is still there.
#
#   make check-pin-live      # the entry point; CI's schedule runs exactly this
#
# WHY THIS EXISTS (S0022). The previous pin was a mid-month daily build. Upstream keeps
# the last 14 dailies, so it 404ed about a fortnight after it was chosen, and the only
# symptom anybody ever saw was a red `install ffmpeg` step on somebody else's unrelated
# pull request, in three different specs, for days. Nobody was told the pin had expired,
# because nothing was watching the pin: the expiry was discovered as collateral damage.
#
# This check is the alarm. It runs on a SCHEDULE, on no pull request, so when the pin
# does expire (a month-end build is retained two years, so roughly 2028) the failure
# arrives as itself, naming the pin and its expiry, in a job whose entire subject is the
# pin.
#
# It is deliberately NOT part of `make check`. `make check` is the PR gate, and a gate
# that fails when a third party has a bad afternoon is a gate that trains people to
# re-run it. The PR gate asserts what can be asserted locally (scripts/check-pins.sh:
# the tag is a dated month-end release, the digests are real digests, NOTICE agrees);
# reachability is a question only the network can answer, so it is asked on its own.
set -euo pipefail

EX_EXPIRED=4
EX_UNREACHABLE=5

ATTEMPTS=3

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dockerfile="$here/Dockerfile"
RELEASE_BASE="https://github.com/BtbN/FFmpeg-Builds/releases/download"

arg() {
  local name="$1" value
  value="$(sed -n "s/^ARG ${name}=\\(.*\\)$/\\1/p" "$dockerfile" | head -1)"
  [ -n "$value" ] || { echo "::error::no 'ARG ${name}=' in $dockerfile" >&2; exit 2; }
  printf '%s' "$value"
}

command -v curl >/dev/null 2>&1 || { echo "::error::curl is required" >&2; exit 2; }

# Parsed, never restated. Same rule as the installer: the Dockerfile ARGs are the pin.
build="$(arg FFMPEG_BUILD)"
version="$(arg FFMPEG_VERSION)"

# The retention horizon, derived from the tag's own date. WHICH horizon depends on which
# KIND of build the tag names, and the difference is two years against a fortnight, so
# this does not assume: a month-end tag gets +2 years, anything else gets the 14-daily
# window. scripts/check-pins.sh refuses a non-month-end pin outright, so in a healthy
# repository only the first branch is ever taken - but reporting a two-year horizon for a
# build that upstream will delete next week would be a comforting lie in exactly the
# situation this script exists for.
expiry="unknown"
days_left=""
retention="unknown (the tag is not a dated release; see 'make check-pins')"
if [[ "$build" =~ ^autobuild-([0-9]{4}-[0-9]{2}-[0-9]{2})-[0-9]{2}-[0-9]{2}$ ]]; then
  pin_day="${BASH_REMATCH[1]}"
  if next_day="$(date -u -d "$pin_day +1 day" +%d 2>/dev/null)"; then
    if [ "$next_day" = "01" ]; then
      horizon="+2 years"
      retention="a month-end build, which upstream keeps for two years"
    else
      horizon="+14 days"
      retention="a MID-MONTH DAILY build, of which upstream keeps only the last 14, so roughly a fortnight ('make check-pins' refuses this pin)"
    fi
    expiry="$(date -u -d "$pin_day $horizon" +%F)"
    days_left=$(( ( $(date -u -d "$pin_day $horizon" +%s) - $(date -u +%s) ) / 86400 ))
  fi
fi

echo "== pin health: ffmpeg ${version} (${build})"
echo "   upstream retention: ${retention}"
echo "   this pin is retained until: ${expiry}${days_left:+ (${days_left} days left)}"

probe() {  # <url> -> 0 ok | 1 gone | 2 transient ; sets $http_code
  local url="$1" code rc=0
  # A one-byte RANGE GET rather than HEAD: GitHub redirects a release asset to a signed
  # URL whose signature is method-scoped, and a HEAD against it can come back 403 for
  # reasons that have nothing to do with whether the asset exists. A range GET is the
  # cheap request that actually answers the question.
  code="$(curl -sSL --connect-timeout 20 --max-time 120 -r 0-0 \
            -o /dev/null -w '%{http_code}' "$url" 2>/dev/null)" || rc=$?
  http_code="$code"
  if [ "$rc" -eq 0 ] && { [ "$code" = "200" ] || [ "$code" = "206" ]; }; then return 0; fi
  case "$code" in
    404|410)    return 1 ;;
    5??|000|"") return 2 ;;
  esac
  [ "$rc" -eq 0 ] || return 2
  return 2
}

expired() {
  local url="$1"
  echo "::error::the pinned ffmpeg release has EXPIRED upstream (HTTP ${http_code}).
       pinned build tag : ${build}
       pinned version   : ${version}
       retained until   : ${expiry}
       url probed       : ${url}
       This is the alarm this check exists to raise. Repin the Dockerfile's FFMPEG_*
       ARGs to a month-end build with its published SHA-256 digests, update NOTICE's
       Build tag and Version, and run 'make check'. Retrying will not help: upstream
       keeps the last build of each month for two years and only the last 14 dailies." >&2
  exit "$EX_EXPIRED"
}

unreachable() {
  local url="$1"
  echo "::error::could not REACH upstream to check the ffmpeg pin after ${ATTEMPTS} attempts (last HTTP status '${http_code}').
       pinned build tag : ${build}
       retained until   : ${expiry}
       url probed       : ${url}
       The pin is NOT known to be missing; this looks transient (DNS, timeout, or a 5xx).
       It is still reported non-zero rather than skipped, because a health check that
       goes quiet when it cannot see anything is a health check that reports nothing." >&2
  exit "$EX_UNREACHABLE"
}

for slug in linux64 linuxarm64; do
  url="${RELEASE_BASE}/${build}/ffmpeg-${version}-${slug}-gpl.tar.xz"
  delay=2
  ok=""
  for attempt in $(seq 1 "$ATTEMPTS"); do
    rc=0
    probe "$url" || rc=$?
    case "$rc" in
      0) ok=yes; break ;;
      1) expired "$url" ;;
      *)
        if [ "$attempt" -ge "$ATTEMPTS" ]; then unreachable "$url"; fi
        echo "   attempt ${attempt}/${ATTEMPTS} for ${slug} failed (HTTP '${http_code}'), retrying in ${delay}s"
        sleep "$delay"
        delay=$((delay * 2))
        ;;
    esac
  done
  [ "$ok" = "yes" ] || unreachable "$url"
  echo "   ok: ${slug} tarball is still served (HTTP ${http_code})"
done

# Early warning, not a failure. The criterion for this check is retrievability, and the
# tarballs ARE retrievable; failing here would red a scheduled job for something that is
# not yet wrong. GitHub surfaces a ::warning:: in the job summary, which is the point:
# somebody should see the horizon before it arrives, not on the day it does.
if [ -n "$days_left" ] && [ "$days_left" -lt 90 ]; then
  echo "::warning::the pinned ffmpeg ${build} falls out of upstream retention on ${expiry} (${days_left} days). Repin to a newer month-end build before then."
fi

echo "== pin health OK: ${build} is still served for both architectures (until ${expiry})"
