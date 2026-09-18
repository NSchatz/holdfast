#!/usr/bin/env bash
# Prove the mutation gate still BITES. NOT part of `make check`: it writes to a copy of
# this repository and it drives the real runner, and a gate that graded a tree its own
# self-test had written would be grading the mutations.
#
# A mutation gate is the guard whose failure is most completely invisible, because every
# way it breaks produces the same word as a healthy one: "passed".
#
#   AC-1  a diff-scoped run mutates the files inside the DOMAIN that the change touched,
#         and nothing else, and it writes a report naming them and the score.
#   AC-2  a report below the floor exits non-zero naming the measured score AND the floor,
#         in both modes. A report at the floor passes: the floor is a floor.
#   AC-3  a change touching no Go file in the domain reports that and exits ZERO, without
#         reaching for the runner. Nothing to mutate is not a score of zero.
#   AC-4  the workflow's own planning shell decides which run happens; flipping it is red.
#   AC-5  the scheduled run's notification is driven against a stubbed issues API by
#         scripts/mutation-gate/notify_test.go, which this target RUNS - the criterion's
#         grade route has to decide the criterion - and the shape of the step that fires it,
#         including the guard that decides whether it fires at all, is graded here.
#   AC-6  a runner that cannot be obtained, or cannot execute, exits non-zero naming the
#         module path, the pin and the command - and reports NO number in place of a score.
#   AC-8  the floor and the exclusion list in .gremlins.yaml and docs/mutation-testing.md
#         must agree; either one drifting is red, and the message names both files.
#
# EVERY MUTATION HAPPENS INSIDE A THROWAWAY CLONE. The tree `make check` grades is read
# and never written, and the last case asserts exactly that.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)" || { echo "::error::mutation selftest: mktemp failed" >&2; exit 1; }
trap 'rm -rf "$work"' EXIT

declared=24
pass=0; failed=0
repo="$work/repo"
out=""

# What the graded tree looks like BEFORE anything runs. The last case compares it again.
before="$(cksum "$here/.gremlins.yaml" "$here/docs/mutation-testing.md" "$here/.github/workflows/mutation.yml" 2>/dev/null || true)"

git clone -q --no-hardlinks --depth 1 "file://$here" "$repo" 2>/dev/null \
  || { echo "::error::mutation selftest could not clone the repo - it did NOT run" >&2; exit 1; }

# Grade the tree AS IT STANDS NOW: the clone carries HEAD, so the working tree is overlaid
# on top of it. Without this an uncommitted change - a fix OR a break - would be graded as
# if it did not exist, and locally is where that mistake gets made.
tar -C "$here" --exclude=.git --exclude=node_modules -cf - . | tar -C "$repo" -xf - \
  || { echo "::error::mutation selftest: could not overlay the working tree - it did NOT run" >&2; exit 1; }

cmp -s "$here/scripts/mutation-gate/main.go" "$repo/scripts/mutation-gate/main.go" \
  || { echo "::error::mutation selftest: the clone's gate is not the working-tree gate - it graded the wrong thing" >&2; exit 1; }

git -C "$repo" add -A >/dev/null 2>&1 || true
git -C "$repo" -c user.email=selftest@invalid -c user.name=selftest commit -q --no-verify -m "selftest baseline" >/dev/null 2>&1 || true
base_ref="$(git -C "$repo" rev-parse HEAD)"

gate="$work/mutation-gate"
( cd "$repo" && go build -o "$gate" ./scripts/mutation-gate ) \
  || { echo "::error::mutation selftest: the gate did not build - it did NOT run" >&2; exit 1; }

# --- helpers --------------------------------------------------------------------------

