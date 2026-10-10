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

# --- 0. Everything this gate reads must actually be there --------------------------
# A check that could not run has NOT passed. Every assertion below is of the form "read
# these files and compare them", so a file that has been moved, renamed or made
# unreadable does not make its assertion vacuous in a visible way - it makes it vacuous
# in an INVISIBLE one, because `sed` on a missing file prints nothing and an empty value
# compares equal to another empty value. That is the same silent-green failure this whole
# script exists to prevent, so the dependency list is stated up front and checked first.
#
# It exits IMMEDIATELY rather than accumulating into `fail`: with a required file absent,
# every downstream section would be reading emptiness, and their messages would describe
# a drift that is not the actual problem. Name what could not be read, and stop.
need_file() {
  if [ ! -e "$here/$1" ]; then
    bad "MISSING: $1 - this gate reads it to $2, so the check cannot run. A check that could not run has not passed; refusing to report that the pins agree."
  elif [ ! -f "$here/$1" ] || [ ! -r "$here/$1" ]; then
    bad "UNREADABLE: $1 - this gate reads it to $2, so the check cannot run. A check that could not run has not passed; refusing to report that the pins agree."
  fi
}
need_dir() {
  if [ ! -e "$here/$1" ]; then
    bad "MISSING: $1/ - this gate enumerates it to $2, so the check cannot run. An empty enumeration would pass every reference it never saw; refusing to report that the pins agree."
  elif [ ! -d "$here/$1" ] || [ ! -r "$here/$1" ] || [ ! -x "$here/$1" ]; then
    bad "UNREADABLE: $1/ - this gate enumerates it to $2, so the check cannot run. An empty enumeration would pass every reference it never saw; refusing to report that the pins agree."
  fi
}

need_file "Dockerfile"                     "read the ffmpeg pin, the Go toolchain pin and every base image"
need_file "NOTICE"                         "confirm the GPL source offer names the ffmpeg the image bundles"
need_file "docker-compose.yml"             "confirm the example deployment pins the image it pulls"
need_dir  ".github/workflows"              "confirm every action is pinned to a commit SHA"
need_file ".github/workflows/ci.yml"       "confirm the gate runs on the Go the shipped binary is built with"
need_file ".github/workflows/release.yml"  "confirm the release runs on the Go the shipped binary is built with"
need_file ".github/dependabot.yml"         "confirm the update bot watches every pin class it can read"
need_file "web/package.json"               "read the web UI's pnpm pin and its dependency versions"
need_file "web/.node-version"              "read the web UI's Node pin"
need_file "web/pnpm-workspace.yaml"        "confirm pnpm runs no lifecycle script and installs no version younger than a day"
need_file "web/pnpm-lock.yaml"             "confirm the web UI's dependencies are resolved by a committed lockfile"

if [ "$fail" -ne 0 ]; then
  printf '::error::%s\n' "check-pins: a file this gate depends on could not be read (named above). Refusing to report green." >&2
  exit 1
fi

# The workflow files, enumerated ONCE here and read by every section that needs them: the Go
# toolchain pin in section 3 and the action references in section 5. Each of those sections
# refuses an enumeration that found nothing, because an empty one passes every file it never
# saw.
wf_files=()
while IFS= read -r f; do
  [ -n "$f" ] && wf_files+=("$f")
done < <(find "$here/.github/workflows" -maxdepth 1 -type f \( -name '*.yml' -o -name '*.yaml' \) 2>/dev/null | sort)

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
# The gate must run on the Go that builds the binary we ship. Nothing forces these files
# together but this check.
#
# The workflows are ENUMERATED, never listed by hand. This section used to read ci.yml and
# release.yml BY NAME, so a third workflow declaring GO_VERSION was a restatement no check
# compared - it could drift to any 1.26.x, or past a govulncheck stdlib advisory, under a
# green build, which is this script's own founding failure. A workflow added later is
# covered on the day it lands, the way section 5 already covers its actions.
go_image="$(arg GO_IMAGE)"                       # golang:1.26.9-trixie@sha256:...
docker_go="${go_image#golang:}"; docker_go="${docker_go%%-*}"

go_wfs=()
go_drift=0
for wf in "${wf_files[@]}"; do
  rel="${wf#"$here"/}"
  wf_go="$(sed -n 's/^ *GO_VERSION: *"\(.*\)"$/\1/p' "$wf" | head -1)"
  [ -n "$wf_go" ] || continue
  go_wfs+=("$rel")
  [ "$wf_go" = "$docker_go" ] && continue
  go_drift=$((go_drift + 1))
  bad "Go version drift - $rel would run on a different Go than the shipped binary is built with.
       Dockerfile GO_IMAGE: $docker_go
       $rel GO_VERSION:     $wf_go"
done

# ci.yml and release.yml are REQUIRED to declare one: they are the gate and the release,
# and a workflow that stopped declaring it would silently run on whatever setup-go resolves
# today - which is the same drift, arrived at by deletion rather than by edit.
for req in .github/workflows/ci.yml .github/workflows/release.yml; do
  case " ${go_wfs[*]} " in
    *" $req "*) ;;
    *)
      go_drift=$((go_drift + 1))
      bad "$req declares no GO_VERSION, so nothing holds it to the Dockerfile's toolchain. It would run on whatever actions/setup-go resolves on the day, and the proof would detach from the artifact."
      ;;
  esac
done

if [ -z "$docker_go" ]; then
  bad "could not read a Go toolchain out of the Dockerfile's GO_IMAGE ARG ('$go_image'), so there is nothing to hold the workflows equal to. A check that could not run has not passed."
elif [ "$go_drift" -eq 0 ]; then
  note "ok: one Go version everywhere ($docker_go - Dockerfile and ${#go_wfs[@]} workflow file(s): ${go_wfs[*]})"
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

# GO_IMAGE is a COPY. The pin is the build stage's own FROM line: that is what Docker pulls,
# and it is the only form the update bot reads (section 7). The ARG exists because the build
# stage's in-image toolchain assertion cannot see the reference its own stage was built
# from. A value that genuinely has to appear twice is held equal HERE, so whoever moves one
# of the two - a bot's pull request included - is told, instead of the image being built
# from one toolchain while the assertion checks the other.
build_from="$(awk 'toupper($1) == "FROM" && NF >= 4 && toupper($(NF-1)) == "AS" && $NF == "build" {
    for (i = 2; i < NF - 1; i++) if ($i !~ /^--/) { print $i; exit }
  }' "$here/Dockerfile")"
if [ -z "$build_from" ]; then
  bad "the Dockerfile has no 'FROM <image> AS build' stage this check could read, so nothing
       says which toolchain image the shipped binary is built with. A check that could not
       run has not passed."
elif [ "$build_from" != "$go_image" ]; then
  bad "GO_IMAGE disagrees with the build stage it is a copy of.
       build stage FROM: $build_from
       ARG GO_IMAGE:     $go_image
       The binary is built FROM the first, and the in-image toolchain assertion reads the
       second. Move both together; a Go bump moves every workflow's GO_VERSION as well."
else
  note "ok: GO_IMAGE is the build stage's own FROM reference ($build_from)"
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
# Bash predecessor's config file, and the phase IDs (TRANSCODE-1 to TRANSCODE-17; there is no
# TRANSCODE-10) are historical labels that must survive - they are how git log names the work.
# Hyphen = history, underscore = identifier. Only the underscore forms are a bug.
#
# The allowlist is line-level, never file-level, and git applies it (`--and --not -e`) rather
# than this script post-processing git grep's `path:lineno:content` output — which was tried
# twice and was a file-level exemption by accident both times (a PATH containing the allow
# term exempted every line in the file). A line may name a banned identifier only to PROHIBIT
# it — the rule in docs/development.md has to quote what it forbids — and must carry this marker, which
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

# --- 5. Every action is pinned to a commit SHA (P3) --------------------------------
# A tag like `v4` is MUTABLE and is moved by its publisher, so `uses: actions/checkout@v4`
# is a standing grant to run whatever that publisher pushes there next, inside a job that
# holds this repository's credentials. That is a supply-chain path straight into the
# build, and - for this repository specifically - into the job that pushes the image whose
# bundled ffmpeg is the instrument the no-loss verdict is measured with.
#
# The SHA is the pin; the trailing comment is what keeps it READABLE, and it is required
# rather than encouraged: a bare 40-hex reference tells a reviewer nothing about how far
# behind it is, so the comment is the only thing that makes a bump reviewable. Whether the
# SHA really IS that version is a question only GitHub can answer, and asking it here
# would put a third party's availability inside `make check` - refused, same as the ffmpeg
# liveness probe. This checks the SHAPE, which is the half that can be checked offline.
#
# The directory is ENUMERATED, never listed by hand (the list is built in section 0): a
# workflow added later must be covered by the pin gate on the day it lands, not on the day
# somebody remembers to add it here. `runs-on: ubuntu-latest` is deliberately NOT matched -
# that is a runner label, not an image reference.
if [ "${#wf_files[@]}" -eq 0 ]; then
  bad ".github/workflows/ contains no workflow files - this check enumerates that directory, so it just asserted nothing at all. An empty enumeration passes every reference it never saw."
