#!/usr/bin/env bash
# Prove the guards in check-pins.sh still BITE: the rename guard and the pin assertions.
# Part of `make check`.
#
# Five families of case live here. Cases 0-6 defeat the RENAME guard; cases 7-17 (S0022)
# defeat the FFMPEG PIN guard - the floating alias, the short-retention daily build, a
# blanked digest, and a NOTICE that has drifted from the Dockerfile it is supposed to be
# the source offer for; case 18 (S0024) defeats the GO TOOLCHAIN pin's digest half and case
# 30 its enumeration half, in a workflow that is neither of the two that section used to
# read by name; cases
# 19-29 (S0057) defeat the SUPPLY-CHAIN pin guards - a mutable action reference, an
# unreadable one, a compose image that lost its digest or gained a `latest` tag, a base
# image that lost its digest or its tag, a node manifest with no lifecycle-script
# decision, and a file the gate reads going missing; cases 31-34 (S0151) defeat the UPDATE
# BOT guards - a build stage that drifted from its GO_IMAGE copy, a base image hidden behind
# an ARG where the bot cannot read it, and a bot configuration that stopped watching a pin
# class or went missing; cases 35-42 (goal 5) defeat the HARDWARE RUNTIME's Debian package
# guards - a package the image copies that NOTICE omits, one NOTICE names that the image
# does not carry, a version drifted between the two, a shortened sha256, a fetch from the
# live mirror or a floating snapshot, a missing pin block, and a NOTICE entry with no source
# offer; cases 43-57 (goal 8) defeat the DYNAMIC-HDR TOOL guards - a floating or missing
# version, a short or uppercase digest, a fetch URL that is not the pinned release asset or
# a second unchecked one, and a NOTICE that omits a tool, drifts from its version, names one
# the image does not carry, loses its MIT licence, source or permission notice, or carries a
# stray version; case 57 asserts a coherent bump of both files still PASSES; cases 58-75
# (goal 13) defeat the WEB UI guards - a pnpm project whose only lifecycle-script decision
# is an .npmrc pnpm does not read, a decision that is false, absent or uncommitted, a
# minimumReleaseAgeExclude list, a release age that is implicit or too short, a dependency
# granted its build script, a Node pin that floats or drifted from the image's stage, a
# literal Node version in a workflow, a package manager without its hash, a package pinned
# by a range, a missing lockfile, a bot that stopped watching the UI's packages, and a
# NOTICE that drifted from the Svelte the binary embeds or lost its permission notice; case
# 74 asserts a coherent Node bump still PASSES. Two of the
# S0057 cases assert a PASS
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

declared=76
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
# The web UI's installed dependencies and build output are left out: they are ignored
# files, no case reads them, and they are the bulk of the tree by size.
tar -C "$here" --exclude=.git --exclude=./web/node_modules --exclude=./web/dist -cf - . | tar -C "$repo" -xf - \
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

# --- 24. The runtime base with its digest dropped. Section 3 guards the build stage, and
#         that is the one base with a second line of defence (the build stage asks the
#         pulled image its own `go env GOVERSION`). The runtime base has none: nothing RUNs
#         in the runtime stage, so an in-image assertion is impossible and this check is
#         the only thing standing between the shipped image and a floating base. It is the
#         one FROM line naming no stage.
sed -i -E '/^FROM [^ ]+$/ s/@sha256:[0-9a-f]{64}$//' "$repo/Dockerfile"
grep -qE '^FROM [^ @$]+$' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not strip the runtime base's digest, so this case did NOT run" >&2; exit 1; }
expect 1 "a runtime base whose digest was dropped is caught (not just the build stage)" "UNPINNED BASE IMAGE"
reset

# --- 25. The other half of a base pin, on a third FROM line: a digest with no tag. It
#         resolves correctly for ever, so nothing breaks - but no human reading the
#         Dockerfile can tell which Debian they are shipping.
sed -i -E 's|^(FROM --platform=[^ ]+ [^:@ ]+):[^@ ]+(@sha256:[0-9a-f]{64} AS ffmpeg)$|\1\2|' "$repo/Dockerfile"
grep -qE '^FROM --platform=[^ ]+ [^:@ ]+@sha256:[0-9a-f]{64} AS ffmpeg$' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not strip the fetch stage's tag, so this case did NOT run" >&2; exit 1; }
expect 1 "a fetch-stage base with a digest but no tag is caught" "UNREADABLE BASE IMAGE PIN"
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

