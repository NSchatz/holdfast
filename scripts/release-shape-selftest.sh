#!/usr/bin/env bash
# Prove the release-shape gate still BITES. `make release-shape-selftest`.
#
# The gate (scripts/release-shape-gate, inside `make check`) is the only thing standing
# between a comment in release.yml and a one-way door. It decides, by RUNNING the workflow's
# planning logic and reading its `permissions:`, `secrets:` and `needs:` graph, that a dry
# run can publish nothing, that `:latest` moves last, that a non-zero major is refused, that
# the operator runbook names every step in the job that can publish, and that the reference
# docker-compose.yml gives users is the one a release promotes.
#
# A gate nobody has tried to defeat is a gate nobody knows works, and every way this one can
# fail is a way it fails SILENTLY - by printing "ok" over a release that would publish from a
# dry run. So each property is defeated here on purpose, against a MUTATED COPY of the real
# inputs, and each defeat has to be red AND to say what it saw. A case that goes red for
# somebody else's reason is counted as a failure, not a pass.
#
# TWO cases decide whether the design was worth it, and they point in opposite directions:
#
#   * case 3 flips the PLANNING SCRIPT so a dispatch sets publish=true and touches not one
#     `if:` in the file. A gate that matched the text of those guards stays green while the
#     publishing job runs.
#   * cases 15a-15d put REAL publishing commands, in the spellings that beat six earlier
#     readers, into a step that runs on a dispatch - and the gate must still PASS, because
#     that step holds no credential. If those cases red, the reader has come back.
#
# Runs entirely inside a throwaway directory; it never mutates the working tree, and it
# publishes nothing (nothing here executes a workflow step except the planning script, and
# that runs with the publishing binaries stubbed).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)" || { echo "::error::selftest: mktemp failed" >&2; exit 1; }
trap 'rm -rf "$work"' EXIT

declared=68
pass=0; failed=0

repo="$work/repo"
pristine="$work/pristine"
gate="$work/release-shape-gate"

# The inputs the gate reads. Copied from the WORKING TREE, so an uncommitted change - a fix
# or a break - is graded as it stands, which is where the mistake gets made.
#
# The two release scripts are here because the gate requires a role step's script to BE a
# file in the repository, executable: a step invoking a script that is not there fails in the
# middle of a release, having passed a gate that only compared strings. They are NOT executed.
inputs=(
  .github/workflows/release.yml
  docker-compose.yml
  docs/release.md
  go.mod
  scripts/resolve-compose-image.sh
  scripts/release-resmoke.sh
  scripts/release-promote.sh
  scripts/smoke-image.sh
)

( cd "$here" && go build -o "$gate" ./scripts/release-shape-gate ) \
  || { echo "::error::selftest: could not build the gate - it did NOT run" >&2; exit 1; }

for f in "${inputs[@]}"; do
  [ -r "$here/$f" ] || { echo "::error::selftest: $f is missing from the working tree - it graded nothing" >&2; exit 1; }
  mkdir -p "$repo/$(dirname "$f")" "$pristine/$(dirname "$f")"
  cp -p "$here/$f" "$repo/$f"
  cp -p "$here/$f" "$pristine/$f"
done
for f in "${inputs[@]}"; do
  cmp -s "$here/$f" "$repo/$f" \
    || { echo "::error::selftest: the fixture's $f is not the working tree's - it graded the wrong thing" >&2; exit 1; }
done

reset() {
  rm -rf "${repo:?}/.github" "${repo:?}/docs" "${repo:?}/scripts" "${repo:?}/docker-compose.yml" "${repo:?}/go.mod"
  for f in "${inputs[@]}"; do
    mkdir -p "$repo/$(dirname "$f")"
    cp -p "$pristine/$f" "$repo/$f"
  done
}

wf="$repo/.github/workflows/release.yml"
compose="$repo/docker-compose.yml"
runbook="$repo/docs/release.md"

# --- mutation helpers -----------------------------------------------------------------
# Every mutation is asserted to have CHANGED something. A sed that matched nothing would
# otherwise leave a clean tree behind and the case would "bite" against the baseline,
# which is the selftest's own version of the silent-green bug it exists to catch.
changed() {  # changed <file-under-$repo> <case-name>
  local rel="${1#"$repo"/}"
  if cmp -s "$pristine/$rel" "$1"; then
    echo "::error::selftest: the mutation for '$2' changed nothing in $rel - that case did NOT run" >&2
    exit 1
  fi
}

# block_range <file> <name-substring> -> "start end" (1-based, inclusive) of one step.
# `done` is load-bearing: awk's `exit` still runs END, so without it the range is printed
# twice and every caller silently gets a second, wrong pair of line numbers.
block_range() {
  awk -v pat="$2" '
    /^      - / { if (s) { print s, NR - 1; done = 1; exit } if (index($0, pat)) s = NR }
    END { if (s && !done) print s, NR }
  ' "$1"
}

