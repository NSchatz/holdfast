#!/usr/bin/env bash
# Prove the guards in check-pins.sh still BITE: the rename guard and the pin assertions.
# Part of `make check`.
#
# Four families of case live here. Cases 0-6 defeat the RENAME guard; cases 7-17 (S0022)
# defeat the FFMPEG PIN guard - the floating alias, the short-retention daily build, a
# blanked digest, and a NOTICE that has drifted from the Dockerfile it is supposed to be
# the source offer for; case 18 (S0024) defeats the GO TOOLCHAIN pin's digest half; cases
# 19-29 (S0057) defeat the SUPPLY-CHAIN pin guards - a mutable action reference, an
# unreadable one, a compose image that lost its digest or gained a `latest` tag, a base
# image ARG that lost its digest or its tag, a node manifest with no lifecycle-script
# decision, and a file the gate reads going missing. Two of those cases assert a PASS
# rather than a bite (the local-action exemption, and a manifest whose decision is
# recorded), because a guard that refuses everything is indistinguishable from a guard
# that works and is impossible to comply with. One asserts that publishing `:latest` is
# still allowed, which is the refusal these checks must NOT make.
#
# It exists because that guard degraded to a silent GREEN — printing "ok" over a real leak —
# several times while it was being written, and every one of those was invisible in a green
# build. The three that survive as cases below:
#
#   - `git grep -I` SKIPS binary files (`-a` searches them). With `-I` the guard reported
#     clean over a tracked binary carrying the old auth-token env var.
#   - The allowlist was tested against git grep's `path:lineno:content` line, which made it a
#     FILE-level exemption by accident: any PATH containing the allow term exempted the whole
#     file.
#   - `git grep` with no rev reads the WORKTREE, but `git commit` ships the INDEX.
#   - git's own exit status was swallowed, so a `dubious ownership` failure (128) turned the
#     guard into a no-op that reported success.
#
# A guard nobody has tried to defeat is a guard nobody knows works. This defeats it on
# purpose, on every run.
#
# NOTE: this file must name the identifiers it forbids in order to build its fixtures, and it
# deliberately does NOT take an allowlist exemption to do so — it COMPOSES them at runtime, so
# the source never contains the banned string. An exemption in the one file whose job is
# proving there are none would be the hole it is testing for.
#
# Runs entirely inside a throwaway clone; it never mutates the working tree.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)" || { echo "::error::selftest: mktemp failed" >&2; exit 1; }
trap 'rm -rf "$work"' EXIT

# The banned identifiers, composed so this file does not itself contain them.
OLD_ENV="TRANSCODE""_SERVER_AUTH_TOKEN"
OLD_CRF="TRANSCODE""_CRF"
OLD_METRIC="transcode""_files_total"

declared=30
pass=0; failed=0
repo="$work/repo"

git clone -q --no-hardlinks --depth 1 "file://$here" "$repo" 2>/dev/null \
  || { echo "::error::selftest could not clone the repo — it did NOT run" >&2; exit 1; }
git -C "$repo" config user.email t@t.t
git -C "$repo" config user.name t

# Grade the tree AS IT STANDS NOW. The clone carries HEAD, so the whole working tree is
# overlaid on top of it: the clean-tree baseline (case 0) is the reference every "must bite"
# case is measured against, and taking it from HEAD means an uncommitted change anywhere —
# a fix OR a break — is graded as if it did not exist. In CI the two are identical and this is
# a no-op; locally they are not, and locally is where the mistake gets made.
tar -C "$here" --exclude=.git -cf - . | tar -C "$repo" -xf - \
  || { echo "::error::selftest: could not overlay the working tree — it did NOT run" >&2; exit 1; }
git -C "$repo" add -A
git -C "$repo" commit -qm "selftest: grade the working tree, not HEAD" --allow-empty

cmp -s "$here/scripts/check-pins.sh" "$repo/scripts/check-pins.sh" \
  || { echo "::error::selftest: the clone's guard is not the working-tree guard — it graded the wrong thing" >&2; exit 1; }

# The output is CAPTURED, not discarded. check-pins.sh now makes several independent
# assertions, and once a script has more than one reason to fail, "it exited 1" stops
# being evidence that the assertion under test is the one that bit. A case that names a
# message is graded on the message too, so a case cannot go green off somebody else's
# failure - which is the selftest version of the silent-green bug this file exists for.
guard_out=""
guard() { guard_out="$( cd "$repo" && bash scripts/check-pins.sh 2>&1 )"; }