# --- 30. A THIRD workflow's Go pin drifting. Section 3 used to read ci.yml and release.yml
#         by name, so any other workflow declaring GO_VERSION was a restatement nothing
#         compared: it could sit a patch release behind the toolchain the shipped binary is
#         built with, or behind a govulncheck stdlib advisory, under a green build. The
#         section enumerates the directory now, the way the action check already does, and
#         this moves the pin in a workflow that is neither of the two former names.
sed -i 's/^  GO_VERSION: ".*"$/  GO_VERSION: "1.25.0"/' "$repo/.github/workflows/mutation.yml"
grep -q '^  GO_VERSION: "1.25.0"$' "$repo/.github/workflows/mutation.yml" \
  || { echo "::error::selftest: could not move mutation.yml's GO_VERSION, so this case did NOT run" >&2; exit 1; }
expect 1 "a Go pin drifting in a workflow that is neither ci.yml nor release.yml is caught" "Go version drift - .github/workflows/mutation.yml"
reset

# =====================================================================================
# The update-bot guards (S0151). The bot is Dependabot, and it reads only what is written
# where it looks: a FROM line, a `uses:` reference, go.mod. Each case below hides a pin
# from it, or splits the one value that has to appear twice, in a way that still builds.
# =====================================================================================

# --- 31. The build stage's FROM line moved without GO_IMAGE. This is exactly the shape of
#         the bot's own toolchain pull request: it rewrites the FROM line and nothing
#         else. The image would build FROM one digest while the in-image assertion reads
#         another, so it must red here, naming the split.
sed -i -E 's|^(FROM --platform=[^ ]+ golang:[^@ ]+@sha256:)[0-9a-f]{64}( AS build)$|\1'"$(printf 'a%.0s' $(seq 64))"'\2|' "$repo/Dockerfile"
grep -qE '^FROM --platform=[^ ]+ golang:[^@ ]+@sha256:a{64} AS build$' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not move the build stage's digest, so this case did NOT run" >&2; exit 1; }
expect 1 "a build stage that moved without its GO_IMAGE copy is caught" "GO_IMAGE disagrees with the build stage"
reset

# --- 32. A base image put back behind an ARG. Pinned by tag and digest, so section 7's
#         other refusals have nothing to say - the only thing wrong is that the bot can no
#         longer see it, and that is what has to be named.
rt="$(sed -n -E 's|^FROM ([^ ]+)$|\1|p' "$repo/Dockerfile" | head -1)"
[ -n "$rt" ] || { echo "::error::selftest: found no runtime FROM line, so this case did NOT run" >&2; exit 1; }
sed -i -E 's|^FROM [^ ]+$|FROM ${RUNTIME_IMAGE}|' "$repo/Dockerfile"
sed -i "0,/^ARG GO_IMAGE=/s||ARG RUNTIME_IMAGE=$rt\nARG GO_IMAGE=|" "$repo/Dockerfile"
grep -qxF "ARG RUNTIME_IMAGE=$rt" "$repo/Dockerfile" && grep -qxF 'FROM ${RUNTIME_IMAGE}' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not move the runtime base behind an ARG, so this case did NOT run" >&2; exit 1; }
expect 1 "a base image pinned through an ARG, where the bot cannot read it, is caught" "BASE IMAGE THE UPDATE BOT CANNOT READ"
reset

# --- 33. The bot told to stop watching the base images. The file is still there and
#         still valid - it just no longer looks at the Dockerfile.
sed -i '/^  - package-ecosystem: "docker"$/,/^$/d' "$repo/.github/dependabot.yml"
! grep -q 'package-ecosystem: "docker"' "$repo/.github/dependabot.yml" \
  || { echo "::error::selftest: could not remove the docker entry, so this case did NOT run" >&2; exit 1; }
