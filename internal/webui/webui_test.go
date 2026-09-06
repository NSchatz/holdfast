package webui

import (
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The UI must be baked into the binary (go:embed) — non-empty and real HTML.
func TestEmbeddedIndexIsPresent(t *testing.T) {
	if len(indexHTML) == 0 {
		t.Fatal("index.html was not embedded (empty)")
	}
	s := string(indexHTML)
	for _, want := range []string{"<!DOCTYPE html>", "/api/events", "id=\"queue\"", "id=\"history\""} {
		if !strings.Contains(s, want) {
			t.Fatalf("embedded index.html missing %q", want)
		}
	}
}

func TestHandlerServesIndexAtRoot(t *testing.T) {
	h := Handler()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: code %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp == "" {
		t.Fatal("missing Content-Security-Policy header")
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "<title>holdfast</title>") {
		t.Fatal("served body is not the dashboard page")
	}
}

// The honesty constraint is a REFUSAL, not a preference (TRANSCODE-14). The page may
// state what a VMAF number licenses, but it must NEVER call an encode visually lossless
// or identical, never grade it, and never rank two files' scores. These are properties
// of the page SOURCE — the render is client-side, so the forbidden strings must not
// exist in the template at all, and the honest labels must.
func TestHonestCopy_NoOverclaimStringsPresent(t *testing.T) {
	s := strings.ToLower(string(indexHTML))
	// Verboten: fidelity claims VMAF does not license, and grades that rank a score.
	for _, banned := range []string{
		"visually lossless", "lossless", "identical", "no visible artifacts",
		"no visible artefacts", "indistinguishable", "perfect quality",
		"grade a", "grade b", "5 stars", "★",
	} {
		if strings.Contains(s, banned) {
			t.Errorf("index.html must not claim %q — VMAF licenses no such statement", banned)
		}
	}
}

// The score is meaningless without its viewing condition, so the page shows the model,
// the pooling, the luma-only blind spot, and BOTH pooled statistics — mean AND worst.
func TestHonestCopy_ShowsModelPoolingAndWorstFrame(t *testing.T) {
	s := string(indexHTML)
	for _, want := range []string{
		"vmaf_model",  // the model travels with the score
		"vmaf_min",    // the worst-frame statistic is rendered, not just the mean
		"worst frame", // and labelled as such
		"luma-only",   // the documented blind spot is shown, not buried
		"pooling",     // how the score was pooled
		"vs your source",
		"not recorded", // a nil outcome renders honestly, never as 0
	} {
		if !strings.Contains(s, want) {
			t.Errorf("index.html missing honest-copy element %q", want)
		}
	}
}

// The page must render the persisted outcome facts and the durable lifetime total, and
// key skips off the engine's guard vocabulary rather than the bare word "skipped".
func TestDashboard_RendersOutcomeFields(t *testing.T) {
	s := string(indexHTML)
	for _, want := range []string{
		"bytes_reclaimed_lifetime", // the durable total, not just the session counter
		"source_bytes", "output_bytes", "encode_ms", "encoder",
		"hardlinked", "low-bitrate", "already-at-target-codec", // guard labels
	} {
		if !strings.Contains(s, want) {
			t.Errorf("index.html missing outcome/guard element %q", want)
		}
	}
}

// The pre-rename brand must be gone from the heading (TRANSCODE-14 also fixes this
// cosmetic leak). The banned identifier `trans`+`code` as an adjacent split must not
// appear; the title/heading say holdfast.
func TestBrand_NoPreRenameHeading(t *testing.T) {
	s := string(indexHTML)
	if strings.Contains(s, "trans<span") {
		t.Error("index.html still renders the pre-rename brand in the heading")
	}
	if !strings.Contains(s, "hold<span") && !strings.Contains(s, ">holdfast<") {
		t.Error("index.html heading does not show the holdfast brand")
	}
}

// TRANSCODE-15 (1): the render idiom. Rows are built as DOM nodes (createElement +
// textContent), NEVER by assigning an HTML string to a sink — the untrusted data here is
// attacker-influencable media file paths. The page must therefore contain no HTML-sink
// assignment at all: no innerHTML / outerHTML / insertAdjacentHTML / document.write.
func TestRenderIdiom_NoHTMLStringSinkFromJobData(t *testing.T) {
	s := string(indexHTML)
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(s, sink) {
			t.Errorf("index.html uses the HTML-string sink %q — rows must be built with createElement + textContent, so an attacker-influencable path is inert text", sink)
		}
	}
	// And the prescribed structure primitives are actually used.
	for _, want := range []string{"createElement", "textContent", "<template", ".content.cloneNode"} {
		if !strings.Contains(s, want) {
			t.Errorf("index.html missing the safe-render primitive %q", want)
		}
	}
}

// TRANSCODE-15 (1): Trusted Types is adopted — the response CSP enforces it, turning the
// no-string-sink discipline into a browser-enforced guarantee.
func TestTrustedTypes_EnforcedByCSP(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "require-trusted-types-for 'script'") {
		t.Fatalf("CSP does not enforce Trusted Types: %q", csp)
	}
}

// contrastRatio is the WCAG 2.x relative-luminance contrast ratio between two #rrggbb
// colors — the measurement the a11y test asserts against, not an eyeballed guess.
func contrastRatio(t *testing.T, hexA, hexB string) float64 {
	t.Helper()
	lum := func(hex string) float64 {
		var rgb [3]float64
		for i := 0; i < 3; i++ {
			var v int
			for _, c := range hex[1+i*2 : 3+i*2] {
				v *= 16
				switch {
				case c >= '0' && c <= '9':
					v += int(c - '0')
				case c >= 'a' && c <= 'f':
					v += int(c-'a') + 10
				case c >= 'A' && c <= 'F':
					v += int(c-'A') + 10
				default:
					t.Fatalf("bad hex color %q", hex)
				}
			}
			c := float64(v) / 255
			if c <= 0.03928 {
				rgb[i] = c / 12.92
			} else {
				rgb[i] = math.Pow((c+0.055)/1.055, 2.4)
			}
		}
		return 0.2126*rgb[0] + 0.7152*rgb[1] + 0.0722*rgb[2]
	}
	la, lb := lum(hexA), lum(hexB)
	hi, lo := math.Max(la, lb), math.Min(la, lb)
	return (hi + 0.05) / (lo + 0.05)
}

// cssVar extracts a `--name:#rrggbb` token value from the embedded page.
func cssVar(t *testing.T, name string) string {
	t.Helper()
	m := regexp.MustCompile(name + `:\s*(#[0-9a-fA-F]{6})`).FindStringSubmatch(string(indexHTML))
	if m == nil {
		t.Fatalf("CSS token %s not found", name)
	}
	return m[1]
}

