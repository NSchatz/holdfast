#!/usr/bin/env bash
# Prove scripts/check-design-record still BITES. `make check-design-record-selftest`.
#
# That check holds interface-craft C1 and C2 for this repository: the design record
# declares five identity values and the dashboard's sources carry no C2 default the record
# has not bought with a reason. Every way it could fail is a way it fails SILENTLY - a
# record that could not be read taken for one with nothing in it, an identity-source list
# that resolved to nothing, an exception bought with no reason, a detector that stopped
# detecting - and each of those reports "design record ok" while measuring nothing.
#
# So each is DEFEATED here on purpose, against a mutated COPY of the tree, and the check is
# required to go red AND to say what it saw. Every blocklist entry is defeated in turn too:
# the eleven this surface does not carry are written INTO the copy, and the four it does
# carry lose their exception, which proves the detector finds what is really there.
#
# Case 1 is why the other twenty-eight mean anything: the UNMUTATED copy must PASS, and
# must print the identity sources it read. Without it, "the check reds" would be equally
# true of a check that reds on everything, which decides nothing at all.
#
# Deliberately NOT part of `make check`: the mutations belong in their own target, and
# `check` must never rewrite the tree it is grading. CI runs it beside the release-shape
# self-test. A guard nobody tries to defeat is a guard nobody knows works.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)" || { echo "::error::design-record selftest: mktemp failed" >&2; exit 1; }
# u+rwX first: one case makes the record unreadable on purpose, and a directory this
# cleanup cannot enter would leave the scratch tree behind.
trap 'chmod -R u+rwX "$work" 2>/dev/null || true; rm -rf "$work"' EXIT

declared=29
pass=0; failed=0

repo="$work/repo"
pristine="$work/pristine"
record="docs/design-record.md"
sheet="internal/webui/src/dashboard.css"

# Every file the check opens. Stated here rather than derived, so a source added to the
# check without a thought for this self-test shows up as a case that could not run.
graded=(
  "docs/design-record.md"
  "internal/webui/src/tokens.css"
  "internal/webui/src/dashboard.css"
  "internal/webui/src/index.html.tmpl"
  "internal/webui/index.html"
)

# --- the gate, built ONCE from the WORKING TREE -------------------------------------------
# The working tree's check grading mutated INPUTS is the question: the mutations are to the
# record and the sources, never to the check, so a check weakened to reach green would still
# be the thing under test here.
gate="$work/check-design-record"
( cd "$here" && go build -o "$gate" ./scripts/check-design-record ) || {
  echo "::error::design-record selftest: the check does not build" >&2; exit 1; }

# --- the copy this runs against -----------------------------------------------------------
for rel in "${graded[@]}"; do
  mkdir -p "$repo/$(dirname "$rel")" "$pristine/$(dirname "$rel")"
  [ -r "$here/$rel" ] || { echo "::error::design-record selftest: $rel is not in the working tree, so nothing can be graded against it" >&2; exit 1; }
  cp -p "$here/$rel" "$repo/$rel"
  cp -p "$here/$rel" "$pristine/$rel"
  cmp -s "$here/$rel" "$repo/$rel" || { echo "::error::design-record selftest: the copy of $rel is not the working tree's - it would grade the wrong thing" >&2; exit 1; }
done

reset() {
  for rel in "${graded[@]}"; do
    chmod u+rw "$repo/$rel" 2>/dev/null || true
    cp -p "$pristine/$rel" "$repo/$rel"
  done
}

# changed asserts the mutation actually landed. A sed that matched nothing would otherwise
# leave the pristine copy behind and the case would be graded against it, which is this
# self-test's own version of the silent green it exists to catch.
changed() {  # changed <relative-path> <case-name>
  if cmp -s "$pristine/$1" "$repo/$1"; then
    echo "::error::design-record selftest: the mutation for '$2' changed nothing - that case did NOT run" >&2
    exit 1
  fi
}

# --- editing the record -------------------------------------------------------------------
# A block runs from its `###` heading to the next `###` or `##` heading, which is the
# record's whole grammar, so awk is enough and no case needs to know how a reason wraps.

drop_block() {  # drop_block <heading text>
  awk -v id="### $1" '
    /^### / { skip = ($0 == id); if (skip) next }
    /^## /  { if ($0 !~ /^### /) skip = 0 }
    skip == 0 { print }
  ' "$repo/$record" > "$work/record.new"
  mv "$work/record.new" "$repo/$record"
}

# put_identity_block drops a value and writes a replacement INSIDE the identity section,
# which is where it has to land: a block appended to the end of the file would be read as
# an exception, and the case would then be grading something else entirely.
put_identity_block() {  # put_identity_block <name>, body on stdin
  local body; body="$(cat)"
  drop_block "$1"
  awk -v body="$body" '
    /^## Blocklist exceptions$/ { print body; print "" }
    { print }
  ' "$repo/$record" > "$work/record.new"
  mv "$work/record.new" "$repo/$record"
}

