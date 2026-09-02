#!/usr/bin/env bash
# Prove the govulncheck gate still BITES. Part of `make check`.
#
# The gate's whole job is to be RED when it should be, and every way it can fail is a way
# it fails SILENTLY - by printing "ok". A suppression file that is skipped because it is
# malformed, a stale entry that keeps covering an advisory nobody has re-checked, an
# empty report from a scanner that crashed, a `latest` tool pin: each of those turns a
# vulnerability gate into a green light with no error anywhere. So defeat it on purpose,
# on every run.
#
# Cases 0-16 drive the decision layer (scripts/govulncheck-gate) with synthetic reports,
# because the mutations that matter are report/record shapes and manufacturing those from
# a real scan is not possible. Cases 17-19 drive the real scripts/govulncheck.sh, which is
# where the tool is run and its exit status judged. Case 20 asserts the step is still
# wired into `make check` at all - a gate detached from the target nobody notices.
#
# Runs entirely in a throwaway directory; it never mutates the working tree.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)" || { echo "::error::selftest: mktemp failed" >&2; exit 1; }
trap 'rm -rf "$work"' EXIT

declared=21
pass=0; failed=0

gate="$work/gate"
( cd "$here" && go build -o "$gate" ./scripts/govulncheck-gate ) \
  || { echo "::error::selftest: could not build the gate - it did NOT run" >&2; exit 1; }

# --- fixtures ------------------------------------------------------------------------
# A govulncheck JSON report is a stream of objects. `config` is the one every real run
# emits first; findings carry the advisory, the fixed version (empty = no fix exists) and
# the trace govulncheck established.
CONFIG='{"config":{"protocol_version":"v1.0.0","scanner_name":"govulncheck","scanner_version":"v1.1.4"}}'

newcase() {  # newcase <name> -> prints the case dir, containing a clean (findings-free) report
  local dir="$work/case-$1"
  rm -rf "$dir"; mkdir -p "$dir"
  printf '%s\n' "$CONFIG" > "$dir/report.json"
  printf '%s' "$dir"
}

finding() {  # finding <dir> <osv-id> <fixed-version, empty for none>
  local dir="$1" id="$2" fixed="$3"
  printf '{"osv":{"id":"%s","summary":"synthetic advisory %s"}}\n' "$id" "$id" >> "$dir/report.json"
  printf '{"finding":{"osv":"%s","fixed_version":"%s","trace":[{"module":"example.test/m","version":"v1.0.0","package":"example.test/m/p","function":"Vulnerable"}]}}\n' \
    "$id" "$fixed" >> "$dir/report.json"
}

entry() {  # entry <dir> <id> <reason> <reachability> <recorded>
  printf -- '- id: "%s"\n  reason: "%s"\n  reachability: "%s"\n  recorded: "%s"\n' \
    "$2" "$3" "$4" "$5" >> "$1/.govulncheck-suppressions.yaml"
}

# expect <want-exit> <case-dir> <must-mention|-> <name>.  0 = must pass, 1 = must bite.
expect() {
  local want="$1" dir="$2" mention="$3" name="$4" got=0 out
  out="$( cd "$dir" && "$gate" < "$dir/report.json" 2>&1 )" || got=$?
  if [ "$got" -ne "$want" ]; then
    printf '::error::selftest: %s - gate exited %s, wanted %s\n%s\n' "$name" "$got" "$want" "$out" >&2
    failed=$((failed + 1)); return
  fi
  if [ "$mention" != "-" ] && ! printf '%s' "$out" | grep -qF -- "$mention"; then
    printf '::error::selftest: %s - gate gave the right verdict but never named %s, so the message does not say what to fix:\n%s\n' \
      "$name" "$mention" "$out" >&2
    failed=$((failed + 1)); return
  fi
  printf '  ok: %s\n' "$name"; pass=$((pass + 1))
}

# --- 0. A clean report with no suppression file PASSES. Without this, every "bites" case
#        below could be a gate that simply fails on everything, which would prove nothing.
d="$(newcase clean)"
expect 0 "$d" - "a clean report with no suppression file passes"

# --- 1. THE case the whole gate exists for: an advisory nobody has answered is RED, and
#        the message names it. Note the fixed version is empty - this one has nowhere to go.
d="$(newcase unrecorded)"; finding "$d" GO-2026-9001 ""
expect 1 "$d" GO-2026-9001 "an unrecorded advisory is caught, naming the id"

# --- 2. The same advisory, recorded with all four keys, passes. This is the other half of
#        case 1: without it, the gate could be one that never accepts a record at all.
d="$(newcase recorded)"; finding "$d" GO-2026-9001 ""
entry "$d" GO-2026-9001 "upstream has published no fixed release" "example.test/m/p.Vulnerable; unreachable here" "2026-09-02"
expect 0 "$d" - "an advisory with no fix, recorded with all four keys, passes"

