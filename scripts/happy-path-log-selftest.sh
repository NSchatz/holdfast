#!/usr/bin/env bash
# Prove the happy-path log grader still BITES (S0130). NOT part of `make check`: it
# MUTATES a copy of the tree, and a grader that graded a tree this script had written
# would be grading its own mutations. CI runs it as its own step beside the gate.
#
# cmd/holdfast/happy_path_log_test.go asserts that ONE completed oneshot run - HP-RUN -
# emits no record at `error` level (observability O3). Every way that assertion can be
# wrong is a way it goes green:
#
#   AC-2   a handled condition HP-RUN actually reaches, logged at `error`, must be FOUND
#          and reported verbatim. A grader that missed it is the whole defect this item
#          exists to prevent.
#   AC-4   a capture holding no record at any level must FAIL. A run that was quiet is
#          never a pass, and "no error records" is true of an empty file.
#   AC-5   a captured line that is not a levelled record must FAIL quoting it, never be
#          skipped and never be counted as not-an-error.
#   AC-6   a tool HP-RUN needs, made unavailable, must fail LOUD and say the grader COULD
#          NOT RUN. A skipping test exits zero.
#   AC-7   a run that cannot reach transcode must FAIL saying the happy path was not
#          reached, and must not report the error-record assertion as satisfied.
#   AC-8   a capture that cannot be read back must FAIL naming the path and the operation,
#          distinctly from AC-4.
#   AC-14  an ADDITIONAL `warn` record must stay GREEN and must show up in the per-level
#          tally. Without this case, AC-12 is satisfied by a grader that never met a warn.
#
# Each is its OWN case against a mutated COPY, each mutation is proved to have landed
# before the grader is run, and the run is reported against the number of cases DECLARED
# rather than the number that executed - a defeat that came back green and a defeat that
# never ran are indistinguishable by exit status, and the second is the failure mode this
# whole item is about.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)" || { echo "::error::selftest: mktemp failed" >&2; exit 1; }
trap 'rm -rf "$work"' EXIT

declared=9
pass=0; failed=0
repo="$work/repo"

grader="cmd/holdfast/happy_path_log_test.go"
cli="cmd/holdfast/main.go"
cfg="internal/config/config.go"
log="internal/logging/logging.go"
touched=("$grader" "$cli" "$cfg" "$log")

# What the graded tree looks like BEFORE anything runs. The last case compares it again.
before="$(cd "$here" && cksum "${touched[@]}" 2>/dev/null || true)"

# --- the copy every mutation happens in ----------------------------------------------
# A COPY of the working tree, not of HEAD: an uncommitted change to the grader - a fix OR
# a break - must be what is graded, and locally is where that mistake gets made.
mkdir -p "$repo"
tar -C "$here" --exclude=./.git --exclude=./holdfast -cf - . | tar -C "$repo" -xf - \
  || { echo "::error::selftest: could not copy the working tree - it did NOT run" >&2; exit 1; }
for f in "${touched[@]}"; do
  cmp -s "$here/$f" "$repo/$f" \
    || { echo "::error::selftest: the copy's $f is not the working-tree $f - it graded the wrong thing" >&2; exit 1; }
  cp "$repo/$f" "$work/pristine.$(echo "$f" | tr / .)"
done

# reset puts every file a case can mutate back to what the working tree holds, so each
# case starts from the tree `make check` grades and no mutation stacks on another.
reset() {
  for f in "${touched[@]}"; do
    cp "$work/pristine.$(echo "$f" | tr / .)" "$repo/$f"
  done
}

# mutate <file> <sed-expression> <what> - applies the edit and PROVES it landed. A sed
# that matched nothing leaves a green case that graded an unmutated tree, which is the
# silent-pass this script exists to refuse.
mutate() {
  local f="$1" expr="$2" what="$3"
  sed -i "$expr" "$repo/$f"
  if cmp -s "$work/pristine.$(echo "$f" | tr / .)" "$repo/$f"; then
    echo "::error::selftest: the mutation for '$what' changed nothing in $f - the case would have proved nothing" >&2
    exit 1
  fi
}

