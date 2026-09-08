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

declared=126
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
#         Put on a step that holds no ROLE, so that the capability check is what catches it:
#         an unclassified environment name on a role step is refused one layer earlier, by
#         role identity (case 17g), and this case is about the grant rather than the role.
in_step "install ffmpeg" 's|^      - name: .*|&\n        env:\n          TOKEN: ${{ secrets.RELEASE_PAT }}|'
changed "$wf" "a repository secret on the dispatch path"
expect 1 "a secret other than the scoped GITHUB_TOKEN is caught on the dispatch path" "reads .secrets.RELEASE_PAT."
reset

# --- 11a. The SAME secret one level further out, in the workflow's own `env:`, which every
#          job inherits. It reaches a dispatch-path step without appearing anywhere inside
#          the job. TWO independent layers now refuse it and the first one wins here: an
#          environment name in scope for a ROLE step must be classified (role identity), and
#          the capability scan walks the workflow's env as well as the job's node. The second
#          layer is asserted directly by TestCanPublish_ASecretInTheWorkflowEnvIsReachedByEveryJob,
#          because a case can only ever see whichever refusal comes first.
replace_line '^  GO_VERSION: "1.25.14"$' '  GO_VERSION: "1.25.14"\n  GHCR_PAT: ${{ secrets.GHCR_PUBLISH_PAT }}'
changed "$wf" "a workflow-level env handing every job a repository secret"
expect 1 "a repository secret in the workflow's own env is refused at the first layer that sees it" "GHCR_PAT., which this gate has not classified"
reset

# --- 11b. THE INHERITED GRANT. The workflow's top-level block widened to write-all and the
#          dispatch-path job's own block removed, so what it runs with is the workflow's.
#          A job's own `permissions:` REPLACES the workflow's, so removing one is not a
#          narrowing.
replace_line '^permissions:$' 'permissions: write-all'
sed -i '/^  contents: read$/d' "$wf"
sed -i '/^      contents: read # read the tree/d' "$wf"
sed -i '/^    permissions:$/{0,/^    permissions:$/d}' "$wf"
changed "$wf" "a dispatch-path job inheriting write-all"
expect 1 "a dispatch-path job inheriting the workflow's write-all is caught" "grants WRITE on"
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

# --- 15f, 15g. A GUARD THIS GATE CANNOT DECIDE IS NOT A GUARD THAT IS FALSE. The publishing
#          job's `if:` is what keeps it off the dispatch path, and both of these make it
#          undecidable: an expression function nobody implemented, and a context path this
#          run never produced. Reading either as "does not run" is the fail-open in its
#          purest form.
sed -i "s|^    if: needs.build.outputs.publish == 'true'\$|    if: fromJSON(needs.build.outputs.publish)|" "$wf"
changed "$wf" "a publishing guard using an unimplemented function"
expect 1 "a publishing guard this gate cannot evaluate is refused, not read as false" "cannot be decided"
reset

sed -i "s|^    if: needs.build.outputs.publish == 'true'\$|    if: github.event.inputs.publish == 'true'|" "$wf"
changed "$wf" "a publishing guard on a context nothing produced"
expect 1 "a publishing guard on a context the run never produced is refused" "cannot be decided"
reset

# --- 15h. A publishing job written as a YAML ANCHOR and cloned by an alias. The node walk
#          yields nothing for the alias, so the CLONE is never named - but the anchor job is
#          a real mapping, is walked, and reds, so the definition as a whole is refused.
#          (GitHub Actions does not accept anchors in a workflow file either.)
cat > "$work/sidecar.yml" <<'YML'
  sidecar: &sidecar
    runs-on: ubuntu-latest
    permissions:
      packages: write
    steps:
      - name: push a dev image so testers can pull dispatch builds
        id: sidecar-push
        run: docker push ghcr.io/nschatz/holdfast:dev

  sidecar-clone: *sidecar

YML
awk -v f="$work/sidecar.yml" '
  /^  build:$/ && !d { while ((getline line < f) > 0) print line; d = 1 }
  { print }
' "$wf" > "$wf.new" && mv "$wf.new" "$wf"
changed "$wf" "an anchored publishing job cloned by an alias"
expect 1 "an anchored dispatch-path job granted packages: write is caught" "holds a capability"
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

