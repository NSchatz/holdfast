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
#   AC-11  every path HP-RUN is aimed at must be one the grader created. holdfast layers
#          HOLDFAST_* OVER the config file, so an ambient HOLDFAST_LIBRARY_ROOTS or
#          HOLDFAST_STATE_DIR must NOT reach the run, and a library root outside the
#          directory created for the run must be REFUSED BEFORE the child is spawned:
#          the encode, the swap and the deletion are real and a red afterwards undoes
#          none of them. A decoy library stands where an operator's would, and each of
#          these cases requires it byte-identical after the run.
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

declared=12
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

# case_env is the environment a case adds to the grader's run, and case_watch is a file
# the case requires BYTE-IDENTICAL across it. Both are per case: reset clears them, so a
# case that forgets to set one cannot inherit the previous case's.
case_env=(); case_watch=""

# reset puts every file a case can mutate back to what the working tree holds, so each
# case starts from the tree `make check` grades and no mutation stacks on another.
reset() {
  for f in "${touched[@]}"; do
    cp "$work/pristine.$(echo "$f" | tr / .)" "$repo/$f"
  done
  case_env=(); case_watch=""
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
  out="$( cd "$repo" && env ${case_env[@]+"${case_env[@]}"} go test -count=1 -v \
            -run '^TestHappyPathRunEmitsNoErrorRecords$' ./cmd/holdfast/ 2>&1 )"
}

# expect <want-exit> <criterion> <name> [must-mention-regex...]
# The output is CAPTURED, not discarded: a grader with more than one reason to exit
# non-zero makes "it exited 1" stop being evidence that the case under test is the one
# that bit, and D6 puts each reason in its own words precisely so this can be checked.
expect() {
  local want="$1" criterion="$2" name="$3"; shift 3
  local got=0 watched_before="" watched_after=""
  # A case that watches a file watches it across the run itself: a grader that destroys
  # an operator's media and then reds has still destroyed it, so the bytes are the
  # observation and the exit status is not.
  [ -z "$case_watch" ] || watched_before="$(cksum "$case_watch" 2>/dev/null || echo ABSENT)"
  set +e
  run_grader
  got=$?
  set -e
  local why=""
  if [ -n "$case_watch" ]; then
    watched_after="$(cksum "$case_watch" 2>/dev/null || echo ABSENT)"
    [ "$watched_before" = "$watched_after" ] \
      || why="the file at $case_watch was REWRITTEN by the run [$watched_before] -> [$watched_after]"
  fi
  if [ "$got" -ne "$want" ]; then
    local status_why="the grader exited $got, wanted $want"
    [ "$want" -eq 0 ] || [ "$got" -ne 0 ] \
      || status_why="the grader CAME BACK GREEN (exit $got) where it had to bite"
    why="${why:+$why; }$status_why"
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

echo "happy-path log grader selftest: defeating AC-2, AC-4, AC-5, AC-6, AC-7, AC-8, AC-11 and AC-14 on purpose"
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

# --- AC-11: the decoy an ambient HOLDFAST_* would aim HP-RUN at -------------------------
# An operator's library, standing where the machine that runs the gate keeps its own: a
# real H.264 file above the shipped bitrate floor, so the pipeline would take it if it
# ever saw it. Nothing below may change a byte of it.
ffmpeg_bin="${HOLDFAST_FFMPEG:-ffmpeg}"
command -v "$ffmpeg_bin" >/dev/null 2>&1 || {
  echo "::error::selftest: $ffmpeg_bin is not on PATH, so the AC-11 decoy cannot be built and those cases did NOT run" >&2
  exit 1
}
decoy_lib="$work/operator-library"; decoy_state="$work/operator-state"
mkdir -p "$decoy_lib" "$decoy_state"
decoy="$decoy_lib/irreplaceable.mkv"
"$ffmpeg_bin" -hide_banner -loglevel error -y -f lavfi \
  -i "testsrc2=duration=2:size=320x240:rate=10" \
  -c:v libx264 -preset ultrafast -b:v 8M -maxrate 8M -bufsize 8M -x264-params nal-hrd=cbr \
  -pix_fmt yuv420p -- "$decoy" || {
  echo "::error::selftest: could not build the AC-11 decoy library - those cases did NOT run" >&2
  exit 1
}

# --- 9. AC-11: an ambient HOLDFAST_* is REMOVED, and the run is unaffected by it ---------
# The unmutated grader, started by a shell that exports the two path keys at the decoy.
# It must run its own happy path to completion anyway, say which variables it removed,
# and leave the decoy alone.
reset
case_env=("HOLDFAST_LIBRARY_ROOTS=$decoy_lib" "HOLDFAST_STATE_DIR=$decoy_state")
case_watch="$decoy"
expect 0 "AC-11" "REQUIRED GREEN - an ambient HOLDFAST_LIBRARY_ROOTS cannot aim HP-RUN at a library the grader did not create" \
  "HP-RUN environment:" "HOLDFAST_LIBRARY_ROOTS" "and nothing else" "error=0"

# --- 10. AC-11: with the isolation removed, the run is REFUSED before the child exists ---
# The defeat: the grader stops emptying its own environment, so the ambient variables
# reach the configuration HP-RUN would run under. The refusal has to come BEFORE the
# child is spawned - a grader that noticed afterwards would have noticed a deletion.
reset
mutate "$grader" 's|removed := hpIsolateFromAmbientConfig(t)|removed := []string{}|' \
  "a grader that inherits the ambient environment"
case_env=("HOLDFAST_LIBRARY_ROOTS=$decoy_lib" "HOLDFAST_STATE_DIR=$decoy_state")
case_watch="$decoy"
expect 1 "AC-11" "DEFEATED - an inherited HOLDFAST_* is caught BEFORE the child is spawned, not after the delete" \
  "HAPPY-PATH LOG GRADER COULD NOT RUN" "HOLDFAST_LIBRARY_ROOTS" \
  "REFUSED BEFORE the child was spawned"

# --- 11. AC-11: a library root the grader did not create is REFUSED ----------------------
# The other half of the same clause, with no environment involved: the fixture builder
# is pointed at the decoy library, so the configuration names a path this run did not
# make. The paths are checked against the directory created for the run, not merely
# against each other.
reset
mutate "$grader" "s|libRoot = filepath.Join(dir, \"library\")|libRoot = \"$decoy_lib\"|" \
  "a library root the grader did not create"
case_watch="$decoy"
expect 1 "AC-11" "DEFEATED - a library root outside the directory created for this run is REFUSED" \
  "HAPPY-PATH LOG GRADER COULD NOT RUN" "$decoy_lib" "is not inside" \
  "REFUSED BEFORE the child was spawned"

# --- 12. the tree the grader grades is never rewritten ----------------------------------
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
echo "happy-path log selftest: $pass/$declared cases bite; defeated on purpose: AC-2 AC-4 AC-5 AC-6 AC-7 AC-8 AC-11 AC-14"
