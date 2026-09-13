#!/usr/bin/env bash
# THE one invocation of the secret scanner (secrets K4). `make secret-scan`, the
# pre-commit hook and scripts/secret-scan-selftest.sh all come through here, so there is
# one definition of what the scan is and one place its exit codes come from.
#
# It exists because `go run` DOES NOT PROPAGATE AN EXIT CODE: a program that exits 3 makes
# `go run` print "exit status 3" and exit 1, which would collapse "found a credential"
# (3) and "could not run" (4) into the same answer and destroy the distinction the scanner
# exists to make. So the binary is BUILT and then EXECUTED, and its status is this
# script's status.
#
# Exit codes (the binary's own -h prints them too):
#   0  clean        3  FOUND a credential        4  COULD NOT RUN        2  bad invocation
#
# A build failure is exit 4, not 1: a scanner that could not be built has not cleared
# anything, and that is exactly what 4 means.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$here"

bin="$(mktemp "${TMPDIR:-/tmp}/holdfast-secret-scan.XXXXXX")" || {
  echo "::error::secret-scan: could not create a temporary file for the scanner binary" >&2
  exit 4
}
trap 'rm -f "$bin"' EXIT

if ! go build -o "$bin" ./scripts/secret-scan; then
  echo "::error::secret-scan COULD NOT RUN: the scanner did not build. Nothing has been cleared." >&2
  exit 4
fi

# --root defaults to the repository this script lives in, so the hook and the gate agree
# about what "the tree" is however they were invoked. An explicit --root in "$@" still
# wins: Go's flag package takes the LAST occurrence of a flag.
#
# NOT `exec`: exec replaces this shell and the EXIT trap above never runs, which would
# leak a 10MB binary into TMPDIR on every commit. Run it, keep its status, exit with it.
status=0
"$bin" --root "$here" "$@" || status=$?
exit "$status"
