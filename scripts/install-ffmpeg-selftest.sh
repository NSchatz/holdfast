#!/usr/bin/env bash
# Prove scripts/install-ffmpeg.sh FAILS THE WAY IT SAYS IT DOES. Part of `make check`.
#
# It exists because S0022 was not caused by the installer being wrong. The installer did
# exactly what it was told: it fetched a URL that had stopped existing and reported curl's
# exit code. What cost days was that the report said nothing - no build tag, no URL, no
# "this pin has expired", nothing to separate a dead release from a bad afternoon on the
# network. Three unrelated specs sat blocked while people read a red step that could not
# tell them which of those two things had happened.
#
# So the installer now claims a lot: five distinct failure modes, each with its own exit
# code, each naming the pin and the URL, and every one of them leaving NO ffmpeg behind.
# Claims in a shell script rot silently. This defeats each one on purpose, on every run.
#
# It is HERMETIC - no network. `curl` is replaced by a fake on PATH that returns whatever
# status the case needs, and the pin is driven by rewriting the FFMPEG_SHA256_* ARGs of a
# THROWAWAY COPY of the real Dockerfile, so the real digest gate, the real tar, and the
# real sha256sum all run for real against fixtures we control. Nothing in the working
# tree is mutated and nothing is downloaded.
#
# The load-bearing assertion in nearly every case is not the exit code, it is the two
# lines after it: the destination is EMPTY, and the message says which failure this was.
# An installer that fails loudly but leaves a half-unpacked tree behind has handed the
# next step something to mistake for an ffmpeg - and this ffmpeg is not just the encoder,
# it is the libvmaf instrument that decides whether a source may be deleted.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

for tool in tar xz sha256sum sed; do
  command -v "$tool" >/dev/null 2>&1 \
    || { echo "::error::install-ffmpeg selftest needs '$tool' and it is not on PATH - it did NOT run" >&2; exit 1; }
done

work="$(mktemp -d)" || { echo "::error::install-ffmpeg selftest: mktemp failed" >&2; exit 1; }
trap 'chmod -R u+w "$work" 2>/dev/null || true; rm -rf "$work"' EXIT

repo="$work/repo"
fakebin="$work/fakebin"
mkdir -p "$repo/scripts" "$fakebin"

# The REAL Dockerfile (so the real ARG parsing is exercised) and the WORKING-TREE
# installer (so an uncommitted change is graded, not HEAD - the mistake gets made
# locally, which is exactly where HEAD would hide it).
cp "$here/Dockerfile" "$repo/Dockerfile"
cp "$here/scripts/install-ffmpeg.sh" "$repo/scripts/install-ffmpeg.sh"
cmp -s "$here/scripts/install-ffmpeg.sh" "$repo/scripts/install-ffmpeg.sh" \
  || { echo "::error::selftest: the sandbox installer is not the working-tree installer - it graded the wrong thing" >&2; exit 1; }

# --- the fake curl -------------------------------------------------------------------
# Honours the real invocation shape: writes the body to the -o path and prints the HTTP
# status on stdout for -w '%{http_code}'. Every call is logged, so "did it retry?" and
# "did it fetch anything at all?" are assertable facts rather than a reading of the code.
cat >"$fakebin/curl" <<'FAKE'
#!/usr/bin/env bash
set -u
printf 'call\n' >> "${FAKE_CURL_LOG:-/dev/null}"
out=""; prev=""
for a in "$@"; do
  [ "$prev" = "-o" ] && out="$a"
  prev="$a"
done
case "${FAKE_CURL_MODE:-ok}" in
  ok)     [ -n "$out" ] && cp "$FAKE_CURL_PAYLOAD" "$out"; printf '200' ;;
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

# --- decoys: things a "just make CI green" fix would reach for ------------------------
# The installer must never touch a package manager or a system ffmpeg. These record any
# invocation, and case 15 asserts the logs stay empty through a failure.
for decoy in apt-get apk dnf yum brew ffmpeg; do
  cat >"$fakebin/$decoy" <<DECOY
#!/usr/bin/env bash
printf '%s %s\n' "$decoy" "\$*" >> "\${DECOY_LOG:-/dev/null}"
exit 0
DECOY
  chmod +x "$fakebin/$decoy"
done

# --- fixture archives -----------------------------------------------------------------
# variant: good | no-bin | not-exec | no-libvmaf
build_archive() {
  local out="$1" variant="$2" src="$work/pkgsrc"
  rm -rf "$src"; mkdir -p "$src/pkg/bin" "$src/pkg/doc"
  printf 'fixture\n' >"$src/pkg/doc/readme.txt"
  if [ "$variant" = "no-bin" ]; then
    rm -rf "$src/pkg/bin"
  else
    cat >"$src/pkg/bin/ffmpeg" <<'SH'
#!/bin/sh
case "$*" in
  *-filters*)  echo " TS. libvmaf           VV->V      Calculate the VMAF score." ;;
  *-encoders*) echo " V....D libx265               libx265 H.265 / HEVC" ;;
