package webui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/webui/gen"
)

// The MECHANICAL half of the styling conventions (S0053).
//
// Styling clause S1 is explicit that this one is a check OF SOURCE TEXT and says why it
// does not collide with F2: "Every colour lives in one committed token file per repo. No
// hex, rgb() or named colour appears anywhere else. The same holds for spacing lengths.
// Both are checked mechanically over the source text, which is a check OF source text
// rather than a grader of a rendered property."
//
// Everything in this file is therefore about what the SOURCES say. What the page PAINTS
// is a different question and is decided in conventions_rendered_test.go by a browser:
// a token this file proves is declared but the cascade never applies would pass here and
// fail there, which is exactly the division of labour the two clauses describe.

// --- reading the surface's sources ------------------------------------------------

// tokenFileName is the ONE committed token file (S1).
const tokenFileName = "tokens.css"

// otherSurfaceSources is every source of the surface that is NOT the token file. A colour
// or a length appearing in any of them is the thing S1 refuses.
//
// The script modules are in it, and are read from the MANIFEST rather than listed here.
// They are surface sources by the criterion's own words ("anywhere else in the surface's
// sources") and they set element attributes, so a value written into one of them reaches
// the page exactly as a value written into the stylesheet does. Reading the manifest also
// means a module added later is covered the day it is added, instead of the day somebody
// remembers to extend a list.
func otherSurfaceSources(t *testing.T) []string {
	t.Helper()
	mods, err := gen.Modules(os.DirFS(srcDir))
	if err != nil {
		t.Fatalf("reading the module manifest: %v", err)
	}
	if len(mods) == 0 {
		t.Fatalf("the module manifest names no script module; the surface has several")
	}
	return append([]string{"dashboard.css", "index.html.tmpl"}, mods...)
}

func readSurfaceSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(srcDir, name))
	if err != nil {
		t.Fatalf("cannot read the surface source %s: %v", name, err)
	}
	return string(b)
}

// readRepoDoc reads a document committed in this repository, by its repository-relative
// path. The tests run in internal/webui, so the repository root is two levels up.
func readRepoDoc(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("cannot read the committed document %s: %v", rel, err)
	}
	return string(b)
}

// stripCSSComments removes /* ... */ blocks, and stripHTMLComments removes <!-- ... -->.
// Both are removed before anything below scans for a colour or a length, deliberately: a
// comment paints nothing, and the token file's own records ("floor 4.5", "--sp-1: 4px")
// and the stylesheet's explanations would otherwise be read as declarations. What S1
// refuses is a VALUE outside the token file, not a mention of one.
func stripCSSComments(s string) string {
	var out strings.Builder
	for {
		i := strings.Index(s, "/*")
		if i < 0 {
			out.WriteString(s)
			return out.String()
		}
		out.WriteString(s[:i])
		j := strings.Index(s[i:], "*/")
		if j < 0 {
			return out.String()
		}
		s = s[i+j+2:]
	}
}

