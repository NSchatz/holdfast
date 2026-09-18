// Package mutation holds what every part of the mutation gate has to agree about: where
// the floor and the mutation domain are committed, how they are read, and the shape of
// the machine-readable report a run publishes.
//
// It exists so that those things are DEFINED ONCE. The gate, the shape check and the
// notification all need the floor; a second reader with its own spelling of the key is
// the failure this repository keeps meeting - a value restated in several files and kept
// in step by a comment.
package mutation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	yaml "go.yaml.in/yaml/v3"
)

// ConfigName is the committed runner configuration: the ONE home of the floor and of the
// mutation domain. docs/mutation-testing.md restates both for a reader, and
// scripts/mutation-shape refuses a disagreement between the two.
const ConfigName = ".gremlins.yaml"

// RunnerModule is the pinned mutation runner. Its VERSION is not here: the Makefile owns
// every tool pin in this repository, and a pin restated elsewhere is a pin that drifts.
const RunnerModule = "github.com/go-gremlins/gremlins/cmd/gremlins"

// DocName is the committed document a reader is sent to for the floor, the domain and the
// reason behind each.
const DocName = "docs/mutation-testing.md"

// Config is the part of the runner's configuration this gate decides things from.
type Config struct {
	Unleash struct {
		Threshold struct {
			Efficacy       float64 `yaml:"efficacy"`
			MutantCoverage float64 `yaml:"mutant-coverage"`
		} `yaml:"threshold"`
		ExcludeFiles []string `yaml:"exclude-files"`
	} `yaml:"unleash"`
}

// Floor is the mutation score a run must reach, as a percentage.
func (c Config) Floor() float64 { return c.Unleash.Threshold.Efficacy }

// ExcludeFiles is the committed exclusion list: the paths the runner may not mutate, which
// is what turns "the module" into "the mutation domain".
func (c Config) ExcludeFiles() []string { return c.Unleash.ExcludeFiles }

// ReadConfig reads the floor and the domain out of the committed configuration at root.
//
// An absent, unreadable or floorless configuration is an ERROR and never a default. A
// gate that fell back to a floor of zero when it could not read its own configuration
// would pass every tree, which is the one answer it must never give by accident.
func ReadConfig(root string) (Config, error) {
	var c Config
	path := filepath.Join(root, ConfigName)
	b, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Floor() <= 0 {
		return c, fmt.Errorf("%s declares no floor (unleash.threshold.efficacy is %v) - a gate with no floor passes everything", path, c.Floor())
	}
	if len(c.ExcludeFiles()) == 0 {
		return c, fmt.Errorf("%s declares no mutation domain (unleash.exclude-files is empty) - the domain is the module minus that list, and an empty list means the run includes the suites it cannot finish", path)
	}
	return c, nil
}

// The modes a run can be in. A diff-scoped run mutates only what a change touched; an
// unscoped run mutates the whole domain.
const (
	ModeDiff = "diff"
	ModeFull = "full"
)

// The verdicts a report can carry. A score is a number or it is ABSENT; it is never a
// zero standing in for "nothing ran", which is the rule the ledger totals already follow.
const (
	VerdictPass       = "pass"
	VerdictBelowFloor = "below-floor"
	VerdictNoMutants  = "no-mutants-in-scope"
)

// MutantCounts is how the run's mutants ended up. TimedOut is counted off the runner's
// own output rather than read from its report, which does not carry the figure: the
// runner treats a timeout as caught, so a run measured with timeouts in it is partly a
// measurement of the machine and a reader is owed the number.
type MutantCounts struct {
	Total      int `json:"total"`
	Killed     int `json:"killed"`
	Lived      int `json:"lived"`
	NotCovered int `json:"not_covered"`
	NotViable  int `json:"not_viable"`
	TimedOut   int `json:"timed_out"`
}

// FileReport is one mutated file and what became of its mutants.
type FileReport struct {
	FileName   string `json:"file_name"`
	Killed     int    `json:"killed"`
	Lived      int    `json:"lived"`
	NotCovered int    `json:"not_covered"`
	NotViable  int    `json:"not_viable"`
	TimedOut   int    `json:"timed_out"`
}

// RunnerInfo records which runner produced the measurement, and what it said the figures
// were in its own words.
type RunnerInfo struct {
	Module                    string   `json:"module"`
	Version                   string   `json:"version"`
	ReportedEfficacy          *float64 `json:"reported_efficacy"`
	ReportedMutationsCoverage *float64 `json:"reported_mutations_coverage"`
}

// Report is the machine-readable result a run publishes: what was mutated, what the score
// was, what the floor was and which way it went.
type Report struct {
	Mode        string       `json:"mode"`
	DiffRef     string       `json:"diff_ref"`
	InScope     bool         `json:"in_scope"`
	ScopedFiles []string     `json:"scoped_files"`
	Floor       float64      `json:"floor"`
	Score       *float64     `json:"score"`
	Verdict     string       `json:"verdict"`
	Mutants     MutantCounts `json:"mutants"`
	Files       []FileReport `json:"files"`
	Runner      RunnerInfo   `json:"runner"`
	Domain      Domain       `json:"domain"`
}

// Domain records where the run took its exclusion list from, and what was in it.
type Domain struct {
	Config       string   `json:"config"`
	ExcludeFiles []string `json:"exclude_files"`
}

// WriteReport writes the report atomically: through a temporary beside the destination
// and a rename, so a reader never sees half a report and a failed run leaves no partial
// artifact behind (pinning P7).
func WriteReport(path string, r Report) error {
	if r.ScopedFiles == nil {
		r.ScopedFiles = []string{}
	}
	if r.Files == nil {
		r.Files = []FileReport{}
	}
	if r.Domain.ExcludeFiles == nil {
		r.Domain.ExcludeFiles = []string{}
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".partial"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ReadReport reads a published report back.
func ReadReport(path string) (Report, error) {
	var r Report
	b, err := os.ReadFile(path)
	if err != nil {
		return r, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("parse %s: %w", path, err)
	}
	return r, nil
}
