#!/usr/bin/env bash
# Prove the HTTP-surface diff gate still BITES. NOT part of `make check` - it is the thing
# that grades the gate `check` runs, and a gate that graded itself would be one nobody could
# trust either way.
#
# The surface gate is the guard whose failure is most completely invisible: a tree whose
# surface is unchanged and a tree whose baseline could not be read print the same word. So
# every way it can answer wrongly is defeated on purpose here, on every run:
#
#   AC-7   an ADDITIVE difference - a new endpoint, a new field, a new status code - must
#          exit ZERO. A gate that froze the surface would be reverted within a week and
#          would then be protecting nothing.
#   AC-8   a BREAKING difference must exit non-zero and name EVERY break it found, not the
#          first: a gate that stopped at one would have to be run once per break.
#   AC-9   a baseline that is missing, empty or unparseable must exit non-zero saying WHICH,
#          and must never report the surface unchanged. A comparison that could not run has
#          not passed.
#   AC-10  a break is non-fatal only where a record names that EXACT difference and the
#          version carrying it; a break nothing names fails, and a record matching nothing
#          in the tree fails too, so the file cannot survive as a blanket switch.
#
# EVERY MUTATION HAPPENS INSIDE A THROWAWAY CLONE. The tree `make check` grades is read and
# never written, and the last case asserts exactly that by comparing the two files this
# script could have touched before and after.
#
# The mutator is COMPOSED AT RUNTIME and never committed: a program whose only purpose is to
# corrupt the baseline has no business sitting in the tree the gate grades.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d)" || { echo "::error::selftest: mktemp failed" >&2; exit 1; }
trap 'rm -rf "$work"' EXIT

declared=16
pass=0; failed=0
repo="$work/repo"
baseline="docs/api-schema.json"
record=".api-schema-breaks.yaml"

# What the graded tree looks like BEFORE anything runs. The last case compares it again.
before="$(cksum "$here/$baseline" "$here/$record" 2>/dev/null || true)"

git clone -q --no-hardlinks --depth 1 "file://$here" "$repo" 2>/dev/null \
  || { echo "::error::selftest could not clone the repo - it did NOT run" >&2; exit 1; }

# Grade the tree AS IT STANDS NOW: the clone carries HEAD, so the whole working tree is
# overlaid on top of it. Without this an uncommitted change - a fix OR a break - would be
# graded as if it did not exist, and locally is where that mistake gets made.
tar -C "$here" --exclude=.git --exclude=node_modules -cf - . | tar -C "$repo" -xf - \
  || { echo "::error::selftest: could not overlay the working tree - it did NOT run" >&2; exit 1; }

cmp -s "$here/scripts/api-schema/main.go" "$repo/scripts/api-schema/main.go" \
  || { echo "::error::selftest: the clone's gate is not the working-tree gate - it graded the wrong thing" >&2; exit 1; }

# --- the mutator, composed here and built inside the clone ---------------------------
# It edits a baseline document through the SAME types the gate reads it with, so a mutation
# cannot produce a shape the gate would reject for an unrelated reason.
mkdir -p "$repo/scripts/api-schema-selftest-mutate"
cat > "$repo/scripts/api-schema-selftest-mutate/main.go" <<'MUTATOR'
// Command api-schema-selftest-mutate edits a surface document. Composed at runtime by
// scripts/api-schema-diff-selftest.sh and never committed.
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/NSchatz/holdfast/internal/server"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: mutate <file> <op> [args...]")
		os.Exit(2)
	}
	path, op, args := os.Args[1], os.Args[2], os.Args[3:]
	raw, err := os.ReadFile(path)
	die(err)
	doc, err := server.ParseDocument(raw)
	die(err)

	arg := func(i int) string {
		if i >= len(args) {
			fmt.Fprintf(os.Stderr, "mutate %s: missing argument %d\n", op, i)
			os.Exit(2)
		}
		return args[i]
	}
	num := func(i int) int {
		n, err := strconv.Atoi(arg(i))
		die(err)
		return n
	}

	changed := false
	switch op {
	case "drop-endpoint":
		kept := doc.Endpoints[:0]
		for _, ep := range doc.Endpoints {
			if ep.Method == arg(0) && ep.Path == arg(1) {
				changed = true
				continue
			}
			kept = append(kept, ep)
		}
		doc.Endpoints = kept
	case "add-endpoint":
		doc.Endpoints = append(doc.Endpoints, server.Endpoint{
			Method: arg(0), Path: arg(1),
			Responses: []server.Response{{Status: 200, MediaType: "text/plain; charset=utf-8",
				Body: server.Shape{Kind: "text"}}},
		})
		changed = true
	case "drop-status":
		for i := range doc.Endpoints {
			ep := &doc.Endpoints[i]
			if ep.Method != arg(0) || ep.Path != arg(1) {
				continue
			}
			kept := ep.Responses[:0]
			for _, r := range ep.Responses {
				if r.Status == num(2) {
					changed = true
					continue
				}
				kept = append(kept, r)
			}
			ep.Responses = kept
		}
	case "add-status":
		for i := range doc.Endpoints {
			ep := &doc.Endpoints[i]
			if ep.Method != arg(0) || ep.Path != arg(1) {
				continue
			}
			ep.Responses = append(ep.Responses, server.Response{Status: num(2),
				MediaType: "text/plain; charset=utf-8", Body: server.Shape{Kind: "text"}})
			changed = true
		}
	case "drop-field":
		forResponse(&doc, arg(0), arg(1), num(2), func(r *server.Response) {
			kept := r.Body.Fields[:0]
			for _, f := range r.Body.Fields {
				if f.Name == arg(3) {
					changed = true
					continue
				}
				kept = append(kept, f)
			}
			r.Body.Fields = kept
		})
	case "add-field":
		forResponse(&doc, arg(0), arg(1), num(2), func(r *server.Response) {
			r.Body.Fields = append(r.Body.Fields, server.Field{
				Name: arg(3), Required: true, Type: server.Shape{Kind: "string"}})
			changed = true
		})
	case "retype-field":
		forResponse(&doc, arg(0), arg(1), num(2), func(r *server.Response) {
			for i := range r.Body.Fields {
				if r.Body.Fields[i].Name == arg(3) {
					r.Body.Fields[i].Type = server.Shape{Kind: arg(4)}
					changed = true
				}
			}
		})
	case "set-media":
		forResponse(&doc, arg(0), arg(1), num(2), func(r *server.Response) {
			r.MediaType = arg(3)
			changed = true
		})
	default:
		fmt.Fprintf(os.Stderr, "mutate: unknown op %q\n", op)
		os.Exit(2)
	}
	if !changed {
		fmt.Fprintf(os.Stderr, "mutate %s: changed nothing, so the case would have proved nothing\n", op)
		os.Exit(3)
	}
	out, err := server.MarshalDocument(doc)
	die(err)
	die(os.WriteFile(path, out, 0o644))
}

