package main

// The release definition, read structurally, and what each of its steps DOES.
//
// Two different questions live here and they are answered in two different ways, on
// purpose:
//
//   - "would this step run?" is decided by RUNNING the workflow's own planning shell and
//     evaluating each guard against the values it produced (shell.go, expr.go). Nothing
//     in this file guesses at it.
//   - "what act does this step perform?" has to be read off the step's definition, because
//     that is where the act is written down. The catalogue below is therefore deliberately
//     broad and errs towards calling something a published act: a step wrongly classified
//     as publishing reds the gate and gets an entry in the runbook, while one wrongly
//     classified as harmless is how an unreviewed publish ships.
//
// The second question has a sharp edge, and destinationInput is where it is handled: an
// action input can decide the act by itself (`docker/build-push-action` publishes when
// `push:` is true), and GitHub lets that input be an EXPRESSION - `push: ${{
// github.event_name != 'pull_request' }}` is the action's own documented idiom. Asking
// whether the literal text says "true" answers the wrong question, because an expression is
// never the string "true", so a step that publishes on the event under test would read as
// harmless. Such an input is therefore DECIDED, through the same evaluator that decides
// `if:` guards, against the same event shape - and everything undecidable is an error, not
// a false.
//
// The same edge has a SECOND face, and it is the reason an input is modelled as a SET
// rather than as one key. `docker/build-push-action` has TWO inputs that set where its
// build goes, and its own input table says they are one act spelled two ways: `push` is
// "shorthand for `--output=type=registry`", and `outputs` is the longhand list of output
// destinations. So `outputs: type=registry`, and `outputs: type=image,name=…,push=true`
// (the spelling in the action's own multi-platform example), publish exactly as hard as
// `push: true` does. Modelling only `push:` turned the ABSENCE of that one key into an
// inferred "this is a local build", which is false whenever the other key carries the act -
// and false in the direction that ships an unreviewed publish. Both are decided here, the
// step publishes if EITHER says so, and an `outputs:` value this gate cannot decide is
// never a quiet no.

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// Workflow / Job / Step model only the keys this gate reasons about. Unknown keys are
// ignored by the decoder; that is safe here because every decision below is made from a
// key this model carries, and a key it does not carry cannot make a step publish.
type Workflow struct {
	Name string         `yaml:"name"`
	Env  map[string]any `yaml:"env"`
	Jobs map[string]Job `yaml:"jobs"`
}

type Job struct {
	Name            string         `yaml:"name"`
	If              string         `yaml:"if"`
	Env             map[string]any `yaml:"env"`
	ContinueOnError any            `yaml:"continue-on-error"`
	Steps           []Step         `yaml:"steps"`
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
}

// Label is how a step is named in every message this gate prints.
func (s Step) Label() string {
	switch {
	case s.Name != "":
		return fmt.Sprintf("step %d %q", s.Index+1, s.Name)
	case s.Uses != "":
		return fmt.Sprintf("step %d (uses %s)", s.Index+1, s.Uses)
	default:
		return fmt.Sprintf("step %d (unnamed)", s.Index+1)
	}
}

// Slug is the stable half of an act id: a step's name reduced to something a runbook can
// carry verbatim. It is derived from the step NAME, so adding a publishing step - or
// renaming one - forces the runbook to gain the new id (A8).
func (s Step) Slug() string {
	base := s.Name
	if base == "" {
		base = s.Uses
	}
	if base == "" {
		base = fmt.Sprintf("step-%d", s.Index+1)
	}
	var sb strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(base) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			sb.WriteByte('-')
			prevDash = true
		}
	}
	return strings.Trim(sb.String(), "-")
}