// stripJSLineComments removes `// ...` to the end of the line, which is the comment form
// the script modules use and the one neither of the strippers above sees. Same reason as
// theirs: a comment paints nothing, and the only value in any module today is the word
// "2px" inside a sentence explaining a stroke width.
//
// It is safe on these sources because none of them writes `//` inside a string literal or
// a regular expression - asserted below, so the day one does, the assertion says so
// instead of the stripper quietly eating the rest of that line.
func stripJSLineComments(s string) string {
	var out strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

func stripHTMLComments(s string) string {
	var out strings.Builder
	for {
		i := strings.Index(s, "<!--")
		if i < 0 {
			out.WriteString(s)
			return out.String()
		}
		out.WriteString(s[:i])
		j := strings.Index(s[i:], "-->")
		if j < 0 {
			return out.String()
		}
		s = s[i+j+3:]
	}
}

// --- the refusals ------------------------------------------------------------------

var (
	hexColourRe = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	colourFnRe  = regexp.MustCompile(`\b(rgba?|hsla?|hwb|lab|lch|oklab|oklch|color|color-mix|light-dark)\s*\(`)
	// A LENGTH literal, with a unit. A bare number is not a length (an SVG geometry
	// attribute, a z-index, a flex factor) and a percentage is a fraction of something
	// else rather than a spacing value, so neither is matched here.
	lengthRe = regexp.MustCompile(`(?i)(?:^|[^\w.#-])(\d+(?:\.\d+)?)(px|rem|em|pt|pc|in|cm|mm|q|ch|ex|vh|vw|vmin|vmax|vi|vb)\b`)
	// A declaration: `property: value` up to the ; or } that ends it.
	declRe = regexp.MustCompile(`(?m)([-\w]+)\s*:\s*([^;{}]*)`)
)

// cssNamedColours is the CSS Color Level 4 named-colour set. S2 says a token is named for
// its ROLE and never for its colour, and S1 says a named colour may not appear outside
// the token file; this is the set that sentence is about.
var cssNamedColours = strings.Fields(`
aliceblue antiquewhite aqua aquamarine azure beige bisque black blanchedalmond blue
blueviolet brown burlywood cadetblue chartreuse chocolate coral cornflowerblue cornsilk
crimson cyan darkblue darkcyan darkgoldenrod darkgray darkgreen darkgrey darkkhaki
darkmagenta darkolivegreen darkorange darkorchid darkred darksalmon darkseagreen
darkslateblue darkslategray darkslategrey darkturquoise darkviolet deeppink deepskyblue
dimgray dimgrey dodgerblue firebrick floralwhite forestgreen fuchsia gainsboro ghostwhite
gold goldenrod gray green greenyellow grey honeydew hotpink indianred indigo ivory khaki
lavender lavenderblush lawngreen lemonchiffon lightblue lightcoral lightcyan
lightgoldenrodyellow lightgray lightgreen lightgrey lightpink lightsalmon lightseagreen
lightskyblue lightslategray lightslategrey lightsteelblue lightyellow lime limegreen linen
magenta maroon mediumaquamarine mediumblue mediumorchid mediumpurple mediumseagreen
mediumslateblue mediumspringgreen mediumturquoise mediumvioletred midnightblue mintcream
mistyrose moccasin navajowhite navy oldlace olive olivedrab orange orangered orchid
palegoldenrod palegreen paleturquoise palevioletred papayawhip peachpuff peru pink plum
powderblue purple rebeccapurple red rosybrown royalblue saddlebrown salmon sandybrown
seagreen seashell sienna silver skyblue slateblue slategray slategrey snow springgreen
steelblue tan teal thistle tomato turquoise violet wheat white whitesmoke yellow
yellowgreen transparent currentcolor
`)

var namedColourSet = func() map[string]bool {
	m := map[string]bool{}
	for _, c := range cssNamedColours {
		m[c] = true
	}
	return m
}()

// valueEscapes reports every place in ONE non-token source where a colour or a length was
// written directly instead of coming from the token file (S1, S3). It is a pure function
// over the source text so that the check itself can be mutation-tested.
func valueEscapes(name, body string) []string {
	if strings.HasSuffix(name, ".tmpl") || strings.HasSuffix(name, ".html") {
		body = stripHTMLComments(body)
	}
	if strings.HasSuffix(name, ".js") {
		body = stripJSLineComments(body)
	}
	body = stripCSSComments(body)

	var out []string
	for _, m := range hexColourRe.FindAllString(body, -1) {
		out = append(out, fmt.Sprintf("%s: the hex colour %s is written here; every colour lives in %s (S1)", name, m, tokenFileName))
	}
	for _, m := range colourFnRe.FindAllString(body, -1) {
		out = append(out, fmt.Sprintf("%s: the colour function %s is written here; every colour lives in %s (S1)",
			name, strings.TrimSpace(m), tokenFileName))
	}
	// A media query's CONDITION is the one place on this surface where a length cannot
	// come from the token file: `@media (max-width: var(--x))` is not valid CSS and never
	// has been - a custom property is not substituted in a media feature. So the prelude
	// is removed before the length sweep, and the breakpoints it contained are held to
	// their own rule below instead of being pretended into the token file.
	body, breakpoints := splitMediaPreludes(body)
	for _, m := range lengthRe.FindAllStringSubmatch(body, -1) {
		out = append(out, fmt.Sprintf("%s: the length %s%s is written here; every length lives in %s (S1, S3)",
			name, m[1], m[2], tokenFileName))
	}
	// The rule that replaces it: this surface has ONE breakpoint. A page that scatters
	// them has a layout nobody can hold in their head, and a set of numbers nobody can
	// find - which is the failure the token file exists to prevent, arriving by the one
	// door the token file cannot cover.
	if len(breakpoints) > 1 {
		out = append(out, fmt.Sprintf("%s: %d different breakpoints are written here (%s); this surface declares ONE, because a media condition is the one length CSS cannot take from %s",
			name, len(breakpoints), strings.Join(breakpoints, ", "), tokenFileName))
	}
	for _, d := range declRe.FindAllStringSubmatch(body, -1) {
		prop, value := d[1], d[2]
		// A custom property may only be DECLARED in the token file; a declaration here
		// would be a second source of truth for a value.
		if strings.HasPrefix(prop, "--") {
			out = append(out, fmt.Sprintf("%s: the token %s is declared here; every token is declared in %s (S1)", name, prop, tokenFileName))
		}
		for _, tok := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
			return !(r >= 'a' && r <= 'z')
		}) {
			// `currentColor` introduces NO value. It defers to the colour already in
			// force on the element, which on this surface can only have come from a
			// token - so it is the one keyword that SERVES S1 rather than escaping it.
			// The alternative is worse by the clause's own measure: the connection
			// state's dot takes its colour from the state, so without this it needs one
			// rule per state naming that state's token again, which is three more
			// colour references in this file rather than none.
			//
			// `transparent` is left in the banned set deliberately. It is a value, it
			// paints nothing, and a boundary painted with it is a boundary that fails the
			// 3:1 floor while looking, in the source, like it was drawn.
			if tok == "currentcolor" {
				continue
			}
			if namedColourSet[tok] {
				out = append(out, fmt.Sprintf("%s: the named colour %q appears in `%s: %s`; every colour lives in %s (S1)",
					name, tok, prop, strings.TrimSpace(value), tokenFileName))
			}
		}
	}
	sort.Strings(out)
	return out
}