# move_step <name-substring-to-move> <name-substring-to-put-it-before>. Both must be in the
# same job; the ordering cases that cross jobs mutate `needs:` instead.
move_step() {
  awk -v mv="$1" -v before="$2" '
    function isstep(l) { return l ~ /^      - / }
    { line[NR] = $0 }
    END {
      for (i = 1; i <= NR; i++) if (isstep(line[i]) && index(line[i], mv))     { a = i; break }
      for (i = 1; i <= NR; i++) if (isstep(line[i]) && index(line[i], before)) { t = i; break }
      if (!a || !t) { exit 3 }
      b = NR
      for (i = a + 1; i <= NR; i++) if (isstep(line[i])) { b = i - 1; break }
      for (i = 1; i <= NR; i++) {
        if (i == t) for (j = a; j <= b; j++) print line[j]
        if (i >= a && i <= b) continue
        print line[i]
      }
    }
  ' "$wf" > "$wf.new" || { echo "::error::selftest: could not move '$1' before '$2'" >&2; exit 1; }
  mv "$wf.new" "$wf"
}

# in_step <name-substring> <sed-program> - apply sed only inside one step's block.
in_step() {
  local r; r="$(block_range "$wf" "$1")"
  [ -n "$r" ] || { echo "::error::selftest: no step matching '$1'" >&2; exit 1; }
  # shellcheck disable=SC2086
  set -- $r "$2"
  sed -i "$1,$2{$3;}" "$wf"
}

# replace_line <awk-regex> <replacement, may contain \n> - the FIRST matching line only.
replace_line() {
  awk -v pat="$1" -v repl="$2" '
    $0 ~ pat && !done { print repl; done = 1; next }
    { print }
  ' "$wf" > "$wf.new" || { echo "::error::selftest: could not replace /$1/" >&2; exit 1; }
  mv "$wf.new" "$wf"
}

# add_build_step <run-body> - insert a step into the DISPATCH-PATH job. This is how a
# publishing command is put where a dry run would execute it.
add_build_step() {
  awk -v body="$1" '
    /^      - name: build the release binaries$/ && !done {
      print "      - name: a step whose text says it publishes"
      print "        id: says-publish"
      print "        run: " body
      print ""
      done = 1
    }
    { print }
  ' "$wf" > "$wf.new" || { echo "::error::selftest: could not add a build step" >&2; exit 1; }
  mv "$wf.new" "$wf"
}

# --- the harness ----------------------------------------------------------------------
out=""
run_gate() { out="$( "$gate" -root "$repo" 2>&1 )"; }

# expect <want-exit> <name> [must-mention-regex]. 0 = must pass, 1 = must bite.
expect() {
  local want="$1" name="$2" want_msg="${3:-}" got=0
  run_gate || got=$?
  if [ "$got" -ne "$want" ]; then
    printf '::error::selftest: %s - the gate exited %s, wanted %s\n' "$name" "$got" "$want" >&2
    printf '%s\n' "$out" | sed 's/^/       | /' >&2
    failed=$((failed + 1)); return
  fi
  if [ -n "$want_msg" ] && ! grep -qE -- "$want_msg" <<<"$out"; then
    printf '::error::selftest: %s - exited %s (correct) but for the WRONG REASON: nothing matched /%s/\n' "$name" "$got" "$want_msg" >&2
    printf '%s\n' "$out" | sed 's/^/       | /' >&2
    failed=$((failed + 1)); return
  fi
  printf '  ok: %s\n' "$name"; pass=$((pass + 1))
}

# expect_absent <want-exit> <name> <must-NOT-mention-regex>. The counterpart of expect's
# third argument, and it grades a different failure: a gate whose EXIT CODE is right while
# the sentence it prints answers a question it just said it could not answer. A6 is about
# that sentence, and a reader skimming stdout sees the reassurance, not the refusal.
expect_absent() {
  local want="$1" name="$2" forbidden="$3" got=0
  run_gate || got=$?
  if [ "$got" -ne "$want" ]; then
    printf '::error::selftest: %s - the gate exited %s, wanted %s\n' "$name" "$got" "$want" >&2
    printf '%s\n' "$out" | sed 's/^/       | /' >&2
    failed=$((failed + 1)); return
  fi
  if grep -qE -- "$forbidden" <<<"$out"; then
    printf '::error::selftest: %s - exited %s (correct) but still printed /%s/ over a question it could not answer\n' "$name" "$got" "$forbidden" >&2
    printf '%s\n' "$out" | sed 's/^/       | /' >&2
    failed=$((failed + 1)); return
  fi
  printf '  ok: %s\n' "$name"; pass=$((pass + 1))
}

# --- 0. The real, unmutated inputs PASS. Without this every "bites" case below could be a
#        gate that simply fails on everything, which would prove nothing at all.
expect 0 "the release definition as committed passes"

# =====================================================================================
# A6 / A12 - THE CAPABILITY PROPERTY. A dry run publishes nothing because nothing that
# runs on a dispatch is handed anything a registry, a ref or a release would accept.
# =====================================================================================

