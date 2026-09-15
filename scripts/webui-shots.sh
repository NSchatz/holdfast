#!/usr/bin/env bash
# The rendered evidence `frontend` F12 obliges, taken by a command anyone can re-run.
#
# WHY IT EXISTS. The craft clauses - interface-craft C1 to C7 - are graded against pictures
# of the page, and until now each session that needed them wrote its own script to take
# them. Evidence produced by a command that no longer exists is evidence nobody can check
# or reproduce, which is the same defect a grader that skips has. This is the one command.
#
# WHAT IT PHOTOGRAPHS. Every view internal/webui/e2e/shots.mjs names, in both colour
# schemes at 360 and at a desktop width, against the fixture server this script starts -
# which mounts the real webui.HandlerFor, so these are pictures of the document
# `holdfast serve` produces rather than of a copy assembled for the occasion.
#
# It DECIDES NOTHING and it is not a gate: it writes files, and a gate never does.
#
#   make webui-shots OUT=/path/to/shots
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="${1:-$here/shots}"
case "$out" in
  /*) ;;
  *) out="$PWD/$out" ;;
esac

e2e="$here/internal/webui/e2e"
if [ ! -d "$e2e/node_modules/@playwright/test" ]; then
  echo "::error::webui-shots: the runner is not installed - run \`npm ci\` in internal/webui/e2e" >&2
  exit 1
fi

# The engine, resolved exactly once and by the repository's own resolver, which PROVES a
# candidate renders rather than trusting a name on PATH. shots.mjs never searches for one.
browser="$("$here/scripts/find-browser.sh")"
export HOLDFAST_BROWSER="$browser"

# A port nothing is listening on, chosen the same way the runner's config chooses one: a
# fixed port makes every run's outcome depend on what else is on the machine, and adopting
# a server this script did not start would photograph a page built from a tree nobody can
# name.
port="$(node -e 'const s=require("node:net").createServer();s.listen(0,"127.0.0.1",()=>{process.stdout.write(String(s.address().port));s.close();});')"

cd "$e2e"
go run ./fixtureserver -addr "127.0.0.1:$port" -fixtures ./fixtures &
server=$!
trap 'kill "$server" 2>/dev/null || true; wait "$server" 2>/dev/null || true' EXIT

# Wait for it to answer rather than for a clock.
for _ in $(seq 1 100); do
  if curl -fsS -o /dev/null "http://127.0.0.1:$port/" 2>/dev/null; then break; fi
  sleep 0.2
done
if ! curl -fsS -o /dev/null "http://127.0.0.1:$port/" 2>/dev/null; then
  echo "::error::webui-shots: the fixture server never answered on 127.0.0.1:$port" >&2
  exit 1
fi

mkdir -p "$out"
node shots.mjs "http://127.0.0.1:$port" "$out" "$browser"
echo "webui-shots: OK - the evidence is in $out"