# expect <want-exit> <name> [must-mention-regex].  0 = must pass, 1 = must bite.
expect() {
  local want="$1" name="$2" want_msg="${3:-}" got=0
  guard || got=$?
  if [ "$got" -ne "$want" ]; then
    printf '::error::selftest: %s — guard exited %s, wanted %s\n' "$name" "$got" "$want" >&2
    printf '%s\n' "$guard_out" | sed 's/^/       | /' >&2
    failed=$((failed + 1))
  elif [ -n "$want_msg" ] && ! grep -qE -- "$want_msg" <<<"$guard_out"; then
    printf '::error::selftest: %s: exited %s (correct) but for the WRONG REASON, nothing in its output matched /%s/\n' "$name" "$got" "$want_msg" >&2
    printf '%s\n' "$guard_out" | sed 's/^/       | /' >&2
    failed=$((failed + 1))
  else
    printf '  ok: %s\n' "$name"; pass=$((pass + 1))
  fi
}

# Move the ffmpeg pin in the CLONE. NOTICE is moved with it deliberately: the two must
# agree, and a case about the tag's SHAPE that also broke that agreement would be graded
# on whichever assertion happened to fire first.
set_build() {
  sed -i -e "s|^ARG FFMPEG_BUILD=.*|ARG FFMPEG_BUILD=$1|" "$repo/Dockerfile"
  sed -i -E "s|^( *Build tag *: *).*|\1$1|" "$repo/NOTICE"
}

reset() { git -C "$repo" reset -q --hard HEAD; git -C "$repo" clean -qfdx; }

# --- 0. A clean tree passes. Without this, every "bites" case below could be a guard that
#        simply fails on everything, which would prove nothing.
expect 0 "a clean tree passes"

# --- 1. A pre-rename identifier in ordinary source.
printf '\n// leak: %s\n' "$OLD_CRF" >> "$repo/internal/version/version.go"
expect 1 "a pre-rename identifier in source is caught"
reset

# --- 2. The same identifier inside a BINARY file. Pins `-a` over `-I`: with `-I`, git grep
#        skips binary files entirely and this case, alone, goes green.
printf 'SQLite format 3\000 fixture %s=synthetic\000' "$OLD_ENV" > "$repo/testdata/jobs.db"
git -C "$repo" add -f testdata/jobs.db
expect 1 "a pre-rename identifier inside a BINARY file is caught (pins -a over -I)"
reset

# --- 3. The allowlist is LINE-level, not FILE-level: a PATH matching the allow term must not
#        exempt the leaks inside it.
mkdir -p "$repo/testdata"
printf '%s=synthetic\n%s 1\n' "$OLD_ENV" "$OLD_METRIC" > "$repo/testdata/rename-guard-allow.conf"
git -C "$repo" add -f testdata/rename-guard-allow.conf
expect 1 "a leak in a file whose PATH matches the allow marker is still caught"
reset

# --- 4. A leak STAGED but deleted from the worktree is what `git commit` will ship, while
#        `git grep` (no rev) reads the worktree. Pins the --cached pass.
printf '%s=synthetic\n' "$OLD_ENV" > "$repo/testdata/staged.conf"
git -C "$repo" add -f testdata/staged.conf
rm -f "$repo/testdata/staged.conf"
expect 1 "a leak STAGED but deleted from the worktree is caught (pins the --cached scan)"
reset

# --- 5. The marker genuinely exempts the LINE that carries it — the rule in CLAUDE.md has to
#        quote the identifiers it forbids, so if this could not pass, the rule could not be
#        written down at all. Planted with a real banned identifier, not asserted on the clean
#        tree (case 0 already does that, and it would be a tautology here).
printf '\n// %s is banned. rename-guard-allow\n' "$OLD_CRF" >> "$repo/internal/version/version.go"
expect 0 "a banned identifier on a line carrying the marker is exempt (the rule can state itself)"
reset

# --- 6. A failing git must be RED, never green. `fatal: detected dubious ownership` (128) is
#        routine in containerised CI when the checkout UID differs from the runner's, and a
#        swallowed exit code turned this guard into a no-op that printed "ok".
fake="$work/fakebin"; mkdir -p "$fake"
printf '#!/bin/sh\necho "fatal: detected dubious ownership in repository" >&2\nexit 128\n' > "$fake/git"
chmod +x "$fake/git"
got=0
( cd "$repo" && PATH="$fake:$PATH" bash scripts/check-pins.sh >/dev/null 2>&1 ) || got=$?
if [ "$got" -ne 0 ]; then
  printf '  ok: a failing git is RED, not a silent pass\n'; pass=$((pass + 1))
