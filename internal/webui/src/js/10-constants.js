"use strict";
// The closed vocabularies the page reads off the wire. Values only, no DOM: this module
// is loadable in a plain JavaScript runtime, which is what lets the unit suite exercise
// the derivations that consume it without standing up a browser.
const STATUSES = ["pending","probing","encoding","verifying","done","skipped","failed"];
const QUEUE_STATUSES = ["pending","probing","encoding","verifying"];
// The one state a progress figure can exist in. Progress is measured BY the encoder
// against the source duration, so it is defined while the encoder runs and at no other
// time: a probing row has not started one and a verifying row's encoder has exited. Those
// states are covered by Elapsed alone, which is exactly what the phase scoped them to.
const PROGRESS_STATUS = "encoding";
const TERMINAL_STATUSES = ["done","skipped","failed"];

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
