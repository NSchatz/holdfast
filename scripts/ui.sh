#!/usr/bin/env bash
# Run one step of the web UI's gate on the PINNED toolchain, or refuse.
#
#   scripts/ui.sh toolchain   name the Node and pnpm this would run, and stop
#   scripts/ui.sh install     pnpm install --frozen-lockfile
#   scripts/ui.sh lint        eslint, no warning allowed
#   scripts/ui.sh typecheck   svelte-check, a warning fails
#   scripts/ui.sh test        vitest, once, no watch
#   scripts/ui.sh build       vite build into web/dist, copied into internal/ui/dist
#
# The Makefile's ui-* targets are this script and nothing else, so `make check`, CI and
# the release run the same commands on the same versions.
#
# The versions live in ONE place each and are read from it here, never restated:
#   Node  web/.node-version
#   pnpm  the "packageManager" field of web/package.json
# and the versions of everything else are the exact ones in web/package.json, resolved
# by the committed web/pnpm-lock.yaml (scripts/check-pins.sh section 12 holds all of it).
#
# A step runs only on exactly those two versions. Where the `node` and `pnpm` on PATH
# are not them, the script asks for them by version - corepack for pnpm (it reads the
# packageManager field and checks the package it downloads against that hash; pnpm 12's
# package is a launcher that fetches the native pnpm of the same version and checks it
# against npm's registry signatures), then mise for both -
# and where none of that yields the pinned pair it REFUSES, naming what it found. A UI
# gate that ran on whatever Node was lying around would be a gate about that Node.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
web="$here/web"
embed="$here/internal/ui/dist"

die() { printf '::error::ui: %s\n' "$*" >&2; exit 1; }

step="${1:-}"
case "$step" in
  toolchain|install|lint|typecheck|test|build) ;;
  *) die "usage: scripts/ui.sh toolchain|install|lint|typecheck|test|build (got '${step}')" ;;
esac

[ -r "$web/.node-version" ] || die "web/.node-version is missing or unreadable: it is the Node pin, and a step that cannot read its pin does not run"
[ -r "$web/package.json" ]  || die "web/package.json is missing or unreadable: it carries the pnpm pin, and a step that cannot read its pin does not run"

node_pin="$(tr -d '[:space:]' < "$web/.node-version")"
printf '%s' "$node_pin" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$' \
  || die "web/.node-version holds '${node_pin}', which is not an exact MAJOR.MINOR.PATCH version"

pnpm_pin="$(sed -n 's/^[[:space:]]*"packageManager":[[:space:]]*"pnpm@\([0-9][0-9.]*\)+sha512\.[0-9a-f]\{128\}",\{0,1\}[[:space:]]*$/\1/p' "$web/package.json")"
printf '%s' "$pnpm_pin" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$' \
  || die "web/package.json has no \"packageManager\": \"pnpm@<exact version>+sha512.<128 hex>\" line this script can read"

# The command prefix that runs pnpm on the pinned pair, as an array, found by trying each
# route in turn and CHECKING what it actually runs.
runner=()
tried=""
versions_of() { # prints "<node version> <pnpm version>" for a command prefix, or nothing
  local nv pv
  nv="$( cd "$web" && "$@" node --version 2>/dev/null )" || return 0
  pv="$( cd "$web" && "$@" pnpm --version 2>/dev/null )" || return 0
  printf '%s %s' "${nv#v}" "$pv"
}
try() { # try <label> <command prefix...>
  local label="$1"; shift
  local got
  got="$(versions_of "$@")"
  if [ "$got" = "$node_pin $pnpm_pin" ]; then
    runner=("$@")
    return 0
  fi
  tried="${tried}
         ${label}: ${got:-not available}"
  return 1
}

export COREPACK_ENABLE_DOWNLOAD_PROMPT=0
# Where corepack's pnpm shim goes when that route is tried: a directory of this run's own,
# removed when the script exits.
shims=""
trap '[ -z "$shims" ] || rm -rf "$shims"' EXIT
corepack_path() {
  shims="$(mktemp -d)"
  corepack enable --install-directory "$shims" pnpm >/dev/null 2>&1 || true
  printf '%s:%s' "$shims" "$PATH"
}
if try "node and pnpm on PATH" env; then :
elif command -v corepack >/dev/null 2>&1 && cp_path="$(corepack_path)" && shims="${cp_path%%:*}" \
     && try "node on PATH with corepack's pnpm" env PATH="$cp_path"; then :
elif command -v mise >/dev/null 2>&1 && try "mise exec node@${node_pin} pnpm@${pnpm_pin}" mise exec "node@${node_pin}" "pnpm@${pnpm_pin}" --; then :
else
  die "the pinned UI toolchain is not available: Node ${node_pin} (web/.node-version) with pnpm ${pnpm_pin} (web/package.json packageManager).
       Tried, as '<node> <pnpm>':${tried}
       Install that Node and run \`corepack enable\`, or install mise. A UI gate that could
       not run on its pinned toolchain has not passed."
fi

pnpm() { ( cd "$web" && CI=true "${runner[@]}" pnpm "$@" ); }

case "$step" in
  toolchain)
    echo "ui: Node ${node_pin} and pnpm ${pnpm_pin}, as pinned (${runner[*]} pnpm)"
    ;;
  install)
    # --frozen-lockfile: what is installed is what the lockfile says, or the step fails.
    # It never writes the lockfile, so a manifest and a lockfile that disagree are a red
    # gate rather than a silently refreshed dependency set.
    pnpm install --frozen-lockfile
    ;;
  lint)      pnpm run lint ;;
  typecheck) pnpm run typecheck ;;
  test)      pnpm run test ;;
  build)
    pnpm run build
    [ -f "$web/dist/index.html" ] || die "vite build left no web/dist/index.html"
    [ -f "$embed/.gitkeep" ] || die "internal/ui/dist/.gitkeep is missing: it is the committed placeholder go:embed needs on a tree with no UI built, and this step must not be what removes it"
    # Vite builds into web/dist and the result is COPIED here. Pointed straight at the
    # embed directory, its emptyOutDir would delete the placeholder. Everything but the
    # placeholder is replaced, so a file an earlier build left cannot outlive it.
    find "$embed" -mindepth 1 -maxdepth 1 ! -name .gitkeep -exec rm -rf {} +
    cp -R "$web/dist/." "$embed/"
    [ -f "$embed/index.html" ] || die "the built UI did not reach internal/ui/dist"
    [ -f "$embed/.gitkeep" ]   || die "copying the built UI removed internal/ui/dist/.gitkeep"
    echo "ui: built with Node ${node_pin}, pnpm ${pnpm_pin}; copied into internal/ui/dist ($(find "$embed" -type f ! -name .gitkeep | wc -l | tr -d ' ') files) for go:embed"
    ;;
esac
