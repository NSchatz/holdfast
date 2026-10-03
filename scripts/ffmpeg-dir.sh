#!/usr/bin/env bash
# Print the directory of the REAL ffmpeg and ffprobe that `ffmpeg` and `ffprobe` on PATH
# run, when what PATH finds first is a version manager's shim; print nothing otherwise.
# `make test` puts a printed directory first on PATH for the suite, and leaves PATH exactly
# as it is when nothing is printed.
#
# Why: a shim (for example mise's, a link to the `mise` binary) starts a dispatcher on
# EVERY call before it execs the real binary. The suite calls ffmpeg and ffprobe thousands
# of times, one after another, and measured at roughly 30 ms of dispatcher per call on the
# gate's host that is several minutes of the engine suite spent starting a version manager.
#
# This chooses no ffmpeg of its own. A candidate later on PATH is used only when its whole
# `-version` output, and its ffprobe's, are byte-identical to what the shim itself runs, so
# the suite runs the same binaries it would have run through the shim. When there is no
# ffmpeg on PATH at all, or nothing qualifies, it prints nothing and PATH is untouched: the
# suite then fails loudly on the missing tool exactly as before (a grader that skips is a
# false green), so this can make the gate faster and can never make it skip.
#
# Exit status is 0 in every case; the output is the answer.
set -uo pipefail

real_name() { basename -- "$(readlink -f -- "$1" 2>/dev/null)" 2>/dev/null; }

first_ffmpeg="$(command -v ffmpeg 2>/dev/null)" || exit 0
first_ffprobe="$(command -v ffprobe 2>/dev/null)" || exit 0
case "$first_ffmpeg" in /*) ;; *) exit 0 ;; esac
case "$first_ffprobe" in /*) ;; *) exit 0 ;; esac

# Already the binaries themselves: nothing to do.
if [ "$(real_name "$first_ffmpeg")" = ffmpeg ] && [ "$(real_name "$first_ffprobe")" = ffprobe ]; then
  exit 0
fi

# What the shim actually runs, asked through the shim once.
want_ffmpeg="$(ffmpeg -hide_banner -version 2>/dev/null)" || exit 0
want_ffprobe="$(ffprobe -hide_banner -version 2>/dev/null)" || exit 0
[ -n "$want_ffmpeg" ] && [ -n "$want_ffprobe" ] || exit 0

IFS=:
for d in $PATH; do
  [ -n "$d" ] || continue
  f="$(readlink -f -- "$d/ffmpeg" 2>/dev/null)" || continue
  p="$(readlink -f -- "$d/ffprobe" 2>/dev/null)" || continue
  [ -f "$f" ] && [ -x "$f" ] && [ -f "$p" ] && [ -x "$p" ] || continue
  [ "$(basename -- "$f")" = ffmpeg ] && [ "$(basename -- "$p")" = ffprobe ] || continue
  dir="$(dirname -- "$f")"
  [ "$(dirname -- "$p")" = "$dir" ] || continue
  [ "$("$f" -hide_banner -version 2>/dev/null)" = "$want_ffmpeg" ] || continue
  [ "$("$p" -hide_banner -version 2>/dev/null)" = "$want_ffprobe" ] || continue
  printf '%s\n' "$dir"
  exit 0
done
exit 0
