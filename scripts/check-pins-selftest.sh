#!/usr/bin/env bash
# Prove the rename guard in check-pins.sh still BITES. Part of `make check`.
#
# It exists because that guard degraded to a silent GREEN — printing "ok" over a real leak —
# several times while it was being written, and every one of those was invisible in a green
# build. The three that survive as cases below:
#
#   - `git grep -I` SKIPS binary files (`-a` searches them). With `-I` the guard reported
#     clean over a tracked binary carrying the old auth-token env var.
#   - The allowlist was tested against git grep's `path:lineno:content` line, which made it a
#     FILE-level exemption by accident: any PATH containing the allow term exempted the whole
#     file.
#   - `git grep` with no rev reads the WORKTREE, but `git commit` ships the INDEX.
#   - git's own exit status was swallowed, so a `dubious ownership` failure (128) turned the
#     guard into a no-op that reported success.
#
# A guard nobody has tried to defeat is a guard nobody knows works. This defeats it on
# purpose, on every run.
#
# NOTE: this file must name the identifiers it forbids in order to build its fixtures, and it
# deliberately does NOT take an allowlist exemption to do so — it COMPOSES them at runtime, so
# the source never contains the banned string. An exemption in the one file whose job is
# proving there are none would be the hole it is testing for.
#
# Runs entirely inside a throwaway clone; it never mutates the working tree.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)" || { echo "::error::selftest: mktemp failed" >&2; exit 1; }
trap 'rm -rf "$work"' EXIT

# The banned identifiers, composed so this file does not itself contain them.
OLD_ENV="TRANSCODE""_SERVER_AUTH_TOKEN"
OLD_CRF="TRANSCODE""_CRF"
OLD_METRIC="transcode""_files_total"

declared=7
pass=0; failed=0
repo="$work/repo"

git clone -q --no-hardlinks --depth 1 "file://$here" "$repo" 2>/dev/null \
  || { echo "::error::selftest could not clone the repo — it did NOT run" >&2; exit 1; }
git -C "$repo" config user.email t@t.t
git -C "$repo" config user.name t

# Grade the tree AS IT STANDS NOW. The clone carries HEAD, so the whole working tree is
# overlaid on top of it: the clean-tree baseline (case 0) is the reference every "must bite"
# case is measured against, and taking it from HEAD means an uncommitted change anywhere —
# a fix OR a break — is graded as if it did not exist. In CI the two are identical and this is
# a no-op; locally they are not, and locally is where the mistake gets made.
tar -C "$here" --exclude=.git -cf - . | tar -C "$repo" -xf - \
  || { echo "::error::selftest: could not overlay the working tree — it did NOT run" >&2; exit 1; }
git -C "$repo" add -A
git -C "$repo" commit -qm "selftest: grade the working tree, not HEAD" --allow-empty

cmp -s "$here/scripts/check-pins.sh" "$repo/scripts/check-pins.sh" \
  || { echo "::error::selftest: the clone's guard is not the working-tree guard — it graded the wrong thing" >&2; exit 1; }

guard() { ( cd "$repo" && bash scripts/check-pins.sh >/dev/null 2>&1 ); }

# expect <want-exit> <name>.  0 = must pass, 1 = must bite.
expect() {
  local want="$1" name="$2" got=0
  guard || got=$?
  if [ "$got" -eq "$want" ]; then
    printf '  ok: %s\n' "$name"; pass=$((pass + 1))
  else
    printf '::error::selftest: %s — guard exited %s, wanted %s\n' "$name" "$got" "$want" >&2
    failed=$((failed + 1))
  fi
}

reset() { git -C "$repo" reset -q --hard HEAD; git -C "$repo" clean -qfdx; }

# --- 0. A clean tree passes. Without this, every "bites" case below could be a guard that
#        simply fails on everything, which would prove nothing.
expect 0 "a clean tree passes"

# --- 1. A pre-rename identifier in ordinary source.
printf '\n// leak: %s\n' "$OLD_CRF" >> "$repo/internal/version/version.go"
expect 1 "a pre-rename identifier in source is caught"
reset

# --- 2. The same identifier inside a BINARY file. Pins `-a` over `-I`: with `-I`, git grep
#        skips binary files entirely and this case, alone, goes green.
printf 'SQLite format 3\000 fixture %s=synthetic\000' "$OLD_ENV" > "$repo/testdata/jobs.db"
git -C "$repo" add -f testdata/jobs.db
expect 1 "a pre-rename identifier inside a BINARY file is caught (pins -a over -I)"
reset

# --- 3. The allowlist is LINE-level, not FILE-level: a PATH matching the allow term must not
#        exempt the leaks inside it.
mkdir -p "$repo/testdata"
printf '%s=synthetic\n%s 1\n' "$OLD_ENV" "$OLD_METRIC" > "$repo/testdata/rename-guard-allow.conf"
git -C "$repo" add -f testdata/rename-guard-allow.conf
expect 1 "a leak in a file whose PATH matches the allow marker is still caught"
reset

# --- 4. A leak STAGED but deleted from the worktree is what `git commit` will ship, while
#        `git grep` (no rev) reads the worktree. Pins the --cached pass.
printf '%s=synthetic\n' "$OLD_ENV" > "$repo/testdata/staged.conf"
git -C "$repo" add -f testdata/staged.conf
rm -f "$repo/testdata/staged.conf"
expect 1 "a leak STAGED but deleted from the worktree is caught (pins the --cached scan)"
reset

# --- 5. The marker genuinely exempts the LINE that carries it — the rule in CLAUDE.md has to
#        quote the identifiers it forbids, so if this could not pass, the rule could not be
#        written down at all. Planted with a real banned identifier, not asserted on the clean
#        tree (case 0 already does that, and it would be a tautology here).
printf '\n// %s is banned. rename-guard-allow\n' "$OLD_CRF" >> "$repo/internal/version/version.go"
expect 0 "a banned identifier on a line carrying the marker is exempt (the rule can state itself)"
reset

# --- 6. A failing git must be RED, never green. `fatal: detected dubious ownership` (128) is
#        routine in containerised CI when the checkout UID differs from the runner's, and a
#        swallowed exit code turned this guard into a no-op that printed "ok".
fake="$work/fakebin"; mkdir -p "$fake"
printf '#!/bin/sh\necho "fatal: detected dubious ownership in repository" >&2\nexit 128\n' > "$fake/git"
chmod +x "$fake/git"
got=0
( cd "$repo" && PATH="$fake:$PATH" bash scripts/check-pins.sh >/dev/null 2>&1 ) || got=$?
if [ "$got" -ne 0 ]; then
  printf '  ok: a failing git is RED, not a silent pass\n'; pass=$((pass + 1))
else
  printf '::error::selftest: a failing git reported GREEN — the guard did not run and said nothing\n' >&2
  failed=$((failed + 1))
fi

echo
# Report against the number of cases DECLARED, not the number that ran: "$pass/$pass" is N/N
# by construction and could never show a shortfall.
total=$((pass + failed))
if [ "$total" -ne "$declared" ]; then
  echo "::error::check-pins selftest: ran $total case(s), expected $declared — a case did not execute" >&2
  exit 1
fi
if [ "$failed" -ne 0 ]; then
  echo "::error::check-pins selftest: $failed of $declared case(s) did not bite — the rename guard is not trustworthy" >&2
  exit 1
fi
echo "check-pins selftest: $pass/$declared cases bite"