# --- 17b to 17f. F18, DRIVEN FOR REAL. Cases 17 and 17a both turn on the FIRST field, and
#         until this loop no case drove a `make` invocation that makes make run nothing.
#         `make -n check` names the full gate exactly, prints every recipe in it, executes
#         not one of them, and exits 0 - so the role held, the order sentence printed, and a
#         tag push would have published an image whose `make check` never ran.
#
#         These are FIVE spellings of one hole and they are refused by ONE rule: a field the
#         role never declared reds by name. That is deliberately not a list of bad flags -
#         a catalogue of spellings is what lost six times in this gate's other half - so the
#         long form, the clustered form and the operand form are all refused by the same
#         sentence as the flag nobody has thought of yet.
for spelling in \
  'make -n check' \
  'make --dry-run check' \
  'make -Bn check' \
  'make check SHELL=/bin/true' \
  'make -f /dev/null check'
do
  in_step "the full gate (make check)" "s|^        run: make check\$|        run: $spelling|"
  changed "$wf" "the full gate neutered as: $spelling"
  expect 1 "a full gate spelled \`$spelling\` runs no gate and cannot hold the role" \
    "a field this role has not classified"
  reset
done

# --- 17f2. A field the role DOES declare, carrying a value it does not. `-C` is permitted
#         because `.` is the directory make would have run in anyway; any other directory is
#         a different Makefile's `check`, which may be empty.
in_step "the full gate (make check)" 's|^        run: make check$|        run: make -C /tmp check|'
changed "$wf" "the full gate pointed at another directory"
expect 1 "a declared field carrying an undeclared value is caught" "That field's value is DECLARED rather than free"
reset

# --- 17g. THE SAME NEUTER WITH THE INVOCATION UNTOUCHED. `MAKEFLAGS: -n` in the step's env
#         makes `run: make check` a dry run without changing one character of the `run:`
#         line, so a rule that only accounted for fields would print "ok" over it.
in_step "the full gate (make check)" 's|^      - name: .*|&\n        env:\n          MAKEFLAGS: -n|'
changed "$wf" "the full gate neutered through its environment"
expect 1 "an unclassified environment name in scope for a role step is caught" "which this gate has not classified"
reset

# --- 17g2. And at the WORKFLOW level, where it reaches every role step at once.
replace_line '^  GO_VERSION: "1.25.14"$' '  GO_VERSION: "1.25.14"\n  MAKEFLAGS: -n'
changed "$wf" "a workflow-level MAKEFLAGS"
expect 1 "a workflow-level environment name nobody classified is caught too" "which this gate has not classified"
reset

# --- 17h. `shell:` decides which interpreter runs the script. GitHub appends the script to
#         the command given, so `shell: cat` prints it and exits 0.
in_step "the full gate (make check)" 's|^      - name: .*|&\n        shell: cat|'
changed "$wf" "the full gate handed to cat"
expect 1 "a role step whose shell is not a shell is caught" "which decides which interpreter runs the script"
reset

# --- 17i. `working-directory:` makes `make check` some other Makefile's check.
in_step "the full gate (make check)" 's|^      - name: .*|&\n        working-directory: /tmp|'
changed "$wf" "the full gate run somewhere else"
expect 1 "a role step moved to another directory is caught" "which decides which directory the script runs in"
reset

# --- 17j. The same two neuters set from the JOB, where nobody reading the step would see them.
sed -i '0,/^    runs-on: ubuntu-latest$/s//&\n    defaults:\n      run:\n        shell: cat/' "$wf"
changed "$wf" "a job-level defaults block over the role steps"
expect 1 "a job \`defaults:\` block over a role step is caught" "declares a .defaults:. block"
reset

# --- 17k. THE OTHER ROLE WITH THIS HOLE. The amd64 smoke role declared what it must NOT
#         pass and nothing it MUST, so `./scripts/smoke-image.sh` with no image argument held
#         it - a run that smokes nothing and exits 2 on its own usage message.
in_step "smoke test the image (real encode" 's|^        run: ./scripts/smoke-image.sh holdfast:release$|        run: ./scripts/smoke-image.sh|'
changed "$wf" "the amd64 smoke run with no image at all"
expect 1 "a smoke run that names no image cannot hold that role" "passes no argument"
reset

# --- 17l. And a script role handed a flag nobody declared.
in_step "smoke test the PUSHED image" 's|^        run: ./scripts/release-resmoke.sh$|        run: ./scripts/release-resmoke.sh --skip|'
changed "$wf" "the re-smoke handed an undeclared flag"
expect 1 "an undeclared field on a script role is caught" "a field this role has not classified"
reset

# --- 17m. THE ACTION ROLE'S VERSION OF F18. `push: false` leaves the step being exactly the
#         action the role names while publishing nothing at all.
in_step "push the multi-arch image" 's|^          push: true$|          push: false|'
changed "$wf" "the version-tag push turned off"
expect 1 "an action role that publishes nothing cannot hold the push role" "that role requires .push: true"
reset

