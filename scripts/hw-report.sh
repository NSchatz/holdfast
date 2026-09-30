#!/usr/bin/env bash
# hw-report.sh - run a fixed synthetic clip set through holdfast's real path with one named
# encoder, and write a redacted hardware report for the owner to commit (brief T43).
#
#   scripts/hw-report.sh --encoder nvenc --image holdfast:dev --docker-arg=--gpus --docker-arg=all
#   scripts/hw-report.sh --encoder vaapi --image holdfast:dev \
#       --docker-arg=--device=/dev/dri --docker-arg=--group-add="$(stat -c %g /dev/dri/renderD128)"
#   scripts/hw-report.sh --encoder amf --holdfast /usr/local/bin/holdfast     # a host install
#   scripts/hw-report.sh --verify testdata/hw-reports/nvenc-2026-09-30.json   # re-check a report
#
# What it does, in order: builds two small lossless FFV1 clips (8-bit SDR 4:2:0 and 10-bit
# 4:2:0 PQ/BT.2020 HDR10 carrying mastering-display and content-light metadata), puts them in
# a throwaway library under $HOME, writes a config naming the encoder with `hw_fallback: skip`,
# runs `holdfast run --file` once per clip (so every gate runs, and each clip has its own
# wall-clock), reads the ledger back with `holdfast export`, and writes
# testdata/hw-reports/<encoder>-<date>.json.
#
# What it refuses: a missing tool (named), an encoder `holdfast validate` rejects, an encoder
# the start-time probe finds unusable (hw_fallback is skip, so that is a refusal and never a
# silent software encode), an existing report (never overwritten), and a report in which a
# forbidden token survives the redaction pass. Every refusal writes no report and exits
# non-zero. docs/hardware-reports.md is the reference.
set -euo pipefail

# --- defaults: the one place the clip set is described ---------------------------------------
CLIP_SECONDS=2          # each clip's length; a report takes minutes, not hours
CLIP_SIZE=640x360       # large enough for every hardware encoder's minimum frame size
CLIP_RATE=24
# The HDR10 static metadata the 10-bit clip carries (SMPTE ST 2086 mastering display, and
# MaxCLL,MaxFALL), in libx265's notation. The same values the engine's fidelity fixtures use.
HDR_MASTER_DISPLAY='G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)'
HDR_MAX_CLL='1000,400'
REPORT_SCHEMA=1
REDACTED='<redacted>'
# ----------------------------------------------------------------------------------------------

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

usage() {
  cat <<'EOF'
usage: hw-report.sh --encoder KEY [--holdfast PATH | --image REF [--docker-arg ARG]...] [--out PATH]
       hw-report.sh --verify REPORT

  --encoder KEY     the encoder to report on; any key `holdfast validate` accepts (required)
  --holdfast PATH   host mode: the holdfast binary (default: holdfast on PATH), with the
                    host's ffmpeg and ffprobe on PATH
  --image REF       image mode: run holdfast, ffmpeg and ffprobe inside this image
  --docker-arg ARG  image mode: one extra `docker run` argument (repeatable), e.g.
                    --docker-arg=--gpus --docker-arg=all, --docker-arg=--device=/dev/dri,
                    --docker-arg=--group-add=993
  --out PATH        where to write the report (default testdata/hw-reports/<encoder>-<date>.json);
                    an existing file is never overwritten
  --verify REPORT   re-run the forbidden-token check over an existing report on this host
EOF
}

die() { printf 'hw-report: %s\n' "$*" >&2; exit 1; }
say() { printf 'hw-report: %s\n' "$*" >&2; }