else
  printf '::error::selftest: a failing git reported GREEN — the guard did not run and said nothing\n' >&2
  failed=$((failed + 1))
fi

# =====================================================================================
# The ffmpeg-pin assertions (S0022). Everything above proves the RENAME guard bites;
# these prove the PIN guard does. The pin that broke was autobuild-2026-07-13-14-11, a
# mid-month daily, and upstream keeps only the last 14 dailies - so it 404ed on schedule
# and took every job that installs ffmpeg down with it, on unrelated pull requests, in
# three different specs. check-pins.sh now refuses that pin at the only moment somebody
# is looking. A refusal nobody has tried to provoke is a refusal nobody knows still works.
# =====================================================================================

# --- 7. The floating alias. It never 404s, which is precisely the problem: it would
#        change the encoder AND the libvmaf instrument the no-loss verdict is measured
#        with, under a permanently green build.
set_build "latest"
expect 1 "a floating 'latest' pin is refused" "not a dated upstream release tag"
reset

# --- 8. The other shape of the same mistake.
set_build "autobuild-latest"
expect 1 "an 'autobuild-latest' pin is refused as undated" "not a dated upstream release tag"
reset

# --- 9. The actual dead pin from the incident. The message must name the POLICY, not
#        just say no: the next person needs to know what to pin instead, and why.
set_build "autobuild-2026-07-13-14-11"
expect 1 "the mid-month daily build that caused S0022 is refused" "MID-MONTH DAILY build"
reset

# --- 10. A date that does not exist is not a release tag either.
set_build "autobuild-2026-02-30-13-00"
expect 1 "an impossible calendar date is refused" "impossible date"
reset

# --- 11 and 12. The month-end test is real calendar arithmetic, not a string match on
#        -30/-31. February in a leap year is the case that separates the two, and it has
#        to work in BOTH directions or the guard is either useless or unusable.
set_build "autobuild-2024-02-29-13-00"
expect 0 "a leap-year 29 February pin PASSES (it is that month's last day)"
reset

set_build "autobuild-2024-02-28-13-00"
expect 1 "28 February in a LEAP year is refused (it is not that month's last day)" "MID-MONTH DAILY build"
reset

# --- 13 and 14. The digest is the half of the pin that says which BYTES, and blanking it
#        is the cheapest way to make a rotted pin appear to work.
sed -i -e "s|^ARG FFMPEG_SHA256_AMD64=.*|ARG FFMPEG_SHA256_AMD64=|" "$repo/Dockerfile"
expect 1 "a blanked FFMPEG_SHA256_AMD64 is refused" "not a 64-character lowercase sha256 digest"
reset

sed -i -e "s|^ARG FFMPEG_SHA256_ARM64=.*|ARG FFMPEG_SHA256_ARM64=deadbeef|" "$repo/Dockerfile"
expect 1 "a truncated FFMPEG_SHA256_ARM64 is refused" "not a 64-character lowercase sha256 digest"
reset

# --- 15 and 16. NOTICE is the GPL source offer and travels INSIDE the image, where no
#        Dockerfile is available to point at. If it drifts, the image redistributes GPL
#        binaries whose corresponding-source record names a different build. The guard for
#        that has existed since TRANSCODE-9 and had never been provoked.
sed -i -E "s|^( *Build tag *: *).*|\1autobuild-2026-06-30-13-34|" "$repo/NOTICE"
expect 1 "a NOTICE naming a different build tag is refused" "NOTICE does not match the Dockerfile"
reset

sed -i -E "s|^( *Version *: *).*|\1N-999999-gdeadbeef99|" "$repo/NOTICE"
expect 1 "a NOTICE naming a different version is refused" "NOTICE does not match the Dockerfile"
reset

# --- 17. The restatement nobody was checking. NOTICE used to name the git revision a
#        THIRD time in its corresponding-source paragraph, on a line no assertion matched,
#        so the two checked lines could be bumped correctly while the source offer went on
#        naming a build the image no longer contains.
printf '\n  (previously built from revision g90436de5e1.)\n' >>"$repo/NOTICE"
expect 1 "a stray ffmpeg build identifier elsewhere in NOTICE is refused" "identifier that is NOT the pinned one"
reset