esac
exit 0
SH
    if [ "$variant" = "no-libvmaf" ]; then
      cat >"$src/pkg/bin/ffmpeg" <<'SH'
#!/bin/sh
case "$*" in
  *-filters*)  echo " TS. ssim              VV->V      Calculate the SSIM." ;;
  *-encoders*) echo " V....D libx265               libx265 H.265 / HEVC" ;;
esac
exit 0
SH
    fi
    printf '#!/bin/sh\nexit 0\n' >"$src/pkg/bin/ffprobe"
    chmod +x "$src/pkg/bin/ffmpeg" "$src/pkg/bin/ffprobe"
    [ "$variant" = "not-exec" ] && chmod 0644 "$src/pkg/bin/ffmpeg"
  fi
  tar -C "$src" -cJf "$out" pkg
}

# Rewrite BOTH digests in the sandbox Dockerfile (which one the installer reads depends
# on uname -m, and this must behave identically on an x86_64 runner and an arm64 laptop).
pin_digest() {
  sed -i -e "s|^ARG FFMPEG_SHA256_AMD64=.*|ARG FFMPEG_SHA256_AMD64=$1|" \
         -e "s|^ARG FFMPEG_SHA256_ARM64=.*|ARG FFMPEG_SHA256_ARM64=$1|" "$repo/Dockerfile"
}
digest_of() { sha256sum "$1" | awk '{print $1}'; }

# The real pin, read from the real Dockerfile, so the message assertions below check the
# installer prints the ACTUAL pin rather than any plausible-looking string.
PIN_BUILD="$(sed -n 's/^ARG FFMPEG_BUILD=\(.*\)$/\1/p' "$here/Dockerfile" | head -1)"
PIN_VERSION="$(sed -n 's/^ARG FFMPEG_VERSION=\(.*\)$/\1/p' "$here/Dockerfile" | head -1)"
[ -n "$PIN_BUILD" ] && [ -n "$PIN_VERSION" ] \
  || { echo "::error::selftest could not read the pin from the Dockerfile - it did NOT run" >&2; exit 1; }

# --- harness ---------------------------------------------------------------------------
pass=0; failed=0
out=""; rc=0; dest=""

run_install() {  # <mode> [dest-name]
  local mode="$1" name="${2:-dest}"
  dest="$work/$name"
  rm -rf "$dest"
  : >"$work/curl.log"
  : >"$work/decoy.log"
  rc=0
  out="$(PATH="$fakebin:$PATH" \
         FAKE_CURL_MODE="$mode" \
         FAKE_CURL_PAYLOAD="${PAYLOAD:-/dev/null}" \
         FAKE_CURL_LOG="$work/curl.log" \
         DECOY_LOG="$work/decoy.log" \
         bash "$repo/scripts/install-ffmpeg.sh" "$dest" 2>&1)" || rc=$?
}

run_install_at() {  # <mode> <absolute dest path>
  local mode="$1"
  dest="$2"
  : >"$work/curl.log"
  : >"$work/decoy.log"
  rc=0
  out="$(PATH="$fakebin:$PATH" \
         FAKE_CURL_MODE="$mode" \
         FAKE_CURL_PAYLOAD="${PAYLOAD:-/dev/null}" \
         FAKE_CURL_LOG="$work/curl.log" \
         DECOY_LOG="$work/decoy.log" \
         bash "$repo/scripts/install-ffmpeg.sh" "$dest" 2>&1)" || rc=$?
}

curl_calls() { printf '%s' "$(( $(wc -l <"$work/curl.log") ))"; }

problems=""
want_rc()   { [ "$rc" -eq "$1" ] || problems+="exited $rc, wanted $1; "; }
want_msg()  { grep -qE -- "$1" <<<"$out" || problems+="message never matched /$1/; "; }
deny_msg()  { grep -qE -- "$1" <<<"$out" && problems+="message wrongly matched /$1/; "; return 0; }
want_calls(){ local n; n="$(curl_calls)"; [ "$n" -eq "$1" ] || problems+="curl was called $n time(s), wanted $1; "; }
want_no_ffmpeg() {
  [ ! -e "$dest/bin/ffmpeg" ]  || problems+="an ffmpeg was left in the destination; "
  [ ! -e "$dest/bin/ffprobe" ] || problems+="an ffprobe was left in the destination; "
  local leftovers
  leftovers="$(find "$dest" -mindepth 1 2>/dev/null | head -5 || true)"
  [ -z "$leftovers" ] || problems+="destination is not empty ($(tr '\n' ' ' <<<"$leftovers")); "
}
judge() {  # <case name>
  if [ -z "$problems" ]; then
    printf '  ok: %s\n' "$1"; pass=$((pass + 1))
  else
    printf '::error::install-ffmpeg selftest: %s - %s\n' "$1" "$problems" >&2
    printf '%s\n' "$out" | sed 's/^/       | /' >&2
    failed=$((failed + 1))
  fi
  problems=""
}

