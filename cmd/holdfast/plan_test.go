package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/logging"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/startup"
	"github.com/NSchatz/holdfast/internal/store"
)

// The plan command's suite, one case per acceptance criterion of
// pipeline/active/S0103-holdfast-plan-report/spec.md.
//
// Two properties are the reason the command exists and they are written first: that running
// it is a READ (AC-2), and that it REFUSES to project a reclaim this install has no history
// to derive (AC-4). Everything else is arithmetic that has to agree with the daemon's own
// pass, which is why several cases below run that pass and compare.

// planLibrary lays out one library carrying, deliberately, a file for every category a plan
// has to separate: sources a run would transcode, one a named guard would skip, files no
// configured extension matches, and each of this tool's own working files.
//
// It returns the config path, the library root and the state directory (which does NOT
// exist: a plan must not need one, and must not create one).
func planLibrary(t *testing.T, extra string) (cfgPath, lib, state string) {
	t.Helper()
	dir := t.TempDir()
	lib = filepath.Join(dir, "media")
	state = filepath.Join(dir, "state")

	// Two a run WOULD transcode: h264 is not the cpu encoder's target codec.
	encodeFixture(t, filepath.Join(lib, "movie.mkv"), "libx264", "320x240", "yuv420p")
	encodeFixture(t, filepath.Join(lib, "show", "ep1.mkv"), "libx264", "256x144", "yuv420p")
	// One a guard skips, and the report has to NAME that guard: it is already hevc.
	encodeFixture(t, filepath.Join(lib, "show", "ep2.mkv"), "libx265", "320x240", "yuv420p")

	writeCensusFile(t, filepath.Join(lib, "notes.txt"), "not media\n")
	writeCensusFile(t, filepath.Join(lib, "cover.jpg"), "not media either\n")
	writeCensusFile(t, filepath.Join(lib, "movie."+engine.TempMarker+".mkv"), "work in progress\n")
	writeCensusFile(t, filepath.Join(lib, "show", "ep1."+engine.RetainedMarker+".mkv"), "a replacement holdfast kept\n")
	writeCensusFile(t, filepath.Join(lib, engine.UndoDirName, "movie.abc123."+engine.UndoMarker+".mkv"), "a retained original\n")

	cfgPath = filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + state +
		"\nmin_bitrate_kbps: 0\nvmaf_enable: false\n" + extra
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	substitute(t, fixedType("ext4"))
	return cfgPath, lib, state
}

// planSources is the three real sources planLibrary writes, which is exactly the set a scan
// over that library covers.
func planSources(lib string) []string {
	return []string{
		filepath.Join(lib, "movie.mkv"),
		filepath.Join(lib, "show", "ep1.mkv"),
		filepath.Join(lib, "show", "ep2.mkv"),
	}
}

// planJSON runs the plan in its machine-readable form and decodes it. Decoding into the
// command's own type is deliberate: a field the renderer publishes and this test cannot see
// does not exist.
func planJSON(t *testing.T, args ...string) *plan {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := dispatch(append([]string{"plan", "--json", "--config"}, args...), &out, &errOut); code != 0 {
		t.Fatalf("plan --json code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	var p plan
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		t.Fatalf("the plan document does not parse whole: %v\n%s", err, out.String())
	}
	return &p
}

// planReport runs the plan in its default form and returns what it wrote to stdout.
func planReport(t *testing.T, cfgPath string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"plan", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("plan code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	return out.String()
}

// planPassOver builds exactly the pass the command runs and returns the files it covered,
// so a test can compare a plan's coverage PATH BY PATH against a daemon pass rather than
// against a summary of it.
func planPassOver(t *testing.T, cfgPath string) *engine.PlanPass {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var errOut bytes.Buffer
	eng, ledger, _, code := planEngine(context.Background(), cfg, startupDecision(cfg), &errOut)
	if code != 0 {
		t.Fatalf("building the plan pass failed with code %d: %s", code, errOut.String())
	}
	if ledger != nil {
		defer func() { _ = ledger.Close() }()
	}
	return eng.Plan(context.Background(), engine.PlanOptions{
		Ledger: planLedgerReader(ledger), Snapshot: planSnapshot(eng)})
}

// seedCompletedEncodes writes real DONE rows carrying the size pair a ratio is derived from,
// under the profile digest that decided them. It is how "this install's own history" is put
// in front of a projection: the rows are the evidence, so the test writes rows.
func seedCompletedEncodes(t *testing.T, state, digest string, pairs ...[2]int64) {
	t.Helper()
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(state, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	for i, pair := range pairs {
		path := fmt.Sprintf("/history/done-%s-%d.mkv", digest, i)
		key := fmt.Sprintf("fingerprint-%s-%d", digest, i)
		if _, err := st.Claim(ctx, path, key, "w0", 3, store.DecisionInputs{}); err != nil {
			t.Fatal(err)
		}
		src, out := pair[0], pair[1]
		if err := st.Finish(ctx, path, key, store.Done, &store.Outcome{
			SourceBytes: &src,
			OutputBytes: &out,
			Decision:    store.Decision{ProfileDigest: digest},
		}, 3); err != nil {
			t.Fatal(err)
		}
	}
}

// rootDigests is the digest of each configured root's resolved profile, in configuration
// order - the same identity the report groups by and a terminal row records.
func rootDigests(t *testing.T, cfgPath string) []string {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range cfg.RootProfiles() {
		out = append(out, r.Profile.Digest())
	}
	return out
}

// daemonPassPaths runs a full DAEMON pass over the library with dry_run on and returns every
// path it decided. A dry run probes, guards and finishes a row per file without encoding, so
// the rows it leaves ARE the set that pass covered - which is what a plan has to equal.
//
// The encoder is nil and stands in for nothing: a dry run returns before the encoder is
// reached, so the unit under test here is the real enumeration and the real guards.
func daemonPassPaths(t *testing.T, cfgPath string) map[string]string {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.DryRun = true

	res := startupDecision(cfg)
	st, err := store.Open(filepath.Join(effectiveStateDir(cfg), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	var logs bytes.Buffer
	prober := probe.New(envOr("HOLDFAST_FFMPEG", "ffmpeg"), envOr("HOLDFAST_FFPROBE", "ffprobe"))
	eng := engine.New(*cfg, prober, nil, st, logging.To(&logs, "error"))
	eng.SetCoverage(res.Coverage, res.Entries)
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("the daemon pass failed: %v\n%s", err, logs.String())
	}

	rows, err := st.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.Path] = string(r.Status)
	}
	return out
}

// TestPlan_ReportsEligibleSkippedAndBytes is AC-1: the report states how many files are
// eligible, how many were skipped BROKEN DOWN BY THE GUARD that skipped each, and the total
// eligible bytes. The fixture carries one file for a named guard, so a report that folded it
// into the eligible count would be short by a known number of files and bytes.
func TestPlan_ReportsEligibleSkippedAndBytes(t *testing.T) {
	cfgPath, lib, _ := planLibrary(t, "")

	var eligibleBytes int64
	for _, p := range []string{filepath.Join(lib, "movie.mkv"), filepath.Join(lib, "show", "ep1.mkv")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		eligibleBytes += fi.Size()
	}

	p := planJSON(t, cfgPath)
	if p.Total.Eligible.Files != 2 || p.Total.Eligible.Bytes != eligibleBytes {
		t.Fatalf("eligible = %d file(s) %d byte(s), want 2 and %d",
			p.Total.Eligible.Files, p.Total.Eligible.Bytes, eligibleBytes)
	}
	if p.Total.Skipped != 1 {
		t.Fatalf("skipped = %d file(s), want 1", p.Total.Skipped)
	}
	byGuard := map[string]int64{}
	for _, b := range p.Total.SkippedByGuard {
		byGuard[b.Key] = b.Count
	}
	if byGuard[engine.SkipAlreadyTargetCodec] != 1 {
		t.Fatalf("the skip is not attributed to %q: %+v", engine.SkipAlreadyTargetCodec, p.Total.SkippedByGuard)
	}
	// Every guard this build has is published, whether or not it fired: a breakdown that
	// listed only the guards that fired cannot tell "skipped nothing" from "does not exist".
	for _, token := range engine.SkipGuards {
		if _, ok := byGuard[token]; !ok {
			t.Fatalf("guard %q is missing from the breakdown: %+v", token, p.Total.SkippedByGuard)
		}
	}

	// The same three figures, in the report a person reads.
	got := planReport(t, cfgPath)
	for _, want := range []string{
		"eligible", fmt.Sprint(eligibleBytes), engine.SkipAlreadyTargetCodec, "skipped",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the report never states %q:\n%s", want, got)
		}
	}
}

// TestPlan_WritesNoJobRows is AC-2, and it is the criterion that keeps this command a read:
// after it has run - in both output forms, over a library with a real ledger beside it - no
// job has been claimed, the ledger holds exactly the rows it held before, and every byte
// under the state directory and under the library root is the byte that was there when the
// command started.
func TestPlan_WritesNoJobRows(t *testing.T) {
	// The names are spelled out rather than derived from the arguments: an empty name makes
	// Go call the subtest "#00", which puts a '#' into its temp directory - and a '#' in a
	// path is exactly what internal/store's own URI regression is about.
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"the report", []string{"plan", "--config"}},
		{"the json document", []string{"plan", "--json", "--config"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, lib, state := planLibrary(t, "")
			want := seedCensusLedger(t, state, "/gone/vanished.mkv")

			libBefore := treeSnapshot(t, lib)
			stateBefore := treeSnapshot(t, state)

			var out, errOut bytes.Buffer
			if code := dispatch(append(tc.args, cfgPath), &out, &errOut); code != 0 {
				t.Fatalf("plan code = %d, want 0 (stderr: %s)", code, errOut.String())
			}

			assertSameTree(t, "the library root", libBefore, treeSnapshot(t, lib))
			assertSameTree(t, "the state directory", stateBefore, treeSnapshot(t, state))
			if got := jobRowCount(t, state); got != want {
				t.Fatalf("the ledger holds %d row(s) after plan, want the %d it held before", got, want)
			}
		})
	}

	// And with NO state directory at all, which is a fresh install: a plan must not create
	// one on its way past.
	t.Run("no state directory", func(t *testing.T) {
		cfgPath, lib, state := planLibrary(t, "")
		libBefore := treeSnapshot(t, lib)

		var out, errOut bytes.Buffer
		if code := dispatch([]string{"plan", "--config", cfgPath}, &out, &errOut); code != 0 {
			t.Fatalf("plan code = %d, want 0 (stderr: %s)", code, errOut.String())
		}
		assertSameTree(t, "the library root", libBefore, treeSnapshot(t, lib))
		if _, err := os.Stat(state); err == nil {
			t.Fatalf("plan created the state directory %s, which it must never do", state)
		}
	})
}

