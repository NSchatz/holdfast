#!/usr/bin/env bash
# Proves scripts/pr-scope.sh still selects what a pull request must run. Its one silent failure
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

for f in Makefile .github/workflows/ci.yml go.mod scripts/pr-scope.sh; do
  out="$(plan "$f")"
  want "$f runs every package"                   "$out" "^pkgs=.*$m/internal/vmaf( |$)"
  want "$f runs every check"                     "$out" "^check_pins_selftest=true$"
done

out="$("$here/scripts/pr-scope.sh" refs/does/not/exist 2>/dev/null)"
want "a base it cannot resolve runs everything"  "$out" "^mutation_selftest=true$"

[ "$fail" -eq 0 ] && echo "pr-scope selftest: OK" || exit 1
