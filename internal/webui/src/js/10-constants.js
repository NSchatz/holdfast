"use strict";
// The closed vocabularies the page reads off the wire. Values only, no DOM: this module
// is loadable in a plain JavaScript runtime, which is what lets the unit suite exercise
// the derivations that consume it without standing up a browser.
const STATUSES = ["pending","probing","encoding","verifying","done","skipped","failed"];
// The one state a progress figure can exist in. Progress is measured BY the encoder
// against the source duration, so it is defined while the encoder runs and at no other
// time: a probing row has not started one and a verifying row's encoder has exited. Those
// states are covered by Elapsed alone, which is exactly what the phase scoped them to.
const PROGRESS_STATUS = "encoding";
// The per-table status lists this module used to carry are GONE (LEDGER-5). They existed
// so the page could roll the summary up into the total each capped table was capped
// against; the server reports that total now, and a list of statuses left here would be an
// invitation to derive one again.

// The three states every view owes (clause F7), in words, one wording per view so a
// reader always knows WHICH view is loading, empty or unreadable. The three are
// deliberately distinct strings inside each view: "nothing to show" and "could not be
// read" are different facts and a page that says the same thing for both is lying about
// one of them.
//
// A view holds exactly one element carrying `data-state`, and its container carries
// `data-view`, so the state a view is in is a rendered property a browser can read back.
const VIEW_STATES = {
  counts: {
    loading: "Loading the live counts.",
    empty: "No file has been recorded in the ledger yet.",
    unreadable: "The live counts could not be read.",
  },
  queue: {
    loading: "Loading the queue.",
    empty: "Nothing queued.",
    unreadable: "The queue could not be read.",
  },
  aggs: {
    loading: "Loading the whole-ledger figures.",
    empty: "No whole-ledger figure has a contributing row yet.",
    unreadable: "The whole-ledger figures could not be read.",
  },
  history: {
    loading: "Loading recent history.",
    empty: "No history yet.",
    unreadable: "Recent history could not be read.",
  },
};

// The column count of each table, so a state row spans the whole table rather than
// sitting in the first column.
const VIEW_COLUMNS = { queue: 5, history: 7 };

// Human labels for the closed vocabulary of skip guards (internal/engine's Skip*
// constants). An unknown token falls back to itself, so a new guard is never hidden.
const GUARD_LABELS = {
  "already-at-target-codec": "already at target codec",
  "low-bitrate": "already efficient (low bitrate)",
  "hardlinked": "hardlinked (would break a seed)",
  "interlaced": "interlaced",
  "dolby-vision": "Dolby Vision (dynamic metadata)",
  "hdr10-plus": "HDR10+ (dynamic metadata)",
  "incomplete-hdr-metadata": "incomplete HDR metadata",
  "exotic-pixel-format": "exotic pixel format",
  "target-already-exists": "target file already exists",
  "symlinked-source": "symlinked source (would replace the link)",
};