# --- 1. The publishing job's guard deleted outright. The crudest version of the mistake:
#        the job that holds `packages: write` now runs on a dispatch.
sed -i "/^    if: needs.build.outputs.publish == 'true'$/d" "$wf"
changed "$wf" "the publish job with no guard at all"
expect 1 "an unguarded publishing job is caught on a dispatch" "job \"publish\" RUNS and it holds a capability"
reset

# --- 2. `always()` reads as caution and means the opposite: the job runs on a dispatch.
sed -i "s|^    if: needs.build.outputs.publish == 'true'$|    if: always()|" "$wf"
changed "$wf" "the publish job guarded with always()"
expect 1 "a publishing job guarded with always() is caught on a dispatch" "job \"publish\" RUNS and it holds a capability"
reset

# --- 3. THE case the executed planning logic exists for. The PLANNING SCRIPT is flipped so
#        a dispatch sets publish=true; every `if:` in the file is untouched, so the guards
#        read exactly as they did a moment ago. A gate that matched their text is green here
#        while a dry run runs the whole publishing job.
sed -i 's/^            publish=false$/            publish=true/' "$wf"
sed -i 's|^            version="0.0.0-dev-${GITHUB_SHA::7}"$|            version="v0.0.0-dev-${GITHUB_SHA::7}"|' "$wf"
changed "$wf" "the planning script flipped to publish on a dispatch"
expect 1 "a planning script that publishes on a dispatch is caught, with every guard untouched" "job \"publish\" RUNS and it holds a capability"
reset

# --- 4 to 7. THE GRANT ITSELF. A dry run's job may hold no write scope of any kind: a token
#        with one can authorise something that leaves this machine whatever its steps say.
replace_line '^      contents: read # read the tree' '      contents: write'
changed "$wf" "the dispatch path granted contents: write"
expect 1 "a dispatch-path job granted contents: write is caught" "grants WRITE on contents"
reset

replace_line '^      contents: read # read the tree' '      contents: read\n      packages: write'
changed "$wf" "the dispatch path granted packages: write"
expect 1 "a dispatch-path job granted packages: write is caught" "grants WRITE on packages"
reset

replace_line '^      contents: read # read the tree' '      contents: read\n      id-token: write'
changed "$wf" "the dispatch path granted id-token: write"
expect 1 "a dispatch-path job granted id-token: write is caught (it mints a credential an external registry accepts)" "grants WRITE on id-token"
reset

awk '
  /^    permissions:$/ && !d { print "    permissions: write-all"; skip = 1; d = 1; next }
  skip && /^      contents: read # read the tree/ { skip = 0; next }
  { print }
' "$wf" > "$wf.new" && mv "$wf.new" "$wf"
changed "$wf" "the dispatch path granted write-all"
expect 1 "a dispatch-path job granted write-all is caught" "grants WRITE on"
reset

# --- 8. NO grant stated at all, at either level. The repository default then applies, and
#        this file cannot see it: it may be write-all. An unstated grant reads CLOSED.
awk '
  /^permissions:$/ { wl = 1; next }
  wl && /^  contents: read$/ { wl = 0; next }
  /^    permissions:$/ && !jd { jb = 1; jd = 1; next }
  jb && /^      contents: read # read the tree/ { jb = 0; next }
  { print }
' "$wf" > "$wf.new" && mv "$wf.new" "$wf"
changed "$wf" "no permissions block anywhere"
expect 1 "a job with no stated permissions is red, not assumed to grant nothing" "declares no .permissions:. and neither does the workflow"
reset

# --- 9. A scope outside GitHub's vocabulary. A typo grants nothing the author meant to
#        grant, and a scope GitHub added since is one nobody has classified. Both red.
replace_line '^      contents: read # read the tree' '      contentz: read'
changed "$wf" "an unknown permission scope"
expect 1 "an unrecognised permission scope reads CLOSED and reds by name" "grants \"contentz\", which is not one of the permission scopes"
reset

# --- 10. A value outside read / write / none.
replace_line '^      contents: read # read the tree' '      contents: maybe'
changed "$wf" "an unknown permission value"
expect 1 "an unrecognised permission value reds by name" "which is not one of read / write / none"
reset

# --- 11. A repository secret on the dispatch path. `permissions:` does not bound one - its
#         scope is whatever was put in it - so a step holding one can publish regardless.
in_step "the full gate (make check)" 's|^      - name: .*|&\n        env:\n          TOKEN: ${{ secrets.RELEASE_PAT }}|'
changed "$wf" "a repository secret on the dispatch path"
expect 1 "a secret other than the scoped GITHUB_TOKEN is caught on the dispatch path" "reads .secrets.RELEASE_PAT."
reset

# --- 12. `environment:` hands the job that environment's secrets, which this file cannot
#         see the value of.
sed -i '0,/^    runs-on: ubuntu-latest$/s//&\n    environment: production/' "$wf"
changed "$wf" "an environment on the dispatch path"
expect 1 "a deployment environment on the dispatch path is caught, naming what it hands over" "uses .environment:., which hands it"

reset