expect 1 "a bot configuration that stopped watching the base images is caught" "UPDATE BOT BLIND SPOT.*"
reset

# --- 34. The bot's configuration, gone. Nothing would ever say a pin went stale again,
#         and nothing about the build would change - the same invisible vacuity as case 28.
rm -f "$repo/.github/dependabot.yml"
expect 1 "a missing .github/dependabot.yml is named, not silently skipped" "MISSING: .github/dependabot.yml"
reset

# =====================================================================================
# The hardware runtime's Debian package guards (goal 5, P3). The amd64 image copies shared
# objects out of Debian packages, and NOTICE is their licence record and source offer.
# Each case below breaks one half of that in a way that still builds: a package the
# image carries with no record, a record for a package it does not carry, a version
# moved in one file only, a hash that no longer pins the bytes, a URL that floats, and
# a record whose source offer points nowhere.
# =====================================================================================

# --- 35. A package copied into the image, missing from NOTICE.
sed -i '/^  deb: libva2 /,/^$/d' "$repo/NOTICE"
! grep -q '^  deb: libva2 ' "$repo/NOTICE" \
  || { echo "::error::selftest: could not remove libva2 from NOTICE, so this case did NOT run" >&2; exit 1; }
expect 1 "a Debian package the image copies but NOTICE omits is caught" "libva2 .* MISSING FROM NOTICE"
reset

# --- 36. A package NOTICE names that the Dockerfile does not copy.
sed -i '/^libxshmfence1 /d' "$repo/Dockerfile"
! grep -q '^libxshmfence1 ' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not remove libxshmfence1 from the pin block, so this case did NOT run" >&2; exit 1; }
expect 1 "a Debian package NOTICE names but the Dockerfile does not copy is caught" "NOTICE names Debian package libxshmfence1 .*does NOT copy"
reset

# --- 37. A version moved in NOTICE only: the record names bytes the image lacks.
sed -i 's/^  deb: libva2 2\.22\.0-3$/  deb: libva2 2.22.0-4/' "$repo/NOTICE"
grep -qx '  deb: libva2 2.22.0-4' "$repo/NOTICE" \
  || { echo "::error::selftest: could not move libva2's version in NOTICE, so this case did NOT run" >&2; exit 1; }
expect 1 "a Debian package version that drifted between the Dockerfile and NOTICE is caught" "libva2: VERSION DRIFT"
reset

# --- 38. A hash cut short: the build's sha256sum -c would no longer pin the bytes.
sed -i -E 's/^(libva2 +[^ ]+ +)[0-9a-f]([0-9a-f]{63}) /\1\2 /' "$repo/Dockerfile"
grep -qE '^libva2 +[^ ]+ +[0-9a-f]{63} ' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not shorten libva2's sha256, so this case did NOT run" >&2; exit 1; }
expect 1 "a Debian pin without a full sha256 is caught" "libva2 .* carries no 64-character lowercase sha256"
reset

# --- 39. The fetch moved to the live mirror, where a pool file vanishes at the next point
#         release. The hashes still verify today, which is exactly why it has to red.
sed -i 's|https://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}/pool/${path}|https://deb.debian.org/debian/pool/${path}|' "$repo/Dockerfile"
grep -qF 'https://deb.debian.org/debian/pool/${path}' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not move the fetch to the live mirror, so this case did NOT run" >&2; exit 1; }
expect 1 "a Debian package fetched from the live mirror is caught" "FLOATING DEBIAN URL"
reset

# --- 40. The snapshot itself floating.
sed -i 's/^ARG DEBIAN_SNAPSHOT=.*/ARG DEBIAN_SNAPSHOT=latest/' "$repo/Dockerfile"
grep -qx 'ARG DEBIAN_SNAPSHOT=latest' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not float the snapshot, so this case did NOT run" >&2; exit 1; }
expect 1 "a Debian snapshot that is not a timestamp is caught" "FLOATING DEBIAN SNAPSHOT"
reset