# --- 3. STALE: a record outliving the advisory it was written for. Left alone it would sit
#        there covering a future advisory that reuses nothing and explains nothing.
d="$(newcase stale)"
entry "$d" GO-2026-9002 "no fixed release" "not reachable" "2026-09-02"
expect 1 "$d" GO-2026-9002 "a record the report no longer names is caught as stale, naming the id"

# --- 4. MALFORMED, missing a key. A record that does not say why, or what it looked at, is
#        an ignore wearing a record's clothes. It must never be silently honoured...
d="$(newcase missingkey)"; finding "$d" GO-2026-9001 ""
printf -- '- id: "GO-2026-9001"\n  reason: "no fixed release"\n  recorded: "2026-09-02"\n' \
  > "$d/.govulncheck-suppressions.yaml"
expect 1 "$d" reachability "an entry missing a required key is caught, naming the key"

# --- 5. ...nor silently SKIPPED, which would leave the advisory unrecorded and is the same
#        bug from the other side. An unknown key means the author believed the file said
#        something it does not.
d="$(newcase unknownkey)"; finding "$d" GO-2026-9001 ""
printf -- '- id: "GO-2026-9001"\n  reason: "r"\n  reachability: "x"\n  recorded: "2026-09-02"\n  expires: "2027-01-01"\n' \
  > "$d/.govulncheck-suppressions.yaml"
expect 1 "$d" expires "an entry with an unknown key is caught, naming the key"

# --- 6. An id that is not an OSV identifier can never match a real advisory, so it would be
#        stale forever while looking like a considered decision.
d="$(newcase badid)"; finding "$d" GO-2026-9001 ""
entry "$d" "CVE-2026-1234" "no fixed release" "not reachable" "2026-09-02"
expect 1 "$d" CVE-2026-1234 "an entry whose id is not GO-YYYY-NNNN is caught, naming the entry"

# --- 7. `recorded` is what makes an entry auditable - when someone decided this, so a
#        reader can ask whether it is still true. A value that is not a date says nothing.
d="$(newcase baddate)"; finding "$d" GO-2026-9001 ""
entry "$d" GO-2026-9001 "no fixed release" "not reachable" "last tuesday"
expect 1 "$d" "last tuesday" "an entry whose recorded is not an ISO date is caught"

# --- 8. An ABSENT file means "no suppressions" (case 0) - it must not thereby become a way
#        to pass with an advisory outstanding. Deleting the file is the cheapest possible
#        way to silence a gate, so it has to be the loudest.
d="$(newcase absent-with-finding)"; finding "$d" GO-2026-9001 ""
expect 1 "$d" GO-2026-9001 "an absent suppression file is not a way to pass an unrecorded advisory"

# --- 9. An EMPTY SEQUENCE is "no suppressions", not an error. The file ships empty, so if
#        this bit, the gate would be red on a green tree.
d="$(newcase emptyseq)"; printf '[]\n' > "$d/.govulncheck-suppressions.yaml"
expect 0 "$d" - "an empty sequence is 'no suppressions', not an error"

# --- 10. Same for a file that is only comments - which is what the committed one is, once
#         its last entry is removed.
d="$(newcase commentsonly)"; printf '# nothing recorded today.\n' > "$d/.govulncheck-suppressions.yaml"
expect 0 "$d" - "a comments-only file is 'no suppressions', not an error"

# --- 11. A fix EXISTS and the advisory was recorded anyway. This is the failure that makes
#         a suppression file rot into a permanent exemption list: it is remediation deferred
#         and then forgotten, and it looks identical to a considered decision.
d="$(newcase fixavailable)"; finding "$d" GO-2026-9001 "v1.2.3"
entry "$d" GO-2026-9001 "we will get to it" "not reachable" "2026-09-02"
expect 1 "$d" v1.2.3 "a record covering an advisory that HAS a fix is caught, naming the fix"

# --- 12. Two entries for one id: the second is invisible, so whichever is wrong is never
#         read and never corrected.
d="$(newcase duplicate)"; finding "$d" GO-2026-9001 ""
entry "$d" GO-2026-9001 "first" "not reachable" "2026-09-02"
entry "$d" GO-2026-9001 "second" "not reachable" "2026-09-02"
expect 1 "$d" GO-2026-9001 "a duplicate id is caught - one entry per advisory"

# --- 13. A suppression file that is not YAML at all must be RED, never skipped. Skipping it
#         would silently un-record every advisory it covers.
d="$(newcase badyaml)"; finding "$d" GO-2026-9001 ""
printf -- '- id: "GO-2026-9001"\n   reason: bad indent\n  reachability: x\n' > "$d/.govulncheck-suppressions.yaml"
expect 1 "$d" .govulncheck-suppressions.yaml "an unparseable suppression file is caught, not skipped"