# --- 17n. And an input nobody classified: this one diverts the build to a local directory.
in_step "push the multi-arch image" 's|^          push: true$|          push: true\n          outputs: type=local,dest=./out|'
changed "$wf" "the version-tag push diverted to a directory"
expect 1 "an unclassified action input reads CLOSED" "which this role has not classified"
reset

# --- 18. THE HONEST OTHER DIRECTION, and it is what keeps 17 a reading of the invocation
#         rather than a refusal of every respelling: `make -C . check` IS the full gate.
in_step "the full gate (make check)" 's|^        run: make check$|        run: make -C . check|'
changed "$wf" "the full gate respelled with -C"
expect 0 "a respelt but real invocation of the full gate still holds the role"
reset

# --- 18a. The other direction for the OPTIONAL half. `--no-encode` is permitted on the arm64
#         smoke run, not required: dropping it makes that run STRICTER, and a role that
#         refused a step for doing MORE than it promises would be exact equality by another
#         name - which is what deny-by-default must not collapse into.
in_step "smoke test the arm64 image" 's|^        run: ./scripts/smoke-image.sh holdfast:release-arm64 linux/arm64 --no-encode$|        run: ./scripts/smoke-image.sh holdfast:release-arm64 linux/arm64|'
changed "$wf" "an arm64 smoke run that also encodes"
expect 0 "an arm64 smoke run that drops its optional flag still holds the role"
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
# WHAT A ROLE STEP IS HANDED. A role is held by a step that INVOKES what the role names,
# and the invocation is the step's whole structured surface - which includes the VALUES its
# `env:` hands the program, not only their names. Naming a value's purpose holds it to
# nothing: every one of the cases below leaves the role held, the invocation untouched and
# the order sentence printing, while the step does the right thing to the WRONG OBJECT.
# =====================================================================================

# --- 49a. The re-smoke pointed at the FLOATING reference. It then pulls back the PREVIOUS
#          release - which passes, because it was gated last time - while the artefact this
#          run just pushed is never pulled back at all, and the promotion moves `:latest`
#          onto it. The push is a cache REBUILD (release.yml says so at that step), which is
#          the entire reason the pull-back exists.
in_step "smoke test the PUSHED image" 's|^          REF: .*|          REF: ${{ needs.build.outputs.image }}:latest|'
changed "$wf" "the re-smoke handed the floating reference"
expect 1 "a re-smoke of the floating reference rather than the pushed artefact is caught" \
  "is not the reference this run pushes and gates"
reset

# --- 49b. And with a reference that has nothing to do with this run at all.
in_step "smoke test the PUSHED image" 's|^          REF: .*|          REF: ghcr.io/nschatz/holdfast:v0.0.1|'
changed "$wf" "the re-smoke handed an unrelated reference"
expect 1 "a re-smoke of a reference this run never produced is caught" \
  "is not the reference this run pushes and gates"
reset

# --- 49c. A11 MADE VACUOUS BY ONE WORD. `scripts/resolve-compose-image.sh` compares the
#          compose reference's digest against `${IMAGE}:${VERSION}`; hand it `VERSION: latest`
#          and that is the compose reference itself, which the gate has ALREADY proved offline
#          is the same string. The release then compares a reference with itself and can never
#          fail - and A11 is the only enforcement A5 has after the first release.
in_step "must resolve to the gated digest" 's|^          VERSION: .*|          VERSION: latest|'
changed "$wf" "the resolution handed the floating tag as its version"
expect 1 "a resolution that would compare the compose reference against itself is caught" \
  "is not the version the planning logic produced"
reset

# --- 49d. The softer half: some other version. It fails loudly at release time rather than
#          silently, and it still resolves nothing about the artefact this run gated.
in_step "must resolve to the gated digest" 's|^          VERSION: .*|          VERSION: v0.0.1|'
changed "$wf" "the resolution handed another version"
expect 1 "a resolution against a version this run never published is caught" \
  "is not the version the planning logic produced"
reset

# --- 49e. And the image half of the same reference.
in_step "must resolve to the gated digest" 's|^          IMAGE: .*|          IMAGE: ghcr.io/someone-else/holdfast|'
changed "$wf" "the resolution handed another image"
expect 1 "a resolution against an image this repository does not publish is caught" \
  "is not the image reference the planning logic produced"
reset

# --- 49f. The promotion moved onto a version this run did not gate. `:latest` would point at
#          an artefact no step in this run pushed, smoked or pulled back.
in_step "promote :latest" 's|^          VERSION: .*|          VERSION: v0.0.1|'
changed "$wf" "the promotion handed a version this run did not gate"
expect 1 "a promotion onto a version this run never gated is caught" \
  "is not the version the planning logic produced"