# --- 41. The pin block gone: an empty comparison would agree with an empty NOTICE.
sed -i "/^COPY <<'DEBPINS' /,/^DEBPINS\$/d" "$repo/Dockerfile"
! grep -q 'DEBPINS' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not remove the pin block, so this case did NOT run" >&2; exit 1; }
expect 1 "a Dockerfile without its Debian pin block is named, not silently skipped" "no Debian package pin block"
reset

# --- 42. A NOTICE entry whose source offer points nowhere.
sed -i '/^       source:  lm-sensors /d' "$repo/NOTICE"
! grep -q '^       source:  lm-sensors ' "$repo/NOTICE" \
  || { echo "::error::selftest: could not remove libsensors5's source line, so this case did NOT run" >&2; exit 1; }
expect 1 "a Debian package in NOTICE with no corresponding source is caught" "no corresponding source on snapshot.debian.org.*libsensors5"
reset

# =====================================================================================
# The dynamic-HDR tool guards (goal 8). The image bundles dovi_tool and hdr10plus_tool, and
# NOTICE is the MIT copyright and permission notice that has to travel with them. Each
# case below breaks one half of that in a way that still builds.
# =====================================================================================

# --- 43. A floating alias for the version.
sed -i 's/^ARG DOVI_TOOL_VERSION=.*/ARG DOVI_TOOL_VERSION=latest/' "$repo/Dockerfile"
grep -qx 'ARG DOVI_TOOL_VERSION=latest' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not float the dovi_tool version, so this case did NOT run" >&2; exit 1; }
expect 1 "a dovi_tool version that is a floating alias is caught" "FLOATING dovi_tool PIN"
reset

# --- 44. A pin ARG gone: the installer would have nothing to parse.
sed -i '/^ARG HDR10PLUS_TOOL_VERSION=/d' "$repo/Dockerfile"
! grep -q '^ARG HDR10PLUS_TOOL_VERSION=' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not remove HDR10PLUS_TOOL_VERSION, so this case did NOT run" >&2; exit 1; }
expect 1 "a missing HDR10PLUS_TOOL_VERSION ARG is caught" "no 'ARG HDR10PLUS_TOOL_VERSION='"
reset

# --- 45. A digest cut short.
sed -i -E 's/^(ARG DOVI_TOOL_SHA256_ARM64=)[0-9a-f]([0-9a-f]{63})$/\1\2/' "$repo/Dockerfile"
grep -qE '^ARG DOVI_TOOL_SHA256_ARM64=[0-9a-f]{63}$' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not shorten DOVI_TOOL_SHA256_ARM64, so this case did NOT run" >&2; exit 1; }
expect 1 "a dovi_tool arm64 digest one character short is caught" "DOVI_TOOL_SHA256_ARM64 is not a 64-character lowercase sha256"
reset

# --- 46. A digest in uppercase: sha256sum prints lowercase, so the shape is exact.
sed -i -E 's/^(ARG HDR10PLUS_TOOL_SHA256_AMD64=)(.*)$/\1\U\2/' "$repo/Dockerfile"
grep -qE '^ARG HDR10PLUS_TOOL_SHA256_AMD64=[0-9A-F]{64}$' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not uppercase HDR10PLUS_TOOL_SHA256_AMD64, so this case did NOT run" >&2; exit 1; }
expect 1 "an uppercase hdr10plus_tool amd64 digest is caught" "HDR10PLUS_TOOL_SHA256_AMD64 is not a 64-character lowercase sha256"
reset

# --- 47. The fetch moved to upstream's floating latest-release redirect.
sed -i 's|https://github.com/quietvoid/hdr10plus_tool/releases/download/${HDR10PLUS_TOOL_VERSION}/|https://github.com/quietvoid/hdr10plus_tool/releases/latest/download/|' "$repo/Dockerfile"
grep -qF 'quietvoid/hdr10plus_tool/releases/latest/download/' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not float the hdr10plus_tool URL, so this case did NOT run" >&2; exit 1; }
expect 1 "an hdr10plus_tool fetch through the floating latest redirect is caught" "FLOATING hdr10plus_tool URL"
reset

