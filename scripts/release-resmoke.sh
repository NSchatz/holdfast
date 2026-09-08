#!/usr/bin/env bash
# Re-smoke the image that was actually PUSHED, both architectures, before `:latest` moves.
#
# The push is a REBUILD from the buildx cache, not a push of the exact bits the pre-push
# smoke steps loaded: buildx cannot push a multi-arch manifest it only loaded locally. The
# inputs are identical and ffmpeg is sha-pinned, so it is equivalent - but "equivalent" is
# not "the same artifact", and this repository does not accept a gate by equivalence. So the
# artifact is pulled back out of the registry and driven through a real encode.
#
# An unqualified `docker pull` resolves only the runner's own architecture, so the arm64
# half would otherwise ship having been gated as a local build alone. Both are named.
#
# It lives in a file rather than inline in release.yml because the release-shape gate
# identifies this step by the script it invokes, matched WHOLE. A role decided by searching
# a step's text for a word is a role a step can claim by mentioning it (S0046 F13); a role
# decided by "the `run:` is exactly this line" cannot be claimed by any quoting, nesting or
# spelling, because nothing is being searched.
#
# Failure modes are distinct and each exits with its own code:
#
#   2  REF not supplied - the caller is wrong, not the registry
#   3  the pushed reference does not pull back
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

ref="${REF:-${1-}}"
if [ -z "$ref" ]; then
  echo "::error::release-resmoke: no REF given. Call it with the image reference this release just pushed." >&2
  exit 2
fi

# Never smoke a local leftover: the whole point is to grade what came back from the
# registry, and a stale local image of the same name would be graded instead.
docker image rm -f "$ref" >/dev/null 2>&1 || true

pull() {  # pull <platform>
  if ! docker pull --platform "$1" "$ref"; then
    echo "::error::release-resmoke: the pushed image does not pull back for $1: $ref" >&2
    exit 3
  fi
}

pull linux/amd64
"$here/scripts/smoke-image.sh" "$ref"

pull linux/arm64
"$here/scripts/smoke-image.sh" "$ref" linux/arm64 --no-encode

echo "release-resmoke: $ref passed the packaging gate on linux/amd64 and linux/arm64, pulled back from the registry"