declared=15
[ "$(id -u)" -eq 0 ] || declared=$((declared + 1))

echo "== install-ffmpeg selftest (hermetic; no network)"

# --- 0. THE BASELINE. Without it every "must fail" case below could be satisfied by an
#        installer that fails on absolutely everything, which would prove nothing.
build_archive "$work/good.tar.xz" good
PAYLOAD="$work/good.tar.xz"; pin_digest "$(digest_of "$PAYLOAD")"
run_install ok
want_rc 0
want_msg 'sha256 ok'
want_msg 'libx265 \+ libvmaf present'
[ -x "$dest/bin/ffmpeg" ]  || problems+="bin/ffmpeg was not installed; "
[ -x "$dest/bin/ffprobe" ] || problems+="bin/ffprobe was not installed; "
[ -z "$(find "$dest" -maxdepth 1 -name '.hf-install-stage.*' 2>/dev/null)" ] \
  || problems+="the staging directory survived a SUCCESSFUL install; "
judge "a good archive installs, verifies, and reports libx265 + libvmaf (baseline)"

# --- 1. Criterion 3: the release is GONE. Distinct exit code, names the tag and the URL,
#        says the pin has EXPIRED, and does not retry - a 404 will not become a 200.
run_install gone
want_rc 4
want_msg 'EXPIRED'
want_msg "$PIN_BUILD"
want_msg 'https://github\.com/BtbN/FFmpeg-Builds/releases/download/'
want_msg 'url attempted'
want_calls 1
want_no_ffmpeg
expired_out="$out"
judge "a 404 is reported as an EXPIRED PIN, naming the tag and the URL, without retrying"

# --- 2. Criterion 3 names 410 as well as 404, so 410 is proven too rather than assumed.
run_install gone410
want_rc 4
want_msg 'EXPIRED'
want_msg 'HTTP 410'
want_no_ffmpeg
judge "a 410 is also reported as an EXPIRED PIN"

# --- 3. Criterion 8: no floating alias, even in the error path. The URL the installer
#        reports must be the PINNED tag - a fallback to 'latest' would make the failure
#        disappear and take the meaning of the pin with it.
problems=""
grep -qE "download/${PIN_BUILD}/ffmpeg-${PIN_VERSION}-" <<<"$expired_out" \
  || problems+="the reported URL does not name the pinned build and version; "
grep -qE 'download/latest/' <<<"$expired_out" \
  && problems+="the installer fell back to the floating 'latest' alias; "
out="$expired_out"
judge "the URL attempted is the PINNED tag, never the floating 'latest' alias"

# --- 4. Criterion 6: a 5xx is transient. Bounded retries, then a message that says it
#        could not REACH upstream and explicitly is not an expiry.
run_install http5xx
want_rc 5
want_msg 'could not REACH upstream'
want_msg 'not known to be missing'
deny_msg 'EXPIRED'
want_calls 3
want_no_ffmpeg
transient_out="$out"
judge "an upstream 5xx retries a bounded 3 times, then fails as UNREACHABLE, not as expired"

# --- 5. Criterion 6: the same for a connection-level failure (curl exit 6, no status).
run_install neterr
want_rc 5
want_msg 'could not REACH upstream'
want_msg 'Could not resolve host'
deny_msg 'EXPIRED'
want_calls 3
want_no_ffmpeg
judge "a DNS/connection failure retries 3 times, then fails as UNREACHABLE"

# --- 6. Criteria 3 and 6 together: the two are DISTINGUISHABLE. This is the whole point
#        of the incident - a red step that cannot tell you which of these happened is the
#        thing that cost three specs a week.
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

# --- 7. Criterion 4: the digest gate. 200 with the wrong bytes must abort BEFORE tar
#        runs, and must print both digests so the reader can tell which is wrong.
build_archive "$work/other.tar.xz" good
printf 'this is not the pinned build\n' >>"$work/other.tar.xz"
PAYLOAD="$work/other.tar.xz"
pin_digest "$(digest_of "$work/good.tar.xz")"
run_install ok
want_rc 6
want_msg 'SHA-256 MISMATCH'
want_msg "expected : $(digest_of "$work/good.tar.xz")"
want_msg "actual   : $(digest_of "$work/other.tar.xz")"
want_msg 'Nothing was extracted'
want_no_ffmpeg
judge "a digest mismatch aborts before extracting, printing BOTH the expected and actual digest"

