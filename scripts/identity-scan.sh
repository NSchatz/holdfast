#!/usr/bin/env bash
# The identity scan: this repository is public, and the owner's name and email appear in no
# tracked file but LICENSE and NOTICE. Part of `make check`; scripts/identity-scan-selftest.sh
# proves it bites.
#
# WHO THE OWNER IS is derived at run time and never written down here. It is the author of
# the repository's FIRST commit (of every root commit, if histories were ever joined):
#
#   - a literal would put the very name this guards into a tracked file, so the guard would
#     need an exemption for itself on day one;
#   - the configured git identity names whoever runs the gate, and in CI that is nobody;
#   - HEAD's author is whoever made the last commit, which on a pull request is a merge
#     commit GitHub made.
#
# The first commit is the same answer in every full clone. A SHALLOW clone does not have it,
# and its cut-off commit would answer "whoever authored that", so a shallow clone is refused
# (exit 4) rather than scanned for the wrong person: an identity read from the wrong commit
# passes every tree, and a vacuous pass is the one answer this must never give. CI checks out
# with fetch-depth: 0 for this reason.
#
# A match is any of: a word of the name (two characters or more), the email, the email's
# local part. Whole words, ignoring case, over the working-tree content of every tracked
# file, binary files included. Whole words is what keeps the GitHub account name in URLs and
# the Go module path out of it: a surname inside a longer word is not the name.
#
# A finding prints the PATH, the LINE and WHICH PART of the identity matched, never the
# matched text and never the identity itself: the CI log of a public repository is public.
# A clean run prints a fingerprint of the identity it derived (the first 12 hex digits of a
# SHA-256), so a local run and a CI run can be seen to have looked for the same person.
#
# Exit codes:
#   0  clean        3  FOUND the owner's identity        4  COULD NOT RUN        2  bad invocation
set -euo pipefail

usage() {
  cat <<'EOF'
usage: scripts/identity-scan.sh [--root DIR]

Fails when a tracked file other than LICENSE and NOTICE names the author of the repository's
first commit: a word of the name, the email, or the email's local part, as a whole word and
ignoring case.

  --root DIR   the repository to scan (default: the one this script lives in)

exit codes: 0 clean, 3 found, 4 could not run, 2 bad invocation
EOF
}

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
while [ $# -gt 0 ]; do
  case "$1" in
    --root)
      [ $# -ge 2 ] || { echo "identity-scan: --root needs a directory" >&2; usage >&2; exit 2; }
      root="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "identity-scan: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

cannot() {
  echo "::error::identity-scan COULD NOT RUN: $*. Nothing has been cleared." >&2
  exit 4
}

git -C "$root" rev-parse --git-dir >/dev/null 2>&1 \
  || cannot "$root is not a git repository, so neither its tracked files nor its first commit can be read"
[ "$(git -C "$root" rev-parse --is-shallow-repository 2>/dev/null)" = false ] \
  || cannot "$root is a shallow clone, so the repository's first commit (where the owner's identity is read from) is not in it; fetch the full history (actions/checkout: fetch-depth: 0)"
roots="$(git -C "$root" rev-list --max-parents=0 HEAD 2>/dev/null)" \
  || cannot "$root has no commit to read the owner's identity from"
[ -n "$roots" ] || cannot "$root has no first commit to read the owner's identity from"

# tokens[i] is what is matched and labels[i] is what a finding calls it; the label is all a
# log ever shows. seen holds the lower-cased tokens, so a local part equal to a name word is
# looked for (and reported) once.
tokens=(); labels=(); seen=" "
add() {
  local lc="${1,,}"
  case "$seen" in *" $lc "*) return ;; esac
  seen="$seen$lc "
  tokens+=("$1"); labels+=("$2")
}

fingerprint_input=""; name_words=0; emails=0; first=""
for c in $roots; do
  name="$(git -C "$root" log -1 --format=%an "$c")"
  email="$(git -C "$root" log -1 --format=%ae "$c")"
  [ -n "$first" ] || first="$(git -C "$root" rev-parse --short "$c")"
  fingerprint_input="$fingerprint_input${name,,}"$'\n'"${email,,}"$'\n'
  read -r -a words <<<"$name"
  n=0
  for w in "${words[@]}"; do
    n=$((n + 1))
    [ "${#w}" -ge 2 ] || continue
    add "$w" "word $n of the name"
    name_words=$((name_words + 1))
  done
  case "$email" in
    ?*@?*)
      add "$email" "the email"
      local_part="${email%@*}"
      [ "${#local_part}" -lt 2 ] || add "$local_part" "the email's local part"
      emails=$((emails + 1))
      ;;
  esac
done
[ "$name_words" -gt 0 ] \
  || cannot "the first commit's author name has no word of two characters or more, so there is no name to look for"
[ "$emails" -gt 0 ] \
  || cannot "the first commit's author has no email address, so there is no email to look for"

tracked="$(git -C "$root" ls-files | wc -l)"
[ "$tracked" -gt 0 ] || cannot "$root tracks no files, so a clean result would describe nothing"

found=0
for i in "${!tokens[@]}"; do
  # -z prints "path NUL line NUL text". The NULs become tabs BEFORE the output is captured,
  # because a shell variable cannot hold a NUL, and then only the first two fields are read:
  # the text is dropped here and never reaches a log. With pipefail the status is git
  # grep's: 1 is "no match", anything above it is a grep that did not run.
  status=0
  hits="$(git -C "$root" grep --no-color -z -n -a -i -w -F -e "${tokens[$i]}" \
    -- . ':(exclude)LICENSE' ':(exclude)NOTICE' | tr '\0' '\t')" || status=$?
  case "$status" in
    0) ;;
    1) continue ;;
    *) cannot "git grep failed (exit $status)" ;;
  esac
  while IFS=$'\t' read -r path line _; do
    printf '::error::identity-scan: FOUND %s at %s:%s\n' "${labels[$i]}" "$path" "$line" >&2
    found=$((found + 1))
  done <<<"$hits"
done

fp="$(printf '%s' "$fingerprint_input" | sha256sum | cut -c1-12)"
if [ "$found" -gt 0 ]; then
  echo "::error::identity-scan: $found line(s) name the owner (the author of the first commit $first, fingerprint $fp). Replace them with a synthetic identity; only LICENSE and NOTICE may carry it." >&2
  exit 3
fi
echo "identity-scan: clean - $tracked tracked files carry no word of the owner's name and not their email outside LICENSE and NOTICE (the author of the first commit $first: $name_words name word(s) and $emails email; fingerprint $fp)."