func forResponse(doc *server.Document, method, path string, status int, fn func(*server.Response)) {
	for i := range doc.Endpoints {
		ep := &doc.Endpoints[i]
		if ep.Method != method || ep.Path != path {
			continue
		}
		for j := range ep.Responses {
			if ep.Responses[j].Status == status {
				fn(&ep.Responses[j])
			}
		}
	}
}

func die(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "mutate:", err)
		os.Exit(2)
	}
}
MUTATOR

gate="$work/api-schema"
mutate="$work/mutate"
( cd "$repo" && go build -o "$gate" ./scripts/api-schema ) \
  || { echo "::error::selftest: the gate did not build - it did NOT run" >&2; exit 1; }
( cd "$repo" && go build -o "$mutate" ./scripts/api-schema-selftest-mutate ) \
  || { echo "::error::selftest: the mutator did not build - it did NOT run" >&2; exit 1; }

cp "$repo/$baseline" "$work/pristine.json" \
  || { echo "::error::selftest: the clone carries no baseline - it did NOT run" >&2; exit 1; }

# reset puts the clone back to a tree whose surface matches its baseline exactly and whose
# record file is empty, so each case starts from the state `make check` sees.
reset() {
  cp "$work/pristine.json" "$repo/$baseline"
  printf '[]\n' > "$repo/$record"
}

mut() { "$mutate" "$repo/$baseline" "$@" >/dev/null; }

out=""
run_gate() { out="$( "$gate" diff -root "$repo" 2>&1 )"; }

# expect <want-exit> <criterion> <name> [must-mention-regex...]
# The output is CAPTURED, not discarded: once a gate has more than one reason to exit
# non-zero, "it exited 1" stops being evidence that the case under test is the one that bit.
expect() {
  local want="$1" criterion="$2" name="$3"; shift 3
  local got=0
  set +e
  run_gate
  got=$?
  set -e
  local why=""
  if [ "$got" -ne "$want" ]; then
    if [ "$want" -ne 0 ]; then
      why="the gate CAME BACK GREEN (exit $got) where it had to bite"
      [ "$got" -eq 0 ] || why="the gate exited $got, wanted $want"
    else
      why="the gate exited $got, wanted $want"
    fi
  fi
  local missing=""
  for re in "$@"; do
    printf '%s' "$out" | grep -qE -- "$re" || missing="$missing [$re]"
  done
  [ -z "$missing" ] || why="$why; output never mentions$missing"
  if [ -z "$why" ]; then
    printf '  ok  %s defeated: %s\n' "$criterion" "$name"
    pass=$((pass + 1))
  else
    printf '::error::selftest %s (%s): %s\n' "$criterion" "$name" "$why" >&2
    printf '%s\n' "$out" | sed 's/^/        /' >&2
    failed=$((failed + 1))
  fi
}

echo "api-schema diff gate selftest: defeating AC-7, AC-8, AC-9 and AC-10 on purpose"
echo

# --- 1. the unmutated clone passes, or every case below is graded against a red tree ---
reset
expect 0 "AC-7" "an unmutated tree matches its own baseline" 'no difference'

