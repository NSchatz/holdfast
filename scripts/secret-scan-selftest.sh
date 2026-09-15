#!/usr/bin/env bash
# Prove the secret scanner still BITES, and prove the PRE-COMMIT PATH refuses a real
# commit. Part of `make check`.
#
# A scanner is the guard whose failure is most completely invisible: a clean run and a
# broken run print the same word. So every covered credential family, every forbidden
# filename, every exit code and the hook itself is defeated on purpose here, against a
# THROWAWAY CLONE, on every run. A guard nobody tries to defeat is a guard nobody knows
# works.
#
# THE FIXTURES ARE COMPOSED AT RUNTIME AND NEVER COMMITTED. This script reads every
# tracked file, including itself, so a fixture written as a literal would make
# `make secret-scan` red by construction and the only ways out would be an allowlist that
# grows until the scanner stops scanning, or deleting the proof. Each credential below is
# therefore built from adjacent string pieces, so the pattern that matches it at runtime
# cannot match this source. scripts/check-pins-selftest.sh settled that question first;
# this follows it.
#
# Four families of case:
#   0-13   a clean tree passes, every credential family is caught with path/line/family,
#          and a finding prints a truncated prefix rather than the credential
#   14-22  the forbidden-filename rule, its documented example exceptions, and the bounded
#          exemption register refusing to go stale or to grant anything it was not given
#   23-29  the exit-code contract: clean, FOUND, COULD NOT RUN, bad invocation, and the
#          help text that documents them
#   30-32  the pre-commit path, driven by REAL `git commit` attempts in a clone that has
#          performed only `make install-hooks`, plus the gate wiring the CI job inherits
#
# Runs entirely inside a throwaway clone; it never mutates the working tree.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)" || { echo "::error::selftest: mktemp failed" >&2; exit 1; }
trap 'rm -rf "$work"' EXIT

declared=33
pass=0; failed=0
repo="$work/repo"

# The exit codes under test, named so a case reads as the contract rather than as a number.
CLEAN=0; FOUND=3; CANNOT_RUN=4; USAGE=2

# --- the fixtures, COMPOSED so this file never contains one ---------------------------
# Each is <prefix><payload>, split across two shell string literals at exactly the point
# where the pattern needs payload, so the pattern cannot match the line that builds it.
F_AWS="AKIA""QQQQQQQQQQQQQQQQ"
F_GH="ghp_""xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
F_GHPAT="github_pat_""yyyyyyyyyyyyyyyyyyyyyyyyyyyyyy"
F_SLACK="xoxb-""111111111111111111111111"
F_SLACKHOOK="hooks.slack.com/services/T""ZZZZZZZZZZZZZZZZZZZZ"
F_GOOGLE="AIza""aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
F_STRIPE="sk_live_""333333333333333333333333"
F_ANTHROPIC="sk-ant-""AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
F_OPENAI="sk-""BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
F_PEM="-----BEGIN RSA PRIVATE"" KEY-----"
F_PYPI="pypi-""AgEIcHlwaS5vcmc""cccccccccc"
F_NPMAUTH="_authToken""=deadbeefdeadbeef"

git clone -q --no-hardlinks --depth 1 "file://$here" "$repo" 2>/dev/null \
  || { echo "::error::selftest could not clone the repo - it did NOT run" >&2; exit 1; }
git -C "$repo" config user.email t@t.t
git -C "$repo" config user.name t

# Grade the tree AS IT STANDS NOW: the clone carries HEAD, so the whole working tree is
# overlaid on top of it. Without this an uncommitted change - a fix OR a break - would be
# graded as if it did not exist, and locally is where that mistake gets made.
tar -C "$here" --exclude=.git --exclude=node_modules -cf - . | tar -C "$repo" -xf - \
  || { echo "::error::selftest: could not overlay the working tree - it did NOT run" >&2; exit 1; }
git -C "$repo" add -A
git -C "$repo" commit -qm "selftest: grade the working tree, not HEAD" --allow-empty --no-verify

