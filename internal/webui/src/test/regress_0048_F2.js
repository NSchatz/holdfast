// regress_0048_F2 (S0048-holdfast-ledger-5, impl-gate ordinal 1). NOT part of the suite:
// the runner globs `src/test/*.test.js`, which this deliberately is not.
//
// derivations.test.js gained, in place of the two deleted sumStatuses cases:
//
//   test("the page carries no way to derive the total a view was capped against", () => {
//     // LEDGER-5 removed the summary roll-up outright, which is a stronger statement than
//     // a test that the renderer stopped calling it: there is nothing left to call.
//     assert.equal(d.sumStatuses, undefined, "sumStatuses must not exist: ...");
//     assert.equal(d.QUEUE_STATUSES, undefined, ...);
//     assert.equal(d.TERMINAL_STATUSES, undefined, ...);
//   });
//
// That assertion cannot fail. `d` is load.js's api object, and load.js builds it from a
// hard-coded NAMES list: `globalThis.__api = { NOT_RECORDED, STATUSES, ... }`. A name that
// is not in NAMES is undefined on `d` WHATEVER the modules declare - so the test is a
// statement about load.js's NAMES array, not about js/20-derive.js or js/10-constants.js.
//
// This reproduces that: it loads the REAL modules with the roll-up put BACK (in memory -
// the checked-in sources are not touched), through the same NAMES epilogue load.js uses,
// and shows the three assertions still pass.
//
//   node internal/webui/src/test/regress_0048_F2.js
"use strict";

const assert = require("node:assert/strict");
const path = require("node:path");
const vm = require("node:vm");
const { moduleSource, DERIVATION_MODULES } = require("./load.js");

// The exact NAMES epilogue load.js uses, read out of load.js itself so this cannot drift.
const loadSrc = require("node:fs").readFileSync(path.join(__dirname, "load.js"), "utf8");
const NAMES = JSON.parse(
  "[" + loadSrc.slice(loadSrc.indexOf("const NAMES = [") + "const NAMES = [".length,
                      loadSrc.indexOf("];", loadSrc.indexOf("const NAMES = ["))).replace(/,\s*$/, "") + "]",
);

// The modules as committed, PLUS the roll-up the test claims no longer exists. This is the
// regression the test is supposed to catch: someone re-introduces a client-side derivation
// of the total behind a cap.
const restored = DERIVATION_MODULES.map(moduleSource).join("\n") + `
const QUEUE_STATUSES = ["pending","probing","encoding","verifying"];
const TERMINAL_STATUSES = ["done","skipped","failed"];
function sumStatuses(sum, keys) {
  if (!sum || typeof sum !== "object" || !Array.isArray(keys)) return null;
  return keys.reduce((a, s) => a + (isNum(sum[s]) ? sum[s] : 0), 0);
}
`;

const ctx = vm.createContext(Object.create(null));
vm.runInContext(restored + "\n;globalThis.__api = { " + NAMES.join(", ") + " };\n", ctx,
  { filename: "regress-0048-F2.js" });
const d = ctx.__api;

// The three assertions derivations.test.js makes, verbatim, against a module set in which
// every one of them is FALSE.
assert.equal(d.sumStatuses, undefined, "sumStatuses must not exist: the server reports the total");
assert.equal(d.QUEUE_STATUSES, undefined, "a per-table status list is an invitation to derive a total");
assert.equal(d.TERMINAL_STATUSES, undefined, "a per-table status list is an invitation to derive a total");

// Reached only because all three passed. Prove the roll-up really is present and callable
// in the module set they were just measured against, so this file is not itself vacuous.
const probe = vm.runInContext(
  `sumStatuses({pending:4,probing:1,encoding:2,verifying:1,done:9,skipped:3,failed:2}, QUEUE_STATUSES)`, ctx);
assert.equal(probe, 8, "the restored roll-up must be live for this reproduction to mean anything");

console.log(
  "REPRODUCED: derivations.test.js's three assertions PASS against modules that declare\n" +
  "sumStatuses, QUEUE_STATUSES and TERMINAL_STATUSES (the roll-up answers " + probe + " for the\n" +
  "queue states). The test measures load.js's NAMES list, not the module sources, so it\n" +
  "cannot detect the regression its own comment says it rules out.");
