package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/hdr"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/startup"
	"github.com/NSchatz/holdfast/internal/store"
)

// `holdfast analyze` - a census of the library the configuration points at.
//
// It answers the two questions a stranger asks before they let a tool that deletes
// originals near their files: what is in here, and (opt-in) what in here is already
// broken. It is therefore a READ and nothing else - no claim, no ledger row, no
// encode, no file created, renamed or removed anywhere, including inside the state
// directory. That is not a promise about the code below; it is the property AC1a
// grades by comparing every byte under the state directory and every library root
// before and after the command.
//
// It pays for exactly ONE traversal of the library: the start-or-refuse walk holdfast
// already makes (FILESYSTEM-1), whose Coverage set is the directories it listed
// successfully and whose Entries are what those listings returned. The census reads
// those listings rather than walking anything of its own, so adding this command added
// no second pass over anybody's library.
//
// WHAT IT CONSUMES RATHER THAN RESTATES. Which files are sources is
// engine.IsSourceName's answer, asked with the configured video_exts - the same
// question the scan asks - plus the retention area the enumeration skips whole and the
// record-based hold-backs a parked job carries. The census never decides membership
// for itself; it only EXPLAINS a negative answer, so a hold-back that changes in the
// engine changes here with it.

// analyzeOptions are the command's two flags.
type analyzeOptions struct {
	// JSON writes the census as exactly one JSON document instead of the table.
	JSON bool
	// Health runs the decode-integrity pass over every enumerated source. Opt-in
	// because it reads every byte of every file in the library.
	Health bool
}

func cmdAnalyze(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("analyze", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "write the census as one JSON document instead of a table")
	health := fs.Bool("health", false,
		"also fully decode every enumerated source and report the ones that fail (slow: it reads every byte)")
	cfg, code := loadConfig(fs, args, stderr)
	if cfg == nil {
		return code
	}
	if rest := fs.Args(); len(rest) > 0 {
		fmt.Fprintf(stderr, "holdfast: analyze takes no positional arguments (got %q)\n", rest[0])
		return 2
	}
	// An interrupted census is a census that was not taken: the signal cancels every
	// probe and decode in flight, and nothing has been written to undo.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runAnalyze(ctx, cfg, analyzeOptions{JSON: *asJSON, Health: *health}, stdout, stderr)
}

// runAnalyze is the command without the signal wiring, so a test can cancel it in the
// middle and assert that an interrupted run left the same nothing behind a completed
// one does.
func runAnalyze(ctx context.Context, cfg *config.Config, opt analyzeOptions, stdout, stderr io.Writer) int {
	// The start-or-refuse decision, TAKEN and then reported rather than obeyed. analyze
	// needs the walk's Coverage set to have anything to report at all, and it mutates
	// nothing, so storage that is not positively local is a verdict it prints beside the
	// census (AC8a) instead of a reason to refuse. Every other cause - a missing root, a
	// root that cannot be listed, a malformed declaration - is a census that cannot be
	// taken, and gets the same operator-facing account `run` writes (AC7b).
	res := startupDecision(cfg)
	if !res.Start && res.Row != rowStorageNotLocal {
		res.WriteRefusal(stderr)
		return 1
	}

	c := censusOverWalk(ctx, cfg, res)
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "holdfast: interrupted - no census was taken, and nothing was written")
		return 1
	}

	exit := 0
	if !c.probeDistributions(ctx, cfg) {
		// AC5: the filesystem figures stand, every probe-derived distribution is marked
		// unavailable WITH ITS REASON, and the exit code stops a script reading a
		// partial census as a complete one.
		exit = 1
	}
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "holdfast: interrupted - no census was taken, and nothing was written")
		return 1
	}
	c.Total = c.totalOfRoots()

	// JSON owes stdout EXACTLY ONE document, so the decode pass runs before anything is
	// written and rides inside that document. Its cost statement still goes out before
	// the first decode - to stderr, which is the only stream that can carry a line in
	// front of a single-document stdout.
	if opt.JSON {
		if opt.Health {
			if !c.runHealth(ctx, cfg, stderr) {
				exit = 1
			}
			if ctx.Err() != nil {
				fmt.Fprintln(stderr, "holdfast: interrupted during the decode-integrity pass - nothing was written")
				return 1
			}
		}
		if err := c.writeJSON(stdout); err != nil {
			fmt.Fprintf(stderr, "holdfast: writing the census: %v\n", err)
			return 1
		}
		return exit
	}

	c.writeTable(stdout)
	if !opt.Health {
		return exit
	}
	if !c.runHealth(ctx, cfg, stdout) {
		exit = 1
	}
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "holdfast: interrupted during the decode-integrity pass - nothing was written")
		return 1
	}
	c.writeHealthTable(stdout)
	return exit
}