cmp -s "$here/scripts/secret-scan.sh" "$repo/scripts/secret-scan.sh" \
  || { echo "::error::selftest: the clone's scanner is not the working-tree scanner - it graded the wrong thing" >&2; exit 1; }

# Build the scanner ONCE, here, and point every case at that binary. The wrapper rebuilds
# per invocation, which over thirty cases is most of this script's runtime; the binary
# under test is identical either way, and case 22 drives the wrapper itself so the
# wrapper's own exit-code propagation is still proven.
scanbin="$work/secret-scan"
( cd "$repo" && go build -o "$scanbin" ./scripts/secret-scan ) \
  || { echo "::error::selftest: the scanner did not build - it did NOT run" >&2; exit 1; }

scan_out=""
scan() { scan_out="$( "$scanbin" --root "$repo" "$@" 2>&1 )"; }

# expect <want-exit> <name> [must-mention-regex...]
# The output is CAPTURED, not discarded: once a scanner has more than one reason to exit
# non-zero, "it exited 3" stops being evidence that the case under test is the one that
# bit. A case naming a message is graded on the message too.
expect() {
  local want="$1" name="$2"; shift 2
  local got=0
  scan || got=$?
  if [ "$got" -ne "$want" ]; then
    printf '::error::selftest: %s - scanner exited %s, wanted %s\n' "$name" "$got" "$want" >&2
    printf '%s\n' "$scan_out" | sed 's/^/       | /' >&2
    failed=$((failed + 1)); return
  fi
  local re
  for re in "$@"; do
    if ! grep -qE -- "$re" <<<"$scan_out"; then
      printf '::error::selftest: %s: exited %s (correct) but for the WRONG REASON, nothing in its output matched /%s/\n' "$name" "$got" "$re" >&2
      printf '%s\n' "$scan_out" | sed 's/^/       | /' >&2
      failed=$((failed + 1)); return
    fi
  done
  printf '  ok: %s\n' "$name"; pass=$((pass + 1))
}

reset() { git -C "$repo" reset -q --hard HEAD; git -C "$repo" clean -qfdx -e /secret-scan; }

# plant <relative-path> <content>: write a TRACKED file, because the scan's scope is
# tracked files and an untracked one is not on its way into a commit.
plant() {
  mkdir -p "$(dirname "$repo/$1")"
  printf '%s\n' "$2" > "$repo/$1"
  git -C "$repo" add -f -- "$1"
}

# --- 0. A clean tree passes. Without this every "bites" case below could be a scanner
#        that simply fails on everything, which would prove nothing. It is also AC-11:
#        this repository AS COMMITTED is clean, and no fixture above sits in it.
expect $CLEAN "a clean tree passes (nothing seeded is in the scanned tree)" "clean"

# =====================================================================================
# Cases 1-12 (AC-9): every covered credential family is caught, and the finding names the
# PATH, the LINE NUMBER and the FAMILY while printing only a truncated prefix.
# =====================================================================================
family_case() {
  local name="$1" payload="$2" want_family="$3"
  plant "internal/version/leak.go" "package version

// fixture
const leak = \"$payload\""
  expect $FOUND "$name is caught" "internal/version/leak\.go:4:" "$want_family"
  reset
}

family_case "an AWS access key id"          "$F_AWS"        "AWS access key id"
family_case "a GitHub token"                "$F_GH"         "GitHub token"
family_case "a GitHub fine-grained token"   "$F_GHPAT"      "GitHub fine-grained token"
family_case "a Slack token"                 "$F_SLACK"      "Slack token"
family_case "a Slack webhook"               "$F_SLACKHOOK"  "Slack webhook"
family_case "a Google API key"              "$F_GOOGLE"     "Google API key"
family_case "a Stripe secret key"           "$F_STRIPE"     "Stripe secret key"
family_case "an Anthropic API key"          "$F_ANTHROPIC"  "Anthropic API key"
family_case "an OpenAI API key"             "$F_OPENAI"     "OpenAI API key"
family_case "a PEM private key block"       "$F_PEM"        "PEM private key block"
family_case "a PyPI upload token"           "$F_PYPI"       "PyPI upload token"
family_case "an npm registry auth directive" "$F_NPMAUTH"   "npm registry auth directive"

