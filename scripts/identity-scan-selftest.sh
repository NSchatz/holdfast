#!/usr/bin/env bash
# Prove the identity scan still BITES. Part of `make check`.
#
# A clean scan and a scan that looked for nobody print the same word, so every way the scan
# could come to look for the wrong person, or for nobody, is driven here on purpose: each
# case builds a THROWAWAY repository whose first commit a synthetic owner authored, plants
# something, and grades the exit code AND the output. Nothing here touches the tree `check`
# is grading, and the synthetic owner is nobody's real identity, so this file is clean under
# the real scan by construction.
#
# Every case also checks that the output never carries the identity it looked for: the CI
# log of a public repository is public, and a scan that printed what it found would publish
# the very name it exists to keep out.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
scan="$here/scripts/identity-scan.sh"
work="$(mktemp -d)" || { echo "::error::identity-scan selftest: mktemp failed" >&2; exit 1; }
trap 'rm -rf "$work"' EXIT

declared=18
pass=0; failed=0

CLEAN=0; FOUND=3; CANNOT_RUN=4; USAGE=2

# The synthetic owner, the author of every throwaway repository's first commit. The gate
# runner is somebody else, and so is the author of the second commit: the scan must find the
# owner in the FIRST commit, not in the configuration and not at HEAD.
OWNER_FIRST="Ada"
OWNER_LAST="Quillfeather"
OWNER_EMAIL="ada.quill@example.invalid"
RUNNER_NAME="Gate Runner"
RUNNER_EMAIL="runner@example.invalid"
empty_hooks="$work/no-hooks"
mkdir -p "$empty_hooks"

# mkrepo <dir> [owner-name] [owner-email]: a repository whose first commit the owner
# authored and whose second commit, and configured identity, belong to the gate runner.
mkrepo() {
  local d="$1" name="${2:-$OWNER_FIRST $OWNER_LAST}" email="${3:-$OWNER_EMAIL}"
  rm -rf "$d"
  mkdir -p "$d"
  git -C "$d" -c init.defaultBranch=main init -q
  git -C "$d" config user.name "$RUNNER_NAME"
  git -C "$d" config user.email "$RUNNER_EMAIL"
  # This container sets a global hooks path; a throwaway repository runs no hook at all.
  git -C "$d" config core.hooksPath "$empty_hooks"
  printf 'a synthetic project\n' > "$d/README.md"
  git -C "$d" add README.md
  GIT_AUTHOR_NAME="$name" GIT_AUTHOR_EMAIL="$email" git -C "$d" commit -qm "first commit"
  printf 'a second line\n' >> "$d/README.md"
  git -C "$d" commit -qam "second commit, by the runner"
}

# plant <repo> <relative-path> <content>: write a TRACKED file, because the scan's scope is
# tracked files.
plant() {
  mkdir -p "$(dirname "$1/$2")"
  printf '%s\n' "$3" > "$1/$2"
  git -C "$1" add -f -- "$2"
}

out=""
# expect <want-exit> <name> <repo-or-dir> [must-mention-regex...]
expect() {
  local want="$1" name="$2" dir="$3"; shift 3
  local got=0
  out="$("$scan" --root "$dir" 2>&1)" || got=$?
  if [ "$got" -ne "$want" ]; then
    printf '::error::identity-scan selftest: %s - the scan exited %s, wanted %s\n' "$name" "$got" "$want" >&2
    printf '%s\n' "$out" | sed 's/^/       | /' >&2
    failed=$((failed + 1)); return
  fi
  local re
  for re in "$@"; do
    if ! grep -qE -- "$re" <<<"$out"; then
      printf '::error::identity-scan selftest: %s - exited %s (correct) but for the WRONG REASON: nothing in its output matched /%s/\n' "$name" "$got" "$re" >&2
      printf '%s\n' "$out" | sed 's/^/       | /' >&2
      failed=$((failed + 1)); return
    fi
  done
  local tok
  for tok in "$OWNER_FIRST" "$OWNER_LAST" "$OWNER_EMAIL" "${OWNER_EMAIL%@*}"; do
    if grep -qiwF -- "$tok" <<<"$out"; then
      printf '::error::identity-scan selftest: %s - the output PRINTED the identity it looked for\n' "$name" >&2
      printf '%s\n' "$out" | sed 's/^/       | /' >&2
      failed=$((failed + 1)); return
    fi
  done
  printf '  ok: %s\n' "$name"; pass=$((pass + 1))
}

r="$work/repo"