// mediaPreludeRe matches a media query's condition - everything from `@media` up to the
// block it opens - which is where a breakpoint length is written.
var mediaPreludeRe = regexp.MustCompile(`@media[^{]*`)

// mediaBreakpointRe pulls the lengths out of one prelude.
var mediaBreakpointRe = regexp.MustCompile(`\d+(?:\.\d+)?(?:px|em|rem|ch)`)

// splitMediaPreludes removes every media-query condition from a stylesheet and returns
// the distinct breakpoint lengths they named, in the order first seen. A feature query
// with no length in it (prefers-color-scheme, prefers-reduced-motion) contributes none,
// which is why those two blocks are not breakpoints and are not counted as any.
func splitMediaPreludes(body string) (string, []string) {
	var seen []string
	inSeen := map[string]bool{}
	out := mediaPreludeRe.ReplaceAllStringFunc(body, func(prelude string) string {
		for _, bp := range mediaBreakpointRe.FindAllString(prelude, -1) {
			if !inSeen[bp] {
				inSeen[bp] = true
				seen = append(seen, bp)
			}
		}
		return "@media "
	})
	return out, seen
}

// stripJSLineComments is only safe while no module writes `//` anywhere but at the start
// of a comment line. That is a condition on the sources, so it is ASSERTED rather than
// assumed: the day a module carries a URL or a regular expression containing `//`, this
// reds and names the line, instead of the stripper silently eating the rest of it and
// hiding a value written after it.
func TestTokens_NoScriptModuleWritesADoubleSlashOutsideALineComment(t *testing.T) {
	modules := 0
	for _, name := range otherSurfaceSources(t) {
		if !strings.HasSuffix(name, ".js") {
			continue
		}
		modules++
		for i, line := range strings.Split(readSurfaceSource(t, name), "\n") {
			j := strings.Index(line, "//")
			if j < 0 {
				continue
			}
			if strings.TrimSpace(line[:j]) != "" {
				t.Errorf("%s:%d writes `//` after code (%q); stripJSLineComments would eat the rest of that line and could hide a value written there",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
	if modules == 0 {
		t.Fatal("no script module was checked; the surface has several and they are surface sources by S1's own words")
	}
}

func TestTokens_NoColourAndNoLengthEscapesTheOneTokenFile(t *testing.T) {
	for _, name := range otherSurfaceSources(t) {
		if probs := valueEscapes(name, readSurfaceSource(t, name)); probs != nil {
			for _, p := range probs {
				t.Error(p)
			}
		}
	}
}

// The check has to BITE. Each mutation writes exactly one value where S1 forbids it, and
// a check that reports none of them is a check that could never have caught the fourteen
// off-scale lengths this repository actually carried.
// The three narrowings above each removed something the escape check used to refuse, so
// each is driven here against what it must STILL refuse. A narrowing nobody tries to
// defeat is a hole nobody knows about - which is the whole reason this file already
// mutation-tests the check it belongs to.
func TestTokens_TheNarrowedEscapeCheckStillRefusesWhatItAlwaysDid(t *testing.T) {
	for _, c := range []struct {
		what string
		body string
		want string // a substring of the complaint the check must make
	}{
		// currentColor is admitted; every other named colour is not, including the one
		// that paints nothing and would silently fail a contrast floor.
		{"a named colour beside currentColor", `a { color: currentColor; border-color: rebeccapurple; }`, "rebeccapurple"},
		{"transparent, which is a value and not a reference", `a { background: transparent; }`, "transparent"},
		{"currentcolor spelled as a value in a shorthand", `a { border: 1px solid darkred; }`, "darkred"},

		// A media PRELUDE is exempt; the block it opens is not.
		{"a length inside a media block", "@media (max-width: 760px) { a { padding: 13px; } }", "13px"},
		{"a hex colour inside a media block", "@media (max-width: 760px) { a { color: #abcdef; } }", "#abcdef"},
		{"a second breakpoint", "@media (max-width: 760px) { a { color: red; } }\n@media (min-width: 900px) { b { color: red; } }", "2 different breakpoints"},

		// The two new families are families in the TOKEN file. Writing one of their
		// values inline is still an escape.
		{"a tracking value written inline", `a { letter-spacing: 0.04em; }`, "0.04em"},
	} {
		probs := valueEscapes("dashboard.css", c.body)
		found := false
		for _, p := range probs {
			if strings.Contains(p, c.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("the narrowed check no longer refuses %s; it reported %v, wanted something naming %q",
				c.what, probs, c.want)
		}
	}

	// And the three things it now admits are admitted for the stated reasons.
	for _, c := range []struct{ what, body string }{
		{"currentColor, which introduces no value", `a { background: currentColor; }`},
		{"one breakpoint in a media prelude", "@media (max-width: 760px) { a { color: var(--fg); } }"},
		{"a feature query carrying no length at all", "@media (prefers-reduced-motion: reduce) { a { color: var(--fg); } }"},
	} {
		if probs := valueEscapes("dashboard.css", c.body); probs != nil {
			t.Errorf("the check refuses %s, which the narrowing exists to admit: %v", c.what, probs)
		}
	}
}

func TestTokens_TheEscapeCheckFailsAgainstEveryWayAValueCanBeWrittenInline(t *testing.T) {
	clean := readSurfaceSource(t, "dashboard.css")
	if probs := valueEscapes("dashboard.css", clean); probs != nil {
		t.Fatalf("the shipped stylesheet already fails the check: %v", probs)
	}
	for name, mutant := range map[string]string{
		"a hex colour":                            clean + "\n.mutant { color: #ff0000; }\n",
		"a short hex colour":                      clean + "\n.mutant { color: #f00; }\n",
		"an rgb() colour":                         clean + "\n.mutant { color: rgb(255, 0, 0); }\n",
		"an hsl() colour":                         clean + "\n.mutant { color: hsl(0, 100%, 50%); }\n",
		"a named colour":                          clean + "\n.mutant { color: rebeccapurple; }\n",
		"a spacing length in px":                  clean + "\n.mutant { padding: 7px; }\n",
		"an off-scale spacing length":             clean + "\n.mutant { margin: 9px 22px; }\n",
		"a length in rem":                         clean + "\n.mutant { gap: 1.5rem; }\n",
		"a length in em":                          clean + "\n.mutant { padding-top: 2em; }\n",
		"a token declared outside the token file": clean + "\n.mutant { --sneaky: 3px; }\n",
		"a colour hidden inside a shorthand":      clean + "\n.mutant { border: 1px solid #123456; }\n",
	} {
		if mutant == clean {
			t.Fatalf("the mutation %q did not change the stylesheet - the assertion below would be vacuous", name)
		}
		if probs := valueEscapes("dashboard.css", mutant); probs == nil {
			t.Errorf("the check PASSED a stylesheet mutated to defeat it (%s)", name)
		}
	}
	// And it must not fire on a COMMENT that merely mentions a value, or the rule would
	// be unwritable-about and every explanation in the file would have to be deleted.
	mentioned := clean + "\n/* the old value here was #0f1115 with 7px of padding */\n"
	if probs := valueEscapes("dashboard.css", mentioned); probs != nil {
		t.Errorf("the check fired on a comment that only MENTIONS a value: %v", probs)
	}
}

// --- the token file itself ----------------------------------------------------------

// s2Vocabulary is the fifteen role names clause S2 fixes, verbatim. Every repository
// defines these names; the VALUES are each product's own.
var s2Vocabulary = []string{
	"--bg", "--panel", "--line", "--fg", "--muted", "--accent", "--ok", "--warn", "--bad",
	"--border", "--mark", "--focus", "--disabled", "--selected", "--link",
}

// tokenGroup is one family of tokens and the rule its values must satisfy. A token that
// belongs to NO group is an error: that is what stops a new ad-hoc value from being
// smuggled in under a name the spacing rule does not happen to cover.
type tokenGroup struct {
	prefix string
	// fourPixelScale requires every value to be a whole multiple of 4px (S3).
	fourPixelScale bool
	// why records, for the groups that are NOT on the 4px scale, the reason they are not.
	why string
	// check validates one value; nil accepts anything.
	check func(v string) error
}

var pxRe = regexp.MustCompile(`^(\d+)px$`)

func mustPx(v string) (int, error) {
	m := pxRe.FindStringSubmatch(v)
	if m == nil {
		return 0, fmt.Errorf("%q is not a whole number of px", v)
	}
	n, _ := strconv.Atoi(m[1])
	return n, nil
}

// tokenGroups is the whole vocabulary of this surface, and the reason each group that is
// not on the 4px spacing scale is not on it. S3 puts SPACING on a 4px scale; a type ramp,
// a hairline border and a corner radius are not spacing, and a 4px type ramp would leave
// 12, 16 and 20 as the only sizes below the large-text threshold, which is not a ramp.
var tokenGroups = []tokenGroup{
	{prefix: "--sp-", fourPixelScale: true},
	{prefix: "--size-", fourPixelScale: true},
	{prefix: "--w-", fourPixelScale: true},
	{prefix: "--fs-", why: "the type scale is a ramp, not spacing", check: func(v string) error {
		n, err := mustPx(v)
		if err != nil {
			return err
		}
		if n < 12 {
			return fmt.Errorf("%q is below the 12px floor this surface sets for readable text", v)
		}
		return nil
	}},
	{prefix: "--lh-", why: "a line height is a ratio, not a length", check: func(v string) error {
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			return fmt.Errorf("%q is not a unitless ratio", v)
		}
		return nil
	}},
	{prefix: "--bw-", why: "a hairline border is 1px; a 4px hairline is not a hairline", check: func(v string) error {
		_, err := mustPx(v)
		return err
	}},
	{prefix: "--radius-", why: "a corner radius is not spacing", check: func(v string) error {
		_, err := mustPx(v)
		return err
	}},
	{prefix: "--measure-", why: "a prose measure is counted in characters, not pixels", check: func(v string) error {
		if !strings.HasSuffix(v, "ch") {
			return fmt.Errorf("%q is not a measure in ch", v)
		}
		return nil
	}},
	{prefix: "--ls-", why: "letter spacing tracks the TYPE SIZE, not the 4px grid: one value has to work under a 24px heading and under a 12px column header", check: func(v string) error {
		if !strings.HasSuffix(v, "em") {
			return fmt.Errorf("%q is not a tracking value in em", v)
		}
		if _, err := strconv.ParseFloat(strings.TrimSuffix(v, "em"), 64); err != nil {
			return fmt.Errorf("%q is not a number of em", v)
		}
		return nil
	}},
	// A SURFACE is a colour a reader never reads text off directly - it is what text is
	// read ON - so it is not one of S2's fifteen ROLE names, which are named for what
	// they colour. It is still a colour, so it is still hand-authored per theme and it
	// still carries a recorded contrast ratio for everything drawn on it.
	{prefix: "--surface-", why: "a surface is a colour, not a length", check: func(v string) error {
		if !hexColourRe.MatchString(v) {
			return fmt.Errorf("%q is not a colour value", v)
		}
		return nil
	}},
	{prefix: "--font-", why: "a family list is not a length"},
	{prefix: "--shadow-", why: "a shadow carries its own offsets and its own colour per theme (S8)"},
	{prefix: "--motion-", why: "a duration is not a length (S9)", check: func(v string) error {
		if !strings.HasSuffix(v, "ms") && !strings.HasSuffix(v, "s") {
			return fmt.Errorf("%q is not a duration", v)
		}
		return nil
	}},
}

var tokenDeclRe = regexp.MustCompile(`(?m)^\s*(--[-\w]+)\s*:\s*([^;]+);`)

// tokenDeclarations returns every token declared in one block of the token file, in
// declaration order.
func tokenDeclarations(block string) [][2]string {
	var out [][2]string
	for _, m := range tokenDeclRe.FindAllStringSubmatch(stripCSSComments(block), -1) {
		out = append(out, [2]string{m[1], strings.TrimSpace(m[2])})
	}
	return out
}

// themeBlocks splits the token file into its two palettes: the DEFAULT block on :root,
// which is the light palette and the one an engine reporting no preference renders, and
// the block inside the prefers-color-scheme: dark query.
func themeBlocks(t *testing.T, body string) (light, dark string) {
	t.Helper()
	i := strings.Index(body, "@media (prefers-color-scheme: dark)")
	if i < 0 {
		t.Fatal("the token file declares no dark palette under a prefers-color-scheme query")
	}
	j := strings.Index(body[i:], "@media (prefers-reduced-motion")
	if j < 0 {
		t.Fatal("the token file declares no prefers-reduced-motion block")
	}
	return body[:i], body[i : i+j]
}

func TestTokens_TheS2VocabularyIsCompleteInBothThemes(t *testing.T) {
	body := readSurfaceSource(t, tokenFileName)
	light, dark := themeBlocks(t, body)

	for _, c := range []struct{ name, block string }{{"light (the :root default)", light}, {"dark", dark}} {
		have := map[string]string{}
		for _, d := range tokenDeclarations(c.block) {
			have[d[0]] = d[1]
		}
		for _, want := range s2Vocabulary {
			v, ok := have[want]
			if !ok {
				t.Errorf("the %s palette does not define %s, which clause S2 requires of every repository", c.name, want)
				continue
			}
			if !hexColourRe.MatchString(v) {
				t.Errorf("the %s palette defines %s as %q, which is not a colour value", c.name, want, v)
			}
		}
	}

	// Hand-authored, not derived (S6): the two palettes must actually differ, value by
	// value, on every role. A dark palette that repeated a light value would be the
	// derived palette S6 refuses, wearing two names.
	lightVals, darkVals := map[string]string{}, map[string]string{}
	for _, d := range tokenDeclarations(light) {
		lightVals[d[0]] = d[1]
	}
	for _, d := range tokenDeclarations(dark) {
		darkVals[d[0]] = d[1]
	}
	for _, name := range s2Vocabulary {
		if lightVals[name] == darkVals[name] {
			t.Errorf("%s carries the same value %q in both themes; every token gets an explicit value of its own per theme (S6)",
				name, lightVals[name])
		}
	}
}

func TestTokens_EverySpacingTokenIsOnTheFourPixelScale(t *testing.T) {
	body := readSurfaceSource(t, tokenFileName)
	colour := map[string]bool{}
	for _, n := range s2Vocabulary {
		colour[n] = true
	}

	shadows := 0
	seenGroup := map[string]bool{}
	for _, d := range tokenDeclarations(body) {
		name, value := d[0], d[1]
		if colour[name] {
			continue
		}
		var g *tokenGroup
		for i := range tokenGroups {
			if strings.HasPrefix(name, tokenGroups[i].prefix) {
				g = &tokenGroups[i]
				break
			}
		}
		if g == nil {
			t.Errorf("the token %s belongs to no declared group: it is neither one of the fifteen role colours nor a member of a named length, type, family, shadow or motion family, so no rule governs its value",
				name)
			continue
		}
		seenGroup[g.prefix] = true
		if g.prefix == "--shadow-" {
			shadows++
		}
		if g.fourPixelScale {
			n, err := mustPx(value)
			if err != nil {
				t.Errorf("%s is %q: every %s* token is a whole number of px on the 4px scale (S3)", name, value, g.prefix)
				continue
			}
			if n <= 0 || n%4 != 0 {
				t.Errorf("%s is %dpx, which is off the 4px scale clause S3 fixes (4, 8, 12, 16, 24, 32, and so on)", name, n)
			}
			continue
		}
		if g.check != nil {
			if err := g.check(value); err != nil {
				t.Errorf("%s: %v (this group is exempt from the 4px scale because %s)", name, err, g.why)
			}
		}
	}

	// S8: two or three shadow tokens for genuinely raised surfaces, and no more. The
	// count is over the DEFAULT palette; the dark block redeclares the same names.
	lightBlock, _ := themeBlocks(t, body)
	lightShadows := 0
	for _, d := range tokenDeclarations(lightBlock) {
		if strings.HasPrefix(d[0], "--shadow-") {
			lightShadows++
		}
	}
	if lightShadows == 0 || lightShadows > 3 {
		t.Errorf("the token file declares %d shadow tokens; clause S8 allows two or three for genuinely raised surfaces", lightShadows)
	}
	// Every shadow token carries its own value PER THEME (S8): a shadow tuned for dark
	// does not work on light.
	_, darkBlock := themeBlocks(t, body)
	darkShadow := map[string]string{}
	for _, d := range tokenDeclarations(darkBlock) {
		if strings.HasPrefix(d[0], "--shadow-") {
			darkShadow[d[0]] = d[1]
		}
	}
	for _, d := range tokenDeclarations(lightBlock) {
		if !strings.HasPrefix(d[0], "--shadow-") {
			continue
		}
		v, ok := darkShadow[d[0]]
		if !ok {
			t.Errorf("%s has no value of its own in the dark palette; a shadow tuned for dark does not work on light (S8)", d[0])
		} else if v == d[1] {
			t.Errorf("%s carries the same value in both themes (%q); S8 requires its own value per theme", d[0], v)
		}
	}
}

// --- F8: the documentation links resolve --------------------------------------------

// The link's content may be a MARK rather than text, so the second group is anything up
// to the closing tag rather than "characters that are not markup". What this regex is for
// is the HREF - the check below resolves it to a committed document and an anchor that
// document carries - and the content is captured only so a link with neither text nor a
// name can be named in the failure.
var doclinkRe = regexp.MustCompile(`(?s)<a class="doclink" href="([^"]+)"[^>]*>(.*?)</a>`)

// headingAnchors returns every GitHub-style fragment a markdown document carries, derived
// from its headings the way every markdown renderer derives them: lower case, spaces to
// hyphens, everything else that is not alphanumeric or a hyphen removed.
func headingAnchors(doc string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(doc, "\n") {
		if !strings.HasPrefix(line, "#") {
			continue
		}
		text := strings.TrimSpace(strings.TrimLeft(line, "#"))
		var b strings.Builder
		for _, r := range strings.ToLower(text) {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				b.WriteRune(r)
			case r == ' ', r == '-':
				b.WriteRune('-')
			}
		}
		out[b.String()] = true
	}
	return out
}

func TestDocs_EveryDocumentationLinkResolvesToACommittedDocument(t *testing.T) {
	doc := string(render(offerFor(upstreamForTest)))
	links := doclinkRe.FindAllStringSubmatch(doc, -1)
	if len(links) != len(docLinks) {
		t.Fatalf("the served document renders %d documentation links, want one per region (%d)", len(links), len(docLinks))
	}
	base := upstreamForTest + "/blob/main/"
	for _, l := range links {
		href, text := l[1], l[2]
		if strings.TrimSpace(text) == "" {
			t.Errorf("the documentation link %q has no text, so it has no accessible name", href)
		}
		rest, ok := strings.CutPrefix(href, base)
		if !ok {
			t.Errorf("the documentation link %q does not point into the tree this binary was built from (%s)", href, base)
			continue
		}
		path, fragment, _ := strings.Cut(rest, "#")
		if _, err := os.Stat(filepath.Join("..", "..", filepath.FromSlash(path))); err != nil {
			t.Errorf("the documentation link %q names %q, which is not a document committed in this repository: %v", href, path, err)
			continue
		}
		if fragment == "" {
			t.Errorf("the documentation link %q carries no fragment, so it does not name the region's own section", href)
			continue
		}
		if anchors := headingAnchors(readRepoDoc(t, path)); !anchors[fragment] {
			t.Errorf("the documentation link %q names the anchor %q, which %s carries no heading for", href, fragment, path)
		}
	}
}

// upstreamForTest is the source URL the served document is rendered for here. The doc
// links are built from whatever tree the binary was built from, exactly as the AGPL
// offer is, and the default build's tree is this repository.
const upstreamForTest = "https://github.com/NSchatz/holdfast"

// movedClaims is every claim that USED to be printed on the dashboard and now lives in
// the linked document (clause F8: "The claims are not dropped; they move"). Each is
// asserted to be absent from the surface and present in the document, so a claim cannot
// be quietly deleted under cover of the relocation.
// movedClaims is every claim taken off the dashboard's surface, read from the ONE file
// that holds them. See internal/webui/prose/moved-claims.txt for why it is a file.
var movedClaims = readMovedClaims()

func readMovedClaims() []string {
	body, err := os.ReadFile(filepath.Join("prose", "moved-claims.txt"))
	if err != nil {
		panic("reading the moved-claims list: " + err.Error())
	}
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		panic("the moved-claims list is empty; a check with no subject proves nothing")
	}
	return out
}

// flattenText collapses every run of whitespace to one space, so a claim is compared as
// TEXT rather than as a particular line wrapping. A claim that survived the move but was
// re-wrapped is still the same claim; one that was deleted is not.
func flattenText(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestDocs_EveryClaimMovedOffTheSurfaceIsInTheLinkedDocument(t *testing.T) {
	doc := flattenText(readRepoDoc(t, DocPath))
	for _, claim := range movedClaims {
		if !strings.Contains(doc, flattenText(claim)) {
			t.Errorf("the claim %q was taken off the dashboard and is NOT in %s; F8 moves claims, it does not drop them",
				claim, DocPath)
		}
	}
}