# =====================================================================================
# The Go-toolchain pin assertion (S0024). Same failure shape as the ffmpeg block above,
# on the other pin: the half of a pin that names the BYTES rather than the URL.
# =====================================================================================

# --- 18. GO_IMAGE with its digest dropped. Section 3 compares only the TAG, so the three
#         files still agree and it prints "ok" while the toolchain floats to whatever the
#         registry serves that day. The Dockerfile's in-image `go env GOVERSION` assertion
#         cannot catch this one either (a floating tag agrees with itself), so the "is a
#         digest pinned at all" half has to hold here.
sed -i 's/^\(ARG GO_IMAGE=[^@]*\)@sha256:[0-9a-f]*$/\1/' "$repo/Dockerfile"
grep -qE '^ARG GO_IMAGE=golang:[^@]+$' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not strip the GO_IMAGE digest, so this case did NOT run" >&2; exit 1; }
expect 1 "a GO_IMAGE whose digest was dropped is caught (the tag alone still agrees)" "GO_IMAGE is not pinned to a well-formed"
reset

# =====================================================================================
# The supply-chain pin assertions (S0057). Same failure shape again, on the references
# this repository does not build: the actions its workflows run, the image its example
# deployment pulls, the bases its Dockerfile builds on, and the lifecycle scripts a node
# manifest would let every transitive dependency execute. Each of these is a reference
# that can be silently downgraded to a mutable one and still look completely normal, so
# each downgrade is performed here on purpose.
# =====================================================================================

# --- 19. An action reference put back to a mutable tag. This is the state the whole
#         repository was in before S0057: `actions/checkout@v4` is a standing grant to run
#         whatever that publisher pushes to `v4` next, inside a job holding this
#         repository's credentials and, in release.yml, its GHCR push token.
sed -i 's|uses: actions/checkout@.*|uses: actions/checkout@v4|' "$repo/.github/workflows/pin-health.yml"
grep -qE 'uses: actions/checkout@v4$' "$repo/.github/workflows/pin-health.yml" \
  || { echo "::error::selftest: could not plant a tag action ref, so this case did NOT run" >&2; exit 1; }
expect 1 "an action reference reverted to a mutable tag is caught" "MUTABLE ACTION REFERENCE"
reset

# --- 20. The SHA is right but the version comment is gone. The pin still HOLDS, so
#         nothing breaks and no test reds - it just becomes unreviewable, because a bare
#         40-hex string tells nobody what it is or how far behind it has fallen. That is
#         precisely the kind of decay that is never noticed without a check.
sed -i 's|uses: actions/checkout@.*|uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262|' "$repo/.github/workflows/pin-health.yml"
expect 1 "an action pinned to a SHA with no version comment is caught" "UNREADABLE ACTION PIN"
reset

# --- 21. The local-action exemption is real and not dead code: a `./` reference is part
#         of THIS repository and has no upstream publisher to pin against. Asserted with a
#         planted reference rather than on the clean tree, which carries none.
sed -i 's|uses: actions/checkout@.*|uses: ./.github/actions/local-thing|' "$repo/.github/workflows/pin-health.yml"
expect 0 "a local ./ action reference is exempt (there is nothing upstream to pin)" "uses a local action"
reset

# --- 22. The compose example put back to a floating pull. A user copies this file and
#         runs it; without the digest they get whatever the registry serves that day,
#         which need not be an image that ever passed scripts/smoke-image.sh.
sed -i 's|^    image: .*|    image: ghcr.io/nschatz/holdfast:v0.1.0|' "$repo/docker-compose.yml"
expect 1 "a compose image with no digest is caught" "UNPINNED IMAGE"
reset

# --- 23. The same reference WITH a digest but tagged `latest`. It has to be a separate
#         case: the digest is present, so a check that only asked "is there a digest"
#         passes it - while `latest` is the tag release.yml MOVES, so the next release
#         silently changes the encoder and the libvmaf instrument under a deployment
#         nobody touched.
sed -i 's|^    image: .*|    image: ghcr.io/nschatz/holdfast:latest@sha256:302242b66f9c160e69b1e7c37d57925ec593bc7ed0ee9df851af0ec58c7cd4b2|' "$repo/docker-compose.yml"
expect 1 "a compose image tagged latest is caught even WITH a digest" "FLOATING ':latest' IMAGE"
reset