// TRANSCODE-15 (2): a11y, MEASURED. The border token that draws every button/input edge
// must clear WCAG 2.2's 3:1 non-text floor on the page background (the shipped default
// --line was 1.31:1), and body text must clear the 4.5:1 text floor.
func TestAccessibility_ContrastMeasured(t *testing.T) {
	bg := cssVar(t, "--bg")
	if got := contrastRatio(t, cssVar(t, "--border"), bg); got < 3.0 {
		t.Errorf("--border on --bg is %.2f:1, under the 3:1 non-text floor", got)
	}
	if got := contrastRatio(t, cssVar(t, "--fg"), bg); got < 4.5 {
		t.Errorf("--fg on --bg is %.2f:1, under the 4.5:1 text floor", got)
	}
	// The border token must actually be the one drawing the interactive edges, not a
	// defined-but-unused value: buttons and control inputs reference it.
	s := string(indexHTML)
	if !strings.Contains(s, "button {") || !strings.Contains(s, "border:1px solid var(--border)") {
		t.Error("buttons/inputs do not draw their border from the accessible --border token")
	}
}

// TRANSCODE-15 (2): a keyboard user must see focus and the token field must have a real
// label (not just a placeholder); the SSE regions get a POLITE summary region, not a
// live-region on the whole table (which would spam a screen reader on every snapshot).
func TestAccessibility_FocusLabelAndPoliteLiveRegion(t *testing.T) {
	s := string(indexHTML)
	if !strings.Contains(s, ":focus-visible") {
		t.Error("no :focus-visible ring — a keyboard-only pass has no visible focus")
	}
	if !strings.Contains(s, `<label for="token"`) {
		t.Error("the token field has no <label> (a placeholder is not a label)")
	}
	if !strings.Contains(s, `aria-live="polite"`) {
		t.Error("no polite aria-live region for the SSE-driven updates")
	}
	// The polite summary must be a small status region, NOT aria-live on the job table.
	if regexp.MustCompile(`<t(able|body)[^>]*aria-live`).MatchString(s) {
		t.Error("aria-live is on the table itself — that spams a screen reader on every snapshot; announce a summary region instead")
	}
}

// TRANSCODE-15 (3): the silent row caps are surfaced and Pause is disabled once paused.
func TestInteraction_CapsSurfacedAndPauseDisabled(t *testing.T) {
	s := string(indexHTML)
	if !strings.Contains(s, "this view is capped") {
		t.Error("the API row caps are not surfaced — a truncated view reads as complete")
	}
	if !strings.Contains(s, `$("pause").disabled = !!snap.paused`) {
		t.Error("Pause is not disabled when already paused")
	}
	if !strings.Contains(s, `id="filter"`) {
		t.Error("no filter control for the queue/history views")
	}
}

// --- in-flight legibility (S0030) --------------------------------------------

// TestQueue_RendersInStateElapsedDerivedFromTheWireTimestamp is AC1. The elapsed figure
// must be DERIVED from the transition timestamp on every tick, never accumulated in a
// counter here: a background tab's timers are throttled by policies with no normative
// guarantee, so a counter would drift while a recomputed value cannot. The proof is
// structural, because the render is client-side: the page must read updated_at into the
// row, take the server's own clock off the snapshot, recompute on a timer, and hold no
// running total of its own.
func TestQueue_RendersInStateElapsedDerivedFromTheWireTimestamp(t *testing.T) {
	s := string(indexHTML)
	for _, want := range []string{
		"j.updated_at", // the transition timestamp already on the wire is the basis
		"snap.now",     // ...read against the SERVER's clock, not the client's alone
		"clockOffset",  // ...so a skewed client clock cannot invent an age
		"refreshElapsed",
		"setInterval(refreshElapsed, 1000)", // refreshed with no page reload
		"dataset.since",                     // each row carries its own basis
		"<th>Elapsed</th>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("index.html missing the elapsed-from-timestamp element %q", want)
		}
	}
	// An accumulating counter is the specific thing AC1 forbids. `now - since` is the
	// only arithmetic that may produce the figure.
	if !strings.Contains(s, "now - since") {
		t.Error("the elapsed figure is not computed as (server now - transition timestamp)")
	}
	for _, banned := range []string{"elapsed++", "elapsed += ", "seconds++", "+= 1000"} {
		if strings.Contains(s, banned) {
			t.Errorf("index.html accumulates elapsed time (%q) instead of deriving it — a throttled tab would drift", banned)
		}
	}
}

// TestQueue_RendersProgressAndShowsUnknownAsUnknown is AC3's page half: a running encode
// shows a figure taken from the encoder's own stream, and an absent one reads "unknown"
// — never a stale figure, never an interpolated one, never a zero.
func TestQueue_RendersProgressAndShowsUnknownAsUnknown(t *testing.T) {
	s := string(indexHTML)
	for _, want := range []string{
		"progress_fraction", "progress_seconds", "progress_duration_seconds",
		"<th>Progress</th>",
		`mk("span", "nr", "unknown")`, // the honest absent-figure node
		"never a stale figure",        // the copy says what the figure is and is not
		"encoder's own progress stream",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("index.html missing progress element %q", want)
		}
	}
	// The empty-queue rendering is unchanged in wording; only its column span moved with
	// the two new columns.
	if !strings.Contains(s, `"Nothing queued."`) {
		t.Error("the empty-queue rendering was changed")
	}
	if !strings.Contains(s, `emptyRow(5, "Nothing queued.")`) {
		t.Error("the empty-queue row does not span the queue table's columns")
	}
}

// TestQueue_ProgressIsShownForARunningEncodeOnly is the page half of the rule the hub
// enforces on the wire. A progress figure is a measurement taken BY the encoder against
// the source duration, so it exists while the encoder runs and at no other time, and the
// spec puts the rest of the pipeline outside it in terms:
//
//	Out (explicit):
//	- A progress percentage for the verify/VMAF phase. `internal/vmaf` keeps its
//	  current invocation; the verifying state is covered by elapsed alone (AC1).
//
// Gating the cell on the whole active set is what put the finished encode's last
// percentage on a verifying row and left it frozen there for the whole verify phase —
// which on a feature-length source is the longest stretch an operator watches.
func TestQueue_ProgressIsShownForARunningEncodeOnly(t *testing.T) {
	s := string(indexHTML)
	if !strings.Contains(s, `const PROGRESS_STATUS = "encoding";`) {
		t.Error("the page does not name the one state a progress figure can exist in")
	}
	if !strings.Contains(s, `if (j.status !== PROGRESS_STATUS) return;`) {
		t.Error("the progress cell is not restricted to a running encode")
	}
	if strings.Contains(s, "ACTIVE_STATUSES") {
		t.Error("the progress cell is gated on the whole active set again — a verifying row would carry the finished encode's figure")
	}
	// And the copy says so, because the operator reading an empty Progress cell on a
	// verifying row is owed the reason.
	if !strings.Contains(s, "covered by\n      Elapsed alone") && !strings.Contains(s, "covered by Elapsed alone") {
		t.Error("the queue note does not say that a probed/verified file is covered by Elapsed alone")
	}
}

