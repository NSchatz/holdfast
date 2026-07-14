#!/usr/bin/env bash
# Assert that every cross-file pin actually agrees. Part of `make check`.
#
# This branch was refuted four times for one failure: a value restated in several files
# and kept in step by a COMMENT. It always looks fine — each file is internally
# consistent, every checksum verifies, CI is green — while the thing the value describes
# has silently detached from the thing that was proven. Prose cannot enforce an
# invariant. So where a value genuinely MUST appear twice, the agreement is checked here
# and the drift is loud.
#
# Where a value need NOT appear twice, it does not: the ffmpeg pin lives in the
# Dockerfile's ARGs and scripts/install-ffmpeg.sh PARSES it. NOTICE is the exception that
# forces this script to exist — it must literally name the ffmpeg build it ships, because
# it is the GPL corresponding-source record that travels inside the image and inside
# every release tarball. It cannot point at a Dockerfile the user does not have.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail=0

note() { printf '  %s\n' "$*"; }
bad()  { printf '::error::%s\n' "$*" >&2; fail=1; }

arg() { sed -n "s/^ARG $1=\\(.*\\)$/\\1/p" "$here/Dockerfile" | head -1; }

# --- 1. NOTICE must name the exact ffmpeg the image bundles ------------------------
# It is the source offer for the GPL binaries the image redistributes. If it drifts, the
# image ships binaries whose licence record names a DIFFERENT upstream build.
df_build="$(arg FFMPEG_BUILD)"
df_version="$(arg FFMPEG_VERSION)"
[ -n "$df_build" ] && [ -n "$df_version" ] || bad "could not read FFMPEG_BUILD/FFMPEG_VERSION from Dockerfile"

no_build="$(sed -n 's/^ *Build tag *: *\(.*[^ ]\) *$/\1/p'  "$here/NOTICE" | head -1)"
no_version="$(sed -n 's/^ *Version *: *\(.*[^ ]\) *$/\1/p'  "$here/NOTICE" | head -1)"

if [ "$no_build" = "$df_build" ] && [ "$no_version" = "$df_version" ]; then
  note "ok: NOTICE names the ffmpeg the Dockerfile pins ($df_version, $df_build)"
else
  bad "NOTICE does not match the Dockerfile's ffmpeg pin — the image would redistribute GPL binaries whose source offer names a different build.
       Dockerfile: build=$df_build version=$df_version
       NOTICE:     build=$no_build version=$no_version"
fi

# --- 2. One Go version across the proof and the artifact ---------------------------
# The gate must run on the Go that builds the binary we ship. Nothing forces these three
# together but this check.
go_image="$(arg GO_IMAGE)"                       # golang:1.25.12-bookworm@sha256:...
docker_go="${go_image#golang:}"; docker_go="${docker_go%%-*}"
ci_go="$(sed -n 's/^ *GO_VERSION: *"\(.*\)"$/\1/p' "$here/.github/workflows/ci.yml" | head -1)"
rel_go="$(sed -n 's/^ *GO_VERSION: *"\(.*\)"$/\1/p' "$here/.github/workflows/release.yml" | head -1)"

if [ -n "$docker_go" ] && [ "$ci_go" = "$docker_go" ] && [ "$rel_go" = "$docker_go" ]; then
  note "ok: one Go version everywhere ($docker_go — Dockerfile, ci.yml, release.yml)"
else
  bad "Go version drift — the gate would run on a different Go than the shipped binary is built with.
       Dockerfile GO_IMAGE: $docker_go
       ci.yml GO_VERSION:   $ci_go
       release.yml:         $rel_go"
fi

[ "$fail" -eq 0 ] || exit 1
echo "pins agree"