# --- 48. The version written into the URL by hand: it agrees today and splits from the
#         ARG on the next bump.
sed -i 's|releases/download/${DOVI_TOOL_VERSION}/dovi_tool-${DOVI_TOOL_VERSION}-|releases/download/2.3.4/dovi_tool-2.3.4-|' "$repo/Dockerfile"
grep -qF 'releases/download/2.3.4/dovi_tool-2.3.4-' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not hard-code the dovi_tool URL, so this case did NOT run" >&2; exit 1; }
expect 1 "a dovi_tool URL not built from DOVI_TOOL_VERSION is caught" "FLOATING dovi_tool URL"
reset

# --- 49. A second fetch from upstream's owner, beside the checked one.
sed -i 's|^    ls -l /dynhdr$|    curl -fsSL -o /tmp/x https://github.com/quietvoid/dovi_tool/releases/download/2.3.3/dovi_tool-2.3.3-x86_64-unknown-linux-musl.tar.gz; \\\n    ls -l /dynhdr|' "$repo/Dockerfile"
grep -qF 'download/2.3.3/dovi_tool-2.3.3-x86_64' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not plant a second dovi_tool fetch, so this case did NOT run" >&2; exit 1; }
expect 1 "a second, unchecked fetch from the tools' upstream is caught" "UNCHECKED dynamic-HDR tool URL"
reset

# --- 50. A bundled tool missing from NOTICE.
sed -i '/^  tool: dovi_tool /,/^       SOFTWARE\.$/d' "$repo/NOTICE"
! grep -q '^  tool: dovi_tool ' "$repo/NOTICE" \
  || { echo "::error::selftest: could not remove dovi_tool from NOTICE, so this case did NOT run" >&2; exit 1; }
expect 1 "a tool the image bundles but NOTICE omits is caught" "dovi_tool .* MISSING FROM NOTICE"
reset

# --- 51. A version moved in NOTICE only.
sed -i 's/^  tool: hdr10plus_tool .*/  tool: hdr10plus_tool 1.7.1/' "$repo/NOTICE"
grep -qx '  tool: hdr10plus_tool 1.7.1' "$repo/NOTICE" \
  || { echo "::error::selftest: could not move hdr10plus_tool's version in NOTICE, so this case did NOT run" >&2; exit 1; }
expect 1 "a tool version that drifted between the Dockerfile and NOTICE is caught" "hdr10plus_tool: VERSION DRIFT"
reset

# --- 52. A tool NOTICE names that the image does not carry.
sed -i 's/^  tool: hdr10plus_tool \(.*\)$/  tool: hdr10plus_tool \1\n\n  tool: mkvextract 1.0.0\n       licence: MIT/' "$repo/NOTICE"
grep -q '^  tool: mkvextract 1.0.0$' "$repo/NOTICE" \
  || { echo "::error::selftest: could not plant a stray NOTICE tool, so this case did NOT run" >&2; exit 1; }
expect 1 "a tool NOTICE names but the Dockerfile does not bundle is caught" "NOTICE names the tool mkvextract .*does NOT bundle"
reset

# --- 53. The licence line changed.
awk '/^  tool: dovi_tool /{t=1} t && /^       licence: MIT$/{print "       licence: GPL-3.0"; t=0; next} {print}' "$repo/NOTICE" >"$repo/NOTICE.new" && mv "$repo/NOTICE.new" "$repo/NOTICE"
grep -qx '       licence: GPL-3.0' "$repo/NOTICE" \
  || { echo "::error::selftest: could not change dovi_tool's licence line, so this case did NOT run" >&2; exit 1; }
expect 1 "a tool entry whose licence is not MIT is caught" "dovi_tool entry is incomplete: it lacks a 'licence: MIT' line"
reset

# --- 54. The source offer gone.
sed -i '/releases\/tag\/1\.7\.2)$/d' "$repo/NOTICE"
! grep -q 'hdr10plus_tool/releases/tag/' "$repo/NOTICE" \
  || { echo "::error::selftest: could not remove hdr10plus_tool's release URL, so this case did NOT run" >&2; exit 1; }
expect 1 "a tool entry with no source release URL is caught" "hdr10plus_tool entry is incomplete: it lacks its source"
reset

