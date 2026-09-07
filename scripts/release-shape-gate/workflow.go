package main

// The release definition, read STRUCTURALLY - and only structurally.
//
// This file used to carry a second thing: a catalogue that decided, from a step's text,
// whether that step performed a published act. It is gone, and the reason it is gone is the
// whole design. Six ordinals of adversarial review found six fail-opens in one direction
// (S0046 F1, F5, F7, F9, F11, F12), each one ordinary shell rather than obfuscation, and
// each fix bought exactly the spelling it named. Replacing the reader with an OBSERVER that
// ran each step in a stubbed environment moved the hole rather than closing it (F14, F15):
// every control such an environment has - the recorder, the guard, the emptied PATH - lives
// inside the shell it is trying to observe, so `export PATH=/usr/bin:/bin` on one line, or
// `exec docker push`, puts the rest of the step outside it.
//
// The question "does this text publish?" is undecidable over arbitrary shell, and it has
// been replaced by one that is decidable from structured YAML: CAN it publish? A step
// publishes nothing it holds no credential for. So the release definition is constrained -
// every irreversible act lives in a job that runs only on a tag push, and that job is the
// only one granted a write permission - and the gate decides that constraint from
// `permissions:`, `secrets:`, `needs:` and `if:`. See capability.go.
//
// What survives here, unchanged and sound in every review: the workflow is PARSED rather
// than matched, and which steps run is decided by RUNNING the workflow's own planning
// shell (shell.go) and evaluating each guard against the values it produced (expr.go).

