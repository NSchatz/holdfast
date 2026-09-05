#!/usr/bin/env bash
# Run govulncheck and grade its report. Part of `make check`.
#
# This is `make check`'s vulnerability step, and it is deliberately STRICTER than the
# tool's own exit status: govulncheck exits non-zero only for advisories your code is
# proven to CALL, so an advisory in a module you merely import is reported and then
# ignored by the exit code. Here every advisory the report names must be answered - by
# taking the fix, or by a recorded entry in .govulncheck-suppressions.yaml.
#
# The pinned version is passed in from the Makefile, which owns it, and is rejected if it
# is not a concrete version: a tool pin that can float is not a pin, and `latest` would
# silently change what the gate is.
#
# The tool is run in JSON mode and graded by scripts/govulncheck-gate, so the report is
# read structurally rather than by scraping prose. The exit status is checked, not
# swallowed: an absent, empty or unparseable report is RED. A tool that did not run is
# not a tool that found nothing, and printing "ok" beside a crashed scanner is how a gate
# comes to be believed.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$here"

version="${1-}"
if [ -z "$version" ]; then
  echo "::error::govulncheck: no version given. Call this as \`scripts/govulncheck.sh \$(GOVULNCHECK_VERSION)\`; the Makefile owns the pin." >&2
  exit 1
fi
if ! printf '%s' "$version" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$'; then
  echo "::error::govulncheck: version '$version' is not a concrete pin. A floating tag (latest, a branch, a bare major) changes what the gate is without changing a line of it." >&2
  exit 1
fi

work="$(mktemp -d)" || { echo "::error::govulncheck: mktemp failed" >&2; exit 1; }
trap 'rm -rf "$work"' EXIT
report="$work/report.json"
toolerr="$work/govulncheck.err"

rc=0
go run "golang.org/x/vuln/cmd/govulncheck@$version" -format json ./... >"$report" 2>"$toolerr" || rc=$?

# In JSON mode govulncheck reports its findings in the stream and exits 0; a non-zero
# status therefore means the TOOL failed - it could not be downloaded or built, the module
# graph would not load, the database was unreachable. None of those are a clean scan.
if [ "$rc" -ne 0 ]; then
  echo "::error::govulncheck did NOT run (exit $rc). Refusing to treat an absent report as a clean one." >&2
  sed 's/^/       /' "$toolerr" >&2 || true
  exit 1
fi
if [ ! -s "$report" ]; then
  echo "::error::govulncheck exited 0 but wrote an empty report - it did not scan anything. Refusing to report green." >&2
  sed 's/^/       /' "$toolerr" >&2 || true
  exit 1
fi

# Anything govulncheck put on stderr while still succeeding is worth seeing, but it is
# not a verdict - the report is.
if [ -s "$toolerr" ]; then
  sed 's/^/  govulncheck: /' "$toolerr" >&2 || true
fi

go run ./scripts/govulncheck-gate <"$report"