// rowStorageNotLocal is the row of the startup decision that fires for storage that is
// not positively local and that no declaration covers. It is the one cause analyze
// reports and continues past; see Result.Row for the whole ordered decision.
const rowStorageNotLocal = 4

// ---- what the census is made of ----------------------------------------------

// figure is one count-and-bytes pair travelling with the SET it covers, which is the
// convention internal/store/aggregate.go established for the ledger: a number whose
// population is not stated is a number a reader will read as some other population's.
type figure struct {
	Set   string `json:"set"`
	Files int64  `json:"files"`
	Bytes int64  `json:"bytes"`
}

// bucket is one keyed count.
type bucket struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

// distribution is a keyed count over one population, with its own unavailability.
//
// Buckets + Excluded == the population's file count, always, and that identity is what
// makes the report checkable rather than decorative. Unavailable is set instead of
// zeroing the buckets when the figure could not be computed at all: empty buckets say
// "the library holds none of these", which is a claim about the operator's files, and
// the one thing a census must never do is make one it did not establish.
type distribution struct {
	Name        string   `json:"name"`
	Set         string   `json:"set"`
	Note        string   `json:"note,omitempty"`
	Buckets     []bucket `json:"buckets"`
	Counted     int64    `json:"counted"`
	Excluded    int64    `json:"excluded"`
	ExcludedWhy []bucket `json:"excluded_reasons,omitempty"`
	Unavailable string   `json:"unavailable,omitempty"`
}

// mechanism is one rule that keeps files out of the source set, and what it withheld.
type mechanism struct {
	Name   string `json:"mechanism"`
	Detail string `json:"detail"`
	Files  int64  `json:"files"`
	Bytes  int64  `json:"bytes"`
}

// coverageReport is what the startup walk read and what it did not, per root. It is
// reported because the census is bounded by it: a directory nobody listed holds an
// unknown number of files, which is not the same as holding none (AC6).
type coverageReport struct {
	Boundary           string   `json:"boundary"`
	DirectoriesRead    int64    `json:"directories_read"`
	DirectoriesNotRead int64    `json:"directories_not_read"`
	NotReadWhy         []bucket `json:"not_read_reasons,omitempty"`
	FilesNotInspected  int64    `json:"files_not_inspected"`
}

// rootCensus is the whole census of one library root (or, for the total, of all of
// them). Found and Sources are AC4's two figures, and Withheld is the account of the
// difference between them: Sources.Files + sum(Withheld.Files) == Found.Files.
type rootCensus struct {
	Root          string         `json:"root"`
	Found         figure         `json:"found"`
	Sources       figure         `json:"sources"`
	Withheld      []mechanism    `json:"withheld_by"`
	WithheldTotal figure         `json:"withheld_total"`
	Coverage      coverageReport `json:"coverage"`
	Distributions []distribution `json:"distributions"`
	Note          string         `json:"note,omitempty"`

	// files is every source under this root, in walk order, kept so the probe pass and
	// the health pass read the SAME set the figures above were computed from.
	files []censusFile
	// withheld accumulates by mechanism name while the filesystem pass runs.
	withheld map[string]*mechanism
	notRead  map[string]int64
}

// censusFile is one source the filesystem pass found, and what the probe said about it.
type censusFile struct {
	path  string
	bytes int64
	props probe.CensusProps
}

// bandSet is the resolution vocabulary this command reports heights in, declared in
// the output for the reason AC1b gives: holdfast has no library-wide resolution
// vocabulary yet (S0089-holdfast-resolution-rules owns that question), so a reader can
// only tell an empty band from a band that does not exist if the set is stated.
type bandSet struct {
	Basis string   `json:"basis"`
	Bands []string `json:"bands"`
	Note  string   `json:"note"`
}

