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

need_file "Dockerfile"                     "read the ffmpeg pin, the Go toolchain pin and every base-image ARG"
need_file "NOTICE"                         "confirm the GPL source offer names the ffmpeg the image bundles"
need_file "docker-compose.yml"             "confirm the example deployment pins the image it pulls"
need_dir  ".github/workflows"              "confirm every action is pinned to a commit SHA"
need_file ".github/workflows/ci.yml"       "confirm the gate runs on the Go the shipped binary is built with"
need_file ".github/workflows/release.yml"  "confirm the release runs on the Go the shipped binary is built with"

if [ "$fail" -ne 0 ]; then
  printf '::error::%s\n' "check-pins: a file this gate depends on could not be read (named above). Refusing to report green." >&2
  exit 1
fi

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
# The directory is ENUMERATED, never listed by hand: a workflow added later must be
# covered by the pin gate on the day it lands, not on the day somebody remembers to add
# it here. `runs-on: ubuntu-latest` is deliberately NOT matched - that is a runner label,
# not an image reference.
wf_files=()
while IFS= read -r f; do
  [ -n "$f" ] && wf_files+=("$f")
done < <(find "$here/.github/workflows" -maxdepth 1 -type f \( -name '*.yml' -o -name '*.yaml' \) 2>/dev/null | sort)

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

# --- 7. Every base image is pinned by tag AND digest (P2) --------------------------
# A `FROM` line is an image reference like any other, and section 3 above guards exactly
# one of them: GO_IMAGE. FETCH_IMAGE and RUNTIME_IMAGE could lose their digests silently,
# and RUNTIME_IMAGE is the worst of the three to lose - it is the base the shipped image
# IS, and the in-image toolchain assertion that backstops GO_IMAGE cannot see it at all,
# because nothing in the runtime stage runs.
#
# The FROM lines here reference ARGs, not literals, so a check that reads `FROM` lines
# alone finds no digest anywhere and is wrong in BOTH directions: it would red on a
# correctly pinned tree and pass a tree whose ARG default had been gutted. Resolve the ARG.
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

  argname=""
  case "$imgtok" in
    '${'*'}') argname="${imgtok#\$\{}"; argname="${argname%\}}" ;;
    '$'*)     argname="${imgtok#\$}" ;;
  esac

  if [ -n "$argname" ]; then
    resolved="$(arg "$argname")"
    label="ARG $argname (Dockerfile line $lineno)"
    if [ -z "$resolved" ]; then
      from_bad=$((from_bad + 1))
      bad "UNRESOLVABLE BASE IMAGE: Dockerfile line $lineno builds FROM \$$argname, but ARG $argname has no default in this Dockerfile. The base image would be whatever the caller passed, or nothing - neither is a pin."
      continue
    fi
  else
    # A reference to an earlier build stage is not an image reference.
    case "$prev_stages" in *" $imgtok "*) continue ;; esac
    resolved="$imgtok"
    label="the literal base image on Dockerfile line $lineno"
    argname="(literal)"
  fi

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
       $argname names a TAG, and a tag is moved by its publisher, so this base floats to
       whatever the registry serves on the day of the build. Pin both:
         ARG $argname=${namepart}@sha256:<64 hex>
       Resolve one with: docker buildx imagetools inspect $namepart"
  elif [ -z "$tag" ]; then
    from_bad=$((from_bad + 1))
    bad "UNREADABLE BASE IMAGE PIN - $label has a digest but no tag: '$resolved'
       The digest is what resolves; the tag is what tells a human which base this is. P2
       requires both:
         ARG $argname=<image>:<tag>@$digest"
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
  note "ok: all $from_seen base image(s) the Dockerfile's FROM lines resolve are pinned by tag AND digest"
fi

# --- 8. A node manifest must carry a lifecycle-script decision (P4) ----------------
# There is no node manifest in this repository today and the dashboard is built by the Go
# toolchain alone, so this check is a TRIPWIRE rather than a current assertion: the moment
# somebody adds a package.json, `npm install` gains the right to execute arbitrary
# `postinstall` code from every transitive dependency, on a runner holding this
# repository's credentials.
#
# It scans the WORKING TREE, not the tracked files, on purpose. The moment to refuse a
# manifest is the moment it is about to be committed - a tracked-files-only check would
# stay green through the entire pull request that introduces it and only bite afterwards,
# which is exactly one merge too late.
#
# The decision surface is a COMMITTED .npmrc, because a decision that is not in the
# repository is not a decision anybody after you can see:
#   ignore-scripts=true    - scripts off. Nothing further needed.
#   ignore-scripts=false   - scripts on, and only legal beside a line carrying
#                            `lifecycle-scripts-reason: <why>` in the same file.
node_manifests=()
while IFS= read -r m; do
  [ -n "$m" ] && node_manifests+=("$m")
done < <(find "$here" \( -name .git -o -name node_modules -o -name vendor \) -prune -o -type f -name package.json -print 2>/dev/null | sort)

if [ "${#node_manifests[@]}" -eq 0 ]; then
  note "ok: no node package manifest in the working tree - nothing can run a lifecycle script"
else
  for man in "${node_manifests[@]}"; do
    mrel="${man#"$here"/}"
    mdir="$(dirname "$man")"
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
         echo 'ignore-scripts=true' > $(dirname "$mrel" | sed 's|^\.$||; s|$|/|; s|^/$||').npmrc
       or, to enable them deliberately, in that same committed .npmrc:
         ignore-scripts=false
         # lifecycle-scripts-reason: <why this repository needs them>
       Then commit it: a decision that is not in the repository is not a decision."
    fi
  done
fi

[ "$fail" -eq 0 ] || exit 1
echo "pins agree"
