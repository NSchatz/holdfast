package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

// What a plan IS, and how it is published in the two forms the criteria pin: a written
// report by default, and exactly one JSON document behind --json.
//
// Both are rendered from the SAME value, so "the document carries every figure the report
// carries" is a property of there being one source rather than a promise to keep two
// renderers in step. The report deliberately does not parse as JSON - it opens with a
// sentence - so a script cannot mistake the default output for the machine-readable form.
//
// TWO THINGS ARE DELIBERATELY ABSENT FROM BOTH, and their absence is the criterion rather
// than an omission:
//
//   - no PER FILE estimated saving. A library-scale ratio printed against one file reads as
//     a measurement of that file, and this install has measured nothing about it. The
//     estimate is published once per population, never once per path.
//   - no predicted perceptual or VMAF score, anywhere. A VMAF score is the output of
//     comparing an encode against its source, so predicting one for a file nobody has
//     encoded would be inventing the evidence the gate exists to collect.

// plan is the whole report. It is the ONE value both output forms are rendered from.
type plan struct {
	Command  string         `json:"command"`
	Storage  storageVerdict `json:"storage"`
	Ledger   string         `json:"ledger"`
	Coverage planCoverage   `json:"coverage"`
	// Roots is the scope of each configured library root, one entry per root in
	// configuration order: what the library under it is before any filter, and what of it
	// the filters and the record hold-backs kept out (S0180).
	Roots    []*planRoot  `json:"roots"`
	Profiles []*planGroup `json:"profiles"`
	Total    *planGroup   `json:"total"`
	// Declined is every enumerated path the pipeline refuses outright, which is in no
	// figure above because a run claims, probes and records nothing about one either.
	Declined planDeclinedPaths `json:"declined"`
	// Probes is how many probe snapshots this invocation took, which is what makes "one
	// invocation is one pass over the library" a figure a reader can check rather than a
	// claim they have to take.
	Probes int    `json:"probe_snapshots_taken"`
	Note   string `json:"note"`
}

// planCoverage is what the startup walk read and what it did not, restricted to the
// configured library roots.
type planCoverage struct {
	DirectoriesRead    int64    `json:"directories_read"`
	DirectoriesNotRead int64    `json:"directories_not_read"`
	NotReadWhy         []bucket `json:"not_read_reasons,omitempty"`
	Boundary           string   `json:"boundary"`
}

// planRoot is one configured library root's SCOPE: the path filters in force for it, the
// whole source-named library under it BEFORE those filters, the record hold-backs or the
// outright refusals, what the filters kept out, what a record held back, and what is
// covered and eligible after them - with the directory coverage it was all read from.
//
// It exists so "how big is this library, and how much of it is in this tool's scope" has
// one answer with the difference named: library = covered + excluded_by_path_filter +
// held_back_by_record + the declined paths under this root, in files and in bytes. A
// directory the startup walk did not list - one it could not read, or one an exclude
// pattern reaches whole, which it prunes - is counted in directories_not_read and its files
// are in NO figure here, because nobody listed them.
//
// It carries these keys and no others, and deliberately no reclaim: a saving is projected
// only from this install's own completed encodes, per resolved profile, never as a fixed
// fraction of a root's eligible bytes.
type planRoot struct {
	Root                 string   `json:"root"`
	ExcludePaths         []string `json:"exclude_paths"`
	IncludePaths         []string `json:"include_paths"`
	Library              figure   `json:"library"`
	ExcludedByPathFilter figure   `json:"excluded_by_path_filter"`
	HeldBackByRecord     figure   `json:"held_back_by_record"`
	Covered              figure   `json:"covered"`
	Eligible             figure   `json:"eligible"`
	DirectoriesRead      int64    `json:"directories_read"`
	DirectoriesNotRead   int64    `json:"directories_not_read"`
}

