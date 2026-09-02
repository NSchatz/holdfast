#!/usr/bin/env bash
# Assert that every cross-file pin actually agrees. Part of `make check`.
#
# This branch was refuted four times for one failure: a value restated in several files
# and kept in step by a COMMENT. It always looks fine — each file is internally
# consistent, every checksum verifies, CI is green — while the thing the value describes
# has silently detached from the thing that was proven. Prose cannot enforce an
# invariant. So where a value genuinely MUST appear twice, the agreement is checked here
# and the drift is loud.
#
# Where a value need NOT appear twice, it does not: the ffmpeg pin lives in the
# Dockerfile's ARGs and scripts/install-ffmpeg.sh PARSES it. NOTICE is the exception that
# forces this script to exist — it must literally name the ffmpeg build it ships, because
# it is the GPL corresponding-source record that travels inside the image and inside
# every release tarball. It cannot point at a Dockerfile the user does not have.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail=0

note() { printf '  %s\n' "$*"; }
bad()  { printf '::error::%s\n' "$*" >&2; fail=1; }

arg() { sed -n "s/^ARG $1=\\(.*\\)$/\\1/p" "$here/Dockerfile" | head -1; }

# --- 1. NOTICE must name the exact ffmpeg the image bundles ------------------------
# It is the source offer for the GPL binaries the image redistributes. If it drifts, the
# image ships binaries whose licence record names a DIFFERENT upstream build.
df_build="$(arg FFMPEG_BUILD)"
df_version="$(arg FFMPEG_VERSION)"
[ -n "$df_build" ] && [ -n "$df_version" ] || bad "could not read FFMPEG_BUILD/FFMPEG_VERSION from Dockerfile"

no_build="$(sed -n 's/^ *Build tag *: *\(.*[^ ]\) *$/\1/p'  "$here/NOTICE" | head -1)"
no_version="$(sed -n 's/^ *Version *: *\(.*[^ ]\) *$/\1/p'  "$here/NOTICE" | head -1)"

if [ "$no_build" = "$df_build" ] && [ "$no_version" = "$df_version" ]; then
  note "ok: NOTICE names the ffmpeg the Dockerfile pins ($df_version, $df_build)"
else
  bad "NOTICE does not match the Dockerfile's ffmpeg pin — the image would redistribute GPL binaries whose source offer names a different build.
       Dockerfile: build=$df_build version=$df_version
       NOTICE:     build=$no_build version=$no_version"
fi

# ... and those two lines are the ONLY ffmpeg build identifiers NOTICE is allowed to
# carry. It used to also spell the git revision out a third time, in the corresponding-
# source paragraph ("revision g90436de5e1 above"), where nothing checked it: the two
# checked lines could be bumped correctly while the source offer went on naming the
# revision of a build the image no longer contains. A restatement no gate looks at is
# exactly the failure this whole script exists to prevent, so a stray one is now RED
# rather than merely regrettable. (Both known values are erased first, so the legitimate
# Build tag and Version lines are not their own violation.)
scan_txt="$(sed -e "s|$df_version||g" -e "s|$df_build||g" "$here/NOTICE")"
strays="$(printf '%s\n' "$scan_txt" \
  | grep -oE 'autobuild-[0-9]{4}(-[0-9]{2}){4}|N-[0-9]+-g[0-9a-f]{7,}|\bg[0-9a-f]{10,}\b' \
  | sort -u || true)"
if [ -z "$strays" ]; then
  note "ok: NOTICE carries no ffmpeg build identifier beyond the two checked lines"
else
  bad "NOTICE names an ffmpeg build identifier that is NOT the pinned one, on a line no check matches. It would drift silently and leave the GPL source offer pointing at a build the image does not contain.
       stray: $(printf '%s' "$strays" | tr '\n' ' ')
       pinned: build=$df_build version=$df_version
       Refer to the 'Build tag'/'Version' lines instead of restating the value."
fi

