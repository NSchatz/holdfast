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
#
# THE RETAG ALONE DECIDES THIS SCRIPT'S EXIT STATUS. The inspect below gates nothing: it
# decides no property of the artefact, it is the only human-readable record in the release log
# of what the floating reference now carries, and that is the whole of its job. So its output
# goes STRAIGHT to this step's stdout, never through a reader that can close the pipe on it -
# a closed pipe kills the inspect with SIGPIPE, and `pipefail` would then hand the step that
# status over a retag that landed, failing a release whose promotion worked and skipping the
# two steps after it, which are the real gates on the moved reference.
#
# An inspect that cannot answer is therefore SAID OUT LOUD and is not a verdict. Whether the
# floating reference resolves to the digest this run gated is decided against the registry by
# scripts/resolve-compose-image.sh, in the step after this one, which exits 4 if that reference
# does not resolve and 5 if it resolves to a different digest.
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

if ! docker buildx imagetools inspect "${image}:${floating}"; then
  echo "::warning::release-promote: the retag landed, but the manifest of ${image}:${floating} could not be inspected. Whether that reference resolves to the gated digest is left to the step that resolves it against the registry." >&2
fi
echo "release-promote: ${image}:${floating} now resolves to the digest published as ${image}:${version}"
