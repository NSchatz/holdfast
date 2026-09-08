#!/usr/bin/env bash
# The two references only a REGISTRY can settle, resolved after the promotion:
#
#   1. the FLOATING reference the promotion just moved must resolve to the digest this run
#      gated. The retag is a registry operation, and nothing else in the release says it
#      landed - `make check` can only read what the workflow HANDS the promotion.
#   2. the EXACT reference the example deployment names must resolve to an image. That is
#      what a stranger pulls, and a release that leaves it pointing at something which does
#      not exist is otherwise discovered only by the first person who tries to run it.
#
# Those two were ONE reference until the example deployment stopped depending on a mutable
# one: it said `:latest`, which was both what a user pulled and what a release moved, so
# resolving it once answered both. It now pins a version tag AND the digest that was gated
# (S0057's P1), while `:latest` goes on being published - publishing a floating reference is
# not depending on one. So each half is resolved against the reference that now carries it,
# and neither assertion is dropped.
#
# Everything else in the release works with `${IMAGE}:${VERSION}`, which the workflow derives
# from `github.repository`, so it is correct by construction and proves nothing about
# docker-compose.yml. `make check` holds that file's NAME against this repository's own
# release and refuses a reference that carries no digest or that pins the tag a release
# moves; this is the half no offline check can cover.
#
# THE REFERENCE HAS ONE READER, and it is not this script. `scripts/release-shape-gate
# -print-compose-ref` decodes docker-compose.yml with a YAML parser and refuses anything it
# cannot answer for (no image, more than one, unparseable); this script asks it. A second
# reader here - a sed for `image:` and a hand-rolled quote strip - would agree with the gate
# on today's file and disagree on a quoted or folded scalar, on a second service carrying an
# `image:`, and on an `image:` key nested outside `services:`. That is the "one value, two
# readers held in step by hope" shape this repository refuses for the ffmpeg pin, which is
# parsed out of the Dockerfile rather than restated.
#
# Failure modes are distinct, named, and each exits with its own code:
#
#   2  IMAGE/VERSION/FLOATING_TAG not supplied - the caller is wrong, not the registry
#   3  docker-compose.yml carries no image reference the one reader can read
#   4  a reference does not resolve at all
#   5  the floating reference resolves to a DIFFERENT digest than the gated version
#   6  the one reader cannot be run here at all - a missing toolchain, not a bad compose file
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
compose="$here/docker-compose.yml"

image="${IMAGE:-${1-}}"
version="${VERSION:-${2-}}"
floating="${FLOATING_TAG:-${3-}}"
if [ -z "$image" ] || [ -z "$version" ] || [ -z "$floating" ]; then
  echo "::error::resolve-compose-image: no IMAGE/VERSION/FLOATING_TAG given. Call it with the image and version the release just published, and the floating tag it just moved." >&2
  exit 2
fi

if [ ! -r "$compose" ]; then
  echo "::error::resolve-compose-image: cannot read docker-compose.yml. The example deployment's image reference is unknown, so it cannot be resolved." >&2
  exit 3
fi

# Ask the one reader. RELEASE_SHAPE_GATE lets a caller that has already built it (the
# self-test) hand over the binary; otherwise it is built from source here, which needs a Go
# toolchain in THIS job - the job that runs `make check` is a different one, and its
# `actions/setup-go` does not reach here.
#
# That preflight is the whole reason for exit 6. Without it a missing toolchain surfaces as
# "docker-compose.yml names NO image reference", which sends the next person to read a file
# that is perfectly correct.
if [ -n "${RELEASE_SHAPE_GATE:-}" ]; then
  if [ ! -x "$RELEASE_SHAPE_GATE" ]; then
    echo "::error::resolve-compose-image: RELEASE_SHAPE_GATE is set to '$RELEASE_SHAPE_GATE', which is not an executable. That is the one reader of docker-compose.yml, and this script will not guess at the reference without it." >&2
    exit 6
  fi
elif ! command -v go >/dev/null 2>&1; then
  echo "::error::resolve-compose-image: no Go toolchain on PATH, and none was handed over in RELEASE_SHAPE_GATE." >&2
  echo "       docker-compose.yml has exactly one reader - scripts/release-shape-gate -print-compose-ref - and building it needs Go." >&2
  echo "       This is a missing toolchain in THIS job, not a problem with the compose file: set up Go in the job that runs" >&2
  echo "       this step, or build the reader earlier and pass it in RELEASE_SHAPE_GATE." >&2
  exit 6
fi

read_ref() {
  if [ -n "${RELEASE_SHAPE_GATE:-}" ]; then
    "$RELEASE_SHAPE_GATE" -root "$here" -print-compose-ref
  else
    ( cd "$here" && go run ./scripts/release-shape-gate -root . -print-compose-ref )
  fi
}

# The reader's own sentence goes straight to the log; this adds why it mattered.
if ! ref="$(read_ref)" || [ -z "$ref" ]; then
  echo "::error::resolve-compose-image: docker-compose.yml names NO image reference this script can resolve (see the line above). An absent or ambiguous reference is not agreement: a user copying that file would have nothing to pull, or would pull something this release never gated." >&2
  exit 3
fi

digest() {  # digest <ref> - prints the manifest digest, or fails
  docker buildx imagetools inspect "$1" --format '{{.Manifest.Digest}}' 2>&1
}

gated_ref="${image}:${version}"
if ! gated="$(digest "$gated_ref")" || [ -z "$gated" ]; then
  echo "::error::resolve-compose-image: the version this run just published does not resolve: $gated_ref" >&2
  printf '       %s\n' "${gated:-(no output)}" >&2
  exit 4
fi

# 1. The promotion is a registry operation. This is the only thing in the release that says
#    it landed on the artefact this run gated rather than on whatever was there before.
floating_ref="${image}:${floating}"
if ! moved="$(digest "$floating_ref")" || [ -z "$moved" ]; then
  echo "::error::resolve-compose-image: the floating reference this release just promoted does NOT RESOLVE: $floating_ref" >&2
  echo "       The promotion is the step that moves it, and it reported success. A reference that does not" >&2
  echo "       resolve after it moved means nothing is published there at all." >&2
  printf '       %s\n' "${moved:-(no output)}" >&2
  exit 4
fi

if [ "$moved" != "$gated" ]; then
  echo "::error::resolve-compose-image: the floating reference resolves to a DIFFERENT image than this release gated." >&2
  echo "       floating:   $floating_ref" >&2
  echo "                            -> $moved" >&2
  echo "       just gated: $gated_ref" >&2
  echo "                            -> $gated" >&2
  echo "       Every user who pulls that reference would get an artefact this run never smoked." >&2
  exit 5
fi

# 2. And the reference a stranger actually copies out of docker-compose.yml must be an image
#    that is THERE. It is pinned to a digest, so this is not a comparison - it is A5's own
#    assertion, "find an image at that exact reference", which only a registry can answer.
if ! found="$(digest "$ref")" || [ -z "$found" ]; then
  echo "::error::resolve-compose-image: the example deployment's image reference does NOT RESOLVE: $ref" >&2
  echo "       docker-compose.yml hands that reference to every user who copies it. The release published" >&2
  echo "       $gated_ref instead. If this repository was renamed, the published reference moved with it;" >&2
  echo "       if the package was deleted or the digest garbage-collected, the pin now names nothing." >&2
  printf '       %s\n' "${found:-(no output)}" >&2
  exit 4
fi

echo "resolve-compose-image: $floating_ref -> $moved (the digest this release gated, published as $gated_ref)"
echo "resolve-compose-image: $ref -> $found (the example deployment's own pinned reference resolves)"
