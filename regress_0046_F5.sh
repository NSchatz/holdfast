#!/usr/bin/env bash
# Reproduction runner for the S0046-holdfast-release-2 impl-gate ordinal 2
# finding (verdict-impl-2.md). Read-only against the working tree; publishes
# nothing.
#
#   bash regress_0046_F5.sh
#
# F5/F5b/F5c must FAIL: they are the hole - a `docker/build-push-action` step
# that publishes through its `outputs:` input rather than its `push:` input is
# classified as performing no published act, so the gate prints "NONE of them
# publishes anything" over a dry run that pushes to GHCR.
#
# F5d and P0-P7 must PASS: F5d is the incidental backstop that catches the same
# respelling on the REAL push step (for a downstream reason, after the act has
# already vanished from the A8 runbook inventory), and P0-P7 are the shapes the
# gate does decide - kept so this file is a measurement rather than a selection
# of only the cases that fail.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
exec go test -tags regress0046 -count=1 -run TestRegress0046L2 -v ./scripts/release-shape-gate/...