ENCODER=""; HOLDFAST=""; IMAGE=""; OUT=""; VERIFY=""
DOCKER_ARGS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --encoder)       [ $# -ge 2 ] || { usage >&2; exit 2; }; ENCODER="$2"; shift 2 ;;
    --encoder=*)     ENCODER="${1#*=}"; shift ;;
    --holdfast)      [ $# -ge 2 ] || { usage >&2; exit 2; }; HOLDFAST="$2"; shift 2 ;;
    --holdfast=*)    HOLDFAST="${1#*=}"; shift ;;
    --image)         [ $# -ge 2 ] || { usage >&2; exit 2; }; IMAGE="$2"; shift 2 ;;
    --image=*)       IMAGE="${1#*=}"; shift ;;
    --docker-arg)    [ $# -ge 2 ] || { usage >&2; exit 2; }; DOCKER_ARGS+=("$2"); shift 2 ;;
    --docker-arg=*)  DOCKER_ARGS+=("${1#*=}"); shift ;;
    --out)           [ $# -ge 2 ] || { usage >&2; exit 2; }; OUT="$2"; shift 2 ;;
    --out=*)         OUT="${1#*=}"; shift ;;
    --verify)        [ $# -ge 2 ] || { usage >&2; exit 2; }; VERIFY="$2"; shift 2 ;;
    --verify=*)      VERIFY="${1#*=}"; shift ;;
    -h|--help)       usage; exit 0 ;;
    *)               printf 'hw-report: unknown argument %q\n' "$1" >&2; usage >&2; exit 2 ;;
  esac
done

# --- required host tools, checked up front and named -------------------------------------------
need() { command -v "$1" >/dev/null 2>&1 || die "required tool '$1' is not on PATH ($2)"; }
need jq "it builds the report from an allow-list of fields and runs the redaction pass"
need date "it stamps the report's file name and times each clip"
need mktemp "it creates the throwaway library"
need id "it finds the user name the redaction pass removes"

# --- the forbidden tokens: what no report may carry -----------------------------------------------
# Literal tokens: this host's names, the user, the home directory and the work directory. A
# token shorter than 3 characters is not treated as one: it cannot identify a host, and
# replacing every "a" in a report would make the report unreadable, not safer.
TOKENS=()
add_token() {
  local t="$1"
  [ "${#t}" -ge 3 ] || return 0
  [ "$t" != "$REDACTED" ] || return 0
  local have
  for have in "${TOKENS[@]+"${TOKENS[@]}"}"; do [ "$have" = "$t" ] && return 0; done
  TOKENS+=("$t")
}
collect_tokens() {
  local h u d
  for h in "$(hostname 2>/dev/null || true)" "$(hostname -f 2>/dev/null || true)" \
           "$(uname -n 2>/dev/null || true)" "$(cat /proc/sys/kernel/hostname 2>/dev/null || true)" \
           "${HOSTNAME:-}"; do
    h="$(printf '%s' "$h" | head -n1 | tr -d '[:space:]')"
    [ -n "$h" ] || continue
    add_token "$h"; add_token "${h%%.*}"
  done
  for u in "${USER:-}" "${LOGNAME:-}" "$(id -un 2>/dev/null || true)"; do add_token "$u"; done
  add_token "${HOME:-}"
  if command -v getent >/dev/null 2>&1; then
    d="$(getent passwd "$(id -u)" 2>/dev/null | cut -d: -f6 || true)"
    add_token "$d"
  fi
  add_token "$here"
}

# The shapes of a serial or a hardware identity: a UUID, an NVIDIA GPU or MIG identity, a MAC
# address, a PCI bus address, and a run of ten or more digits (a board serial) or sixteen or
# more hex digits. One list, read by the redaction pass and by the check that follows it.
SERIAL_RE='[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}|(GPU|MIG)-[0-9A-Fa-f-]{8,}|([0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2}|[0-9A-Fa-f]{4,8}:[0-9A-Fa-f]{2}:[0-9A-Fa-f]{2}\.[0-7]|[0-9]{10,}|[0-9A-Fa-f]{16,}'
# An absolute filesystem path: a slash at the start of a string or after a space, bracket,
# quote, equals sign, colon, comma or a redaction marker. No field the report copies needs one.
PATH_LEAD='(^|[[:space:]([=:,>"'"'"'])'
PATH_TAIL='/[^[:space:])\],"'"'"']*'
PATH_RE="${PATH_LEAD}${PATH_TAIL}"
# The longest string value a report keeps. It is applied AFTER the redaction, so a cut can
# never leave half of a token the redaction would have recognised whole.
MAX_FIELD=200

