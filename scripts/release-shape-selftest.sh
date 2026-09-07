#!/usr/bin/env bash
# Prove the release-shape gate still BITES. `make release-shape-selftest`.
#
# The gate (scripts/release-shape-gate, inside `make check`) is the only thing standing
# between a comment in release.yml and a one-way door: it decides, by RUNNING the
# workflow's planning shell, that a dry run publishes nothing, that `:latest` moves last,
# that a non-zero major is refused, that the operator runbook names every publishing step,
# and that the reference docker-compose.yml gives users is the one a release promotes.
#
# A gate nobody has tried to defeat is a gate nobody knows works, and every way this one
# can fail is a way it fails SILENTLY - by printing "ok" over a release that would publish
# from a dry run. So each property is defeated here on purpose, against a MUTATED COPY of
# the real inputs, and each defeat has to be red AND to say what it saw. A case that goes
# red for somebody else's reason is counted as a failure, not a pass.
#
# Case 3 is the one that decides whether this whole design was worth it: it flips the
# PLANNING SCRIPT so a manual dispatch sets publish=true, and touches not one `if:` in the
# file. A gate that matched the text of those guards stays green through it while a dry run
# pushes an image.
#
# Runs entirely inside a throwaway directory; it never mutates the working tree, and it
# publishes nothing (the gate stubs every command that could).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)" || { echo "::error::selftest: mktemp failed" >&2; exit 1; }
trap 'rm -rf "$work"' EXIT

declared=71
pass=0; failed=0

repo="$work/repo"
pristine="$work/pristine"
gate="$work/release-shape-gate"

