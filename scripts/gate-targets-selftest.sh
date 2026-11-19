#!/usr/bin/env bash
# Proves the standard gate names still mean what they say: `make gate-full` is `check` and
# `make gate-quick` is tier-fast, and ci.yml's gate step runs gate-full and no `make check`.
# A dry run only (`make -n`): nothing here builds, tests or encodes. pr.yml runs it in its
# changed job; a Makefile or workflow change makes scripts/pr-scope.sh run everything.
set -euo pipefail

here="$(git rev-parse --show-toplevel)"
fail=0

ok()  { echo "  ok: $1"; }
bad() { echo "::error::gate-targets selftest: $1"; fail=1; }

# The first command a dry run prints is the alias's own recipe line.
first() { make -C "$here" -n --no-print-directory "$1" 2>/dev/null | head -1 || true; }

test_gate_full_is_check() {
  local got; got="$(first gate-full)"
  if grep -qE '(^|/)make --no-print-directory check$' <<<"$got"; then ok "gate-full runs check"
  else bad "gate-full does not run check (dry run's first line: '$got')"; fi
}

test_gate_quick_is_tier_fast() {
  local got; got="$(first gate-quick)"
  if grep -qE '(^|/)make --no-print-directory tier-fast$' <<<"$got"; then ok "gate-quick runs tier-fast"
  else bad "gate-quick does not run tier-fast (dry run's first line: '$got')"; fi
}

test_ci_yml_calls_gate_full() {
  # The build job's lines: from its name line to the next job's, comments skipped.
  local build
  build="$(awk '
    /^[[:space:]]*#/ { next }
    /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { in_build = ($1 == "build:") }
    in_build
  ' "$here/.github/workflows/ci.yml")"
  if grep -qE '^[[:space:]]*run:[[:space:]]*make gate-full[[:space:]]*$' <<<"$build"; then
    ok "ci.yml's build job runs make gate-full"
  else bad "ci.yml's build job has no \`run: make gate-full\` step"; fi
  if grep -qE 'make check([[:space:]]|$)' <<<"$build"; then
    bad "ci.yml's build job still runs \`make check\`"
  else ok "ci.yml's build job runs no make check step"; fi
}

test_gate_full_is_check
test_gate_quick_is_tier_fast
test_ci_yml_calls_gate_full

[ "$fail" -eq 0 ] && echo "gate-targets selftest: OK" || exit 1