// storageVerdict is the start-or-refuse decision as the census reports it.
type storageVerdict struct {
	// WouldStart is what `run` and `serve` would do with this configuration.
	WouldStart bool   `json:"a_mutating_run_would_start"`
	DecidedAt  int    `json:"decided_at_check"`
	Verdict    string `json:"verdict"`
	Causes     []struct {
		Path        string `json:"path"`
		Kind        string `json:"kind"`
		Detail      string `json:"detail"`
		Declaration string `json:"declaration,omitempty"`
	} `json:"causes,omitempty"`
	Reduced []string `json:"reduced_guarantee,omitempty"`
}

// healthReport is the opt-in decode-integrity pass: what it was about to cost, and
// what failed.
type healthReport struct {
	Set         string   `json:"set"`
	Files       int64    `json:"files"`
	Bytes       int64    `json:"bytes"`
	Checked     int64    `json:"checked"`
	Failed      []string `json:"failed"`
	Unavailable string   `json:"unavailable,omitempty"`
}

// census is the whole report, and it is the ONE value both output forms are rendered
// from. That is what makes AC1c's "carries every figure the default output carries"
// structural rather than a promise: there is no second place for a figure to live.
type census struct {
	Command  string         `json:"command"`
	Bands    bandSet        `json:"resolution_bands"`
	Storage  storageVerdict `json:"storage"`
	Ledger   string         `json:"ledger"`
	Roots    []*rootCensus  `json:"roots"`
	Total    *rootCensus    `json:"total"`
	Health   *healthReport  `json:"health,omitempty"`
	heldBack map[string]string
}

// ---- the filesystem pass ------------------------------------------------------

// The mechanism names. Each is the identifier an operator can grep for in the tree,
// never a paraphrase, so the report names the rule rather than describing it.
const (
	mechExt       = "video_exts"
	mechUndoDir   = engine.UndoDirName
	mechTemp      = engine.TempMarker
	mechRetained  = engine.RetainedMarker
	mechUndoName  = engine.UndoMarker
	mechRecord    = "parked-job record hold-back"
	mechIrregular = "not a regular file"
	mechOther     = "another enumeration hold-back"
)

var mechanismOrder = []string{
	mechExt, mechUndoDir, mechTemp, mechRetained, mechUndoName, mechRecord, mechIrregular, mechOther,
}

var mechanismDetail = map[string]string{
	mechExt:       "the extension is not one the configured video_exts lists",
	mechUndoDir:   "the undo window's retention area, which the enumeration skips whole",
	mechTemp:      "a work-in-progress temp this tool wrote",
	mechRetained:  "a replacement this tool retained because its job did not complete cleanly",
	mechUndoName:  "an original the undo window is holding",
	mechRecord:    "a path a parked job's record, or a recorded replacement, holds back",
	mechIrregular: "a symbolic link, device, socket or FIFO - not a regular file, and never a source",
	mechOther:     "an enumeration hold-back this report does not name individually",
}

