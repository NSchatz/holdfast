#!/usr/bin/env bash
# Reproduction runner for the S0046-holdfast-release-2 impl-gate ordinal 5
# findings (verdict-impl-5.md, F11, F12 and F13). Read-only against the working
# tree; publishes nothing.
#
#   bash regress_0046_F11.sh
#
# F11/F11b are a publishing command the shell runs out of a QUOTED word:
# `sh -c "docker push …"` and `eval "docker push …"`. The new single reader
# lexes the quoted span into one argv element, Command.invocation then reports
# the program as `sh`/the quoted string, and Command.strayRegistryTool - the
# backstop for exactly this shape - skips every word containing whitespace. The
# invocation is neither decided nor reported, so the gate prints "NONE of them
# publishes anything" over a dry run that pushes. F11s is the ground truth: the
# identical lines under a real `bash -e` with a recording stub named `docker`
# first on PATH, which records both pushes. This is also a REGRESSION - the
# regex catalogue this pass deleted matched raw text, quotes included.
#
# F12 is buildx's attached short-flag spelling of --output. `-otype=registry`
# IS `--output=type=registry` (pflag takes an attached shorthand value), and
# decideBuildDestination reads only `-o`, `-o=`, `--output` and `--output=`, so
# the spec falls through `default: continue` and the build is reported as
# having no destination flag at all. Measured against a real buildx:
#
#   printf 'FROM scratch\n' | docker buildx build \
#     -otype=registry,name=localhost:1/holdfast-probe:dev -f - <ctx>
#   => #3 naming to localhost:1/holdfast-probe:dev done
#      #3 pushing layers done
#      #3 ERROR: failed to push localhost:1/holdfast-probe:dev: … dial tcp [::1]:1
#
# F13 is the same quote resolution reaching the ORDERING half. Step.script() now
# renders the command list with quoting removed, so `echo "make check"` reads as
# `make check` and a step that only PRINTS the gate's name satisfies A7's "the
# full gate appears before the promotion". F13r measures both sides of that with
# the committed regex.
#
# P0-P5 must PASS: the committed baseline, the same acts in the spellings the
# model does decide, the honest other direction (a local exporter is not a
# publish, the real `make check` still reads as one) and the EDGE the fifth pass
# built, which still reds an unmodelled `crane copy` by name. They are kept so
# this file is a measurement rather than a selection of only the cases that fail.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
exec go test -tags regress0046 -count=1 -run TestRegress0046L5 -v ./scripts/release-shape-gate/...
