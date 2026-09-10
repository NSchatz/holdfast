#!/usr/bin/env bash
# The DETERMINISM gate for the rendered dashboard graders (S0069).
#
# A grader that returns a different verdict on identical bytes cannot carry an acceptance
# criterion: it reports how busy the machine was. This target runs the rendered graders
# more than once against ONE unchanged working tree and fails if any repetition disagrees
# with any other - not merely if one of them was red, which `webui-check` already decides,
# but if two of them decided the same bytes differently.
#
# What it can and cannot say, stated rather than implied. Agreeing repetitions cannot
# PROVE determinism; they can only fail to disprove it. The criteria that remove the
# mechanism live in the graders themselves - the latency case that holds a reading back
# for two minutes and the one that holds a snapshot back past any budget this harness
# carries - and they are worth more than this loop. This is the cheap, repeatable
# disconfirmation that runs beside them, and it is what catches a NEW clock dependence
# somebody introduces later without having read any of that.
#
# WHAT THIS LOOP DOES NOT COVER, stated rather than left to be discovered. The selector is
# TestRendered_, so the loop is the Go graders that read the page. The Playwright half
# (TestPlaywright_, the graders that need the engine OPERATED) is NOT repeated here and is
# not asked whether it agrees with itself. That is a deliberate scope, not an oversight: the
# subject of this work is the rendered graders, and a repetition of the Playwright project
# costs minutes per pass against a gate that has a declared timeout to stay inside. It is
# also a real gap - a Playwright case has been seen to time out under heavy host contention
# and pass on the same bytes on an idle machine - so a reader chasing a disagreement that
# this loop reports nothing about should look there next, with `make webui-check`.
#
# HOLDFAST_WEBUI_REPEAT sets the repetition count (default 3, minimum 2). The count is
# PRINTED, because "it was repeated" is a claim a reader has to be able to check.
set -euo pipefail

repeat="${HOLDFAST_WEBUI_REPEAT:-3}"
case "$repeat" in
  ''|*[!0-9]*) echo "webui-repeat-check: HOLDFAST_WEBUI_REPEAT must be a whole number, got '$repeat'" >&2; exit 2 ;;
esac
if [ "$repeat" -lt 2 ]; then
  echo "webui-repeat-check: a repetition count of $repeat cannot disagree with itself; the minimum is 2" >&2
  exit 2
fi

run="./internal/webui/"
sel="TestRendered_"
work="$(mktemp -d -t webui-repeat.XXXXXX)"
trap 'rm -rf "$work"' EXIT

echo "webui-repeat-check: running $sel* in $run $repeat times against one unchanged tree"
echo "webui-repeat-check: repetitions=$repeat"
echo "webui-repeat-check: scope is $sel* only; the Playwright half (TestPlaywright_) is outside this loop - see 'make webui-check'"

# The tree must not move under the graders, or a disagreement below would be a fact about
# the edit and not about the harness. Recorded before the first repetition and checked
# after the last.
tree_before="$(git -C . status --porcelain --untracked-files=no 2>/dev/null | sha256sum || echo 'not a git tree')"

status=0
for i in $(seq 1 "$repeat"); do
  echo "webui-repeat-check: repetition $i of $repeat"
  # -count=1 defeats the test cache: a cached result is not a repetition.
  # A failing repetition is RECORDED and the loop goes on, because "run 1 passed and run 3
  # failed" is the finding, and stopping at the first red would hide which runs disagreed.
  if HOLDFAST_WEBUI_REQUIRED=1 go test -v -count=1 -timeout 30m -run "$sel" "$run" >"$work/run-$i.log" 2>&1; then
    echo "webui-repeat-check: repetition $i passed"
  else
    status=1
    echo "webui-repeat-check: repetition $i FAILED"
  fi
  # The per-test verdicts, sorted, are what the repetitions are compared on. A run that
  # reported nothing at all leaves an empty file, which disagrees with every other run.
  grep -E '^(    )*--- (PASS|FAIL|SKIP): ' "$work/run-$i.log" \
    | sed -E 's/^ *--- (PASS|FAIL|SKIP): ([^ ]+).*$/\1 \2/' \
    | sort >"$work/verdicts-$i" || true
done

tree_after="$(git -C . status --porcelain --untracked-files=no 2>/dev/null | sha256sum || echo 'not a git tree')"
if [ "$tree_before" != "$tree_after" ]; then
  echo "webui-repeat-check: FAILED - the working tree changed while the repetitions ran, so they did not grade one tree" >&2
  exit 1
fi

# Nothing may skip: a skipped grader agrees with every other skipped grader and measures
# nothing, which is the false green required mode exists to refuse.
for i in $(seq 1 "$repeat"); do
  if grep -q '^SKIP ' "$work/verdicts-$i"; then
    echo "webui-repeat-check: FAILED - repetition $i reported a SKIP under required mode:" >&2
    grep '^SKIP ' "$work/verdicts-$i" >&2
    exit 1
  fi
  if [ ! -s "$work/verdicts-$i" ]; then
    echo "webui-repeat-check: FAILED - repetition $i decided nothing (no test verdict in its output)" >&2
    tail -40 "$work/run-$i.log" >&2
    exit 1
  fi
done

# THE question this target exists to ask: did two repetitions decide the same bytes
# differently? Compared per grader, so the report names the grader that disagreed rather
# than saying only that something did.
disagreed=0
for i in $(seq 2 "$repeat"); do
  if ! diff -u "$work/verdicts-1" "$work/verdicts-$i" >"$work/diff-$i"; then
    disagreed=1
    echo "webui-repeat-check: FAILED - repetition $i disagrees with repetition 1 on the SAME tree:" >&2
    sed -n '3,$p' "$work/diff-$i" >&2
  fi
done

if [ "$disagreed" -ne 0 ]; then
  echo "webui-repeat-check: FAILED - the rendered graders are not deterministic over $repeat repetitions" >&2
  exit 1
fi

if [ "$status" -ne 0 ]; then
  echo "webui-repeat-check: FAILED - every repetition agreed, and they agreed on a FAILURE" >&2
  for i in $(seq 1 "$repeat"); do
    grep -E '^(    )*--- FAIL: ' "$work/run-$i.log" >&2 || true
  done
  exit 1
fi

echo "webui-repeat-check: OK - $repeat repetitions, $(wc -l <"$work/verdicts-1") graders, every repetition agreed and all passed"