// TestQueue_ProgressAddsNoHTMLSinkAndNoExternalAsset is AC11. The new cells are built
// with the same DOM-node idiom as every other row (TestRenderIdiom already forbids the
// string sinks page-wide), the CSP is unchanged, and the page still fetches nothing from
// outside the binary that served it.
func TestQueue_ProgressAddsNoHTMLSinkAndNoExternalAsset(t *testing.T) {
	s := string(indexHTML)
	// No off-binary fetch: no absolute URL, no protocol-relative one, and the only
	// network call is to this server's own API.
	for _, banned := range []string{"http://", "https://", "//cdn", "src=\"//", "@import", "url(http"} {
		if strings.Contains(s, banned) {
			t.Errorf("index.html would fetch %q from outside the binary that served it", banned)
		}
	}
	// The CSP is exactly the one that was already enforced, Trusted Types included.
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	const want = "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; require-trusted-types-for 'script'"
	if got := rec.Header().Get("Content-Security-Policy"); got != want {
		t.Errorf("the response CSP changed.\n got: %q\nwant: %q", got, want)
	}
	// The progress cell is assembled from nodes and text, like every other cell.
	if !strings.Contains(s, "function progressCell(td, j)") {
		t.Fatal("no progressCell renderer")
	}
	if !strings.Contains(s, "td.textContent = fmtSpan(") {
		t.Error("the elapsed cell is not filled with textContent")
	}
}

// --- DASH-7: the whole-ledger aggregates on the page -------------------------

// The page renders each published aggregate with the set it covers and the rows that
// were excluded for want of a recorded value. Rendering is client-side, so these are
// properties of the page SOURCE: the keys it reads, the statements it draws.
func TestDashboard_RendersTheWholeLedgerAggregatesWithTheirCoverage(t *testing.T) {
	s := string(indexHTML)
	for _, want := range []string{
		// every published figure is read by name
		`"outcomes"`, `"skips_by_guard"`, `"size_ratio"`, `"encode_ms"`, `"vmaf_mean"`, `"vmaf_min"`,
		// and each is drawn with its coverage, its window when bounded, and its exclusions
		`"over " + (a.covers`, "a.window", "excluded: no recorded value",
		"Across the whole ledger",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("index.html does not render the aggregates properly: missing %q", want)
		}
	}
	// The figures must come from the snapshot's aggregates, not be recomputed in the
	// browser from the capped rows the tables were handed.
	if !strings.Contains(s, "renderAggregates(snap.aggregates)") {
		t.Error("the page does not render the server-computed aggregates from the snapshot")
	}
}

// An aggregate that could not be read is drawn AS unavailable, and the rest of the page
// still draws. Three independent guarantees, each asserted: the card checks its own
// `available` flag, each card is built inside its own try/catch so one throwing figure
// cannot take the others, and the whole aggregate render happens AFTER the queue and
// history rows are already on the page.
func TestDashboard_AnUnavailableAggregateStillLeavesThePageRendering(t *testing.T) {
	s := string(indexHTML)
	if !strings.Contains(s, "a.available !== true") {
		t.Error("a card does not check whether its figure is available, so an unreadable figure would render as data")
	}
	if !strings.Contains(s, `v.appendChild(mk("span", "nr", "unavailable"))`) {
		t.Error("an unavailable figure is not drawn as unavailable")
	}
	if !strings.Contains(s, "card = aggCard(null, title, nodes);") {
		t.Error("cards are not built independently - one figure that throws would take the others with it")
	}
	hist := strings.Index(s, "hbody.appendChild(histRow(j))")
	agg := strings.Index(s, "renderAggregates(snap.aggregates)")
	if hist < 0 || agg < 0 || agg < hist {
		t.Errorf("the aggregates render before the tables (history at %d, aggregates at %d) - an aggregate failure could then cost the rows", hist, agg)
	}
	if !strings.Contains(s, "try { renderAggregates(snap.aggregates); } catch (_) {}") {
		t.Error("the aggregate render is not guarded, so a throw inside it would abort the rest of render()")
	}
	// A figure with nothing recorded reads as "not recorded", never as 0.
	if !strings.Contains(s, "if (counted === 0) {") || !strings.Contains(s, "v.appendChild(nrNode());") {
		t.Error("an aggregate with no contributing row is not rendered as 'not recorded'")
	}
}

// The response the page is served with is unchanged by this phase: the same
// Content-Security-Policy, byte for byte, and Trusted Types still required. Asserted
// against the literal policy rather than a substring, because a widened CSP is exactly
// the kind of change that passes a "contains" check.
func TestCSP_ByteForByteUnchangedAndStillEnforcesTrustedTypes(t *testing.T) {
	const want = "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; " +
		"connect-src 'self'; require-trusted-types-for 'script'"
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Header().Get("Content-Security-Policy"); got != want {
		t.Errorf("Content-Security-Policy changed\n got: %q\nwant: %q", got, want)
	}
}

// The page fetches nothing from outside the binary that served it. `default-src 'none'`
// would block it anyway, but a page that TRIES is a page that renders broken behind the
// policy, so the source itself must name no off-binary origin.
func TestPage_FetchesNothingFromOutsideTheBinaryThatServedIt(t *testing.T) {
	s := string(indexHTML)
	for _, banned := range []string{"http://", "https://", "//cdn", "@import", "<script src", "<link rel"} {
		if strings.Contains(s, banned) {
			t.Errorf("index.html reaches outside the binary: %q", banned)
		}
	}
	// The only origin the page talks to is its own server.
	for _, want := range []string{`new EventSource("/api/events")`} {
		if !strings.Contains(s, want) {
			t.Errorf("index.html no longer talks to its own API: missing %q", want)
		}
	}
}

func TestHandler404sOtherPaths(t *testing.T) {
	h := Handler()
	req := httptest.NewRequest(http.MethodGet, "/nope.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /nope.js: code %d, want 404", rec.Code)
	}
}

