#!/usr/bin/env bash
# client-report.sh - the owner's live check of their own Plex, Sonarr or Radarr: it sends the
# requests holdfast's shipped clients send, and writes a redacted report for the owner to
# commit (brief T49). No goal, test or gate ever runs it against a real service.
#
#   scripts/client-report.sh --service plex   --config /srv/holdfast/config.yaml --image holdfast:live \
#       --docker-arg=-v --docker-arg=/srv/holdfast/secrets:/run/secrets:ro
#   scripts/client-report.sh --service sonarr --config ./config.yaml             # host mode: go run
#   scripts/client-report.sh --service radarr --config ./config.yaml --bin ./holdfast-client-report
#   scripts/client-report.sh --verify testdata/client-reports/plex-2026-10-02.json
#
# What it does: runs scripts/clientreport (the Go half) with the address, the credential
# reference and the path map read from the holdfast configuration exactly as holdfast reads
# them, and writes testdata/client-reports/<service>-<date>.json (UTC date), or --out. The
# credential is never an argument and never in the environment: it is a `file:` or `cmd:`
# reference in the configuration, resolved inside the process that sends the requests.
#
# What a report carries: versions, HTTP status codes, counts, booleans and failure classes,
# and nothing else - no address, host name, port, URL, credential, machine identifier, title,
# path, user or device name. The Go half builds it from one closed struct and refuses to
# write it if the resolved credential or the configured address is anywhere in its bytes.
#
# The checks are read-only unless ONE write check is asked for by name: --refresh-dir (plex:
# one partial refresh of that one directory) or --rescan-dir (sonarr, radarr: one
# RescanSeries or RescanMovie, by id, for the owner of that directory).
#
# What it refuses: a missing tool (named), an existing report (never overwritten), and
# everything the Go half refuses. Every refusal writes no report and exits non-zero.
# docs/client-reports.md is the reference.
set -euo pipefail

# The Go half's path inside the image (the Dockerfile's runtime stage copies it there).
IMAGE_BIN=/usr/local/bin/holdfast-client-report

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

usage() {
  cat <<'EOF'
usage: client-report.sh --service plex|sonarr|radarr --config CONFIG [--out PATH]
                        [--image REF [--docker-arg ARG]... | --bin PATH]
                        [--refresh-dir DIR | --rescan-dir DIR]
       client-report.sh --verify REPORT [--image REF | --bin PATH]

  --service KIND    the service to check: plex, sonarr or radarr (required)
  --config CONFIG   the holdfast configuration naming the service: its `<service>_url`, its
                    credential by reference and its path map are read exactly as holdfast
                    reads them (required)
  --out PATH        where to write the report (default
                    testdata/client-reports/<service>-<date>.json); an existing file is
                    never overwritten
  --image REF       image mode: run the check inside this holdfast image, as your user; no
                    Go toolchain is needed. The configuration is mounted read-only; mount
                    what its `file:` references name with --docker-arg
  --docker-arg ARG  image mode: one extra `docker run` argument (repeatable), e.g.
                    --docker-arg=-v --docker-arg=/srv/holdfast/secrets:/run/secrets:ro,
                    --docker-arg=--network=media
  --bin PATH        host mode with a built binary (go build -o PATH ./scripts/clientreport)
                    instead of `go run`
  --refresh-dir DIR plex only, a WRITE: one partial refresh of that one directory
  --rescan-dir DIR  sonarr or radarr only, a WRITE: one rescan command, by id, for the
                    series or movie that owns that directory
  --verify REPORT   check that an existing report carries only what a report may carry
EOF
}

die() { printf 'client-report: %s\n' "$*" >&2; exit 1; }
say() { printf 'client-report: %s\n' "$*" >&2; }

SERVICE=""; CONFIG=""; OUT=""; IMAGE=""; BIN=""; VERIFY=""; REFRESH_DIR=""; RESCAN_DIR=""
DOCKER_ARGS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --service)        [ $# -ge 2 ] || { usage >&2; exit 2; }; SERVICE="$2"; shift 2 ;;
    --service=*)      SERVICE="${1#*=}"; shift ;;
    --config)         [ $# -ge 2 ] || { usage >&2; exit 2; }; CONFIG="$2"; shift 2 ;;
    --config=*)       CONFIG="${1#*=}"; shift ;;
    --out)            [ $# -ge 2 ] || { usage >&2; exit 2; }; OUT="$2"; shift 2 ;;
    --out=*)          OUT="${1#*=}"; shift ;;
    --image)          [ $# -ge 2 ] || { usage >&2; exit 2; }; IMAGE="$2"; shift 2 ;;
    --image=*)        IMAGE="${1#*=}"; shift ;;
    --docker-arg)     [ $# -ge 2 ] || { usage >&2; exit 2; }; DOCKER_ARGS+=("$2"); shift 2 ;;
    --docker-arg=*)   DOCKER_ARGS+=("${1#*=}"); shift ;;
    --bin)            [ $# -ge 2 ] || { usage >&2; exit 2; }; BIN="$2"; shift 2 ;;
    --bin=*)          BIN="${1#*=}"; shift ;;
    --refresh-dir)    [ $# -ge 2 ] || { usage >&2; exit 2; }; REFRESH_DIR="$2"; shift 2 ;;
    --refresh-dir=*)  REFRESH_DIR="${1#*=}"; shift ;;
    --rescan-dir)     [ $# -ge 2 ] || { usage >&2; exit 2; }; RESCAN_DIR="$2"; shift 2 ;;
    --rescan-dir=*)   RESCAN_DIR="${1#*=}"; shift ;;
    --verify)         [ $# -ge 2 ] || { usage >&2; exit 2; }; VERIFY="$2"; shift 2 ;;
    --verify=*)       VERIFY="${1#*=}"; shift ;;
    -h|--help)        usage; exit 0 ;;
    *)                printf 'client-report: unknown argument %q\n' "$1" >&2; usage >&2; exit 2 ;;
  esac