// censusOverWalk builds the census from the startup walk's own listings. It opens no
// media file and lists no directory: every entry it counts was read by the walk that
// had already happened, and the only per-file syscall is the stat that gives a size.
func censusOverWalk(ctx context.Context, cfg *config.Config, res startup.Result) *census {
	c := &census{
		Command: "holdfast analyze",
		Bands:   declaredBands(),
		Storage: storageVerdictOf(res),
	}
	c.readLedgerHoldBacks(ctx, cfg)

	roots := make([]string, 0, len(cfg.LibraryRoots))
	for _, r := range cfg.LibraryRoots {
		roots = append(roots, filepath.Clean(r))
	}
	for _, root := range roots {
		c.Roots = append(c.Roots, newRootCensus(root, root, len(res.Coverage)))
	}
	byRoot := func(path string) *rootCensus {
		best, bestLen := (*rootCensus)(nil), -1
		for i, root := range roots {
			if (path == root || strings.HasPrefix(path, root+string(filepath.Separator))) && len(root) > bestLen {
				best, bestLen = c.Roots[i], len(root)
			}
		}
		return best
	}

	for _, dir := range res.Coverage {
		rc := byRoot(dir)
		if rc == nil {
			continue // not beneath any configured root: nothing this census may attribute
		}
		rc.Coverage.DirectoriesRead++
		// The retention area is skipped WHOLE, exactly as the enumeration skips it, and
		// what is in it is counted as withheld by that mechanism rather than passed over
		// in silence: it is one of the reasons the two figures differ.
		inUndoDir := filepath.Base(dir) == engine.UndoDirName
		for _, ent := range res.Entries[dir] {
			p := filepath.Join(dir, ent.Name)
			info, err := os.Lstat(p)
			if err != nil {
				rc.Coverage.FilesNotInspected++
				continue
			}
			if info.IsDir() {
				continue // a directory is covered in its own right, or was not descended
			}
			size := info.Size()
			if !info.Mode().IsRegular() {
				rc.withhold(mechIrregular, size)
				continue
			}
			rc.Found.Files++
			rc.Found.Bytes += size
			if inUndoDir {
				rc.withhold(mechUndoDir, size)
				continue
			}
			if !engine.IsSourceName(ent.Name, cfg.VideoExts) {
				rc.withhold(nameMechanism(ent.Name, cfg.VideoExts), size)
				continue
			}
			if _, held := c.heldBack[censusResolvedForm(p)]; held {
				rc.withhold(mechRecord, size)
				continue
			}
			rc.Sources.Files++
			rc.Sources.Bytes += size
			rc.files = append(rc.files, censusFile{path: p, bytes: size})
		}
	}

	for _, n := range res.Notices {
		if rc := byRoot(n.Path); rc != nil {
			rc.notRead[string(n.Kind)]++
			rc.Coverage.DirectoriesNotRead++
		}
	}
	for _, rc := range c.Roots {
		rc.finish()
	}
	return c
}

// newRootCensus starts one root's figures, each already carrying the set it covers.
func newRootCensus(root, label string, covered int) *rootCensus {
	return &rootCensus{
		Root: root,
		Found: figure{Set: "every regular file in the directories the startup walk covered under " +
			label},
		Sources: figure{Set: "the subset of those files the configuration in force would consider a source, " +
			"under " + label},
		withheld: map[string]*mechanism{},
		notRead:  map[string]int64{},
	}
}

// withhold records one file the source set does not contain, and why.
func (rc *rootCensus) withhold(name string, size int64) {
	m, ok := rc.withheld[name]
	if !ok {
		m = &mechanism{Name: name, Detail: mechanismDetail[name]}
		rc.withheld[name] = m
	}
	m.Files++
	m.Bytes += size
	// An irregular entry is not one of Found's regular files, so it is reported as a
	// mechanism without being subtracted from a figure it was never in.
	if name != mechIrregular {
		rc.WithheldTotal.Files++
		rc.WithheldTotal.Bytes += size
	}
}

// finish publishes the maps this root accumulated, in a fixed order, so two runs over
// an unchanged library print the same report.
//
// EVERY mechanism is published, including the ones that withheld nothing. A report that
// listed only the rules that fired would leave a reader unable to tell a mechanism that
// held nothing back from one this build does not have - and the whole point of AC4's
// account is that the set of reasons the two figures differ is stated, not inferred.
func (rc *rootCensus) finish() {
	rc.WithheldTotal.Set = "every regular file the mechanisms below keep out of the source set, under " + rc.Root
	for _, name := range mechanismOrder {
		m, ok := rc.withheld[name]
		if !ok {
			m = &mechanism{Name: name, Detail: mechanismDetail[name]}
		}
		rc.Withheld = append(rc.Withheld, *m)
	}
	rc.Coverage.NotReadWhy = sortedBuckets(rc.notRead)
	rc.Coverage.Boundary = "the coverage boundary is the last mechanism between the two figures above, " +
		"and the only one carrying no file count: a directory the startup walk declined, could not read " +
		"or never reached yields no file here, and what is inside it is UNKNOWN rather than absent"
}

// nameMechanism explains why a name is not a source. engine.IsSourceName has already
// DECIDED that it is not; this only says which of its clauses fired, so the mechanism
// table adds up without ever being the thing that decides membership.
func nameMechanism(base string, exts []string) string {
	switch {
	case !matchesConfiguredExt(base, exts):
		return mechExt
	case strings.Contains(base, "."+engine.TempMarker+"."):
		return mechTemp
	case engine.IsRetainedReplacementName(base):
		return mechRetained
	case strings.Contains(base, "."+engine.UndoMarker):
		return mechUndoName
	default:
		return mechOther
	}
}