# expect <want-exit> <criterion> <name> [regex...] - runs the command in `cmd`, captures
# everything, and grades the exit code plus every pattern the output has to carry.
run_expect() {
  local want="$1" criterion="$2" name="$3"; shift 3
  local got=0
  set +e
  out="$("${cmd[@]}" 2>&1)"
  got=$?
  set -e
  local why=""
  if [ "$got" -ne "$want" ]; then
    why="exited $got, wanted $want"
    [ "$want" -eq 0 ] || why="the gate CAME BACK with $got where it had to exit $want"
  fi
  local missing=""
  for re in "$@"; do
    printf '%s' "$out" | grep -qE -- "$re" || missing="$missing [$re]"
  done
  [ -z "$missing" ] || why="$why; output never mentions$missing"
  if [ -z "$why" ]; then
    printf '  ok  %s defeated: %s\n' "$criterion" "$name"
    pass=$((pass + 1))
  else
    printf '::error::mutation selftest %s (%s): %s\n' "$criterion" "$name" "$why" >&2
    printf '%s\n' "$out" | sed 's/^/        /' >&2
    failed=$((failed + 1))
  fi
}

refute_output() {  # the output must NOT carry these patterns
  local criterion="$1" name="$2"; shift 2
  local found=""
  for re in "$@"; do
    printf '%s' "$out" | grep -qE -- "$re" && found="$found [$re]"
  done
  if [ -z "$found" ]; then
    printf '  ok  %s defeated: %s\n' "$criterion" "$name"
    pass=$((pass + 1))
  else
    printf '::error::mutation selftest %s (%s): output carried%s when it must carry no number at all\n' "$criterion" "$name" "$found" >&2
    printf '%s\n' "$out" | sed 's/^/        /' >&2
    failed=$((failed + 1))
  fi
}

# fabricate a runner report with the given killed/lived counts and efficacy.
runner_report() {  # <path> <killed> <lived> <efficacy> [not_covered]
  local path="$1" killed="$2" lived="$3" efficacy="$4" notcov="${5:-0}"
  cat > "$path" <<JSON
{"go_module":"github.com/NSchatz/holdfast","test_efficacy":$efficacy,"mutations_coverage":50.0,
 "mutants_total":$((killed + lived)),"mutants_killed":$killed,"mutants_lived":$lived,
 "mutants_not_viable":0,"mutants_not_covered":$notcov,"elapsed_time":1.0,
 "files":[{"file_name":"internal/schedule/window.go","mutations":[{"line":1,"column":1,"type":"CONDITIONALS_NEGATION","status":"LIVED"}]}]}
JSON
}

restore_clone() {  # put back anything a shape case rewrote
  git -C "$repo" checkout -q -- .gremlins.yaml docs/mutation-testing.md .github/workflows/mutation.yml
}

echo "mutation selftest: defeating the gate inside $repo"

# --- 1-4. the floor still bites, in both modes (AC-2) ---------------------------------
# 60% against a floor of 70% is the whole point of the gate. It has to name both numbers:
# a gate that says "failed" and not "62.50% against 70%" sends its reader to a log.
runner_report "$work/below.json" 6 4 60.0
cmd=("$gate" grade --root "$repo" --mode full --raw "$work/below.json" --out "$work/r.json")
run_expect 3 AC-2 "a report below the floor reds the UNSCOPED run" '60\.00%' '70\.00%' 'BELOW the floor'

cmd=("$gate" grade --root "$repo" --mode diff --ref origin/main --raw "$work/below.json" --out "$work/r.json")
run_expect 3 AC-2 "a report below the floor reds the DIFF-SCOPED run" '60\.00%' '70\.00%' 'BELOW the floor'

# Exactly at the floor is a PASS. A gate that failed here would be holding a different
# number than the one it prints, and the document would be wrong about the figure.
runner_report "$work/at.json" 7 3 70.0
cmd=("$gate" grade --root "$repo" --mode full --raw "$work/at.json" --out "$work/r.json")
run_expect 0 AC-2 "a report exactly AT the floor passes" '70\.00%'

# The floor is applied to the counts, not to the percentage the runner printed. A runner
# whose own figure stops meaning KILLED/(KILLED+LIVED) must not be able to move this gate.
runner_report "$work/lying.json" 6 4 99.0
cmd=("$gate" grade --root "$repo" --mode full --raw "$work/lying.json" --out "$work/r.json")
run_expect 5 AC-2 "a runner figure that disagrees with its own counts is refused" 'reports efficacy 99\.00%' '60\.00%'