else
  uses_total=0
  uses_bad=0
  for wf in "${wf_files[@]}"; do
    rel="${wf#"$here"/}"
    while IFS=: read -r lineno rest; do
      [ -n "$lineno" ] || continue
      trimmed="$(printf '%s' "$rest" | sed 's/^[[:space:]]*//; s/^-[[:space:]]*//')"
      case "$trimmed" in
        uses:*) ;;
        *) continue ;;                      # prose, or a comment that merely says "uses:"
      esac
      uses_total=$((uses_total + 1))

      val="${trimmed#uses:}"
      before="${val%%#*}"
      if [ "$before" = "$val" ]; then comment=""; else comment="${val#*#}"; fi
      ref="$(printf '%s' "$before" | tr -d '\042\047' | awk '{print $1}')"
      ver="$(printf '%s' "$comment" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"

      # A local action is part of THIS repository and moves only when this repository
      # moves, so there is no upstream publisher to pin against.
      case "$ref" in
        ./*|../*)
          note "ok: $rel:$lineno uses a local action ($ref) - nothing upstream to pin"
          continue
          ;;
      esac

      if ! printf '%s' "$ref" | grep -qE '^[A-Za-z0-9._-]+/[A-Za-z0-9._/-]+@[0-9a-f]{40}$'; then
        uses_bad=$((uses_bad + 1))
        bad "MUTABLE ACTION REFERENCE at $rel line $lineno: '$ref'
       An action must be pinned to a 40-character lowercase commit SHA:
         uses: owner/action@0123456789abcdef0123456789abcdef01234567 # v4
       A tag or branch is moved by its publisher, so this reference runs whatever they
       push there next - inside a job holding this repository's credentials.
       Resolve the tag once and pin the commit:
         gh api repos/OWNER/ACTION/git/ref/tags/TAG --jq .object.sha"
        continue
      fi

      if [ -z "$ver" ]; then
        uses_bad=$((uses_bad + 1))
        bad "UNREADABLE ACTION PIN at $rel line $lineno: '$ref'
       The SHA is correct but there is no trailing version comment, so nobody reviewing a
       bump can tell what this pin IS or how far behind it has fallen. Add the version the
       SHA was resolved from:
         uses: $ref # v4"
      fi
    done < <(grep -nE '(^|[[:space:]])uses:' "$wf" || true)
  done
  if [ "$uses_bad" -eq 0 ]; then
    note "ok: all $uses_total action reference(s) across ${#wf_files[@]} workflow file(s) are SHA-pinned with a version comment"
  fi
fi

# --- 6. The example deployment pins the image it pulls (P1) ------------------------
# docker-compose.yml is not documentation, it is the stack definition a stranger copies
# and runs. `image: ghcr.io/nschatz/holdfast:latest` hands them whatever that tag points
# at on the day they pull - and `:latest` is the tag release.yml MOVES, so the example
# would silently change the encoder and the libvmaf instrument the no-loss verdict is
# measured with, under a user who changed nothing. A digest is the only reference that
# names the artifact that was actually gated by scripts/smoke-image.sh.
#
# Publishing `:latest` is NOT depending on it: release.yml promotes that tag onto a digest
# that has already passed the smoke gate, and this check never reads release.yml. P1
# forbids depending on a mutable reference, not publishing one.
#
# The exemption is narrow and deliberate: a service that declares `build:` builds from
# this checkout, so its `image:` is a LOCAL NAME for the thing just built, not something
# fetched from a registry. `latest` is refused either way - as a name it says nothing, and
# as a tag it floats.
compose_map="$(awk '
  {
    line = $0
    stripped = line
    sub(/^[[:space:]]*/, "", stripped)
    if (stripped ~ /^#/ || stripped == "") next
    indent = match(line, /[^ ]/) - 1
    if (line ~ /^services:[[:space:]]*$/) { in_svc = 1; svc_indent = -1; next }
    if (indent == 0) { in_svc = 0; next }
    if (!in_svc) next
    if (svc_indent == -1) svc_indent = indent
    if (indent == svc_indent && stripped ~ /^[A-Za-z0-9._-]+:[[:space:]]*$/) {
      name = stripped; sub(/:[[:space:]]*$/, "", name); cur = name; next
    }
    if (indent > svc_indent && cur != "") {
      if (stripped ~ /^image:/) {
        v = stripped; sub(/^image:[[:space:]]*/, "", v)
        printf "IMAGE\t%s\t%s\t%s\n", cur, NR, v
      } else if (stripped ~ /^build:/) {
        printf "BUILD\t%s\t%s\t\n", cur, NR
      }
    }
  }
' "$here/docker-compose.yml")"

build_svcs=" "
while IFS=$'\t' read -r kind svc _ln _v; do
  [ "$kind" = "BUILD" ] || continue
  build_svcs="$build_svcs$svc "
done <<<"$compose_map"

