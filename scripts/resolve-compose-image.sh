#!/usr/bin/env bash
# Resolve the EXACT image reference the example deployment names, against the registry,
# and prove it is the digest this release just gated. Run by release.yml after `:latest`
# is promoted.
#
# Everything else in the release works with `${IMAGE}:${VERSION}`, which the workflow
# derives from `github.repository`, so it is correct by construction and proves nothing
# about docker-compose.yml, which is the reference a stranger actually pulls. `make check`
# holds the two in agreement offline; this is the half that agreement cannot cover: that
# the reference RESOLVES, in the registry, to the digest that passed the gate. A release
# that leaves the published compose file pointing at something that does not exist is
# otherwise discovered only by the first person who tries to run it.
#
# Failure modes are distinct, named, and each exits with its own code:
#
#   2  IMAGE/VERSION not supplied - the caller is wrong, not the registry
#   3  docker-compose.yml carries no image reference this script can read
#   4  a reference does not resolve at all
#   5  the compose reference resolves to a DIFFERENT digest than the gated version
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
compose="$here/docker-compose.yml"

image="${IMAGE:-${1-}}"
version="${VERSION:-${2-}}"
if [ -z "$image" ] || [ -z "$version" ]; then
  echo "::error::resolve-compose-image: no IMAGE/VERSION given. Call it with the image and version the release just published." >&2
  exit 2
fi

if [ ! -r "$compose" ]; then
  echo "::error::resolve-compose-image: cannot read docker-compose.yml. The example deployment's image reference is unknown, so it cannot be resolved." >&2
  exit 3
fi

# The first real `image:` key (a commented-out one does not start with whitespace-then-i).
ref="$(sed -n 's/^[[:space:]]*image:[[:space:]]*\([^[:space:]#]*\).*$/\1/p' "$compose" | head -1)"
ref="${ref%\"}"; ref="${ref#\"}"
ref="${ref%\'}"; ref="${ref#\'}"
if [ -z "$ref" ]; then
  echo "::error::resolve-compose-image: docker-compose.yml names NO image reference. An absent reference is not agreement: a user copying that file would have nothing to pull." >&2
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

if ! found="$(digest "$ref")" || [ -z "$found" ]; then
  echo "::error::resolve-compose-image: the example deployment's image reference does NOT RESOLVE: $ref" >&2
  echo "       docker-compose.yml hands that reference to every user who copies it. The release published" >&2
  echo "       $gated_ref instead. If this repository was renamed, the published reference moved with it." >&2
  printf '       %s\n' "${found:-(no output)}" >&2
  exit 4
fi

if [ "$found" != "$gated" ]; then
  echo "::error::resolve-compose-image: the example deployment's image reference resolves to a DIFFERENT image than this release gated." >&2
  echo "       docker-compose.yml: $ref" >&2
  echo "                            -> $found" >&2
  echo "       just gated:         $gated_ref" >&2
  echo "                            -> $gated" >&2
  exit 5
fi

echo "resolve-compose-image: $ref -> $found (the digest this release gated, published as $gated_ref)"