# --- 55. The MIT permission notice dropped from one entry: the binary would ship without
#         the text its licence says must travel with it.
awk '/^  tool: hdr10plus_tool /{t=1} t && /Permission is hereby granted, free of charge/{t=0; next} {print}' "$repo/NOTICE" >"$repo/NOTICE.new" && mv "$repo/NOTICE.new" "$repo/NOTICE"
# Two remain: dovi_tool's, and the Svelte runtime's in the web UI's own section.
[ "$(grep -c 'Permission is hereby granted, free of charge' "$repo/NOTICE")" -eq 2 ] \
  || { echo "::error::selftest: could not drop hdr10plus_tool's permission notice, so this case did NOT run" >&2; exit 1; }
expect 1 "a tool entry without the MIT permission notice is caught" "hdr10plus_tool entry is incomplete: it lacks the MIT permission notice"
reset

# --- 56. A stray other version of a tool elsewhere in NOTICE, on a line no entry check reads.
printf '\n  (an earlier image carried dovi_tool 2.3.3.)\n' >>"$repo/NOTICE"
expect 1 "a stray other version of a tool elsewhere in NOTICE is caught" "stray: dovi_tool 2\.3\.3"
reset

# --- 57. A coherent bump of BOTH files passes: the guard is a comparison, not a freeze. The
#         digests are left as they are; only upstream can say whether they match, and the
#         build's sha256sum -c is what asks.
sed -i 's/^ARG DOVI_TOOL_VERSION=.*/ARG DOVI_TOOL_VERSION=2.3.5/' "$repo/Dockerfile"
sed -i 's|dovi_tool 2\.3\.4|dovi_tool 2.3.5|; s|release tag 2\.3\.4,|release tag 2.3.5,|; s|dovi_tool/releases/tag/2\.3\.4|dovi_tool/releases/tag/2.3.5|' "$repo/NOTICE"
grep -qx '  tool: dovi_tool 2.3.5' "$repo/NOTICE" && grep -qx 'ARG DOVI_TOOL_VERSION=2.3.5' "$repo/Dockerfile" \
  || { echo "::error::selftest: could not bump dovi_tool in both files, so this case did NOT run" >&2; exit 1; }
expect 0 "a dovi_tool bump made in the Dockerfile AND NOTICE together passes" "dovi_tool 2\.3\.5 and hdr10plus_tool"
reset

# =============================================================================
# The web UI's guards (goal 13): sections 8, 9 and 12 over web/.
#
# Every case edits the CLONE's own web/ files, which the clean-tree baseline (case 0) has
# already shown to pass, so each red below is the one edit it names.
# =============================================================================
ws="$repo/web/pnpm-workspace.yaml"
[ -f "$ws" ] && [ -f "$repo/web/package.json" ] && [ -f "$repo/web/.node-version" ] \
  || { echo "::error::selftest: the clone has no web/ project, so the web UI cases did NOT run" >&2; exit 1; }

# --- 58. THE case this section was rewritten for: a pnpm project whose only decision is an
#         .npmrc. It is the setup the previous rule accepted, and under pnpm 11 and later
#         that line stops nothing. The workspace file keeps every other line, so the red is
#         the missing `ignoreScripts` and nothing else.
sed -i '/^ignoreScripts:/d' "$ws"
printf 'ignore-scripts=true\n' > "$repo/web/.npmrc"
git -C "$repo" add -f web/.npmrc web/pnpm-workspace.yaml
grep -q '^ignoreScripts:' "$ws" && { echo "::error::selftest: could not remove ignoreScripts, so this case did NOT run" >&2; exit 1; }
expect 1 "a pnpm project that sets ignore-scripts ONLY in .npmrc is caught (pnpm does not read it)" "PNPM MANIFEST WITH NO LIFECYCLE-SCRIPT DECISION PNPM READS: web/package.json"
# The message must say WHY the .npmrc does not count, or the fix an operator reaches for
# is the one that does nothing.
if grep -q 'web/.npmrc sets ignore-scripts, and pnpm 11 and later DO NOT READ IT' <<<"$guard_out"; then
  :