// matchesConfiguredExt is the extension half of IsSourceName, for REPORTING only:
// config.Load normalises every entry to a dot-free lowercase token, and the scan
// compares a basename's extension against them case-insensitively.
func matchesConfiguredExt(base string, exts []string) bool {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(base), "."))
	if ext == "" {
		return false
	}
	for _, want := range exts {
		if ext == strings.ToLower(want) {
			return true
		}
	}
	return false
}

// censusResolvedForm is the key the enumeration's record-based hold-back is looked up
// by: the absolute path with every symbolic link resolved, falling back to the cleaned
// path where it cannot be resolved (which is what a recorded path for a file that has
// since gone gets).
func censusResolvedForm(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(real)
	}
	return filepath.Clean(p)
}

// readLedgerHoldBacks reads the two record-based hold-backs, and NOTHING else, from the
// ledger - and reads them through a handle that creates nothing at all, not even the
// sidecars an ordinary SQLite reader leaves behind (store.OpenSnapshot).
//
// Every failure here is survived and reported rather than fatal, in the same direction
// the engine's own loadHoldBacks fails: a census is a report about the filesystem, and
// an unreadable ledger must not be able to stop one. What it costs is stated in the
// output instead of being silently absorbed - the source figure is then the set the
// configuration would consider a source WITHOUT the record-based hold-back applied.
func (c *census) readLedgerHoldBacks(ctx context.Context, cfg *config.Config) {
	c.heldBack = map[string]string{}
	dbPath := filepath.Join(effectiveStateDir(cfg), "jobs.db")
	if _, err := os.Stat(dbPath); err != nil {
		c.Ledger = fmt.Sprintf("no ledger at %s, so no path is held back by a record", dbPath)
		return
	}
	st, err := store.OpenSnapshot(dbPath)
	if err != nil {
		c.Ledger = fmt.Sprintf("the ledger at %s could not be read (%v), so the source figures below do "+
			"not apply the record-based hold-back; every other figure is unaffected", dbPath, err)
		return
	}
	defer func() { _ = st.Close() }()

	parked, perr := st.ParkedIncidents(ctx)
	for _, in := range parked {
		c.heldBack[censusResolvedForm(in.SourcePath)] = "parked job"
		c.heldBack[censusResolvedForm(in.ReplacementPath)] = "parked job"
	}
	excluded, eerr := st.ExcludedReplacementPaths(ctx)
	for _, p := range excluded {
		if _, ok := c.heldBack[censusResolvedForm(p)]; !ok {
			c.heldBack[censusResolvedForm(p)] = "a recorded replacement path"
		}
	}
	switch {
	case perr != nil || eerr != nil:
		c.Ledger = fmt.Sprintf("the ledger at %s answered only in part (parked jobs: %s, recorded "+
			"replacements: %s), so the record-based hold-back may be incomplete", dbPath,
			errText(perr), errText(eerr))
	default:
		c.Ledger = fmt.Sprintf("%s was read for the record-based hold-back alone (%d path(s) held back); "+
			"no figure below comes from it - the census reports the filesystem as it is now",
			dbPath, len(c.heldBack))
	}
}

// errText renders an error for a report, or "ok" where there is none.
func errText(err error) string {
	if err == nil {
		return "ok"
	}
	return err.Error()
}

// ---- the probe pass -----------------------------------------------------------

// probeDistributions fills the four distributions, and reports whether the probe could
// answer at all. A false return is AC5's degraded path: the filesystem figures above
// stand untouched, every probe-derived distribution is marked unavailable with the
// reason, and the caller exits non-zero.
func (c *census) probeDistributions(ctx context.Context, cfg *config.Config) bool {
	ffprobeBin := envOr("HOLDFAST_FFPROBE", "ffprobe")
	ffmpegBin := envOr("HOLDFAST_FFMPEG", "ffmpeg")
	reason := ""
	if _, err := exec.LookPath(ffprobeBin); err != nil {
		reason = fmt.Sprintf("the configured ffprobe %q could not be run: %v", ffprobeBin, err)
	} else if prober := probe.New(ffmpegBin, ffprobeBin); !prober.Usable(ctx) {
		reason = fmt.Sprintf("the configured ffprobe %q ran but answered nothing when asked for its own "+
			"version, so nothing it says about a file is evidence about that file", ffprobeBin)
	}
	if reason != "" {
		for _, rc := range c.Roots {
			rc.fillDistributions(reason)
		}
		return false
	}

	prober := probe.New(ffmpegBin, ffprobeBin)
	for _, rc := range c.Roots {
		eachInParallel(ctx, cfg.EffectiveWorkers(), len(rc.files), func(i int) {
			rc.files[i].props = prober.CensusProps(ctx, rc.files[i].path)
		})
		rc.fillDistributions("")
	}
	return true
}