# --- 2. The ffmpeg pin must be one upstream will STILL SERVE next year -------------
# This is the check that would have prevented S0022. The pin was
# autobuild-2026-07-13-14-11, a mid-month DAILY build, and BtbN/FFmpeg-Builds publishes
# its retention policy in the repository README:
#
#     "The last build of each month is kept for two years.
#      The last 14 daily builds are kept.
#      The special 'latest' build floats and provides consistent URLs always
#      pointing to the latest build."
#
# So a daily pin has a roughly two-week fuse and was always going to 404, and it did:
# every job that installs ffmpeg went red, on unrelated pull requests, across three
# specs, and the cause was not visible from any of them. A month-end pin has a TWO-YEAR
# fuse. `latest` never 404s and is worse than either: it silently changes the encoder
# AND the libvmaf instrument the no-loss verdict is measured with, which is a pin that
# does not pin. Both are refused here, at the only moment a human is looking.
#
# Local arithmetic only: no network. `make check` runs on every PR, and a gate that
# depends on a third party being up is a gate that reds for reasons the author cannot
# fix. The live reachability probe is `make check-pin-live`, on a schedule, deliberately
# NOT here.
if [[ "$df_build" =~ ^autobuild-([0-9]{4})-([0-9]{2})-([0-9]{2})-[0-9]{2}-[0-9]{2}$ ]]; then
  pin_day="${BASH_REMATCH[1]}-${BASH_REMATCH[2]}-${BASH_REMATCH[3]}"
  if ! next_day="$(date -u -d "$pin_day +1 day" +%d 2>/dev/null)"; then
    bad "FFMPEG_BUILD names an impossible date ($pin_day): '$df_build' is not a real upstream release tag."
  elif [ "$next_day" != "01" ]; then
    bad "FFMPEG_BUILD is a MID-MONTH DAILY build ($df_build) and upstream does not keep it for a year.
       Upstream's published retention policy (BtbN/FFmpeg-Builds README):
         'The last build of each month is kept for two years.
          The last 14 daily builds are kept.'
       A daily build therefore survives about a FORTNIGHT. Pin the last build of a month
       (a tag whose date is the final calendar day of that month), which is retained for
       two years, and put its published SHA-256 digests in FFMPEG_SHA256_AMD64/ARM64."
  else
    note "ok: ffmpeg pin $df_build is a month-end build (upstream keeps it two years, until $(date -u -d "$pin_day +2 years" +%F))"
  fi
else
  bad "FFMPEG_BUILD is not a dated upstream release tag: '$df_build'.
       It must match autobuild-YYYY-MM-DD-HH-MM. A floating alias such as 'latest' is
       REFUSED outright: upstream documents it as 'the special latest build floats and
       provides consistent URLs always pointing to the latest build', so it would never
       404 and would instead change the encoder, and the libvmaf instrument the no-loss
       verdict is measured with, under a green build. That is a pin that does not pin."
fi

# The digest is the other half of the pin, and it is the half that makes the tag mean
# anything: a tag says which URL, the digest says which BYTES. Dropping or blanking it is
# the cheapest way to make a rotted pin "work", so its shape is asserted rather than
# assumed. (The installer and the Dockerfile both verify against these before they trust
# an archive; an empty value would make that verification vacuous.)
for a in AMD64 ARM64; do
  d="$(arg "FFMPEG_SHA256_$a")"
  if [[ "$d" =~ ^[0-9a-f]{64}$ ]]; then
    note "ok: FFMPEG_SHA256_$a is a full sha256 digest"
  else
    bad "FFMPEG_SHA256_$a is not a 64-character lowercase sha256 digest (got '$d'). The archive verification it feeds would be vacuous, and an ffmpeg that is merely 'whatever answered 200' is also the instrument that decides a source may be deleted."
  fi
done

# --- 3. One Go version across the proof and the artifact ---------------------------
# The gate must run on the Go that builds the binary we ship. Nothing forces these three
# together but this check.
go_image="$(arg GO_IMAGE)"                       # golang:1.25.14-bookworm@sha256:...
docker_go="${go_image#golang:}"; docker_go="${docker_go%%-*}"
ci_go="$(sed -n 's/^ *GO_VERSION: *"\(.*\)"$/\1/p' "$here/.github/workflows/ci.yml" | head -1)"
rel_go="$(sed -n 's/^ *GO_VERSION: *"\(.*\)"$/\1/p' "$here/.github/workflows/release.yml" | head -1)"

if [ -n "$docker_go" ] && [ "$ci_go" = "$docker_go" ] && [ "$rel_go" = "$docker_go" ]; then
  note "ok: one Go version everywhere ($docker_go — Dockerfile, ci.yml, release.yml)"
else
  bad "Go version drift — the gate would run on a different Go than the shipped binary is built with.
       Dockerfile GO_IMAGE: $docker_go
       ci.yml GO_VERSION:   $ci_go
       release.yml:         $rel_go"
fi

# The tag above is only a LABEL. What Docker pulls is the digest beside it, and this
# script cannot look inside an image to learn which toolchain a digest carries, so the
# tag comparison is satisfied by a GO_IMAGE whose digest was never moved. Two halves
# close that: the Dockerfile's build stage asks the pulled image its own `go env
# GOVERSION` and refuses when it disagrees with the tag, and this checks the half that
# the build cannot: that a digest is pinned AT ALL. Drop the `@sha256:` and the tag
# floats to whatever the registry serves today, and the in-image assertion still passes
# because a floating tag is self-consistent.
go_digest=""
case "$go_image" in *@*) go_digest="${go_image##*@}" ;; esac
if printf '%s' "$go_digest" | grep -qE '^sha256:[0-9a-f]{64}$'; then
  note "ok: GO_IMAGE is digest-pinned ($go_digest)"