# --- 2-4. AC-7: an ADDITIVE difference exits zero -------------------------------------
# The baseline is the LAST RELEASE. Additive drift between releases is the normal state,
# and a gate that reds on it is a gate that freezes the surface.
reset; mut drop-endpoint GET /api/schema
expect 0 "AC-7" "a new endpoint is additive" 'additive: endpoint-added:GET /api/schema'

reset; mut drop-field GET /api/queue 200 now
expect 0 "AC-7" "a new response field is additive" 'additive: field-added:GET /api/queue:200:now'

reset; mut drop-status GET /api/queue 500
expect 0 "AC-7" "a new status code is additive" 'additive: status-added:GET /api/queue:500'

# --- 5. AC-8: every breaking difference, named, not the first --------------------------
reset
mut add-endpoint GET /api/phantom
mut add-field GET /api/queue 200 phantom_field
mut add-status GET /api/queue 599
mut retype-field GET /api/history 200 history string
mut set-media GET /api/summary 200 text/plain
expect 1 "AC-8" "five breaking differences are ALL named" \
  'endpoint-removed:GET /api/phantom' \
  'field-removed:GET /api/queue:200:phantom_field' \
  'status-removed:GET /api/queue:599' \
  'field-narrowed:GET /api/history:200:history' \
  'response-changed:GET /api/summary:200' \
  'in 5 way'

# --- 6-8. AC-9: a comparison that could not run has not passed -------------------------
reset; rm -f "$repo/$baseline"
expect 1 "AC-9" "a MISSING baseline is refused" 'is MISSING'

reset; : > "$repo/$baseline"
expect 1 "AC-9" "an EMPTY baseline is refused" 'is EMPTY'

reset; printf 'this is not a surface document\n' > "$repo/$baseline"
expect 1 "AC-9" "an UNPARSEABLE baseline is refused" 'NOT PARSEABLE'

# A document that is valid JSON and is not a surface document is the same refusal: a gate
# that took it would compare this build against nothing and report it unchanged.
reset; printf '{"something": "else"}\n' > "$repo/$baseline"
expect 1 "AC-9" "JSON that is not a surface document is refused" 'NOT PARSEABLE'

# --- 10-15. AC-10: the record is the only thing that makes a break non-fatal -----------
broke="field-removed:GET /api/queue:200:phantom_field"

record_entry() { # <difference> [extra-yaml-lines...]
  {
    printf -- '- difference: "%s"\n' "$1"
    printf '  version: "v0.3.0"\n'
    printf '  reason: "selftest fixture"\n'
    printf '  recorded: "2026-09-18"\n'
    shift
    for line in "$@"; do printf '%s\n' "$line"; done
  } > "$repo/$record"
}

reset; mut add-field GET /api/queue 200 phantom_field
expect 1 "AC-10" "a break NO record names is fatal" 'field-removed:GET /api/queue:200:phantom_field' 'records none of them'

reset; mut add-field GET /api/queue 200 phantom_field; record_entry "$broke"
expect 0 "AC-10" "a break the record names EXACTLY is non-fatal" 'RECORDED for v0.3.0' 'recorded break'

reset; mut add-field GET /api/queue 200 phantom_field
record_entry "field-removed:GET /api/queue:200:some_other_field"
expect 1 "AC-10" "a record naming a DIFFERENT break covers nothing" 'some_other_field' 'does not have'

reset; record_entry "$broke"
expect 1 "AC-10" "a record matching NO difference in the tree is refused" 'does not have'

reset; mut add-field GET /api/queue 200 phantom_field
printf -- '- difference: "%s"\n  reason: "no version"\n  recorded: "2026-09-18"\n' "$broke" > "$repo/$record"
expect 1 "AC-10" "a record naming no version is refused" 'carries no "version"'

reset; mut add-field GET /api/queue 200 phantom_field
record_entry "$broke" '  blanket: "yes"'
expect 1 "AC-10" "a record carrying an unknown key is refused" 'unknown key'

# --- 16. the tree the gate grades is never rewritten -----------------------------------
after="$(cksum "$here/$baseline" "$here/$record" 2>/dev/null || true)"
if [ "$before" = "$after" ] && [ -n "$before" ]; then
  printf '  ok  the graded tree was never written: %s and %s are byte-identical\n' "$baseline" "$record"
  pass=$((pass + 1))
else
  printf '::error::selftest: the graded tree CHANGED. Mutations must happen in the clone alone.\n' >&2
  printf '        before: %s\n        after:  %s\n' "$before" "$after" >&2
  failed=$((failed + 1))
fi

echo
# Report against the number of cases DECLARED, not the number that ran: "$pass/$pass" is N/N
# by construction and could never show a shortfall.
total=$((pass + failed))
if [ "$total" -ne "$declared" ]; then
  echo "::error::api-schema diff selftest: ran $total case(s), expected $declared - a defeat did NOT execute" >&2
  exit 1
fi
if [ "$failed" -ne 0 ]; then
  echo "::error::api-schema diff selftest: $failed of $declared case(s) did not bite - the surface gate is not trustworthy" >&2
  exit 1
fi
echo "api-schema diff selftest: $pass/$declared cases bite; defeated on purpose: AC-7 AC-8 AC-9 AC-10"
