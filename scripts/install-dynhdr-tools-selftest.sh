#!/usr/bin/env bash
# Prove scripts/install-dynhdr-tools.sh FAILS THE WAY IT SAYS IT DOES. Part of `make check`.
#
# Modelled on scripts/install-ffmpeg-selftest.sh, for the same reason: an installer's
# claims about its failure modes rot silently unless something defeats each one on purpose.
# This installer claims more than the ffmpeg one in one respect - it installs dovi_tool and
# hdr10plus_tool BOTH or NEITHER - so that is defeated here too.
#
# HERMETIC - no network. `curl` is a fake on PATH that serves a fixture archive per tool,
# chosen by the URL it was asked for, and the pin is driven by rewriting the
# *_SHA256_* ARGs of a THROWAWAY COPY of the real Dockerfile, so the real digest gate, tar
# and sha256sum run for real. Nothing in the working tree is mutated and nothing is
# downloaded.
#
# The load-bearing assertion in nearly every case is that the destination is EMPTY
# afterwards and the message says which failure this was.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

for t in tar gzip sha256sum sed; do
  command -v "$t" >/dev/null 2>&1 \
    || { echo "::error::install-dynhdr-tools selftest needs '$t' and it is not on PATH - it did NOT run" >&2; exit 1; }
done

work="$(mktemp -d)" || { echo "::error::install-dynhdr-tools selftest: mktemp failed" >&2; exit 1; }
trap 'chmod -R u+w "$work" 2>/dev/null || true; rm -rf "$work"' EXIT

repo="$work/repo"
fakebin="$work/fakebin"
mkdir -p "$repo/scripts" "$fakebin"

# The REAL Dockerfile and the WORKING-TREE installer, so an uncommitted change is graded.
cp "$here/Dockerfile" "$repo/Dockerfile"
cp "$here/scripts/install-dynhdr-tools.sh" "$repo/scripts/install-dynhdr-tools.sh"
cmp -s "$here/scripts/install-dynhdr-tools.sh" "$repo/scripts/install-dynhdr-tools.sh" \
  || { echo "::error::selftest: the sandbox installer is not the working-tree installer - it graded the wrong thing" >&2; exit 1; }

# --- the fake curl -------------------------------------------------------------------
# Writes the body to the -o path and prints the HTTP status for -w '%{http_code}'. The
# payload is picked by the URL (the last argument), so each tool gets its own archive.
# FAKE_CURL_GONE_FOR names one tool that answers 404 while the other answers normally.
cat >"$fakebin/curl" <<'FAKE'
#!/usr/bin/env bash
set -u
url="${*: -1}"
printf 'call %s\n' "$url" >> "${FAKE_CURL_LOG:-/dev/null}"
out=""; prev=""
for a in "$@"; do
  [ "$prev" = "-o" ] && out="$a"
  prev="$a"