// TestPlan_ProjectionCarriesSampleAndSpread is AC-3: with completed encodes in this
// install's own history, the projection is derived from THEIR size ratios and is reported
// with the sample size and the spread it came from - never as a single unqualified number.
func TestPlan_ProjectionCarriesSampleAndSpread(t *testing.T) {
	cfgPath, lib, state := planLibrary(t, "")
	digest := rootDigests(t, cfgPath)[0]
	// Three completed encodes: ratios 0.25, 0.50 and 0.75, so the mean is 0.50 and the
	// spread is a real interval rather than a point.
	seedCompletedEncodes(t, state, digest, [2]int64{1000, 250}, [2]int64{1000, 500}, [2]int64{1000, 750})

	var eligibleBytes int64
	for _, p := range []string{filepath.Join(lib, "movie.mkv"), filepath.Join(lib, "show", "ep1.mkv")} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		eligibleBytes += fi.Size()
	}

	p := planJSON(t, cfgPath)
	pr := p.Total.Projection
	if !pr.Made || pr.Reclaim == nil {
		t.Fatalf("no projection was made with three completed encodes in the ledger: %+v", pr)
	}
	s := pr.Reclaim.Sample
	if s.CompletedEncodes != 3 {
		t.Fatalf("the sample size is %d, want the 3 completed encodes it was derived from", s.CompletedEncodes)
	}
	if s.RatioMin != 0.25 || s.RatioMean != 0.5 || s.RatioMax != 0.75 {
		t.Fatalf("the spread is %v/%v/%v, want 0.25/0.5/0.75", s.RatioMin, s.RatioMean, s.RatioMax)
	}
	// The reclaim is those ratios applied to the eligible bytes, and the two bounds come
	// from the two ends of the sample rather than from the mean alone.
	if want := int64(float64(eligibleBytes) * 0.5); pr.Reclaim.Bytes != want {
		t.Fatalf("the estimated reclaim is %d, want %d (half the %d eligible bytes)",
			pr.Reclaim.Bytes, want, eligibleBytes)
	}
	if want := int64(float64(eligibleBytes) * 0.25); pr.Reclaim.LowBytes != want {
		t.Fatalf("the low bound is %d, want %d (the sample's largest ratio)", pr.Reclaim.LowBytes, want)
	}
	if want := int64(float64(eligibleBytes) * 0.75); pr.Reclaim.HighBytes != want {
		t.Fatalf("the high bound is %d, want %d (the sample's smallest ratio)", pr.Reclaim.HighBytes, want)
	}

	// The report a person reads carries the sample and the spread beside the figure.
	got := planReport(t, cfgPath)
	for _, want := range []string{"3 completed encode(s)", "0.2500", "0.5000", "0.7500"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the report publishes the projection without %q:\n%s", want, got)
		}
	}
}

// TestPlan_RefusesToProjectWithNoHistory is AC-4, and it is the criterion that keeps this
// command honest: with no completed encode of its own, the plan reports the eligible bytes
// ALONE and refuses to project a ratio, saying plainly why. A zero, or a figure borrowed
// from somebody else's measurements, would both be inventions.
func TestPlan_RefusesToProjectWithNoHistory(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, state string)
	}{
		{name: "no ledger at all", seed: func(*testing.T, string) {}},
		{name: "a ledger with no completed encode", seed: func(t *testing.T, state string) {
			seedCensusLedger(t, state, "/gone/vanished.mkv")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, _, state := planLibrary(t, "")
			tc.seed(t, state)

			p := planJSON(t, cfgPath)
			pr := p.Total.Projection
			if pr.Made || pr.Reclaim != nil {
				t.Fatalf("a projection was made with no completed encode in this install's history: %+v", pr)
			}
			if !strings.Contains(pr.Refused, "never completed an encode") {
				t.Fatalf("the refusal does not say this install has never completed an encode: %q", pr.Refused)
			}
			// The eligible bytes still stand: refusing the ratio is not refusing the report.
			if p.Total.Eligible.Files != 2 || p.Total.Eligible.Bytes == 0 {
				t.Fatalf("the eligible figures were lost with the projection: %+v", p.Total.Eligible)
			}

			got := planReport(t, cfgPath)
			if !strings.Contains(got, "NO PROJECTION") || !strings.Contains(got, "never completed an encode") {
				t.Fatalf("the report does not refuse the projection plainly:\n%s", got)
			}
			// No number anywhere is offered as a reclaim.
			if strings.Contains(got, "estimated 0 byte(s)") {
				t.Fatalf("the report emitted a zero reclaim instead of refusing:\n%s", got)
			}
		})
	}
}

