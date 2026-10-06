#!/usr/bin/env bash
# What a pull request's CI runs: the packages the change touches and the packages that depend
# on them, and each check or selftest whose inputs changed. The full gate (`make check`) runs
# nightly on main (ci.yml); this decides only what a PR waits for.
#
# Usage: scripts/pr-scope.sh <base-ref>      (changed files = git diff <merge-base>...HEAD)
#        scripts/pr-scope.sh --files <file>  (changed files read from <file>, one per line)
#
# Prints key=value lines for $GITHUB_OUTPUT:
#   pkgs     the import paths `go test` runs (space-separated; empty runs none)
#   vet      the import paths vet and staticcheck run (pkgs plus internal/engine when touched)
#   <check>  true|false for each conditional check below
#
# Fail-safe: when in doubt it runs MORE. A diff it cannot compute, or a change to the
# Makefile, a workflow or this script, runs every package and every check. internal/engine is
# the one exception: its suite is the real-encode proof (~2000 s of a full run), so a PR only
# vets and staticchecks it, and the nightly full gate runs it.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
module="$(go list -m)"
engine="$module/internal/engine"

changed=""
all=0
if [ "${1:-}" = "--files" ]; then
  changed="$(cat "$2")"
elif base="$(git merge-base "${1:?usage: pr-scope.sh <base-ref> | --files <file>}" HEAD 2>/dev/null)"; then
  changed="$(git diff --name-only "$base" HEAD)"
else
  echo "pr-scope: no merge base with $1; running everything" >&2
  all=1
fi

has() { printf '%s\n' "$changed" | grep -qE "$1"; }

if has '^(Makefile|\.github/workflows/.*|scripts/pr-scope(-selftest)?\.sh|go\.(mod|sum))$'; then
  all=1
fi

# Every package directory, relative, and each package with its test-inclusive deps.
mapfile -t dirs < <(go list -f '{{.Dir}}' ./... | sed "s|^$PWD/||")
deps="$(go list -test -f '{{.ImportPath}}|{{join .Deps " "}}' ./...)"

declare -A touched=()  # their dependents are affected too
declare -A readers=()  # only their own tests read the changed file
if [ "$all" -eq 1 ]; then
  for d in "${dirs[@]}"; do touched["$module/$d"]=1; done
else
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    # A file inside a package directory (its code, its testdata) touches that package.
    best=""
    for d in "${dirs[@]}"; do
      case "$f" in "$d"/*) [ "${#d}" -gt "${#best}" ] && best="$d" ;; esac
    done
    if [ -n "$best" ]; then touched["$module/$best"]=1; continue; fi
    # Any other file (docs, root testdata, a script) is read by the packages whose test
    # files name it or its directory; a top-level directory alone (docs/, scripts/) is too
    # broad to name a reader. Every Markdown file is read by the packages that import
    # internal/corpus, which is where the document checks enumerate them from. No code
    # changed, so a reader's dependents are not affected.
    names=(-e "$(basename "$f")")
    case "$(dirname "$f")" in */*) names+=(-e "$(dirname "$f")/") ;; esac
    for d in "${dirs[@]}"; do
      grep -lqF "${names[@]}" "$d"/*_test.go 2>/dev/null && readers["$module/$d"]=1
    done
    case "$f" in
      *.md)
        while IFS='|' read -r p ds; do
          case " $ds " in *" $module/internal/corpus "*) p="${p%% *}"; readers["${p%.test}"]=1 ;; esac
        done <<<"$deps"
        readers["$module/internal/corpus"]=1
        ;;
    esac
  done <<<"$changed"
fi

# Close over reverse dependencies, test imports included.
declare -A affected=()
for r in "${!readers[@]}"; do affected["$r"]=1; done
while IFS='|' read -r p ds; do
  p="${p%% *}"; p="${p%.test}"
  [ -n "${touched[$p]:-}" ] && { affected["$p"]=1; continue; }
  for t in "${!touched[@]}"; do
    case " $ds " in *" $t "*) affected["$p"]=1; break ;; esac
  done
done <<<"$deps"

# Only real packages (go list -test also lists the generated .test mains).
mapfile -t vet < <(for p in "${!affected[@]}"; do
  [ -d "${p#"$module"/}" ] && printf '%s\n' "$p"; done | sort -u)
mapfile -t pkgs < <(printf '%s\n' "${vet[@]}" | grep -vxF "$engine" | grep -v '^$' || true)

in_vet() { printf '%s\n' "${vet[@]}" | grep -qxF "$module/$1"; }
flag() { # name, condition exit status
  local v=false; { [ "$all" -eq 1 ] || [ "$2" -eq 0 ]; } && v=true
  echo "$1=$v"
}
st() { "$@" && echo 0 || echo 1; }

echo "pkgs=${pkgs[*]}"
echo "vet=${vet[*]}"
flag ui                         "$(st has '^(web/|scripts/ui\.sh$|internal/ui/)')"
flag check_pins_selftest        "$(st has '^(scripts/check-pins|testdata/(rename-guard-allow|staged)\.conf$)')"
flag install_ffmpeg_selftest    "$(st has '^(scripts/install-ffmpeg|Dockerfile$)')"
flag install_dynhdr_selftest    "$(st has '^(scripts/install-dynhdr-tools|scripts/install-ffmpeg-selftest|Dockerfile$)')"
flag secret_scan_selftest       "$(st has '^(scripts/secret-scan|internal/secretscan/|\.githooks/)')"
flag identity_scan_selftest     "$(st has '^scripts/identity-scan')"
flag api_schema_diff            "$( { has '^(docs/api-schema\.json|\.api-schema-breaks\.yaml)$' || in_vet internal/server || in_vet scripts/api-schema; } && echo 0 || echo 1)"
flag api_schema_diff_selftest   "$(st has '^scripts/api-schema')"
flag mutation_shape             "$(st has '^(\.gremlins\.yaml|docs/mutation-testing\.md|scripts/mutation)')"
flag mutation_selftest          "$(st has '^(scripts/mutation|\.gremlins\.yaml$|internal/(schedule|store)/selftest)')"
flag happy_path_selftest        "$(st has '^(scripts/happy-path-log-selftest|cmd/holdfast/happy_path_log_test\.go$)')"
flag govulncheck                "$(st has '^(\.govulncheck-suppressions\.yaml$|scripts/govulncheck)')"
flag govulncheck_selftest       "$(st has '^scripts/govulncheck')"
flag config_selftest            "$( { has '^(config\.example\.yaml|testdata/config-invalid)' || in_vet internal/config || in_vet cmd/holdfast; } && echo 0 || echo 1)"
flag package                    "$(st has '^(Dockerfile|\.dockerignore|docker-compose\.yml|scripts/smoke-image\.sh)$')"
flag pr_scope_selftest          "$(st has '^scripts/pr-scope')"