# --- 13. A job key nobody has classified. DENY BY DEFAULT is the whole difference between
#         this and the catalogues that lost six times: silence must be unreachable.
sed -i '0,/^    runs-on: ubuntu-latest$/s//&\n    some-future-key: whatever/' "$wf"
changed "$wf" "an unclassified job key"
expect 1 "an unclassified job key reads CLOSED and reds by name" "the job key .some-future-key:., which this gate has not classified"
reset

# --- 14. The same one level down, on a step.
in_step "the full gate (make check)" 's|^      - name: .*|&\n        some-step-key: x|'
changed "$wf" "an unclassified step key"
expect 1 "an unclassified step key reads CLOSED and reds by name" "the step key .some-step-key:., which this gate has not classified"
reset

# --- 15a to 15d. THE CONTROL THAT DECIDES WHETHER THIS DESIGN WAS WORTH IT.
#
#         Each of these puts a REAL publishing command into a step that runs on a manual
#         dispatch, in a spelling that defeated an earlier version of this gate: a plain
#         push (F1), one inside a quoted word handed to a nested shell (F11), one after the
#         step restores PATH (F14), and one through `exec`, which is a shell builtin and so
#         reaches no wrapper at all (F15).
#
#         THE GATE MUST PASS ALL FOUR. Not because they are harmless to write, but because
#         they cannot publish: the job that runs them holds `contents: read` and no secret,
#         so the registry, the ref and the release API each refuse it. If one of these ever
#         reds, a reader has come back into this gate, and the six ordinals that proved
#         reading cannot be made to work are being repeated.
for spelling in \
  'docker push ghcr.io/nschatz/holdfast:dev' \
  'sh -c "docker push ghcr.io/nschatz/holdfast:dev"' \
  'export PATH=/usr/bin:/bin; docker push ghcr.io/nschatz/holdfast:dev' \
  'exec docker push ghcr.io/nschatz/holdfast:dev'
do
  add_build_step "$spelling"
  changed "$wf" "a dispatch-path step spelled: $spelling"
  expect 0 "a dispatch-path step running \`$spelling\` publishes nothing, and the gate says so" \
    "NONE of them holds a capability that could publish anything"
  reset
done

# --- 15e. AND THE OTHER DIRECTION, or the four cases above would be satisfied by a gate that
#          passes everything. The IDENTICAL step, in a job that has been granted the write
#          scope, is caught - so what changed the verdict is the grant, which is the claim.
add_build_step 'docker push ghcr.io/nschatz/holdfast:dev'
replace_line '^      contents: read # read the tree' '      contents: read\n      packages: write'
changed "$wf" "the same publishing step, in a job granted packages: write"
expect 1 "the identical step in a job granted packages: write IS caught - the grant is what decides" "grants WRITE on packages"
reset

# --- 16. The other half of the split: an irreversible act must live in a job that can
#         actually perform it, or the boundary is decorative and the release fails at run
#         time having passed this gate.
awk '
  /^      contents: write # cut the GitHub release$/ { print "      contents: read"; next }
  /^      packages: write # push the image to GHCR$/ { print "      packages: read"; next }
  { print }
' "$wf" > "$wf.new" && mv "$wf.new" "$wf"
changed "$wf" "the publishing job stripped of its grants"
expect 1 "a publishing job that holds no grant is caught, not quietly accepted" "holds NO capability that could carry it out"
reset

# =====================================================================================
# A7 - ROLE IDENTITY. A step holds a position because it DECLARES it and INVOKES what
# that position names, never because its text mentions the thing.
# =====================================================================================

# --- 17. F13, closed by construction. A step that PRINTS the gate's name and runs nothing
#         used to satisfy the full-gate role.
in_step "the full gate (make check)" 's|^        run: make check$|        run: echo "make check"|'
changed "$wf" "the full gate reduced to an echo"
expect 1 "a step that merely mentions the gate cannot hold the full-gate role" "the first word of its script has to BE that program"
reset

# --- 17a. The same, UNQUOTED, which is the spelling that separates the two designs: every
#          word the role names is present, in order, as a whole field - and the step still
#          runs nothing. Only "the FIRST field must BE the program" refuses it.
in_step "the full gate (make check)" 's|^        run: make check$|        run: echo make check|'
changed "$wf" "the full gate reduced to an unquoted echo"
expect 1 "a step whose fields merely include the gate's cannot hold the role either" "the first word of its script has to BE that program"
reset

# --- 18. THE HONEST OTHER DIRECTION, and it is what keeps 17 a reading of the invocation
#         rather than a refusal of every respelling: `make -C . check` IS the full gate.
in_step "the full gate (make check)" 's|^        run: make check$|        run: make -C . check|'
changed "$wf" "the full gate respelled with -C"
expect 0 "a respelt but real invocation of the full gate still holds the role"
reset

# --- 19. The role's declaration removed. The gate refuses rather than grading an order over
#         a set of steps it could not identify.
in_step "the full gate (make check)" '/^        id: full-gate$/d'
changed "$wf" "the full-gate role undeclared"
expect 1 "a missing role declaration is refused, not worked around" "no step declares .id: full-gate."
reset