done

need() { command -v "$1" >/dev/null 2>&1 || die "required tool '$1' is not on PATH ($2)"; }

if [ -n "$IMAGE" ] && [ -n "$BIN" ]; then die "--image and --bin are two different modes; name one"; fi
if [ -z "$IMAGE" ] && [ "${#DOCKER_ARGS[@]}" -gt 0 ]; then die "--docker-arg needs --image"; fi

# abs prints an absolute path for a file whose directory exists.
abs() {
  local d
  d="$(cd "$(dirname -- "$1")" 2>/dev/null && pwd)" || return 1
  printf '%s/%s' "$d" "$(basename -- "$1")"
}

# tool runs the Go half with the given arguments. In image mode `mounts` carries the bind
# mounts the caller prepared; the container runs as the calling user, so the report is theirs.
mounts=()
if [ -n "$IMAGE" ]; then
  need docker "image mode runs the check inside --image"
  need id "image mode runs the container as your user"
  tool() {
    docker run --rm -u "$(id -u):$(id -g)" "${mounts[@]+"${mounts[@]}"}" \
      "${DOCKER_ARGS[@]+"${DOCKER_ARGS[@]}"}" --entrypoint "$IMAGE_BIN" "$IMAGE" "$@"
  }
elif [ -n "$BIN" ]; then
  [ -x "$BIN" ] || die "--bin '$BIN' is not an executable file"
  BIN="$(abs "$BIN")"
  tool() { "$BIN" "$@"; }
else
  need go "host mode runs the check with 'go run'; use --image to run it inside the holdfast image instead"
  commit="$(git -C "$here" rev-parse --short=12 HEAD 2>/dev/null || true)"
  [[ "$commit" =~ ^[0-9a-f]{7,40}$ ]] || commit=unknown
  tool() {
    ( cd "$here" && go run -ldflags "-X github.com/NSchatz/holdfast/internal/version.Commit=$commit" \
        ./scripts/clientreport "$@" )
  }
fi

# --- --verify: the identity check alone ----------------------------------------------------------
if [ -n "$VERIFY" ]; then
  if [ -n "$SERVICE$CONFIG$OUT$REFRESH_DIR$RESCAN_DIR" ]; then die "--verify takes no other flag but --image or --bin"; fi
  [ -f "$VERIFY" ] || die "no report at $VERIFY"
  VERIFY="$(abs "$VERIFY")"
  if [ -n "$IMAGE" ]; then
    mounts=(-v "$VERIFY:/client-report/report.json:ro")
    tool --verify /client-report/report.json
  else
    tool --verify "$VERIFY"
  fi
  exit $?
fi

# --- the check ------------------------------------------------------------------------------------
case "$SERVICE" in
  plex|sonarr|radarr) ;;
  *) usage >&2; die "--service must be plex, sonarr or radarr" ;;
esac
[ -n "$CONFIG" ] || { usage >&2; die "--config is required"; }
[ -f "$CONFIG" ] || die "no configuration at $CONFIG"
CONFIG="$(abs "$CONFIG")"

need date "it stamps the report's file name"
stamp="$(date -u +%Y-%m-%d)"
if [ -z "$OUT" ]; then
  OUT="$here/testdata/client-reports/$SERVICE-$stamp.json"
fi
OUT="$(abs "$OUT")" || die "the directory of --out does not exist"
out_dir="$(dirname -- "$OUT")"
[ -d "$out_dir" ] && [ -w "$out_dir" ] || die "the report's directory $out_dir is not a writable directory"
[ ! -e "$OUT" ] && [ ! -L "$OUT" ] || die "refusing to overwrite the existing report $OUT (a report is a record; move it or name another --out)"

args=(--service "$SERVICE")
[ -z "$REFRESH_DIR" ] || args+=(--refresh-dir "$REFRESH_DIR")
[ -z "$RESCAN_DIR" ] || args+=(--rescan-dir "$RESCAN_DIR")
if [ -n "$REFRESH_DIR$RESCAN_DIR" ]; then
  say "a WRITE check was asked for: one refresh or rescan request for that one directory will be sent"
fi

rc=0
if [ -n "$IMAGE" ]; then
  mounts=(-v "$CONFIG:/client-report/config.yaml:ro" -v "$out_dir:/client-report/out")
  tool "${args[@]}" --config /client-report/config.yaml --out "/client-report/out/$(basename -- "$OUT")" >&2 || rc=$?
else
  tool "${args[@]}" --config "$CONFIG" --out "$OUT" >&2 || rc=$?
fi
if [ "$rc" -ne 0 ]; then
  [ ! -e "$OUT" ] || die "the check failed (exit $rc) and left a file at $OUT; it is not a report"
  die "no report was written (exit $rc, the reason is above)"
fi
[ -f "$OUT" ] || die "the check reported success and wrote no report at $OUT"
say "wrote $OUT"
say "read it, then commit it: git add '$OUT' && git commit -m 'test(client-reports): $SERVICE live check $stamp'"