reset

# --- 49g. THE OTHER DIRECTION, or every case above would be satisfied by a gate that refuses
#          any change at all. What is decided is the SOURCE the expression names, not the text
#          it is written in: quoting the scalar and closing up the braces changes every
#          character the old text matchers would have looked at and names the same two outputs.
in_step "smoke test the PUSHED image" 's|^          REF: .*|          REF: "${{needs.build.outputs.image}}:${{  needs.build.outputs.version  }}"|'
changed "$wf" "the re-smoke's reference respelt around the same references"
expect 0 "a respelt expression naming the SAME planning outputs still holds the role"
reset

# --- 49g2. And the stronger half of that direction: the reference reaches the planning logic
#           through a DIFFERENT job output. The gate follows the `needs:` graph rather than
#           matching a spelling, so a second output carrying the same plan output is accepted -
#           which is what stops this rule being one more catalogue of permitted strings.
replace_line '^      image: \$\{\{ steps.plan.outputs.image \}\}$' '      image: ${{ steps.plan.outputs.image }}\n      alias: ${{ steps.plan.outputs.image }}'
in_step "smoke test the PUSHED image" 's|^          REF: .*|          REF: ${{ needs.build.outputs.alias }}:${{ needs.build.outputs.version }}|'
changed "$wf" "the re-smoke reaching the same plan output through another job output"
expect 0 "a reference that reaches the same planning output through a different job output still holds the role"
reset

# --- 49h. And nothing handed at all. A role whose program reads a value that is set at none
#          of the three levels either dies in the middle of a release or falls back to a
#          default nobody chose; an absent value is not a harmless one.
in_step "smoke test the PUSHED image" '/^          REF: /d'
changed "$wf" "the re-smoke handed no reference at all"
expect 1 "a role step handed nothing at all is caught, naming the value and what it should be" \
  "nothing sets it, at any of the three levels"
reset

# =====================================================================================
# THE SPELLING NO COMPARISON CAN SEE. Every case above is caught by a gate that compares the
# value with the one a planned release produced. These are the ones that are NOT: a literal
# equal to that sample. The sample was `v0.1.0` - not an arbitrary string but the version this
# repository has actually published, named throughout docs/release.md, CLAUDE.md and
# README.md, and the one a maintainer copies out of a green run's log (S0046 F26). They are
# caught here because nothing is compared with a sample at all: a role step's object has to BE
# the planning logic's own output, traced through the `needs:` graph, so EVERY literal reds -
# the one that coincides with a sample no differently from the one that does not.
# =====================================================================================

# --- 49i. The re-smoke pinned to the literal v0.1.0. Every later release would then pull back
#          and smoke the ALREADY-PUBLISHED v0.1.0 - which passes, it was gated in July - while
#          the artefact that run pushed is never pulled back at all and `:latest` moves onto
#          it. This is 49a's harm at the one spelling a comparison cannot see.
in_step "smoke test the PUSHED image" 's|^          REF: .*|          REF: ghcr.io/nschatz/holdfast:v0.1.0|'
changed "$wf" "the re-smoke pinned to the published version"
expect 1 "a re-smoke pinned to a literal equal to the published version is caught" \
  "is not the reference this run pushes and gates"
reset

# --- 49j. The same spelling on A11's resolution step: the compose reference's digest would be
#          compared against v0.1.0 on every future release, so A11 - the only enforcement A5
#          has after the first release - stops grading anything that run published.
in_step "must resolve to the gated digest" 's|^          VERSION: .*|          VERSION: v0.1.0|'
changed "$wf" "the resolution pinned to the published version"
expect 1 "a resolution pinned to a literal equal to the published version is caught" \
  "is a LITERAL where"
reset

# --- 49k. And on the promotion, so `:latest` is retagged onto the previous release for ever.
in_step "promote :latest" 's|^          VERSION: .*|          VERSION: v0.1.0|'
changed "$wf" "the promotion pinned to the published version"
expect 1 "a promotion pinned to a literal equal to the published version is caught" \
  "is a LITERAL where"
reset

# --- 49l. The PUSH, whose object is an action input rather than an `env:` scalar. Pinned, a
#          later tag republishes v0.1.0 - and semver.org, the authority this repository's
#          version scheme rests on, is explicit that a released version's contents must never
#          be modified.
in_step "push the multi-arch image" 's|^          tags: .*|          tags: ghcr.io/nschatz/holdfast:v0.1.0|'
changed "$wf" "the push pinned to the published version"
expect 1 "a version-tag push pinned to a literal equal to the published version is caught" \
  "is not the reference this run pushes and gates"