// eachInParallel runs fn for every index, at most workers at a time. Results are
// written by index into a slice the caller owns, so the census is the same whatever
// order the probes finish in - and the worker count is the configuration's own, so a
// census never takes more of the host than the operator declared the pipeline may.
func eachInParallel(ctx context.Context, workers, n int, fn func(i int)) {
	if n == 0 {
		return
	}
	if workers < 1 {
		workers = 1
	}
	idx := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range idx {
				fn(i)
			}
		}()
	}
	for i := 0; i < n; i++ {
		select {
		case <-ctx.Done():
			close(idx)
			wg.Wait()
			return
		case idx <- i:
		}
	}
	close(idx)
	wg.Wait()
}

// fillDistributions turns this root's per-file probe answers into the four
// distributions. reason, when non-empty, is why the probe-derived three could not be
// computed at all: they are then marked unavailable rather than published as buckets
// of zero, which would be a claim about the operator's library.
func (rc *rootCensus) fillDistributions(reason string) {
	population := fmt.Sprintf("the %d source(s) under %s", rc.Sources.Files, rc.Root)
	codec := map[string]int64{}
	band := map[string]int64{}
	container := map[string]int64{}
	depth := map[string]int64{}
	for _, f := range rc.files {
		container[strings.ToLower(strings.TrimPrefix(filepath.Ext(f.path), "."))]++
		if reason != "" {
			continue
		}
		codec[codecKey(f.props)]++
		band[bandFor(f.props)]++
		depth[depthKey(f.props)]++
	}
	rc.Distributions = []distribution{
		newDistribution("video codec", population,
			"unknown is a file ffprobe did not answer for, or answered for and found no video stream in; "+
				"it is never guessed at from the name", codec, reason),
		newDistribution("resolution band", population,
			"by the coded height of the first video stream; see the declared band set", band, reason),
		newDistribution("container", population,
			"from the file's extension, which is what the configuration matches on; never probe-derived, "+
				"so it survives an ffprobe that cannot answer", container, ""),
		newDistribution("video bit depth", population,
			"from the pixel format, parsed by the one parser the encoder uses (internal/hdr.ParsePixFmt); "+
				"a format outside the recognised planar-YUV set is reported as such and never as a depth",
			depth, reason),
	}
	for i := range rc.Distributions {
		rc.Distributions[i].reconcile(rc.Sources.Files)
	}
}

func newDistribution(name, set, note string, counts map[string]int64, unavailable string) distribution {
	d := distribution{Name: name, Set: set, Note: note, Unavailable: unavailable}
	if unavailable != "" {
		return d
	}
	d.Buckets = sortedBuckets(counts)
	return d
}

// reconcile makes the arithmetic identity explicit and true: the buckets plus the
// explicitly excluded count equal the population's file count. An unavailable
// distribution excludes the whole population and says so.
func (d *distribution) reconcile(population int64) {
	var counted int64
	for _, b := range d.Buckets {
		counted += b.Count
	}
	d.Counted = counted
	d.Excluded = population - counted
	if d.Excluded > 0 && d.Unavailable != "" {
		d.ExcludedWhy = []bucket{{Key: "the figure could not be computed", Count: d.Excluded}}
	}
}

// codecKey is the codec bucket a file falls in. probe.CensusProps reports an
// unanswered probe and a file with no video stream as the same empty string at the
// field level; both are honestly "unknown" for a count, and neither may be guessed.
func codecKey(p probe.CensusProps) string {
	if !p.Answered || p.Codec == "" {
		return bandUnknown
	}
	return p.Codec
}