// =============================================================================
// S0035: the token system, the two themes, the layout floors and the states.
//
// Everything below grades a PROPERTY of the embedded page, read out of the page's
// own stylesheet. The render is client-side and CI has no browser, so the proof is
// structural — the idiom this file already uses — but it is DERIVED, not
// enumerated. The contrast proof measures the pairs the RULES paint, and a rule
// that paints text without saying which surface it sits on FAILS here rather than
// going unmeasured. That is the property that stops this decaying into a
// hand-written list a later change walks straight past: a new coloured rule is
// either resolvable and therefore measured, or it reds until it says where it
// paints.
// =============================================================================

type cssDecl struct{ prop, value string }

type cssRule struct {
	at    string // the at-rule prelude this rule sits under ("" at the top level)
	sel   string
	decls []cssDecl
}

// get returns the LAST declaration of prop in the rule, which is the one the
// cascade keeps.
func (r cssRule) get(prop string) string {
	v := ""
	for _, d := range r.decls {
		if d.prop == prop {
			v = d.value
		}
	}
	return v
}

// styleBlock is the contents of the page's single <style> element.
func styleBlock(t *testing.T) string {
	t.Helper()
	s := string(indexHTML)
	i := strings.Index(s, "<style>")
	j := strings.Index(s, "</style>")
	if i < 0 || j < 0 || j < i {
		t.Fatal("index.html has no <style> block")
	}
	return s[i+len("<style>") : j]
}

var (
	cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	whitespace = regexp.MustCompile(`\s+`)
)

func normSpace(s string) string { return strings.TrimSpace(whitespace.ReplaceAllString(s, " ")) }

// parseCSS is a small, deliberately literal CSS reader: enough to see rules,
// selectors, at-rule preludes and declarations, which is all any assertion here
// needs. It fails loudly on a shape it cannot read rather than silently skipping
// it, because a silently skipped rule is an unmeasured one.
func parseCSS(t *testing.T) []cssRule {
	t.Helper()
	return parseCSSRules(t, cssComment.ReplaceAllString(styleBlock(t), " "), "")
}

func parseCSSRules(t *testing.T, css, at string) []cssRule {
	t.Helper()
	var out []cssRule
	for i := 0; i < len(css); {
		j := strings.IndexByte(css[i:], '{')
		if j < 0 {
			if strings.TrimSpace(css[i:]) != "" {
				t.Fatalf("CSS the parser could not read: %q", normSpace(css[i:]))
			}
			break
		}
		prelude := normSpace(css[i : i+j])
		depth, k := 1, i+j+1
		for ; k < len(css) && depth > 0; k++ {
			switch css[k] {
			case '{':
				depth++
			case '}':
				depth--
			}
		}
		if depth != 0 {
			t.Fatalf("unbalanced braces in CSS near %q", prelude)
		}
		body := css[i+j+1 : k-1]
		if strings.HasPrefix(prelude, "@") {
			out = append(out, parseCSSRules(t, body, prelude)...)
		} else {
			out = append(out, cssRule{at: at, sel: prelude, decls: parseDecls(body)})
		}
		i = k
	}
	return out
}

func parseDecls(body string) []cssDecl {
	var out []cssDecl
	depth, start := 0, 0
	flush := func(end int) {
		part := strings.TrimSpace(body[start:end])
		start = end + 1
		c := strings.IndexByte(part, ':')
		if part == "" || c < 0 {
			return
		}
		out = append(out, cssDecl{
			prop:  strings.ToLower(strings.TrimSpace(part[:c])),
			value: normSpace(part[c+1:]),
		})
	}
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ';':
			if depth == 0 {
				flush(i)
			}
		}
	}
	flush(len(body))
	return out
}

