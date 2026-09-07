#!/usr/bin/env bash
# Reproduction runner for the S0046-holdfast-release-2 impl-gate ordinal 3
# finding (verdict-impl-3.md, F7). Read-only against the working tree;
# publishes nothing.
#
#   bash regress_0046_F7.sh
#
# F7/F7b/F7c are the hole: a `docker buildx build \` with `--push` on the next
# line, and a `gh release \` with `create` on the next line, were classified as
# performing no published act at all, because the `run:` detectors matched
# regular expressions against PHYSICAL lines. They FAILED before the fix and
# must PASS after it.
#
# P0-P8 must PASS either way: they are the same acts written on one line (which
# is what makes F7 a hole in the spelling rather than a missing detector), the
# honest other direction (a continued build with no --push is still local), and
# ordinal 2's `outputs:` finding re-checked in three shapes. They are kept so
# this file is a measurement rather than a selection of only the cases that
# fail.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
exec go test -tags regress0046 -count=1 -run TestRegress0046L3 -v ./scripts/release-shape-gate/...