done
case "$url" in
  */hdr10plus_tool/*) payload="${FAKE_PAYLOAD_H10P:-/dev/null}"; tool=hdr10plus_tool ;;
  */dovi_tool/*)      payload="${FAKE_PAYLOAD_DOVI:-/dev/null}"; tool=dovi_tool ;;
  *) printf '000'; exit 2 ;;
esac
mode="${FAKE_CURL_MODE:-ok}"
[ -n "${FAKE_CURL_GONE_FOR:-}" ] && [ "$tool" = "$FAKE_CURL_GONE_FOR" ] && mode=gone
case "$mode" in
  ok)     [ -n "$out" ] && cp "$payload" "$out"; printf '200' ;;
  gone)   [ -n "$out" ] && printf 'Not Found' > "$out"; printf '404' ;;
  gone410)[ -n "$out" ] && printf 'Gone' > "$out";      printf '410' ;;
  http5xx)[ -n "$out" ] && : > "$out";                  printf '503' ;;
  neterr) printf '000'
          printf 'curl: (6) Could not resolve host: github.com\n' >&2
          exit 6 ;;
  *) printf '000'; exit 2 ;;
esac
exit 0
FAKE
chmod +x "$fakebin/curl"

# --- decoys: what a "just make CI green" fix would reach for ----------------------------
for decoy in apt-get apk dnf yum brew cargo dovi_tool hdr10plus_tool; do
  cat >"$fakebin/$decoy" <<DECOY
#!/usr/bin/env bash
printf '%s %s\n' "$decoy" "\$*" >> "\${DECOY_LOG:-/dev/null}"
exit 0
DECOY
  chmod +x "$fakebin/$decoy"
done

# The real pin, read from the real Dockerfile, so the message assertions check the
# installer prints the ACTUAL pin.
DOVI_V="$(sed -n 's/^ARG DOVI_TOOL_VERSION=\(.*\)$/\1/p' "$here/Dockerfile" | head -1)"
H10P_V="$(sed -n 's/^ARG HDR10PLUS_TOOL_VERSION=\(.*\)$/\1/p' "$here/Dockerfile" | head -1)"
[ -n "$DOVI_V" ] && [ -n "$H10P_V" ] \
  || { echo "::error::selftest could not read the tool pins from the Dockerfile - it did NOT run" >&2; exit 1; }

# --- fixture archives -----------------------------------------------------------------
# build_archive <out> <tool> <variant>: good | wrong-version | extra-entry | not-exec | broken
build_archive() {
  local out="$1" tool="$2" variant="$3" src="$work/pkgsrc" v
  v="$DOVI_V"; [ "$tool" = hdr10plus_tool ] && v="$H10P_V"
  rm -rf "$src"; mkdir -p "$src"
  case "$variant" in
    wrong-version) printf '#!/bin/sh\necho "%s 0.0.1"\n' "$tool" >"$src/$tool" ;;
    broken)        printf '#!/bin/sh\necho "cannot execute: exec format error" >&2\nexit 126\n' >"$src/$tool" ;;
    *)             printf '#!/bin/sh\necho "%s %s"\n' "$tool" "$v" >"$src/$tool" ;;
  esac
  chmod +x "$src/$tool"
  [ "$variant" = not-exec ] && chmod 0644 "$src/$tool"
  if [ "$variant" = extra-entry ]; then
    printf 'surprise\n' >"$src/README"
    tar -C "$src" -czf "$out" "./$tool" ./README
  else
    tar -C "$src" -czf "$out" "./$tool"
  fi
}

digest_of() { sha256sum "$1" | awk '{print $1}'; }

# pin <dovi archive> <hdr10plus archive>: both arches' digests, so this behaves the same
# on an x86_64 runner and an arm64 laptop.
pin() {
  local d h
  d="$(digest_of "$1")"; h="$(digest_of "$2")"
  sed -i -e "s|^ARG DOVI_TOOL_SHA256_AMD64=.*|ARG DOVI_TOOL_SHA256_AMD64=$d|" \
         -e "s|^ARG DOVI_TOOL_SHA256_ARM64=.*|ARG DOVI_TOOL_SHA256_ARM64=$d|" \
         -e "s|^ARG HDR10PLUS_TOOL_SHA256_AMD64=.*|ARG HDR10PLUS_TOOL_SHA256_AMD64=$h|" \
         -e "s|^ARG HDR10PLUS_TOOL_SHA256_ARM64=.*|ARG HDR10PLUS_TOOL_SHA256_ARM64=$h|" "$repo/Dockerfile"
}

# --- harness ---------------------------------------------------------------------------
pass=0; failed=0
out=""; rc=0; dest=""

run_install_at() {  # <mode> <absolute dest path>
  local mode="$1"
  dest="$2"
  : >"$work/curl.log"
  : >"$work/decoy.log"
  rc=0
  out="$(PATH="$fakebin:$PATH" \
         FAKE_CURL_MODE="$mode" \
         FAKE_CURL_GONE_FOR="${GONE_FOR:-}" \
         FAKE_PAYLOAD_DOVI="$DOVI" \
         FAKE_PAYLOAD_H10P="$H10P" \
         FAKE_CURL_LOG="$work/curl.log" \
         DECOY_LOG="$work/decoy.log" \
         bash "$repo/scripts/install-dynhdr-tools.sh" "$dest" 2>&1)" || rc=$?
}
run_install() { rm -rf "$work/dest"; run_install_at "$1" "$work/dest"; }

curl_calls() { printf '%s' "$(( $(wc -l <"$work/curl.log") ))"; }

problems=""
want_rc()   { [ "$rc" -eq "$1" ] || problems+="exited $rc, wanted $1; "; }
want_msg()  { grep -qE -- "$1" <<<"$out" || problems+="message never matched /$1/; "; }
deny_msg()  { grep -qE -- "$1" <<<"$out" && problems+="message wrongly matched /$1/; "; return 0; }
want_calls(){ local n; n="$(curl_calls)"; [ "$n" -eq "$1" ] || problems+="curl was called $n time(s), wanted $1; "; }
want_no_tools() {
  [ ! -e "$dest/bin/dovi_tool" ]      || problems+="a dovi_tool was left in the destination; "
  [ ! -e "$dest/bin/hdr10plus_tool" ] || problems+="an hdr10plus_tool was left in the destination; "
  local leftovers
  leftovers="$(find "$dest" -mindepth 1 2>/dev/null | head -5 || true)"
  [ -z "$leftovers" ] || problems+="destination is not empty ($(tr '\n' ' ' <<<"$leftovers")); "
}
judge() {
  if [ -z "$problems" ]; then
    printf '  ok: %s\n' "$1"; pass=$((pass + 1))
  else
    printf '::error::install-dynhdr-tools selftest: %s - %s\n' "$1" "$problems" >&2
    printf '%s\n' "$out" | sed 's/^/       | /' >&2
    failed=$((failed + 1))
  fi
  problems=""
}

declared=17
[ "$(id -u)" -eq 0 ] || declared=$((declared + 1))

echo "== install-dynhdr-tools selftest (hermetic; no network)"

build_archive "$work/dovi-good.tgz" dovi_tool good
build_archive "$work/h10p-good.tgz" hdr10plus_tool good

# --- 0. THE BASELINE: without it every "must fail" case could be an installer that fails
#        on everything.
DOVI="$work/dovi-good.tgz"; H10P="$work/h10p-good.tgz"; pin "$DOVI" "$H10P"
run_install ok
want_rc 0
want_msg 'sha256 ok'
want_msg "dovi_tool ${DOVI_V} runs"
want_msg "hdr10plus_tool ${H10P_V} runs"
[ -x "$dest/bin/dovi_tool" ]      || problems+="bin/dovi_tool was not installed; "
[ -x "$dest/bin/hdr10plus_tool" ] || problems+="bin/hdr10plus_tool was not installed; "
[ -z "$(find "$dest" -maxdepth 1 -name '.hf-install-stage.*' 2>/dev/null)" ] \
  || problems+="the staging directory survived a SUCCESSFUL install; "
want_calls 2
judge "good archives install both tools, verified and version-checked (baseline)"

# --- 1. The release is GONE: exit 4, names the tool, the version and the URL, no retry.
run_install gone
want_rc 4
want_msg 'EXPIRED'
want_msg "pinned version: ${DOVI_V}"
want_msg 'https://github\.com/quietvoid/dovi_tool/releases/download/'
want_msg 'url attempted'
want_calls 1
want_no_tools
expired_out="$out"
judge "a 404 is reported as an EXPIRED PIN, naming the tool, version and URL, without retrying"

# --- 2. 410 as well as 404.
run_install gone410
want_rc 4
want_msg 'EXPIRED'
want_msg 'HTTP 410'
want_no_tools
judge "a 410 is also reported as an EXPIRED PIN"

# --- 3. The URL is the PINNED release built from the Dockerfile's version, never a
#        floating alias.
problems=""
grep -qE "download/${DOVI_V}/dovi_tool-${DOVI_V}-(x86_64|aarch64)-unknown-linux-musl\.tar\.gz" <<<"$expired_out" \
  || problems+="the reported URL does not name the pinned version and the musl asset; "
grep -qE 'download/latest/|releases/latest' <<<"$expired_out" \
  && problems+="the installer fell back to a floating 'latest' alias; "
out="$expired_out"
judge "the URL attempted is the PINNED release asset, never 'latest'"

# --- 4. A 5xx is transient: bounded retries, then UNREACHABLE, not expired.
run_install http5xx
want_rc 5
want_msg 'could not REACH upstream'
want_msg 'not known to be missing'
deny_msg 'EXPIRED'
want_calls 3
want_no_tools
transient_out="$out"
judge "an upstream 5xx retries a bounded 3 times, then fails as UNREACHABLE, not as expired"

# --- 5. A connection-level failure.
run_install neterr
want_rc 5
want_msg 'could not REACH upstream'
want_msg 'Could not resolve host'
deny_msg 'EXPIRED'
want_calls 3
want_no_tools
judge "a DNS/connection failure retries 3 times, then fails as UNREACHABLE"

# --- 6. Expired and transient are distinguishable by code AND wording.
problems=""
grep -qE 'EXPIRED' <<<"$expired_out"            || problems+="the expired message does not say EXPIRED; "
grep -qE 'could not REACH' <<<"$transient_out"  || problems+="the transient message does not say it could not reach upstream; "
grep -qE 'EXPIRED' <<<"$transient_out"          && problems+="the transient message claims an expiry; "
grep -qE 'could not REACH' <<<"$expired_out"    && problems+="the expired message claims unreachability; "
out="(expired)
$expired_out
(transient)
$transient_out"
judge "expired and transient are distinguishable by exit code (4 vs 5) AND by wording"

# --- 7. The digest gate: 200 with the wrong bytes aborts BEFORE tar, printing both.
cp "$work/dovi-good.tgz" "$work/dovi-other.tgz"
printf 'not the pinned build\n' >>"$work/dovi-other.tgz"
pin "$work/dovi-good.tgz" "$work/h10p-good.tgz"
DOVI="$work/dovi-other.tgz"; H10P="$work/h10p-good.tgz"
run_install ok
want_rc 6
want_msg 'SHA-256 MISMATCH'
want_msg "expected : $(digest_of "$work/dovi-good.tgz")"
want_msg "actual   : $(digest_of "$work/dovi-other.tgz")"
want_msg 'Nothing was extracted'
want_no_tools
judge "a digest mismatch aborts before extracting, printing BOTH the expected and actual digest"

# --- 8. The digest matches but the bytes are not an archive.
printf 'not an archive, but it is the pinned bytes\n' >"$work/junk.bin"
DOVI="$work/junk.bin"; H10P="$work/h10p-good.tgz"; pin "$DOVI" "$H10P"
run_install ok
want_rc 7
want_msg 'could NOT BE UNPACKED'
want_no_tools
judge "a verified archive that will not unpack fails, leaving no partial install"

# --- 9. The archive carries something besides the binary.
build_archive "$work/dovi-extra.tgz" dovi_tool extra-entry
DOVI="$work/dovi-extra.tgz"; H10P="$work/h10p-good.tgz"; pin "$DOVI" "$H10P"
run_install ok
want_rc 7
want_msg "does not hold exactly one entry, 'dovi_tool'"
want_no_tools
judge "an archive holding more than the one binary fails, leaving no partial install"

# --- 10. The binary is there but not executable.
build_archive "$work/h10p-noexec.tgz" hdr10plus_tool not-exec
DOVI="$work/dovi-good.tgz"; H10P="$work/h10p-noexec.tgz"; pin "$DOVI" "$H10P"
run_install ok
want_rc 7
want_msg "'hdr10plus_tool' is NOT EXECUTABLE"
want_no_tools
judge "a binary that is not executable fails, and the tool verified before it is NOT published"

# --- 11. A binary that runs but is not the pinned version never becomes the tool on disk.
build_archive "$work/dovi-wrongver.tgz" dovi_tool wrong-version
DOVI="$work/dovi-wrongver.tgz"; H10P="$work/h10p-good.tgz"; pin "$DOVI" "$H10P"
run_install ok
want_rc 8
want_msg "reports 'dovi_tool 0\.0\.1', not the pinned 'dovi_tool ${DOVI_V}'"
want_no_tools
judge "a verified binary that reports another version is NOT installed"

# --- 12. A binary that does not run here (the wrong architecture, say).
build_archive "$work/h10p-broken.tgz" hdr10plus_tool broken
DOVI="$work/dovi-good.tgz"; H10P="$work/h10p-broken.tgz"; pin "$DOVI" "$H10P"
run_install ok
want_rc 8
want_msg 'the staged hdr10plus_tool does not run here'
want_no_tools
judge "a verified binary that does not run here is NOT installed"

# --- 13. BOTH OR NEITHER: dovi_tool fetches and verifies, then hdr10plus_tool is gone.
#         A half-install would leave the RPU tool beside no HDR10+ tool, so it must leave
#         neither.
DOVI="$work/dovi-good.tgz"; H10P="$work/h10p-good.tgz"; pin "$DOVI" "$H10P"
GONE_FOR=hdr10plus_tool run_install ok
want_rc 4
want_msg 'pinned hdr10plus_tool release has EXPIRED'
want_msg "dovi_tool ${DOVI_V} runs"
want_calls 2
want_no_tools
judge "when the second tool fails after the first verified, NEITHER is installed"

# --- 14. An uncreatable destination: nothing is fetched.
printf 'i am a file\n' >"$work/blocker"
run_install_at ok "$work/blocker/tools"
want_rc 3
want_msg 'could not be created'
want_msg 'Nothing was downloaded'
want_msg "$work/blocker/tools"
want_calls 0
judge "an uncreatable destination fails naming the path, and downloads nothing"

# --- 15. The destination is a regular file.
printf 'i am a file\n' >"$work/dest-is-a-file"
run_install_at ok "$work/dest-is-a-file"
want_rc 3
want_msg 'Nothing was downloaded'
want_calls 0
judge "a destination that is a file, not a directory, fails before downloading anything"

# --- 16. An unwritable destination; only expressible as non-root, and said so out loud.
if [ "$(id -u)" -ne 0 ]; then
  ro="$work/readonly"; rm -rf "$ro"; mkdir -p "$ro"; chmod 0555 "$ro"
  run_install_at ok "$ro"
  want_rc 3
  want_msg 'not writable'
  want_msg 'Nothing was downloaded'
  want_calls 0
  chmod 0755 "$ro"
  judge "an unwritable destination fails before downloading anything"
else
  echo "  note: running as uid 0, so the unwritable-destination case is not expressible (root ignores DAC); it is not counted as declared"
fi

# --- 17. Through a failure, with a package manager, cargo and system copies of both tools
#         on PATH, NOTHING is substituted.
run_install gone
want_rc 4
want_no_tools
if [ -s "$work/decoy.log" ]; then
  problems+="the installer invoked a package manager or a system tool: $(tr '\n' ' ' <"$work/decoy.log"); "
fi
judge "no package manager, no cargo and no system copy of either tool is reached for when the pin fails"

echo
total=$((pass + failed))
if [ "$total" -ne "$declared" ]; then
  echo "::error::install-dynhdr-tools selftest: ran $total case(s), expected $declared - a case did not execute" >&2
  exit 1
fi
if [ "$failed" -ne 0 ]; then
  echo "::error::install-dynhdr-tools selftest: $failed of $declared case(s) did not bite - the pinned dynamic-HDR tool installer is not trustworthy" >&2
  exit 1
fi
echo "install-dynhdr-tools selftest: $pass/$declared cases bite"