else
  bad "GO_IMAGE is not pinned to a well-formed @sha256: digest. The toolchain would float to
       whatever the registry serves today, and the in-image assertion cannot catch that
       because a floating tag agrees with itself.
       Dockerfile GO_IMAGE: $go_image"
fi

# --- 4. No pre-rename identifier survives (TRANSCODE-12) ---------------------------
# The project was `transcode` and is now `holdfast`. The rename had to land BEFORE the
# first tag because none of these surfaces can be redirected afterwards: Go has no
# module-path rename primitive, nothing rewrites a container-image reference in a user's
# compose file, and a renamed Prometheus metric silently breaks every dashboard built on
# it. So a MISSED rename must fail LOUD here rather than ship — a half-renamed metric or
# env var is not cosmetic, it is a permanent, unfixable break for whoever adopts it first.
#
# Scope is deliberately narrow: IDENTIFIERS only. Prose is NOT matched, because "transcode"
# is still an ordinary English verb here ("an in-place transcode"), `transcode.conf` is the
# Bash predecessor's config file, and the phase IDs (TRANSCODE-1 … TRANSCODE-15) are
# historical labels that must survive — they are how git log and the roadmap name the work.
# Hyphen = history, underscore = identifier. Only the underscore forms are a bug.
#
# The allowlist is line-level, never file-level, and git applies it (`--and --not -e`) rather
# than this script post-processing git grep's `path:lineno:content` output — which was tried
# twice and was a file-level exemption by accident both times (a PATH containing the allow
# term exempted every line in the file). A line may name a banned identifier only to PROHIBIT
# it — the rule in CLAUDE.md has to quote what it forbids — and must carry this marker, which
# keeps every exemption greppable. Exactly one line in the repo does.
allow='rename-guard-allow'

# The pattern is COMPOSED so this file does not contain the identifiers it bans, and so needs
# no exemption to scan itself. `-a` is load-bearing and `-I` must not come back: they are
# opposites (`-I` SKIPS binary files, `-a` searches them), and with `-I` this check went green
# over a tracked 18MB binary carrying the old auth-token env var. git's exit status is checked,
# not swallowed — grep exits 0=found, 1=none, >1=error, and an error must be RED (`fatal:
# detected dubious ownership` is routine in containerised CI, and swallowing it turned this
# guard into a no-op that printed "ok").
env_ns='TRANSCODE''_'                        # the old env prefix        (composed)
met_ns='transcode''_'                        # the old metric namespace  (composed)
old_mod='github\.com/NSchatz/transcode'      # regex: the backslash means this line is not its own match
old_img='ghcr\.io/[Nn][Ss]chatz/transcode'   # ditto — the old image ref, equally unredirectable
pat="${env_ns}|${met_ns}|${old_mod}|${old_img}"

# BOTH the worktree and the index are scanned; a hit in either is a leak. `git grep` with no
# rev reads the WORKTREE, but what gets committed is the INDEX: stage a leak, delete it from
# disk, and a worktree-only scan prints "ok" over an identifier that is about to ship. The
# worktree pass catches what you are about to stage; the --cached pass catches what you
# already staged. Neither alone is the thing that gets committed.
scan() {  # <extra-git-grep-args…> — prints matches, returns git's own exit status
  git -C "$here" grep -naE "$@" -e "$pat" --and --not -e "$allow" -- . 2>&1
}
set +e
wt="$(scan)";              rc_wt=$?
idx="$(scan --cached)";    rc_idx=$?
set -e
if [ "$rc_wt" -gt 1 ] || [ "$rc_idx" -gt 1 ]; then
  # Do NOT fall through to the "ok" note: a check that could not run has not passed, and
  # printing an attestation beside its own error is how a gate comes to be believed.
  bad "git grep could not run (worktree exit $rc_wt, index exit $rc_idx) — the rename guard did NOT execute. Refusing to report green.
$(printf '%s\n%s\n' "$wt" "$idx" | sed 's/^/       /')"
else
  leaks="$(printf '%s\n%s\n' "$wt" "$idx" | grep -v '^$' | sort -u || true)"
  if [ -z "$leaks" ]; then
    note "ok: no pre-rename identifier survives, in the worktree OR the index"
  else
    bad "pre-rename identifier(s) survived the holdfast rename — these CANNOT be redirected after the first tag:
$(printf '%s\n' "$leaks" | sed 's/^/       /')"
  fi
fi

[ "$fail" -eq 0 ] || exit 1
echo "pins agree"