// newPlanRoot starts one root's entry, each figure already carrying the set it covers and
// each pattern list non-nil, because an absent list and an empty one must print the same.
func newPlanRoot(r config.Root) *planRoot {
	under := " under " + r.Clean
	return &planRoot{
		Root:         r.Clean,
		ExcludePaths: append([]string{}, r.Filters.Exclude...),
		IncludePaths: append([]string{}, r.Filters.Include...),
		Library: figure{Set: "every source-named file in a directory the startup walk listed" + under +
			", before path filters, record hold-backs and outright refusals"},
		ExcludedByPathFilter: figure{Set: "the part of the library a path filter in force for this root " +
			"kept out of the pass"},
		HeldBackByRecord: figure{Set: "the part of the library a parked job's record or a recorded " +
			"replacement path held back, among files no path filter excluded"},
		Covered:  figure{Set: "every file a scan would enumerate" + under + ", whatever the guards then concluded"},
		Eligible: figure{Set: "the covered files" + under + " a run would transcode"},
	}
}

// add counts one file into a figure.
func (f *figure) add(bytes int64) {
	f.Files++
	f.Bytes += bytes
}

// planDeclinedPaths is every enumerated path the pipeline refuses OUTRIGHT, with the rule
// that refused each. They are in NO figure of this report, deliberately and for the reason
// that makes the report worth reading: the daemon claims, probes and records nothing about
// such a path, so counting one among the files a run would transcode would promise
// something no run will do. They are published rather than dropped, because a path silently
// missing from a report about a library reads as a path that is not there.
type planDeclinedPaths struct {
	Files int64          `json:"files"`
	Paths []planDeclined `json:"paths,omitempty"`
	Note  string         `json:"note"`
}

// planDeclined is one such path and the rule that refused it.
type planDeclined struct {
	Path   string `json:"path"`
	Rule   string `json:"rule"`
	Detail string `json:"detail"`
}

// planGroup is one resolved encode profile's whole plan: what is eligible under it, what
// its guards held back, what it could not account for, and the projection - or the refusal
// - that belongs to that profile's own history.
type planGroup struct {
	// Profile is the digest of the resolved values that judge these files. It is the same
	// identifier `validate` prints beside each root and a terminal row records.
	Profile      string   `json:"profile"`
	LibraryRoots []string `json:"library_roots,omitempty"`

	// Covered is every file the enumeration covered under this profile, whatever the guards
	// then concluded. It is published beside Eligible because it is the figure that has to
	// EQUAL what a daemon pass covers: the eligible count alone cannot tell a plan that
	// missed half a library from one whose guards held half of it back.
	Covered        figure          `json:"covered"`
	Eligible       figure          `json:"eligible"`
	SkippedByGuard []bucket        `json:"skipped_by_guard"`
	Skipped        int64           `json:"skipped_total"`
	Unaccounted    planUnaccounted `json:"unaccounted"`
	Projection     planProjection  `json:"projection"`

	// guards accumulates while the pass is folded in.
	guards map[string]int64
}

// planUnaccounted is every covered file this plan could not read or probe, with the reason
// for each. A total that hid them would be a confident total about a library this command
// only partly saw, which is the one thing a report that precedes a destructive run must
// never be.
type planUnaccounted struct {
	Files   int64            `json:"files"`
	Reasons []planUnreadable `json:"reasons,omitempty"`
	Note    string           `json:"note"`
}

// planUnreadable is one file the plan could not account for, and why.
type planUnreadable struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// planProjection is the reclaim figure, or the refusal standing in its place.
//
// Made is the field a caller branches on, and it is a field rather than an exit code on
// purpose: the exit status says whether a report was produced, and a refused projection is
// a stated state INSIDE a report that was produced. A non-zero code would make the ordinary
// first run on a fresh install look like a failure to every supervisor that wraps it.
type planProjection struct {
	Made bool `json:"made"`
	// Refused is why there is no projection, and it is empty exactly when there is one.
	Refused string `json:"refused,omitempty"`
	// Reclaim carries the numbers, and names what they are beside them.
	Reclaim *planReclaim `json:"estimated_reclaim,omitempty"`
}

// planReclaim is the projection itself. Estimate is not a label on the object; it is the
// sentence that travels with these three numbers wherever they appear, because a figure
// derived from other files' ratios stops being honest the moment it is read as a
// measurement of these ones.
type planReclaim struct {
	Estimate  string     `json:"estimate"`
	Bytes     int64      `json:"bytes"`
	LowBytes  int64      `json:"low_bytes"`
	HighBytes int64      `json:"high_bytes"`
	Sample    planSample `json:"sample"`
}

