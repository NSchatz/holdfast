#!/usr/bin/env bash
# Runs shard <k> of <n> of a pull request's `go test` (pr.yml): the packages scripts/pr-scope.sh
# chose, with their top-level tests dealt round-robin by name across the shards, so the slow
# integration suites (cmd/holdfast, internal/server) run in parallel jobs.
#
# Fail-safe: shard k < n-1 runs `-run` of the names dealt to it; the LAST shard runs `-skip` of
# every name dealt to the others, so a test this name scan missed still runs there.
#
# Usage: scripts/pr-shard.sh <k> <n> <import-path>...
set -euo pipefail

k="$1"; n="$2"; shift 2
[ "$#" -gt 0 ] || { echo "pr-shard: no packages"; exit 0; }
cd "$(git rev-parse --show-toplevel)"
module="$(go list -m)"

names="$(for p in "$@"; do
  grep -hoE '^func (Test|Example|Fuzz)[A-Za-z0-9_]*\(' "${p#"$module"/}"/*_test.go 2>/dev/null || true
done | sed -E 's/^func //; s/\($//' | grep -vx 'TestMain' | sort -u)"

deal() { awk -v k="$k" -v n="$n" -v want="$1" '((NR - 1) % n == k) == want' <<<"$names" | paste -sd'|'; }

args=(-race -timeout 90m)
if [ "$k" -eq $((n - 1)) ]; then
  others="$(deal 0)"
  [ -n "$others" ] && args+=(-skip "^(${others})\$")
else
  mine="$(deal 1)"
  [ -n "$mine" ] || { echo "pr-shard $k/$n: no tests dealt to this shard"; exit 0; }
  args+=(-run "^(${mine})\$")
fi

echo "pr-shard $k/$n: go test ${args[*]:0:3} ... $*"
go test "${args[@]}" "$@"