# The inputs the gate reads. Copied from the WORKING TREE, so an uncommitted change - a fix
# or a break - is graded as it stands, which is where the mistake gets made.
#
# The three scripts the release path INVOKES are here because the gate now observes each step
# rather than reading it: it mirrors the repository, answers every invocation of a file in it
# with a recorder, and descends into a shell script so a step cannot reach a registry by
# putting the command one file away. A fixture missing one of them would be a fixture whose
# steps invoke a program that is not there, which the gate refuses rather than passes.
inputs=(
  .github/workflows/release.yml
  docker-compose.yml
  docs/release.md
  go.mod
  scripts/resolve-compose-image.sh
  scripts/smoke-image.sh
  scripts/install-ffmpeg.sh
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

# move_step <name-substring-to-move> <name-substring-to-put-it-before>
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
# A12 - a publishing step that would run on a manual dispatch.
# =====================================================================================

# --- 1. The guard deleted outright. The crudest version of the mistake.
in_step "push the multi-arch image" "/^        if:/d"
changed "$wf" "the image push with no guard at all"
expect 1 "an unguarded image push is caught on a dispatch" "on a manual dispatch.*WOULD RUN"
reset

# --- 2. `always()` reads as caution and means the opposite: it runs on a dispatch AND
#        after a failure.
in_step "push the multi-arch image" "s/^        if: .*/        if: always()/"
changed "$wf" "the image push guarded with always()"
expect 1 "an image push guarded with always() is caught on a dispatch" "on a manual dispatch.*WOULD RUN"
reset

# --- 3. THE case this gate exists for. The PLANNING SCRIPT is flipped so a dispatch sets
#        publish=true; every `if:` in the file is untouched, so the guards read exactly as
#        they did a moment ago. A gate that matched their text is green here while a dry
#        run pushes an image to a public registry. This one is only catchable by running
#        the planning logic and reading what it produced.
sed -i 's/^            publish=false$/            publish=true/' "$wf"
sed -i 's|^            version="0.0.0-dev-${GITHUB_SHA::7}"$|            version="v0.0.0-dev-${GITHUB_SHA::7}"|' "$wf"
changed "$wf" "the planning script flipped to publish on a dispatch"
grep -q "if: steps.plan.outputs.publish == 'true'" "$wf" \
  || { echo "::error::selftest: case 3 also changed a guard, so it no longer proves the planning logic is executed" >&2; exit 1; }
expect 1 "a PLANNING SCRIPT that publishes on a dispatch is caught, with every guard untouched" "on a manual dispatch.*WOULD RUN.*published act"
reset

# --- 3a to 3d. THE SAME HOLE, ONE LAYER IN. Case 3 flips the value the planning script
#     produces; these flip the value the ACTION ITSELF is handed. `docker/build-push-action`
#     publishes when its `push:` input is true, and GitHub lets that input be an EXPRESSION -
#     `push: ${{ github.event_name != 'pull_request' }}` is the action's own documented idiom
#     for a conditional push. An expression is never the literal string "true", so asking
#     whether the text says "true" reads a dry run that PUSHES as publishing nothing, and the
#     step needs no `if:` at all to get there. The input is decided through the same
#     evaluator that decides `if:` guards, and anything undecidable is red, not false.
dev_push_step() {  # dev_push_step <what to write after `push:`>
  printf '%s\n' \
    '      - name: publish a dev image so testers can pull dispatch builds' \
    '        uses: docker/build-push-action@v6' \
    '        with:' \
    '          context: .' \
    '          platforms: linux/amd64' \
    "          push: $1" \
    '          tags: ghcr.io/nschatz/holdfast:dev' \
    '' > "$work/devpush.yml"
}

# insert_before <anchor-substring> - splice $work/devpush.yml in ahead of the first line
# containing the anchor.
insert_before() {
  awk -v anchor="$1" -v block="$work/devpush.yml" '
    index($0, anchor) && !done { while ((getline l < block) > 0) print l; close(block); done = 1 }
    { print }
  ' "$wf" > "$wf.new" && mv "$wf.new" "$wf"
}

# --- 3a. True on a dispatch. This dry run pushes ghcr.io/nschatz/holdfast:dev.
dev_push_step "\${{ github.event_name == 'workflow_dispatch' }}"
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch that publishes through an expression-valued push: input"
expect 1 "an expression-valued push: that is TRUE on a dispatch is caught, naming the step" "on a manual dispatch.*publish a dev image.*WOULD RUN"
reset

# --- 3b. Undecidable: the value reads a context no run here produces. Resolving that to
#         false is exactly the fail-open; the gate must refuse.
dev_push_step "\${{ vars.PUBLISH_DEV }}"
insert_before "- name: build the release binaries"
changed "$wf" "a push: input the gate cannot decide"
expect 1 "a push: input that cannot be decided reds the gate rather than reading as harmless" "cannot be decided"
reset

# --- 3c. Not a boolean at all. `push: yes` is a STRING in YAML 1.2, and GitHub's own
#         boolean-input parser rejects it - so what this step does is unknown, not "no".
dev_push_step "yes"
insert_before "- name: build the release binaries"
changed "$wf" "a push: input that is not a boolean"
expect 1 "a push: input that is not a boolean reds the gate" "is not a boolean"
reset

# --- 3d. The other direction, and it matters as much: the gate must DECIDE these inputs,
#         not refuse every one of them. The real push step respelled with the idiomatic
#         expression is still a correct release definition and must still pass.
in_step "push the multi-arch image" 's|^          push: true$|          push: ${{ steps.plan.outputs.publish }}|'
changed "$wf" "the version-tag push respelled as an expression"
expect 0 "an expression-valued push: that is correct is DECIDED, not refused"
reset

# --- 3e to 3k. THE SECOND SPELLING OF THE SAME ACT. `push:` is not the action's only route
#     to a registry, it is a SHORTHAND for the other one: the action's own input table
#     defines `push` as "shorthand for `--output=type=registry`", and `outputs` as the list
#     of output destinations. So a step carrying `outputs: type=image,name=…,push=true` (the
#     spelling in the action's own multi-platform example) or `outputs: type=registry`, and
#     no `push:` key at all, publishes exactly as hard - while a gate that models only
#     `push:` reads the ABSENCE of that key as "a local build" and prints "NONE of them
#     publishes anything" over a dry run that pushes to a public registry. Both inputs are
#     decided, the step publishes if either says so, and an `outputs:` this gate cannot
#     decide is never a quiet no.
dev_output_step() {  # dev_output_step <what to write after `outputs:`>
  printf '%s\n' \
    '      - name: publish a dev image so testers can pull dispatch builds' \
    '        uses: docker/build-push-action@v6' \
    '        with:' \
    '          context: .' \
    '          platforms: linux/amd64' \
    "          outputs: $1" \
    '' > "$work/devpush.yml"
}

# --- 3e. The action's own README spelling. This dry run pushes ghcr.io/nschatz/holdfast:dev
#         and carries no `push:` key whatsoever.
dev_output_step "type=image,name=ghcr.io/nschatz/holdfast:dev,push=true"
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch that publishes through outputs: type=image,...,push=true"
expect 1 "an outputs: that pushes an image is caught on a dispatch, naming the step" "on a manual dispatch.*publish a dev image.*WOULD RUN"
reset

# --- 3f. buildx's own shorthand for the identical destination.
dev_output_step "type=registry"
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch that publishes through outputs: type=registry"
expect 1 "an outputs: type=registry is caught on a dispatch" "on a manual dispatch.*publish a dev image.*WOULD RUN"
reset

# --- 3g. Undecidable, exactly as 3b is for `push:`: the value reads a context no run here
#         produces, and resolving that to "local build" is the fail-open.
dev_output_step "\${{ vars.PUBLISH_DEV }}"
insert_before "- name: build the release binaries"
changed "$wf" "an outputs: input the gate cannot decide"
expect 1 "an outputs: input that cannot be decided reds the gate rather than reading as harmless" "cannot be decided"
reset

# --- 3h. An exporter this gate has never heard of. buildx gains exporters; an unmodelled
#         destination must red naming itself, not fall into the local-build bucket by
#         default.
dev_output_step "type=quay-direct,name=ghcr.io/nschatz/holdfast:dev"
insert_before "- name: build the release binaries"
changed "$wf" "an outputs: naming an exporter the gate does not model"
expect 1 "an outputs: exporter the gate does not model reds it, naming the exporter" "does not model"
reset

# --- 3i. THE HONEST OTHER DIRECTION, and it matters as much here as 3d does for `push:`:
#         `outputs:` is not a synonym for publishing. `load: true` IS `--output=type=docker`,
#         a local load, and respelling it that way must still PASS. A fix that turned any
#         `outputs:` into a publish would be caught here and nowhere else.
in_step "build the image (linux/amd64)" 's|^          load: true$|          outputs: type=docker|'
changed "$wf" "the amd64 local load respelled as outputs: type=docker"
expect 0 "a LOCAL outputs: spelling is decided as local, not refused as a publish"
reset

# --- 3j. The other honest direction, on the step that actually publishes: the real
#         version-tag push respelled with `outputs:` is a correct definition, and the act
#         must stay IN the inventory rather than vanishing from it - which is what makes the
#         runbook cross-check (A8) still demand an entry for it and gives the ordering
#         property a push to order.
in_step "push the multi-arch image" '/^          push: true$/d'
in_step "push the multi-arch image" 's@^          tags: \(.*\)$@          outputs: type=image,name=\1,push=true@'
changed "$wf" "the version-tag push respelled with outputs:"
expect 0 "the version-tag push respelled with outputs: passes AND stays in the act inventory" \
  "names every one of the 3 published act.*image-push@push-the-multi-arch-image-version-tag-only"
reset

# --- 3k. And the reference a push publishes has the same two spellings. `tags:` was the
#         only one the gate read, so a push that names the FLOATING reference inside
#         `outputs:` slipped past the check that `:latest` is never pushed directly - live
#         before the artefact has been pulled back and re-smoked.
in_step "push the multi-arch image" '/^          push: true$/d'
in_step "push the multi-arch image" 's@^          tags: \(.*\)$@          outputs: |\n            type=image,name=\1,push=true\n            type=image,name=${{ steps.plan.outputs.image }}:latest,push=true@'
changed "$wf" "the build pushing :latest through an outputs: name="
expect 1 "a build that pushes the floating reference through outputs: name= is caught" \
  "pushes ghcr.io/.*:latest directly. The floating reference would then be pullable"
reset

# --- 3l. A6 grades a SENTENCE as well as an exit code, and the two came apart: the gate
#         counted ACTS, so a step it could not decide contributed no act, `found` stayed 0,
#         and "NONE of them publishes anything" was printed in the affirmative on the very
#         run where the gate had just refused to answer. The exit code was right and nothing
#         unreviewed could ship, but a reader skimming stdout saw the reassurance rather than
#         the refusal. An undecided step must suppress that sentence, not survive it.
dev_output_step "\${{ vars.PUBLISH_DEV }}"
insert_before "- name: build the release binaries"
changed "$wf" "an undecidable step, to grade what the gate SAYS about the dispatch"
expect_absent 1 "a run the gate could not decide never claims NONE of them publishes anything" \
  "NONE of them publishes anything"
reset

# --- 3m to 3r. THE SAME ACT, DEFEATED BY PRESSING RETURN. 3a-3l are all about an action's
#     inputs; these are about the other half of the catalogue, the `run:` commands, and the
#     hole there was not a missing detector but a matcher confined to one PHYSICAL line. A
#     shell line continuation makes one logical command out of several lines, and this
#     repository writes its multi-flag commands that way - release.yml itself continues
#     `go build -trimpath \` and `gh release create ... \`. So the very command the gate
#     already names, in the spelling the repository already uses, read as performing no act
#     at all, and a dispatch that pushed to a public registry was reported as publishing
#     nothing. The fix is ONE normalisation - the shell's own line joining, applied before
#     any pattern runs - and not a second spelling per pattern, because the spelling after
#     that is always the one nobody wrote a pattern for.
dev_run_step() {  # dev_run_step <line>... - a step with no `if:`, so it runs on a dispatch
  { printf '%s\n' \
      '      - name: publish a dev image so testers can pull dispatch builds' \
      '        env:' \
      '          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}' \
      '        run: |' \
      '          set -euo pipefail'
    printf '          %s\n' "$@"
    printf '\n'
  } > "$work/devpush.yml"
}

# --- 3m. The whole finding in six lines: `--push` on the line below the build.
dev_run_step 'docker buildx build \' \
             '  --push \' \
             '  --platform linux/amd64 \' \
             '  -t ghcr.io/nschatz/holdfast:dev \' \
             '  .'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch that pushes via a line-continued docker buildx build"
expect 1 "a buildx --push written across a line continuation is caught on a dispatch, naming the step" \
  "on a manual dispatch.*publish a dev image.*WOULD RUN.*published act"
reset

# --- 3n. And it cannot be waved away as a push that would 401 anyway: the same mutation
#         plus the GHCR login unguarded, so this dry run authenticates and THEN publishes.
dev_run_step 'docker buildx build \' \
             '  --push \' \
             '  --platform linux/amd64 \' \
             '  -t ghcr.io/nschatz/holdfast:dev \' \
             '  .'
insert_before "- name: build the release binaries"
in_step "log in to GHCR" "/^        if:/d"
changed "$wf" "a dispatch that logs in to GHCR and then pushes via a continued build"
expect 1 "a continued push on a dispatch that has ALSO authenticated is caught" \
  "on a manual dispatch.*publish a dev image.*WOULD RUN.*published act"
reset

# --- 3o. The same confinement on the other command, and this one creates a published
#         RELEASE object - irreversible in exactly the sense docs/release.md records.
dev_run_step 'gh release \' \
             '  create v0.0.0-dev \' \
             '  --title "dev build" \' \
             '  --notes "for testers"'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch that cuts a release via a line-continued gh release create"
expect 1 "a gh release create written across a line continuation is caught on a dispatch, naming the step" \
  "on a manual dispatch.*publish a dev image.*WOULD RUN.*published act"
reset

# --- 3p. THE HONEST OTHER DIRECTION, and the reason this is a normalisation rather than a
#         refusal: joining lines must not turn every continued build into a publish. A
#         continued build with no `--push` is a LOCAL build and must still pass.
dev_run_step 'docker buildx build \' \
             '  --load \' \
             '  --platform linux/amd64 \' \
             '  -t holdfast:dev \' \
             '  .'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch running a continued build that stays local"
expect 0 "a line-continued build with no --push is still decided as local, not refused as a publish"
reset

# --- 3q. Parity, not presence. `\\` is an ESCAPED backslash: the shell ends the command
#         there and runs the next line separately, so `--push .` is its own (failing)
#         command and nothing is published. Joining on any trailing backslash would splice
#         two commands the shell keeps apart and report an act the definition cannot perform.
dev_run_step 'docker buildx build -t ghcr.io/nschatz/holdfast:dev . \\' \
             '  --push'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch whose build ends in an escaped backslash, not a continuation"
expect 0 "an ESCAPED trailing backslash is not read as a continuation"
reset

# --- 3r. A continuation on the LAST line of a run block continues into nothing. The join
#         has to end the string rather than reach past it, and the act on the line above
#         still has to be seen.
dev_run_step 'docker buildx build --push -t ghcr.io/nschatz/holdfast:dev \' \
             '  . \'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch whose publishing run block ends in a dangling continuation"
expect 1 "a dangling continuation at the end of a run block is handled and the push still caught" \
  "on a manual dispatch.*publish a dev image.*WOULD RUN.*published act"
reset

# --- 3s and 3t. THE CATALOGUE'S EDGE, which the gate never used to state. `usesDetectors`
#     answers "does this action publish?" with yes or with silence, and silence read as no:
#     an action in neither half of the catalogue contributed no act AND no message, so
#     "NONE of them publishes anything" covered a step the gate had never asked about. Every
#     `uses:` must now be classified, one way or the other, by a human who wrote down why.

# --- 3s. An action outside both lists - and not a contrived one: docker/bake-action takes
#         `push: true` and publishes exactly as hard as the action beside it in the file.
printf '%s\n' \
  '      - name: publish a dev image so testers can pull dispatch builds' \
  '        uses: docker/bake-action@v5' \
  '        with:' \
  '          push: true' \
  '' > "$work/devpush.yml"
insert_before "- name: build the release binaries"
changed "$wf" "a step using an action the gate has never classified"
expect 1 "an action in neither half of the catalogue reds the gate, naming it" \
  "uses .docker/bake-action@v5., and this gate does not classify that action"
reset

# --- 3t. The other direction: stating the boundary must not become a refusal of every
#         `uses:`. An action already checked and classified as local stays passable, even
#         added unguarded on the dispatch path.
printf '%s\n' \
  '      - name: log in to GHCR again, unguarded' \
  '        uses: docker/login-action@v3' \
  '        with:' \
  '          registry: ghcr.io' \
  '          username: ${{ github.actor }}' \
  '          password: ${{ secrets.GITHUB_TOKEN }}' \
  '' > "$work/devpush.yml"
insert_before "- name: build the release binaries"
changed "$wf" "an unguarded step using an action classified as publishing nothing"
expect 0 "a classified non-publishing action still passes, unguarded, on a dispatch"
reset

# --- 3u to 3ab. THE `run:` HALF'S DESTINATION MODEL, AND ITS EDGE. 3m-3r fixed how the text
#     is READ; these are about what is read OUT of it. The catalogue used to know exactly one
#     destination - the literal flag `--push` - so `docker buildx build --output=type=registry`
#     (the thing `push:` is documented shorthand FOR, and the spelling 3e-3k closed on the
#     `uses:` half) performed no act, and neither did `docker image push`, the
#     management-command form of `docker push`. Neither involves a continuation. A command's
#     destination is now decided from the flags that SET it, through the SAME function that
#     decides an action's `outputs:` input, and every invocation of a tool that can reach a
#     registry has to land on a rule: an invocation on none of them is UNDECIDED and reds by
#     name, which is why `crane copy` reds without appearing anywhere in the gate.

# --- 3u. buildx's own longhand for a registry push. This dry run publishes.
dev_run_step 'docker buildx build --platform linux/amd64 --output=type=registry,name=ghcr.io/nschatz/holdfast:dev .'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch that pushes through --output=type=registry"
expect 1 "a run-step build that pushes through --output=type=registry is caught on a dispatch" \
  "on a manual dispatch.*publish a dev image.*WOULD RUN.*published act"
reset

# --- 3v. The short flag with the image exporter's own attributes.
dev_run_step 'docker buildx build --platform linux/amd64 -o type=image,name=ghcr.io/nschatz/holdfast:dev,push=true .'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch that pushes through -o type=image,...,push=true"
expect 1 "a run-step build that pushes through -o type=image,...,push=true is caught" \
  "on a manual dispatch.*publish a dev image.*WOULD RUN.*published act"
reset

# --- 3w. The plainest one: a word between `docker` and `push` used to hide the act.
dev_run_step 'docker image push ghcr.io/nschatz/holdfast:dev'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch that runs docker image push"
expect 1 "docker image push, the management-command spelling, is caught on a dispatch" \
  "on a manual dispatch.*publish a dev image.*WOULD RUN.*published act"
reset

# --- 3x. THE HONEST OTHER DIRECTION, and it is the one that makes this a destination model
#         rather than a refusal: `--output=` is not a synonym for publishing. The `docker`
#         exporter writes a local tar and must still pass. A fix that red every `--output=`
#         would be caught here and nowhere else.
dev_run_step 'docker buildx build --platform linux/amd64 --output=type=docker,dest=/tmp/img.tar .'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch running a build whose exporter is local"
expect 0 "a LOCAL --output= exporter in a run step is decided as local, not refused as a publish"
reset

# --- 3y. THE EDGE. `crane copy` appears nowhere in the gate, and that is the point: an
#         invocation of a registry tool that lands on no rule is UNDECIDED, and undecided
#         reds by name. A catalogue that answers an unknown spelling with silence has to grow
#         a row per spelling, and the spelling after that is always the one nobody added.
dev_run_step 'crane copy ghcr.io/nschatz/holdfast:dev ghcr.io/nschatz/holdfast:latest'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch running a registry command the gate has never been taught"
expect 1 "a registry command in no rule reds the gate, naming the invocation" \
  "does not model that invocation of .crane."
reset

# --- 3z. THE TWO READERS' DISAGREEMENT, which was fail-open. Reading a `run:` block used to
#         take two passes - strip comments per PHYSICAL line, then join continuations - and
#         they disagreed about what a line is: quote state reset at exactly the boundary the
#         join erased. A `#` inside a quoted argument on a continuation line was therefore
#         read as a comment, and the rest of the logical line was deleted, the `--push` and
#         the backslash that would have joined it included. An OCI annotation carrying an
#         issue number is the everyday way that `#` arrives, and bash really does perform the
#         push.
dev_run_step 'docker buildx build \' \
             '  --annotation "org.opencontainers.image.description=dev build, \' \
             '  see #123" \' \
             '  --push \' \
             '  --platform linux/amd64 \' \
             '  -t ghcr.io/nschatz/holdfast:dev \' \
             '  .'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch whose push sits after a quoted # across a continuation"
expect 1 "a quoted # on a continuation line does not hide the --push below it" \
  "on a manual dispatch.*publish a dev image.*WOULD RUN.*published act"
reset

# --- 3aa. And the other direction for the same shape: the identical argument on a build that
#          stays LOCAL must still pass, so "quotes survive a continuation" cannot be
#          satisfied by calling every annotated build a publish.
dev_run_step 'docker buildx build \' \
             '  --annotation "org.opencontainers.image.description=dev build, \' \
             '  see #123" \' \
             '  --load \' \
             '  --platform linux/amd64 \' \
             '  -t holdfast:dev \' \
             '  .'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch whose annotated build stays local"
expect 0 "the same quoted # on a build that only loads locally is still local"
reset

# --- 3ab. The act belongs to the program that performs it, not to the word at the start of
#          the line. `sudo docker push` is a push.
dev_run_step 'sudo docker push ghcr.io/nschatz/holdfast:dev'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch pushing through a wrapper command"
expect 1 "a push behind a wrapper command is still caught on a dispatch" \
  "on a manual dispatch.*publish a dev image.*WOULD RUN.*published act"
reset

# --- 3ac..3ai. THE OBSERVED GRADE ROUTE. Reading a `run:` step lost six times in one
#         direction, so A6, A7 and A12 are no longer decided by reading it: each step is RUN
#         in an environment where nothing external executes and every invocation is recorded
#         with the argv bash built. These cases are the spellings that beat the reader, and
#         the ones the environment itself has to refuse rather than pass.

# --- 3ac. F11. A push inside a QUOTED WORD handed to a nested shell. To any lexer this is
#          the single argv element `docker push …` belonging to `sh`; to the shell it is a
#          push, and the observation follows the nested shell into it.
dev_run_step 'sh -c "docker push ghcr.io/nschatz/holdfast:dev"'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch pushing from inside a quoted sh -c"
expect 1 "a push inside a quoted sh -c is caught on a dispatch" \
  "on a manual dispatch.*publish a dev image.*WOULD RUN.*published act"
reset

# --- 3ad. F11b. The same through `eval`, where stepping over the wrapper left the quoted
#          string as the program name.
dev_run_step 'eval "docker push ghcr.io/nschatz/holdfast:dev"'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch pushing from inside an evaled string"
expect 1 "a push inside an evaled string is caught on a dispatch" \
  "on a manual dispatch.*publish a dev image.*WOULD RUN.*published act"
reset

# --- 3ae. F12. buildx's `-o` is a pflag shorthand and pflag takes an ATTACHED value, so
#          `-otype=registry` IS `--output=type=registry` and really publishes - measured
#          against a real buildx. A switch over flag spellings decided it "local".
dev_run_step 'docker buildx build --platform linux/amd64 -otype=registry,name=ghcr.io/nschatz/holdfast:dev .'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch pushing through buildx's attached short flag"
expect 1 "a push through the attached shorthand -otype=registry is caught" \
  "on a manual dispatch.*publish a dev image.*WOULD RUN.*published act"
reset

# --- 3af. THE OTHER DIRECTION for 3ae, and the one that keeps this a reading of the flag
#          rather than a refusal of it: the same attached shorthand naming a LOCAL exporter
#          must still pass.
dev_run_step 'docker buildx build --platform linux/amd64 -otype=docker,dest=/tmp/img.tar .'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch whose attached short flag names a local exporter"
expect 0 "an attached -otype=docker,dest= is decided as local, not refused as a publish"
reset

# --- 3ag. DENY BY DEFAULT. Every program an observed step invokes has to be classified. One
#          that is not FAILS naming it, which is what turns the next unseen spelling into a
#          loud stop instead of the seventh fail-open.
dev_run_step 'rclone copy dist ghcr:holdfast'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch invoking a program the gate has never been taught"
expect 1 "a program in none of the gate's lists reds by name rather than reading as harmless" \
  "does not classify that program"
reset

# --- 3ah. A COMMAND THE ENVIRONMENT CANNOT WATCH. An absolute path is resolved by bash
#          itself, so a program at one this gate has not shimmed would run for real and be
#          recorded nowhere. It is refused before it runs.
dev_run_step '/opt/vendor/bin/docker push ghcr.io/nschatz/holdfast:dev'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch invoking a program by an unshimmed absolute path"
expect 1 "an invocation this environment could not watch is refused, not passed" \
  "could NOT OBSERVE"
reset

# --- 3ai. THE PATH BEHIND A FAILURE. A publish reached only when an earlier command fails is
#          never on the all-succeed path, so the environment re-runs the step making each of
#          its commands fail in turn. One run down the happy path reports this as clean.
dev_run_step 'if ! docker manifest inspect ghcr.io/nschatz/holdfast:dev; then' \
             '  docker push ghcr.io/nschatz/holdfast:dev' \
             'fi'
insert_before "- name: build the release binaries"
changed "$wf" "a dispatch that pushes only when an inspect fails"
expect 1 "a push reachable only when an earlier command fails is still caught" \
  "on a manual dispatch.*publish a dev image.*WOULD RUN.*published act"
reset

# --- 3aj. F13, in the ordering half. A step that PRINTS the gate's name and runs nothing
#          satisfied the full-gate role once quoting was resolved, so A7 read the promotion
#          as gated. A role is now what the step was observed to invoke.
in_step "the full gate (make check)" 's|run: make check|run: echo "make check"|'
changed "$wf" "a full gate reduced to an echo of its own name"
expect 1 "a step that only prints the gate's name does not satisfy the full-gate role" \
  "the full gate .make check. does not run before"
reset

# --- 3ak. THE OTHER DIRECTION for 3aj: the real gate, and a spelling of it that is still the
#          gate, must both still read as one.
in_step "the full gate (make check)" 's|run: make check|run: make -C . check|'
changed "$wf" "the full gate spelled with a directory flag"
expect 0 "a make invocation that really runs the check target still reads as the full gate"
reset

# =====================================================================================
# A13 / A3 - the floating reference moving before the artefact was proved.
# =====================================================================================

# --- 4. Promotion hoisted above the push. `:latest` would then point at a tag that does
#        not exist yet, and would be moved without the pushed artefact being gated at all.
move_step "promote :latest" "push the multi-arch image"
changed "$wf" "the promotion hoisted above the version-tag push"
expect 1 "a promotion that runs before the version-tag push is caught" "runs AFTER .*moves the floating reference"
reset

# --- 5. Promotion hoisted above the re-smoke of the PULLED artefact. This is the exact
#        ordering the workflow's own comment claims and nothing enforced: the push is a
#        cache rebuild, so `:latest` would be promoted onto bytes nobody smoked.
move_step "promote :latest" "smoke test the PUSHED image"
changed "$wf" "the promotion hoisted above the re-smoke"
expect 1 "a promotion that runs before the re-smoke of the pulled artefact is caught" "re-smoke of the linux/(amd|arm)64 artefact.*runs AFTER"
reset

# --- 6. The floating reference pushed directly by the build, so it is live before the
#        artefact is pulled back at all - which is what promoting it separately avoids.
in_step "push the multi-arch image" 's|^\( *tags: .*\)$|\1,${{ steps.plan.outputs.image }}:latest|'
changed "$wf" "the build pushing :latest itself"
expect 1 "a build that pushes the floating reference itself is caught" "pushes ghcr.io/.*:latest directly"
reset

# --- 7. The promotion marked `always()`. It then runs after the gate or a smoke run has
#        FAILED - the precise state in which `:latest` must stay where it was.
in_step "promote :latest" "s/^        if: .*/        if: always() \&\& steps.plan.outputs.publish == 'true'/"
changed "$wf" "the promotion guarded with always()"
expect 1 "a promotion that survives a failed gate is caught" "still runs after an earlier step has FAILED"
reset

# =====================================================================================
# A14 - a step before the promotion whose failure does not fail the run.
# =====================================================================================

# --- 8. The full gate, tolerated. `make check` reds, the run stays green, `:latest` moves
#        onto an image whose verify/swap logic was never proved.
in_step "the full gate" 's|^      - name: .*|&\n        continue-on-error: true|'
changed "$wf" "the full gate marked continue-on-error"
expect 1 "a tolerated failure on the full gate is caught" 'marked .continue-on-error: true'
reset

# --- 9. The same, on the re-smoke of the pushed artefact.
in_step "smoke test the PUSHED image" 's|^      - name: .*|&\n        continue-on-error: true|'
changed "$wf" "the re-smoke marked continue-on-error"
expect 1 "a tolerated failure on the re-smoke is caught" 'marked .continue-on-error: true'
reset

# =====================================================================================
# A15 - a definition that cannot be read, cannot be parsed, or names no step. Each must
# say WHICH; a vacuous pass over nothing is the failure mode all three share.
# =====================================================================================

# --- 10.
rm -f "$wf"
expect 1 "a release definition that cannot be read is red, and says so" "CANNOT BE READ"
reset

# --- 11.
printf '\nthis is not: [valid: yaml\n' >> "$wf"
changed "$wf" "an unparseable release definition"
expect 1 "a release definition that cannot be parsed is red, and says so" "CANNOT BE PARSED"
reset

# --- 12.
: > "$wf"
expect 1 "an empty release definition is red, and says so" "IS EMPTY"
reset

# --- 13. A job whose step list is empty. Every ordering assertion below would otherwise
#         hold over nothing and report green.
awk '/^    steps:$/ { print "    steps: []"; exit } { print }' "$pristine/.github/workflows/release.yml" > "$wf"
changed "$wf" "a job with no steps"
expect 1 "a release definition that names no step is red, and says so" "NAMES NO STEP AT ALL|names NO step that performs a published act"
reset

# --- 14. No planning step at all: nothing writes to $GITHUB_OUTPUT, so there is no runtime
#         value to decide any guard from. The gate must refuse rather than fall back to
#         reading the text, which is the whole thing it is not allowed to do.
sed -i 's/GITHUB_OUTPUT/GITHUB_NOWHERE/g' "$wf"
changed "$wf" "a release definition with no planning step"
expect 1 "a definition whose planning step produces nothing is red" "NO planning step"
reset

# =====================================================================================
# A16 / A8 - the operator runbook.
# =====================================================================================

# --- 15. No runbook at all. Three irreversible acts would then be checked against nothing.
rm -f "$runbook"
expect 1 "a missing operator runbook is red, and lists the acts it should have named" "CANNOT BE READ"
reset

# --- 16. A runbook that exists and names no act. This is the vacuous pass A16 names: a
#         document can be present, long, and about nothing.
printf '# Releasing\n\nAsk Noah.\n' > "$runbook"
changed "$runbook" "a runbook that names no irreversible act"
expect 1 "a runbook that names NO irreversible act is red" "names NO irreversible act"
reset

# --- 17. One act quietly dropped from the runbook - what happens when a publishing step is
#         added and the documentation is not.
sed -i 's/`github-release@cut-the-github-release`/(dropped)/' "$runbook"
changed "$runbook" "a runbook missing one act"
expect 1 "a runbook that stops naming one publishing step is red, naming the id it needs" 'Add .github-release@cut-the-github-release.'
reset

# =====================================================================================
# A17 / A9 - the example deployment's image reference.
# =====================================================================================

# --- 18. The reference removed. An absent reference is not agreement.
sed -i '/^ *image: ghcr/d' "$compose"
changed "$compose" "a compose file with no image reference"
expect 1 "an example deployment with no image reference is red, naming the file" "docker-compose.yml NAMES NO IMAGE REFERENCE"
reset

# --- 19. The reference made unreadable by breaking the file around it.
printf '\nthis: [is: broken\n' >> "$compose"
changed "$compose" "an unparseable compose file"
expect 1 "an unparseable example deployment is red, naming the file" "docker-compose.yml CANNOT BE PARSED"
reset

# --- 20.
rm -f "$compose"
expect 1 "a missing example deployment is red, naming the file" "docker-compose.yml CANNOT BE READ"
reset

# --- 21. The disagreement itself: the compose file names an image this repository's
#         release would never produce. This is what a repository rename does, silently.
sed -i 's|^\( *image: \).*|\1ghcr.io/someone-else/holdfast:latest|' "$compose"
changed "$compose" "a compose reference naming a different repository"
expect 1 "a compose reference nothing publishes is red, and prints both" "ghcr.io/someone-else/holdfast:latest"
reset

# =====================================================================================
# A4 / A10 - the major-version-zero refusal, and the record it has to name.
# =====================================================================================

# --- 22. The refusal removed. A v1.0.0 tag would then publish, and 1.0.0 "defines the
#         public API" over three surfaces this project has not frozen.
sed -i 's/^          if \[ "$publish" = "true" \]; then$/          if false; then/' "$wf"
changed "$wf" "the major-version-zero refusal removed"
expect 1 "a release path that accepts a non-zero major is red" "ACCEPTS v1.0.0"
reset

# --- 23. The refusal kept, but stripped of the record it points at. "No" without "go and
#         read this first" is how the refusal gets deleted by the next person in a hurry.
sed -i 's|docs/release.md|the stability record|g' "$wf"
changed "$wf" "a refusal that names no record"
expect 1 "a refusal that names no record is red" "names no record"
reset

# --- 24. The refusal pointing at a document that does not exist - prose again.
sed -i 's|docs/release.md|docs/stability.md|g' "$wf"
changed "$wf" "a refusal naming a record that does not exist"
expect 1 "a refusal naming a record that does not exist is red" "docs/stability.md, which does not exist"
reset

# =====================================================================================
# A11 - the post-promotion resolution of the example deployment's reference.
# =====================================================================================

# --- 25. The step deleted. Every release after the first would then stop checking that the
#         reference users actually pull resolves at all.
r="$(block_range "$wf" "must resolve to the gated digest")"
[ -n "$r" ] || { echo "::error::selftest: could not find the resolution step" >&2; exit 1; }
# shellcheck disable=SC2086
set -- $r
sed -i "$1,$2d" "$wf"
changed "$wf" "the resolution step deleted"
expect 1 "a release that never resolves the example deployment's reference is red" "no step resolves the example deployment"
reset

# --- 26. The step kept, but moved before the promotion, where it would resolve the
#         PREVIOUS release's digest and pass while this one is broken.
move_step "must resolve to the gated digest" "promote :latest"
changed "$wf" "the resolution step moved before the promotion"
expect 1 "a resolution that runs before the promotion is red" "BEFORE the promotion"
reset

# --- 27 to 30. scripts/resolve-compose-image.sh itself, driven against a fake registry.
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

# --- 30a. The shape that made two readers dangerous: a SECOND service with its own
#          `image:`. There is one reader now - the gate's YAML decoder, asked for by
#          `-print-compose-ref` - and it REFUSES rather than silently grading whichever
#          service came first, which is what a `sed … | head -1` did.
printf '\n  sidecar:\n    image: ghcr.io/nschatz/something-else:latest\n' >> "$compose"
changed "$compose" "a compose file whose second service carries an image"
printf 'ghcr.io/nschatz/holdfast:v0.1.0 sha256:aaa\nghcr.io/nschatz/holdfast:latest sha256:aaa\nghcr.io/nschatz/something-else:latest sha256:aaa\n' > "$digests"
resolve "a second service's image reference is refused, not silently ignored" 3 "names 2 image references"
reset

# --- 30b. The DEFAULT reader path, which is the one a real release takes: no binary handed
#          over, so the script builds the single reader itself. Driven against the real
#          working tree, so the whole chain - script, reader, docker-compose.yml - is the
#          committed one and not a fixture.
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

# --- 31. And the gate has to still be IN `make check`. A target nothing depends on is a
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