tokens_json() { printf '%s\n' "${TOKENS[@]+"${TOKENS[@]}"}" | jq -R . | jq -s 'map(select(length > 0)) | sort_by(-length)'; }

# redact: every string value in the JSON on stdin with each literal token (longest first), each
# serial-shaped token and each absolute path replaced, then cut to MAX_FIELD. Keys are the
# script's own and are not touched.
redact() {
  jq --argjson toks "$(tokens_json)" --arg re "$SERIAL_RE" --arg lead "$PATH_LEAD" --arg tail "$PATH_TAIL" \
     --arg red "$REDACTED" --argjson max "$MAX_FIELD" '
    def scrub: reduce $toks[] as $t (.; split($t) | join($red))
      | gsub($re; $red)
      | gsub("(?<lead>" + $lead + ")" + $tail; "\(.lead)" + $red)
      | .[0:$max];
    walk(if type == "string" then scrub else . end)'
}

# check_report FILE: refuses (returns non-zero, naming the kind but never echoing the token)
# when any literal token appears anywhere in the file's bytes, or any serial-shaped token in
# any string value.
check_report() {
  local f="$1" t bad=0
  jq -e . "$f" >/dev/null 2>&1 || { say "the report is not valid JSON"; return 1; }
  for t in "${TOKENS[@]+"${TOKENS[@]}"}"; do
    if command grep -qF -- "$t" "$f"; then
      say "a forbidden token (a host name, user name or path of this host) is present in the report"
      bad=1
    fi
  done
  if jq -e --arg re "$SERIAL_RE" '[.. | strings | select(test($re))] | length > 0' "$f" >/dev/null; then
    say "a serial-shaped token (UUID, GPU identity, MAC, PCI address or long serial) is present in the report"
    bad=1
  fi
  if jq -e --arg re "$PATH_RE" '[.. | strings | select(test($re))] | length > 0' "$f" >/dev/null; then
    say "an absolute filesystem path is present in the report"
    bad=1
  fi
  return "$bad"
}

collect_tokens

if [ -n "$VERIFY" ]; then
  [ -f "$VERIFY" ] || die "no report at '$VERIFY'"
  check_report "$VERIFY" || die "refusing: the report carries a forbidden token (above)"
  say "the report carries none of this host's names, user, paths or serial-shaped tokens"
  exit 0
fi

[ -n "$ENCODER" ] || { usage >&2; die "--encoder KEY is required"; }
# The key names the report's file, so it must be a plain token; whether it is an encoder at
# all is `holdfast validate`'s answer, below, and no list is kept here.
[[ "$ENCODER" =~ ^[A-Za-z0-9_]+$ ]] || die "--encoder '$ENCODER' is not a plain encoder key (letters, digits, underscore)"
if [ -n "$IMAGE" ] && [ -n "$HOLDFAST" ]; then die "--image and --holdfast are two different modes; name one"; fi
if [ -z "$IMAGE" ] && [ "${#DOCKER_ARGS[@]}" -gt 0 ]; then die "--docker-arg needs --image"; fi

if [ -n "$IMAGE" ]; then
  MODE=image
  need docker "image mode runs holdfast, ffmpeg and ffprobe inside --image"
