package startup

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// ConfigKey is the configuration key an opt-in is declared under. It appears in
// every printed declaration, so what holdfast prints is what an operator pastes.
const ConfigKey = "allow_non_local"

// Log emits the classification record for every distinct checked path, the
// reports that do not decide the run, and the complete set of filesystem types
// this build classifies local. Every one of these is emitted before the caller
// may open the job store, encode anything, or create, rename or remove any file.
func (res Result) Log(log *slog.Logger) {
	if log == nil {
		return
	}
	log.Info("startup filesystem check",
		"local_filesystem_types", strings.Join(res.LocalSet, " "),
		"checked_paths", len(res.Records))

	for _, rec := range res.Records {
		attrs := []any{
			"path", rec.Path,
			"kind", string(rec.Kind),
		}
		// A path that does not exist has NO classification, and printing one
		// would make the record for a missing root read exactly like the record
		// for a path whose type could not be determined. `missing=true` below is
		// what it carries instead.
		if rec.Class != Unclassified {
			attrs = append(attrs, "classification", string(rec.Class))
		}
		if rec.Resolved != "" && rec.Resolved != rec.Path {
			attrs = append(attrs, "resolved", rec.Resolved)
		}
		if rec.Type != "" {
			attrs = append(attrs, "filesystem", rec.Type)
		}
		if rec.Reason != "" {
			attrs = append(attrs, "reason", rec.Reason)
		}
		if rec.Missing {
			attrs = append(attrs, "missing", true)
		}
		if rec.Covered {
			attrs = append(attrs, "opted_in", true)
		}
		if rec.Class.IsLocal() {
			log.Info("checked path", attrs...)
		} else {
			log.Warn("checked path", attrs...)
		}
	}

	for _, n := range res.Notices {
		attrs := []any{"path", n.Path, "detail", n.Detail}
		switch n.Kind {
		case NoticeEmptyRoot:
			log.Info("library root is present and empty", attrs...)
		case NoticeReducedGuarantee:
			log.Warn("REDUCED no-loss guarantee on storage that is not local", attrs...)
		case NoticeUnnecessary:
			log.Warn("opt-in declaration is unnecessary and covers nothing", attrs...)
		case NoticeScratchNotLocal:
			// A statement about throughput, not about safety: the no-loss
			// guarantee is not reduced here, so this is not the reduced-guarantee
			// warning wearing another name.
			log.Warn("the scratch directory is on storage that is not local (the run starts; no declaration is required)", attrs...)
		default:
			log.Warn(string(n.Kind), attrs...)
		}
	}
}

// WriteRefusal writes the operator-facing account of a refused run. It names
// EVERY cause already established, not only the row that decided, so three
// uncovered mounts beside one permission denial are fixed in one pass instead of
// paying the startup traversal three times. Where a declaration would permit the
// run it prints one this build's own syntactic check accepts, so the text can be
// pasted back verbatim; where no declaration could lift the refusal it prints
// none at all and gives the remedy instead.
func (res Result) WriteRefusal(w io.Writer) {
	if res.Start {
		return
	}
	fmt.Fprintf(w, "holdfast: refusing to start - %d problem(s) with the storage this run would act on (decided at check %d of %d):\n",
		len(res.Causes), res.Row, rowStart)
	for _, c := range res.Causes {
		fmt.Fprintf(w, "\n  %s\n      %s: %s\n", c.Path, c.Kind, c.Detail)
		if c.Declaration != "" {
			fmt.Fprintf(w, "      permit this path by adding to your configuration:\n        %s:\n        - %s\n", ConfigKey, c.Declaration)
			continue
		}
		if c.Remedy != "" {
			fmt.Fprintf(w, "      no declaration can permit this. remedy: %s\n", c.Remedy)
		}
	}
	fmt.Fprintf(w, "\nholdfast: nothing was encoded, no file under a library root was created, renamed or removed,\n"+
		"          and nothing was created in or under the state directory.\n")
	// The whole truth, and the only thing this check ever writes anywhere. The
	// scratch writability probe creates one zero-length file and removes it, and it
	// is asked LAST - after existence, kind, resolution, overlap and free space -
	// so a run refused for any of those reasons never reached it, and a run refused
	// BY it was refused because the creation failed. The line prints when a probe
	// could have run at all, so an operator reading a refusal is never left to
	// wonder whether "created nothing" was the whole sentence.
	if res.ScratchProbed {
		fmt.Fprintf(w, "          The one exception, stated in full: a zero-length probe file was created in the\n"+
			"          scratch directory and removed again, to establish that it is writable.\n")
	}
}