# --- 20. A role step turned into an inline multi-line script. One line is what makes the
#         comparison total; a script would have to be searched, and searching is what lost.
replace_line '^        run: ./scripts/release-resmoke.sh$' '        run: |\n          ./scripts/release-resmoke.sh\n          echo "and something else"'
changed "$wf" "the re-smoke role turned into a multi-line script"
expect 1 "a role step with a multi-line script is refused" "is more than one line"
reset

# --- 21. The action role pointed at a different action.
in_step "push the multi-arch image" 's|^        uses: docker/build-push-action@v6$|        uses: someone/else@v1|'
changed "$wf" "the push role pointed at another action"
expect 1 "a role that names an action is that action, not a lookalike" "but uses \"someone/else\""
reset

# --- 22. The amd64 smoke run reduced to an exec check. It is the run that must drive a REAL
#         encode; `--no-encode` makes it prove the binary starts and nothing else.
in_step "smoke test the image (real encode" 's|holdfast:release$|holdfast:release --no-encode|'
changed "$wf" "the amd64 smoke run reduced to an exec check"
expect 1 "an amd64 smoke run that does not encode cannot hold that role" 'passes "--no-encode", which that role must not'
reset

# --- 23. A role script deleted from the repository. The step would fail in the middle of a
#         release, having passed a gate that only compared strings.
rm -f "$repo/scripts/release-promote.sh"
expect 1 "a role invoking a script that is not in the repository is caught" "is not in this repository"
reset

# =====================================================================================
# A7 / A13 - the order itself.
# =====================================================================================

# --- 24. Promotion hoisted above the push. `:latest` would then point at a tag that does
#         not exist yet, moved without the pushed artefact being gated at all.
move_step "promote :latest" "push the multi-arch image"
changed "$wf" "the promotion hoisted above the version-tag push"
expect 1 "a promotion that runs before the version-tag push is caught" "does NOT run before"
reset

# --- 25. Promotion hoisted above the re-smoke of the PULLED artefact. This is the exact
#         ordering the workflow's own comment claims and nothing enforced: the push is a
#         cache rebuild, so `:latest` would be promoted onto bytes nobody smoked.
move_step "promote :latest" "smoke test the PUSHED image"
changed "$wf" "the promotion hoisted above the re-smoke"
expect 1 "a promotion that runs before the re-smoke of the pulled artefact is caught" "does NOT run before"
reset

# --- 26. The floating reference pushed directly by the build, so it is live before the
#         artefact is pulled back at all - which is what promoting it separately avoids.
in_step "push the multi-arch image" 's|^          tags: .*|&,${{ needs.build.outputs.image }}:latest|'
changed "$wf" "the build pushing :latest itself"
expect 1 "a build that pushes the floating reference itself is caught" "pushes ghcr.io/.*:latest directly"
reset

# --- 27. `needs:` removed. The jobs are then CONCURRENT: the gate refuses to report an order
#         it did not check, rather than reading declaration order as one.
sed -i '/^    needs: build$/d' "$wf"
changed "$wf" "the needs: edge removed"
expect 1 "two concurrent jobs are refused rather than ordered by where they appear" "CONCURRENT"
reset

# =====================================================================================
# A3 - once anything has failed, nothing publishes.
# =====================================================================================

# --- 28. `always()` moved to the END of the guard. On a dispatch it still does not run - so
#         case 1's assertion would not catch this - but after a FAILED gate it does, which is
#         the precise state in which `:latest` must stay where it was.
sed -i "s|^    if: needs.build.outputs.publish == 'true'$|    if: needs.build.outputs.publish == 'true' \&\& always()|" "$wf"
changed "$wf" "the publish job surviving a failed gate"
expect 1 "a publishing job that survives a failed gate is caught" "would STILL RUN after an earlier job has FAILED"
reset

# =====================================================================================
# A14 - a step before the promotion whose failure does not fail the run.
# =====================================================================================

# --- 29. The full gate, tolerated. `make check` reds, the run stays green, `:latest` moves
#         onto an image whose verify/swap logic was never proved.
in_step "the full gate (make check)" 's|^      - name: .*|&\n        continue-on-error: true|'
changed "$wf" "the full gate marked continue-on-error"
expect 1 "a tolerated failure on the full gate is caught" 'marked .continue-on-error: true'
reset

# --- 30. The same, on the re-smoke of the pushed artefact.
in_step "smoke test the PUSHED image" 's|^      - name: .*|&\n        continue-on-error: true|'
changed "$wf" "the re-smoke marked continue-on-error"
expect 1 "a tolerated failure on the re-smoke is caught" 'marked .continue-on-error: true'
reset

# --- 31. And on the JOB, where it tolerates every step at once.
sed -i '0,/^    runs-on: ubuntu-latest$/s//&\n    continue-on-error: true/' "$wf"
changed "$wf" "the dispatch-path job marked continue-on-error"
expect 1 "a tolerated failure on the whole job is caught" 'is marked .continue-on-error: true'
reset

# =====================================================================================
# A15 - a definition that cannot be read, cannot be parsed, or names no step. Each must
# say WHICH; a vacuous pass over nothing is the failure mode all three share.
# =====================================================================================