// depthKey is the bit-depth bucket, taken from the pixel format through the repository's
// ONE pix_fmt parser so the census cannot drift from what the encoder derives.
func depthKey(p probe.CensusProps) string {
	if !p.Answered || p.PixFmt == "" {
		return bandUnknown
	}
	f, ok := hdr.ParsePixFmt(p.PixFmt)
	if !ok {
		return "outside the recognised planar-YUV set"
	}
	return fmt.Sprintf("%d-bit", f.Depth)
}

// bandUnknown is the declared bucket for a value ffprobe did not answer for. It is a
// real bucket and not an omission: a file with no answer is still a file in the
// library, and dropping it would make the distribution disagree with the file count.
const bandUnknown = "unknown"

// The resolution bands. The boundaries are this command's own and are declared in the
// output, because holdfast has no library-wide resolution vocabulary yet: the bands are
// the familiar broadcast heights, each stated as the closed interval it is, so nothing
// here pre-empts what the tool will one day DO per resolution.
var resolutionBands = []string{
	bandUnknown, "480 and below", "481-720", "721-1080", "1081-1440", "1441-2160", "above 2160",
}

func declaredBands() bandSet {
	return bandSet{
		Basis: "the coded height in pixels of the first video stream, as ffprobe reports it",
		Bands: resolutionBands,
		Note: "this band set is this command's own. holdfast has no library-wide resolution vocabulary " +
			"yet, so an empty band here means no file fell in it - never that the band does not exist.",
	}
}

func bandFor(p probe.CensusProps) string {
	if !p.Answered || p.Height <= 0 {
		return bandUnknown
	}
	switch {
	case p.Height <= 480:
		return resolutionBands[1]
	case p.Height <= 720:
		return resolutionBands[2]
	case p.Height <= 1080:
		return resolutionBands[3]
	case p.Height <= 1440:
		return resolutionBands[4]
	case p.Height <= 2160:
		return resolutionBands[5]
	default:
		return resolutionBands[6]
	}
}