reset

# --- 49m. All four at once, which is what a maintainer copying a green run's log would
#          actually write, and the shape in which every other assertion in this gate stays
#          green: the order holds, the grant holds, the runbook names every act.
in_step "push the multi-arch image" 's|^          tags: .*|          tags: ghcr.io/nschatz/holdfast:v0.1.0|'
in_step "smoke test the PUSHED image" 's|^          REF: .*|          REF: ghcr.io/nschatz/holdfast:v0.1.0|'
in_step "promote :latest" 's|^          VERSION: .*|          VERSION: v0.1.0|'
in_step "must resolve to the gated digest" 's|^          VERSION: .*|          VERSION: v0.1.0|'
changed "$wf" "a release pinned end to end to the published version"
expect 1 "a release pinned end to end to the published version is caught, so a later tag cannot republish an already-released version" \
  "is not the reference this run pushes and gates"
reset

# --- 49p. AN EXPRESSION THIS GATE CANNOT FOLLOW IS NOT A HARMLESS ONE. `env.` is in scope for
#          a `with:` input and for an `env:` value, it names something set anywhere at all, and
#          following it would mean reading whatever put it there. It reads CLOSED.
in_step "must resolve to the gated digest" 's|^          VERSION: .*|          VERSION: ${{ env.GO_VERSION }}|'
changed "$wf" "the resolution's version taken from an environment name"
expect 1 "a reference this gate cannot trace back to the planning logic reads CLOSED" \
  "CANNOT TRACE"
reset

# --- 49q. A reference to a job whose outputs this one does not receive. GitHub hands a job the
#          outputs of the jobs it NEEDS and nothing else, so this is the empty string at
#          release time - not an error, just an act performed on nothing.
in_step "smoke test the PUSHED image" 's|^          REF: .*|          REF: ${{ needs.nobody.outputs.image }}:${{ needs.build.outputs.version }}|'
changed "$wf" "the re-smoke reading a job that is not needed"
expect 1 "a reference to a job this one does not need reads CLOSED" "CANNOT TRACE"
reset

# --- 49r. And an output the producing job does not declare, which is the same empty string
#          reached the other way.
in_step "smoke test the PUSHED image" 's|^          REF: .*|          REF: ${{ needs.build.outputs.image }}:${{ needs.build.outputs.nosuch }}|'
changed "$wf" "the re-smoke reading an output that does not exist"
expect 1 "a reference to an output the producing job does not declare reads CLOSED" "CANNOT TRACE"
reset

# --- 49s. THE FLOATING TAG IS THE ONE VALUE A RELEASE DECLARES RATHER THAN DERIVES, so it is
#          the one literal this rule permits - and it is held against docker-compose.yml
#          instead. An EXPRESSION there moves it somewhere the gate cannot follow.
in_step "promote :latest" 's|^          FLOATING_TAG: .*|          FLOATING_TAG: ${{ needs.build.outputs.version }}|'
changed "$wf" "the floating tag turned into an expression"
expect 1 "a floating tag that is an expression rather than the declared literal is red" \
  "which carries an expression"
reset

# --- 49n. THE OTHER DIRECTION for the input half, or the cases above would be satisfied by a
#          gate that refuses any change to `tags:` at all. A different spelling naming the same
#          two planning outputs still holds the role.
in_step "push the multi-arch image" 's|^          tags: .*|          tags: "${{needs.build.outputs.image}}:${{ needs.build.outputs.version }}"|'
changed "$wf" "the push's tags respelt around the same references"
expect 0 "a respelt input naming the SAME planning outputs still holds the role"
reset

# --- 49o. And the input removed altogether. An act with no object is not that act.
in_step "push the multi-arch image" '/^          tags: /d'
changed "$wf" "the push handed no tags at all"
expect 1 "a version-tag push that names no reference to publish is red" "declares no .tags:. input"
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

# =====================================================================================
# THE EVENT SURFACE. Every shape above is one the gate INVENTS - a dispatch, a version tag,
# a pre-release tag, a non-zero major. They are only the right shapes if `on:` says so.
# =====================================================================================

# --- 61a. The hole this closes, in one line of YAML: a branch filter beside the tag filter.
#          A push to a BRANCH named `v0.9.9` is a `push` event whose ref_name is `v0.9.9`,
#          and the planning logic keys on the event name and the ref name - it cannot tell a
#          branch from a tag. `git push origin HEAD:v0.9.9` would publish a release, and
#          every assertion above would stay green, because the shape they graded is still the
#          shape the gate invented.
sed -i 's|^    tags: \["v\*"\]$|    tags: ["v*"]\n    branches: ["v0.**"]|' "$wf"
changed "$wf" "a branch filter beside the tag filter"
expect 1 "a push filter that admits a BRANCH is caught, naming what it lets through" "a push to a BRANCH"
reset