else
  printf '::error::selftest: the .npmrc-only refusal does not say that pnpm does not read .npmrc\n' >&2
  failed=$((failed + 1)); pass=$((pass - 1))
fi
reset

# --- 59. The same mistake in a SECOND project, arriving new: a manifest that names pnpm,
#         with a committed .npmrc beside it and no workspace file at all. This is the shape
#         the old rule passed with "committed lifecycle-script decision"; the project is
#         pnpm's by its own manifest, so the .npmrc decides nothing.
mkdir -p "$repo/tools/probe"
printf '{\n  "packageManager": "pnpm@12.8.1"\n}\n' > "$repo/tools/probe/package.json"
printf 'ignore-scripts=true\n' > "$repo/tools/probe/.npmrc"
git -C "$repo" add -f tools/probe/package.json tools/probe/.npmrc
expect 1 "a new pnpm project with a committed .npmrc and no pnpm-workspace.yaml is caught" "PNPM MANIFEST WITH NO LIFECYCLE-SCRIPT DECISION PNPM READS: tools/probe/package.json"
grep -q 'there is no committed tools/probe/pnpm-workspace.yaml' <<<"$guard_out" \
  || { printf '::error::selftest: the refusal does not name the workspace file that is missing\n' >&2; failed=$((failed + 1)); pass=$((pass - 1)); }
reset

# --- 60. The decision made, and made the wrong way. No reason unlocks it.
sed -i 's/^ignoreScripts:.*/ignoreScripts: false # lifecycle-scripts-reason: a native build/' "$ws"
expect 1 "ignoreScripts: false is caught, and no reason unlocks it" "does not carry a top-level .ignoreScripts: true. \(it reads: 'false'\)"
reset

# --- 61. The decision indented under another key: pnpm reads the TOP-LEVEL key only.
sed -i 's/^ignoreScripts: true/settings:\n  ignoreScripts: true/' "$ws"
expect 1 "an ignoreScripts nested under another key is caught" "it reads: 'absent'"
reset

# --- 62. The decision present in the working tree and never committed.
git -C "$repo" rm -q --cached web/pnpm-workspace.yaml
expect 1 "an uncommitted pnpm-workspace.yaml is not a decision" "there is no committed web/pnpm-workspace.yaml"
reset

# --- 63. The list pnpm writes for itself when it lets a young version through.
printf '\nminimumReleaseAgeExclude:\n  - typescript-eslint@8.71.0\n' >> "$ws"
expect 1 "a minimumReleaseAgeExclude list is refused" "RELEASE-AGE EXCLUDE LIST"
reset

# --- 64. The same key as an inline list, and empty: its presence is the finding.
printf '\nminimumReleaseAgeExclude: []\n' >> "$ws"
expect 1 "an empty inline minimumReleaseAgeExclude is refused too" "RELEASE-AGE EXCLUDE LIST"
reset

# --- 65. The release age left to its default, which is what lets pnpm write that list.
sed -i '/^minimumReleaseAge:/d' "$ws"
expect 1 "an implicit minimumReleaseAge is caught" "NO EXPLICIT RELEASE AGE"
reset

# --- 66. Set, and shorter than a day.
sed -i 's/^minimumReleaseAge:.*/minimumReleaseAge: 60/' "$ws"
expect 1 "a minimumReleaseAge under a day is caught" "NO EXPLICIT RELEASE AGE.*it reads: '60'"
reset

# --- 67. A dependency granted its build script.
sed -i 's/^  fsevents: false/  fsevents: true/' "$ws"
grep -q '^  fsevents: true' "$ws" || { echo "::error::selftest: could not flip allowBuilds, so this case did NOT run" >&2; exit 1; }
expect 1 "a dependency granted its build script under allowBuilds is caught" "DEPENDENCY BUILD SCRIPT ALLOWED.*fsevents"
reset

# --- 68. The Node pin moved in one home only: the gate would prove the UI on one Node and
#         the image would ship one built on another.
printf '24.20.0\n' > "$repo/web/.node-version"
expect 1 "a Node pin that drifted from the image's ui stage is caught" "Node version drift"
reset

