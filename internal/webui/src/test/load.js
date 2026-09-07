// Loading the dashboard's own modules into a plain JavaScript runtime.
//
// The page ships as ONE inline <script>, so its modules are plain scripts sharing a
// top-level scope rather than ES modules with import/export - that is what keeps the
// generated document a single self-contained file with nothing to resolve at load time.
// node:vm gives that same shape a scope a test can reach into: the modules are
// concatenated exactly as the generator concatenates them, evaluated in one context, and
// the names they declared are handed back.
//
// Nothing here is installed. node:test, node:assert, node:fs, node:path and node:vm are
// all built into the runtime, so the dashboard's unit suite introduces no registry
// package and no lockfile (B18).
"use strict";

const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const JS_DIR = path.join(__dirname, "..", "js");

// The names the derivation modules declare. Top-level `const`/`let` in a vm script live
// in a lexical scope that is not a property of the context object, so the epilogue below
// - evaluated as part of the same script - is what carries them out.
const NAMES = [
  "NOT_RECORDED", "STATUSES", "QUEUE_STATUSES", "TERMINAL_STATUSES", "PROGRESS_STATUS",
  "GUARD_LABELS", "isNum", "fmtBytes", "fmtTime", "fmtDur", "fmtSpan", "pct", "fmtScore",
  "fmtCount", "sumStatuses", "clockOffsetFrom", "serverNow", "elapsedText", "sizeFigures",
  "vmafFigures", "progressFigure", "guardLabel", "capNoteText", "announceText",
  "aggCoverageText", "aggExclusionText",
];

// The modules that hold no DOM reference at all, and so load anywhere.
const DERIVATION_MODULES = ["10-constants.js", "20-derive.js"];

function moduleSource(name) {
  return fs.readFileSync(path.join(JS_DIR, name), "utf8");
}

// load evaluates the derivation modules in a context with NO document, NO window and NO
// localStorage. A module that reached for the DOM would throw here, which is the property
// that makes these functions unit-testable in the first place.
function load() {
  const ctx = vm.createContext(Object.create(null));
  const src = DERIVATION_MODULES.map(moduleSource).join("\n")
    + "\n;globalThis.__api = { " + NAMES.join(", ") + " };\n";
  vm.runInContext(src, ctx, { filename: "holdfast-derivations.js" });
  const api = ctx.__api;
  for (const n of NAMES) {
    if (api[n] === undefined) throw new Error("module set does not declare " + n);
  }
  return api;
}

module.exports = { load, moduleSource, DERIVATION_MODULES, JS_DIR };
