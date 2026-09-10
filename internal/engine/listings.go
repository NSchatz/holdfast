package engine

import (
	"io/fs"
	"os"
	"path/filepath"

	"github.com/NSchatz/holdfast/internal/startup"
)

// One pass over the library, one listing per directory (FILESYSTEM-1).
//
// Three parts of a run want to know what is in a covered directory: the startup
// walk, which lists every one of them to classify the storage and bound the run;
// the stale-temp sweep, which looks for this tool's own orphaned work; and the
// enumeration, which looks for sources. They want the same entry names, and
// asking three times is three times the metadata I/O of a scan that may have
// nothing to do - on a network mount, where every listing is a round trip, that
// is the dominant cost of an idle pass.
//
// So a listing is made once and carried to whoever needs it next. The walk's
// listings are handed to the engine with the coverage set they bound
// (SetCoverage) and are good for the FIRST scan after that walk: they are taken
// by that scan and released as it consumes them, so a later scan lists for
// itself and never acts on a picture of the library taken before it began.

// listed is one directory's listing, as an answer: the entries it returned, or
// the failure it returned instead. The failure is kept deliberately - a listing
// that failed is an answer this pass already has, and asking again would be a
// second listing of the same directory for a question already refused.
type listed struct {
	entries []startup.Entry
	err     error
}

// listings is what one pass has read of the covered directories, whether the
// startup walk read it or the pass listed it itself. It is owned by exactly one
// scan at a time (RunOneshot takes it before anything reads it), so it needs no
// lock and may be released entry by entry as the enumeration consumes it.
type listings struct {
	byDir map[string]listed
	// selfListed counts the directories THIS pass listed, so the run can say so:
	// entry information that was never collected is never evidence, and a scan
	// that had to go and look says which case it was in.
	selfListed int
}

// newListings takes the entry information a startup walk collected. The engine
// takes OWNERSHIP of that map: it releases each directory's entries as the scan
// consumes them, so nothing may read it afterwards.
func newListings(walked map[string][]startup.Entry) *listings {
	l := &listings{byDir: map[string]listed{}}
	for dir, ents := range walked {
		l.byDir[dir] = listed{entries: ents}
	}
	return l
}

func (l *listings) get(dir string) (listed, bool) {
	got, ok := l.byDir[dir]
	return got, ok
}

func (l *listings) put(dir string, got listed) {
	l.byDir[dir] = got
	l.selfListed++
}

// take returns a directory's listing and RELEASES it: the enumeration is the
// last part of a pass to want one, so what it has consumed is dropped rather
// than held to the end of the scan.
func (l *listings) take(dir string) (listed, bool) {
	got, ok := l.byDir[dir]
	if ok {
		delete(l.byDir, dir)
	}
	return got, ok
}

// passListings takes the listings this scan may use, leaving none behind. A
// second scan in the same process - the serve loop's next pass, or an operator's
// rescan racing it - gets an empty set and lists for itself, which is what keeps
// a file that appeared since the startup walk from waiting for a restart.
func (e *Engine) passListings() *listings {
	if l := e.carried.Swap(nil); l != nil {
		return l
	}
	return newListings(nil)
}

// listIn lists dir unless this pass has already read it, and keeps whatever came
// back. It is what makes "one listing per directory per pass" true across the
// sweep and the enumeration whether or not a startup walk fed them.
func (e *Engine) listIn(pass *listings, dir string) listed {
	if got, ok := pass.get(dir); ok {
		return got
	}
	ents, err := e.listDir(dir)
	got := listed{entries: ents, err: err}
	pass.put(dir, got)
	return got
}

// listDir lists one directory the way a scan needs it read: the entry names, the
// kind the listing itself reports, and - for a symbolic link carrying a source
// name, the one place where following it changes what happens - what it resolves
// to. It is the coverage-bounded pass's ONLY route to a directory listing, so a
// test counts what a whole pass costs by substituting it.
func (e *Engine) listDir(dir string) ([]startup.Entry, error) {
	ents, err := e.readDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]startup.Entry, len(ents))
	for i, ent := range ents {
		link := ent.Type()&fs.ModeSymlink != 0
		out[i] = startup.Entry{Name: ent.Name(), IsDir: ent.IsDir(), IsLink: link}
		// Only for a name a scan would enumerate: everything else is decided by
		// the kind the listing already reported, so following the link would buy
		// nothing and cost a stat per symbolic link in the library.
		if link && IsSourceName(ent.Name(), e.Cfg.VideoExts) {
			out[i].ResolvesToDir = isDirectory(filepath.Join(dir, ent.Name()))
		}
	}
	return out, nil
}

// readDir makes the listing itself, routing through the test seam when one is
// set. It is deliberately the RAW listing rather than the whole of listDir, so a
// test that counts what a pass costs still counts the production answer.
func (e *Engine) readDir(dir string) ([]os.DirEntry, error) {
	if e.readDirFn != nil {
		return e.readDirFn(dir)
	}
	return os.ReadDir(dir)
}

// isDirectory reports whether path is a directory once its links are followed. A
// failure to answer is NOT a directory: the caller's other rules then decide the
// path exactly as a build that never asked would have.
func isDirectory(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// SetCoverage binds one startup check's walk to this engine: the directories it
// traversed successfully, which BOUND the run, and the entries those listings
// returned, which the first scan after that walk uses instead of listing them
// again.
//
// The engine takes ownership of entries and releases each directory's listing as
// it consumes it, so nothing may read that map afterwards. Passing nil entries
// is a coverage set with no entry information: the scan then lists those
// directories itself and says so.
func (e *Engine) SetCoverage(dirs []string, entries map[string][]startup.Entry) {
	e.Coverage = dirs
	e.carried.Store(newListings(entries))
}

// skipSourceNamedDirectory reports a path this run will not offer to the
// pipeline: its name is one a scan would enumerate and the path is a DIRECTORY.
// Every step below enumeration - probe, encode, verify, swap - is about a file,
// so a directory reaching them can only fail, once per pass, for ever. The case
// that produces one is a symbolic link to a directory, which a listing reports
// as a non-directory whatever it points at; skipping it with the reason said out
// loud is this repository's rule for input it cannot act on.
func (e *Engine) skipSourceNamedDirectory(path string) {
	e.Log.Info("not enumerating (a source name on a directory)", "file", path,
		"why", "the name is one this run would enumerate as a source and the path resolves to a directory")
}