# --- 24. RUNTIME_IMAGE with its digest dropped. Section 3 guards GO_IMAGE alone, and
#         GO_IMAGE is the one base with a second line of defence (the build stage asks the
#         pulled image its own `go env GOVERSION`). RUNTIME_IMAGE has none: nothing RUNs
#         in the runtime stage, so an in-image assertion is impossible and this check is
#         the only thing standing between the shipped image and a floating base.
sed -i 's|^\(ARG RUNTIME_IMAGE=[^@]*\)@sha256:[0-9a-f]*$|\1|' "$repo/Dockerfile"
grep -qE '^ARG RUNTIME_IMAGE=[^@]+$' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not strip the RUNTIME_IMAGE digest, so this case did NOT run" >&2; exit 1; }
expect 1 "a RUNTIME_IMAGE whose digest was dropped is caught (not just GO_IMAGE)" "UNPINNED BASE IMAGE"
reset

# --- 25. The other half of a base pin, on the third ARG: a digest with no tag. It
#         resolves correctly for ever, so nothing breaks - but no human reading the
#         Dockerfile can tell which Debian they are shipping.
sed -i 's|^ARG FETCH_IMAGE=\([^:]*\):[^@]*@|ARG FETCH_IMAGE=\1@|' "$repo/Dockerfile"
grep -qE '^ARG FETCH_IMAGE=[^:]+@sha256:[0-9a-f]{64}$' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not strip the FETCH_IMAGE tag, so this case did NOT run" >&2; exit 1; }
expect 1 "a FETCH_IMAGE with a digest but no tag is caught" "UNREADABLE BASE IMAGE PIN"
reset

# --- 26. A node manifest arriving with no lifecycle-script decision. Deliberately
#         UNTRACKED: the moment to refuse a manifest is the moment it is about to be
#         committed, so a tracked-files-only check would stay green through the entire
#         pull request that introduces it and bite one merge too late.
printf '{}\n' > "$repo/package.json"
expect 1 "an untracked node manifest with no lifecycle-script decision is caught" "NODE MANIFEST WITH NO LIFECYCLE-SCRIPT DECISION"
reset

# --- 27. And the decision genuinely satisfies it, so the refusal is a tripwire on an
#         UNDECLARED risk rather than a blanket ban on node. Without this case the check
#         above could be a guard that refuses every manifest unconditionally, which would
#         be indistinguishable from working and impossible to comply with.
printf '{}\n' > "$repo/package.json"
printf 'ignore-scripts=true\n' > "$repo/.npmrc"
git -C "$repo" add -f package.json .npmrc
expect 0 "a node manifest WITH a committed ignore-scripts=true .npmrc passes" "committed lifecycle-script decision"
reset

# --- 28. A file the gate reads, gone. Every assertion here is "read these files and
#         compare them", and comparing two empty strings succeeds - so a missing file does
#         not make an assertion visibly vacuous, it makes it INVISIBLY vacuous. A check
#         that could not run has not passed.
rm -f "$repo/docker-compose.yml"
expect 1 "a missing docker-compose.yml is named, not silently skipped" "MISSING: docker-compose.yml"
reset

# --- 29. Publishing `:latest` must NOT red the gate. release.yml promotes that tag onto a
#         digest that has ALREADY passed the smoke gate, which is the opposite of
#         depending on it - and a refusal that cannot tell a publish from a dependency
#         would force the next person to weaken the check to get their release out. Planted
#         rather than relying on the promotion step already in release.yml, so the case
#         still means something if that step is ever reworded.
cat >> "$repo/.github/workflows/release.yml" <<'YAML'

      # selftest fixture: publishing :latest is not depending on it.
      - name: promote latest (selftest fixture)
        run: |
          docker buildx imagetools create -t "ghcr.io/nschatz/holdfast:latest" "ghcr.io/nschatz/holdfast:v9.9.9"
          echo "image: ghcr.io/nschatz/holdfast:latest"
YAML
expect 0 "release.yml publishing :latest does NOT red the gate (publishing is not depending)"
reset

echo
# Report against the number of cases DECLARED, not the number that ran: "$pass/$pass" is N/N
# by construction and could never show a shortfall.
total=$((pass + failed))
if [ "$total" -ne "$declared" ]; then
  echo "::error::check-pins selftest: ran $total case(s), expected $declared — a case did not execute" >&2
  exit 1
fi
if [ "$failed" -ne 0 ]; then
  echo "::error::check-pins selftest: $failed of $declared case(s) did not bite - check-pins.sh is not trustworthy" >&2
  exit 1
fi
echo "check-pins selftest: $pass/$declared cases bite"