import (
	"fmt"
	"os"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// Workflow / Job / Step model the keys this gate reasons about. Unknown keys are NOT
// ignored: capability.go walks the raw node of every job that runs on a dispatch and reds
// on a key it has not classified, because the failure mode this whole gate exists to
// delete is a key nobody looked at contributing silence.
type Workflow struct {
	Name        string         `yaml:"name"`
	Env         map[string]any `yaml:"env"`
	Jobs        map[string]Job `yaml:"jobs"`
	Permissions any            `yaml:"permissions"`

	HasPermissions bool                  // whether the key is present at all; absence is not "none"
	JobNodes       map[string]*yaml.Node // the raw mapping node of each job
}

type Job struct {
	ID              string
	Name            string            `yaml:"name"`
	If              string            `yaml:"if"`
	Needs           any               `yaml:"needs"`
	Env             map[string]any    `yaml:"env"`
	Outputs         map[string]string `yaml:"outputs"`
	ContinueOnError any               `yaml:"continue-on-error"`
	Permissions     any               `yaml:"permissions"`
	Steps           []Step            `yaml:"steps"`

	HasPermissions bool
	Node           *yaml.Node
}

type Step struct {
	ID              string         `yaml:"id"`
	Name            string         `yaml:"name"`
	Uses            string         `yaml:"uses"`
	Run             string         `yaml:"run"`
	If              string         `yaml:"if"`
	Env             map[string]any `yaml:"env"`
	With            map[string]any `yaml:"with"`
	ContinueOnError any            `yaml:"continue-on-error"`

	Index int    // declaration order within the job; the ordering property is about this
	JobID string // which job it came from
	Node  *yaml.Node
}

// Label is how a step is named in every message this gate prints.
func (s Step) Label() string {
	switch {
	case s.ID != "" && s.Name != "":
		return fmt.Sprintf("job %q step %d `%s` (%q)", s.JobID, s.Index+1, s.ID, s.Name)
	case s.ID != "":
		return fmt.Sprintf("job %q step %d `%s`", s.JobID, s.Index+1, s.ID)
	case s.Name != "":
		return fmt.Sprintf("job %q step %d %q", s.JobID, s.Index+1, s.Name)
	case s.Uses != "":
		return fmt.Sprintf("job %q step %d (uses %s)", s.JobID, s.Index+1, s.Uses)
	default:
		return fmt.Sprintf("job %q step %d (unnamed)", s.JobID, s.Index+1)
	}
}

// ActID is what the operator runbook must carry, verbatim (A8). It is the step's own
// declared identity - `<job>/<step id>` - and NOT anything derived from what the step's
// text appears to do, because that derivation is the thing this pass deleted. Every step in
// a job that holds a publishing grant gets one, so a new step in that job cannot be added
// without the runbook gaining it, whatever the step says it does.
func (s Step) ActID() string { return s.JobID + "/" + s.ID }

// LoadWorkflow reads and parses a workflow definition. Its failure modes are distinct and
// named, because "the gate passed" over a file it could not read is the vacuous pass this
// whole gate exists to refuse (A15).
func LoadWorkflow(path string) (*Workflow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("the release definition CANNOT BE READ (%s): %v", path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, fmt.Errorf("the release definition IS EMPTY (%s): there is nothing to gate, and an empty file must not pass", path)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("the release definition CANNOT BE PARSED (%s): %v", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("the release definition is not a YAML mapping (%s): a workflow is a mapping of top-level keys", path)
	}
	root := doc.Content[0]

	var wf Workflow
	if err := root.Decode(&wf); err != nil {
		return nil, fmt.Errorf("the release definition CANNOT BE PARSED (%s): %v", path, err)
	}
	wf.HasPermissions = mappingValue(root, "permissions") != nil
	if len(wf.Jobs) == 0 {
		return nil, fmt.Errorf("the release definition NAMES NO JOB (%s)", path)
	}

	jobsNode := mappingValue(root, "jobs")
	wf.JobNodes = map[string]*yaml.Node{}
	if jobsNode != nil && jobsNode.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(jobsNode.Content); i += 2 {
			wf.JobNodes[jobsNode.Content[i].Value] = jobsNode.Content[i+1]
		}
	}

	total := 0
	for id, job := range wf.Jobs {
		job.ID = id
		job.Node = wf.JobNodes[id]
		job.HasPermissions = job.Node != nil && mappingValue(job.Node, "permissions") != nil
		stepsNode := (*yaml.Node)(nil)
		if job.Node != nil {
			stepsNode = mappingValue(job.Node, "steps")
		}
		for i := range job.Steps {
			job.Steps[i].Index = i
			job.Steps[i].JobID = id
			if stepsNode != nil && stepsNode.Kind == yaml.SequenceNode && i < len(stepsNode.Content) {
				job.Steps[i].Node = stepsNode.Content[i]
			}
		}
		wf.Jobs[id] = job
		total += len(job.Steps)
	}
	if total == 0 {
		return nil, fmt.Errorf("the release definition NAMES NO STEP AT ALL (%s): a pass over an empty step list measures nothing", path)
	}
	return &wf, nil
}

func (w *Workflow) JobIDs() []string {
	ids := make([]string, 0, len(w.Jobs))
	for id := range w.Jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// NeedsOf reads a job's `needs:`, which YAML lets be a scalar or a sequence.
func (j Job) NeedsOf() ([]string, error) {
	switch v := j.Needs.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{v}, nil
	case []any:
		var out []string
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("job %q has a `needs:` entry that is not a job name (%v)", j.ID, e)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("job %q has a `needs:` this gate cannot read (%T). The order of the release is decided from that graph, so an unreadable one is not a harmless one", j.ID, j.Needs)
	}
}

// reaches reports whether `from` transitively needs `to`, i.e. whether `to` is guaranteed to
// have finished before `from` starts. Two jobs with no path between them are CONCURRENT and
// this gate refuses to order them (see Ordering in main.go) rather than reporting an order
// it did not check.
func (w *Workflow) reaches(from, to string) (bool, error) {
	seen := map[string]bool{}
	var walk func(string) (bool, error)
	walk = func(id string) (bool, error) {
		if seen[id] {
			return false, nil
		}
		seen[id] = true
		job, ok := w.Jobs[id]
		if !ok {
			return false, fmt.Errorf("job %q needs %q, which this workflow does not define", from, id)
		}
		needs, err := job.NeedsOf()
		if err != nil {
			return false, err
		}
		for _, n := range needs {
			if n == to {
				return true, nil
			}
			if ok, err := walk(n); err != nil || ok {
				return ok, err
			}
		}
		return false, nil
	}
	return walk(from)
}

// TolerateFailure reports whether a step's failure would be swallowed.
func (s Step) TolerateFailure() (bool, string) { return tolerates(s.ContinueOnError) }

func tolerates(v any) (bool, string) {
	switch t := v.(type) {
	case nil:
		return false, ""
	case bool:
		if t {
			return true, "continue-on-error: true"
		}
		return false, ""
	case string:
		s := strings.TrimSpace(t)
		if s == "" || strings.EqualFold(s, "false") {
			return false, ""
		}
		// An expression here decides at run time whether a failure counts. This gate
		// refuses it rather than guessing which way it lands: `continue-on-error:
		// ${{ … }}` before the promotion is a failure that may or may not stop the run,
		// and "may not" is the whole hazard.
		return true, "continue-on-error: " + s
	default:
		return true, fmt.Sprintf("continue-on-error: %v", t)
	}
}

// --- yaml.Node helpers -----------------------------------------------------------------

func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func mappingKeys(n *yaml.Node) []string {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	var out []string
	for i := 0; i+1 < len(n.Content); i += 2 {
		out = append(out, n.Content[i].Value)
	}
	return out
}

// scalars yields every scalar VALUE under a node, with a path describing where it came
// from. It is how the secret scan sees keys this gate does not model: a credential in a key
// nobody thought of is exactly the shape that keeps winning.
func scalars(n *yaml.Node, path string, fn func(where, value string)) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.ScalarNode:
		fn(path, n.Value)
	case yaml.SequenceNode:
		for i, c := range n.Content {
			scalars(c, fmt.Sprintf("%s[%d]", path, i), fn)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i].Value
			p := k
			if path != "" {
				p = path + "." + k
			}
			// The key itself can carry the reference too (rare, but `${{ … }}` is legal
			// in a key), so it is scanned as well.
			fn(p+" (key)", k)
			scalars(n.Content[i+1], p, fn)
		}
	}
}