var (
	varRefRe = regexp.MustCompile(`var\(\s*(--[a-zA-Z0-9_-]+)`)
	hex6     = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

func varRefs(value string) []string {
	var out []string
	for _, m := range varRefRe.FindAllStringSubmatch(value, -1) {
		out = append(out, m[1])
	}
	return out
}

// themeTokens returns the two token sets the page declares: the dark set (the
// default, in the top-level :root) and the light set (the same names, overridden
// under prefers-color-scheme: light, merged over the dark defaults).
func themeTokens(t *testing.T, rules []cssRule) (dark, light map[string]string) {
	t.Helper()
	dark, over := map[string]string{}, map[string]string{}
	for _, r := range rules {
		if r.sel != ":root" {
			continue
		}
		var target map[string]string
		switch {
		case r.at == "":
			target = dark
		case strings.Contains(r.at, "prefers-color-scheme") && strings.Contains(r.at, "light"):
			target = over
		default:
			continue
		}
		for _, d := range r.decls {
			if strings.HasPrefix(d.prop, "--") {
				target[d.prop] = d.value
			}
		}
	}
	if len(dark) == 0 {
		t.Fatal("no :root token block: the page declares no design tokens")
	}
	if len(over) == 0 {
		t.Fatal("no :root block under @media (prefers-color-scheme: light): the page has one theme")
	}
	light = map[string]string{}
	for k, v := range dark {
		light[k] = v
	}
	for k, v := range over {
		light[k] = v
	}
	return dark, light
}

// resolveToken follows a token through any var() indirection to its literal value.
func resolveToken(tokens map[string]string, name string) string {
	v := strings.TrimSpace(tokens[name])
	for i := 0; i < 8 && strings.HasPrefix(v, "var("); i++ {
		refs := varRefs(v)
		if len(refs) != 1 {
			return ""
		}
		v = strings.TrimSpace(tokens[refs[0]])
	}
	return v
}

// colourTokens returns the tokens a value references that resolve to a colour, so
// `outline:var(--focus-w) solid var(--focus)` yields --focus and not the width
// beside it.
func colourTokens(tokens map[string]string, value string) []string {
	var out []string
	for _, n := range varRefs(value) {
		if hex6.MatchString(resolveToken(tokens, n)) {
			out = append(out, n)
		}
	}
	return out
}

// ratio measures two tokens against each other in one theme.
func ratio(t *testing.T, tokens map[string]string, a, b string) float64 {
	t.Helper()
	ha, hb := resolveToken(tokens, a), resolveToken(tokens, b)
	if !hex6.MatchString(ha) || !hex6.MatchString(hb) {
		t.Fatalf("cannot measure %s (%q) against %s (%q): not both #rrggbb tokens", a, ha, b, hb)
	}
	return contrastRatio(t, ha, hb)
}

// pxOf reads a px length out of a token reference or a literal. Anything that is
// not a px length (a percentage, auto, a keyword) reads 0, which is never the
// unsafe direction for the two sweeps that use it.
func pxOf(tokens map[string]string, v string) float64 {
	v = strings.TrimSpace(v)
	if refs := varRefs(v); len(refs) == 1 {
		v = strings.TrimSpace(resolveToken(tokens, refs[0]))
	}
	if !strings.HasSuffix(v, "px") {
		return 0
	}
	f, err := strconv.ParseFloat(strings.TrimSuffix(v, "px"), 64)
	if err != nil {
		return 0
	}
	return f
}

// disabledOnly reports whether a selector applies ONLY to a disabled control, which
// WCAG 2.2 exempts from 1.4.3 and 1.4.11 alike. `:not(:disabled)` is the opposite
// statement and must not be caught by it.
func disabledOnly(sel string) bool {
	return strings.Contains(sel, ":disabled") && !strings.Contains(sel, ":not(:disabled)")
}

// --- AC1: every colour comes from a token ------------------------------------

// isColourProp names the properties whose value can carry a colour. Custom
// properties are included, so a literal cannot be smuggled into a token defined
// outside the :root blocks and then referenced as if it were one.
func isColourProp(p string) bool {
	if strings.HasPrefix(p, "--") {
		return true
	}
	switch p {
	case "color", "background", "background-color",
		"border", "border-color", "border-top", "border-right", "border-bottom", "border-left",
		"border-top-color", "border-right-color", "border-bottom-color", "border-left-color",
		"outline", "outline-color", "box-shadow", "text-shadow", "text-decoration",
		"text-decoration-color", "caret-color", "accent-color", "fill", "stroke",
		"column-rule", "column-rule-color", "scrollbar-color":
		return true
	}
	return false
}

var (
	hexLiteral   = regexp.MustCompile(`#[0-9a-fA-F]{3,8}`)
	colourFuncRe = regexp.MustCompile(`\b(rgba?|hsla?|hwb|lab|lch|oklab|oklch|color-mix)\s*\(`)
	numericValue = regexp.MustCompile(`^-?[0-9]*\.?[0-9]+(px|em|rem|ch|ex|%|vh|vw|ms|s|deg|fr)?$`)
	varCall      = regexp.MustCompile(`var\([^()]*\)`)
	percentage   = regexp.MustCompile(`^-?[0-9]*\.?[0-9]+%$`)
)

// nonColourKeywords are the words a colour-accepting declaration may legitimately
// carry that are not colours. Anything else left standing once the var()
// references are removed is a colour written at the point of use — which is a
// whitelist, so `color:rebeccapurple` reds exactly as `color:red` does.
var nonColourKeywords = map[string]bool{
	"solid": true, "dashed": true, "dotted": true, "double": true, "groove": true,
	"ridge": true, "inset": true, "outset": true, "none": true, "hidden": true,
	"thin": true, "medium": true, "thick": true, "inherit": true, "initial": true,
	"unset": true, "revert": true, "!important": true, "underline": true,
	"overline": true, "line-through": true, "auto": true,
}

func TestTokens_EveryColourIsDeclaredInATokenBlock(t *testing.T) {
	rules := parseCSS(t)
	checked := 0
	for _, r := range rules {
		if r.sel == ":root" {
			continue // the token block IS where the literals are allowed to live
		}
		for _, d := range r.decls {
			if hexLiteral.MatchString(d.value) {
				t.Errorf("%s { %s: %s } writes a colour literal at its point of use; declare it in :root and reference it as var(--...)", r.sel, d.prop, d.value)
			}
			if colourFuncRe.MatchString(d.value) {
				t.Errorf("%s { %s: %s } writes a colour function at its point of use; declare it in :root and reference it as var(--...)", r.sel, d.prop, d.value)
			}
			if !isColourProp(d.prop) {
				continue
			}
			checked++
			rest := varCall.ReplaceAllString(d.value, " ")
			for _, w := range strings.FieldsFunc(rest, func(c rune) bool {
				return c == ' ' || c == ',' || c == '(' || c == ')'
			}) {
				lw := strings.ToLower(w)
				if numericValue.MatchString(lw) || nonColourKeywords[lw] {
					continue
				}
				t.Errorf("%s { %s: %s } names the colour %q at its point of use; every colour must come from a token", r.sel, d.prop, d.value, w)
			}
		}
	}
	if checked == 0 {
		t.Fatal("the colour sweep examined no declaration - it is not reading the stylesheet")
	}
}

// --- AC2: type and space come from the declared scales ------------------------

var spaceProps = map[string]bool{
	"padding": true, "padding-top": true, "padding-right": true, "padding-bottom": true,
	"padding-left": true, "padding-inline": true, "padding-block": true,
	"margin": true, "margin-top": true, "margin-right": true, "margin-bottom": true,
	"margin-left": true, "margin-inline": true, "margin-block": true,
	"gap": true, "row-gap": true, "column-gap": true,
}

func TestTokens_TypeAndSpaceComeFromTheDeclaredScales(t *testing.T) {
	rules := parseCSS(t)
	dark, _ := themeTokens(t, rules)
	fs, sp := 0, 0
	for name, v := range dark {
		if strings.HasPrefix(name, "--fs-") && strings.HasSuffix(v, "px") {
			fs++
		}
		if strings.HasPrefix(name, "--sp-") && strings.HasSuffix(v, "px") {
			sp++
		}
	}
	if fs < 4 {
		t.Errorf("the --fs-* type scale has %d steps; that is not a scale", fs)
	}
	if sp < 4 {
		t.Errorf("the --sp-* space scale has %d steps; that is not a scale", sp)
	}

	seen := 0
	for _, r := range rules {
		if r.sel == ":root" {
			continue
		}
		for _, d := range r.decls {
			if d.prop == "font" {
				t.Errorf("%s { font: %s } - the font shorthand hides a font-size; declare font-size from the type scale", r.sel, d.value)
				continue
			}
			want := ""
			switch {
			case d.prop == "font-size":
				want = "--fs-"
			case spaceProps[d.prop]:
				want = "--sp-"
			default:
				continue
			}
			seen++
			for _, part := range strings.Fields(d.value) {
				if part == "0" || part == "auto" || percentage.MatchString(part) {
					continue
				}
				if refs := varRefs(part); len(refs) == 1 && strings.HasPrefix(refs[0], want) {
					continue
				}
				t.Errorf("%s { %s: %s } writes %q at its point of use; take it from a %s* token (only 0, auto and a percentage may be literal)",
					r.sel, d.prop, d.value, part, want)
			}
		}
	}
	if seen == 0 {
		t.Fatal("the scale sweep examined no declaration - it is not reading the stylesheet")
	}
}

// --- AC3 / AC4: every painted pair, measured, in both themes ------------------

type paintedPair struct {
	sel   string
	fg    string
	bg    string
	floor float64
	kind  string
}

// isLargeText applies WCAG 2.2's large-text exception, and only where the rule
// states its own size. A rule that does not state one is held to the 4.5:1 floor,
// which is never the unsafe direction.
func isLargeText(tokens map[string]string, r cssRule) bool {
	size := pxOf(tokens, r.get("font-size"))
	if size >= 24 {
		return true
	}
	weight, _ := strconv.Atoi(strings.TrimSpace(r.get("font-weight")))
	return size >= 18.66 && weight >= 700
}

// paintedTextPairs derives every foreground/background pair the stylesheet paints
// text with. A rule that sets a colour is measured against the surface it declares:
// its own background when it paints one, else the --paints-on token(s) it names. A
// rule that declares neither is a FAILURE rather than a skip, and a rule that paints
// a surface without a text colour on it is one too (unless it marks itself
// --decorative, the one exemption, for a fill that never holds text). Between them
// those two refusals are what make this a derivation instead of a list.
func paintedTextPairs(t *testing.T, rules []cssRule, tokens map[string]string) []paintedPair {
	t.Helper()
	var out []paintedPair
	for _, r := range rules {
		if r.sel == ":root" {
			continue
		}
		fg := colourTokens(tokens, r.get("color"))
		bgVal := r.get("background-color")
		if bgVal == "" {
			bgVal = r.get("background")
		}
		bg := colourTokens(tokens, bgVal)
		on := colourTokens(tokens, r.get("--paints-on"))

		if len(bg) > 0 && len(fg) == 0 && r.get("--decorative") == "" {
			t.Errorf("%s paints the surface %s but declares no text colour on it: give it one so the pair is measured, or mark the rule --decorative:1 if it never holds text",
				r.sel, bg[0])
		}
		if len(fg) == 0 || disabledOnly(r.sel) {
			continue
		}
		surfaces := on
		if len(bg) > 0 {
			surfaces = bg
		}
		if len(surfaces) == 0 {
			t.Errorf("%s declares color:%s but names no surface, so the pair it paints goes unmeasured - add --paints-on:var(--<surface token>)", r.sel, fg[0])
			continue
		}
		floor, kind := 4.5, "text"
		if isLargeText(tokens, r) {
			floor, kind = 3.0, "large-text"
		}
		for _, s := range surfaces {
			out = append(out, paintedPair{sel: r.sel, fg: fg[0], bg: s, floor: floor, kind: kind})
		}
	}
	return out
}

type theme struct {
	name   string
	tokens map[string]string
}

func themes(dark, light map[string]string) []theme {
	return []theme{{"dark", dark}, {"light", light}}
}

func TestContrast_EveryPaintedPairMeetsItsFloorInBothThemes(t *testing.T) {
	rules := parseCSS(t)
	dark, light := themeTokens(t, rules)
	pairs := paintedTextPairs(t, rules, dark)
	if len(pairs) < 20 {
		t.Fatalf("only %d painted pairs derived from the stylesheet - the derivation is not seeing the page", len(pairs))
	}
	for _, th := range themes(dark, light) {
		for _, p := range pairs {
			if got := ratio(t, th.tokens, p.fg, p.bg); got < p.floor {
				t.Errorf("%s theme: %s paints %s (%s) on %s (%s) at %.2f:1, under the %.1f:1 %s floor",
					th.name, p.sel, p.fg, resolveToken(th.tokens, p.fg), p.bg, resolveToken(th.tokens, p.bg),
					got, p.floor, p.kind)
			}
		}
	}
}

func TestThemes_ALightSetIsDeclaredAndColorSchemeFollowsIt(t *testing.T) {
	rules := parseCSS(t)
	dark, light := themeTokens(t, rules)
	var root cssRule
	for _, r := range rules {
		if r.sel == ":root" && r.at == "" {
			root = r
		}
	}
	if cs := root.get("color-scheme"); !strings.Contains(cs, "light") || !strings.Contains(cs, "dark") {
		t.Errorf("color-scheme is %q; it must declare `light dark` so native controls, scrollbars and form fields follow the page's theme instead of staying light inside a dark page", cs)
	}
	// The dark set is the DEFAULT, so a user agent expressing no preference still
	// gets the page holdfast ships today.
	if got := resolveToken(dark, "--bg"); got != "#0f1115" {
		t.Errorf("the default (no-preference) --bg is %q; the dark set the page ships must remain the default", got)
	}
	// The light set is a real second set, not a copy of the first.
	for _, name := range []string{"--bg", "--panel", "--fg", "--muted", "--accent", "--ok", "--warn", "--bad", "--border", "--focus"} {
		d, l := resolveToken(dark, name), resolveToken(light, name)
		if !hex6.MatchString(l) {
			t.Errorf("the light theme does not declare %s", name)
			continue
		}
		if d == l {
			t.Errorf("the light theme reuses the dark %s (%s) - that is not a light set", name, d)
		}
	}
	// ...and it is light: its page surface is lighter than its text, the opposite of
	// the dark set. (Contrast against black is monotonic in luminance.)
	lightBg := contrastRatio(t, resolveToken(light, "--bg"), "#000000")
	lightFg := contrastRatio(t, resolveToken(light, "--fg"), "#000000")
	if lightBg < lightFg {
		t.Error("the light theme's --bg is darker than its --fg; that is a second dark theme, not a light one")
	}
}

// --- AC3 (non-text) / AC7 / AC8: the controls ---------------------------------

type controlRule struct {
	sel    string
	fill   string
	edge   string
	behind string
}

var controlSel = regexp.MustCompile(`(^|[^a-zA-Z0-9_-])(button|input)([^a-zA-Z0-9_-]|$)`)

// controlRules derives the page's interactive controls and, for each, the fill it
// paints, the edge that draws it and the surface it sits on. A variant (:hover, a
// .primary modifier) inherits whatever it does not restate from its base rule, the
// way the cascade gives it to it.
func controlRules(t *testing.T, rules []cssRule, tokens map[string]string) []controlRule {
	t.Helper()
	base := map[string]cssRule{}
	for _, r := range rules {
		if r.at == "" && (r.sel == "button" || r.sel == ".controls input") {
			base[r.sel] = r
		}
	}
	if len(base) != 2 {
		t.Fatalf("expected base rules for `button` and `.controls input`; found %d", len(base))
	}
	first := func(vals ...string) []string {
		for _, v := range vals {
			if c := colourTokens(tokens, v); len(c) > 0 {
				return c
			}
		}
		return nil
	}
	var out []controlRule
	for _, r := range rules {
		if !controlSel.MatchString(r.sel) || disabledOnly(r.sel) || strings.Contains(r.sel, "::") {
			continue
		}
		b := base["button"]
		if strings.Contains(r.sel, "input") {
			b = base[".controls input"]
		}
		fill := first(r.get("background-color"), r.get("background"), b.get("background-color"), b.get("background"))
		edge := first(r.get("border-color"), r.get("border"), b.get("border-color"), b.get("border"))
		behind := first(r.get("--paints-on"), b.get("--paints-on"))
		if len(fill) == 0 || len(edge) == 0 || len(behind) == 0 {
			continue
		}
		out = append(out, controlRule{sel: r.sel, fill: fill[0], edge: edge[0], behind: behind[0]})
	}
	return out
}

func TestControls_AreIdentifiableAgainstTheSurfaceTheySitOn(t *testing.T) {
	rules := parseCSS(t)
	dark, light := themeTokens(t, rules)
	ctrls := controlRules(t, rules, dark)
	if len(ctrls) < 4 {
		t.Fatalf("only %d control rules derived; the derivation is not seeing the page's controls", len(ctrls))
	}
	for _, th := range themes(dark, light) {
		for _, c := range ctrls {
			edge := ratio(t, th.tokens, c.edge, c.behind)
			fill := ratio(t, th.tokens, c.fill, c.behind)
			if math.Max(edge, fill) < 3.0 {
				t.Errorf("%s theme: the control %s reads %.2f:1 by its edge (%s) and %.2f:1 by its fill (%s) against %s - under the 3:1 non-text floor, so nothing identifies it as a control",
					th.name, c.sel, edge, c.edge, fill, c.fill, c.behind)
			}
		}
	}
}

func TestFocus_RingClearsBothTheControlFillAndTheSurfaceBehindIt(t *testing.T) {
	rules := parseCSS(t)
	dark, light := themeTokens(t, rules)
	focus := ""
	for _, r := range rules {
		if !strings.Contains(r.sel, ":focus-visible") {
			continue
		}
		v := r.get("outline-color")
		if v == "" {
			v = r.get("outline")
		}
		if c := colourTokens(dark, v); len(c) > 0 {
			focus = c[0]
		}
	}
	if focus == "" {
		t.Fatal("no :focus-visible rule draws an outline from a colour token")
	}
	ctrls := controlRules(t, rules, dark)
	if len(ctrls) < 4 {
		t.Fatalf("only %d control rules derived; the focus proof would cover almost nothing", len(ctrls))
	}
	for _, th := range themes(dark, light) {
		for _, c := range ctrls {
			if got := ratio(t, th.tokens, focus, c.fill); got < 3.0 {
				t.Errorf("%s theme: the focus ring %s is %.2f:1 on %s's own fill %s - a ring you cannot see on the control it is marking is not an indicator",
					th.name, focus, got, c.sel, c.fill)
			}
			if got := ratio(t, th.tokens, focus, c.behind); got < 3.0 {
				t.Errorf("%s theme: the focus ring %s is %.2f:1 on the surface behind %s (%s)",
					th.name, focus, got, c.sel, c.behind)
			}
		}
	}
	// Keyboard focus shows it; a mouse click does not.
	s := string(indexHTML)
	if strings.Contains(s, ":focus {") || strings.Contains(s, ":focus{") {
		t.Error("a bare :focus rule would show the ring on a mouse click; the ring is :focus-visible only")
	}
	if !strings.Contains(s, "outline-offset:var(--focus-offset)") {
		t.Error("the ring has no offset, so the gap that puts it against the surface behind the control is gone")
	}
}

func controlRowSpans(t *testing.T, s string) [][2]int {
	t.Helper()
	var spans [][2]int
	const open = `<div class="controls">`
	for i := 0; ; {
		k := strings.Index(s[i:], open)
		if k < 0 {
			break
		}
		start := i + k
		e := strings.Index(s[start:], "</div>")
		if e < 0 {
			t.Fatal("a .controls row is never closed")
		}
		spans = append(spans, [2]int{start, start + e})
		i = start + e
	}
	if len(spans) != 2 {
		t.Fatalf("expected the page's two .controls rows, found %d", len(spans))
	}
	return spans
}

func inAnySpan(spans [][2]int, at int) bool {
	for _, s := range spans {
		if at >= s[0] && at < s[1] {
			return true
		}
	}
	return false
}

func TestTargets_EveryPointerTargetClearsTheTwentyFourPixelFloor(t *testing.T) {
	rules := parseCSS(t)
	dark, _ := themeTokens(t, rules)
	if min := pxOf(dark, "var(--target-min)"); min < 24 {
		t.Errorf("--target-min is %gpx, under WCAG 2.2 2.5.8's 24px floor", min)
	}
	for _, sel := range []string{"button", ".controls input"} {
		var r cssRule
		found := false
		for _, c := range rules {
			if c.at == "" && c.sel == sel {
				r, found = c, true
			}
		}
		if !found {
			t.Fatalf("no base rule for %q", sel)
		}
		for _, prop := range []string{"min-height", "min-width"} {
			if px := pxOf(dark, r.get(prop)); px < 24 {
				t.Errorf("%s { %s: %s } holds the target at %gpx in that dimension, under the 24px floor", sel, prop, r.get(prop), px)
			}
		}
	}
	// The pointer targets ARE every button and input in the two control rows. One
	// added anywhere else would not be covered by the two rules above, so it reds.
	s := string(indexHTML)
	spans := controlRowSpans(t, s)
	counts := map[string]int{}
	for _, tag := range []string{"<button", "<input"} {
		for i := 0; ; {
			k := strings.Index(s[i:], tag)
			if k < 0 {
				break
			}
			at := i + k
			i = at + len(tag)
			counts[tag]++
			if !inAnySpan(spans, at) {
				t.Errorf("a %s sits outside the .controls rows at offset %d, so no rule holds it at the 24px target floor", tag, at)
			}
		}
	}
	if counts["<button"] < 3 || counts["<input"] < 2 {
		t.Fatalf("found %d buttons and %d inputs; the control set changed and this proof no longer covers it", counts["<button"], counts["<input"])
	}
}

// --- AC5: motion ---------------------------------------------------------------

func msOf(v string) float64 {
	v = strings.TrimSpace(strings.ReplaceAll(v, "!important", ""))
	switch {
	case strings.HasSuffix(v, "ms"):
		f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(v, "ms")), 64)
		if err != nil {
			return math.Inf(1)
		}
		return f
	case strings.HasSuffix(v, "s"):
		f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(v, "s")), 64)
		if err != nil {
			return math.Inf(1)
		}
		return f * 1000
	}
	return math.Inf(1)
}

