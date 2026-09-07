#!/usr/bin/env bash
# Reproduction runner for the S0046-holdfast-release-2 impl-gate findings
# (verdict-impl-1.md). Read-only against the working tree; publishes nothing.
#
#   bash regress_0046_F1.sh
#
# F1/F1b must FAIL: they are the hole. F2-F6 must PASS: they are the mutations
# the gate does catch, kept so the file is a measurement rather than a
# selection of only the cases that fail.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
exec go test -tags regress0046 -count=1 -run TestRegress0046 -v ./scripts/release-shape-gate/...