# --- 1. A clean tree passes, and says whose identity it derived and from where. Without
#        this every "bites" case below could be a scan that fails everything.
mkrepo "$r"
plant "$r" docs/guide.md "Nothing here names anybody."
expect "$CLEAN" "a tree that names nobody is clean, and the scan says what it looked for" "$r" \
  '^identity-scan: clean' '2 name word\(s\) and 1 email' 'fingerprint [0-9a-f]{12}'

# --- 2. The full name bites, and the finding names the path and the line.
mkrepo "$r"
plant "$r" docs/guide.md "$(printf 'line one\nline two\nWritten by %s %s.' "$OWNER_FIRST" "$OWNER_LAST")"
expect "$FOUND" "the full name bites, naming path and line" "$r" \
  'FOUND word 1 of the name at docs/guide\.md:3' 'FOUND word 2 of the name at docs/guide\.md:3'

# --- 3. The first name alone, lower-cased, as a username: the proxy-header fixture shape
#        this scan was written after.
mkrepo "$r"
plant "$r" internal/fake/fixture_test.go "var user = \"${OWNER_FIRST,,}\""
expect "$FOUND" "a lower-cased first name alone bites (a username in a fixture)" "$r" \
  'FOUND word 1 of the name at internal/fake/fixture_test\.go:1'

# --- 4. The surname alone bites.
mkrepo "$r"
plant "$r" NOTES.txt "ask ${OWNER_LAST^^} about it"
expect "$FOUND" "the surname alone, in any case, bites" "$r" 'FOUND word 2 of the name at NOTES\.txt:1'

# --- 5. The email bites.
mkrepo "$r"
plant "$r" config.yaml "contact: <$OWNER_EMAIL>"
expect "$FOUND" "the email bites" "$r" 'FOUND the email at config\.yaml:1'

# --- 6. The email's local part at another domain bites.
mkrepo "$r"
plant "$r" config.yaml "contact: ${OWNER_EMAIL%@*}@elsewhere.invalid"
expect "$FOUND" "the email's local part at another domain bites" "$r" \
  "FOUND the email's local part at config\.yaml:1"

# --- 7. Whole words only: an account name that CONTAINS the surname (in a URL, in a Go
#        module path) and a longer word that starts with the first name are not the name.
mkrepo "$r"
plant "$r" go.mod "module github.com/A${OWNER_LAST}/tool"
plant "$r" README.md "$(printf 'See https://github.com/A%s/tool and ghcr.io/a%s/tool.\nAn %smant refusal.' "$OWNER_LAST" "${OWNER_LAST,,}" "$OWNER_FIRST")"
expect "$CLEAN" "whole words only: an account name containing the surname is not the name" "$r" '^identity-scan: clean'

# --- 8. LICENSE and NOTICE at the root may carry the name; the same file name anywhere else
#        may not.
mkrepo "$r"
plant "$r" LICENSE "Copyright (C) $OWNER_FIRST $OWNER_LAST"
plant "$r" NOTICE "Copyright (C) $OWNER_FIRST $OWNER_LAST <$OWNER_EMAIL>"
expect "$CLEAN" "LICENSE and NOTICE at the root are exempt" "$r" '^identity-scan: clean'
plant "$r" docs/NOTICE "Copyright (C) $OWNER_FIRST $OWNER_LAST"
expect "$FOUND" "a NOTICE below the root is not exempt" "$r" 'FOUND word 1 of the name at docs/NOTICE:1'

# --- 9. A tracked BINARY file is scanned too: a committed fixture can carry a name in its
#        metadata.
mkrepo "$r"
mkdir -p "$r/testdata"
printf 'MKV\000\001\002title=%s %s\000\377\n' "$OWNER_FIRST" "$OWNER_LAST" > "$r/testdata/clip.mkv"
git -C "$r" add -f testdata/clip.mkv
expect "$FOUND" "a tracked binary file naming the owner bites" "$r" 'FOUND word 1 of the name at testdata/clip\.mkv:1'

# --- 10. An untracked file is out of scope: the scan grades what can reach a commit.
mkrepo "$r"
printf '%s %s\n' "$OWNER_FIRST" "$OWNER_LAST" > "$r/scratch.txt"
expect "$CLEAN" "an untracked file is out of scope" "$r" '^identity-scan: clean'