# --- 13 (AC-9). The report TRUNCATES. A finding that printed the whole match would put the
#        credential into every CI log that ever ran the gate - which outlives the rotation.
plant "internal/version/leak.go" "package version
const leak = \"$F_AWS\""
got=0
scan || got=$?
if [ "$got" -ne "$FOUND" ]; then
  printf '::error::selftest: truncation case did not reach a finding (exit %s)\n' "$got" >&2
  failed=$((failed + 1))
elif grep -qF -- "$F_AWS" <<<"$scan_out"; then
  printf '::error::selftest: the WHOLE MATCH was printed - the report discloses the credential it found\n' >&2
  printf '%s\n' "$scan_out" | sed 's/^/       | /' >&2
  failed=$((failed + 1))
elif ! grep -qF -- "${F_AWS:0:8}" <<<"$scan_out"; then
  printf '::error::selftest: no truncated prefix was printed - a finding nobody can locate\n' >&2
  failed=$((failed + 1))
else
  printf '  ok: a finding prints a truncated prefix and never the whole match\n'; pass=$((pass + 1))
fi
reset

# =====================================================================================
# Cases 14-21 (AC-10): the forbidden-FILENAME rule, whatever the file contains.
# =====================================================================================
name_case() {
  local name="$1" path="$2"
  plant "$path" "# this file is deliberately empty of credentials"
  expect $FOUND "$name" "$(printf '%s' "$path" | sed 's/\./\\./g'): forbidden filename"
  reset
}

name_case "a tracked .env is refused whatever it contains"        "testdata/.env"
name_case "a tracked .env.<anything> is refused"                  "testdata/.env.production"
name_case "a tracked id_rsa is refused"                           "testdata/id_rsa"
name_case "a tracked id_ed25519 is refused"                       "testdata/id_ed25519"
name_case "a tracked .npmrc outside the register is refused"      "testdata/.npmrc"
name_case "a tracked .pypirc is refused"                          "testdata/.pypirc"

# --- 20. The documented example suffixes are ALLOWED. A rule that refuses everything is
#         indistinguishable from a rule that works and is impossible to comply with.
plant "testdata/.env.example" "TOKEN=put-a-reference-here"
plant "testdata/.env.sample" "TOKEN="
plant "testdata/.env.template" "TOKEN="
expect $CLEAN "the documented .env.example/.sample/.template names are allowed"
reset

# --- 21. `.env.example.real` is NOT an example: the exceptions are exact NAMES, not a
#         prefix rule, or every forbidden name would be one suffix away from allowed.
name_case "a name that merely STARTS like an example is still refused" "testdata/.env.example.real"

# =====================================================================================
# Cases 22-25: the bounded exemption register. It exists for one collision the repository
# cannot resolve otherwise (check-pins.sh section 8 REQUIRES a committed .npmrc), and its
# whole value is that it refuses to become an allowlist.
# =====================================================================================
# --- 22. The exempt path is still CONTENT-scanned in full. This is what makes the
#         exemption safe rather than a hole: the name is forgiven, the content never is.
plant "internal/webui/e2e/.npmrc" "ignore-scripts=true
//registry.npmjs.org/:$F_NPMAUTH"
expect $FOUND "the exempted .npmrc still bites on CONTENT" "e2e/\.npmrc:2:" "npm registry auth directive"
reset

# --- 23. A register entry naming a path that is not in the tree grants nothing, and a
#         register nobody prunes is a register that outlives its reason.
sed -i 's|Path: "internal/webui/e2e/\.npmrc"|Path: "internal/webui/e2e/.npmrc-gone"|' \
  "$repo/internal/secretscan/secretscan.go"
( cd "$repo" && go build -o "$scanbin" ./scripts/secret-scan ) >/dev/null 2>&1 \
  || { echo "::error::selftest: case 23 could not rebuild the scanner" >&2; failed=$((failed + 1)); }
