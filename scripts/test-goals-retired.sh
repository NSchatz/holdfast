#!/usr/bin/env bash
# test_docs_name_no_live_goals: CLAUDE.md and CARD.md name no live goals command, and
# CARD.md hands work over with `harness plan add holdfast`. Run directly; not part of the gate.
set -euo pipefail
cd "$(dirname "$0")/.."
re="goals (task|item|project) (add|list|show|edit|done|recur)|goals serve|goals'? nightly|\.local/bin/goals|/goals:|goals'? app|goals' (Approve|weekly|start)|harness: .goals."
if grep -nE "$re" CLAUDE.md CARD.md; then
  echo "FAIL: live goals text remains" >&2
  exit 1
fi
grep -q 'harness plan add holdfast' CARD.md || { echo "FAIL: CARD.md lacks harness plan add holdfast" >&2; exit 1; }
echo "ok: test_docs_name_no_live_goals"