# --- 5-6. a report that says nothing is not a pass (AC-2, AC-6) -----------------------
printf '' > "$work/empty.json"
cmd=("$gate" grade --root "$repo" --mode full --raw "$work/empty.json" --out "$work/r.json")
run_expect 5 AC-6 "an EMPTY runner report is refused, not read as clean" 'did not measure anything'

runner_report "$work/nothing.json" 0 0 0.0 40
cmd=("$gate" grade --root "$repo" --mode full --raw "$work/nothing.json" --out "$work/r.json")
run_expect 5 AC-6 "an unscoped run that produced NO runnable mutant is refused" 'produced NO runnable mutant'

# --- 7. an empty diff scope passes, and never reaches for the runner (AC-3) -----------
mkdir -p "$repo/internal/engine"
printf '\n// selftest: a change OUTSIDE the mutation domain.\n' >> "$repo/internal/engine/engine.go"
printf 'a selftest line\n' >> "$repo/README.md"
git -C "$repo" add -A >/dev/null
git -C "$repo" -c user.email=selftest@invalid -c user.name=selftest commit -q --no-verify -m "selftest: out-of-domain change" >/dev/null
rm -f "$repo/mutation-runner-report.json"
cmd=("$repo/scripts/mutation.sh" --version v0.6.0 --mode diff --ref "$base_ref" --out "$repo/mutation-report.json")
run_expect 0 AC-3 "a change touching no file in the domain passes, reporting NO MUTANT IN SCOPE" 'NO MUTANT IN SCOPE'
if [ -f "$repo/mutation-runner-report.json" ]; then
  echo "::error::mutation selftest AC-3: the runner was invoked for an empty scope" >&2
  failed=$((failed + 1))
else
  grep -q '"score": null' "$repo/mutation-report.json" \
    && grep -q '"in_scope": false' "$repo/mutation-report.json" \
    && printf '  ok  AC-3 defeated: the report carries a NULL score and in_scope false, never a zero\n' \
    && pass=$((pass + 1)) \
    || { echo "::error::mutation selftest AC-3: the report does not carry a null score" >&2; failed=$((failed + 1)); }
fi

# --- 8. a diff-scoped run mutates the changed in-domain file and nothing else (AC-1) --
# Two files change: one inside the domain and one outside it. The report has to name the
# first and not the second, and it has to carry a score.
cat > "$repo/internal/schedule/selftest_probe.go" <<'PROBE'
package schedule

// selftestProbeStep exists only inside the self-test's clone.
func selftestProbeStep(n int) int {
	if n < 3 {
		return n + 1
	}
	return n * 2
}
PROBE
cat > "$repo/internal/schedule/selftest_probe_test.go" <<'PROBETEST'
package schedule

import "testing"