// planSample is what the ratio was derived from: how many of this install's own completed
// encodes contributed one, how many done rows could not, and the spread of what they
// recorded. A ratio published without them would be a single unqualified number, which is
// the shape an estimate is most often mistaken for a measurement in.
type planSample struct {
	Set              string  `json:"set"`
	CompletedEncodes int64   `json:"completed_encodes"`
	RowsExcluded     int64   `json:"done_rows_recording_no_size_pair"`
	RatioMin         float64 `json:"size_ratio_min"`
	RatioMean        float64 `json:"size_ratio_mean"`
	RatioMax         float64 `json:"size_ratio_max"`
}

// theEstimateSentence names a projection at every point of appearance. One constant, so the
// report and the document cannot say it differently, and so a criterion about what the
// output SAYS has one string to be graded against.
const theEstimateSentence = "an ESTIMATE, not a measurement: it is this install's own completed " +
	"encodes applied to files it has not encoded, and the actual result depends on the content of " +
	"those files, which nothing here has looked inside"

func newPlanGroup(digest string) *planGroup {
	return &planGroup{
		Profile: digest,
		Covered: figure{Set: "every file a scan under this profile would enumerate, whatever the " +
			"guards then concluded about it"},
		Eligible: figure{Set: "the files a run under this profile would transcode: every covered " +
			"file that passed every skip guard"},
		guards: map[string]int64{},
		Unaccounted: planUnaccounted{Note: "covered files this plan could not read or probe. They are " +
			"in NO figure above: a file whose size or codec could not be established is one this " +
			"report cannot say anything about, and counting it either way would be a guess."},
	}
}

// add folds one covered file into this group's figures.
func (g *planGroup) add(f engine.PlanFile) {
	g.Covered.Files++
	g.Covered.Bytes += f.Bytes
	switch {
	case f.Unreadable:
		g.Unaccounted.Files++
		g.Unaccounted.Reasons = append(g.Unaccounted.Reasons,
			planUnreadable{Path: f.Path, Reason: f.Detail})
	case f.Guard != "":
		g.Skipped++
		g.guards[f.Guard]++
	default:
		g.Eligible.Files++
		g.Eligible.Bytes += f.Bytes
	}
}

// finish publishes this group's guard breakdown and decides its projection.
//
// EVERY guard this build has is published, including the ones that held nothing back. A
// breakdown listing only the guards that fired would leave a reader unable to tell a guard
// that skipped nothing from one this build does not have, and the whole point of naming the
// guard beside the count is that the set of reasons is stated rather than inferred.
func (g *planGroup) finish(sample store.Spread, noHistory string) {
	for _, token := range engine.SkipGuards {
		g.SkippedByGuard = append(g.SkippedByGuard, bucket{Key: token, Count: g.guards[token]})
	}
	// A token outside SkipGuards can still stop a file (the hardlink guard is mutable and is
	// deliberately not a requeue selector), so anything the pass recorded and the list above
	// did not publish is appended rather than dropped.
	var extra []string
	for token := range g.guards {
		if !engine.KnownGuard(token) {
			extra = append(extra, token)
		}
	}
	sort.Strings(extra)
	for _, token := range extra {
		g.SkippedByGuard = append(g.SkippedByGuard, bucket{Key: token, Count: g.guards[token]})
	}
	g.Projection = projectReclaim(g.Eligible.Bytes, sample, noHistory)
}