# --- 32.
rm -f "$wf"
expect 1 "a release definition that cannot be read is red, and says so" "CANNOT BE READ"
reset

# --- 33.
printf '\nthis is not: [valid: yaml\n' >> "$wf"
changed "$wf" "an unparseable release definition"
expect 1 "a release definition that cannot be parsed is red, and says so" "CANNOT BE PARSED"
reset

# --- 34.
: > "$wf"
expect 1 "an empty release definition is red, and says so" "IS EMPTY"
reset

# --- 35. A definition with no steps at all. Every assertion below would otherwise hold over
#         nothing and report green.
awk '/^    steps:$/ { print "    steps: []"; exit } { print }' "$pristine/.github/workflows/release.yml" > "$wf"
changed "$wf" "a definition with no steps"
expect 1 "a release definition that names no step is red, and says so" "NAMES NO STEP AT ALL"
reset

# --- 36. No planning step: nothing writes to $GITHUB_OUTPUT, so there is no runtime value to
#         decide any guard from. The gate must refuse rather than fall back to reading text,
#         which is the whole thing it is not allowed to do.
sed -i 's/GITHUB_OUTPUT/GITHUB_NOWHERE/g' "$wf"
changed "$wf" "a release definition with no planning step"
expect 1 "a definition whose planning step produces nothing is red" "writes nothing to .GITHUB_OUTPUT"
reset

# --- 37. THE BOUND ON WHAT THIS GATE EXECUTES. Exactly one step's script is ever run. A
#         SECOND step writing outputs would be a second script the gate had to execute to
#         know what the guards see, and running workflow step scripts to find out what they
#         do is the mechanism that was defeated in one line.
add_build_step 'echo "x=y" >> "$GITHUB_OUTPUT"'
changed "$wf" "a second step writing to GITHUB_OUTPUT"
expect 1 "a second planning script is refused rather than executed" "the only step this gate executes"
reset

# --- 38. And the one script it does execute may not reach a registry. The planning step
#         DECIDES; a step that publishes is not planning.
replace_line '^          set -euo pipefail$' '          set -euo pipefail\n          docker push ghcr.io/nschatz/holdfast:dev'
changed "$wf" "a planning step that reaches a registry"
expect 1 "a planning step that invokes a registry tool is refused" "is not planning"
reset

# =====================================================================================
# A16 / A8 - the operator runbook. An ACT is every step in the job that holds the grant,
# identified by its declared id: nothing about what a step SAYS is consulted.
# =====================================================================================

# --- 39. No runbook at all.
rm -f "$runbook"
expect 1 "a missing operator runbook is red, and lists the acts it should have named" "CANNOT BE READ"
reset

# --- 40. A runbook that exists and names no act. This is the vacuous pass A16 names: a
#         document can be present, long, and about nothing.
printf '# Releasing\n\nAsk Noah.\n' > "$runbook"
changed "$runbook" "a runbook that names no irreversible act"
expect 1 "a runbook that names NO irreversible act is red" "names NO irreversible act"
reset

# --- 41. One act quietly dropped from the runbook - what happens when a publishing step is
#         documented and then the documentation is edited.
sed -i 's|`publish/github-release`|(dropped)|' "$runbook"
changed "$runbook" "a runbook missing one act"
expect 1 "a runbook that stops naming one act is red, naming the id it needs" 'Add .publish/github-release.'
reset

# --- 42. THE PROPERTY ITSELF: a NEW step added to the job that can publish, which the
#         runbook has never heard of. It says nothing about publishing; it does not need to.
awk '
  /^      - name: cut the GitHub release$/ && !d {
    print "      - name: a step nobody wrote down"
    print "        id: brand-new"
    print "        run: echo hello"
    print ""
    d = 1
  }
  { print }
' "$wf" > "$wf.new" && mv "$wf.new" "$wf"
changed "$wf" "a new step in the publishing job"
expect 1 "a step added to the publishing job that the runbook does not name is red" 'Add .publish/brand-new.'
reset

# --- 43. A step in that job with no id at all. An act that cannot be named cannot be
#         reviewed.
in_step "cut the GitHub release" '/^        id: github-release$/d'
changed "$wf" "a publishing-job step with no id"
expect 1 "a step in the publishing job with no id is red" "carries no .id:."
reset

# =====================================================================================
# A17 / A9 - the example deployment's image reference.
# =====================================================================================

# --- 44. The reference removed. An absent reference is not agreement.
sed -i '/^ *image: ghcr/d' "$compose"
changed "$compose" "a compose file with no image reference"
expect 1 "an example deployment with no image reference is red, naming the file" "docker-compose.yml NAMES NO IMAGE REFERENCE"
reset

# --- 45. The reference made unreadable by breaking the file around it.
printf '\nthis: [is: broken\n' >> "$compose"
changed "$compose" "an unparseable compose file"
expect 1 "an unparseable example deployment is red, naming the file" "docker-compose.yml CANNOT BE PARSED"
reset

# --- 46.
rm -f "$compose"
expect 1 "a missing example deployment is red, naming the file" "docker-compose.yml CANNOT BE READ"
reset

