#!/usr/bin/env bash
# Proves scripts/pr-scope.sh and scripts/pr-shard.sh still select what a pull request must run. Its one silent failure
# is selecting too LITTLE - a PR green because the package it broke never ran - so each case
# names a change and something its plan must include (or, for internal/engine, must not).
set -euo pipefail

here="$(git rev-parse --show-toplevel)"
m="$(cd "$here" && go list -m)"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
fail=0

plan() { printf '%s\n' "$@" > "$tmp/files"; "$here/scripts/pr-scope.sh" --files "$tmp/files"; }
want() { # description, plan output, extended regex the output must match
  if printf '%s\n' "$2" | grep -qE "$3"; then echo "  ok: $1"
  else echo "::error::pr-scope selftest: $1 (no match for /$3/)"; fail=1; fi
}
deny() {
  if printf '%s\n' "$2" | grep -qE "$3"; then echo "::error::pr-scope selftest: $1 (matched /$3/)"; fail=1
  else echo "  ok: $1"; fi
}

out="$(plan internal/queuekey/key.go)"
want "a changed package is tested"               "$out" "^pkgs=.*$m/internal/queuekey( |$)"
want "a package that imports it is tested"       "$out" "^pkgs=.*$m/internal/server( |$)"
deny "an unrelated package is not tested"        "$out" "^pkgs=.*$m/internal/vmaf( |$)"
deny "internal/engine's encodes never run on a PR" "$out" "^pkgs=.*$m/internal/engine( |$)"
want "internal/engine is still vetted"           "$out" "^vet=.*$m/internal/engine( |$)"

out="$(plan internal/engine/engine.go)"
deny "a change to internal/engine runs no encodes" "$out" "^pkgs=.*$m/internal/engine( |$)"
want "its dependents are tested"                 "$out" "^pkgs=.*$m/cmd/holdfast( |$)"

out="$(plan docs/design/swap.md)"
want "a Markdown change runs the document checks" "$out" "^pkgs=.*$m/internal/docscheck( |$)"

out="$(plan web/package.json)"
want "a web/ change runs the UI steps"           "$out" "^ui=true$"
want "a web/ change tests what embeds the UI"    "$out" "^pkgs=.*$m/cmd/holdfast( |$)"

# A move out of a package must still test the package it left: a real two-commit diff.
git -C "$here" worktree add -q --detach "$tmp/wt" HEAD
( cd "$tmp/wt" && mkdir -p internal/zzmoved && git mv internal/queuekey/queuekey_test.go internal/zzmoved/queuekey_test.go \
    && git -c user.name=selftest -c user.email=selftest@example.invalid commit -qm move )
out="$(cd "$tmp/wt" && "$here/scripts/pr-scope.sh" HEAD~1 2>/dev/null || true)"
git -C "$here" worktree remove --force "$tmp/wt"
want "a file moved out of a package tests the package it left" "$out" "^pkgs=.*$m/internal/queuekey( |$)"

for f in Makefile .github/workflows/ci.yml go.mod scripts/pr-scope.sh; do
  out="$(plan "$f")"
  want "$f runs every package"                   "$out" "^pkgs=.*$m/internal/vmaf( |$)"
  want "$f runs every check"                     "$out" "^check_pins_selftest=true$"
done

out="$("$here/scripts/pr-scope.sh" refs/does/not/exist 2>/dev/null)"
want "a base it cannot resolve runs everything"  "$out" "^mutation_selftest=true$"

# scripts/pr-shard.sh: the shards together run every top-level test exactly once.
q="$m/internal/queuekey"
count() { grep -c '^=== RUN   [^/]*$' || true; }
whole="$(cd "$here" && go test -v -count=1 "$q" 2>&1 | count)"
split=0
for k in 0 1 2; do split=$((split + $(GOFLAGS=-v "$here/scripts/pr-shard.sh" "$k" 3 "$q" 2>&1 | count))); done
if [ "$whole" -gt 0 ] && [ "$split" -eq "$whole" ]; then echo "  ok: three shards run the $whole tests one run does"
else echo "::error::pr-scope selftest: shards ran $split top-level tests, one run $whole"; fail=1; fi

[ "$fail" -eq 0 ] && echo "pr-scope selftest: OK" || exit 1