func TestMotion_NoStateDependsOnItAndAReducePreferenceStopsIt(t *testing.T) {
	// Comments stripped: a comment saying the page declares no keyframes is not a
	// keyframes block.
	if strings.Contains(cssComment.ReplaceAllString(styleBlock(t), " "), "@keyframes") {
		t.Error("the page declares @keyframes; a state that arrives by animation is not readable from a static frame")
	}
	rules := parseCSS(t)
	moved := 0
	for _, r := range rules {
		for _, d := range r.decls {
			if strings.HasPrefix(d.prop, "animation") {
				t.Errorf("%s { %s: %s } animates; every state on this page must be readable from a static frame", r.sel, d.prop, d.value)
			}
			if strings.HasPrefix(d.prop, "transition") && !strings.Contains(r.at, "prefers-reduced-motion") {
				moved++
			}
		}
	}
	if moved == 0 {
		return // the page declares no motion at all: the criterion is met with no media block
	}
	for _, r := range rules {
		if !strings.Contains(r.at, "prefers-reduced-motion") || !strings.Contains(r.at, "reduce") {
			continue
		}
		if !strings.Contains(r.sel, "*") {
			continue
		}
		v := r.get("transition-duration")
		if strings.Contains(v, "!important") && msOf(v) <= 1 {
			return
		}
	}
	t.Errorf("the page declares %d transitions but no @media (prefers-reduced-motion: reduce) rule cuts every transition-duration to no perceptible motion", moved)
}

