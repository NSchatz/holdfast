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
// The second question has a sharp edge, and decideRequiredInput is where it is handled: an
// action input can decide the act by itself (`docker/build-push-action` publishes when
// `push:` is true), and GitHub lets that input be an EXPRESSION - `push: ${{
// github.event_name != 'pull_request' }}` is the action's own documented idiom. Asking
// whether the literal text says "true" answers the wrong question, because an expression is
// never the string "true", so a step that publishes on the event under test would read as
// harmless. Such an input is therefore DECIDED, through the same evaluator that decides
// `if:` guards, against the same event shape - and everything undecidable is an error, not
// a false.

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

// runDetectors are matched against the step's run script with comments removed.
var runDetectors = []struct {
	kind ActKind
	re   *regexp.Regexp
	what string
}{
	{ActImagePush, regexp.MustCompile(`\bdocker\s+push\b`), "docker push"},
	{ActImagePush, regexp.MustCompile(`\bpodman\s+push\b`), "podman push"},
	{ActImagePush, regexp.MustCompile(`\bdocker\s+(buildx\s+)?(build|bake)\b[^\n]*--push\b`), "a buildx build with --push"},
	{ActTagMove, regexp.MustCompile(`\bimagetools\s+create\b`), "docker buildx imagetools create"},
	{ActTagMove, regexp.MustCompile(`\bdocker\s+manifest\s+push\b`), "docker manifest push"},
	{ActTagMove, regexp.MustCompile(`\b(crane|regctl)\s+(tag|index)\b`), "a registry tag/index write"},
	{ActRelease, regexp.MustCompile(`\bgh\s+release\s+(create|edit|upload|delete)\b`), "gh release"},
	{ActRelease, regexp.MustCompile(`\bgh\s+api\b[^\n]*/releases\b`), "a gh api call against /releases"},
	{ActRefPush, regexp.MustCompile(`\bgit\s+push\b`), "git push"},
	{ActArtefact, regexp.MustCompile(`\b(npm|pnpm)\s+publish\b`), "npm publish"},
	{ActArtefact, regexp.MustCompile(`\bcargo\s+publish\b`), "cargo publish"},
	{ActArtefact, regexp.MustCompile(`\bhelm\s+push\b`), "helm push"},
	{ActArtefact, regexp.MustCompile(`\b(skopeo|oras)\s+(copy|push)\b`), "a registry copy/push"},
}

// usesDetectors are matched against a step's `uses:` and its `with:` inputs.
var usesDetectors = []struct {
	kind    ActKind
	action  *regexp.Regexp
	require string // a `with:` key that must be TRUE, empty = the action always publishes
	what    string
}{
	{ActImagePush, regexp.MustCompile(`^docker/build-push-action`), "push", "docker/build-push-action"},
	{ActRelease, regexp.MustCompile(`^softprops/action-gh-release`), "", "softprops/action-gh-release"},
	{ActRelease, regexp.MustCompile(`^ncipollo/release-action`), "", "ncipollo/release-action"},
	{ActRefPush, regexp.MustCompile(`^ad-m/github-push-action`), "", "ad-m/github-push-action"},
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
	script := StripShellComments(s.Run)
	for _, d := range runDetectors {
		if d.re.MatchString(script) {
			out = append(out, Act{Kind: d.kind, Step: s, Why: d.what})
		}
	}
	for _, d := range usesDetectors {
		if !d.action.MatchString(s.Uses) {
			continue
		}
		why := d.what
		if d.require != "" {
			publishes, detail, err := decideRequiredInput(d.require, s.With[d.require], ctx)
			if err != nil {
				return nil, fmt.Errorf("%s uses %s, and %w.\nThis gate will not read an input it cannot decide as harmless: a step wrongly called publishing reds this gate, one wrongly called harmless is how an unreviewed publish ships", s.Label(), s.Uses, err)
			}
			if !publishes {
				continue
			}
			why = d.what + " (" + detail + ")"
		}
		out = append(out, Act{Kind: d.kind, Step: s, Why: why})
	}
	return out, nil
}

// decideRequiredInput decides a `with:` input that by itself makes an action publish.
//
// Fail-closed at every branch, because the wrong way to be wrong here is a silent false:
//
//   - absent: the action's own default, and a build step with no `push:` is a local build.
//     Not an act. This is the only "no" that is inferred rather than read.
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

func (s Step) script() string { return StripShellComments(s.Run) }

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

// StripShellComments removes `#` comments from a shell script without stripping a `#`
// that is inside a quoted string. Comment text is prose, and prose that mentions `docker
// push` must not be read as a publishing act - nor must a real `docker push` hide behind
// a quote.
func StripShellComments(script string) string {
	var out strings.Builder
	for _, line := range strings.Split(script, "\n") {
		var (
			single, double bool
			cut            = -1
		)
		for i := 0; i < len(line); i++ {
			c := line[i]
			switch {
			case c == '\\' && double:
				i++
			case c == '\'' && !double:
				single = !single
			case c == '"' && !single:
				double = !double
			case c == '#' && !single && !double:
				if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' {
					cut = i
				}
			}
			if cut >= 0 {
				break
			}
		}
		if cut >= 0 {
			line = line[:cut]
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}
