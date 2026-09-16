#!/usr/bin/env bash
# What this repository's test mass IS, re-derived from the tree.
#
# A figure nobody can re-derive is a claim, and a claim about test mass gets re-litigated
# instead of re-run. So the ratio and its per-package composition are produced by this
# script, docs/test-mass.md records what it printed and at which commit, and `-check`
# re-derives every figure and compares it against that document.
#
# IT REPORTS AND NEVER REFUSES A CHANGE. There is no threshold here, no target ratio, and
# nothing in `make check` or CI invokes it: a line-count gate would be exactly the
# process-grading this repository's suite was cut free of. `-check` is not a counter-example
# - it grades the DOCUMENT against the tree, never the tree against a target, so the only
# thing it can catch is a figure that went stale.
#
# What counts as a line: a line of a tracked *.go file that, once leading whitespace is
# removed, is neither empty nor beginning with `//`. Test lines are the ones in files named
# *_test.go; production lines are the rest. `/* */` block comments are NOT recognised -
# there are none in this tree, and a scanner for them mistakes the glob patterns that are
# (`**/4K/**` carries both markers), which is the worse error for a figure to carry.
#
# Exit codes: 0 all good · 1 provenance unreadable, or -check found a difference · 2 usage.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
doc_rel="docs/test-mass.md"
marker_begin="<!-- test-mass:begin"
marker_end="<!-- test-mass:end -->"

mode="report"
case "${1-}" in
  "")       ;;
  "-check") mode="check" ;;
  *) echo "usage: scripts/test-mass.sh [-check]" >&2; exit 2 ;;
esac

fail() { printf 'test-mass: %s\n' "$*" >&2; exit 1; }

# --- provenance, before any figure -------------------------------------------------------
# A figure with no provenance is worse than no figure: it reads as a fact about a tree
# nobody can name. So every input that says WHICH tree this is must be readable first, and
# each one that is not is named.
[ -r "$here/go.mod" ] || fail "cannot read $here/go.mod - without it there is no module to
       measure, and any count printed here would name no repository at all."
module="$(sed -n 's/^module[[:space:]]\{1,\}//p' "$here/go.mod" | head -1)"
[ -n "$module" ] || fail "$here/go.mod declares no module path, so the figures would name no repository."

command -v git >/dev/null 2>&1 || fail "no git on PATH - the commit these figures are taken at cannot be read."
top="$(git -C "$here" rev-parse --show-toplevel 2>/dev/null)" ||
  fail "$here is not inside a git repository - the commit these figures are taken at cannot be read."
[ "$top" = "$here" ] ||
  fail "the module root ($here) is not the root of the git repository ($top). The commit
       readable here describes a different tree than the one being counted."
commit="$(git -C "$here" rev-parse HEAD 2>/dev/null)" ||
  fail "$here has no HEAD commit - the figures would carry no provenance."

# --- the measurement ---------------------------------------------------------------------
# The counted set is what git TRACKS, so an untracked scratch file cannot move a figure and
# the set is reproducible from the commit above.
mapfile -t gofiles < <(git -C "$here" ls-files -- '*.go')
[ "${#gofiles[@]}" -gt 0 ] || fail "no tracked *.go file under $here - there is nothing to measure."