// --- AC6: the narrow viewport --------------------------------------------------

var maxWidthRe = regexp.MustCompile(`max-width\s*:\s*([0-9.]+)px`)

func TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody(t *testing.T) {
	rules := parseCSS(t)
	dark, _ := themeTokens(t, rules)
	narrow := pxOf(dark, "var(--bp-narrow)")
	if narrow < 360 {
		t.Fatalf("--bp-narrow is %gpx; the criterion is written at a 360px viewport", narrow)
	}
	want := map[string][2]string{
		"header":    {"flex-direction", "column"},
		".controls": {"flex-direction", "column"},
		".aggs":     {"grid-template-columns", "1fr"},
	}
	for _, r := range rules {
		if !strings.Contains(r.at, "@media") {
			continue
		}
		m := maxWidthRe.FindStringSubmatch(r.at)
		if m == nil {
			continue
		}
		bp, err := strconv.ParseFloat(m[1], 64)
		if err != nil || bp < narrow {
			continue // this block does not apply at the narrow viewport
		}
		if w, ok := want[r.sel]; ok && strings.Contains(r.get(w[0]), w[1]) {
			delete(want, r.sel)
		}
	}
	for _, sel := range []string{"header", ".controls", ".aggs"} {
		if w, ok := want[sel]; ok {
			t.Errorf("at a %gpx viewport, %s is not laid out in a single column (expected %s: %s under a max-width media block)", narrow, sel, w[0], w[1])
		}
	}
	// Nothing outside a .tablewrap may assert a width or a minimum width wider than
	// the narrow viewport, or the page BODY scrolls sideways. The two data tables
	// keep scrolling inside their own .tablewrap, which is how they behave today.
	for _, r := range rules {
		if strings.Contains(r.sel, "tablewrap") {
			continue
		}
		for _, prop := range []string{"width", "min-width"} {
			v := r.get(prop)
			if v == "" {
				continue
			}
			if px := pxOf(dark, v); px > narrow {
				t.Errorf("%s { %s: %s } is wider than the %gpx narrow viewport, so the page body itself scrolls sideways", r.sel, prop, v, narrow)
			}
		}
	}
}