else
  MODE=host
  HOLDFAST="${HOLDFAST:-holdfast}"
  if [[ "$HOLDFAST" != */* ]]; then
    need "$HOLDFAST" "host mode runs this holdfast binary (or name one with --holdfast)"
  else
    [ -x "$HOLDFAST" ] || die "--holdfast '$HOLDFAST' is not an executable file"
  fi
  need ffmpeg "host mode builds the clips and holdfast encodes with the host's ffmpeg"
  need ffprobe "host mode inspects the clips and outputs with the host's ffprobe"
fi

[ -n "${HOME:-}" ] && [ -d "$HOME" ] || die "\$HOME is not a directory: the throwaway library goes under it (holdfast refuses a tmpfs state directory, and /tmp is tmpfs on Debian 13)"

stamp="$(date -u +%Y-%m-%d)"
if [ -z "$OUT" ]; then
  [ -d "$here/testdata/hw-reports" ] || die "no testdata/hw-reports directory in '$here'"
  OUT="$here/testdata/hw-reports/$ENCODER-$stamp.json"
fi
out_dir="$(dirname -- "$OUT")"
[ -d "$out_dir" ] || die "the report's directory does not exist: $out_dir"
[ ! -e "$OUT" ] && [ ! -L "$OUT" ] || die "refusing to overwrite the existing report $OUT (a report is a record; move it or name another --out)"
add_token "$(cd "$out_dir" && pwd)"

# --- the throwaway library ---------------------------------------------------------------------
work="$(mktemp -d "$HOME/.holdfast-hw-report.XXXXXX")"
cleanup() { rm -rf -- "$work"; }
trap cleanup EXIT
trap 'exit 130' INT TERM
add_token "$work"
mkdir -p "$work/media" "$work/state"
[ -n "$HOLDFAST" ] && [[ "$HOLDFAST" == */* ]] && add_token "$(cd "$(dirname -- "$HOLDFAST")" && pwd)"

# Where the library, state and config are as holdfast sees them, and how each tool is run.
if [ "$MODE" = image ]; then
  M=/media; S=/state; C=/config.yaml
  mounts=(-u "$(id -u):$(id -g)" -v "$work/media:/media" -v "$work/state:/state" -v "$work/config.yaml:/config.yaml:ro")
  hf()      { docker run --rm "${mounts[@]}" "${DOCKER_ARGS[@]+"${DOCKER_ARGS[@]}"}" "$IMAGE" "$@"; }
  ff()      { docker run --rm "${mounts[@]}" --entrypoint /usr/local/bin/ffmpeg "$IMAGE" "$@"; }
  fp()      { docker run --rm "${mounts[@]}" --entrypoint /usr/local/bin/ffprobe "$IMAGE" "$@"; }
  ff_dev()  { docker run --rm "${DOCKER_ARGS[@]+"${DOCKER_ARGS[@]}"}" --entrypoint /usr/local/bin/ffmpeg "$IMAGE" "$@"; }
else
  M="$work/media"; S="$work/state"; C="$work/config.yaml"
  hf()      { "$HOLDFAST" "$@"; }
  ff()      { ffmpeg "$@"; }
  fp()      { ffprobe "$@"; }
  ff_dev()  { ffmpeg "$@"; }
fi
: >"$work/config.yaml"

# --- the clips -------------------------------------------------------------------------------
# Lossless intra FFV1 in Matroska: no holdfast target skips it as already at target or as a
# better family, and a lossless source is large enough that any honest encode is smaller.
say "building the clips (${CLIP_SECONDS}s, ${CLIP_SIZE} at ${CLIP_RATE} fps)"
src="testsrc2=duration=${CLIP_SECONDS}:size=${CLIP_SIZE}:rate=${CLIP_RATE}"
ff -hide_banner -nostdin -loglevel error -y -f lavfi -i "$src" \
  -c:v ffv1 -level 3 -pix_fmt yuv420p \
  -color_primaries bt709 -color_trc bt709 -colorspace bt709 -color_range tv \
  -f matroska -- "$M/sdr8.mkv" || die "could not build the 8-bit SDR clip"
# The HDR10 blocks are written by libx265 into an HEVC intermediate (with the colour tags in its
# VUI), then carried by ffmpeg into the FFV1 clip, where Matroska stores them in its Colour
# element and the decoder hands them to every frame. The check below reads them back.
ff -hide_banner -nostdin -loglevel error -y -f lavfi -i "$src" \
  -c:v libx265 -preset ultrafast -pix_fmt yuv420p10le \
  -x265-params "log-level=error:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:master-display=${HDR_MASTER_DISPLAY}:max-cll=${HDR_MAX_CLL}" \
  -f matroska -- "$M/.hdr10-intermediate.mkv" || die "could not build the HDR10 intermediate"
ff -hide_banner -nostdin -loglevel error -y -i "$M/.hdr10-intermediate.mkv" \
  -c:v ffv1 -level 3 -pix_fmt yuv420p10le \
  -color_primaries bt2020 -color_trc smpte2084 -colorspace bt2020nc -color_range tv \
  -f matroska -- "$M/hdr10.mkv" || die "could not build the HDR10 clip"