# --- 61b. The short form, which carries no filter at all, so every push to every branch runs
#          this workflow.
awk '
  /^on:$/ && !d { print "on: [push, workflow_dispatch]"; d = 1; skip = 1; next }
  skip && /^$/ { skip = 0 }
  skip { next }
  { print }
' "$wf" > "$wf.new" && mv "$wf.new" "$wf"
changed "$wf" "the short on: form"
expect 1 "an \`on:\` that is not a mapping of events to filters is caught" "The short forms"
reset

# --- 61c. A trigger nothing plans. Every assertion here is made about a shape the gate
#          planned, so an event outside that set is an entry nothing below says anything
#          about - publishing job included.
replace_line '^  workflow_dispatch:$' '  workflow_dispatch:\n  schedule:\n    - cron: "0 3 * * *"'
changed "$wf" "a trigger no shape plans"
expect 1 "a trigger this gate plans no shape for is caught by name" "which this gate plans no shape for"
reset

# --- 61d. `workflow_dispatch: inputs:` - which is how a dispatch-publish tick-box gets added.
#          release.yml's own header says there is deliberately no such input.
replace_line '^  workflow_dispatch:$' '  workflow_dispatch:\n    inputs:\n      publish:\n        type: boolean'
changed "$wf" "a dispatch input"
expect 1 "a workflow_dispatch input is caught, because it is a second dispatch shape" "A dispatch is planned as ONE shape"
reset

# --- 61e. DENY BY DEFAULT AT THE THIRD LEVEL. The job's keys and the step's keys have read
#          CLOSED since the capability split; the WORKFLOW's own did not - they were handled
#          one at a time and anything else was never looked at. Two levels saying so and the
#          third staying quiet is an asymmetry nobody reading the output can see.
replace_line '^permissions:$' 'some-future-workflow-key: whatever\npermissions:'
changed "$wf" "an unclassified workflow-level key"
expect 1 "an unclassified top-level key reads CLOSED and reds by name" \
  "the top-level key .some-future-workflow-key:., which this gate has not classified"
reset

# --- 61f. And the other direction, or 61e would be satisfied by a gate that refuses any
#          top-level key it does not itself use: a classified one still passes.
replace_line '^permissions:$' 'run-name: release ${{ github.ref_name }}\npermissions:'
changed "$wf" "a classified workflow-level key"
expect 0 "a classified top-level key is accepted"
reset

# =====================================================================================
# THE RELEASE SCRIPTS THEMSELVES. The gate does NOT read them and must not - deciding what
# a step's `run:` script does is the route six ordinals defeated. So what they do is proved
# the way scripts/install-ffmpeg.sh's failure modes are: by RUNNING them, against a
# recording stub, with every invocation compared whole. Nothing here reaches a network.
#
# Each assertion is also driven against a deliberately gutted copy of its script, because an
# assertion that could not fail is not evidence - which is the same rule case 0 exists for.
# =====================================================================================

recbin="$work/recbin"; mkdir -p "$recbin"
rec="$work/argv.log"
cat > "$recbin/docker" <<'REC'
#!/bin/sh
# Records its argv and succeeds, unless REC_FAIL globs the invocation.
printf '%s\n' "docker $*" >> "$REC_LOG"
case "docker $*" in
  ${REC_FAIL:-__never__}) exit 1 ;;
esac
exit 0
REC
chmod +x "$recbin/docker"

# release-resmoke.sh drives the packaging gate itself; here that is a recorder too, so this
# case measures WHICH references it smokes rather than re-running the real smoke test.
stub_smoke() {
  cat > "$repo/scripts/smoke-image.sh" <<'REC'
#!/bin/sh
printf '%s\n' "smoke-image.sh $*" >> "$REC_LOG"
exit 0
REC
  chmod +x "$repo/scripts/smoke-image.sh"
  changed "$repo/scripts/smoke-image.sh" "the packaging gate replaced by a recorder"
}