// TestPlan_ProjectionNamesItselfAnEstimate is AC-5: wherever a projection appears it names
// itself an estimate AT THAT POINT and states that the actual result depends on content this
// install has not encoded yet.
func TestPlan_ProjectionNamesItselfAnEstimate(t *testing.T) {
	cfgPath, _, state := planLibrary(t, "")
	seedCompletedEncodes(t, state, rootDigests(t, cfgPath)[0], [2]int64{1000, 400})

	p := planJSON(t, cfgPath)
	for _, g := range append(append([]*planGroup{}, p.Profiles...), p.Total) {
		pr := g.Projection
		if !pr.Made {
			continue
		}
		if pr.Reclaim.Estimate != theEstimateSentence {
			t.Fatalf("the projection for %q does not name itself an estimate where its numbers appear: %q",
				g.Profile, pr.Reclaim.Estimate)
		}
		if !strings.Contains(pr.Reclaim.Estimate, "has not encoded") {
			t.Fatalf("the estimate never says the result depends on content this install has not "+
				"encoded: %q", pr.Reclaim.Estimate)
		}
	}

	got := planReport(t, cfgPath)
	if !strings.Contains(got, "ESTIMATE, not a measurement") || !strings.Contains(got, "has not encoded") {
		t.Fatalf("the report publishes a projection without naming it an estimate at that point:\n%s", got)
	}
	// A mutation check on this grader: the sentence is what carries the criterion, so the
	// constant and the published text must be the same string.
	if !strings.Contains(got, theEstimateSentence[:40]) {
		t.Fatalf("the report's estimate sentence is not the one constant both forms render:\n%s", got)
	}
}