out=""
run_grader() {
  out="$( cd "$repo" && go test -count=1 -v \
            -run '^TestHappyPathRunEmitsNoErrorRecords$' ./cmd/holdfast/ 2>&1 )"
}

# expect <want-exit> <criterion> <name> [must-mention-regex...]
# The output is CAPTURED, not discarded: a grader with more than one reason to exit
# non-zero makes "it exited 1" stop being evidence that the case under test is the one
# that bit, and D6 puts each reason in its own words precisely so this can be checked.
expect() {
  local want="$1" criterion="$2" name="$3"; shift 3
  local got=0
  set +e
  run_grader
  got=$?
  set -e
  local why=""
  if [ "$got" -ne "$want" ]; then
    if [ "$want" -ne 0 ]; then
      why="the grader CAME BACK GREEN (exit $got) where it had to bite"
      [ "$got" -eq 0 ] || why="the grader exited $got, wanted $want"
    else
      why="the grader exited $got, wanted $want"
    fi
  fi
  local missing=""
  for re in "$@"; do
    printf '%s' "$out" | grep -qF -- "$re" || missing="$missing [$re]"
  done
  [ -z "$missing" ] || why="$why; output never names$missing"
  if [ -z "$why" ]; then
    printf '  ok  %s: %s\n' "$criterion" "$name"
    pass=$((pass + 1))
  else
    printf '::error::selftest %s (%s): %s\n' "$criterion" "$name" "$why" >&2
    printf '%s\n' "$out" | sed 's/^/        /' >&2
    failed=$((failed + 1))
  fi
}

# tally_warn reads the `warn=` count out of the per-level tally the grader printed. An
# absent tally reads as "" and fails the comparison rather than as zero.
tally_warn() {
  printf '%s' "$out" | sed -n 's/^HP-RUN per-level tally:.*[[:space:]]warn=\([0-9]\{1,\}\).*$/\1/p' | head -1
}

echo "happy-path log grader selftest: defeating AC-2, AC-4, AC-5, AC-6, AC-7, AC-8 and AC-14 on purpose"
echo

# --- 1. the unmutated copy passes, or every case below is graded against a red tree ---
# It also supplies the BASELINE warn count AC-14 measures against: an extra warn record
# has to move a number, and a number nothing was measured against moves nothing.
reset
expect 0 "AC-2" "BASELINE (required GREEN) - an unmutated tree emits no record at error level" \
  "HP-RUN per-level tally:" "error=0"
baseline_warn="$(tally_warn)"
if [ -z "$baseline_warn" ]; then
  echo "::error::selftest: the unmutated run printed no per-level tally - AC-14 cannot be measured" >&2
  exit 1
fi
printf '      baseline per-level tally carries warn=%s\n' "$baseline_warn"

# --- 2. AC-2: a handled condition HP-RUN reaches, logged at error ----------------------
# The undo-window notice: a shipped default, logged at WARN on purpose, and exactly the
# kind of handled condition O3 says must never be an `error`.
reset
mutate "$cli" 's/^\t\tlog\.Warn(n)$/\t\tlog.Error(n)/' "a handled condition logged at error"
expect 1 "AC-2" "DEFEATED - a handled condition HP-RUN reaches, logged at error, is FOUND" \
  "HP-RUN RAN AND EMITTED ERROR RECORD(S)" "level=ERROR" "undo_window_hours is 0"

# --- 3. AC-4: a capture with no record at any level ------------------------------------
# The process is silenced above `error`, so it completes the happy path and says nothing.
reset
mutate "$log" 's/^\t\treturn slog\.LevelInfo$/\t\treturn slog.LevelError + 4/' "a silenced logger"
expect 1 "AC-4" "DEFEATED - a capture holding no record at ANY level is a FAILURE, not a clean run" \
  "HP-RUN RAN AND THE PROCESS EMITTED NOTHING" "no record at ANY level"

# --- 4. AC-5: a captured line that is not a levelled record -----------------------------
reset
mutate "$cli" 's|^\tlog\.Info("scan complete")$|\tfmt.Fprintln(os.Stderr, "scan complete")|' \
  "a raw line on the captured stream"