// LoadWorkflow reads and parses a workflow definition. Its three failure modes are
// distinct and named, because "the gate passed" over a file it could not read is the
// vacuous pass this whole gate exists to refuse (A15).
func LoadWorkflow(path string) (*Workflow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("the release definition CANNOT BE READ (%s): %v", path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, fmt.Errorf("the release definition IS EMPTY (%s): there is nothing to gate, and an empty file must not pass", path)
	}
	var wf Workflow
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(&wf); err != nil {
		return nil, fmt.Errorf("the release definition CANNOT BE PARSED (%s): %v", path, err)
	}
	if len(wf.Jobs) == 0 {
		return nil, fmt.Errorf("the release definition NAMES NO JOB (%s)", path)
	}
	total := 0
	for id, job := range wf.Jobs {
		for i := range job.Steps {
			job.Steps[i].Index = i
			job.Steps[i].JobID = id
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

// --- what a step does ----------------------------------------------------------------

// ActKind names a class of published act - something that, once done, is out in the world
// and no re-run of the workflow takes it back.
type ActKind string

const (
	ActImagePush ActKind = "image-push"       // an image reaches a registry
	ActTagMove   ActKind = "tag-move"         // a floating reference is pointed somewhere new
	ActRelease   ActKind = "github-release"   // a published release object is created
	ActRefPush   ActKind = "git-ref-push"     // a ref (tag/branch) reaches the remote
	ActArtefact  ActKind = "artefact-publish" // a package/chart/module is published
)

// Act is one published act performed by one step.
type Act struct {
	Kind ActKind
	Step Step
	// Why names the thing in the step that made this call, so the message says what it
	// saw rather than merely that it objected.
	Why string
}

// ID is what the runbook must carry, verbatim (A8). It is per-STEP, not per-kind, so a
// SECOND publishing step of an already-documented kind still forces a runbook entry.
func (a Act) ID() string { return string(a.Kind) + "@" + a.Step.Slug() }

// destinationInput is one `with:` input through which an action decides where its build
// goes. An action's inputs are modelled as a SET because a destination can be spelled more
// than one way on the same action, and a set of one is how the "only `push:` exists"
// reading shipped a hole: absence of the modelled key was read as absence of the act.
type destinationInput struct {
	key    string
	decide func(key string, raw any, ctx *evalCtx) (publishes bool, detail string, err error)
}

// usesDetectors are matched against a step's `uses:` and its `with:` inputs.
//
// `inputs` are ALTERNATIVE spellings of the same destination, not a conjunction: the action
// publishes if ANY of them says so, because buildx unions its output destinations (`push:
// true` appends `--output=type=registry` to whatever `outputs:` already asked for).
var usesDetectors = []struct {
	kind   ActKind
	action *regexp.Regexp
	inputs []destinationInput // nil = the action always publishes
	what   string
}{
	{ActImagePush, regexp.MustCompile(`^docker/build-push-action`), []destinationInput{
		{"push", decideRequiredInput},
		{"outputs", decideOutputsInput},
	}, "docker/build-push-action"},
	{ActRelease, regexp.MustCompile(`^softprops/action-gh-release`), nil, "softprops/action-gh-release"},
	{ActRelease, regexp.MustCompile(`^ncipollo/release-action`), nil, "ncipollo/release-action"},
	{ActRefPush, regexp.MustCompile(`^ad-m/github-push-action`), nil, "ad-m/github-push-action"},
}

// classifiedLocalActions is the OTHER half of the catalogue, and its whole job is to give
// the catalogue an EDGE the gate can state.
//
// usesDetectors alone answered "does this action publish?" with either yes or silence, and
// silence read as no. An action in neither list - `docker/bake-action` with `push: true`, a
// local composite action, a reusable workflow, a container action - then contributed no act
// and no message, so "NONE of them publishes anything" was printed over a definition
// carrying a step the gate had never asked about. That is the same sentence and the same
// fail-open direction as reading an absent input, or an unjoined continuation, as harmless.
//
// So the rule is: every `uses:` must be in ONE of the two lists. This one is short on
// purpose and grows one line at a time, each entry naming a reason a human checked, because
// a list that grows by guessing is the unbounded catalogue this gate refuses to become. An
// action in neither list is UNDECIDED and reds by name; classifying it costs one line and
// forces the question to be asked once, out loud, in a file under review.
var classifiedLocalActions = []struct {
	action *regexp.Regexp
	why    string
}{
	{regexp.MustCompile(`^actions/checkout(@|$)`), "checks a ref out into the workspace; it writes nothing outward"},
	{regexp.MustCompile(`^actions/setup-go(@|$)`), "installs a toolchain onto the runner"},
	{regexp.MustCompile(`^actions/upload-artifact(@|$)`), "uploads a RUN-SCOPED artifact, which expires with the run and is not a published artefact"},
	{regexp.MustCompile(`^docker/setup-qemu-action(@|$)`), "registers binfmt emulators on the runner"},
	{regexp.MustCompile(`^docker/setup-buildx-action(@|$)`), "creates a local builder instance"},
	{regexp.MustCompile(`^docker/login-action(@|$)`), "authenticates to a registry; a credential is not a publish, and every push it enables is decided on its own step"},
}

// Acts reports every published act this step performs, for the event shape ctx describes.
//
// ctx is nil for the STATIC question - "what published acts does this definition name at
// all?", which the runbook cross-check (A8) asks and which no single event answers. With no
// event to decide against, an input written as an expression counts as publishing: a step
// that might publish is a step the runbook has to name.
//
// An input this gate cannot decide is an error, never an omission. Returning no act for it
// would print "NONE of them publishes anything" over a dry run that pushes.
func (s Step) Acts(ctx *evalCtx) ([]Act, error) {
	var out []Act
	seen := map[string]bool{}
	for _, c := range s.commands() {
		kind, why, err := c.Act()
		if err != nil {
			return nil, fmt.Errorf("%s %w.\nThis gate will not read a command it cannot decide as harmless: a command wrongly called publishing reds this gate, one wrongly called harmless is how an unreviewed publish ships", s.Label(), err)
		}
		if kind == "" || seen[string(kind)+"\x00"+why] {
			continue
		}
		seen[string(kind)+"\x00"+why] = true
		out = append(out, Act{Kind: kind, Step: s, Why: why})
	}
	matched := false
	for _, d := range usesDetectors {
		if !d.action.MatchString(s.Uses) {
			continue
		}
		matched = true
		why := d.what
		if len(d.inputs) > 0 {
			var reasons []string
			for _, in := range d.inputs {
				publishes, detail, err := in.decide(in.key, s.With[in.key], ctx)
				if err != nil {
					return nil, fmt.Errorf("%s uses %s, and %w.\nThis gate will not read an input it cannot decide as harmless: a step wrongly called publishing reds this gate, one wrongly called harmless is how an unreviewed publish ships", s.Label(), s.Uses, err)
				}
				if publishes {
					reasons = append(reasons, detail)
				}
			}
			if len(reasons) == 0 {
				continue
			}
			why = d.what + " (" + strings.Join(reasons, "; ") + ")"
		}
		out = append(out, Act{Kind: d.kind, Step: s, Why: why})
	}
	if u := strings.TrimSpace(s.Uses); u != "" && !matched && !isClassifiedLocal(u) {
		return nil, fmt.Errorf("%s uses `%s`, and this gate does not classify that action.\nEvery `uses:` has to sit in ONE of the two halves of the act catalogue in scripts/release-shape-gate/workflow.go: usesDetectors, for an action that can publish and whose destination inputs are then decided, or classifiedLocalActions, for one a human has checked and found performs no published act. An action in NEITHER is undecided, which is not the same as harmless - `docker/bake-action` with `push: true`, a local composite action and a reusable workflow all publish, and answering silence with \"no\" is how an unreviewed publish ships. Add it to whichever list is right, with the reason", s.Label(), u)
	}
	return out, nil
}

func isClassifiedLocal(uses string) bool {
	for _, c := range classifiedLocalActions {
		if c.action.MatchString(uses) {
			return true
		}
	}
	return false
}

// decideRequiredInput decides a BOOLEAN `with:` input that by itself makes an action
// publish - `docker/build-push-action`'s `push:`.
//
// Fail-closed at every branch, because the wrong way to be wrong here is a silent false:
//
//   - absent: this key does not set a destination. It is NOT on its own a verdict that the
//     step is a local build - the same action's `outputs:` can carry the very same act, and
//     that is why a detector decides every one of an action's destination inputs and
//     publishes if any of them says so. Reading absence here as "harmless" while the other
//     spelling went unmodelled is precisely how a dry run that pushes read as green.
//   - a YAML boolean, or a string that is exactly true/false once every ${{ … }} span in it
//     has been EVALUATED against the event under test: that value.
//   - anything else - an expression this evaluator cannot decide, a context the planning run
//     did not produce, a string that is not a boolean (`push: yes` is a string in YAML 1.2,
//     and GitHub's own boolean-input parser rejects it), a type that is not one either - is
//     an ERROR that reds the gate by name.
func decideRequiredInput(key string, raw any, ctx *evalCtx) (publishes bool, detail string, err error) {
	switch t := raw.(type) {
	case nil:
		return false, "", nil
	case bool:
		return t, fmt.Sprintf("%s: %v", key, t), nil
	case string:
		text := strings.TrimSpace(t)
		if text == "" {
			return false, "", fmt.Errorf("`%s:` is empty. An input that decides whether this step publishes must say which", key)
		}
		if strings.Contains(text, "${{") {
			if ctx == nil {
				return true, fmt.Sprintf("%s: %s - an expression, counted as publishing because no single event decides it", key, text), nil
			}
			v, ierr := Interpolate(text, *ctx)
			if ierr != nil {
				return false, "", fmt.Errorf("`%s: %s` cannot be decided for this event: %w", key, text, ierr)
			}
			decided := strings.TrimSpace(v)
			switch strings.ToLower(decided) {
			case "true":
				return true, fmt.Sprintf("%s: %s, which is TRUE here", key, text), nil
			case "false":
				return false, "", nil
			}
			return false, "", fmt.Errorf("`%s: %s` evaluates to %q, which is not a boolean, so whether this step publishes is unknown", key, text, decided)
		}
		switch strings.ToLower(text) {
		case "true":
			return true, fmt.Sprintf("%s: %s", key, text), nil
		case "false":
			return false, "", nil
		}
		return false, "", fmt.Errorf("`%s: %s` is not a boolean. GitHub's own boolean-input parser accepts only true/false, so this step's publish decision is unknown", key, text)
	default:
		return false, "", fmt.Errorf("`%s:` is a %T (%v), not a boolean and not an expression, so this step's publish decision is unknown", key, raw, raw)
	}
}

// decideOutputsInput decides `outputs:` - buildx's LONGHAND for the destination `push:`
// sets in shorthand. The action's own input table calls `push` "shorthand for
// `--output=type=registry`", so these two keys are one act with two spellings and both have
// to be decided or the absence of one is read as the absence of the act.
//
// The value is a LIST, one buildx output specification per LINE (the action parses it with
// commas ignored as separators, because a single specification is itself a comma-separated
// attribute list: `type=image,name=…,push=true`). The step publishes if ANY line does.
//
// Fail-closed at every branch, in the same two ways `push:` is:
//
//   - absent: this key sets no destination. Not on its own a verdict about the step.
//   - an expression with no event to decide against (the static question the runbook
//     cross-check asks): counted as PUBLISHING, so a step that might publish is a step the
//     runbook has to name.
//   - an expression this evaluator cannot decide for the event under test: an ERROR.
//   - a line whose exporter this gate does not model, whose `type=` is missing, whose
//     `push=` is not a boolean, or which cannot be read as attributes at all: an ERROR that
//     names the line. An unmodelled destination must red rather than read as harmless, and
//     an error is deliberately stronger than counting it as publishing here - a publish
//     miscounted as an act can be silenced by adding a runbook entry, whereas this can only
//     be silenced by teaching the gate what that exporter does.
func decideOutputsInput(key string, raw any, ctx *evalCtx) (publishes bool, detail string, err error) {
	switch t := raw.(type) {
	case nil:
		return false, "", nil
	case string:
		text := strings.TrimSpace(t)
		if text == "" {
			return false, "", fmt.Errorf("`%s:` is empty. An input that decides where this step's build is written must say where", key)
		}
		if strings.Contains(text, "${{") {
			if ctx == nil {
				return true, fmt.Sprintf("%s: %s - an expression, counted as publishing because no single event decides it", key, oneLine(text)), nil
			}
			v, ierr := Interpolate(text, *ctx)
			if ierr != nil {
				return false, "", fmt.Errorf("`%s: %s` cannot be decided for this event: %w", key, oneLine(text), ierr)
			}
			text = strings.TrimSpace(v)
			if text == "" {
				return false, "", fmt.Errorf("`%s:` evaluates to nothing for this event, so where this step's build is written is unknown", key)
			}
		}
		return decideBuildxOutputs(key+":", text)
	default:
		return false, "", fmt.Errorf("`%s:` is a %T (%v), not a list of buildx output specifications and not an expression, so where this step's build is written is unknown", key, raw, raw)
	}
}

// buildxLocalExporters write to the local filesystem or the local daemon. None of them can
// reach a registry, so none of them is a published act. Listed rather than defaulted: an
// exporter this gate has never heard of is an error, not a member of this set.
var buildxLocalExporters = map[string]bool{
	"local": true, "tar": true, "oci": true, "docker": true, "cacheonly": true,
}

// decideBuildxOutputs decides a list of buildx output specifications. It is the ONE model of
// where a build goes, and it is called from both spellings of that question: an action's
// `outputs:` input (decideOutputsInput, above) and a build command's `--output` flag
// (decideBuildDestination, in command.go). `display` is how the caller's spelling is named
// in a message, so the same refusal reads correctly either way.
func decideBuildxOutputs(display, text string) (bool, string, error) {
	for _, line := range strings.Split(text, "\n") {
		entry := strings.TrimSpace(line)
		if entry == "" {
			continue
		}
		attrs, aerr := parseCSVAttrs(entry)
		if aerr != nil {
			return false, "", fmt.Errorf("`%s` entry %q cannot be read as buildx output attributes (%v), so where it writes the build is unknown", display, entry, aerr)
		}
		typ, ok := attrs["type"]
		if !ok {
			return false, "", fmt.Errorf("`%s` entry %q names no `type=`, so which buildx exporter it selects - and whether that exporter writes to a registry - is unknown", display, entry)
		}
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch {
		case typ == "registry":
			return true, fmt.Sprintf("%s %s - the `registry` exporter, which buildx documents as `type=image,push=true`", display, entry), nil
		case typ == "image":
			push, has := attrs["push"]
			if !has {
				continue // the image exporter keeps its result locally unless told to push
			}
			switch strings.ToLower(strings.TrimSpace(push)) {
			case "true":
				return true, fmt.Sprintf("%s %s - the `image` exporter with push=true, which IS a registry push", display, entry), nil
			case "false":
				continue
			default:
				return false, "", fmt.Errorf("`%s` entry %q sets `push=%s`, which is not a boolean, so whether this step publishes is unknown", display, entry, push)
			}
		case buildxLocalExporters[typ]:
			continue
		default:
			return false, "", fmt.Errorf("`%s` entry %q selects the buildx exporter %q, which this gate does not model. Teach it that exporter - and whether it writes to a registry - rather than letting an unmodelled destination read as harmless", display, entry, typ)
		}
	}
	return false, "", nil
}

// parseCSVAttrs reads one buildx output specification: comma-separated `key=value` pairs,
// where a value may be double-quoted and may itself contain commas and `=`.
func parseCSVAttrs(entry string) (map[string]string, error) {
	out := map[string]string{}
	for _, field := range splitOutsideQuotes(entry, ',') {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			return nil, fmt.Errorf("%q is not a key=value attribute", field)
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" {
			return nil, fmt.Errorf("%q names no attribute", field)
		}
		out[k] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("it names no attributes at all")
	}
	return out, nil
}

func splitOutsideQuotes(s string, sep byte) []string {
	var (
		out    []string
		cur    strings.Builder
		quoted bool
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			quoted = !quoted
			cur.WriteByte(c)
		case c == sep && !quoted:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(out, cur.String())
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// PushedRefs reports the image references a publishing step actually publishes, read from
// BOTH spellings the action accepts: the `tags:` input, and the `name=` attribute of an
// `outputs:` entry that pushes. Reading only `tags:` made an absent `tags:` key mean "this
// push names nothing I need to check", which is the same absent-key inference that let a
// step publishing through `outputs:` disappear.
//
// An empty result from a step that DOES publish is the caller's problem to report: it means
// the gate cannot see what the step pushes, which is not the same as "it pushes nothing".
func (s Step) PushedRefs(ctx evalCtx) ([]string, error) {
	var out []string
	if raw, ok := s.With["tags"]; ok {
		tags, err := Interpolate(yamlString(raw), ctx)
		if err != nil {
			return nil, fmt.Errorf("`tags:` cannot be decided for this event: %w", err)
		}
		out = append(out, strings.Fields(strings.ReplaceAll(tags, ",", " "))...)
	}
	if raw, ok := s.With["outputs"]; ok {
		text, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("`outputs:` is a %T, not a list of buildx output specifications", raw)
		}
		resolved, err := Interpolate(text, ctx)
		if err != nil {
			return nil, fmt.Errorf("`outputs:` cannot be decided for this event: %w", err)
		}
		for _, line := range strings.Split(resolved, "\n") {
			entry := strings.TrimSpace(line)
			if entry == "" {
				continue
			}
			attrs, err := parseCSVAttrs(entry)
			if err != nil {
				return nil, fmt.Errorf("`outputs:` entry %q cannot be read as buildx output attributes: %v", entry, err)
			}
			if name := strings.TrimSpace(attrs["name"]); name != "" {
				for _, n := range strings.Split(name, ";") {
					if n = strings.TrimSpace(n); n != "" {
						out = append(out, n)
					}
				}
			}
		}
	}
	return out, nil
}

// TolerateFailure reports whether the step is marked so that its own failure does not
// fail the run. A14 is about exactly this marking, so an expression here (which this gate
// cannot decide without a job context) is treated as tolerant - fail-closed.
func (s Step) TolerateFailure() (bool, string) { return tolerates(s.ContinueOnError) }

func tolerates(v any) (bool, string) {
	switch t := v.(type) {
	case nil:
		return false, ""
	case bool:
		return t, fmt.Sprintf("continue-on-error: %v", t)
	case string:
		if strings.EqualFold(strings.TrimSpace(t), "false") {
			return false, ""
		}
		return true, fmt.Sprintf("continue-on-error: %s", t)
	default:
		return true, fmt.Sprintf("continue-on-error: %v", t)
	}
}

// --- roles the ordering property is stated in terms of --------------------------------

var (
	reMakeCheck  = regexp.MustCompile(`(^|[\s;&|(])make\s+(-[^\s]+\s+)*check(\s|$|[;&|)])`)
	reSmoke      = regexp.MustCompile(`smoke-image\.sh`)
	reDockerPull = regexp.MustCompile(`\bdocker\s+pull\b`)
	reArm64      = regexp.MustCompile(`\blinux/arm64\b`)
	reAmd64      = regexp.MustCompile(`\blinux/amd64\b`)
	reResolve    = regexp.MustCompile(`resolve-compose-image\.sh`)
)

// commands is the ONE reader of a step's shell, shared by the act catalogue and by every
// role detector below. There used to be two - a comment stripper and a continuation joiner -
// and having two was itself the defect: they disagreed about what a line is, in the
// fail-open direction. command.go says what one reader costs and buys.
func (s Step) commands() []Command { return ShellCommands(s.Run) }

// script is what the role detectors match against: the step's shell as the reader read it,
// one command per line, quoting removed. A pattern here can no longer be defeated by
// pressing return, because the line it sees is the LOGICAL one the shell would run.
func (s Step) script() string {
	var b strings.Builder
	for _, c := range s.commands() {
		b.WriteString(c.String())
		b.WriteByte('\n')
	}
	return b.String()
}

func (s Step) RunsFullGate() bool      { return reMakeCheck.MatchString(s.script()) }
func (s Step) RunsSmoke() bool         { return reSmoke.MatchString(s.script()) }
func (s Step) PullsFromRegistry() bool { return reDockerPull.MatchString(s.script()) }
func (s Step) ResolvesComposeRef() bool {
	return reResolve.MatchString(s.script())
}

// SmokeArches reports which architectures a smoke step covers. smoke-image.sh takes the
// platform as its second argument and defaults to the runner's own (amd64), so a step
// that names no platform is an amd64 smoke.
func (s Step) SmokeArches() []string {
	script := s.script()
	var out []string
	if reArm64.MatchString(script) {
		out = append(out, "linux/arm64")
	}
	if reAmd64.MatchString(script) || len(out) == 0 {
		out = append(out, "linux/amd64")
	}
	sort.Strings(out)
	return out
}

// PullArches reports which architectures a step pulls back from the registry.
func (s Step) PullArches() []string {
	script := s.script()
	var out []string
	if reArm64.MatchString(script) {
		out = append(out, "linux/arm64")
	}
	if reAmd64.MatchString(script) {
		out = append(out, "linux/amd64")
	}
	sort.Strings(out)
	return out
}