# run_release_script <script> <want-exit> -- runs it under the recording stub. Sets $out.
run_release_script() {
  local script="$1" want="$2" name="$3" got=0
  : > "$rec"
  out="$( cd "$repo" && PATH="$recbin:$PATH" REC_LOG="$rec" REC_FAIL="${REC_FAIL:-}" \
          IMAGE="${S_IMAGE-}" VERSION="${S_VERSION-}" FLOATING_TAG="${S_FLOATING-}" REF="${S_REF-}" \
          "./scripts/$script" 2>&1 )" || got=$?
  if [ "$got" -ne "$want" ]; then
    printf '::error::selftest: %s - %s exited %s, wanted %s\n' "$name" "$script" "$got" "$want" >&2
    printf '%s\n' "$out" | sed 's/^/       | /' >&2
    printf '%s\n' "$(cat "$rec")" | sed 's/^/       argv | /' >&2
    return 1
  fi
  return 0
}

recorded() { grep -qxF -- "$1" "$rec"; }

# --- 63. The promotion retags the exact version this run gated onto the floating reference,
#         and the retag is `imagetools create`, which does not rebuild.
S_IMAGE=ghcr.io/nschatz/holdfast S_VERSION=v0.1.0 S_FLOATING=latest S_REF='' REC_FAIL=''
promote_argv='docker buildx imagetools create -t ghcr.io/nschatz/holdfast:latest ghcr.io/nschatz/holdfast:v0.1.0'
if run_release_script release-promote.sh 0 "the promotion retags the gated version" && recorded "$promote_argv"; then
  printf '  ok: the promotion retags the gated version reference onto the floating one\n'; pass=$((pass + 1))
else
  printf '::error::selftest: the promotion did not retag %s\n' "$promote_argv" >&2
  sed 's/^/       argv | /' "$rec" >&2
  failed=$((failed + 1))
fi

# --- 64. AND THAT ASSERTION BITES. The mutation is impl-gate ordinal 1's own F5 probe: the
#         floating reference re-pointed at an image built locally that the run never pushed.
sed -i 's|"${image}:${version}"|holdfast:release|g' "$repo/scripts/release-promote.sh"
changed "$repo/scripts/release-promote.sh" "a promotion retagging from a locally built image"
run_release_script release-promote.sh 0 "a promotion from a local build" >/dev/null 2>&1 || true
if recorded "$promote_argv"; then
  printf '::error::selftest: case 63 does not bite - a promotion from a locally built image still produced the expected argv\n' >&2
  failed=$((failed + 1))
else
  printf '  ok: case 63 bites: a promotion pointing at a locally built image is not the gated retag\n'; pass=$((pass + 1))
fi
reset

# --- 65. Its named failure modes, exit code by exit code. Refusing to guess which reference
#         a release moves is the whole reason FLOATING_TAG is passed in rather than spelled.
S_IMAGE='' S_VERSION='' S_FLOATING='' REC_FAIL=''
if run_release_script release-promote.sh 2 "the promotion with nothing supplied" \
   && grep -qE 'Refusing to guess which reference' <<<"$out"; then
  printf '  ok: the promotion refuses to guess which reference to move\n'; pass=$((pass + 1))
else
  printf '::error::selftest: the promotion did not refuse an unsupplied reference by name\n' >&2
  failed=$((failed + 1))
fi

# --- 66. A retag that does not take leaves the floating reference exactly where it was, and
#         says so - which is A3's promise at the level of the one command that moves it.
S_IMAGE=ghcr.io/nschatz/holdfast S_VERSION=v0.1.0 S_FLOATING=latest
REC_FAIL='docker buildx imagetools create*'
if run_release_script release-promote.sh 3 "a retag that does not take" \
   && grep -qE 'left exactly where it was' <<<"$out"; then
  printf '  ok: a failed retag reports that the floating reference did not move\n'; pass=$((pass + 1))
else
  printf '::error::selftest: a failed retag did not report the floating reference as unmoved\n' >&2
  failed=$((failed + 1))
fi
REC_FAIL=
reset

# --- 67. The re-smoke pulls the reference that was PUSHED back out of the registry, for BOTH
#         architectures, and drives the packaging gate over each. An unqualified `docker
#         pull` resolves only the runner's own architecture, so the arm64 half would ship
#         having been gated as a local build alone.
stub_smoke
S_IMAGE='' S_VERSION='' S_FLOATING='' S_REF=ghcr.io/nschatz/holdfast:v0.1.0
resmoke_ok=1
run_release_script release-resmoke.sh 0 "the re-smoke of the pushed artefact" || resmoke_ok=0
for want in \
  'docker pull --platform linux/amd64 ghcr.io/nschatz/holdfast:v0.1.0' \
  'docker pull --platform linux/arm64 ghcr.io/nschatz/holdfast:v0.1.0' \
  'smoke-image.sh ghcr.io/nschatz/holdfast:v0.1.0' \
  'smoke-image.sh ghcr.io/nschatz/holdfast:v0.1.0 linux/arm64 --no-encode'
