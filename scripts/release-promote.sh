#!/usr/bin/env bash
# Promote the floating reference onto the digest this release just gated.
#
# `imagetools create` RETAGS the existing manifest; it does not rebuild, so the floating
# reference and the version reference are the same bytes rather than two builds that ought
# to agree. This runs last on purpose: push `:latest` first and a failing gate has already
# handed every `docker compose pull` user an image it went on to reject.
#
# THE FLOATING TAG IS NOT WRITTEN HERE. It arrives as $FLOATING_TAG, declared in
# release.yml's promotion step, because that is the value `make check` compares against
# docker-compose.yml - and a value this script also spelled would be a second copy held in
# step by hope, which is the shape this repository refuses for the ffmpeg pin.
#
# It lives in a file rather than inline in release.yml so the release-shape gate can
# identify this step by the script it invokes, matched WHOLE. See release-resmoke.sh.
#
# Failure modes are distinct and each exits with its own code:
#
#   2  IMAGE/VERSION/FLOATING_TAG not supplied - the caller is wrong, not the registry
#   3  the retag did not take
set -euo pipefail

image="${IMAGE:-}"
version="${VERSION:-}"
floating="${FLOATING_TAG:-}"

if [ -z "$image" ] || [ -z "$version" ] || [ -z "$floating" ]; then
  echo "::error::release-promote: IMAGE, VERSION and FLOATING_TAG must all be set. Refusing to guess which reference a release moves." >&2
  exit 2
fi

if ! docker buildx imagetools create -t "${image}:${floating}" "${image}:${version}"; then
  echo "::error::release-promote: could not retag ${image}:${version} as ${image}:${floating}. The floating reference is left exactly where it was." >&2
  exit 3
fi

docker buildx imagetools inspect "${image}:${floating}" | head -20
echo "release-promote: ${image}:${floating} now resolves to the digest published as ${image}:${version}"