expect $CANNOT_RUN "a stale register entry is COULD NOT RUN, not a wider scanner" "grants nothing"
git -C "$repo" checkout -q -- internal/secretscan/secretscan.go
( cd "$repo" && go build -o "$scanbin" ./scripts/secret-scan ) >/dev/null 2>&1
reset

# --- 24. An entry naming a path whose NAME was never forbidden grants nothing either, so
#         the register cannot be used to exempt an ordinary file from anything.
sed -i 's|Path: "internal/webui/e2e/\.npmrc"|Path: "go.mod"|' \
  "$repo/internal/secretscan/secretscan.go"
( cd "$repo" && go build -o "$scanbin" ./scripts/secret-scan ) >/dev/null 2>&1
expect $CANNOT_RUN "a register entry for a name that is not forbidden is COULD NOT RUN" "not forbidden"
git -C "$repo" checkout -q -- internal/secretscan/secretscan.go
( cd "$repo" && go build -o "$scanbin" ./scripts/secret-scan ) >/dev/null 2>&1
reset

# --- 25. An entry with no reason is refused: an exemption nobody can check is an
#         exemption nobody can retire. Mutated by a compiled-in init rather than by sed,
#         because the reason is a multi-line concatenation and a sed that edited only its
#         first line would leave a reason that is still non-empty - a mutation that did not
#         mutate, which is the selftest version of the silent green this file exists for.
cat > "$repo/internal/secretscan/zz_selftest_mutation.go" <<'GO'
package secretscan

func init() { NameExemptions[0].Reason = "" }
GO
if ( cd "$repo" && go build -o "$scanbin" ./scripts/secret-scan ) >/dev/null 2>&1; then
  expect $CANNOT_RUN "a register entry with no reason is COULD NOT RUN" "no reason"
else
  printf '::error::selftest: case 25 could not rebuild the scanner after the mutation\n' >&2
  failed=$((failed + 1))
fi
rm -f "$repo/internal/secretscan/zz_selftest_mutation.go"
( cd "$repo" && go build -o "$scanbin" ./scripts/secret-scan ) >/dev/null 2>&1
reset

# =====================================================================================
# Cases 26-29 (AC-12): the exit-code contract. A caller RETRIES "could not run" and OBEYS
# "found a credential", so one code for both is a failure of the contract.
# =====================================================================================
# --- 26. An EMPTY enumeration is COULD NOT RUN, never clean. Comparing nothing against a
#         ruleset passes every file it never saw, which is the silent-green failure this
#         whole script exists for.
empty="$work/empty"; mkdir -p "$empty"; git -C "$empty" init -q
got=0
out="$( "$scanbin" --root "$empty" 2>&1 )" || got=$?
if [ "$got" -eq "$CANNOT_RUN" ] && grep -qE "listed no files" <<<"$out"; then
  printf '  ok: an empty enumeration is COULD NOT RUN (%s), never clean\n' "$CANNOT_RUN"; pass=$((pass + 1))
else
  printf '::error::selftest: an empty enumeration exited %s (wanted %s):\n%s\n' "$got" "$CANNOT_RUN" "$out" >&2
  failed=$((failed + 1))
fi

# --- 27. A FAILING git is COULD NOT RUN, not a silent pass. `fatal: detected dubious
#         ownership` (128) is routine in containerised CI, and a swallowed status turned
#         check-pins.sh into a no-op that printed "ok".
fake="$work/fakebin"; mkdir -p "$fake"
printf '#!/bin/sh\necho "fatal: detected dubious ownership in repository" >&2\nexit 128\n' > "$fake/git"
chmod 0755 "$fake/git"
got=0
out="$( PATH="$fake:$PATH" "$scanbin" --root "$repo" 2>&1 )" || got=$?
if [ "$got" -eq "$CANNOT_RUN" ]; then
  printf '  ok: a failing git is COULD NOT RUN (%s), not a silent pass\n' "$CANNOT_RUN"; pass=$((pass + 1))