// projectReclaim derives the reclaim figure from a sample of this install's own completed
// encodes, or refuses with the reason there is none to derive it from.
//
// A ratio of 0.35 means "the replacement was 35% of the original", so the reclaim is the
// eligible bytes times one minus that ratio. The three figures come from the sample's mean,
// its highest ratio (the least reclaim it has ever managed) and its lowest (the most), which
// is the honest way to publish a spread rather than a point.
func projectReclaim(eligibleBytes int64, sample store.Spread, noHistory string) planProjection {
	switch {
	case eligibleBytes == 0:
		// Nothing is eligible, so there is nothing to project a reclaim OVER. A zero here
		// would be a number about a population that is empty rather than a population that
		// was measured, and the two must not print the same.
		return planProjection{Refused: "no covered file is eligible, so there is nothing to project a " +
			"reclaim over"}
	case sample.Err != nil:
		return planProjection{Refused: fmt.Sprintf("the size ratios could not be read (%v), so no "+
			"projection was made from them", sample.Err)}
	case sample.Counted == 0 || sample.Mean == nil || sample.Min == nil || sample.Max == nil:
		return planProjection{Refused: noHistory}
	}

	reclaim := func(ratio float64) int64 {
		saved := float64(eligibleBytes) * (1 - ratio)
		if saved < 0 {
			// A ratio above 1 is an encode that grew. It is real evidence and stays in the
			// sample, but a NEGATIVE reclaim is not a figure an operator can act on, so the
			// bound is stated as none rather than as a number below zero.
			return 0
		}
		return int64(saved)
	}
	return planProjection{
		Made: true,
		Reclaim: &planReclaim{
			Estimate: theEstimateSentence,
			Bytes:    reclaim(*sample.Mean),
			// The LEAST reclaim this sample has ever produced comes from its LARGEST ratio.
			LowBytes:  reclaim(*sample.Max),
			HighBytes: reclaim(*sample.Min),
			Sample: planSample{
				Set:              "this install's own completed encodes: " + sample.Coverage.Set,
				CompletedEncodes: sample.Counted,
				RowsExcluded:     sample.Excluded,
				RatioMin:         *sample.Min,
				RatioMean:        *sample.Mean,
				RatioMax:         *sample.Max,
			},
		},
	}
}

// writeJSON writes the plan as one JSON document. One document, one value: a reader may
// json.Unmarshal the whole of stdout.
func (p *plan) writeJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(p)
}

func (p *plan) writeReport(w io.Writer) {
	fmt.Fprintf(w, "holdfast plan - what this configuration would do to this library, and what it would save.\n")
	fmt.Fprintf(w, "%s\n", wrapAt(p.Note, 92, "  "))

	fmt.Fprintf(w, "\nstorage: %s\n", wrapAt(p.Storage.Verdict, 92, "  "))
	for _, cause := range p.Storage.Causes {
		fmt.Fprintf(w, "  %s\n      %s: %s\n", cause.Path, cause.Kind, cause.Detail)
	}
	for _, r := range p.Storage.Reduced {
		fmt.Fprintf(w, "  reduced no-loss guarantee: %s\n", r)
	}

	fmt.Fprintf(w, "\nledger: %s\n", wrapAt(p.Ledger, 92, "  "))

	fmt.Fprintf(w, "\ncoverage  %8d directory(ies) read, %d not read\n",
		p.Coverage.DirectoriesRead, p.Coverage.DirectoriesNotRead)
	for _, b := range p.Coverage.NotReadWhy {
		fmt.Fprintf(w, "    %-44s %6d\n", b.Key, b.Count)
	}
	fmt.Fprintf(w, "          %s\n", wrapAt(p.Coverage.Boundary, 88, "          "))

	// The scope of each root, before the profiles: what the library under it is, and what of
	// it the filters and the record hold-backs kept out, so the part of a library this tool
	// does not consider is a named figure rather than a gap between two reports.
	fmt.Fprintf(w, "\nlibrary roots - what each holds before the path filters, and what of it is in scope\n")
	for _, r := range p.Roots {
		r.writeReport(w)
	}

	// Per profile FIRST and the total last, because the per-profile figures are the ones an
	// operator acts on: a total across roots that would be encoded differently is a summary,
	// never the number to plan by.
	for _, g := range p.Profiles {
		g.writeReport(w, "encode profile "+g.Profile)
	}
	if p.Total != nil {
		p.Total.writeReport(w, "every resolved profile, combined")
	}

	fmt.Fprintf(w, "\ndeclined  %8d path(s) this pipeline refuses outright\n", p.Declined.Files)
	fmt.Fprintf(w, "          %s\n", wrapAt(p.Declined.Note, 88, "          "))
	for _, d := range p.Declined.Paths {
		fmt.Fprintf(w, "    %s\n      %s: %s\n", d.Path, d.Rule, d.Detail)
	}

	fmt.Fprintf(w, "\nprobe snapshots taken this invocation: %d. One invocation is one pass over the library.\n",
		p.Probes)
}