// TestPlan_JSONIsStdoutOnlyAndCarriesRefusal is AC-6: --json writes the WHOLE plan as one
// parseable document to stdout and writes nothing else there, every human message and error
// goes to stderr, and the document distinguishes a projection that was made from one that
// was refused with the refusal carrying its reason.
func TestPlan_JSONIsStdoutOnlyAndCarriesRefusal(t *testing.T) {
	cfgPath, _, state := planLibrary(t, "")

	// Refused: nothing in this install's history.
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"plan", "--json", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("plan --json code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	// Parsed WHOLE, not line by line: anything else on stdout makes this fail.
	var refused plan
	if err := json.Unmarshal(out.Bytes(), &refused); err != nil {
		t.Fatalf("stdout does not parse as one JSON document: %v\n%s", err, out.String())
	}
	if refused.Total.Projection.Made {
		t.Fatal("the document says a projection was made when there is no history for one")
	}
	if refused.Total.Projection.Refused == "" {
		t.Fatal("a refused projection carries no reason")
	}

	// Made: the same document shape, with the field a script branches on flipped.
	seedCompletedEncodes(t, state, rootDigests(t, cfgPath)[0], [2]int64{1000, 400})
	made := planJSON(t, cfgPath)
	if !made.Total.Projection.Made {
		t.Fatal("the document says no projection was made with a completed encode in the ledger")
	}
	if made.Total.Projection.Refused != "" {
		t.Fatalf("a projection that WAS made still carries a refusal: %q", made.Total.Projection.Refused)
	}

	// Without --json the default output is the human report, and it is deliberately not JSON.
	report := planReport(t, cfgPath)
	if json.Valid([]byte(report)) {
		t.Fatalf("the default output parses as JSON, so a script cannot tell the two forms apart:\n%s", report)
	}
	if !strings.HasPrefix(report, "holdfast plan -") {
		t.Fatalf("the default output is not the human report:\n%s", report)
	}

	// An ERROR under --json goes to stderr and leaves stdout empty (cli L2).
	var badOut, badErr bytes.Buffer
	if code := dispatch([]string{"plan", "--json", "--config", filepath.Join(t.TempDir(), "nope.yaml")},
		&badOut, &badErr); code != 1 {
		t.Fatalf("an unloadable config under --json exits %d, want 1", code)
	}
	if badOut.Len() != 0 {
		t.Fatalf("an error under --json wrote to stdout: %q", badOut.String())
	}
	if badErr.Len() == 0 {
		t.Fatal("an error under --json wrote nothing to stderr")
	}
}

// TestPlan_ReportsPerProfile is AC-7: where the configuration resolves more than one encode
// profile across its roots, the eligible count, the eligible bytes and the projection are
// reported PER PROFILE as well as in total, so no reported number spans roots that would be
// encoded differently.
func TestPlan_ReportsPerProfile(t *testing.T) {
	dir := t.TempDir()
	tv := filepath.Join(dir, "tv")
	films := filepath.Join(dir, "films")
	state := filepath.Join(dir, "state")

	encodeFixture(t, filepath.Join(tv, "ep1.mkv"), "libx264", "256x144", "yuv420p")
	encodeFixture(t, filepath.Join(films, "a.mkv"), "libx264", "320x240", "yuv420p")
	encodeFixture(t, filepath.Join(films, "b.mkv"), "libx264", "320x240", "yuv420p")

	cfgPath := filepath.Join(dir, "config.yaml")
	body := "library_roots:\n" +
		"  - path: " + tv + "\n    crf: 26\n" +
		"  - path: " + films + "\n    crf: 20\n" +
		"state_dir: " + state + "\nmin_bitrate_kbps: 0\nvmaf_enable: false\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	substitute(t, fixedType("ext4"))

	digests := rootDigests(t, cfgPath)
	if len(digests) != 2 || digests[0] == digests[1] {
		t.Fatalf("the fixture did not resolve two distinct profiles: %v", digests)
	}
	// History under the FILMS profile only, so the two groups must reach different verdicts
	// about whether a ratio can be projected at all.
	seedCompletedEncodes(t, state, digests[1], [2]int64{1000, 400})

	p := planJSON(t, cfgPath)
	if len(p.Profiles) != 2 {
		t.Fatalf("the plan reports %d profile group(s), want 2: %+v", len(p.Profiles), p.Profiles)
	}
	byDigest := map[string]*planGroup{}
	for _, g := range p.Profiles {
		byDigest[g.Profile] = g
	}
	tvGroup, filmsGroup := byDigest[digests[0]], byDigest[digests[1]]
	if tvGroup == nil || filmsGroup == nil {
		t.Fatalf("the groups are not keyed by the resolved profile digests %v: %+v", digests, byDigest)
	}
	if tvGroup.Eligible.Files != 1 || filmsGroup.Eligible.Files != 2 {
		t.Fatalf("eligible per profile = %d and %d, want 1 (tv) and 2 (films)",
			tvGroup.Eligible.Files, filmsGroup.Eligible.Files)
	}
	// No number spans the two: the bytes add up to the total and neither group carries the
	// other's.
	if tvGroup.Eligible.Bytes+filmsGroup.Eligible.Bytes != p.Total.Eligible.Bytes {
		t.Fatalf("%d + %d bytes per profile != %d total", tvGroup.Eligible.Bytes,
			filmsGroup.Eligible.Bytes, p.Total.Eligible.Bytes)
	}
	if tvGroup.Projection.Made {
		t.Fatalf("a profile with no history of its own was projected for anyway: %+v", tvGroup.Projection)
	}
	if !filmsGroup.Projection.Made {
		t.Fatalf("the profile WITH history was refused a projection: %+v", filmsGroup.Projection)
	}
	if filmsGroup.Projection.Reclaim.Sample.CompletedEncodes != 1 {
		t.Fatalf("the films projection was drawn from %d encode(s), want the 1 decided under it",
			filmsGroup.Projection.Reclaim.Sample.CompletedEncodes)
	}

	got := planReport(t, cfgPath)
	for _, want := range []string{"encode profile " + digests[0], "encode profile " + digests[1], tv, films} {
		if !strings.Contains(got, want) {
			t.Fatalf("the report never names %q:\n%s", want, got)
		}
	}
}

// TestPlan_IsNotAnAliasForDryRun is AC-8: a daemon pass with dry_run enabled records a
// terminal row per file it decided, and plan over that same library records none and takes
// no daemon pass - so a caller invoking one never gets the other's observable effect.
func TestPlan_IsNotAnAliasForDryRun(t *testing.T) {
	// dry_run DOES record, and this is the no-regression half.
	cfgPath, lib, state := planLibrary(t, "")
	decided := daemonPassPaths(t, cfgPath)
	for _, src := range planSources(lib) {
		if _, ok := decided[src]; !ok {
			t.Fatalf("a dry run recorded no terminal row for %s: %+v", src, decided)
		}
	}
	if decided[filepath.Join(lib, "movie.mkv")] != string(store.WouldTranscode) {
		t.Fatalf("a dry run did not record would-transcode for an eligible file: %+v", decided)
	}

	// plan does NOT, over a library whose ledger already holds those rows.
	before := jobRowCount(t, state)
	stateBefore := treeSnapshot(t, state)
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"plan", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("plan code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if got := jobRowCount(t, state); got != before {
		t.Fatalf("plan moved the ledger from %d row(s) to %d", before, got)
	}
	assertSameTree(t, "the state directory", stateBefore, treeSnapshot(t, state))
	// And it says so in the report, because the pair is what an operator confuses.
	if !strings.Contains(out.String(), "dry_run") || !strings.Contains(out.String(), "records none") {
		t.Fatalf("the report never separates plan from dry_run:\n%s", out.String())
	}
}

// TestPlan_ProbesEachCoveredFileAtMostOnce is AC-9: one invocation is one pass over the
// library, so no covered file is probed twice.
//
// The seam wraps the REAL prober and counts; nothing stands in for the subject.
func TestPlan_ProbesEachCoveredFileAtMostOnce(t *testing.T) {
	cfgPath, lib, _ := planLibrary(t, "")

	var mu sync.Mutex
	seen := map[string]int{}
	planSnapshotWrap = func(real func(context.Context, string) *probe.VideoProps) func(context.Context, string) *probe.VideoProps {
		return func(ctx context.Context, path string) *probe.VideoProps {
			mu.Lock()
			seen[path]++
			mu.Unlock()
			return real(ctx, path)
		}
	}
	t.Cleanup(func() { planSnapshotWrap = nil })

	p := planJSON(t, cfgPath)

	mu.Lock()
	defer mu.Unlock()
	for path, n := range seen {
		if n > 1 {
			t.Fatalf("%s was probed %d times in one invocation", path, n)
		}
	}
	// Every covered file was probed, so "at most once" is not satisfied by probing nothing.
	for _, src := range planSources(lib) {
		if seen[src] != 1 {
			t.Fatalf("%s was probed %d time(s), want exactly 1: %+v", src, seen[src], seen)
		}
	}
	if p.Probes != len(seen) {
		t.Fatalf("the plan reports %d probe snapshot(s), the prober was asked for %d", p.Probes, len(seen))
	}
	if p.Probes != int(p.Total.Covered.Files) {
		t.Fatalf("%d probe snapshot(s) over %d covered file(s)", p.Probes, p.Total.Covered.Files)
	}
}

// plannedSet is the set of files one read-only pass covered, and the pass itself.
func plannedSet(t *testing.T, cfgPath string) (*engine.PlanPass, map[string]bool) {
	t.Helper()
	pass := planPassOver(t, cfgPath)
	planned := map[string]bool{}
	for _, f := range pass.Files {
		planned[f.Path] = true
	}
	return pass, planned
}

// planEqualsDaemonPass is AC-10's equality, over whatever library cfgPath describes: the dry
// run's terminal rows are the set that pass covered, and the plan pass's files are the set
// plan covers. It is a helper so the criterion can be graded over SEVERAL libraries, which is
// the only way an equality about coverage can be trusted - one fixture proves it holds where
// the two agree, never that it holds where they could differ.
func planEqualsDaemonPass(t *testing.T, cfgPath string) (*engine.PlanPass, map[string]bool) {
	t.Helper()
	pass, planned := plannedSet(t, cfgPath)
	covered := map[string]bool{}
	for p := range daemonPassPaths(t, cfgPath) {
		covered[p] = true
	}
	if !sameSet(planned, covered) {
		t.Fatalf("plan covers a different set of files than the daemon pass does:\n  plan:   %v\n  daemon: %v",
			sortedSet(planned), sortedSet(covered))
	}
	return pass, planned
}

// tabNamedSource writes a real, probeable source whose NAME carries a literal tab, which is a
// path the pipeline declines outright. It skips where the filesystem will not hold one.
func tabNamedSource(t *testing.T, lib string) string {
	t.Helper()
	tabbed := filepath.Join(lib, "two\tnames.mkv")
	encodeFixture(t, tabbed, "libx264", "256x144", "yuv420p")
	if _, err := os.Stat(tabbed); err != nil {
		t.Skipf("this filesystem will not hold a tab in a file name: %v", err)
	}
	return tabbed
}

// socketNamedSource puts a unix socket INODE under a configured video extension: an entry
// that is neither a regular file nor a symbolic link. A scan enumerates an entry by NAME, so
// this is the case where a report carrying its own rule about what KIND of entry counts
// drifts from the enumeration every other reader shares. mknod(2) lets an unprivileged caller
// make one, and a socket is used rather than a FIFO because opening a FIFO with no writer
// blocks.
func socketNamedSource(t *testing.T, lib string) string {
	t.Helper()
	sock := filepath.Join(lib, "stream.mkv")
	if err := syscall.Mknod(sock, syscall.S_IFSOCK|0o600, 0); err != nil {
		t.Skipf("this filesystem will not hold a unix socket inode: %v", err)
	}
	return sock
}

// TestPlan_CoverageEqualsDaemonPass is AC-10: plan reports exactly the set of files the
// daemon's own scanning pass covers - same root membership, same extension handling, same
// treatment of everything the configuration holds back - rather than a coverage rule of its
// own.
//
// It is graded as an EQUALITY against a real daemon pass over FIVE libraries, and the four
// beyond the first are the ones that can tell a shared decision from two copies of it: a path
// the pipeline declines outright before it claims or probes anything, a symbolic link carrying
// a source name, which a scan enumerates and then skips at a named guard, a symbolic link onto
// nothing, and an entry that is neither a regular file nor a link.
func TestPlan_CoverageEqualsDaemonPass(t *testing.T) {
	t.Run("the library a scan covers", func(t *testing.T) {
		cfgPath, lib, _ := planLibrary(t, "")
		_, planned := planEqualsDaemonPass(t, cfgPath)
		// And it is the right set, so the equality is not two commands agreeing on nothing.
		if !sameSet(planned, setOf(planSources(lib))) {
			t.Fatalf("the covered set is not the library's sources:\n  covered: %v", sortedSet(planned))
		}

		// The published report does not lose any of them on the way out.
		p := planJSON(t, cfgPath)
		if p.Total.Covered.Files != int64(len(planned)) {
			t.Fatalf("the report publishes %d covered file(s) over a pass that covered %d",
				p.Total.Covered.Files, len(planned))
		}
		if p.Total.Covered.Files != p.Total.Eligible.Files+p.Total.Skipped+p.Total.Unaccounted.Files {
			t.Fatalf("the covered figure does not account for itself: %d != %d + %d + %d",
				p.Total.Covered.Files, p.Total.Eligible.Files, p.Total.Skipped, p.Total.Unaccounted.Files)
		}
	})

	// ProcessFile declines a path carrying a literal tab or newline before it claims, probes
	// or records anything, so a run never touches one. A plan that counted it among the files
	// a run would transcode would be promising something no run will do, which is the exact
	// shape of confident wrong result this repository's fail-safe rule forbids.
	t.Run("a path the pipeline declines outright", func(t *testing.T) {
		cfgPath, lib, _ := planLibrary(t, "")
		tabbed := tabNamedSource(t, lib)

		_, planned := planEqualsDaemonPass(t, cfgPath)
		if planned[tabbed] {
			t.Fatalf("plan covers %q, which the daemon pass declines without recording anything", tabbed)
		}

		// It is reported with its reason rather than dropped, in both forms: a path silently
		// missing from a report about a library reads as a path that is not there.
		p := planJSON(t, cfgPath)
		if p.Total.Eligible.Files != 2 {
			t.Fatalf("plan reports %d eligible file(s) over a library whose daemon pass would transcode 2",
				p.Total.Eligible.Files)
		}
		if p.Declined.Files != 1 || len(p.Declined.Paths) != 1 || p.Declined.Paths[0].Path != tabbed {
			t.Fatalf("the declined path is not published: %+v", p.Declined)
		}
		if got := p.Declined.Paths[0].Rule; got != engine.RuleUnsupportedCharacters {
			t.Fatalf("the declined path names rule %q, want %q", got, engine.RuleUnsupportedCharacters)
		}
		if p.Declined.Paths[0].Detail == "" || p.Declined.Note == "" {
			t.Fatalf("a declined path is published with no reason: %+v", p.Declined)
		}
		if rep := planReport(t, cfgPath); !strings.Contains(rep, tabbed) ||
			!strings.Contains(rep, engine.RuleUnsupportedCharacters) {
			t.Fatalf("the written report never names the declined path or its rule:\n%s", rep)
		}
	})

	// A symbolic link carrying a source name IS enumerated by a scan - membership is decided
	// by NAME - and is then stopped at the symlinked-source guard with a terminal row to show
	// for it. So plan covers it too, under that same guard.
	t.Run("a symbolic link carrying a source name", func(t *testing.T) {
		cfgPath, lib, _ := planLibrary(t, "")
		linked := filepath.Join(lib, "linked.mkv")
		if err := os.Symlink(filepath.Join(lib, "movie.mkv"), linked); err != nil {
			t.Fatal(err)
		}

		pass, planned := planEqualsDaemonPass(t, cfgPath)
		if !planned[linked] {
			t.Fatalf("plan does not cover %q, which the daemon pass enumerates and records a skip for", linked)
		}
		for _, f := range pass.Files {
			if f.Path == linked && f.Guard != engine.SkipSymlink {
				t.Fatalf("plan reports %q under guard %q, want %q", linked, f.Guard, engine.SkipSymlink)
			}
		}
	})

	// A symbolic link with NOTHING at the other end is the same case from the other side: a
	// scan enumerates it by name just as it enumerates a live one, and the daemon's door then
	// finds no file to act on and returns without claiming, probing or recording anything.
	t.Run("a symbolic link onto nothing", func(t *testing.T) {
		cfgPath, lib, _ := planLibrary(t, "")
		dangling := filepath.Join(lib, "dangling.mkv")
		if err := os.Symlink(filepath.Join(lib, "nowhere.mkv"), dangling); err != nil {
			t.Fatal(err)
		}

		_, planned := planEqualsDaemonPass(t, cfgPath)
		if planned[dangling] {
			t.Fatalf("plan covers %q, which has no file at the other end and which the daemon pass "+
				"declines without recording anything", dangling)
		}

		p := planJSON(t, cfgPath)
		if p.Total.Covered.Files != int64(len(planSources(lib))) {
			t.Fatalf("plan covers %d file(s) over a library of %d sources plus one dangling link",
				p.Total.Covered.Files, len(planSources(lib)))
		}
		if p.Declined.Files != 1 || len(p.Declined.Paths) != 1 || p.Declined.Paths[0].Path != dangling {
			t.Fatalf("the dangling link is not published as declined: %+v", p.Declined)
		}
		if got := p.Declined.Paths[0].Rule; got != engine.RuleNotARegularFile {
			t.Fatalf("the dangling link names rule %q, want %q", got, engine.RuleNotARegularFile)
		}
	})

	// An entry that is neither a regular file NOR a symbolic link - a socket, a FIFO, a device
	// node - carrying a source name. Membership is decided by NAME, so a scan enumerates it and
	// the daemon's door finds something there to act on: it claims it, probes it, and leaves a
	// terminal row. plan covers it for exactly that reason, as one it could not account for
	// rather than one a guard decided.
	t.Run("an entry that is neither a regular file nor a link", func(t *testing.T) {
		cfgPath, lib, _ := planLibrary(t, "")
		sock := socketNamedSource(t, lib)

		pass, planned := planEqualsDaemonPass(t, cfgPath)
		if !planned[sock] {
			t.Fatalf("plan does not cover %q, which the daemon pass enumerates and records a row for", sock)
		}
		for _, f := range pass.Files {
			if f.Path == sock && !f.Unreadable {
				t.Fatalf("plan accounts for %q under guard %q; the probe can answer nothing about a "+
					"socket, so it is a file this plan could not account for", sock, f.Guard)
			}
		}
	})
}

// TestPlan_AgreesWithAnalyzeCoverage is AC-11. The build DOES contain `analyze`, so this
// grades for real rather than skipping: both commands run over the same library with the
// same configuration and must report the identical covered file set, and neither may name a
// file the other accounts for differently.
// It is graded over FIVE libraries for the same reason AC-10 is: a single fixture on which the
// two commands happen to agree cannot tell a shared decision from two copies of one, and the
// cases below are precisely where two copies drift - a symbolic link carrying a source name, a
// path the pipeline declines outright, a symbolic link onto nothing, and an entry that is
// neither a regular file nor a link.
func TestPlan_AgreesWithAnalyzeCoverage(t *testing.T) {
	if !buildHasAnalyze() {
		// Kept rather than omitted (testing T2). It cannot fire in this build, where
		// `analyze` is in dispatch; it exists so a build without the census says so.
		t.Skip("this build has no analyze command, so there is no second reading to agree with")
	}

	t.Run("the library a scan covers", func(t *testing.T) {
		cfgPath, lib, _ := planLibrary(t, "")
		c, p := planAndAnalyzeAgree(t, cfgPath)
		if p.Total.Covered.Files != int64(len(planSources(lib))) {
			t.Fatalf("both agree on %d file(s), which is not the library's %d sources",
				p.Total.Covered.Files, len(planSources(lib)))
		}

		// The per-file reason agrees too: every file analyze withheld, and the mechanism it
		// named, is a file plan does not cover at all.
		_, planned := plannedSet(t, cfgPath)
		withheld := map[string]bool{
			filepath.Join(lib, "notes.txt"):                                 true,
			filepath.Join(lib, "cover.jpg"):                                 true,
			filepath.Join(lib, "movie."+engine.TempMarker+".mkv"):           true,
			filepath.Join(lib, "show", "ep1."+engine.RetainedMarker+".mkv"): true,
		}
		for path := range withheld {
			if planned[path] {
				t.Fatalf("plan covers %s, which analyze accounts for as withheld", path)
			}
		}
		var named int64
		for _, m := range c.Total.Withheld {
			if m.Name != mechIrregular {
				named += m.Files
			}
		}
		if named == 0 {
			t.Fatal("analyze named no withholding mechanism, so there was nothing to agree about")
		}
	})

	// A symbolic link carrying a source name: a scan enumerates it and then skips it at the
	// symlinked-source guard, so the census counts it as a source and plan covers it. Both
	// count the LINK's own bytes; the target's are counted once already, for the file itself.
	t.Run("a symbolic link carrying a source name", func(t *testing.T) {
		cfgPath, lib, _ := planLibrary(t, "")
		if err := os.Symlink(filepath.Join(lib, "movie.mkv"), filepath.Join(lib, "linked.mkv")); err != nil {
			t.Fatal(err)
		}
		_, p := planAndAnalyzeAgree(t, cfgPath)
		if want := int64(len(planSources(lib)) + 1); p.Total.Covered.Files != want {
			t.Fatalf("both agree on %d file(s), want the library's %d sources plus the link",
				p.Total.Covered.Files, want)
		}
	})

	// A path the pipeline declines outright is in neither report's source set: the daemon
	// claims, probes and records nothing about one, so naming it a source in either would
	// name a file nothing will ever touch.
	t.Run("a path the pipeline declines outright", func(t *testing.T) {
		cfgPath, lib, _ := planLibrary(t, "")
		tabbed := tabNamedSource(t, lib)
		c, p := planAndAnalyzeAgree(t, cfgPath)
		if p.Total.Covered.Files != int64(len(planSources(lib))) {
			t.Fatalf("both agree on %d file(s), which is not the library's %d sources - the extra is %q",
				p.Total.Covered.Files, len(planSources(lib)), tabbed)
		}
		var declined int64
		for _, m := range c.Total.Withheld {
			if m.Name == mechDeclined {
				declined = m.Files
			}
		}
		if declined != 1 {
			t.Fatalf("analyze withheld %d path(s) under %q, want 1", declined, mechDeclined)
		}
	})

	// A symbolic link onto NOTHING is in neither source set: a scan enumerates it by name,
	// and the daemon's door then finds no file to act on.
	t.Run("a symbolic link onto nothing", func(t *testing.T) {
		cfgPath, lib, _ := planLibrary(t, "")
		if err := os.Symlink(filepath.Join(lib, "nowhere.mkv"), filepath.Join(lib, "dangling.mkv")); err != nil {
			t.Fatal(err)
		}
		c, p := planAndAnalyzeAgree(t, cfgPath)
		if p.Total.Covered.Files != int64(len(planSources(lib))) {
			t.Fatalf("both agree on %d file(s), which is not the library's %d sources - the extra is "+
				"a link with nothing at the other end", p.Total.Covered.Files, len(planSources(lib)))
		}
		var irregular int64
		for _, m := range c.Total.Withheld {
			if m.Name == mechIrregular {
				irregular = m.Files
			}
		}
		if irregular != 1 {
			t.Fatalf("analyze withheld %d entry(ies) under %q, want the dangling link", irregular, mechIrregular)
		}
	})

	// An entry that is neither a regular file NOR a symbolic link, carrying a source name. A
	// scan enumerates it by name like any other entry and a run records a terminal row for it,
	// so it is a source in both reports or in neither: a rule of the census's own about what
	// KIND of entry counts is exactly where two copies of one membership rule drift apart.
	t.Run("an entry that is neither a regular file nor a link", func(t *testing.T) {
		cfgPath, lib, _ := planLibrary(t, "")
		sock := socketNamedSource(t, lib)
		c, p := planAndAnalyzeAgree(t, cfgPath)
		if want := int64(len(planSources(lib)) + 1); p.Total.Covered.Files != want {
			t.Fatalf("both agree on %d file(s), want the library's %d sources plus %s",
				p.Total.Covered.Files, len(planSources(lib)), sock)
		}
		var irregular int64
		for _, m := range c.Total.Withheld {
			if m.Name == mechIrregular {
				irregular = m.Files
			}
		}
		if irregular != 0 {
			t.Fatalf("analyze withheld %d entry(ies) under %q; a run acts on this one, so no report "+
				"may hold it back", irregular, mechIrregular)
		}
	})
}

// planAndAnalyzeAgree is AC-11's equality over whatever library cfgPath describes: an
// identical covered file set, by count and by bytes, from both commands.
func planAndAnalyzeAgree(t *testing.T, cfgPath string) (census, *plan) {
	t.Helper()
	c := analyzeJSON(t, cfgPath)
	p := planJSON(t, cfgPath)
	if p.Total.Covered.Files != c.Total.Sources.Files {
		t.Fatalf("plan covers %d file(s), analyze counts %d source(s) over the same library with the "+
			"same configuration", p.Total.Covered.Files, c.Total.Sources.Files)
	}
	if p.Total.Covered.Bytes != c.Total.Sources.Bytes {
		t.Fatalf("plan covers %d byte(s), analyze counts %d",
			p.Total.Covered.Bytes, c.Total.Sources.Bytes)
	}
	return c, p
}

// buildHasAnalyze reports whether this build carries the census command, which is what
// AC-11's trigger is written against.
func buildHasAnalyze() bool {
	var out, errOut bytes.Buffer
	return dispatch([]string{"analyze", "-h"}, &out, &errOut) == 0
}

// TestPlan_NoPerFileEstimateOrScoreProjection is AC-12: no per-file estimated saving and no
// predicted perceptual or VMAF score appears anywhere in any plan output.
//
// It is a NEGATIVE criterion and is graded against an output that HAS a projection, so
// "nothing was found" is not the same as "nothing was published".
func TestPlan_NoPerFileEstimateOrScoreProjection(t *testing.T) {
	cfgPath, lib, state := planLibrary(t, "")
	seedCompletedEncodes(t, state, rootDigests(t, cfgPath)[0], [2]int64{1000, 400})

	var jsonOut, errOut bytes.Buffer
	if code := dispatch([]string{"plan", "--json", "--config", cfgPath}, &jsonOut, &errOut); code != 0 {
		t.Fatalf("plan --json code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	forms := map[string]string{"the report": planReport(t, cfgPath), "the document": jsonOut.String()}

	for what, got := range forms {
		// A projection IS present, so the assertions below are about its shape rather than
		// about an output that projected nothing.
		if !strings.Contains(strings.ToLower(got), "estimat") {
			t.Fatalf("%s carries no projection at all, so this proves nothing:\n%s", what, got)
		}
		// The PATHS are removed before the banned words are looked for, for the reason the
		// per-line check below already removes them: a path is not a projection. The report
		// prints the library root and the ledger, both of which live under the temporary
		// directory this process was given, and that directory is named after whatever
		// started it - so a grader reading the whole output would fire on a word that
		// appears only in the name of a scratch directory and nowhere in the report's own
		// figures.
		lower := strings.ToLower(withoutPaths(got, lib, state, cfgPath, os.TempDir()))
		for _, banned := range []string{"vmaf", "perceptual"} {
			if strings.Contains(lower, banned) {
				t.Fatalf("%s carries a %q projection for files nothing has encoded:\n%s", what, banned, got)
			}
		}
		// No line names one of the library's files AND an estimate or a saving: a
		// library-scale ratio printed against one file reads as a measurement of that file.
		//
		// The path is REMOVED before the rest of the line is read, because a path is not a
		// figure: a temp directory is named after the test that made it, so a grader that
		// searched the whole line would fire on this test's own directory name.
		for _, line := range strings.Split(got, "\n") {
			for _, src := range planSources(lib) {
				if !strings.Contains(line, src) {
					continue
				}
				rest := strings.ToLower(strings.ReplaceAll(line, src, " "))
				if strings.Contains(rest, "estimat") || strings.Contains(rest, "saving") {
					t.Fatalf("%s carries a per-file estimate: %q", what, line)
				}
			}
		}
	}
}

// withoutPaths blanks each given path wherever it appears in s, longest first so a path
// that contains another is removed whole rather than left in pieces. It is how a grader
// reads what an output SAYS rather than where this process happened to put its files.
func withoutPaths(s string, paths ...string) string {
	sort.Slice(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })
	for _, p := range paths {
		if p == "" {
			continue
		}
		s = strings.ReplaceAll(s, p, " ")
	}
	return s
}

// TestPlan_UnreadableFileIsCountedNotFatal is AC-13: a file or directory that cannot be read
// or probed is counted as unreadable WITH ITS REASON, the walk continues, and the totals say
// how many files the report could not account for rather than hiding them in a confident
// total.
func TestPlan_UnreadableFileIsCountedNotFatal(t *testing.T) {
	cfgPath, lib, _ := planLibrary(t, "")
	// A file carrying a configured video extension that no probe can answer for. It is
	// COVERED - the enumeration decides membership by name - and is therefore exactly the
	// file a plan must neither count as eligible nor quietly drop.
	broken := filepath.Join(lib, "broken.mkv")
	writeCensusFile(t, broken, "this is not a media file at all\n")

	p := planJSON(t, cfgPath)
	if p.Total.Unaccounted.Files != 1 {
		t.Fatalf("unaccounted = %d file(s), want the 1 that cannot be probed: %+v",
			p.Total.Unaccounted.Files, p.Total.Unaccounted)
	}
	if len(p.Total.Unaccounted.Reasons) != 1 ||
		p.Total.Unaccounted.Reasons[0].Path != broken ||
		p.Total.Unaccounted.Reasons[0].Reason == "" {
		t.Fatalf("the unreadable file is not reported with its reason: %+v", p.Total.Unaccounted.Reasons)
	}
	// The walk continued: the two eligible files are still there, and the unreadable one is
	// in neither the eligible nor the skipped figure.
	if p.Total.Eligible.Files != 2 {
		t.Fatalf("the walk did not continue past the unreadable file: %+v", p.Total.Eligible)
	}
	if p.Total.Covered.Files != p.Total.Eligible.Files+p.Total.Skipped+p.Total.Unaccounted.Files {
		t.Fatalf("the totals hide the file they could not account for: %d != %d + %d + %d",
			p.Total.Covered.Files, p.Total.Eligible.Files, p.Total.Skipped, p.Total.Unaccounted.Files)
	}
	got := planReport(t, cfgPath)
	if !strings.Contains(got, "unaccounted for: 1") || !strings.Contains(got, broken) {
		t.Fatalf("the report does not say how many files it could not account for:\n%s", got)
	}

	// A DIRECTORY the process may not read, which bounds the plan rather than failing it.
	t.Run("permission denied on a directory", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: a mode-000 directory is still readable, so there is nothing to deny")
		}
		cfgPath, lib, _ := planLibrary(t, "")
		locked := filepath.Join(lib, "locked")
		if err := os.MkdirAll(locked, 0o755); err != nil {
			t.Fatal(err)
		}
		encodeFixture(t, filepath.Join(locked, "hidden.mkv"), "libx264", "256x144", "yuv420p")
		if err := os.Chmod(locked, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

		var out, errOut bytes.Buffer
		if code := dispatch([]string{"plan", "--json", "--config", cfgPath}, &out, &errOut); code != 0 {
			t.Fatalf("a directory it may not read made plan exit %d, want 0 (stderr: %s)",
				code, errOut.String())
		}
		var p plan
		if err := json.Unmarshal(out.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		if p.Coverage.DirectoriesNotRead == 0 {
			t.Fatalf("the plan reports every directory as read despite one it may not list: %+v", p.Coverage)
		}
		if p.Coverage.Boundary == "" {
			t.Fatal("the coverage boundary - what is inside an unread directory is UNKNOWN - is never stated")
		}
		if p.Total.Eligible.Files != 2 {
			t.Fatalf("the walk did not continue past the unreadable directory: %+v", p.Total.Eligible)
		}
	})
}

// TestPlan_EmptyEligibleSetReportsZero is AC-14: with nothing eligible, the plan reports zero
// eligible files and zero eligible bytes with the skip breakdown intact, exits zero, and
// presents NO projection - a zero reclaim over an empty population would be a number about
// nothing.
func TestPlan_EmptyEligibleSetReportsZero(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	state := filepath.Join(dir, "state")
	// Every source is already at the target codec, so every one of them is skipped by a
	// named guard and none is eligible.
	encodeFixture(t, filepath.Join(lib, "a.mkv"), "libx265", "320x240", "yuv420p")
	encodeFixture(t, filepath.Join(lib, "b.mkv"), "libx265", "256x144", "yuv420p")

	cfgPath := filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + state +
		"\nmin_bitrate_kbps: 0\nvmaf_enable: false\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	substitute(t, fixedType("ext4"))
	// History exists, so a projection COULD have been made if the command projected over an
	// empty population - which is the mistake this criterion forbids.
	seedCompletedEncodes(t, state, rootDigests(t, cfgPath)[0], [2]int64{1000, 400})

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"plan", "--json", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("an empty eligible set exits %d, want 0 (stderr: %s)", code, errOut.String())
	}
	var p plan
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if p.Total.Eligible.Files != 0 || p.Total.Eligible.Bytes != 0 {
		t.Fatalf("eligible = %d file(s) %d byte(s), want zero of each", p.Total.Eligible.Files, p.Total.Eligible.Bytes)
	}
	if p.Total.Skipped != 2 {
		t.Fatalf("the skip breakdown was lost with the eligible set: %+v", p.Total.SkippedByGuard)
	}
	byGuard := map[string]int64{}
	for _, b := range p.Total.SkippedByGuard {
		byGuard[b.Key] = b.Count
	}
	if byGuard[engine.SkipAlreadyTargetCodec] != 2 {
		t.Fatalf("the breakdown does not attribute both skips to their guard: %+v", p.Total.SkippedByGuard)
	}
	if p.Total.Projection.Made {
		t.Fatalf("a reclaim was projected over an empty eligible set: %+v", p.Total.Projection)
	}
	if !strings.Contains(p.Total.Projection.Refused, "nothing to project") {
		t.Fatalf("the refusal does not say there is nothing to project over: %q", p.Total.Projection.Refused)
	}
}

// TestPlan_ExitCodeVocabulary is AC-15 and cli L1/L1a: 1 is "it could not run", 2 is "the
// caller got the invocation wrong", and 0 is "a report was produced" - a refused projection
// included.
func TestPlan_ExitCodeVocabulary(t *testing.T) {
	cfgPath, _, _ := planLibrary(t, "")
	dir := t.TempDir()

	badCfg := filepath.Join(dir, "invalid.yaml")
	if err := os.WriteFile(badCfg, []byte("library_roots:\n  - relative/path\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"a config that will not load", []string{"plan", "--config", filepath.Join(dir, "absent.yaml")}, 1},
		{"a config that will not validate", []string{"plan", "--config", badCfg}, 1},
		{"an unknown flag", []string{"plan", "--nope", "--config", cfgPath}, 2},
		{"no --config at all", []string{"plan"}, 2},
		{"a positional argument", []string{"plan", "--config", cfgPath, "extra"}, 2},
		{"a report, with the projection refused", []string{"plan", "--config", cfgPath}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			got := dispatch(tc.args, &out, &errOut)
			if got != tc.want {
				t.Fatalf("exit %d, want %d (stdout: %s) (stderr: %s)", got, tc.want, out.String(), errOut.String())
			}
			if tc.want == 0 {
				return
			}
			// A failure says why, on stderr, and writes nothing to stdout.
			if errOut.Len() == 0 {
				t.Fatal("a non-zero exit wrote no reason to stderr")
			}
			if out.Len() != 0 {
				t.Fatalf("a non-zero exit wrote to stdout: %q", out.String())
			}
		})
	}

	// 0 really does mean a report was produced, and the refusal is inside it.
	p := planJSON(t, cfgPath)
	if p.Total.Projection.Made {
		t.Fatal("this fixture has no history, so the exit-0 case is not the refused one")
	}
}

// TestPlan_HelpListsFlagsAndExitCodes is AC-16 and cli L5: `holdfast plan -h` lists every
// flag, every exit code with its meaning and one runnable example, and the top-level usage
// lists plan among its commands.
func TestPlan_HelpListsFlagsAndExitCodes(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"plan", "-h"}, &out, &errOut); code != 0 {
		t.Fatalf("plan -h exits %d, want 0", code)
	}
	help := out.String() + errOut.String()
	for _, want := range []string{
		"--config", "--json", // every flag
		"0 ", "1 ", "2 ", // every exit code
		"Exit codes:",
		"holdfast plan --config", // one runnable example
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("plan -h never mentions %q:\n%s", want, help)
		}
	}
	// Each code appears with its MEANING, not as a bare number.
	for _, want := range []string{"a report was produced", "could not be made", "invocation was wrong"} {
		if !strings.Contains(help, want) {
			t.Fatalf("plan -h lists an exit code without its meaning (%q):\n%s", want, help)
		}
	}

	var topOut, topErr bytes.Buffer
	if code := dispatch([]string{"-h"}, &topOut, &topErr); code != 0 {
		t.Fatalf("holdfast -h exits %d, want 0", code)
	}
	if !strings.Contains(topOut.String(), "\n  plan ") {
		t.Fatalf("the top-level usage does not list plan among its commands:\n%s", topOut.String())
	}
}