do
  recorded "$want" || { printf '::error::selftest: the re-smoke never ran: %s\n' "$want" >&2; resmoke_ok=0; }
done
if [ "$resmoke_ok" -eq 1 ]; then
  printf '  ok: the re-smoke pulls the pushed reference back for both architectures and smokes each\n'; pass=$((pass + 1))
else
  sed 's/^/       argv | /' "$rec" >&2
  failed=$((failed + 1))
fi

# --- 68. AND THAT ASSERTION BITES: the script gutted to `exit 0` pulls nothing and smokes
#         nothing, and the gate cannot see it - which is exactly why this case is here.
printf '#!/usr/bin/env bash\n# pulls nothing back, smokes nothing.\nexit 0\n' > "$repo/scripts/release-resmoke.sh"
chmod +x "$repo/scripts/release-resmoke.sh"
changed "$repo/scripts/release-resmoke.sh" "a gutted re-smoke"
run_release_script release-resmoke.sh 0 "a gutted re-smoke" >/dev/null 2>&1 || true
if [ -s "$rec" ]; then
  printf '::error::selftest: case 67 does not bite - a re-smoke gutted to `exit 0` still recorded invocations\n' >&2
  failed=$((failed + 1))
else
  printf '  ok: case 67 bites: a re-smoke gutted to `exit 0` pulls nothing and smokes nothing\n'; pass=$((pass + 1))
fi
reset

# --- 69. Its named failure modes.
stub_smoke
S_REF=
if run_release_script release-resmoke.sh 2 "the re-smoke with no reference" \
   && grep -qE 'no REF given' <<<"$out"; then
  printf '  ok: the re-smoke refuses when it is given no reference to pull back\n'; pass=$((pass + 1))
else
  printf '::error::selftest: the re-smoke did not refuse a missing reference by name\n' >&2
  failed=$((failed + 1))
fi

# --- 70. A pushed image that does not pull back fails the release BEFORE the promotion, which
#         is the ordering `:latest` depends on.
S_REF=ghcr.io/nschatz/holdfast:v0.1.0
REC_FAIL='docker pull*'
if run_release_script release-resmoke.sh 3 "a pushed image that does not pull back" \
   && grep -qE 'does not pull back' <<<"$out"; then
  printf '  ok: a pushed image that does not pull back fails the re-smoke by name\n'; pass=$((pass + 1))
else
  printf '::error::selftest: a failed pull-back was not reported by name\n' >&2
  failed=$((failed + 1))
fi
REC_FAIL=
S_REF=
reset

# --- 71. resolve-compose-image.sh's own preflight. The one reader is BUILT here, and the job
#         that runs `make check` (with its setup-go) is a different one - so a missing
#         toolchain has to name itself rather than surface as "docker-compose.yml names no
#         image reference", which sends the next person to read a file that is correct.
nogo="$work/nogo"; mkdir -p "$nogo"
for t in bash dirname sed awk grep cat; do
  p="$(command -v "$t" 2>/dev/null)" && ln -sf "$p" "$nogo/$t"
done
ln -sf "$recbin/docker" "$nogo/docker"
got=0
o="$( cd "$repo" && PATH="$nogo" IMAGE=ghcr.io/nschatz/holdfast VERSION=v0.1.0 \
      ./scripts/resolve-compose-image.sh 2>&1 )" || got=$?
if [ "$got" -eq 6 ] && grep -qE 'no Go toolchain on PATH' <<<"$o"; then
  printf '  ok: a missing Go toolchain names itself rather than blaming the compose file\n'; pass=$((pass + 1))
else
  printf '::error::selftest: resolve-compose-image.sh with no Go toolchain exited %s, wanted 6\n' "$got" >&2
  printf '%s\n' "$o" | sed 's/^/       | /' >&2
  failed=$((failed + 1))
fi

# =====================================================================================
# WHAT THE GATE'S OWN OUTPUT CLAIMS. A6 grades a SENTENCE as well as an exit code, and the
# two can come apart in both directions: a refusal that still reassures, and a PASS that
# states something nothing checked.
# =====================================================================================

# --- 72, 73. A green run must not claim what the release scripts DO. Both of these sentences
#         were printed by a green run until this loop, and gutting either script left both of
#         them printing: the gate reads neither file beyond an executable-bit check, and it
#         must not (the capability ruling). A sentence that states their effect reassures
#         every reader who skims stdout over a question nothing asked.
expect_absent 0 "a green run never claims the promotion is the same digest rather than a rebuild" "the same digest, not a rebuild"
expect_absent 0 "a green run never claims the re-smoke pulled anything back" "the re-smoke of the pulled artefact"

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