# --- 47. The disagreement itself: the compose file names an image this repository's
#         release would never produce. This is what a repository rename does, silently.
sed -i 's|^\( *image: \).*|\1ghcr.io/someone-else/holdfast:latest|' "$compose"
changed "$compose" "a compose reference naming a different repository"
expect 1 "a compose reference nothing publishes is red, and prints both" "ghcr.io/someone-else/holdfast:latest"
reset

# --- 48. The floating tag moved. It is declared ONCE, in the promotion step's env, read by
#         this gate and by scripts/release-promote.sh - so changing it there changes what a
#         release promotes, and the compose file no longer names it.
in_step "promote :latest" 's|^          FLOATING_TAG: latest$|          FLOATING_TAG: stable|'
changed "$wf" "the floating tag moved to :stable"
expect 1 "a promotion that moves a different floating reference than the compose file names is red" "ghcr.io/nschatz/holdfast:stable"
reset

# --- 49. The floating tag removed altogether. Which reference a release moves is then
#         unknown to this gate, and unknown is not harmless.
in_step "promote :latest" '/^          FLOATING_TAG: latest$/d'
changed "$wf" "the floating tag undeclared"
expect 1 "a promotion that declares no floating tag is red" "does not declare both IMAGE and FLOATING_TAG"
reset

# =====================================================================================
# A4 / A10 - the major-version-zero refusal, and the record it has to name.
# =====================================================================================

# --- 50. The refusal removed. A v1.0.0 tag would then publish, and 1.0.0 "defines the
#         public API" over three surfaces this project has not frozen.
sed -i 's/^          if \[ "$publish" = "true" \]; then$/          if false; then/' "$wf"
changed "$wf" "the major-version-zero refusal removed"
expect 1 "a release path that accepts a non-zero major is red" "ACCEPTS v1.0.0"
reset

# --- 51. The refusal kept, but stripped of the record it points at. "No" without "go and
#         read this first" is how the refusal gets deleted by the next person in a hurry.
sed -i 's|docs/release.md|the stability record|g' "$wf"
changed "$wf" "a refusal that names no record"
expect 1 "a refusal that names no record is red" "names no record"
reset

# --- 52. The refusal pointing at a document that does not exist - prose again.
sed -i 's|docs/release.md|docs/stability.md|g' "$wf"
changed "$wf" "a refusal naming a record that does not exist"
expect 1 "a refusal naming a record that does not exist is red" "docs/stability.md, which does not exist"
reset

# =====================================================================================
# A11 - the post-promotion resolution of the example deployment's reference.
# =====================================================================================

# --- 53. The step deleted. Every release after the first would then stop checking that the
#         reference users actually pull resolves at all.
r="$(block_range "$wf" "must resolve to the gated digest")"
[ -n "$r" ] || { echo "::error::selftest: could not find the resolution step" >&2; exit 1; }
# shellcheck disable=SC2086
set -- $r
sed -i "$1,$2d" "$wf"
changed "$wf" "the resolution step deleted"
expect 1 "a release that never resolves the example deployment's reference is red" "no step declares .id: resolve-compose."
reset

# --- 54. The step kept, but moved before the promotion, where it would resolve the
#         PREVIOUS release's digest and pass while this one is broken.
move_step "must resolve to the gated digest" "promote :latest"
changed "$wf" "the resolution step moved before the promotion"
expect 1 "a resolution that runs before the promotion is red" "does NOT run before"
reset

# --- 55 to 58. scripts/resolve-compose-image.sh itself, driven against a fake registry.
#         It is the enforcement behind A5, so its verdict has to be real: a matching digest
#         passes, a different one fails, an unresolvable reference fails, and a compose file
#         with no reference fails - each with its own exit code and its own sentence.
fake="$work/fakebin"; mkdir -p "$fake"
cat > "$fake/docker" <<'FAKE'
#!/bin/sh
# A registry that answers only from $FAKE_DIGESTS ("<ref> <digest>" per line).
ref=""
for a in "$@"; do
  case "$a" in
    -*) ;;
    *:*) ref="$a" ;;
  esac
done
d="$(awk -v r="$ref" '$1 == r { print $2; exit }' "$FAKE_DIGESTS")"
[ -n "$d" ] || { echo "ERROR: $ref: not found" >&2; exit 1; }
echo "$d"
FAKE
chmod +x "$fake/docker"

digests="$work/digests"
resolve() {  # resolve <name> <want-exit> <must-mention>
  local name="$1" want="$2" mention="$3" got=0 o
  o="$( cd "$repo" && PATH="$fake:$PATH" FAKE_DIGESTS="$digests" RELEASE_SHAPE_GATE="$gate" \
        IMAGE=ghcr.io/nschatz/holdfast VERSION=v0.1.0 ./scripts/resolve-compose-image.sh 2>&1 )" || got=$?
  if [ "$got" -ne "$want" ]; then
    printf '::error::selftest: %s - exited %s, wanted %s\n%s\n' "$name" "$got" "$want" "$o" >&2
    failed=$((failed + 1)); return
  fi
  if [ -n "$mention" ] && ! grep -qE -- "$mention" <<<"$o"; then
    printf '::error::selftest: %s - exited %s (correct) but nothing matched /%s/:\n%s\n' "$name" "$got" "$mention" "$o" >&2
    failed=$((failed + 1)); return
  fi
  printf '  ok: %s\n' "$name"; pass=$((pass + 1))
}