# --- 11. The owner is the FIRST commit's author: the configured identity and HEAD's author
#         (the gate runner) are not looked for, and the owner still is.
mkrepo "$r"
plant "$r" docs/runner.md "The gate was run by $RUNNER_NAME <$RUNNER_EMAIL>."
expect "$CLEAN" "the configured identity and HEAD's author are not the owner" "$r" '^identity-scan: clean'
plant "$r" docs/owner.md "and the owner is ${OWNER_LAST}"
expect "$FOUND" "the owner is still found when HEAD and the configuration name someone else" "$r" \
  'FOUND word 2 of the name at docs/owner\.md:1'

# --- 12. A SHALLOW clone is refused, loudly: its cut-off commit is not the first commit,
#         and scanning for its author would pass every tree.
mkrepo "$r"
plant "$r" docs/guide.md "Written by $OWNER_FIRST $OWNER_LAST."
git -C "$r" commit -qm "third commit, which names the owner"
rm -rf "$work/shallow"
git -c core.hooksPath="$empty_hooks" clone -q --depth 1 "file://$r" "$work/shallow" 2>/dev/null
expect "$CANNOT_RUN" "a shallow clone is refused, never scanned for its cut-off commit's author" "$work/shallow" \
  'COULD NOT RUN' 'shallow' 'fetch-depth: 0'

# --- 13. Not a git repository: refused, never "clean".
rm -rf "$work/plain"; mkdir -p "$work/plain"
printf '%s\n' "$OWNER_FIRST" > "$work/plain/file.txt"
expect "$CANNOT_RUN" "a directory that is not a git repository is refused" "$work/plain" \
  'COULD NOT RUN' 'not a git repository'

# --- 14. A first commit whose author has no usable name word leaves nothing to look for:
#         refused, never "clean".
mkrepo "$r" "X Y" "$OWNER_EMAIL"
expect "$CANNOT_RUN" "an author name with no word of two characters is refused" "$r" \
  'COULD NOT RUN' 'no word of two characters'

# --- 15. A bad invocation is exit 2, never a scan of some default.
got=0
"$scan" --no-such-flag >/dev/null 2>&1 || got=$?
if [ "$got" -eq "$USAGE" ]; then
  printf '  ok: an unknown argument is a bad invocation (exit 2)\n'; pass=$((pass + 1))
else
  printf '::error::identity-scan selftest: an unknown argument exited %s, wanted %s\n' "$got" "$USAGE" >&2
  failed=$((failed + 1))
fi

# --- 16. The wiring: `make check` reaches the scan and this selftest, and every workflow
#         job that runs `make check` checks out the full history the scan reads from. A scan
#         wired into nothing blocks nothing, and one that CI ran on a shallow checkout would
#         exit 4 on every pull request.
wiring=""
grep -qE '^check:.* identity-scan( |$)' "$here/Makefile" \
  || wiring="$wiring the Makefile's check: target does not name identity-scan;"
grep -qE '^check:.* identity-scan-selftest( |$)' "$here/Makefile" \
  || wiring="$wiring the Makefile's check: target does not name identity-scan-selftest;"
for wf in "$here/.github/workflows/ci.yml" "$here/.github/workflows/release.yml"; do
  # Every job that runs `make check` must carry `fetch-depth: 0` on its checkout. awk walks
  # the jobs, skipping comments: a job's name line resets the state, a fetch-depth of 0
  # marks it, and a `run: make check` in an unmarked job is reported.
  bad="$(awk '
    /^[[:space:]]*#/ { next }
    /^  [A-Za-z0-9_-]+:[[:space:]]*$/ { job=$1; full=0 }
    /fetch-depth:[[:space:]]*0([[:space:]]|$)/ { full=1 }
    /run:[[:space:]]*make check[[:space:]]*$/ { if (!full) print job }
  ' "$wf")"
  [ -z "$bad" ] || wiring="$wiring $(basename "$wf") runs make check in a shallow checkout (job $bad);"
done
if [ -z "$wiring" ]; then
  printf '  ok: make check reaches the scan and its selftest, and every job running it checks out the full history\n'
  pass=$((pass + 1))
else
  printf '::error::identity-scan selftest: gate wiring:%s\n' "$wiring" >&2
  failed=$((failed + 1))
fi

echo
# Graded against the number of cases DECLARED, not the number that ran: "$pass/$pass" is
# N/N by construction and could never show a shortfall.
total=$((pass + failed))
if [ "$total" -ne "$declared" ]; then
  echo "::error::identity-scan selftest: ran $total case(s), expected $declared - a case did not execute" >&2
  exit 1
fi
if [ "$failed" -ne 0 ]; then
  echo "::error::identity-scan selftest: $failed of $declared case(s) did not bite - the scan is not trustworthy" >&2
  exit 1
fi
echo "identity-scan selftest: $pass/$declared cases bite"