// writeReport writes one root's scope. The patterns are printed as the JSON document spells
// them, and every figure carries the label and the numbers the document carries, so the two
// forms cannot say different things about the same root.
func (r *planRoot) writeReport(w io.Writer) {
	fmt.Fprintf(w, "  library root %s\n", r.Root)
	fmt.Fprintf(w, "    exclude_paths %s\n", patternsText(r.ExcludePaths))
	fmt.Fprintf(w, "    include_paths %s\n", patternsText(r.IncludePaths))
	for _, f := range []struct {
		label string
		fig   figure
	}{
		{"library", r.Library},
		{"excluded_by_path_filter", r.ExcludedByPathFilter},
		{"held_back_by_record", r.HeldBackByRecord},
		{"covered", r.Covered},
		{"eligible", r.Eligible},
	} {
		fmt.Fprintf(w, "    %-24s %8d file(s)  %16d byte(s)\n", f.label, f.fig.Files, f.fig.Bytes)
	}
	fmt.Fprintf(w, "    directories  %d read, %d not read\n", r.DirectoriesRead, r.DirectoriesNotRead)
}

// patternsText renders a pattern list exactly as the JSON document does.
func patternsText(patterns []string) string {
	b, err := json.Marshal(patterns)
	if err != nil {
		return fmt.Sprint(patterns)
	}
	return string(b)
}

func (g *planGroup) writeReport(w io.Writer, heading string) {
	fmt.Fprintf(w, "\n%s\n", heading)
	for _, r := range g.LibraryRoots {
		fmt.Fprintf(w, "  library root %s\n", r)
	}
	fmt.Fprintf(w, "  covered   %8d file(s)  %16d byte(s)\n", g.Covered.Files, g.Covered.Bytes)
	fmt.Fprintf(w, "            set: %s\n", g.Covered.Set)
	fmt.Fprintf(w, "  eligible  %8d file(s)  %16d byte(s)\n", g.Eligible.Files, g.Eligible.Bytes)
	fmt.Fprintf(w, "            set: %s\n", g.Eligible.Set)
	fmt.Fprintf(w, "  skipped   %8d file(s), by these guards and no others:\n", g.Skipped)
	for _, b := range g.SkippedByGuard {
		fmt.Fprintf(w, "    %-26s %6d file(s)\n", b.Key, b.Count)
	}
	fmt.Fprintf(w, "  unaccounted for: %d file(s)\n", g.Unaccounted.Files)
	fmt.Fprintf(w, "            %s\n", wrapAt(g.Unaccounted.Note, 88, "            "))
	for _, u := range g.Unaccounted.Reasons {
		fmt.Fprintf(w, "    %s: %s\n", u.Path, u.Reason)
	}

	pr := g.Projection
	if !pr.Made {
		fmt.Fprintf(w, "  reclaim   NO PROJECTION: %s\n", wrapAt(pr.Refused, 88, "            "))
		return
	}
	r := pr.Reclaim
	fmt.Fprintf(w, "  reclaim   estimated %d byte(s), between %d and %d\n", r.Bytes, r.LowBytes, r.HighBytes)
	fmt.Fprintf(w, "            %s\n", wrapAt(r.Estimate, 88, "            "))
	fmt.Fprintf(w, "            sample: %d completed encode(s), size ratio %.4f low / %.4f mean / %.4f high\n",
		r.Sample.CompletedEncodes, r.Sample.RatioMin, r.Sample.RatioMean, r.Sample.RatioMax)
	fmt.Fprintf(w, "            %s\n", wrapAt(r.Sample.Set, 88, "            "))
	if r.Sample.RowsExcluded > 0 {
		fmt.Fprintf(w, "            %d done row(s) recorded no size pair and contributed nothing\n",
			r.Sample.RowsExcluded)
	}
}
