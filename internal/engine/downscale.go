package engine

import (
	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/downscale"
	"github.com/NSchatz/holdfast/internal/probe"
)

// The resolution ceiling, resolved in ONE place for the three sites that must agree about
// it: the encoder that scales the picture down, the perceptual gate that scales the output
// back up to score it, and the terminal row that records what produced the replacement.
//
// They agree because each of them resolves from the SAME two inputs - the library profile
// the engine handed down and the source's own probe snapshot - through the function below
// and through nothing else. A site that read the configuration for itself would be a second
// answer to "what was this file scaled to", and the gate's whole claim is that it scored
// this encode at the resolution of the file it is about to delete.

// downscaleApplied is the scale that runs for THIS source: the profile's configured ceiling
// against the source's own dimensions, and none where the ceiling is unset, the source is
// already at or below it, or the probe did not establish a size.
//
// The LAST of those is the fail-safe arm and it is not dead code. A file whose dimensions
// nobody could read is skipped in front of the encoder (see SkipUndeterminedSourceHeight),
// so the engine never reaches here with one - but the exported encoder is reachable by a
// direct caller that never pre-probed, and a Scale built against a guessed source size would
// encode a library to a resolution nobody measured and hand the gate a reference resolution
// nobody measured either. Nothing is scaled then, which is exactly what this tool did before
// the key existed.
//
// props may be nil, which is what a direct caller of the exported encoder passes when it did
// not pre-probe; that is the same arm.
func downscaleApplied(prof config.Profile, props *probe.VideoProps) downscale.Scale {
	if props == nil {
		return downscale.Scale{}
	}
	w, h, ok := props.Dimensions()
	if !ok {
		return downscale.Scale{}
	}
	return prof.DownscaleFor(w, h)
}

// downscaleRefused reports whether this job is one the FINAL-SWAP guard stops, and the two
// conditions that stopped it.
//
// The guard is the whole of AC-4 and it is deliberately narrow: it fires only where all
// three of these hold at once - the file WOULD be scaled, the undo window is disabled so the
// swap that replaces it is final, and the governing profile did not separately acknowledge
// that trade. Any one of the three missing leaves the job exactly as it would have been.
//
// It is a SKIP and not a failure, because nothing about the file is wrong: the configuration
// asks for an irreversible swap to fewer pixels and has not said so out loud. An operator
// fixes that by acknowledging it or by opening the undo window, and either edit offers every
// held file straight back (the skip records both keys it read).
func downscaleRefused(cfg *config.Config, prof config.Profile, s downscale.Scale) bool {
	return s.Enabled() && !cfg.UndoEnabled() && !prof.DownscaleAcknowledged()
}