func TestSelftestProbeStep(t *testing.T) {
	for _, c := range []struct{ in, want int }{{0, 1}, {2, 3}, {3, 6}, {4, 8}} {
		if got := selftestProbeStep(c.in); got != c.want {
			t.Fatalf("selftestProbeStep(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
PROBETEST
cat > "$repo/internal/store/selftest_probe.go" <<'OUTPROBE'
package store

// selftestProbeOutside is in an EXCLUDED package: the run must not mutate it.
func selftestProbeOutside(n int) int {
	if n < 3 {
		return n + 1
	}
	return n * 2
}

var _ = selftestProbeOutside
OUTPROBE
git -C "$repo" add -A >/dev/null
git -C "$repo" -c user.email=selftest@invalid -c user.name=selftest commit -q --no-verify -m "selftest: in-domain change" >/dev/null
cmd=("$repo/scripts/mutation.sh" --version v0.6.0 --mode diff --ref "$base_ref" --out "$repo/mutation-report.json")
run_expect 0 AC-1 "a diff-scoped run scores the changed in-domain file" 'internal/schedule/selftest_probe.go' 'mutation score'
if grep -q 'selftest_probe.go' "$repo/mutation-report.json" \
   && ! grep -q 'internal/store/selftest_probe.go' "$repo/mutation-report.json" \
   && ! grep -q '"score": null' "$repo/mutation-report.json"; then
  printf '  ok  AC-1 defeated: the report names the mutated file and a score, and the EXCLUDED file is absent\n'
  pass=$((pass + 1))
else
  echo "::error::mutation selftest AC-1: the report does not name exactly the in-domain changed file with a score" >&2
  sed 's/^/        /' "$repo/mutation-report.json" >&2
  failed=$((failed + 1))
fi

# --- 9. a runner that cannot be OBTAINED (AC-6) ---------------------------------------
# GOPROXY=off makes the pin unresolvable without inventing a network failure, and the
# version is one that has never existed. Both halves of O4 are asserted: which dependency
# and what was tried, and what happens next - which is stop, with NO number in its place.
cmd=(env GOPROXY=off "$repo/scripts/mutation.sh" --version v0.0.0-never-published --mode full --out "$repo/mutation-report.json")
run_expect 4 AC-6 "an unobtainable runner stops the gate" \
  'COULD NOT BE OBTAINED' 'github.com/go-gremlins/gremlins/cmd/gremlins' 'v0\.0\.0-never-published' 'command that failed' 'STOPPING'
refute_output AC-6 "an unobtainable runner reports no score, no coverage and no number" '[0-9]+\.[0-9]+%' 'mutation score [0-9]'

# --- 10. a red suite is never measured (AC-2, AC-6) -----------------------------------
# The cheapest silent green there is: a mutant is judged KILLED when the package's tests
# FAIL, so on a tree whose tests already fail every mutant is judged caught and the run
# reports a perfect score it never measured.
cat > "$repo/internal/schedule/selftest_red_test.go" <<'REDTEST'
package schedule

import "testing"

func TestSelftestRed(t *testing.T) { t.Fatal("the selftest makes this suite red on purpose") }
REDTEST
cmd=("$repo/scripts/mutation.sh" --version v0.6.0 --mode full --out "$repo/mutation-report.json")
run_expect 9 AC-2 "a RED suite is refused rather than scored at 100%" 'SUITE IS NOT GREEN' 'STOPPING'
refute_output AC-2 "a red suite produces no score at all" 'mutation score [0-9]'
rm -f "$repo/internal/schedule/selftest_red_test.go"

# --- 11-16. the hermetic agreement gate still bites (AC-8, AC-4, AC-5) ----------------
# Through `go run` rather than `make mutation-shape` so the case grades the gate's own
# exit status instead of make's.
shape=(env -C "$repo" go run ./scripts/mutation-shape)

sed -i 's/^    efficacy: 70$/    efficacy: 65/' "$repo/.gremlins.yaml"
cmd=("${shape[@]}")
run_expect 1 AC-8 "a floor in the configuration that the document does not carry" \
  'THE FLOOR DISAGREES' '\.gremlins\.yaml' 'docs/mutation-testing\.md' '65' '70'
restore_clone

sed -i '/^| `\^internal\/metrics\/`/d' "$repo/docs/mutation-testing.md"
cmd=("${shape[@]}")
run_expect 1 AC-8 "an exclusion the runner honours and the document never mentions" \
  'EXCLUDED PATH IS NOT IN THE DOCUMENT' '\^internal/metrics/'
restore_clone

sed -i 's|^            mode="diff"$|            mode="full"|' "$repo/.github/workflows/mutation.yml"
cmd=("${shape[@]}")
run_expect 1 AC-4 "a workflow whose planning shell stops scoping the pull-request run" \
  'plans the WRONG RUN'
restore_clone

sed -i 's|^          path: mutation-report.json$|          path: some-other-file.json|' "$repo/.github/workflows/mutation.yml"
cmd=("${shape[@]}")
run_expect 1 AC-4 "an artifact that is not the report the run writes" \
  'PUBLISHED ARTIFACT IS NOT THE REPORT' 'mutation-report.json'
restore_clone

sed -i "/- cron: '23 3 \* \* 6'/d" "$repo/.github/workflows/mutation.yml"
sed -i '/^  schedule:$/d' "$repo/.github/workflows/mutation.yml"
cmd=("${shape[@]}")
run_expect 1 AC-4 "an unscoped run with no schedule to start it" 'declares no schedule'
restore_clone

sed -i 's|^      issues: write$|      issues: read|' "$repo/.github/workflows/mutation.yml"
cmd=("${shape[@]}")
run_expect 1 AC-5 "a job that could not open its tracking issue" 'issues: write'
restore_clone

# A guard on a STEP OUTPUT is empty for a run that died before that step ran, so a
# scheduled run that fell over at the checkout, the toolchain or the ffmpeg install would
# file nothing - and that is the run nobody is watching. AC-5 binds a failure for ANY
# reason, so the guard has to read something the job has before its first step.
sed -i "s|^        if: failure() && github.event_name != 'pull_request'$|        if: failure() \&\& steps.plan.outputs.notify == 'true'|" \
  "$repo/.github/workflows/mutation.yml"
cmd=("${shape[@]}")
run_expect 1 AC-5 "a notification guarded by an output a dead run never wrote" \
  'STEP OUTPUT' 'github.event_name'
restore_clone

# --- 17-18. AC-5's substance, on AC-5's own grade route --------------------------------
# The assignee, the three facts in the body and the marker a second consecutive failure
# finds are decided by driving the notification against a stubbed issues API, and those
# tests live beside the gate. They are RUN here because this target is the criterion's
# grade route: a criterion graded by a command that does not exercise it is graded by
# nothing.
cmd=(env -C "$repo" go test -count=1 ./scripts/mutation-gate)
run_expect 0 AC-5 "the notification driven against a stubbed issues API" 'ok.*scripts/mutation-gate'

# ... and those tests BITE. Without the marker in the body there is nothing for the next
# failure to find, so every red Saturday opens another issue - the weekly noise that
# teaches a reader to close them unread, which is the whole reason the marker exists.
sed -i 's|^\tb.WriteString(issueMarker)$|\tb.WriteString("")|' "$repo/scripts/mutation-gate/notify.go"
cmd=(env -C "$repo" go test -count=1 ./scripts/mutation-gate)
run_expect 1 AC-5 "a notification body that lost its de-duplication marker" 'FAIL'
git -C "$repo" checkout -q -- scripts/mutation-gate/notify.go

# --- 19. the tree the gate grades is never rewritten -----------------------------------
after="$(cksum "$here/.gremlins.yaml" "$here/docs/mutation-testing.md" "$here/.github/workflows/mutation.yml" 2>/dev/null || true)"
if [ "$before" = "$after" ] && [ -n "$before" ]; then
  printf '  ok  the graded tree was never written: the configuration, the document and the workflow are byte-identical\n'
  pass=$((pass + 1))
else
  printf '::error::mutation selftest: the graded tree CHANGED. Mutations must happen in the clone alone.\n' >&2
  printf '        before: %s\n        after:  %s\n' "$before" "$after" >&2
  failed=$((failed + 1))
fi

echo
# Report against the number of cases DECLARED, not the number that ran: "$pass/$pass" is
# N/N by construction and could never show a shortfall.
total=$((pass + failed))
if [ "$total" -ne "$declared" ]; then
  echo "::error::mutation selftest: ran $total case(s), expected $declared - a defeat did NOT execute" >&2
  exit 1
fi
if [ "$failed" -ne 0 ]; then
  echo "::error::mutation selftest: $failed of $declared case(s) did not bite - the mutation gate is not trustworthy" >&2
  exit 1
fi
echo "mutation selftest: $pass/$declared cases bite; defeated on purpose: AC-1 AC-2 AC-3 AC-4 AC-5 AC-6 AC-8"