// sortedBuckets orders by count descending then key ascending, which is the order the
// ledger's own aggregates publish, so a reader meets one ordering across the tool.
func sortedBuckets(counts map[string]int64) []bucket {
	out := make([]bucket, 0, len(counts))
	for k, n := range counts {
		out = append(out, bucket{Key: k, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// ---- the total ----------------------------------------------------------------

// totalOfRoots folds every root into one census over all of them. It folds the
// COMPUTED figures rather than re-deriving anything, so the total cannot disagree with
// the rows above it.
func (c *census) totalOfRoots() *rootCensus {
	label := "all configured library roots"
	t := newRootCensus(label, label, 0)
	codec := map[string]int64{}
	band := map[string]int64{}
	container := map[string]int64{}
	depth := map[string]int64{}
	unavailable := ""
	for _, rc := range c.Roots {
		t.Found.Files += rc.Found.Files
		t.Found.Bytes += rc.Found.Bytes
		t.Sources.Files += rc.Sources.Files
		t.Sources.Bytes += rc.Sources.Bytes
		t.Coverage.DirectoriesRead += rc.Coverage.DirectoriesRead
		t.Coverage.DirectoriesNotRead += rc.Coverage.DirectoriesNotRead
		t.Coverage.FilesNotInspected += rc.Coverage.FilesNotInspected
		for _, m := range rc.Withheld {
			t.withheld[m.Name] = addMechanism(t.withheld[m.Name], m)
			if m.Name != mechIrregular {
				t.WithheldTotal.Files += m.Files
				t.WithheldTotal.Bytes += m.Bytes
			}
		}
		for k, n := range rc.notRead {
			t.notRead[k] += n
		}
		for _, d := range rc.Distributions {
			if d.Unavailable != "" && d.Name != "container" {
				unavailable = d.Unavailable
			}
			switch d.Name {
			case "video codec":
				addBuckets(codec, d.Buckets)
			case "resolution band":
				addBuckets(band, d.Buckets)
			case "container":
				addBuckets(container, d.Buckets)
			case "video bit depth":
				addBuckets(depth, d.Buckets)
			}
		}
	}
	t.finish()
	population := fmt.Sprintf("the %d source(s) under every configured library root", t.Sources.Files)
	t.Distributions = []distribution{
		newDistribution("video codec", population, "", codec, unavailable),
		newDistribution("resolution band", population, "", band, unavailable),
		newDistribution("container", population, "", container, ""),
		newDistribution("video bit depth", population, "", depth, unavailable),
	}
	for i := range t.Distributions {
		t.Distributions[i].reconcile(t.Sources.Files)
	}
	return t
}

func addMechanism(into *mechanism, m mechanism) *mechanism {
	if into == nil {
		return &mechanism{Name: m.Name, Detail: m.Detail, Files: m.Files, Bytes: m.Bytes}
	}
	into.Files += m.Files
	into.Bytes += m.Bytes
	return into
}

func addBuckets(into map[string]int64, from []bucket) {
	for _, b := range from {
		into[b.Key] += b.Count
	}
}

// ---- the storage verdict ------------------------------------------------------

// storageVerdictOf is AC8a: the decision `run` and `serve` obey, reported here instead.
func storageVerdictOf(res startup.Result) storageVerdict {
	v := storageVerdict{WouldStart: res.Start, DecidedAt: res.Row}
	switch {
	case res.Start:
		v.Verdict = "every path this census read is on storage this build classifies local, " +
			"so `holdfast run` and `holdfast serve` would start on this configuration"
	default:
		v.Verdict = "`holdfast run` and `holdfast serve` would REFUSE this configuration: a path this run " +
			"would act on is on storage that is not positively local and no declaration covers it. " +
			"analyze reports that and takes the census anyway, because it mutates nothing"
	}
	for _, cause := range res.Causes {
		v.Causes = append(v.Causes, struct {
			Path        string `json:"path"`
			Kind        string `json:"kind"`
			Detail      string `json:"detail"`
			Declaration string `json:"declaration,omitempty"`
		}{Path: cause.Path, Kind: string(cause.Kind), Detail: cause.Detail, Declaration: cause.Declaration})
	}
	for _, n := range res.Notices {
		if n.Kind == startup.NoticeReducedGuarantee {
			v.Reduced = append(v.Reduced, fmt.Sprintf("%s: %s", n.Path, n.Detail))
		}
	}
	return v
}

// ---- the health pass ----------------------------------------------------------

// runHealth writes the cost statement, then fully decodes every enumerated source and
// reports the ones that fail. It repairs nothing, moves nothing and records nothing:
// the report IS the whole of it (AC2a).
func (c *census) runHealth(ctx context.Context, cfg *config.Config, costTo io.Writer) bool {
	h := &healthReport{Set: "every source this census enumerated, under every configured library root"}
	for _, rc := range c.Roots {
		h.Files += rc.Sources.Files
		h.Bytes += rc.Sources.Bytes
	}
	ffmpegBin := envOr("HOLDFAST_FFMPEG", "ffmpeg")
	ffprobeBin := envOr("HOLDFAST_FFPROBE", "ffprobe")
	if _, err := exec.LookPath(ffmpegBin); err != nil {
		h.Unavailable = fmt.Sprintf("the configured ffmpeg %q could not be run: %v", ffmpegBin, err)
		c.Health = h
		return false
	}
	// BEFORE the first decode, and it is the whole point of the statement: a full decode
	// of every source is the expensive thing this tool can be asked to do, and an
	// operator is owed the size of the bill while they can still stop it.
	fmt.Fprintf(costTo, "\ndecode-integrity pass: about to FULLY DECODE %d file(s), %d byte(s) in total. "+
		"Every byte of every source is read; nothing is written, moved or repaired.\n", h.Files, h.Bytes)

	prober := probe.New(ffmpegBin, ffprobeBin)
	for _, rc := range c.Roots {
		failed := make([]bool, len(rc.files))
		eachInParallel(ctx, cfg.EffectiveWorkers(), len(rc.files), func(i int) {
			failed[i] = !prober.DecodeOK(ctx, rc.files[i].path)
		})
		if ctx.Err() != nil {
			c.Health = h
			return true
		}
		for i, f := range rc.files {
			h.Checked++
			if failed[i] {
				h.Failed = append(h.Failed, f.path)
			}
		}
	}
	if h.Failed == nil {
		h.Failed = []string{}
	}
	c.Health = h
	return true
}