# --- 14. The file is a SEQUENCE of entries. A mapping at the root is the natural typo, and
#         a lenient reader would find no entries in it and report "no suppressions".
d="$(newcase notaseq)"; finding "$d" GO-2026-9001 ""
printf 'GO-2026-9001:\n  reason: "no fixed release"\n' > "$d/.govulncheck-suppressions.yaml"
expect 1 "$d" sequence "a suppression file that is not a sequence is caught"

# --- 15. An EMPTY report is what a crashed, killed or misinvoked scanner leaves behind, and
#         reading it as "found nothing" is the exact failure this gate exists to prevent.
d="$(newcase emptyreport)"; : > "$d/report.json"
expect 1 "$d" "did not run" "an empty report is caught, not read as a clean scan"

# --- 16. A report that is valid JSON but carries no govulncheck `config` object is not a
#         govulncheck report - a truncated stream, or some other tool's output.
d="$(newcase noconfig)"; printf '{"progress":{"message":"scanning"}}\n' > "$d/report.json"
expect 1 "$d" config "a report with no govulncheck config object is caught"

# --- The end-to-end script: where the tool is actually run and its status judged. --------

script() {  # script <name> <must-mention> <extra-PATH-or-> <args…>
  local name="$1" mention="$2" path="$3"; shift 3
  local got=0 out
  if [ "$path" = "-" ]; then
    out="$( cd "$here" && bash scripts/govulncheck.sh "$@" 2>&1 )" || got=$?
  else
    out="$( cd "$here" && PATH="$path:$PATH" bash scripts/govulncheck.sh "$@" 2>&1 )" || got=$?
  fi
  if [ "$got" -eq 0 ]; then
    printf '::error::selftest: %s - the script reported GREEN\n%s\n' "$name" "$out" >&2
    failed=$((failed + 1)); return
  fi
  if ! printf '%s' "$out" | grep -qF -- "$mention"; then
    printf '::error::selftest: %s - red, but never said %s:\n%s\n' "$name" "$mention" "$out" >&2
    failed=$((failed + 1)); return
  fi
  printf '  ok: %s\n' "$name"; pass=$((pass + 1))
}

# --- 17. govulncheck itself failing - no network, no module proxy, a tool that will not
#         build. The status must not be swallowed: a scanner that did not run has not
#         reported a clean scan, and this is the one failure that looks like success.
fake="$work/fakebin"; mkdir -p "$fake"
real_go="$(command -v go)" || { echo "::error::selftest: no go on PATH" >&2; exit 1; }
printf '#!/bin/sh\ncase "$*" in\n  *govulncheck@*) echo "go: module lookup disabled by GOPROXY=off" >&2; exit 1 ;;\nesac\nexec %s "$@"\n' \
  "$real_go" > "$fake/go"
chmod +x "$fake/go"
script "a govulncheck that could not run is RED, not a silent pass" "did NOT run" "$fake" "v1.1.4"

# --- 18. A floating tool pin changes what the gate IS without changing a line of it.
script "a floating version pin is refused" "not a concrete pin" - "latest"

# --- 19. No pin at all would let the script pick one, which puts the pin somewhere other
#         than the Makefile that is supposed to own it.
script "a missing version argument is refused" "no version given" -

# --- 20. And the step has to still be IN the gate. A target that nothing depends on is a
#         gate that runs nowhere, and nothing else in this file would notice.
prereqs=" $(sed -n 's/^check:[[:space:]]*//p' "$here/Makefile" | head -1) "
missing=""
case "$prereqs" in *" govulncheck "*) ;; *) missing="govulncheck" ;; esac
case "$prereqs" in *" govulncheck-selftest "*) ;; *) missing="$missing govulncheck-selftest" ;; esac
if [ -z "$missing" ]; then
  printf '  ok: the check target still depends on govulncheck and this selftest\n'; pass=$((pass + 1))
else
  printf "::error::selftest: the \`check\` target no longer depends on:%s - the gate is detached\n" "$missing" >&2
  failed=$((failed + 1))
fi

echo
# Report against the number of cases DECLARED, not the number that ran: "$pass/$pass" is
# N/N by construction and could never show a shortfall.
total=$((pass + failed))
if [ "$total" -ne "$declared" ]; then
  echo "::error::govulncheck selftest: ran $total case(s), expected $declared - a case did not execute" >&2
  exit 1
fi
if [ "$failed" -ne 0 ]; then
  echo "::error::govulncheck selftest: $failed of $declared case(s) did not bite - the vulnerability gate is not trustworthy" >&2
  exit 1
fi
echo "govulncheck selftest: $pass/$declared cases bite"