# --- 69. A Node pin that is not a version.
printf '24\n' > "$repo/web/.node-version"
expect 1 "a Node pin that is a major alone is caught" "FLOATING NODE PIN"
reset

# --- 70. A workflow restating the Node version instead of reading the file.
sed -i 's|^\( *\)node-version-file: web/\.node-version|\1node-version: "24.21.0"|' "$repo/.github/workflows/ci.yml"
grep -q 'node-version: "24.21.0"' "$repo/.github/workflows/ci.yml" || { echo "::error::selftest: could not rewrite ci.yml, so this case did NOT run" >&2; exit 1; }
expect 1 "a literal node-version in a workflow is caught" "SECOND HOME FOR THE NODE PIN"
reset

# --- 71. The package manager named without the hash that makes corepack refuse other bytes.
sed -i 's|"packageManager": "pnpm@\([0-9.]*\)+sha512\.[0-9a-f]*"|"packageManager": "pnpm@\1"|' "$repo/web/package.json"
expect 1 "a packageManager with no sha512 is caught" "UNPINNED PACKAGE MANAGER"
reset

# --- 72. One package pinned by a range. The lockfile still resolves it today.
sed -i 's|"vite": "\([0-9.]*\)"|"vite": "^\1"|' "$repo/web/package.json"
grep -q '"vite": "^' "$repo/web/package.json" || { echo "::error::selftest: could not loosen vite, so this case did NOT run" >&2; exit 1; }
expect 1 "a package pinned by a caret range is caught" "NOT AN EXACT VERSION.*vite"
reset

# --- 73. The licence record left behind by a Svelte bump made in the manifest alone, and
#         the bot told to stop watching the UI's packages. Two edits, two messages, one
#         case each would be the same shape; the Svelte one is graded here and the bot's
#         in the same run by its own message.
sed -i 's|"svelte": "[0-9.]*"|"svelte": "5.57.2"|' "$repo/web/package.json"
sed -i 's|directory: "/web"|directory: "/elsewhere"|' "$repo/.github/dependabot.yml"
expect 1 "a Svelte version that drifted between web/package.json and NOTICE is caught" "svelte: VERSION DRIFT"
if grep -q 'UPDATE BOT BLIND SPOT.*' <<<"$guard_out" && grep -q 'npm (directory: "/web")' <<<"$guard_out"; then
  :
else
  printf '::error::selftest: a bot that stopped watching /web was not reported\n' >&2
  failed=$((failed + 1)); pass=$((pass - 1))
fi
reset

# --- 74. A coherent Node bump - the pin file and the image's stage together - passes: the
#         guard is a comparison, not a freeze. The digest is left as it is; only the
#         registry can say whether it matches, and the stage's own `node --version` check
#         is what asks.
printf '24.21.1\n' > "$repo/web/.node-version"
sed -i 's|node:24\.21\.0-trixie-slim@|node:24.21.1-trixie-slim@|' "$repo/Dockerfile"
grep -q 'node:24.21.1-trixie-slim@sha256:' "$repo/Dockerfile" || { echo "::error::selftest: could not bump the ui stage, so this case did NOT run" >&2; exit 1; }
expect 0 "a Node bump made in web/.node-version AND the Dockerfile together passes" "built on Node 24\.21\.1"
reset

# --- 75. The Svelte entry with its permission notice gone: the binary would embed MIT code
#         without the text its licence says must travel with it.
awk '/^  package: svelte /{t=1} t && /Permission is hereby granted, free of charge/{t=0; next} {print}' "$repo/NOTICE" >"$repo/NOTICE.new" && mv "$repo/NOTICE.new" "$repo/NOTICE"
[ "$(grep -c 'Permission is hereby granted, free of charge' "$repo/NOTICE")" -eq 2 ] \
  || { echo "::error::selftest: could not drop Svelte's permission notice, so this case did NOT run" >&2; exit 1; }
expect 1 "a Svelte entry without the MIT permission notice is caught" "svelte entry is incomplete: it lacks the MIT permission notice"
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