else
  printf '::error::selftest: a failing git exited %s (wanted %s):\n%s\n' "$got" "$CANNOT_RUN" "$out" >&2
  failed=$((failed + 1))
fi

# --- 28. A bad invocation is its own code, so "I typed it wrong" is never mistaken for
#         either "clean" or "found".
got=0
out="$( "$scanbin" --root "$repo" --no-such-flag 2>&1 )" || got=$?
if [ "$got" -eq "$USAGE" ]; then
  printf '  ok: a bad invocation is %s, distinct from clean, FOUND and COULD NOT RUN\n' "$USAGE"; pass=$((pass + 1))
else
  printf '::error::selftest: a bad invocation exited %s (wanted %s):\n%s\n' "$got" "$USAGE" "$out" >&2
  failed=$((failed + 1))
fi

# --- 29. Every code is LISTED IN THE HELP TEXT. A documented code nobody can read from the
#         invocation is not documented, and AC-12 asks for the list to live there.
help_out="$( "$scanbin" -h 2>&1 || true )"
missing=""
for token in "^  $CLEAN  clean" "^  $FOUND  FOUND" "^  $CANNOT_RUN  COULD NOT RUN"; do
  grep -qE -- "$token" <<<"$help_out" || missing="$missing /$token/"
done
if [ -z "$missing" ]; then
  printf '  ok: the help text lists every exit code (%s clean, %s FOUND, %s COULD NOT RUN)\n' "$CLEAN" "$FOUND" "$CANNOT_RUN"
  pass=$((pass + 1))
else
  printf '::error::selftest: the help text does not list:%s\n%s\n' "$missing" "$help_out" >&2
  failed=$((failed + 1))
fi

# =====================================================================================
# Cases 30-32 (AC-13, AC-14): the PRE-COMMIT PATH, driven by real commit attempts, and the
# gate wiring the CI job inherits.
# =====================================================================================
# THE ONE DOCUMENTED SETUP STEP, and nothing else. If a second step were needed, this case
# would fail and the documentation would be wrong.
hooked="$work/hooked"
git clone -q --no-hardlinks "file://$repo" "$hooked" 2>/dev/null \
  || { echo "::error::selftest: could not clone for the pre-commit cases" >&2; exit 1; }
git -C "$hooked" config user.email t@t.t
git -C "$hooked" config user.name t
( cd "$hooked" && make install-hooks ) >/dev/null \
  || { echo "::error::selftest: \`make install-hooks\` - the one documented setup step - failed" >&2; exit 1; }

# --- 30 (AC-13). A commit carrying a seeded credential is REFUSED: HEAD does not move, no
#         new commit object exists, and the refusal names the path and the line.
before="$(git -C "$hooked" rev-parse HEAD)"
before_n="$(git -C "$hooked" rev-list --count HEAD)"
mkdir -p "$hooked/internal/version"
printf 'package version\n\n// fixture\nconst leak = "%s"\n' "$F_AWS" > "$hooked/internal/version/leak.go"
git -C "$hooked" add -f -- internal/version/leak.go
commit_out=""; got=0
commit_out="$( cd "$hooked" && git commit -m "S0132: a credential that must never land" 2>&1 )" || got=$?
after="$(git -C "$hooked" rev-parse HEAD)"
after_n="$(git -C "$hooked" rev-list --count HEAD)"
if [ "$got" -eq 0 ]; then
  printf '::error::selftest: THE COMMIT SUCCEEDED - a credential reached a commit object\n%s\n' "$commit_out" >&2
  failed=$((failed + 1))
elif [ "$before" != "$after" ] || [ "$before_n" != "$after_n" ]; then
  printf '::error::selftest: HEAD moved (%s -> %s, %s -> %s commits) despite the refusal\n' \
    "$before" "$after" "$before_n" "$after_n" >&2
  failed=$((failed + 1))
elif ! grep -qE "internal/version/leak\.go:4:" <<<"$commit_out"; then
  printf '::error::selftest: the refusal did not name the path and line that blocked it\n%s\n' "$commit_out" >&2
  failed=$((failed + 1))