append_exception() {  # append_exception <entry id> <why line>
  printf '\n### %s\n%s\n' "$1" "$2" >> "$repo/$record"
}

append_rule() {  # append_rule <css>
  printf '\n%s\n' "$1" >> "$repo/$sheet"
}

# --- the harness --------------------------------------------------------------------------
out=""; status=0
run_gate() {
  set +e
  out="$("$gate" -root "$repo" 2>&1)"
  status=$?
  set -e
}

# expect <want-exit> <name> <must-mention-regex...>
expect() {
  local want="$1" name="$2"; shift 2
  run_gate
  if [ "$status" -ne "$want" ]; then
    printf '::error::design-record selftest: %s - the check exited %s, wanted %s\n' "$name" "$status" "$want" >&2
    printf '%s\n' "$out" | sed 's/^/       | /' >&2
    failed=$((failed + 1)); return
  fi
  local want_msg
  for want_msg in "$@"; do
    if ! grep -qE -- "$want_msg" <<<"$out"; then
      printf '::error::design-record selftest: %s - exited %s (correct) but for the WRONG REASON: nothing matched /%s/\n' "$name" "$status" "$want_msg" >&2
      printf '%s\n' "$out" | sed 's/^/       | /' >&2
      failed=$((failed + 1)); return
    fi
  done
  printf '  ok: %s\n' "$name"; pass=$((pass + 1))
}

# =========================================================================================
# 1. THE CONTROL. The unmutated copy passes, and says what it read (AC-3, AC-4).
#    The file list is asserted here rather than left for a human to read: the criterion it
#    answers is decided by reading stdout, so something mechanical has to read it.
# =========================================================================================
reset
expect 0 "AC-3, AC-4: the tree as it stands passes, and names every identity source it read" \
  'read [1-9][0-9]* file\(s\), of which [1-9][0-9]* identity source\(s\)' \
  'identity source.*internal/webui/src/tokens\.css \([0-9]+ bytes' \
  'identity source.*internal/webui/src/dashboard\.css \([0-9]+ bytes' \
  'identity source.*internal/webui/src/index\.html\.tmpl \([0-9]+ bytes' \
  'identity source.*internal/webui/index\.html \([0-9]+ bytes' \
  'design record ok'

# =========================================================================================
# 2 to 4. THE RECORD ITSELF (AC-10). Absent, unreadable and unparseable are three different
#    faults with three different fixes, and none of them is "empty": a record this check
#    cannot read must never be read as one that declares nothing and excepts nothing,
#    because that record passes every other assertion over no evidence at all.
# =========================================================================================
reset
rm -f "$repo/$record"
expect 1 "AC-10: an ABSENT record is named as absent, not read as empty" 'IS ABSENT'

reset
chmod 000 "$repo/$record"
expect 1 "AC-10: an UNREADABLE record is named as unreadable" 'CANNOT BE READ'
chmod u+rw "$repo/$record"

reset
sed -i 's/^## Identity$/## Identiy/' "$repo/$record"
changed "$record" "a record that does not parse"
expect 1 "AC-10: a record that DOES NOT PARSE says so, and does not pass" 'DOES NOT PARSE'

# =========================================================================================
# 5 to 9. THE FIVE IDENTITY VALUES (AC-9, and AC-1's reason sentence). Each branch names
#    which value is at fault and which fault it was.
# =========================================================================================
reset
drop_block "accent"
changed "$record" "a missing identity value"
expect 1 "AC-9: a MISSING identity value is named" 'declares NO "accent"'

reset
put_identity_block "accent" <<'BLOCK'
### accent
value: `#0f5aa8`
why: a value that names no token at all, which is held to nothing.
BLOCK
changed "$record" "an identity value naming no token"
expect 1 "AC-9: an identity value that NAMES NO TOKEN is named" '"accent" .*NAMES NO TOKEN'

reset
put_identity_block "accent" <<'BLOCK'
### accent
token: `--accent-that-is-not-declared`
value: `#0f5aa8`
why: a value naming a token the token file has never heard of.
BLOCK
changed "$record" "an identity value naming an undeclared token"
expect 1 "AC-9: an identity value naming a token the token file DOES NOT DECLARE is named" \
  '"accent" names the token .*DOES NOT DECLARE'

reset
put_identity_block "accent" <<'BLOCK'
### accent
token: `--accent`
value: `#ff00ff`
why: a value that has drifted away from the token it names.
BLOCK
changed "$record" "an identity value disagreeing with its token"
expect 1 "AC-9: an identity value that DISAGREES with the token it names is named" \
  '"accent" DISAGREES WITH THE TOKEN IT NAMES' '#ff00ff'

reset
put_identity_block "accent" <<'BLOCK'
### accent
token: `--accent`
value: `#0f5aa8`
why:
BLOCK
changed "$record" "an identity value with no reason"
expect 1 "AC-1: an identity value with NO REASON SENTENCE is named" '"accent" .*carries NO REASON SENTENCE'

