#!/usr/bin/env bash
# THE one invocation of the mutation gate (testing T5). `make mutation-diff`,
# `make mutation-full`, the workflow and scripts/mutation-selftest.sh all come through
# here, so there is one definition of what a mutation run is and one place its exit codes
# come from.
#
# It exists for the same reason scripts/secret-scan.sh does: `go run` DOES NOT PROPAGATE
# AN EXIT CODE. A program that exits 3 makes `go run` print "exit status 3" and exit 1,
# which would collapse "the suite is not asserting enough" (3) into "the gate could not
# run at all" (4) - and those two must never be the same answer from a gate whose subject
# is whether the tests assert anything. So the gate is BUILT and then EXECUTED, and its
# status is this script's status.
#
# Exit codes (mutation-gate --help prints them too):
#   0  at or above the floor, or nothing was in scope
#   2  bad invocation
#   3  BELOW the floor
#   4  the pinned runner could not be obtained or could not execute
#   5  the runner ran but its report cannot be believed
#   6  .gremlins.yaml could not be read
#   7  the diff reference could not be resolved to a merge base
#   9  the domain's own suite is red, so a mutation score would be meaningless
#
# The floor and the mutation domain are NOT arguments here. They live in .gremlins.yaml,
# the gate reads them from there, and docs/mutation-testing.md is held equal to that file
# by `make mutation-shape`. A threshold passed on a command line is a threshold that can
# differ between the pull request, the schedule and the person reproducing it.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$here"

version=""
mode=""
ref=""
out=""

die_usage() {
  echo "::error::mutation: $1" >&2
  echo "usage: scripts/mutation.sh --version <vX.Y.Z> --mode diff|full [--ref <git ref>] --out <report.json>" >&2
  exit 2
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) version="${2-}"; shift 2 || die_usage "--version needs a value" ;;
    --mode)    mode="${2-}";    shift 2 || die_usage "--mode needs a value" ;;
    --ref)     ref="${2-}";     shift 2 || die_usage "--ref needs a value" ;;
    --out)     out="${2-}";     shift 2 || die_usage "--out needs a value" ;;
    *) die_usage "unknown argument '$1'" ;;
  esac
done

[ -n "$version" ] || die_usage "no runner version given. Call this as \`scripts/mutation.sh --version \$(GREMLINS_VERSION) ...\`; the Makefile owns the pin."
[ -n "$mode" ]    || die_usage "no --mode given (diff or full)"
[ -n "$out" ]     || die_usage "no --out given"

# A tool pin that can float is not a pin: `latest`, a branch or a bare major would change
# what this gate IS without changing a line of it (pinning P6).
if ! printf '%s' "$version" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$'; then
  echo "::error::mutation: runner version '$version' is not a concrete pin. A floating tag changes what the gate is without changing a line of it." >&2
  exit 2
fi

case "$mode" in
  diff)
    [ -n "$ref" ] || die_usage "--mode diff needs the --ref it scopes against (in a pull request, the base branch)"
    ;;
  full)
    ;;
  *) die_usage "--mode must be 'diff' or 'full'" ;;
esac

bin="$(mktemp "${TMPDIR:-/tmp}/holdfast-mutation-gate.XXXXXX")" || {
  echo "::error::mutation: could not create a temporary file for the gate binary" >&2
  exit 4
}
trap 'rm -f "$bin"' EXIT

if ! go build -o "$bin" ./scripts/mutation-gate; then
  echo "::error::mutation: the mutation gate did not build. Nothing has been measured, and no score is being reported in its place." >&2
  exit 4
fi

# The tree has to COMPILE before anything is mutated. A mutant is judged KILLED when the
# package's test command fails, and a tree that does not build fails every one of them -
# so a broken build reads as a perfect score. That is the single cheapest way this gate
# can go silently green, and it is refused here rather than measured.
if ! go build ./... >/dev/null; then
  echo "::error::mutation: THE MODULE DOES NOT BUILD, so no mutation score can be measured." >&2
  echo "       Every mutant is judged by whether the package's tests fail, and they fail for everyone on a tree that does not compile: the run would report a perfect score it never measured." >&2
  echo "       next: STOPPING. Fix the build and run this again." >&2
  exit 4
fi

args=(run --root "$here" --mode "$mode" --runner-version "$version" --out "$out")
if [ "$mode" = "diff" ]; then
  args+=(--ref "$ref")
fi

# NOT `exec`: exec replaces this shell and the EXIT trap never runs, which would leak the
# built binary into TMPDIR on every run. Run it, keep its status, exit with it.
status=0
"$bin" "${args[@]}" || status=$?
exit "$status"