elif grep -qF -- "$F_AWS" <<<"$commit_out"; then
  printf '::error::selftest: the refusal printed the whole credential\n%s\n' "$commit_out" >&2
  failed=$((failed + 1))
else
  printf '  ok: the pre-commit hook REFUSED the commit, HEAD unmoved, naming the path and line\n'
  pass=$((pass + 1))
fi

# --- 31 (AC-13). A credential STAGED and then deleted from the worktree is still refused.
#         `git commit` ships the INDEX while a worktree scan would report this tree clean -
#         the same bug check-pins.sh had, on the path where it matters most.
rm -f "$hooked/internal/version/leak.go"
got=0
commit_out="$( cd "$hooked" && git commit -m "S0132: staged but not in the worktree" 2>&1 )" || got=$?
if [ "$got" -ne 0 ] && grep -qE "internal/version/leak\.go:4:" <<<"$commit_out"; then
  printf '  ok: a credential staged but absent from the worktree is still refused (the index is what ships)\n'
  pass=$((pass + 1))
else
  printf '::error::selftest: a staged-only credential was not caught (exit %s):\n%s\n' "$got" "$commit_out" >&2
  failed=$((failed + 1))
fi
git -C "$hooked" reset -q HEAD -- internal/version/leak.go 2>/dev/null || true
git -C "$hooked" checkout -q -- . 2>/dev/null || true
git -C "$hooked" clean -qfd

# --- 32. A CLEAN commit still SUCCEEDS, and the gate the CI job runs actually reaches the
#         scan. A hook that refuses everything is indistinguishable from a hook that works
#         and is impossible to comply with; and a scan wired into nothing blocks nothing.
printf '\n// selftest: an ordinary comment, no credential.\n' >> "$hooked/README.md"
git -C "$hooked" add -- README.md
clean_ok=1
( cd "$hooked" && git commit -qm "S0132: an ordinary commit must still land" ) || clean_ok=0
wiring=""
grep -qE '^check:.* secret-scan( |$)' "$repo/Makefile" || wiring="$wiring the Makefile's check: target does not name secret-scan;"
grep -qE '^check:.* secret-scan-selftest( |$)' "$repo/Makefile" || wiring="$wiring the Makefile's check: target does not name secret-scan-selftest;"
# The gate step in ci.yml must carry no `if:` - a conditional required step is a step that
# can be skipped, and AC-14 asks for one that cannot.
awk '/- name: make check \(the gate\)/{found=1} found && /if:/{print "conditional"; exit}' \
  "$repo/.github/workflows/ci.yml" | grep -q conditional && wiring="$wiring ci.yml's gate step carries an if:;"
grep -qE '^[[:space:]]*run: make check$' "$repo/.github/workflows/ci.yml" \
  || wiring="$wiring ci.yml has no unconditional \`run: make check\` step;"
if [ "$clean_ok" -eq 1 ] && [ -z "$wiring" ]; then
  printf '  ok: a clean commit still lands, and `make check` (which CI runs unconditionally) reaches the scan\n'
  pass=$((pass + 1))
else
  [ "$clean_ok" -eq 1 ] || printf '::error::selftest: a CLEAN commit was refused - the hook refuses everything\n' >&2
  [ -z "$wiring" ] || printf '::error::selftest: gate wiring:%s\n' "$wiring" >&2
  failed=$((failed + 1))
fi

echo
# Report against the number of cases DECLARED, not the number that ran: "$pass/$pass" is
# N/N by construction and could never show a shortfall.
total=$((pass + failed))
if [ "$total" -ne "$declared" ]; then
  echo "::error::secret-scan selftest: ran $total case(s), expected $declared - a case did not execute" >&2
  exit 1
fi
if [ "$failed" -ne 0 ]; then
  echo "::error::secret-scan selftest: $failed of $declared case(s) did not bite - the scanner is not trustworthy" >&2
  exit 1
fi
echo "secret-scan selftest: $pass/$declared cases bite"