rm -f -- "$work/media/.hdr10-intermediate.mkv"

# facts NAME: one JSON object of what ffprobe reads from $M/NAME - the codec, pixel format and
# colour tags, and whether the HDR10 blocks are present in stream or first-frame side data.
facts() {
  local stream frame
  stream="$(fp -v error -select_streams v:0 -show_streams -of json -- "$M/$1")" || return 1
  frame="$(fp -v error -select_streams v:0 -read_intervals '%+#1' -show_frames -of json -- "$M/$1")" || return 1
  jq -n --argjson s "$stream" --argjson f "$frame" '
    ($s.streams[0] // {}) as $st | ($f.frames[0] // {}) as $fr |
    ([$st.side_data_list[]?.side_data_type, $fr.side_data_list[]?.side_data_type] | map(select(. != null))) as $sd |
    { codec: $st.codec_name, pix_fmt: $st.pix_fmt,
      color_primaries: ($st.color_primaries // $fr.color_primaries),
      color_transfer: ($st.color_transfer // $fr.color_transfer),
      color_space: ($st.color_space // $fr.color_space),
      mastering_display: ($sd | index("Mastering display metadata") != null),
      content_light: ($sd | index("Content light level metadata") != null) }'
}

sdr_src="$(facts sdr8.mkv)" || die "could not inspect the 8-bit SDR clip"
hdr_src="$(facts hdr10.mkv)" || die "could not inspect the HDR10 clip"
jq -e '.codec == "ffv1" and .pix_fmt == "yuv420p"' <<<"$sdr_src" >/dev/null \
  || die "the 8-bit SDR clip is not FFV1 yuv420p: $sdr_src"
jq -e '.codec == "ffv1" and .pix_fmt == "yuv420p10le" and .color_primaries == "bt2020" and
       .color_transfer == "smpte2084" and .mastering_display and .content_light' <<<"$hdr_src" >/dev/null \
  || die "the HDR10 clip does not carry what it must (FFV1 yuv420p10le, BT.2020, PQ, mastering display, content light): $hdr_src"
sdr_bytes="$(stat -c %s "$work/media/sdr8.mkv")"
hdr_bytes="$(stat -c %s "$work/media/hdr10.mkv")"

# --- the configuration -----------------------------------------------------------------------
cat >"$work/config.yaml" <<EOF
library_roots:
  - $M
state_dir: $S
encoder: $ENCODER
hw_fallback: skip
log_level: info
EOF
hf validate --config "$C" >"$work/validate.log" 2>&1 \
  || { cat "$work/validate.log" >&2; die "holdfast validate rejected the configuration for encoder '$ENCODER' (above)"; }

# --- the runs: one per clip, each timed ----------------------------------------------------------
now_ms() { local n; n="$(date +%s%N)"; printf '%s' "$((n / 1000000))"; }
declare -A WALL_MS
for clip in sdr8 hdr10; do
  say "running holdfast over the $clip clip with encoder '$ENCODER'"
  t0="$(now_ms)"
  rc=0
  hf run --config "$C" --file "$M/$clip.mkv" >"$work/run-$clip.log" 2>&1 || rc=$?
  WALL_MS[$clip]=$(( $(now_ms) - t0 ))
  if [ "$rc" -ne 0 ]; then
    command grep -E 'hardware:|encoder|probe' "$work/run-$clip.log" >&2 || tail -n 20 "$work/run-$clip.log" >&2
    if command grep -qE 'hw_fallback is skip|encoder probed|RequireAvailable|not available|unavailable|refused' "$work/run-$clip.log"; then
      die "encoder '$ENCODER' is unavailable on this host (holdfast refused to start; the reason is above). No report was written."
    fi
    die "holdfast run exited $rc over the $clip clip (the log is above). No report was written."
  fi
done

hf export --config "$C" >"$work/export.ndjson" 2>"$work/export.log" \
  || { cat "$work/export.log" >&2; die "holdfast export could not read the ledger back"; }

# The start-time probe's answer for a hardware encoder (holdfast logs one line per probe).
probe_json="null"
line="$(command grep -h 'hardware: encoder probed' "$work/run-sdr8.log" | head -n1 || true)"
if [ -n "$line" ]; then
  e8="$(sed -n 's/.* 8bit=\([a-z]*\).*/\1/p' <<<"$line")"
  e10="$(sed -n 's/.* 10bit=\([a-z]*\).*/\1/p' <<<"$line")"
  probe_json="$(jq -n --arg a "$e8" --arg b "$e10" '{"8bit": ($a == "true"), "10bit": ($b == "true")}')"
fi

# --- device facts, each from a named source, each an allow-listed short string -------------------
# clean: printable, one line, no shell or JSON surprises. It neither redacts nor shortens: the
# pass below does both, in that order.
clean() { tr -cd 'A-Za-z0-9 ._:()+/,@-' | sed 's/^ *//; s/ *$//'; }

nvidia_json="null"
if command -v nvidia-smi >/dev/null 2>&1; then
  nv="$(nvidia-smi --query-gpu=driver_version,name --format=csv,noheader 2>/dev/null | head -n1 || true)"
  if [ -n "$nv" ]; then
    nvidia_json="$(jq -n --arg d "$(cut -d, -f1 <<<"$nv" | clean)" --arg n "$(cut -d, -f2- <<<"$nv" | clean)" \
      '{driver_version: $d, name: $n}')"
  fi
fi

vainfo_driver=""
if command -v vainfo >/dev/null 2>&1; then
  vainfo_driver="$(vainfo --display drm 2>&1 | sed -n 's/.*Driver version: *//p' | head -n1 | clean || true)"
fi

# Each render node's vendor and kernel driver from sysfs, and, for an Intel or AMD node, the VA
# driver ffmpeg itself loads there (its verbose `VAAPI driver:` line). Opening a device to
# initialise it encodes nothing. Node paths are not recorded.
nodes_json="[]"
for n in /sys/class/drm/renderD*; do
  [ -e "$n/device/vendor" ] || continue
  vid="$(cat "$n/device/vendor" 2>/dev/null || true)"
  case "$vid" in 0x8086) vendor=intel ;; 0x1002) vendor=amd ;; 0x10de) vendor=nvidia ;; *) vendor="$(clean <<<"$vid")" ;; esac
  kdrv=""; [ -L "$n/device/driver" ] && kdrv="$(basename "$(readlink "$n/device/driver")" | clean)"
  va=""
  if [ "$vendor" = intel ] || [ "$vendor" = amd ]; then
    va="$(ff_dev -hide_banner -nostdin -v verbose -init_hw_device "vaapi=va:/dev/dri/$(basename "$n"),connection_type=drm" \
      -f lavfi -i nullsrc=s=64x64 -frames:v 1 -f null - 2>&1 | sed -n 's/.*VAAPI driver: *//p' | head -n1 | clean || true)"
  fi
  nodes_json="$(jq --arg v "$vendor" --arg k "$kdrv" --arg va "$va" \
    '. + [{vendor: $v, kernel_driver: (if $k == "" then null else $k end), va_driver: (if $va == "" then null else $va end)}]' <<<"$nodes_json")"