# --- 8. Criterion 5: the digest matches but the bytes are not an archive at all.
printf 'not an archive, but it is the pinned bytes\n' >"$work/junk.bin"
PAYLOAD="$work/junk.bin"; pin_digest "$(digest_of "$PAYLOAD")"
run_install ok
want_rc 7
want_msg 'could NOT BE UNPACKED'
want_no_ffmpeg
judge "a verified archive that will not unpack fails, leaving no partial install"

# --- 9. Criterion 5: it unpacks, but there is no bin/ffmpeg in it.
build_archive "$work/nobin.tar.xz" no-bin
PAYLOAD="$work/nobin.tar.xz"; pin_digest "$(digest_of "$PAYLOAD")"
run_install ok
want_rc 7
want_msg "bin/ffmpeg' is MISSING"
want_no_ffmpeg
judge "an archive missing bin/ffmpeg fails, leaving no partial install"

# --- 10. Criterion 5: bin/ffmpeg is there but is not executable.
build_archive "$work/noexec.tar.xz" not-exec
PAYLOAD="$work/noexec.tar.xz"; pin_digest "$(digest_of "$PAYLOAD")"
run_install ok
want_rc 7
want_msg "bin/ffmpeg' is NOT EXECUTABLE"
want_no_ffmpeg
judge "an archive whose bin/ffmpeg is not executable fails, leaving no partial install"

# --- 11. Criterion 8: a build that fetches and verifies but lacks libvmaf never becomes
#         the thing on disk. libvmaf is the instrument the no-loss verdict is measured
#         with; a silently installed build without it is the one substitution that would
#         look like success right up until an unmeasured output.
build_archive "$work/novmaf.tar.xz" no-libvmaf
PAYLOAD="$work/novmaf.tar.xz"; pin_digest "$(digest_of "$PAYLOAD")"
run_install ok
want_rc 8
want_msg 'lacks libvmaf'
want_no_ffmpeg
judge "a verified build that lacks libvmaf is NOT installed (checked while still staged)"

# --- 12. Criterion 7: the destination cannot be created (its parent is a file). Nothing
#         is fetched - the zero-call assertion is the "SHALL NOT download" half.
PAYLOAD="$work/good.tar.xz"; pin_digest "$(digest_of "$PAYLOAD")"
printf 'i am a file\n' >"$work/blocker"
run_install_at ok "$work/blocker/ffmpeg"
want_rc 3
want_msg 'could not be created'
want_msg 'Nothing was downloaded'
want_msg "$work/blocker/ffmpeg"
want_calls 0
judge "an uncreatable destination fails naming the path, and downloads nothing"

# --- 13. Criterion 7: the destination path exists but is a regular file.
printf 'i am a file\n' >"$work/dest-is-a-file"
run_install_at ok "$work/dest-is-a-file"
want_rc 3
want_msg 'Nothing was downloaded'
want_calls 0
judge "a destination that is a file, not a directory, fails before downloading anything"

# --- 14. Criterion 7 (write half): the directory exists but cannot be written. Only
#         expressible as a non-root user - root ignores the permission bits - so it is
#         declared conditionally and its absence is stated out loud rather than skipped
#         quietly into a green tally.
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

# --- 15. Criterion 8, stated directly: through a failure, with a package manager and a
#         system ffmpeg sitting right there on PATH, NOTHING is substituted.
run_install gone
want_rc 4
want_no_ffmpeg
if [ -s "$work/decoy.log" ]; then
  problems+="the installer invoked a package manager or a system ffmpeg: $(tr '\n' ' ' <"$work/decoy.log"); "
fi
judge "no distro package and no system ffmpeg is ever reached for when the pin fails"

echo
# Report against the number of cases DECLARED, not the number that ran: "$pass/$pass" is
# N/N by construction and could never show a shortfall.
total=$((pass + failed))
if [ "$total" -ne "$declared" ]; then
  echo "::error::install-ffmpeg selftest: ran $total case(s), expected $declared - a case did not execute" >&2
  exit 1
fi
if [ "$failed" -ne 0 ]; then
  echo "::error::install-ffmpeg selftest: $failed of $declared case(s) did not bite - the pinned-ffmpeg installer is not trustworthy" >&2
  exit 1
fi
echo "install-ffmpeg selftest: $pass/$declared cases bite"