// --- AC9: a filter term that matches nothing -----------------------------------

func TestFilter_ANonMatchingTermSaysSoAndSaysTheLoadedRowsAreCapped(t *testing.T) {
	s := string(indexHTML)
	for _, want := range []string{
		"const NO_MATCH_TEXT",
		"No loaded row matches this filter.",
		"themselves capped",
		"function setNoMatchRow(body, want)",
		`body.querySelector("tr[data-nomatch]")`,
		`tr.dataset.nomatch = "1"`,
		"existing.remove()",
		`mk("td", "empty", NO_MATCH_TEXT)`,
		`table.querySelectorAll("thead th").length`,
		`setNoMatchRow(body, term !== "" && loaded > 0 && shown === 0)`,
		"if (!hide) shown++;",
		`const hide = term !== "" && !(tr.dataset.path || "").includes(term);`,
		"tr.hidden = hide;",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the filter's no-match state is missing %q", want)
		}
	}
	// The message is a row in the table's own body, built as DOM nodes like every
	// other row (TestRenderIdiom forbids the HTML string sinks page-wide).
	if !strings.Contains(s, "const tr = document.createElement(\"tr\");\n  tr.dataset.nomatch = \"1\";") {
		t.Error("the no-match row is not built with createElement")
	}
	// It has to be reachable: applyFilter is what the input event and every render
	// call, and the term is read from the filter box.
	for _, want := range []string{
		`$("filter").addEventListener("input", applyFilter)`,
		"  applyFilter();",
		`const term = $("filter").value.trim().toLowerCase();`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("applyFilter is no longer wired up: missing %q", want)
		}
	}
}

// --- AC10: the degraded-state copy, verbatim and legible -----------------------

func TestDegradedStates_CopySurvivesVerbatimAndStaysLegibleInBothThemes(t *testing.T) {
	s := string(indexHTML)
	for _, want := range []string{
		"Nothing queued.",
		"No history yet.",
		"unavailable",
		"not recorded",
		"unknown",
		"this view is capped",
		"reconnecting…",
		"control disabled — set server_auth_token on the server.",
		"unauthorized — check the control token.",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the degraded-state wording %q no longer appears on the page", want)
		}
	}
	rules := parseCSS(t)
	dark, light := themeTokens(t, rules)
	pairs := paintedTextPairs(t, rules, dark)
	// Each degraded state names the rule that paints it. Every one of those rules
	// must be among the pairs the contrast derivation SAW, and must clear the text
	// floor in both themes - a degraded state nobody can read is a degraded state
	// that is not reported.
	states := []struct{ state, sel string }{
		{"empty queue / empty history", ".empty"},
		{"not recorded / unknown / unavailable", ".nr"},
		{"the row cap notice", ".note.cap"},
		{"the reconnecting connection state", "#conn.down"},
		{"a 401 / 403 refusal", "#msg.err"},
	}
	for _, st := range states {
		found := false
		for _, p := range pairs {
			if p.sel != st.sel {
				continue
			}
			found = true
			for _, th := range themes(dark, light) {
				if got := ratio(t, th.tokens, p.fg, p.bg); got < 4.5 {
					t.Errorf("%s theme: %s (%s on %s) is %.2f:1, under the 4.5:1 text floor", th.name, st.state, p.fg, p.bg, got)
				}
			}
		}
		if !found {
			t.Errorf("%s is painted by %s, which the contrast derivation never saw - that state is unmeasured", st.state, st.sel)
		}
	}
}
