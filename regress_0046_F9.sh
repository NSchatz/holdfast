#!/usr/bin/env bash
# Reproduction runner for the S0046-holdfast-release-2 impl-gate ordinal 4
# findings (verdict-impl-4.md, F9 and F10). Read-only against the working tree;
# publishes nothing.
#
#   bash regress_0046_F9.sh
#
# F9/F9b/F9c/F9d are the hole in the `run:` half of the act catalogue: it knows
# exactly one destination, the literal flag `--push`. So
# `docker buildx build --output=type=registry` (the thing `push:` is documented
# shorthand FOR, and the spelling ordinal 2's F5 closed on the `uses:` half),
# `-o type=image,...,push=true`, and `docker image push` all perform no act at
# all. None of them involves a line continuation.
#
# F10 is the continuation normalisation's own two passes interacting:
# StripShellComments resets its quote state at every PHYSICAL line, so a `#` on
# a continuation line inside a string opened earlier is read as a comment,
# truncating the rest of the LOGICAL line - the `--push` and the backslash that
# would have joined it. F10s runs the identical script under a real `bash -e`
# with a recording stub named `docker` on PATH and shows the push happening;
# it PASSES, and it is what makes F10 a measurement rather than a reading.
#
# P0-P7 must PASS: the committed baseline, the same acts in the spelling the
# catalogue does know, the honest other direction (a local exporter is not a
# publish, a `--push` inside a comment is prose), and the continuation
# normalisation itself - including that the ORDERING properties read the joined
# text too. They are kept so this file is a measurement rather than a selection
# of only the cases that fail.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
exec go test -tags regress0046 -count=1 -run TestRegress0046L4 -v ./scripts/release-shape-gate/...