// TestPlan_EmitsProgressWhileItRuns is `cli` L4: a command that can exceed thirty seconds
// emits progress to stderr at least that often. A plan takes one probe snapshot per covered
// file, so a real library is minutes to hours of ffprobe, and a silent command is one a
// supervisor kills and retries.
//
// It is graded the way the clause says to grade it - emission during a SLOWED run - with the
// interval shortened and the phase delayed, so what is asserted is the reporter rather than
// how long these fixtures happen to take. The clause binds the COMMAND, so BOTH phases a plan
// spends time in are graded: the walk that establishes coverage, and the probe pass over what
// it found. The walk is the one with no count of its own, which is why it is the one that
// would go silent.
func TestPlan_EmitsProgressWhileItRuns(t *testing.T) {
	// The clause binds the SHIPPED command, so the interval this build ships is asserted
	// first. Without this the case would prove only that a reporter it configured itself
	// works, and would stay green over a build that reports every hour or not at all.
	was := planProgressEvery
	if was <= 0 || was > 30*time.Second {
		t.Fatalf("plan ships a progress interval of %s; cli L4 binds it to at most thirty seconds", was)
	}
	planProgressEvery = 5 * time.Millisecond
	t.Cleanup(func() { planProgressEvery = was })

	t.Run("the probe pass", func(t *testing.T) {
		cfgPath, _, _ := planLibrary(t, "")
		planSnapshotWrap = func(real func(context.Context, string) *probe.VideoProps) func(context.Context, string) *probe.VideoProps {
			return func(ctx context.Context, path string) *probe.VideoProps {
				time.Sleep(40 * time.Millisecond)
				return real(ctx, path)
			}
		}
		t.Cleanup(func() { planSnapshotWrap = nil })
		errOut := planSaysItIsStillRunning(t, cfgPath)
		if !strings.Contains(errOut, "probed so far") {
			t.Fatalf("a slowed probe pass never said it was still running:\n%s", errOut)
		}
	})

	// The walk runs BEFORE a single file is probed, and on a library of the scale this command
	// is written for it is the part that can cross thirty seconds on its own. A reporter
	// started after it would leave exactly that stretch silent.
	t.Run("the library walk before it", func(t *testing.T) {
		cfgPath, _, _ := planLibrary(t, "")
		real := planWalk
		planWalk = func(cfg *config.Config) startup.Result {
			time.Sleep(60 * time.Millisecond)
			return real(cfg)
		}
		t.Cleanup(func() { planWalk = real })
		errOut := planSaysItIsStillRunning(t, cfgPath)
		if !strings.Contains(errOut, "still walking the library") {
			t.Fatalf("a slowed walk never said it was still running:\n%s", errOut)
		}
	})
}

// planSaysItIsStillRunning runs the slowed command and returns its stderr, having asserted the
// half of L4 that is the same in both phases: L2 holds while progress is emitted, so stdout
// still carries the one whole document and nothing else.
func planSaysItIsStillRunning(t *testing.T, cfgPath string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"plan", "--json", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("plan --json code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	var p plan
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		t.Fatalf("the plan document does not parse whole: %v\n%s", err, out.String())
	}
	return errOut.String()
}

// --- small set helpers, so a failure prints the difference rather than two lengths ------

func setOf(paths []string) map[string]bool {
	out := map[string]bool{}
	for _, p := range paths {
		out[p] = true
	}
	return out
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedSet(s map[string]bool) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