compose_imgs=0
compose_bad=0
while IFS=$'\t' read -r kind svc lineno raw; do
  [ "$kind" = "IMAGE" ] || continue
  compose_imgs=$((compose_imgs + 1))
  img="$(printf '%s' "$raw" | sed 's/[[:space:]]*#.*$//; s/[[:space:]]*$//' | tr -d '\042\047')"

  digest=""
  case "$img" in *@*) digest="${img##*@}" ;; esac
  namepart="${img%@*}"
  lastseg="${namepart##*/}"
  tag=""
  case "$lastseg" in *:*) tag="${lastseg##*:}" ;; esac
  is_registry=0
  case "$namepart" in */*) is_registry=1 ;; esac
  has_build=0
  case "$build_svcs" in *" $svc "*) has_build=1 ;; esac

  if [ "$tag" = "latest" ]; then
    compose_bad=$((compose_bad + 1))
    bad "FLOATING ':latest' IMAGE in docker-compose.yml line $lineno (service '$svc'): '$img'
       \`latest\` is never a reference anything depends on. release.yml MOVES that tag onto
       each newly gated release, so a user who changed nothing would silently get a
       different encoder - and a different libvmaf instrument for the no-loss verdict -
       on their next \`docker compose pull\`.
       Pin the version tag AND the digest that scripts/smoke-image.sh actually gated:
         image: ghcr.io/nschatz/holdfast:vX.Y.Z@sha256:<64 hex>
       Resolve one with: docker buildx imagetools inspect ghcr.io/nschatz/holdfast:vX.Y.Z"
    continue
  fi

  if [ "$is_registry" -eq 1 ]; then
    if ! printf '%s' "$digest" | grep -qE '^sha256:[0-9a-f]{64}$'; then
      compose_bad=$((compose_bad + 1))
      bad "UNPINNED IMAGE in docker-compose.yml line $lineno (service '$svc'): '$img'
       A registry reference is pinned by tag AND digest - the tag stays readable to a
       human, the digest is what actually resolves. Without the digest this example pulls
       whatever the registry serves that day, which need not be an image that ever passed
       scripts/smoke-image.sh.
         image: ${namepart}:vX.Y.Z@sha256:<64 hex>"
      continue
    fi
    if [ -z "$tag" ]; then
      compose_bad=$((compose_bad + 1))
      bad "DIGEST-ONLY IMAGE in docker-compose.yml line $lineno (service '$svc'): '$img'
       The digest is right but the tag is missing, and a bare digest tells a human reading
       this file nothing about which release they are running. Carry both:
         image: ${namepart}:vX.Y.Z@$digest"
      continue
    fi
  elif [ "$has_build" -eq 0 ]; then
    compose_bad=$((compose_bad + 1))
    bad "UNRESOLVABLE IMAGE in docker-compose.yml line $lineno (service '$svc'): '$img'
       This is a bare local name, but service '$svc' declares no \`build:\`, so there is
       nothing in this checkout that produces it and compose would try to PULL it. Either
       add a \`build:\` stanza to that service, or use a registry reference pinned by tag
       and digest."
  fi
done <<<"$compose_map"

if [ "$compose_imgs" -eq 0 ]; then
  bad "docker-compose.yml declares no service \`image:\` at all - this check parses that file and just asserted nothing. Either the example lost its image reference or the parser stopped understanding the file; both are refusals, not a green build."
elif [ "$compose_bad" -eq 0 ]; then
  note "ok: all $compose_imgs docker-compose.yml image reference(s) are pinned (tag + digest, or a local build)"
fi

# --- 7. Every base image is pinned by tag AND digest, on its FROM line (P2) -------
# A `FROM` line is an image reference like any other. The fetch and runtime bases could
# lose their digests silently, and the runtime base is the worst of them to lose - it is
# the base the shipped image IS, and the in-image toolchain assertion that backstops the
# build stage cannot see it at all, because nothing in the runtime stage runs.
#
# The reference must also be WRITTEN on the FROM line. The update bot reads FROM lines and
# nothing else - Dependabot's Docker parser matches `FROM [--platform=...] <image>:<tag>
# @sha256:<digest>` and never resolves an ARG (dependabot-core docker/lib/dependabot/docker/
# file_parser.rb, FROM_LINE, at 78005a8; read 2026-09-29) - so a base written through an
# ARG is a pin nobody is ever told has gone stale. That is refused here, which is what makes
# "the bot watches every base image" true of a Dockerfile edited after this line was written.
stages=" "
from_seen=0
from_bad=0
while IFS=: read -r lineno content; do
  [ -n "$lineno" ] || continue
  prev_stages="$stages"
  stage="$(printf '%s' "$content" | sed -n 's/.*[[:space:]][Aa][Ss][[:space:]]\{1,\}\([A-Za-z0-9._-]\{1,\}\).*/\1/p')"
  [ -n "$stage" ] && stages="$stages$stage "

  imgtok="$(printf '%s' "$content" \
    | sed 's/^[[:space:]]*[Ff][Rr][Oo][Mm][[:space:]]\{1,\}//' \
    | awk '{ for (i = 1; i <= NF; i++) if ($i !~ /^--/) { print $i; exit } }')"
  [ -n "$imgtok" ] || continue

  case "$imgtok" in
    '$'*)
      from_seen=$((from_seen + 1))
      from_bad=$((from_bad + 1))
      bad "BASE IMAGE THE UPDATE BOT CANNOT READ - Dockerfile line $lineno builds FROM $imgtok.
       The update bot reads the image reference written on a FROM line and never resolves
       an ARG, so this base would go stale with nobody told. Write the reference on the
       FROM line itself, tag and digest together:
         FROM <image>:<tag>@sha256:<64 hex>"
      continue
      ;;
  esac

  # A reference to an earlier build stage is not an image reference.
  case "$prev_stages" in *" $imgtok "*) continue ;; esac
  resolved="$imgtok"
  label="the base image on Dockerfile line $lineno"

  from_seen=$((from_seen + 1))
  digest=""
  case "$resolved" in *@*) digest="${resolved##*@}" ;; esac
  namepart="${resolved%@*}"
  lastseg="${namepart##*/}"
  tag=""
  case "$lastseg" in *:*) tag="${lastseg##*:}" ;; esac

  if ! printf '%s' "$digest" | grep -qE '^sha256:[0-9a-f]{64}$'; then
    from_bad=$((from_bad + 1))
    bad "UNPINNED BASE IMAGE - $label carries no \`@sha256:\` digest: '$resolved'
       It names a TAG, and a tag is moved by its publisher, so this base floats to
       whatever the registry serves on the day of the build. Pin both:
         FROM ${namepart}@sha256:<64 hex>
       Resolve one with: docker buildx imagetools inspect $namepart"
  elif [ -z "$tag" ]; then
    from_bad=$((from_bad + 1))
    bad "UNREADABLE BASE IMAGE PIN - $label has a digest but no tag: '$resolved'
       The digest is what resolves; the tag is what tells a human which base this is. P2
       requires both:
         FROM <image>:<tag>@$digest"
  elif [ "$tag" = "latest" ]; then
    from_bad=$((from_bad + 1))
    bad "FLOATING BASE IMAGE TAG - $label is tagged \`latest\`: '$resolved'
       \`latest\` is never a reference anything depends on, even beside a digest: the next
       person to refresh the digest would silently move to a different major version."
  fi
done < <(grep -nE '^[[:space:]]*[Ff][Rr][Oo][Mm][[:space:]]' "$here/Dockerfile" || true)

if [ "$from_seen" -eq 0 ]; then
  bad "the Dockerfile declares no base image this check could resolve - it just asserted nothing. A parser that stopped understanding the Dockerfile is a refusal, not a green build."
elif [ "$from_bad" -eq 0 ]; then
  note "ok: all $from_seen base image(s) are written on the Dockerfile's FROM lines, each pinned by tag AND digest"
fi

# --- 8. A node manifest must carry a lifecycle-script decision (P4) ----------------
# Installing from a package.json gives every transitive dependency the right to run
# arbitrary `preinstall`/`postinstall` code, on a runner holding this repository's
# credentials. So every manifest in the tree must sit beside a COMMITTED decision about
# that, in the file ITS package manager actually reads.
#
# Which file that is depends on the package manager, and getting it wrong is silent:
#
#   pnpm   pnpm-workspace.yaml. pnpm 11 and later read NO non-auth setting from .npmrc
#          ("pnpm no longer reads non-auth settings from .npmrc",
#          https://pnpm.io/blog/releases/11.0, read 2026-09-29), and that was measured
#          here with a root `postinstall` probe: under pnpm 11.27.1, 12.6.0 and 12.8.1 an
#          .npmrc carrying `ignore-scripts=true` did NOT stop the script, and
#          `ignoreScripts: true` in pnpm-workspace.yaml did
#          (https://github.com/NSchatz/holdfast/blob/2fd9d5986a101e4ae9d5394d2a42a3a158e3a4bf/.claude/goals/2026-09-holdfast-research/verify-nodes-clients-spa.md, claim 3).
#          An .npmrc is therefore NOT a decision for a pnpm project, whatever it says:
#          accepting one would be a green check on a line nothing reads.
#          Required, at the top level of the file:
#            ignoreScripts: true            no reason unlocks `false` - the UI's toolchain
#                                           needs no lifecycle script, and section 12 is
#                                           where a build that needs one would be argued
#            minimumReleaseAge: <minutes>   explicit, and at least 1440 (one day)
#          Refused:
#            minimumReleaseAgeExclude       pnpm WRITES this list itself when it meets a pin
#                                           younger than the default age, unless the age
#                                           is set explicitly - so its presence means a
#                                           young version was let through by the tool
#                                           and nobody decided it (the same verify file,
#                                           claim 4)
#            allowBuilds: <name>: true      a dependency granted its build script
#            dangerouslyAllowAllBuilds      the same grant, to all of them
#
#   npm    .npmrc, beside the manifest or at the repository root:
#            ignore-scripts=true    - scripts off. Nothing further needed.
#            ignore-scripts=false   - scripts on, and only legal beside a line carrying
#                                     `lifecycle-scripts-reason: <why>` in the same file.
#
# A manifest is pnpm's when it names pnpm in `packageManager`, or sits beside a
# pnpm-lock.yaml or a pnpm-workspace.yaml. Anything else is npm's.
#
# It scans the WORKING TREE, not the tracked files, on purpose. The moment to refuse a
# manifest is the moment it is about to be committed - a tracked-files-only check would
# stay green through the entire pull request that introduces it and only bite afterwards,
# which is exactly one merge too late. The DECISION, by contrast, must be tracked: a
# decision that is not in the repository is not a decision anybody after you can see.
node_manifests=()
while IFS= read -r m; do
  [ -n "$m" ] && node_manifests+=("$m")
done < <(find "$here" \( -name .git -o -name node_modules -o -name vendor \) -prune -o -type f -name package.json -print 2>/dev/null | sort)

# yaml_top <file> <key>: the value of a TOP-LEVEL scalar key, comment and quotes
# stripped; nothing if the key is absent, indented, or opens a block.
yaml_top() {
  sed -n "s/^$2:[[:space:]]*\\([^#]*[^#[:space:]]\\)[[:space:]]*\\(#.*\\)\\{0,1\\}\$/\\1/p" "$1" | head -1 | tr -d "\"'"
}
# yaml_has_top <file> <key>: the key appears at the top level in ANY form - a scalar, an
# inline list or the opening of a block.
yaml_has_top() { grep -qE "^[\"']?$2[\"']?[[:space:]]*:" "$1"; }

if [ "${#node_manifests[@]}" -eq 0 ]; then
  bad "no node package manifest was found in the working tree, and web/package.json is one: the enumeration this section reads has stopped seeing it. An empty enumeration passes every manifest it never saw."
else
  for man in "${node_manifests[@]}"; do
    mrel="${man#"$here"/}"
    mdir="$(dirname "$man")"
    mdirrel="$(dirname "$mrel")"

    is_pnpm=0
    grep -qE '"packageManager"[[:space:]]*:[[:space:]]*"pnpm@' "$man" && is_pnpm=1
    [ -e "$mdir/pnpm-lock.yaml" ] && is_pnpm=1
    [ -e "$mdir/pnpm-workspace.yaml" ] && is_pnpm=1

    if [ "$is_pnpm" -eq 1 ]; then
      ws="$mdir/pnpm-workspace.yaml"
      wsrel="${ws#"$here"/}"
      npmrc_note=""
      for cand in "$mdir/.npmrc" "$here/.npmrc"; do
        if [ -f "$cand" ] && grep -qE '^[[:space:]]*ignore-scripts[[:space:]]*=' "$cand"; then
          npmrc_note="
       ${cand#"$here"/} sets ignore-scripts, and pnpm 11 and later DO NOT READ IT: under pnpm
       that line stops nothing, so it is not this manifest's decision."
          break
        fi
      done
      pnpm_bad=0
      if [ ! -f "$ws" ] || ! git -C "$here" ls-files --error-unmatch -- "$wsrel" >/dev/null 2>&1; then
        pnpm_bad=1
        bad "PNPM MANIFEST WITH NO LIFECYCLE-SCRIPT DECISION PNPM READS: $mrel
       It is a pnpm project and there is no committed $wsrel beside it.$npmrc_note
       pnpm takes this decision from pnpm-workspace.yaml and from nowhere else in the
       repository. Create it, with at least:
         ignoreScripts: true
         minimumReleaseAge: 1440
       Then commit it: a decision that is not in the repository is not a decision."
      else
        is_val="$(yaml_top "$ws" ignoreScripts)"
        if [ "$is_val" != "true" ]; then
          pnpm_bad=1
          bad "PNPM MANIFEST WITH NO LIFECYCLE-SCRIPT DECISION PNPM READS: $mrel
       $wsrel does not carry a top-level \`ignoreScripts: true\` (it reads: '${is_val:-absent}').$npmrc_note
       Without that line pnpm runs this project's own lifecycle scripts at install, on a
       runner holding this repository's credentials. Set, at the top level of $wsrel:
         ignoreScripts: true"
        fi
        if yaml_has_top "$ws" minimumReleaseAgeExclude; then
          pnpm_bad=1
          bad "RELEASE-AGE EXCLUDE LIST - $wsrel carries \`minimumReleaseAgeExclude\`.
       pnpm writes that list ITSELF when it installs a version younger than the release
       age and the age was not set explicitly, so each name on it is a version that
       reached the lockfile before it had been public for a day, let through by the tool.
       Delete the whole key, keep \`minimumReleaseAge\` explicit, and pin each package it
       named to a version that is at least a day old."
        fi
        mra="$(yaml_top "$ws" minimumReleaseAge)"
        if ! printf '%s' "$mra" | grep -qE '^[0-9]+$' || [ "$mra" -lt 1440 ]; then
          pnpm_bad=1
          bad "NO EXPLICIT RELEASE AGE - $wsrel does not set a top-level \`minimumReleaseAge\` of at least 1440 minutes (it reads: '${mra:-absent}').
       Left to its default the rule is not strict: pnpm installs a younger version anyway
       and records it in a \`minimumReleaseAgeExclude\` list it writes for itself. Set:
         minimumReleaseAge: 1440"
        fi
        if yaml_has_top "$ws" minimumReleaseAgeStrict && [ "$(yaml_top "$ws" minimumReleaseAgeStrict)" != "true" ]; then
          pnpm_bad=1
          bad "RELEASE AGE NOT ENFORCED - $wsrel sets \`minimumReleaseAgeStrict\` to something other than true.
       With it false pnpm installs a version younger than \`minimumReleaseAge\` anyway and
       writes the \`minimumReleaseAgeExclude\` list for itself (measured with pnpm 12.8.1,
       2026-10-03). Remove the key: an explicit \`minimumReleaseAge\` is strict by default."
        fi
        # 6: a pnpmfile is code pnpm runs at every install, whatever ignoreScripts says
        # (measured with pnpm 12.8.1, 2026-10-03).
        for pf in .pnpmfile.cjs .pnpmfile.mjs; do
          if [ -e "$mdir/$pf" ]; then
            pnpm_bad=1
            bad "CODE THAT RUNS AT INSTALL - $mdirrel/$pf exists. pnpm executes a pnpmfile at every install, and \`ignoreScripts: true\` does not stop it. The web UI needs no install hook; delete it."
          fi
        done
        if yaml_has_top "$ws" pnpmfile || yaml_has_top "$ws" globalPnpmfile; then
          pnpm_bad=1
          bad "CODE THAT RUNS AT INSTALL - $wsrel names a pnpmfile, which pnpm executes at every install whatever \`ignoreScripts\` says."
        fi
        if yaml_has_top "$ws" onlyBuiltDependencies; then
          pnpm_bad=1
          bad "DEPENDENCY BUILD SCRIPT ALLOWED - $wsrel carries \`onlyBuiltDependencies\`, a list of dependencies granted their build script."
        fi
        # An inline map after the key is a shape the block reader below does not read, so
        # it is refused rather than passed unread.
        if grep -qE '^allowBuilds:[[:space:]]*[^[:space:]#]' "$ws"; then
          pnpm_bad=1
          bad "DEPENDENCY BUILD SCRIPT ALLOWED - $wsrel writes \`allowBuilds\` on one line. Write it as a block, one \`<name>: false\` per line, so each entry can be read."
        fi
        if yaml_has_top "$ws" dangerouslyAllowAllBuilds && [ "$(yaml_top "$ws" dangerouslyAllowAllBuilds)" != "false" ]; then
          pnpm_bad=1
          bad "EVERY DEPENDENCY BUILD ALLOWED - $wsrel sets \`dangerouslyAllowAllBuilds\`, which grants every transitive dependency its install script."
        fi
        # allowBuilds is a block of `<name>: <bool>`; every entry must be a refusal.
        allowed="$(awk '
          /^allowBuilds:/ { inblock = 1; next }
          inblock && /^[^[:space:]#]/ { inblock = 0 }
          inblock && /^[[:space:]]+[^#[:space:]]/ {
            line = $0; sub(/[[:space:]]*#.*/, "", line); gsub(/["\047]/, "", line)
            n = split(line, kv, /:[[:space:]]*/)
            name = kv[1]; gsub(/^[[:space:]]+/, "", name)
            if (kv[n] != "false") print name
          }' "$ws" | tr '\n' ' ')"
        if [ -n "${allowed// /}" ]; then
          pnpm_bad=1
          bad "DEPENDENCY BUILD SCRIPT ALLOWED - $wsrel grants a build script under \`allowBuilds\` to: $allowed
       Every entry there must be \`false\`: an entry exists to say a dependency that
       declares an install script is deliberately NOT built, never to build one."
        fi
      fi
      if [ "$pnpm_bad" -eq 0 ]; then
        note "ok: $mrel is a pnpm project and $wsrel, the file pnpm reads, is committed with ignoreScripts: true, minimumReleaseAge: $mra, no minimumReleaseAgeExclude list and no build allowed"
      fi
      continue
    fi

    decided=""
    for cand in "$mdir/.npmrc" "$here/.npmrc"; do
      [ -f "$cand" ] || continue
      crel="${cand#"$here"/}"
      git -C "$here" ls-files --error-unmatch -- "$crel" >/dev/null 2>&1 || continue
      if grep -qE '^[[:space:]]*ignore-scripts[[:space:]]*=[[:space:]]*true[[:space:]]*$' "$cand"; then
        decided="$crel disables them (ignore-scripts=true)"
        break
      fi
      if grep -qE '^[[:space:]]*ignore-scripts[[:space:]]*=[[:space:]]*false[[:space:]]*$' "$cand" \
         && grep -qE 'lifecycle-scripts-reason:[[:space:]]*[^[:space:]]' "$cand"; then
        decided="$crel enables them with a committed reason"
        break
      fi
    done
    if [ -n "$decided" ]; then
      note "ok: $mrel has a committed lifecycle-script decision - $decided"
    else
      bad "NODE MANIFEST WITH NO LIFECYCLE-SCRIPT DECISION: $mrel
       Installing from this manifest would let every transitive dependency run arbitrary
       \`preinstall\`/\`postinstall\` code, on a runner holding this repository's
       credentials. The repository must either DISABLE lifecycle scripts in a committed
       .npmrc, or RECORD A REASON for enabling them:
         echo 'ignore-scripts=true' > $(printf '%s' "$mdirrel" | sed 's|^\.$||; s|$|/|; s|^/$||').npmrc
       or, to enable them deliberately, in that same committed .npmrc:
         ignore-scripts=false
         # lifecycle-scripts-reason: <why this repository needs them>
       Then commit it: a decision that is not in the repository is not a decision.
       (For a pnpm project the decision is \`ignoreScripts: true\` in pnpm-workspace.yaml:
       pnpm 11 and later do not read .npmrc for it.)"
    fi
  done
fi

# --- 9. The update bot watches every pin class it can read (S0151) -----------------
# A pin does not report its own staleness: a fix in a base image, an action or a module can
# sit unadopted for as long as nobody looks. .github/dependabot.yml is what looks - decided
# by the owner at Checkpoint T (S0151) - and it opens a pull request when a pin has moved
# upstream, which merges only through a human review of a green gate. It can only watch
# what it is configured to watch, so each class it can read is required here, at the
# repository root where the pins live:
#   github-actions  every `uses:` reference (section 5 keeps each SHA-pinned with a comment)
#   docker          every base image (section 7 keeps each written on its FROM line)
#   gomod           the module requirements in go.mod
#   npm             the web UI's packages in web/package.json, at /web (section 12 keeps each exact)
# The ffmpeg pin is not one of them - no ecosystem reads a GitHub release tag out of an ARG -
# and it stays with .github/workflows/pin-health.yml. Nothing here asks the network anything.
#
# The file is read one `updates` entry at a time, and an entry's keys may come in any
# order. A shape this reader does not understand (a `directories:` list, say) reads as a
# missing entry: a refusal to vouch, never a pass.
dep_entries="$(awk '
  function flush() { if (eco != "") print eco "|" dir; eco = ""; dir = "" }
  /^[[:space:]]*-[[:space:]]/ { flush() }
  /^[^[:space:]#]/ { flush() }
  /package-ecosystem:/ { v = $0; sub(/.*package-ecosystem:[[:space:]]*/, "", v); sub(/[[:space:]]*#.*/, "", v); gsub(/["\047[:space:]]/, "", v); eco = v }
  /^[[:space:]]*(-[[:space:]]*)?directory:/ { v = $0; sub(/.*directory:[[:space:]]*/, "", v); sub(/[[:space:]]*#.*/, "", v); gsub(/["\047[:space:]]/, "", v); dir = v }
  END { flush() }
' "$here/.github/dependabot.yml")"
dep_blind=()
for eco in github-actions docker gomod; do
  printf '%s\n' "$dep_entries" | grep -qxF "$eco|/" || dep_blind+=("$eco")
done
# The web UI's packages (web/package.json, section 12) are a fourth class, in their own
# directory.
printf '%s\n' "$dep_entries" | grep -qxF "npm|/web" || dep_blind+=("npm (directory: \"/web\")")
if [ "${#dep_blind[@]}" -ne 0 ]; then
  bad "UPDATE BOT BLIND SPOT - .github/dependabot.yml has no entry watching the repository
       root (package-ecosystem plus directory: \"/\"; the web UI's packages at \"/web\") for: ${dep_blind[*]}
       A pin class nobody watches goes stale with nobody told. The entries it read were:
$(printf '%s\n' "$dep_entries" | sed 's/^/         /')"
else
  note "ok: .github/dependabot.yml watches github-actions, docker and gomod at the repository root, and npm at /web"
fi

# --- 10. The hardware runtime's Debian packages: pinned, permanent, and in NOTICE ----
# The amd64 image copies shared objects out of Debian packages (the Dockerfile's
# hwruntime stage, P3). Three things make those copies what they claim to be, and each
# is checked here, offline:
#   - each package is pinned by exact version AND by the sha256 of its .deb, so the bytes
#     are the ones that were read and reviewed, not whatever answered 200;
#   - each is fetched from a snapshot.debian.org timestamp, never the live mirror: a pool
#     file on deb.debian.org is removed when a point release supersedes it, which is the
#     same two-week fuse as the ffmpeg daily build in section 2, and a floating `dists/`
#     path is a pin that does not pin;
#   - NOTICE names every package the image copies, at the version it copies, and nothing
#     it does not. NOTICE travels inside the image as the licence record and the source
#     offer for the GPL and LGPL members of the set, so a package added to the Dockerfile
#     and not to NOTICE ships without either, and a version bumped in one file only leaves
#     the record naming bytes the image does not contain. Both directions are compared.
# The pin block is the heredoc `COPY <<'DEBPINS' /hwruntime.pins` ... `DEBPINS`, one
# package per line: name, version, sha256, path under the snapshot's pool/.
deb_snapshot="$(arg DEBIAN_SNAPSHOT)"
deb_pins="$(awk '
  /^COPY <<.?DEBPINS.? / { inblk = 1; seen = 1; next }
  inblk && /^DEBPINS[[:space:]]*$/ { inblk = 0; closed = 1; next }
  inblk { print }
  END { if (!seen) print "@@NOBLOCK@@"; else if (!closed) print "@@UNCLOSED@@" }
' "$here/Dockerfile")"
deb_bad=0
declare -A df_debs=()
if printf '%s\n' "$deb_pins" | grep -qx '@@NOBLOCK@@'; then
  deb_bad=1
  bad "the Dockerfile has no Debian package pin block (COPY <<'DEBPINS' /hwruntime.pins ... DEBPINS), so nothing says which packages the image's hardware runtime copies or which bytes they are. A check that could not run has not passed."
elif printf '%s\n' "$deb_pins" | grep -qx '@@UNCLOSED@@'; then
  deb_bad=1
  bad "the Dockerfile's Debian package pin block is never closed by a DEBPINS line, so this check cannot tell where the pins end."
else
  deb_n=0
  while read -r name version sha path extra; do
    [ -n "$name" ] || continue
    deb_n=$((deb_n + 1))
    if [ -n "${extra:-}" ] || [ -z "$path" ]; then
      deb_bad=$((deb_bad + 1))
      bad "MALFORMED DEBIAN PIN in the Dockerfile: '$name $version $sha $path${extra:+ $extra}'
       Each line is exactly: <package> <exact version> <sha256 of the .deb> <path under pool/>"
      continue
    fi
    if [[ ! "$name" =~ ^[a-z0-9][a-z0-9.+-]+$ ]]; then
      deb_bad=$((deb_bad + 1)); bad "DEBIAN PIN with an impossible package name: '$name'"; continue
    fi
    if [[ ! "$version" =~ ^([0-9]+:)?[0-9][A-Za-z0-9.+~-]*$ ]]; then
      deb_bad=$((deb_bad + 1))
      bad "DEBIAN PIN $name has no exact version (got '$version'). A package is pinned by the version it was read at, never by a suite or a range."
    fi
    if [[ ! "$sha" =~ ^[0-9a-f]{64}$ ]]; then
      deb_bad=$((deb_bad + 1))
      bad "DEBIAN PIN $name $version carries no 64-character lowercase sha256 (got '$sha'). The build's sha256sum -c would be vacuous, and the image would copy whatever the URL served."
    fi
    upver="${version#*:}"
    if [[ ! "$path" =~ ^(main|contrib|non-free|non-free-firmware)/[a-z0-9]+/[a-z0-9][a-z0-9.+-]*/[^/]+$ ]] \
       || { [ "${path##*/}" != "${name}_${upver}_amd64.deb" ] && [ "${path##*/}" != "${name}_${upver}_all.deb" ]; }; then
      deb_bad=$((deb_bad + 1))
      bad "DEBIAN PIN $name $version names a pool path that is not that package at that version: '$path'
       Expected <component>/<prefix>/<source>/${name}_${upver}_amd64.deb (or _all.deb)."
    fi
    if [ -n "${df_debs[$name]:-}" ]; then
      deb_bad=$((deb_bad + 1)); bad "DEBIAN PIN $name appears twice in the Dockerfile's pin block."
    fi
    df_debs[$name]="$version"
  done <<<"$deb_pins"
  if [ "$deb_n" -eq 0 ]; then
    deb_bad=$((deb_bad + 1))
    bad "the Dockerfile's Debian package pin block is EMPTY. The image's hardware runtime would copy nothing, and this check would vouch for nothing."
  fi
fi

# The URL is part of the pin. The snapshot must be a real timestamp, the fetch must go
# through it, and no instruction in the Dockerfile may name the live mirror or a `dists/`
# path, where the bytes behind a URL change under a green build. Comments are exempt:
# they cite packages.debian.org pages, which are read, never fetched.
if [[ ! "$deb_snapshot" =~ ^[0-9]{8}T[0-9]{6}Z$ ]]; then
  deb_bad=$((deb_bad + 1))
  bad "FLOATING DEBIAN SNAPSHOT: ARG DEBIAN_SNAPSHOT is '$deb_snapshot', not a snapshot.debian.org timestamp (YYYYMMDDTHHMMSSZ). Without one the package URLs name no fixed archive state."
fi
if ! grep -qF 'https://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}/pool/${path}' "$here/Dockerfile"; then
  deb_bad=$((deb_bad + 1))
  bad "FLOATING DEBIAN URL: the hwruntime stage no longer fetches its packages from https://snapshot.debian.org/archive/debian/\${DEBIAN_SNAPSHOT}/pool/\${path}. A pool file on the live mirror disappears at the next point release; only a snapshot URL is permanent."
fi
deb_float="$(grep -nE 'https?://[^[:space:]"]*debian\.(org|net)' "$here/Dockerfile" \
  | grep -vE '^[0-9]+:[[:space:]]*#' \
  | grep -vF 'https://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT}/pool/${path}' || true)"
if [ -n "$deb_float" ]; then
  deb_bad=$((deb_bad + 1))
  bad "FLOATING DEBIAN URL in a Dockerfile instruction (only the snapshot pool URL is allowed):
$(printf '%s\n' "$deb_float" | sed 's/^/       /')"
fi

# NOTICE side: every `  deb: <package> <version>` line, each followed within its entry by
# a `source:` line pointing at snapshot.debian.org, where the corresponding source is kept.
declare -A no_debs=()
while read -r name version; do
  [ -n "$name" ] || continue
  if [ -n "${no_debs[$name]:-}" ]; then
    deb_bad=$((deb_bad + 1)); bad "NOTICE names Debian package $name twice."
  fi
  no_debs[$name]="$version"
done < <(sed -n 's/^  deb: \([^ ]*\) \([^ ]*\)[[:space:]]*$/\1 \2/p' "$here/NOTICE")
no_nosrc="$(awk '
  /^  deb: / { if (cur != "" && !src) print cur; cur = $2; src = 0; next }
  cur != "" && /^       source: .*https:\/\/snapshot\.debian\.org\/package\// { src = 1 }
  END { if (cur != "" && !src) print cur }
' "$here/NOTICE")"
if [ -n "$no_nosrc" ]; then
  deb_bad=$((deb_bad + 1))
  bad "NOTICE names Debian package(s) with no corresponding source on snapshot.debian.org (a 'source:' line with https://snapshot.debian.org/package/...): $(printf '%s' "$no_nosrc" | tr '\n' ' ')"
fi

for name in "${!df_debs[@]}"; do
  if [ -z "${no_debs[$name]:-}" ]; then
    deb_bad=$((deb_bad + 1))
    bad "Debian package $name ${df_debs[$name]} is copied into the image but MISSING FROM NOTICE. The image would ship it without its licence record or its source offer. Add a '  deb: $name ${df_debs[$name]}' entry with its licence and its snapshot.debian.org source."
  elif [ "${no_debs[$name]}" != "${df_debs[$name]}" ]; then
    deb_bad=$((deb_bad + 1))
    bad "Debian package $name: VERSION DRIFT between the Dockerfile and NOTICE - the licence record names bytes the image does not contain.
       Dockerfile: ${df_debs[$name]}
       NOTICE:     ${no_debs[$name]}"
  fi
done
for name in "${!no_debs[@]}"; do
  if [ -z "${df_debs[$name]:-}" ]; then
    deb_bad=$((deb_bad + 1))
    bad "NOTICE names Debian package $name ${no_debs[$name]}, which the Dockerfile does NOT copy into the image. A licence record for something the image does not carry hides which record is real; remove it, or pin the package."
  fi
done

if [ "$deb_bad" -eq 0 ]; then
  note "ok: ${#df_debs[@]} Debian package(s) pinned by version and sha256 at snapshot $deb_snapshot, and NOTICE names exactly those, at those versions"
fi

# --- 11. The dynamic-HDR tools: pinned, fetched from their release, and in NOTICE ----
# The image bundles dovi_tool and hdr10plus_tool (the Dockerfile's dynhdr stage, goal 8),
# and scripts/install-dynhdr-tools.sh installs the same builds for CI by PARSING the
# Dockerfile's ARGs. What is checked here, offline:
#   - each ARG exists: <TOOL>_VERSION, <TOOL>_SHA256_AMD64, <TOOL>_SHA256_ARM64;
#   - the version is an exact release tag (MAJOR.MINOR.PATCH), never a floating alias
#     such as `latest`, which would change the RPU and HDR10+ instruments under a green
#     build;
#   - each digest is a full 64-character lowercase sha256, or the build's sha256sum -c
#     and the installer's digest gate would be vacuous;
#   - the fetch goes to the upstream GitHub release asset built from those ARGs, and no
#     other instruction fetches anything from that owner;
#   - NOTICE names each tool at exactly the pinned version, with licence MIT, its source
#     repository and release tag, and the copyright line and permission notice the MIT
#     licence requires to travel with the binary - and names no tool, and no version of
#     either tool, that the Dockerfile does not pin. Both directions are compared.
# Upstream: https://github.com/quietvoid/dovi_tool and https://github.com/quietvoid/hdr10plus_tool,
# each MIT (gh api repos/quietvoid/<repo>/license, read 2026-10-01).
dyn_bad=0
declare -A df_tools=()
# The Dockerfile's instructions with comment lines removed, captured once: a comment that
# quotes the URL must not satisfy the check, and a `grep | grep -q` pipeline can SIGPIPE
# its writer and fail under pipefail on a match.
df_instr="$(grep -vE '^[[:space:]]*#' "$here/Dockerfile" || true)"
for tool in dovi_tool hdr10plus_tool; do
  prefix="$(printf '%s' "$tool" | tr '[:lower:]' '[:upper:]')"
  for a in VERSION SHA256_AMD64 SHA256_ARM64; do
    if ! grep -qE "^ARG ${prefix}_${a}=" "$here/Dockerfile"; then
      dyn_bad=$((dyn_bad + 1))
      bad "the Dockerfile has no 'ARG ${prefix}_${a}=' - the ${tool} pin is incomplete, and scripts/install-dynhdr-tools.sh reads it from there."
    fi
  done
  v="$(arg "${prefix}_VERSION")"
  if [[ ! "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    dyn_bad=$((dyn_bad + 1))
    bad "FLOATING ${tool} PIN: ${prefix}_VERSION is '$v', not an exact release tag (MAJOR.MINOR.PATCH). A floating alias such as 'latest' never 404s; it changes the tool that decides whether dynamic HDR metadata survived, under a green build."
  fi
  for a in AMD64 ARM64; do
    d="$(arg "${prefix}_SHA256_$a")"
    if [[ ! "$d" =~ ^[0-9a-f]{64}$ ]]; then
      dyn_bad=$((dyn_bad + 1))
      bad "${prefix}_SHA256_$a is not a 64-character lowercase sha256 digest (got '$d'). The build's sha256sum -c and the installer's digest gate would be vacuous."
    fi
  done
  want_url="https://github.com/quietvoid/${tool}/releases/download/\${${prefix}_VERSION}/${tool}-\${${prefix}_VERSION}-\${triple}.tar.gz"
  if ! grep -qF "\"${want_url}\"" <<<"$df_instr"; then
    dyn_bad=$((dyn_bad + 1))
    bad "FLOATING ${tool} URL: the Dockerfile's dynhdr stage no longer fetches
       ${want_url}
       which is the upstream release asset built from the pinned ARGs. A URL not built from
       ${prefix}_VERSION fetches something the pin does not name."
  fi
  df_tools[$tool]="$v"
done

# No other instruction may fetch from upstream's owner: a second URL beside the checked one
# is a second, unchecked pin. Comments are exempt; they cite release pages, never fetch.
dyn_float="$(grep -nE 'https?://[^[:space:]"]*github\.com/quietvoid/' "$here/Dockerfile" \
  | grep -vE '^[0-9]+:[[:space:]]*#' \
  | grep -vF 'https://github.com/quietvoid/dovi_tool/releases/download/${DOVI_TOOL_VERSION}/dovi_tool-${DOVI_TOOL_VERSION}-${triple}.tar.gz' \
  | grep -vF 'https://github.com/quietvoid/hdr10plus_tool/releases/download/${HDR10PLUS_TOOL_VERSION}/hdr10plus_tool-${HDR10PLUS_TOOL_VERSION}-${triple}.tar.gz' || true)"
if [ -n "$dyn_float" ]; then
  dyn_bad=$((dyn_bad + 1))
  bad "UNCHECKED dynamic-HDR tool URL in a Dockerfile instruction (only the two pinned release-asset URLs are allowed):
$(printf '%s\n' "$dyn_float" | sed 's/^/       /')"
fi

# NOTICE side: every `  tool: <name> <version>` line opens an entry that runs to the next
# `  tool:` line or the next rule of dashes, and each entry must carry its licence, its
# source and the MIT notice.
declare -A no_tools=()
while read -r name version; do
  [ -n "$name" ] || continue
  if [ -n "${no_tools[$name]:-}" ]; then
    dyn_bad=$((dyn_bad + 1)); bad "NOTICE names the tool $name twice."
  fi
  no_tools[$name]="$version"
done < <(sed -n 's/^  tool: \([^ ]*\) \([^ ]*\)[[:space:]]*$/\1 \2/p' "$here/NOTICE")

for tool in "${!df_tools[@]}"; do
  v="${df_tools[$tool]}"
  if [ -z "${no_tools[$tool]:-}" ]; then
    dyn_bad=$((dyn_bad + 1))
    bad "$tool $v is bundled into the image but MISSING FROM NOTICE. The image would redistribute an MIT binary without the copyright and permission notice its licence requires. Add a '  tool: $tool $v' entry."
    continue
  fi
  if [ "${no_tools[$tool]}" != "$v" ]; then
    dyn_bad=$((dyn_bad + 1))
    bad "$tool: VERSION DRIFT between the Dockerfile and NOTICE - the licence record names a build the image does not contain.
       Dockerfile: $v
       NOTICE:     ${no_tools[$tool]}"
  fi
  entry="$(awk -v t="$tool" '
    /^  tool: / { inent = ($2 == t); if (inent) print; next }
    /^-{10,}/   { inent = 0 }
    inent       { print }
  ' "$here/NOTICE")"
  missing=()
  grep -qE '^[[:space:]]+licence:[[:space:]]+MIT[[:space:]]*$' <<<"$entry" || missing+=("a 'licence: MIT' line")
  grep -qF "https://github.com/quietvoid/${tool}/releases/tag/${v}" <<<"$entry" \
    || missing+=("its source, https://github.com/quietvoid/${tool}/releases/tag/${v}")
  grep -qE 'Copyright \(c\) [0-9]{4} ' <<<"$entry" || missing+=("the upstream copyright line")
  grep -qF 'Permission is hereby granted, free of charge' <<<"$entry" || missing+=("the MIT permission notice")
  if [ "${#missing[@]}" -ne 0 ]; then
    dyn_bad=$((dyn_bad + 1))
    bad "NOTICE's $tool entry is incomplete: it lacks $(printf '%s; ' "${missing[@]}")the MIT licence requires the copyright and permission notice to travel with the binary, and the record must say where the source is."
  fi
done
for tool in "${!no_tools[@]}"; do
  if [ -z "${df_tools[$tool]:-}" ]; then
    dyn_bad=$((dyn_bad + 1))
    bad "NOTICE names the tool $tool ${no_tools[$tool]}, which the Dockerfile does NOT bundle. A licence record for something the image does not carry hides which record is real."
  fi
done

# Any other version of either tool written anywhere in NOTICE is a stray: it would drift
# silently, the way the ffmpeg revision did before section 1 refused it.
dyn_strays="$(grep -oE '(dovi_tool|hdr10plus_tool)([ -]|/releases/(tag|download)/)v?[0-9]+\.[0-9]+(\.[0-9]+)?' "$here/NOTICE" \
  | while read -r hit; do
      t="$(printf '%s' "$hit" | grep -oE '^(dovi_tool|hdr10plus_tool)')"
      hv="$(printf '%s' "$hit" | grep -oE 'v?[0-9]+\.[0-9]+(\.[0-9]+)?$')"
      [ "$hv" = "${df_tools[$t]:-}" ] || printf '%s\n' "$hit"
    done | sort -u || true)"
if [ -n "$dyn_strays" ]; then
  dyn_bad=$((dyn_bad + 1))
  bad "NOTICE names a dynamic-HDR tool at a version the Dockerfile does NOT pin:
       stray: $(printf '%s' "$dyn_strays" | tr '\n' ' ')
       pinned: dovi_tool ${df_tools[dovi_tool]:-?}, hdr10plus_tool ${df_tools[hdr10plus_tool]:-?}"
fi

if [ "$dyn_bad" -eq 0 ]; then
  note "ok: dovi_tool ${df_tools[dovi_tool]} and hdr10plus_tool ${df_tools[hdr10plus_tool]} are pinned by exact version and per-arch sha256, fetched from their release, and NOTICE names exactly those, MIT, with source"
fi

# --- 12. The web UI's toolchain: Node, pnpm and every package, each pinned once ------
# `make check` lints, typechecks, tests and builds the web UI (web/), and the image builds
# it again in its own stage. Both must run the SAME toolchain, or the UI the gate proved is
# not the UI the image ships. Each pin has one home, and what is checked here is that the
# home holds an exact version and that the one unavoidable restatement agrees with it:
#
#   Node   web/.node-version, an exact MAJOR.MINOR.PATCH. scripts/ui.sh reads it, and the
#          workflows hand that file to setup-node (`node-version-file`), so neither restates
#          it. The Dockerfile's `ui` stage MUST restate it - a FROM line is a literal, and
#          section 7 requires the reference written there - so that tag is compared here.
#          A literal `node-version:` in a workflow is refused: it is a second home.
#   pnpm   web/package.json `packageManager`, as pnpm@<exact>+sha512.<128 hex>: corepack
#          downloads that version's package and refuses bytes that do not hash to it. For
#          pnpm 12 that package is a launcher: it fetches the native pnpm of the same
#          version and checks it against npm's registry signatures, not against this hash
#          (read in the package's bin/pnpm.mjs, 2026-10-03). The version is what is pinned
#          end to end; the hash pins the launcher.
#   every package   an exact version in web/package.json - no range, no tag - and a
#          committed web/pnpm-lock.yaml that resolves the rest. Svelte and Vite are named,
#          because the gate's claim is about them.
ui_bad=0
ui_node="$(tr -d '[:space:]' < "$here/web/.node-version")"
if ! printf '%s' "$ui_node" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  ui_bad=$((ui_bad + 1))
  bad "FLOATING NODE PIN - web/.node-version holds '$ui_node', which is not an exact MAJOR.MINOR.PATCH version.
       A major alone, an alias like \`lts/*\` or a range is whatever Node is newest on the
       day of the build."
fi

# The ui stage's base: the FROM line of the stage NAMED `ui`, which must be the official
# node image, and the build stage must take the UI from that stage and no other.
ui_from="$(grep -nE '^[[:space:]]*[Ff][Rr][Oo][Mm][[:space:]].*[[:space:]][Aa][Ss][[:space:]]+ui[[:space:]]*$' "$here/Dockerfile" || true)"
ui_from_count="$(printf '%s' "$ui_from" | grep -c . || true)"
ui_from_tag=""
if [ "$ui_from_count" -ne 1 ]; then
  ui_bad=$((ui_bad + 1))
  bad "the Dockerfile has $ui_from_count stage(s) named \`ui\`, and the web UI is built in exactly one: this check could not find the stage whose Node version it compares. A parser that stopped understanding the Dockerfile is a refusal, not a green build."
else
  ui_from_img="$(printf '%s' "$ui_from" | sed 's/^[0-9]*:[[:space:]]*[Ff][Rr][Oo][Mm][[:space:]]\{1,\}//' \
    | awk '{ for (i = 1; i <= NF; i++) if ($i !~ /^--/) { print $i; exit } }')"
  case "$ui_from_img" in
    node:*|library/node:*|docker.io/library/node:*)
      ui_from_tag="${ui_from_img#*node:}"; ui_from_tag="${ui_from_tag%%@*}"
      ui_from_ver="${ui_from_tag%%-*}"
      if [ "$ui_from_ver" != "$ui_node" ]; then
        ui_bad=$((ui_bad + 1))
        bad "Node version drift - the image builds the web UI on node:$ui_from_tag (Dockerfile line ${ui_from%%:*}) and web/.node-version pins $ui_node.
       \`make check\` proves the UI on the version in web/.node-version; the image would
       ship one built on another. Move both together (and the FROM line's digest with its
       tag)."
      fi
      ;;
    *)
      ui_bad=$((ui_bad + 1))
      bad "NOT THE NODE IMAGE - the Dockerfile's \`ui\` stage is built FROM '$ui_from_img' (line ${ui_from%%:*}), not the official \`node:<version>\` image: the Node the gate proved the UI on is not what would build it."
      ;;
  esac
  ui_copies="$(grep -cE '^[[:space:]]*COPY[[:space:]]+--from=ui[[:space:]]+/src/web/dist/?[[:space:]]+\./internal/ui/dist/?[[:space:]]*$' "$here/Dockerfile" || true)"
  ui_other="$(grep -E '^[[:space:]]*COPY[[:space:]].*internal/ui/dist' "$here/Dockerfile" | grep -cvE -- '--from=ui[[:space:]]+/src/web/dist/?[[:space:]]' || true)"
  if [ "$ui_copies" -ne 1 ] || [ "$ui_other" -ne 0 ]; then
    ui_bad=$((ui_bad + 1))
    bad "the Dockerfile does not embed the web UI from its \`ui\` stage alone: expected exactly one \`COPY --from=ui /src/web/dist/ ./internal/ui/dist/\` and no other COPY into internal/ui/dist (found $ui_copies and $ui_other)."
  fi
fi

# The install line. pnpm-workspace.yaml (section 8) is where the decisions live, and a
# command-line flag or a pnpm_config_* variable overrides that file: `--ignore-scripts=false`,
# `--no-ignore-scripts`, `--config.ignoreScripts=false` and `pnpm_config_ignore_scripts=false`
# each ran a root postinstall script past a workspace file saying `ignoreScripts: true`
# (measured with pnpm 12.8.1, 2026-10-03). So each place that installs is held to the one
# install command, with no other flag, and no pnpm_config_* variable is set anywhere a
# build reads.
for inst in Dockerfile scripts/ui.sh; do
  inst_lines="$(grep -nE 'pnpm[[:space:]]+(install|i|add|update|up|import|rebuild|dlx|exec[[:space:]]+pnpm)([[:space:]]|$)' "$here/$inst" | grep -vE '^[0-9]+:[[:space:]]*#' || true)"
  inst_ok="$(printf '%s\n' "$inst_lines" | grep -cE '^[0-9]+:[[:space:]]*(RUN[[:space:]]+)?pnpm install --frozen-lockfile[[:space:]]*$' || true)"
  inst_all="$(printf '%s' "$inst_lines" | grep -c . || true)"
  if [ "$inst_ok" -ne 1 ] || [ "$inst_all" -ne 1 ]; then
    ui_bad=$((ui_bad + 1))
    bad "UNHELD INSTALL - $inst must install the web UI's packages with exactly one \`pnpm install --frozen-lockfile\`, alone on its line, and no other installing command. Found:
$(printf '%s\n' "$inst_lines" | sed 's/^/         /')
       A flag on that line overrides web/pnpm-workspace.yaml (--ignore-scripts=false runs
       lifecycle scripts), and without --frozen-lockfile the install rewrites the lockfile
       instead of failing on it."
  fi
done
ui_cfg_env="$(grep -nEi 'pnpm_config_' "$here/Dockerfile" "$here/scripts/ui.sh" "$here/Makefile" "${wf_files[@]}" | grep -vE '^[^:]+:[0-9]+:[[:space:]]*#' || true)"
if [ -n "$ui_cfg_env" ]; then
  ui_bad=$((ui_bad + 1))
  bad "PNPM SETTING OUTSIDE ITS FILE - a pnpm_config_* variable is set where a build reads it, and it overrides web/pnpm-workspace.yaml:
$(printf '%s\n' "$ui_cfg_env" | sed "s|^$here/||; s/^/         /")
       Every pnpm decision lives in web/pnpm-workspace.yaml, where section 8 reads it."
fi

for wf in "${wf_files[@]}"; do
  wfrel="${wf#"$here"/}"
  if grep -nE '^[[:space:]]*node-version:' "$wf" >/dev/null; then
    ui_bad=$((ui_bad + 1))
    bad "SECOND HOME FOR THE NODE PIN - $wfrel writes a literal \`node-version:\` (line $(grep -nE '^[[:space:]]*node-version:' "$wf" | head -1 | cut -d: -f1)).
       The pin lives in web/.node-version; a workflow reads it with
         node-version-file: web/.node-version
       so the two cannot drift."
  fi
done
for wfname in ci.yml release.yml; do
  if ! grep -qE '^[[:space:]]*node-version-file:[[:space:]]*web/\.node-version[[:space:]]*(#.*)?$' "$here/.github/workflows/$wfname"; then
    ui_bad=$((ui_bad + 1))
    bad ".github/workflows/$wfname runs \`make check\`, which runs the web UI's gate, and does not set up Node from the pin (\`node-version-file: web/.node-version\`): its gate would run on whatever Node the runner image carries, or not at all."
  fi
done

ui_pm="$(sed -n 's/^[[:space:]]*"packageManager":[[:space:]]*"\([^"]*\)",\{0,1\}[[:space:]]*$/\1/p' "$here/web/package.json" | head -1)"
if ! printf '%s' "$ui_pm" | grep -qE '^pnpm@[0-9]+\.[0-9]+\.[0-9]+\+sha512\.[0-9a-f]{128}$'; then
  ui_bad=$((ui_bad + 1))
  bad "UNPINNED PACKAGE MANAGER - web/package.json \`packageManager\` is '${ui_pm:-absent}', not pnpm@<MAJOR.MINOR.PATCH>+sha512.<128 lowercase hex>.
       The version says which pnpm; the hash is what makes corepack refuse any other
       package under that version."
fi

# Every dependency, in either block, one `"name": "version"` per line as pnpm writes it.
ui_deps="$(awk '
  /^[[:space:]]*"(dependencies|devDependencies|optionalDependencies|peerDependencies)"[[:space:]]*:[[:space:]]*\{/ { inblock = 1; next }
  inblock && /^[[:space:]]*\}/ { inblock = 0; next }
  inblock {
    line = $0; gsub(/[[:space:]",]/, "", line)
    n = index(line, ":"); if (n == 0) next
    # a scoped name carries no colon, so the first colon ends the name
    print substr(line, 1, n - 1) "|" substr(line, n + 1)
  }' "$here/web/package.json")"
ui_dep_count="$(printf '%s' "$ui_deps" | grep -c . || true)"
if [ "$ui_dep_count" -eq 0 ]; then
  ui_bad=$((ui_bad + 1))
  bad "web/package.json declares no dependency this check could read - it just asserted nothing about any version. A parser that stopped understanding the manifest is a refusal, not a green build."
else
  ui_float="$(printf '%s\n' "$ui_deps" | awk -F'|' '$2 !~ /^[0-9]+\.[0-9]+\.[0-9]+$/ { printf "%s (%s) ", $1, $2 }')"
  if [ -n "$ui_float" ]; then
    ui_bad=$((ui_bad + 1))
    bad "NOT AN EXACT VERSION - web/package.json pins these by a range, a tag or a pre-release: $ui_float
       Every package there is MAJOR.MINOR.PATCH and nothing else. A \`^\` or \`~\` range is a
       different toolchain on the day the lockfile is next refreshed."
  fi
  for must in svelte vite; do
    printf '%s\n' "$ui_deps" | grep -q "^$must|" || {
      ui_bad=$((ui_bad + 1))
      bad "web/package.json does not name \`$must\`: the gate's claim is that the UI is built with a pinned $must, and a package that is only transitive is pinned by nobody."
    }
  done
fi

# The Svelte runtime is the one package whose code ships inside the binary's embedded
# bundle, so NOTICE carries its MIT notice and names its version. That is a restatement of
# web/package.json, in the licence record that travels with the image and the release
# tarballs, so it is compared: a bump made in the manifest alone would leave the record
# naming a version the binary does not contain.
ui_svelte="$(printf '%s\n' "$ui_deps" | sed -n 's/^svelte|//p' | head -1)"
ui_notice_svelte="$(sed -n 's/^  package: svelte \([^[:space:]]*\)[[:space:]]*$/\1/p' "$here/NOTICE")"
if [ -z "$ui_notice_svelte" ]; then
  ui_bad=$((ui_bad + 1))
  bad "svelte MISSING FROM NOTICE - the binary embeds the Svelte runtime in its web UI bundle and NOTICE has no \`  package: svelte <version>\` entry: MIT requires its copyright and permission notice to travel with every copy."
elif [ "$ui_notice_svelte" != "$ui_svelte" ]; then
  ui_bad=$((ui_bad + 1))
  bad "svelte: VERSION DRIFT - web/package.json pins ${ui_svelte:-nothing} and NOTICE names $(printf '%s' "$ui_notice_svelte" | tr '\n' ' '). Move both together."
fi
if [ -n "$ui_notice_svelte" ]; then
  ui_entry="$(awk '
    /^  package: svelte / { inent = 1; print; next }
    /^-{10,}/             { inent = 0 }
    inent                 { print }
  ' "$here/NOTICE")"
  ui_missing=()
  grep -qE '^[[:space:]]+licence:[[:space:]]+MIT[[:space:]]*$' <<<"$ui_entry" || ui_missing+=("a 'licence: MIT' line")
  grep -qF 'https://github.com/sveltejs/svelte' <<<"$ui_entry" || ui_missing+=("its source, https://github.com/sveltejs/svelte")
  grep -qE 'Copyright \(c\) [0-9]{4}' <<<"$ui_entry" || ui_missing+=("the upstream copyright line")
  grep -qF 'Permission is hereby granted, free of charge' <<<"$ui_entry" || ui_missing+=("the MIT permission notice")
  if [ "${#ui_missing[@]}" -ne 0 ]; then
    ui_bad=$((ui_bad + 1))
    bad "NOTICE's svelte entry is incomplete: it lacks $(printf '%s; ' "${ui_missing[@]}")the MIT licence requires the copyright and permission notice to travel with every copy of the bundle the binary embeds."
  fi
fi

if ! git -C "$here" ls-files --error-unmatch -- web/pnpm-lock.yaml >/dev/null 2>&1; then
  ui_bad=$((ui_bad + 1))
  bad "web/pnpm-lock.yaml is not committed: without it \`pnpm install --frozen-lockfile\` has nothing to hold the transitive dependencies to, and each install resolves them afresh."
fi
# Every package the lockfile resolves comes from the registry with its sha512: a
# `tarball:` or git resolution, or one with no integrity, is bytes nothing pins.
ui_res_all="$(grep -cE '^[[:space:]]+resolution:' "$here/web/pnpm-lock.yaml" || true)"
ui_res_ok="$(grep -cE '^[[:space:]]+resolution: \{integrity: sha512-[A-Za-z0-9+/=]+\}[[:space:]]*$' "$here/web/pnpm-lock.yaml" || true)"
if [ "$ui_res_all" -eq 0 ] || [ "$ui_res_all" -ne "$ui_res_ok" ]; then
  ui_bad=$((ui_bad + 1))
  bad "UNPINNED LOCKFILE ENTRY - web/pnpm-lock.yaml has $ui_res_all resolution(s) and $ui_res_ok of them are a registry package with a sha512 and nothing else:
$(grep -nE '^[[:space:]]+resolution:' "$here/web/pnpm-lock.yaml" | grep -vE 'resolution: \{integrity: sha512-[A-Za-z0-9+/=]+\}[[:space:]]*$' | head -5 | sed 's/^/         /')
       A tarball URL, a git reference or a missing integrity is a dependency no hash holds."
fi
if grep -qE '^[[:space:]]*"(overrides|pnpm|resolutions)"[[:space:]]*:' "$here/web/package.json"; then
  ui_bad=$((ui_bad + 1))
  bad "web/package.json carries an \`overrides\`, \`resolutions\` or \`pnpm\` block: a version written there is read by none of the exact-version checks above. Pin the package as a dependency instead."
fi
if ! grep -qE "^lockfileVersion:" "$here/web/pnpm-lock.yaml"; then
  ui_bad=$((ui_bad + 1))
  bad "web/pnpm-lock.yaml carries no \`lockfileVersion:\` line: it is not a pnpm lockfile this check recognises."
fi

if [ "$ui_bad" -eq 0 ]; then
  note "ok: the web UI is built on Node $ui_node (web/.node-version, and the Dockerfile's node:$ui_from_tag stage) with ${ui_pm%%+*} pinned by sha512, all $ui_dep_count packages in web/package.json are exact versions under a committed lockfile, and NOTICE names svelte $ui_svelte"
fi

[ "$fail" -eq 0 ] || exit 1
echo "pins agree"