printf 'ghcr.io/nschatz/holdfast:v0.1.0 sha256:aaa\nghcr.io/nschatz/holdfast:latest sha256:aaa\n' > "$digests"
resolve "the compose reference resolving to the gated digest passes" 0 "the digest this release gated"

printf 'ghcr.io/nschatz/holdfast:v0.1.0 sha256:aaa\nghcr.io/nschatz/holdfast:latest sha256:bbb\n' > "$digests"
resolve "a compose reference resolving to a DIFFERENT digest fails the release" 5 "DIFFERENT image"

printf 'ghcr.io/nschatz/holdfast:v0.1.0 sha256:aaa\n' > "$digests"
resolve "a compose reference that does not resolve at all fails the release" 4 "does NOT RESOLVE"

sed -i '/^ *image: ghcr/d' "$compose"
changed "$compose" "resolving a compose file with no image reference"
printf 'ghcr.io/nschatz/holdfast:v0.1.0 sha256:aaa\nghcr.io/nschatz/holdfast:latest sha256:aaa\n' > "$digests"
resolve "a compose file naming no image is refused before any registry call" 3 "names NO image reference"
reset

# --- 59. The shape that made two readers dangerous: a SECOND service with its own
#         `image:`. There is one reader now - the gate's YAML decoder, asked for by
#         `-print-compose-ref` - and it REFUSES rather than silently grading whichever
#         service came first, which is what a `sed … | head -1` did.
printf '\n  sidecar:\n    image: ghcr.io/nschatz/something-else:latest\n' >> "$compose"
changed "$compose" "a compose file whose second service carries an image"
printf 'ghcr.io/nschatz/holdfast:v0.1.0 sha256:aaa\nghcr.io/nschatz/holdfast:latest sha256:aaa\nghcr.io/nschatz/something-else:latest sha256:aaa\n' > "$digests"
resolve "a second service's image reference is refused, not silently ignored" 3 "names 2 image references"
reset

# --- 60. The DEFAULT reader path, which is the one a real release takes: no binary handed
#         over, so the script builds the single reader itself. Driven against the real
#         working tree, so the whole chain - script, reader, docker-compose.yml - is the
#         committed one and not a fixture.
printf 'ghcr.io/nschatz/holdfast:v0.1.0 sha256:aaa\nghcr.io/nschatz/holdfast:latest sha256:aaa\n' > "$digests"
got=0
o="$( cd "$here" && PATH="$fake:$PATH" FAKE_DIGESTS="$digests" \
      IMAGE=ghcr.io/nschatz/holdfast VERSION=v0.1.0 ./scripts/resolve-compose-image.sh 2>&1 )" || got=$?
if [ "$got" -eq 0 ] && grep -qE 'the digest this release gated' <<<"$o"; then
  printf '  ok: the script builds the single reader itself when no binary is handed to it\n'
  pass=$((pass + 1))
else
  printf '::error::selftest: the default reader path (built from source) failed - exit %s\n' "$got" >&2
  printf '%s\n' "$o" | sed 's/^/       | /' >&2
  failed=$((failed + 1))
fi

# --- 61. A6 grades a SENTENCE as well as an exit code, and the two can come apart: a gate
#         that says "NONE of them holds a capability" over a question it just refused would
#         reassure every reader who skims stdout.
replace_line '^      contents: read # read the tree' '      contentz: read'
changed "$wf" "an undecidable grant on the dispatch path"
expect_absent 1 "a dispatch it could not decide never prints the reassurance" "NONE of them holds a capability"
reset

# --- 62. And the gate has to still be IN `make check`. A target nothing depends on is a
#         gate that runs nowhere, and nothing else in this file would notice.
prereqs=" $(sed -n 's/^check:[[:space:]]*//p' "$here/Makefile" | head -1) "
case "$prereqs" in
  *" release-shape "*)
    printf '  ok: the check target still depends on release-shape\n'; pass=$((pass + 1)) ;;
  *)
    printf '::error::selftest: the `check` target no longer depends on release-shape - the gate is detached\n' >&2
    failed=$((failed + 1)) ;;
esac

echo
# Report against the number of cases DECLARED, not the number that ran: "$pass/$pass" is
# N/N by construction and could never show a shortfall (A18).
total=$((pass + failed))
if [ "$total" -ne "$declared" ]; then
  echo "::error::release-shape selftest: ran $total case(s), expected $declared - a case did not execute" >&2
  exit 1
fi
if [ "$failed" -ne 0 ]; then
  echo "::error::release-shape selftest: $failed of $declared case(s) did not bite - the release-shape gate is not trustworthy" >&2
  exit 1
fi
echo "release-shape selftest: $pass/$declared cases bite"
