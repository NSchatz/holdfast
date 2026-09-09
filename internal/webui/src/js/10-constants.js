"use strict";
// The closed vocabularies the page reads off the wire. Values only, no DOM: this module
// is loadable in a plain JavaScript runtime, which is what lets the unit suite exercise
// the derivations that consume it without standing up a browser.
// "indeterminate" and "applied-despite-error" are FILESYSTEM-1 outcomes and they get
// their own chip and their own history rows. A job whose swap outcome could not be
// established must show as the state it is in: rendering it as done would be a lie and
// rendering it as failed would be the more dangerous lie, because "failed" on this
// dashboard has always meant "and your source is fine".
// "would-transcode" is a DRY RUN's recorded decision: the file passed every guard, so a
// run allowed to transcode would take it. It is a state of the FINISHED group and not of
// the work in hand - the decision is taken and nothing is examining that file - which is
// also where an operator reads it, beside the skipped and the actually-reclaimed counts.
// Before it existed those files sat under `probing` and the page reported a dry run as
// having concluded nothing at all.
const STATUSES = ["pending","probing","encoding","verifying","done","skipped","failed","would-transcode","indeterminate","applied-despite-error"];
// The one status whose rows carry a source codec and a source size and no output at all,
// named once here so the cells, the total and the graders read the same constant.
const CANDIDATE_STATUS = "would-transcode";
// The one state a progress figure can exist in. Progress is measured BY the encoder
// against the source duration, so it is defined while the encoder runs and at no other
// time: a probing row has not started one and a verifying row's encoder has exited. Those
// states are covered by Elapsed alone, which is exactly what the phase scoped them to.
const PROGRESS_STATUS = "encoding";
// The statuses that are WORK IN HAND, as against the terminal ones. It is a partition of
// STATUSES and nothing else: the page draws the two groups apart, and a status that
// appeared in neither would be a status the page silently mis-grouped, so the split is
// declared here beside the vocabulary it partitions rather than inferred at the point of
// use. A file is in flight until it reaches a status it can never leave.
const IN_FLIGHT = ["pending", "probing", "encoding", "verifying"];
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
//
// Each phrasing names its view WITHOUT repeating the heading above it or a column header
// beneath it (S0052 AC5): a state row sits inside its own table, so "the queue could not
// be read" under a heading reading "Queue and active" is the page telling a reader a word
// it has already read. The words a view is named by here are therefore the ones the
// heading does not use, and every one of the four is inside the eight-word block ceiling.
const VIEW_STATES = {
  counts: {
    loading: "Loading the live counts.",
    empty: "No file recorded yet.",
    unreadable: "The live counts are unreadable.",
  },
  queue: {
    loading: "Loading the work in hand.",
    empty: "No work in hand.",
    unreadable: "The work in hand is unreadable.",
  },
  aggs: {
    loading: "Loading these figures.",
    empty: "No figure has a contributing row.",
    unreadable: "These figures are unreadable.",
  },
  history: {
    loading: "Loading finished swaps.",
    empty: "No swap finished yet.",
    unreadable: "Finished swaps are unreadable.",
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