expect 1 "AC-5" "DEFEATED - a line that is not a levelled record is quoted, never skipped" \
  "HP-RUN RAN AND EMITTED A LINE THAT IS NOT A LEVELLED RECORD" "scan complete"

# --- 5. AC-6: a tool HP-RUN needs, made unavailable -------------------------------------
reset
mutate "$grader" 's|envOr("HOLDFAST_FFMPEG", "ffmpeg")|"ffmpeg-made-absent-by-the-defeat-runner"|' \
  "an absent ffmpeg"
expect 1 "AC-6" "DEFEATED - an absent tool fails LOUD and says the grader COULD NOT RUN" \
  "HAPPY-PATH LOG GRADER COULD NOT RUN" "ffmpeg-made-absent-by-the-defeat-runner" "NEVER skips"

# --- 6. AC-7: a run that cannot reach transcode -----------------------------------------
# The bitrate floor is raised past the fixture, so every file is skipped and the run
# completes, exit zero, having transcoded nothing. AC-2 must be UNDECIDED, not satisfied.
reset
mutate "$cfg" 's/^\t\t"min_bitrate_kbps":       2500,$/\t\t"min_bitrate_kbps":       2500000,/' \
  "a bitrate floor nothing clears"
expect 1 "AC-7" "DEFEATED - a run that never reaches transcode does not satisfy the error assertion" \
  "HP-RUN RAN AND DID NOT REACH THE HAPPY PATH" "REACHED-TRANSCODE does not hold" \
  "AC-2 is UNDECIDED, not satisfied"

# --- 7. AC-8: a capture that cannot be read back ----------------------------------------
reset
mutate "$grader" 's|os\.ReadFile(capturePath)|os.ReadFile(capturePath + ".absent")|' \
  "a capture that cannot be read back"
expect 1 "AC-8" "DEFEATED - a capture that cannot be read back names the path and the operation" \
  "HAPPY-PATH LOG GRADER COULD NOT RUN" "could not READ BACK the capture file"

# --- 8. AC-14: an ADDITIONAL warn record must stay GREEN and must show in the tally -----
# AC-9's cases are all required RED, so the case that must stay GREEN cannot live among
# them. Without it, AC-12's "never fails on warn" is satisfied by a grader that never met
# one.
reset
mutate "$cli" 's|^\tlog\.Info("scan complete")$|\tlog.Warn("an EXTRA warn record, injected by the happy-path log selftest"); log.Info("scan complete")|' \
  "an additional warn record"
want_warn=$((baseline_warn + 1))
expect 0 "AC-14" "REQUIRED GREEN - an additional warn record stays green and shows in the per-level tally" \
  "HP-RUN per-level tally:" "warn=$want_warn" "error=0"

# --- 9. the tree the grader grades is never rewritten -----------------------------------
after="$(cd "$here" && cksum "${touched[@]}" 2>/dev/null || true)"
if [ "$before" = "$after" ] && [ -n "$before" ]; then
  printf '  ok  the graded tree was never written: %s are byte-identical\n' "${touched[*]}"
  pass=$((pass + 1))
else
  printf '::error::selftest: the graded tree CHANGED. Mutations must happen in the copy alone.\n' >&2
  printf '        before: %s\n        after:  %s\n' "$before" "$after" >&2
  failed=$((failed + 1))
fi

echo
# Report against the number of cases DECLARED, not the number that ran: "$pass/$pass" is
# N/N by construction and could never show a shortfall.
total=$((pass + failed))
if [ "$total" -ne "$declared" ]; then
  echo "::error::happy-path log selftest: ran $total case(s), expected $declared - a defeat did NOT execute" >&2
  exit 1
fi
if [ "$failed" -ne 0 ]; then
  echo "::error::happy-path log selftest: $failed of $declared case(s) did not bite - the happy-path log grader is not trustworthy" >&2
  exit 1
fi
echo "happy-path log selftest: $pass/$declared cases bite; defeated on purpose: AC-2 AC-4 AC-5 AC-6 AC-7 AC-8 AC-14"