# =========================================================================================
# 10. THE VACUOUS PASS (AC-5). A scan that read no file has not passed, and the printed
#     count has to MOVE, or it is a constant dressed up as a measurement.
# =========================================================================================
reset
rm -f "$repo/internal/webui/src/tokens.css" "$repo/internal/webui/src/dashboard.css" \
      "$repo/internal/webui/src/index.html.tmpl" "$repo/internal/webui/index.html"
expect 1 "AC-5: an EMPTY identity-source set is a failure, and the printed count says 0" \
  'NO IDENTITY SOURCE COULD BE READ' 'of which 0 identity source\(s\)'

# =========================================================================================
# 11 to 14. THE EXCEPTION MECHANISM (AC-6, AC-7, AC-8). An entry the record has not bought
#     is refused by name, file and line; one it has bought with a reason is allowed; one it
#     has bought with no reason is not bought.
# =========================================================================================
reset
append_rule '.sd-unexcepted { font-family: Inter, sans-serif; }'
changed "$sheet" "an unexcepted blocklist entry"
expect 1 "AC-6: an UNEXCEPTED entry names the entry, the file and the line" \
  'IS NOT EXCEPTED' "$sheet:[0-9]+" 'inter'

reset
append_rule '.sd-excepted { font-family: Inter, sans-serif; }'
append_exception "inter" 'why: the fixture this self-test writes, which is a reason sentence.'
changed "$sheet" "an excepted blocklist entry"
changed "$record" "an excepted blocklist entry"
expect 0 "AC-7: an entry the record EXCEPTS WITH ITS REASON is allowed" \
  'allowed by the design record' 'design record ok'

reset
append_rule '.sd-reasonless { font-family: Inter, sans-serif; }'
append_exception "inter" 'why: nope'
changed "$record" "an exception with no reason"
expect 1 "AC-8: an exception bought with NO REASON SENTENCE grants nothing" \
  'EXCEPTS .inter. WITH NO REASON SENTENCE'

reset
append_exception "not-a-blocklist-entry" 'why: an exception for something that is not on the list.'
changed "$record" "an exception naming nothing"
expect 1 "AC-7 (the same clause read the other way): an exception naming something that is NOT A BLOCKLIST ENTRY is refused" \
  'NOT A BLOCKLIST ENTRY'

# =========================================================================================
# 15 to 29. EVERY BLOCKLIST ENTRY IN TURN (AC-11). Fifteen entries, fifteen defeats.
#
#     `css`  writes the entry into the copy's stylesheet: the surface does not carry it, so
#            the detector has to find what was just put there.
#     `drop` takes the entry's exception out of the record: the surface DOES carry it, so
#            the detector has to find what is really there, in the real file, at its real
#            line. That direction is the one a weakened detector fails.
# =========================================================================================
while IFS='|' read -r id kind payload; do
  [ -n "$id" ] || continue
  reset
  case "$kind" in
    css)
      append_rule "$payload"
      changed "$sheet" "the blocklist entry $id"
      ;;
    drop)
      drop_block "$payload"
      changed "$record" "the blocklist entry $id"
      ;;
    *)
      echo "::error::design-record selftest: entry '$id' has no defeat kind" >&2
      exit 1
      ;;
  esac
  expect 1 "AC-11: the entry '$id' is found, and the hit names where it is" \
    "\`$id\` IS NOT EXCEPTED" 'internal/webui[^ ]*:[0-9]+'
done <<'ENTRIES'
inter|css|.sd-x { font-family: Inter, sans-serif; }
roboto|drop|roboto
helvetica|drop|helvetica
arial|drop|arial
space-grotesk|css|.sd-x { font-family: "Space Grotesk", sans-serif; }
bare-system-stack|drop|bare-system-stack
indigo-500|css|.bg-indigo-500 { color: var(--fg); }
blue-600|css|.bg-blue-600 { color: var(--fg); }
zinc|css|.bg-zinc-500 { color: var(--fg); }
slate|css|.text-slate-400 { color: var(--fg); }
purple-to-blue-gradient|css|.sd-x { background: linear-gradient(90deg, #a855f7, #3b82f6); }
bg-clip-text|css|.bg-clip-text { color: var(--fg); }
glassmorphism|css|.sd-x { backdrop-filter: blur(8px); }
rounded-2xl|css|.rounded-2xl { border-radius: var(--radius-md); }
shadow-lg|css|.shadow-lg { box-shadow: var(--shadow-raised); }
ENTRIES

reset

echo
# Report against the number of cases DECLARED, not the number that ran: "$pass/$pass" is
# N/N by construction and could never show a shortfall.
total=$((pass + failed))
if [ "$total" -ne "$declared" ]; then
  echo "::error::design-record selftest: ran $total case(s), expected $declared - a case did not execute" >&2
  exit 1
fi
if [ "$failed" -ne 0 ]; then
  echo "::error::design-record selftest: $failed of $declared case(s) did not bite - the design-record check is not trustworthy" >&2
  exit 1
fi
echo "design-record selftest: $pass/$declared cases bite"