done

ffmpeg_pin="$(ff -hide_banner -version 2>/dev/null | head -n1 | clean || true)"
[ -n "$ffmpeg_pin" ] || ffmpeg_pin="$(ff -version 2>/dev/null | head -n1 | clean || true)"
holdfast_version="$(hf version 2>/dev/null | head -n1 | clean || true)"
[ -n "$ffmpeg_pin" ] || die "could not read the ffmpeg version"
[ -n "$holdfast_version" ] || die "could not read the holdfast version"

# --- the per-clip figures, from the ledger, on an allow-list -------------------------------------
clip_json() {
  local name="$1" srcfacts="$2" bytes="$3" row outfacts="null"
  row="$(jq -c --arg f "$name.mkv" 'select((.path | split("/") | last) == $f)' "$work/export.ndjson" | tail -n1)"
  [ -n "$row" ] || die "the ledger holds no row for the $name clip"
  if [ "$(jq -r .status <<<"$row")" = "done" ]; then
    outfacts="$(facts "$name.mkv")" || die "could not inspect the $name output"
  fi
  jq -n --arg name "$name" --argjson src "$srcfacts" --argjson bytes "$bytes" --argjson row "$row" \
        --argjson out "$outfacts" --argjson wall "${WALL_MS[$name]}" '
    { clip: $name,
      source: ($src + {bytes: $bytes}),
      outcome: {
        status: $row.status,
        reason: ($row.reason // null),
        encoder_ran: ($row.encoder // null),
        vmaf_mean: $row.vmaf_mean, vmaf_min_pool: $row.vmaf_min, vmaf_model: ($row.vmaf_model // null),
        vmaf_pix_fmt: ($row.vmaf_pix_fmt // null),
        vmaf_chroma: $row.vmaf_chroma, vmaf_chroma_metric: ($row.vmaf_chroma_metric // null),
        source_codec: $row.source_codec,
        source_bytes: $row.source_bytes, output_bytes: $row.output_bytes,
        savings_bytes: (if $row.source_bytes != null and $row.output_bytes != null
                        then $row.source_bytes - $row.output_bytes else null end),
        savings_percent: (if $row.source_bytes != null and $row.output_bytes != null and $row.source_bytes > 0
                          then ((($row.source_bytes - $row.output_bytes) * 10000 / $row.source_bytes) | round) / 100
                          else null end),
        output_width: $row.output_width, output_height: $row.output_height,
        encode_ms: $row.encode_ms },
      output: $out,
      timing: { wall_ms: $wall } }'
}

sdr_clip="$(clip_json sdr8 "$sdr_src" "$sdr_bytes")"
hdr_clip="$(clip_json hdr10 "$hdr_src" "$hdr_bytes")"

jq -n --argjson schema "$REPORT_SCHEMA" --arg date "$stamp" --arg enc "$ENCODER" --arg mode "$MODE" \
      --arg hv "$holdfast_version" --arg fv "$ffmpeg_pin" --argjson probe "$probe_json" \
      --argjson nv "$nvidia_json" --arg vainfo "$vainfo_driver" --argjson nodes "$nodes_json" \
      --argjson c1 "$sdr_clip" --argjson c2 "$hdr_clip" \
      --arg secs "$CLIP_SECONDS" --arg size "$CLIP_SIZE" --arg rate "$CLIP_RATE" '
  { schema: $schema,
    date: $date,
    encoder: { requested: $enc, ran: ([$c1, $c2] | map(.outcome.encoder_ran) | map(select(. != null)) | unique),
               start_probe: $probe },
    mode: $mode,
    holdfast_version: $hv,
    ffmpeg: $fv,
    device: { nvidia: $nv, vainfo_driver: (if $vainfo == "" then null else $vainfo end), render_nodes: $nodes },
    clip_set: { seconds: ($secs | tonumber), size: $size, rate: ($rate | tonumber), source_codec: "ffv1" },
    clips: [$c1, $c2] }' | redact >"$work/report.json" || die "could not assemble the report"

check_report "$work/report.json" || die "refusing to write the report: a forbidden token survived the redaction pass (above)"

# Never overwrite: link the finished file into place (link(2) fails if the name exists), in
# the report's own directory so a reader never sees a half-written file.
tmp="$(mktemp "$out_dir/.hw-report.XXXXXX")"
cp -- "$work/report.json" "$tmp"
chmod 0644 "$tmp"
if ! ln -- "$tmp" "$OUT" 2>/dev/null; then
  rm -f -- "$tmp"
  die "refusing to overwrite the existing report $OUT"
fi
rm -f -- "$tmp"
say "wrote $OUT"
jq -r '.clips[] | "  \(.clip): \(.outcome.status) \(.outcome.reason // "") encoder=\(.outcome.encoder_ran // "-") vmaf_mean=\(.outcome.vmaf_mean // "-") min_pool=\(.outcome.vmaf_min_pool // "-") \(.outcome.source_bytes // "-") -> \(.outcome.output_bytes // "-") bytes, \(.timing.wall_ms) ms"' "$OUT" >&2