measure() {  # prints "prod <n>", "test <n>" and one "pkg <dir> <n>" per package with tests
  ( cd "$here" && awk '
    {
      line = $0
      sub(/^[ \t]+/, "", line)
      if (line == "") next
      if (substr(line, 1, 2) == "//") next
      if (FILENAME ~ /_test\.go$/) {
        test_total++
        dir = FILENAME
        if (sub(/\/[^\/]+$/, "", dir) == 0) dir = "."
        per[dir]++
      } else {
        prod_total++
      }
    }
    END {
      printf "prod %d\n", prod_total + 0
      printf "test %d\n", test_total + 0
      for (d in per) printf "pkg %s %d\n", d, per[d]
    }
  ' "${gofiles[@]}" )
}

block() {
  local raw prod tests ratio
  raw="$(measure)"
  prod="$(printf '%s\n' "$raw" | awk '$1 == "prod" { print $2; exit }')"
  tests="$(printf '%s\n' "$raw" | awk '$1 == "test" { print $2; exit }')"
  ratio="$(awk -v t="$tests" -v p="$prod" 'BEGIN { if (p + 0 == 0) print "n/a"; else printf "%.2f", t / p }')"
  printf 'commit %s\n' "$commit"
  printf 'module %s\n' "$module"
  printf 'production-lines %s\n' "$prod"
  printf 'test-lines %s\n' "$tests"
  printf 'test-to-production %s\n' "$ratio"
  printf '%s\n' "$raw" | awk '$1 == "pkg" { printf "package %s %s\n", $2, $3 }' | LC_ALL=C sort
}

if [ "$mode" = "report" ]; then
  block
  exit 0
fi

# --- -check ------------------------------------------------------------------------------
doc="$here/$doc_rel"
[ -r "$doc" ] || fail "cannot read $doc_rel - there is no recorded measurement to check."

recorded="$(awk -v b="$marker_begin" -v e="$marker_end" '
  index($0, b) == 1 { on = 1; next }
  index($0, e) == 1 { on = 0 }
  on && $0 !~ /^```/ { print }
' "$doc")"
[ -n "$recorded" ] || fail "$doc_rel carries no measurement block between $marker_begin ... and $marker_end."

recorded_commit="$(printf '%s\n' "$recorded" | awk '$1 == "commit" { print $2; exit }')"
[ -n "$recorded_commit" ] || fail "$doc_rel records no commit, so there is no way to tell which tree its figures describe."

# The document cannot name the commit that CONTAINS it - a file would have to carry the hash
# of the object it is part of - so the recorded commit is the one the figures were taken at,
# and what is verified is that this tree is still that tree: every counted file identical
# between the two commits, and nothing counted left uncommitted. A difference in any counted
# file is refused rather than compared, because that would be comparing figures across two
# different trees.
git -C "$here" cat-file -e "$recorded_commit^{commit}" 2>/dev/null ||
  fail "$doc_rel records commit $recorded_commit, which this repository does not have. Its
       figures describe a tree that cannot be reached from here."
git -C "$here" diff --quiet HEAD -- '*.go' ||
  fail "there are uncommitted changes to counted *.go files, so no commit describes the tree
       being measured. Commit them and re-run scripts/test-mass.sh."
if [ "$recorded_commit" != "$commit" ]; then
  git -C "$here" diff --quiet "$recorded_commit" HEAD -- '*.go' ||
    fail "$doc_rel records commit $recorded_commit and HEAD is $commit, and the *.go files
       differ between them. The recorded figures describe the other tree: re-run
       scripts/test-mass.sh and record what it prints."
  printf 'test-mass: the record was taken at %s and HEAD is %s; no counted file differs between them.\n' \
    "${recorded_commit:0:12}" "${commit:0:12}"
fi

derived="$(block | sed "s/^commit .*/commit $recorded_commit/")"

diff_line="$(diff <(printf '%s\n' "$recorded") <(printf '%s\n' "$derived") | head -40 || true)"
if [ -n "$diff_line" ]; then
  first_recorded="$(printf '%s\n' "$recorded" | head -1)"
  first_derived="$(printf '%s\n' "$derived" | head -1)"
  # Name the FIRST figure that differs, rather than handing over a whole diff to read.
  n=1
  while :; do
    r="$(printf '%s\n' "$recorded" | sed -n "${n}p")"
    d="$(printf '%s\n' "$derived" | sed -n "${n}p")"
    [ -z "$r" ] && [ -z "$d" ] && break
    if [ "$r" != "$d" ]; then
      fail "$doc_rel is stale at line $n of its measurement block.
       it records: ${r:-(nothing)}
       the tree says: ${d:-(nothing)}
       Re-run scripts/test-mass.sh and record what it prints."
    fi
    n=$((n + 1))
  done
  fail "$doc_rel disagrees with the tree:
$(printf '%s\n' "$diff_line" | sed 's/^/       /')
       (first line recorded: $first_recorded; first line derived: $first_derived)"
fi

printf 'test-mass: %s matches the tree at %s.\n' "$doc_rel" "${recorded_commit:0:12}"
