package engine

import (
	"os"

	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/subtitle"
)

// publishSidecars publishes a job's gated subtitle sidecars once its swap has committed, makes
// the new names durable, and says what happened to every carried subtitle stream: a sidecar
// written, a stream this build writes none for, or a sidecar refused. None of it changes the
// job's outcome (docs/design/subtitles.md#sidecars).
func (e *Engine) publishSidecars(f, dir string, p *subtitle.Prepared, perm os.FileMode) []store.Sidecar {
	recs := p.Publish(perm)
	published := false
	for _, r := range recs {
		args := []any{"file", f, "stream", r.Index, "codec", r.Codec, "language", r.Language, "forced", r.Forced}
		switch {
		case r.Path != "":
			published = true
			e.Log.Info("subtitle sidecar written", append(args, "sidecar", r.Path, "events", *r.Events)...)
			if r.FontsLost {
				e.Log.Warn("the ASS sidecar does not carry the source's font attachments: a player reads it with its own fonts "+
					"(the replacement still carries the stream and its fonts)", append(args, "sidecar", r.Path)...)
			}
		case subtitle.IsFailure(r.Skipped):
			e.Log.Warn("subtitle sidecar NOT written (the replacement still carries the stream)",
				append(args, "why", r.Skipped, "detail", r.Detail)...)
		default:
			e.Log.Info("no subtitle sidecar for this stream", append(args, "why", r.Skipped)...)
		}
	}
	if published {
		if err := e.fsync(dir); err != nil {
			e.Log.Warn("could not fsync the directory after writing subtitle sidecars (durability not guaranteed)",
				"file", f, "err", err)
		}
	}
	return recs
}
