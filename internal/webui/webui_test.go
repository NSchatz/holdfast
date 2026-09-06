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
// structural, the idiom this file already uses, but it is DERIVED, not
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

// styleBlock is the contents of the page's single <style> element. "Single" is
// ASSERTED here rather than assumed: every CSS sweep in this file is built on this
// function and reads the first <style> element only, so a second stylesheet would
// carry declarations no criterion ever looks at. That is the same hole the inline
// style= attributes opened one element lower down (impl-gate finding F13), and it
// is closed in the same place - at the reader, once, for every sweep.
func styleBlock(t *testing.T) string {
	t.Helper()
	s := string(indexHTML)
	if n := strings.Count(s, "<style"); n != 1 {
		t.Fatalf("the page carries %d <style elements; every sweep in this file reads one of them, so the rest would go unswept", n)
	}
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
	varRefRe   = regexp.MustCompile(`var\(\s*(--[a-zA-Z0-9_-]+)`)
	hex6       = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	soleVarRef = regexp.MustCompile(`^var\(\s*(--[a-zA-Z0-9_-]+)\s*\)$`)
)

func varRefs(value string) []string {
	var out []string
	for _, m := range varRefRe.FindAllStringSubmatch(value, -1) {
		out = append(out, m[1])
	}
	return out
}

// canonicalAt is an at-rule prelude with every space removed, so a media query
// can be compared for what it IS rather than for the words it happens to contain.
// Matching a prelude with strings.Contains is the F12 defect in another position:
// `@media (prefers-color-scheme: light) and (min-width: 99999px)` contains both
// words, reads as the live light theme, and applies to nothing.
func canonicalAt(at string) string { return strings.ReplaceAll(normSpace(at), " ", "") }

const (
	lightAt  = "@media(prefers-color-scheme:light)"
	reduceAt = "@media(prefers-reduced-motion:reduce)"
)

// --- reading a NAME for the property it IS -------------------------------------

// vendorPrefix is a `-webkit-` / `-moz-` / `-ms-` / `-o-` prefix on a property
// name. A CUSTOM property is deliberately not one: `--paints-on` has a hyphen
// where this pattern needs a letter, so it is never stripped.
var vendorPrefix = regexp.MustCompile(`^-[a-zA-Z]+-`)

// unprefixed reads a property name for the property it IS rather than for the
// spelling it happens to carry. `strings.HasPrefix(prop, "animation")` is a
// membership narrower than AC5's own set - "any transition, animation or
// @keyframes" - by exactly the vendor prefixes WebKit and Blink both still
// honour, so a page whose motion was written `-webkit-animation` declared no
// motion at all to that sweep and it took the criterion's no-motion exit
// (impl-gate finding F18). Every property-NAME lookup in this file goes through
// this reader, for the same reason canonicalAt exists for a prelude.
func unprefixed(prop string) string {
	return vendorPrefix.ReplaceAllString(strings.ToLower(strings.TrimSpace(prop)), "")
}

// cssPx reads a length written as a plain px literal. It is the one place a
// number is taken out of a CSS value, so measuredPx (which resolves a token
// first) and the media-query reader (which has no tokens to resolve) agree about
// what counts as a measurable length.
func cssPx(v string) (float64, bool) {
	v = strings.TrimSpace(v)
	if !strings.HasSuffix(v, "px") {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(v, "px")), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// --- reading an AT-RULE PRELUDE for the condition it IS ------------------------
//
// canonicalAt above compares a prelude for EQUALITY, which is how AC4's light
// block and AC5's reduce block refuse a second condition that never holds. Two
// sweeps were never given that reader and asked something looser instead: AC6's
// narrow block was matched with `strings.Contains(r.at, "@media")` plus a
// max-width regex, so `@media (max-width: 640px) and (min-width: 99999px)` read
// as the narrow block and applied at no viewport (impl-gate finding F14); AC8's
// coverage loop asked nothing at all about the at-rule, so the page's one
// :focus-visible rule could be wrapped in any media query and every control still
// counted as covered (impl-gate finding F15). Both now read the prelude here.
//
// An equality test is not enough for either of them, because AC6's block is
// legitimately conditional (it is a breakpoint) and AC8's must legitimately be
// unconditional - so the prelude is PARSED and then evaluated, at a viewport for
// AC6 and against every user for AC8.

// mediaCond is one condition of a media query: a media type (`screen`), or a
// parenthesised feature test (`(max-width: 640px)`).
type mediaCond struct {
	typ   string // the media type, lowercased; "" for a feature test
	name  string // the feature name, lowercased; "" for a media type
	value string // the feature's value, lowercased and space-free
}

// mediaTokens splits one media query into its top-level tokens: each
// parenthesised group is one token and each bare word is one.
func mediaTokens(q string) ([]string, bool) {
	var out []string
	for i := 0; i < len(q); {
		switch {
		case q[i] == ' ':
			i++
		case q[i] == '(':
			depth, j := 0, i
			for ; j < len(q); j++ {
				if q[j] == '(' {
					depth++
				}
				if q[j] == ')' {
					depth--
					if depth == 0 {
						j++
						break
					}
				}
			}
			if depth != 0 {
				return nil, false
			}
			out = append(out, strings.TrimSpace(q[i:j]))
			i = j
		default:
			j := i
			for j < len(q) && q[j] != ' ' && q[j] != '(' {
				j++
			}
			out = append(out, q[i:j])
			i = j
		}
	}
	return out, true
}

// mediaQueries splits an @media prelude into its comma-separated queries, each a
// conjunction of conditions. ok is false when the prelude is not an @media rule,
// or carries a shape this reader does not model - a `not`, an `or`, a boolean
// feature, the range syntax, an unbalanced parenthesis - in which case NO caller
// may claim to know when the rule applies.
func mediaQueries(at string) ([][]mediaCond, bool) {
	s := strings.TrimSpace(normSpace(at))
	if !strings.HasPrefix(strings.ToLower(s), "@media") {
		return nil, false
	}
	s = strings.TrimSpace(s[len("@media"):])
	if s == "" {
		return nil, false
	}
	var out [][]mediaCond
	for _, q := range strings.Split(s, ",") {
		toks, ok := mediaTokens(strings.TrimSpace(q))
		if !ok {
			return nil, false
		}
		var conds []mediaCond
		for _, tok := range toks {
			lt := strings.ToLower(tok)
			switch lt {
			case "":
				continue
			case "and", "only":
				continue
			case "not", "or":
				return nil, false
			}
			if strings.HasPrefix(tok, "(") {
				if !strings.HasSuffix(tok, ")") {
					return nil, false
				}
				inner := strings.TrimSpace(tok[1 : len(tok)-1])
				c := strings.IndexByte(inner, ':')
				if c < 0 {
					return nil, false // a boolean feature, or the range syntax
				}
				conds = append(conds, mediaCond{
					name:  strings.ToLower(strings.TrimSpace(inner[:c])),
					value: strings.ToLower(strings.ReplaceAll(strings.TrimSpace(inner[c+1:]), " ", "")),
				})
				continue
			}
			if strings.ContainsAny(tok, "()<>=") {
				return nil, false
			}
			conds = append(conds, mediaCond{typ: lt})
		}
		if len(conds) == 0 {
			return nil, false
		}
		out = append(out, conds)
	}
	return out, true
}

// holdsAtWidth reports whether one condition holds on a screen px CSS pixels
// wide whose user has expressed no preference of any kind. A condition this
// reader cannot evaluate does NOT hold: for AC6 that means a block it cannot
// read never counts as the narrow block, which is the strict direction and the
// one that reds.
func (c mediaCond) holdsAtWidth(px float64) bool {
	if c.typ != "" {
		return c.typ == "all" || c.typ == "screen"
	}
	n, ok := cssPx(c.value)
	if !ok {
		return false
	}
	switch c.name {
	case "width":
		return n == px
	case "min-width":
		return px >= n
	case "max-width":
		return px <= n
	}
	return false
}

// atAppliesAtViewportWidth reports whether a rule under this prelude is live at a
// viewport px CSS pixels wide, for a user expressing no preference. The top level
// applies at every viewport; an @media prelude applies when one of its queries
// does; a prelude this reader cannot read does not, because a sweep that assumed
// otherwise is exactly how a block matching no viewport read as the narrow one.
func atAppliesAtViewportWidth(at string, px float64) bool {
	if strings.TrimSpace(at) == "" {
		return true
	}
	queries, ok := mediaQueries(at)
	if !ok {
		return false
	}
	for _, conds := range queries {
		live := true
		for _, c := range conds {
			if !c.holdsAtWidth(px) {
				live = false
				break
			}
		}
		if live {
			return true
		}
	}
	return false
}

// atDecidableAtViewportWidth reports whether this reader can DECIDE, at all,
// whether a rule under this prelude is live at a given viewport.
// atAppliesAtViewportWidth answers false both for a prelude that provably does not
// apply and for one it cannot read, and those are not the same answer where the
// question is which rule DECIDES a layout: dropping a rule the reader cannot
// evaluate is how an override survives a sweep, which is finding F21's shape
// spelled in the prelude instead of the selector. `@media (max-width: 40em)` is
// live at 360px in a real browser and this reader will not say so;
// `@supports (display: grid)` may hold for everyone it reaches;
// `@media (prefers-color-scheme: light)` holds for half the users the criterion is
// written about. A rule declaring one of AC6's regions under any of them is RED,
// not absent - resolvable or red, as the width sweep beside it already is.
func atDecidableAtViewportWidth(at string) bool {
	if strings.TrimSpace(at) == "" {
		return true
	}
	queries, ok := mediaQueries(at)
	if !ok {
		return false
	}
	for _, conds := range queries {
		for _, c := range conds {
			if c.typ != "" {
				continue // a media type: holdsAtWidth models every one of them
			}
			switch c.name {
			case "width", "min-width", "max-width":
			default:
				return false
			}
			if _, okPx := cssPx(c.value); !okPx {
				return false
			}
		}
	}
	return true
}

// atAppliesToEveryUser reports whether a rule under this prelude is live for
// EVERY user agent: no viewport condition, no user preference, no media type
// narrower than `all`. AC8 says the ring is drawn "in both themes"; a ring
// declared under any condition at all leaves some keyboard operator with none,
// which is the plainest violation that criterion has - not a poor ring, no ring.
func atAppliesToEveryUser(at string) bool {
	if strings.TrimSpace(at) == "" {
		return true
	}
	queries, ok := mediaQueries(at)
	if !ok {
		return false
	}
	for _, conds := range queries {
		if len(conds) == 1 && conds[0].typ == "all" {
			return true
		}
	}
	return false
}

// --- reading a declared VALUE, rather than searching it -------------------------

// topLevelFields splits a value into its space-separated parts, treating a
// parenthesised group as ONE part. `repeat(3, 1fr)` is one part, not two.
func topLevelFields(v string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ' ':
			if depth == 0 {
				if f := strings.TrimSpace(v[start:i]); f != "" {
					out = append(out, f)
				}
				start = i + 1
			}
		}
	}
	if f := strings.TrimSpace(v[start:]); f != "" {
		out = append(out, f)
	}
	return out
}

// gridTrackCount counts the columns a grid-template-columns value declares, and
// reports ok=false when the count is not decidable from the source (auto-fit,
// auto-fill, a shape this reader does not model). Searching the value for the
// wanted one instead is how `repeat(3, 1fr)` satisfied a single-column check, by
// CONTAINING "1fr" at the very viewport the criterion says must show one column
// (impl-gate finding F14).
func gridTrackCount(v string) (int, bool) {
	v = strings.TrimSpace(strings.ReplaceAll(normSpace(v), "!important", ""))
	if v == "" {
		return 0, false
	}
	if strings.EqualFold(v, "none") {
		// No explicit track list: with the default row auto-flow every item is
		// placed in one implicit column.
		return 1, true
	}
	n := 0
	for _, f := range topLevelFields(v) {
		lf := strings.ToLower(f)
		switch {
		case strings.HasPrefix(lf, "["): // a line name, not a track
			continue
		case strings.HasPrefix(lf, "repeat("):
			inner := strings.TrimSpace(f[len("repeat("):])
			if !strings.HasSuffix(inner, ")") {
				return 0, false
			}
			inner = strings.TrimSpace(inner[:len(inner)-1])
			c := strings.IndexByte(inner, ',')
			if c < 0 {
				return 0, false
			}
			count, err := strconv.Atoi(strings.TrimSpace(inner[:c]))
			if err != nil || count < 1 {
				return 0, false // auto-fit / auto-fill: the count is the viewport's
			}
			tracks, ok := gridTrackCount(strings.TrimSpace(inner[c+1:]))
			if !ok {
				return 0, false
			}
			n += count * tracks
		default:
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return n, true
}

// isSingleColumnTrackList reports whether a grid-template-columns value lays its
// items out in exactly one column.
func isSingleColumnTrackList(v string) bool {
	n, ok := gridTrackCount(v)
	return ok && n == 1
}

// isColumnFlexDirection reports whether a flex-direction value stacks its items
// in one column. `column-reverse` is refused: it is a single column, but it also
// reverses the reading order of a header this criterion is about, so a page that
// wants it will red here and be looked at.
func isColumnFlexDirection(v string) bool {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(v, "!important", ""))) == "column"
}

// declaredIdents reads a value as the IDENT LIST it is, lowercased, with a
// parenthesised group left whole. A value SEARCHED instead of compared answers a
// question nobody asked: `color-scheme: lightdark` is one <custom-ident> naming a
// scheme no user agent implements - the document opts into NEITHER, and every
// native control falls back to the UA's light default inside the dark page, which
// is verbatim the harm AC4 names - and it satisfies
// `strings.Contains(cs, "light") && strings.Contains(cs, "dark")` with the single
// word that contains both (impl-gate finding F24).
func declaredIdents(v string) map[string]bool {
	out := map[string]bool{}
	for _, f := range topLevelFields(strings.ToLower(strings.ReplaceAll(normSpace(v), "!important", ""))) {
		out[strings.TrimSpace(f)] = true
	}
	return out
}

// --- reading a number, and a rendered SIZE rather than an authored minimum ------

// numberOrPercent reads a scale component: a bare number, or a percentage of the
// authored size. ok=false means this reader cannot evaluate it.
func numberOrPercent(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "%") {
		f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, "%")), 64)
		return f / 100, err == nil
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

// scaleProps are the properties that change the size a box RENDERS at without
// changing any length declared on it. AC7 measures what is rendered ("WHEN a
// pointer target is rendered ... at least 24 CSS px in both dimensions"), and a
// sweep reading only min-height / min-width / min-block-size / min-inline-size is a
// membership narrower than the criterion's own set by exactly these three:
// `#rescan { transform:scale(0.4) }` renders a 32px control at 12.8px, half of
// WCAG 2.2 2.5.8's floor in both dimensions and hit area included, and not one
// minimum on the page moves (impl-gate finding F25).
var scaleProps = map[string]bool{"transform": true, "scale": true, "zoom": true}

// scaleFactorOf returns the smallest factor a declaration applies to the rendered
// box, and ok=false when this reader cannot SHOW the declaration leaves that box at
// the size the floor sweep measured. Undecidable is red, never 1: scoring a value
// the reader cannot evaluate as a harmless one is the shape findings F2 and F11
// both named, and "resolvable or red" is the discipline the contrast derivation is
// already built on.
func scaleFactorOf(prop, value string) (float64, bool) {
	p := unprefixed(prop)
	if !scaleProps[p] {
		return 1, true
	}
	v := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(normSpace(value), "!important", "")))
	if v == "" {
		return 0, false
	}
	switch p {
	case "zoom":
		if v == "normal" || v == "reset" {
			return 1, true
		}
		return numberOrPercent(v)
	case "scale":
		if v == "none" {
			return 1, true
		}
		min := math.Inf(1)
		for _, f := range topLevelFields(v) {
			n, ok := numberOrPercent(f)
			if !ok {
				return 0, false
			}
			min = math.Min(min, n)
		}
		if math.IsInf(min, 1) {
			return 0, false
		}
		return min, true
	}
	// transform: a space-separated list of functions. Only the ones this reader can
	// show are non-shrinking are accepted; anything else (a matrix, a skew, a
	// function it does not model) is undecidable and therefore red.
	if v == "none" {
		return 1, true
	}
	min := 1.0
	for _, f := range topLevelFields(v) {
		open := strings.IndexByte(f, '(')
		if open < 0 || !strings.HasSuffix(f, ")") {
			return 0, false
		}
		args := strings.Split(f[open+1:len(f)-1], ",")
		switch strings.TrimSpace(f[:open]) {
		case "translate", "translatex", "translatey", "translatez", "translate3d":
			// A translation moves the box; its rendered size is unchanged.
		case "scale", "scalex", "scaley", "scale3d":
			for _, a := range args {
				n, ok := numberOrPercent(a)
				if !ok {
					return 0, false
				}
				min = math.Min(min, n)
			}
		default:
			return 0, false
		}
	}
	return min, true
}

// --- reading how long a motion RUNS --------------------------------------------

// motionDurationsMs returns the durations a motion declaration establishes, in
// milliseconds. A `<family>-duration` longhand states them directly; the
// `transition` / `animation` shorthand states each as the FIRST time in its
// comma-separated part (the second time is the delay). A motion longhand carrying
// no time at all - transition-property, animation-name - establishes none.
//
// It exists because AC5's reduce check read one property name, so a reduce block
// restoring motion through the shorthand, or through a rule scoped to a subtree,
// was never asked about (impl-gate finding F22 and its class).
func motionDurationsMs(prop, value string) []float64 {
	fam := motionFamily(prop)
	if fam == "" {
		return nil
	}
	v := strings.TrimSpace(strings.ReplaceAll(normSpace(value), "!important", ""))
	var out []float64
	switch unprefixed(prop) {
	case fam + "-duration":
		for _, part := range strings.Split(v, ",") {
			out = append(out, msOf(part))
		}
	case fam:
		for _, part := range strings.Split(v, ",") {
			for _, f := range topLevelFields(part) {
				if ms := msOf(f); !math.IsInf(ms, 1) {
					out = append(out, ms)
					break
				}
			}
		}
	}
	return out
}

// --- reading whether a node an operator should SEE is taken off the page --------

var (
	hiddenAssign  = regexp.MustCompile(`[\w$.]*\.hidden\s*=\s*([^;\n]+)`)
	hiddenAttr    = regexp.MustCompile(`[\w$.]*\.(?:setAttribute|toggleAttribute)\(\s*["']hidden["'][^)]*\)`)
	displayAssign = regexp.MustCompile(`[\w$.]*\.style\.(?:display|visibility)\s*=\s*([^;\n]+)`)
	nodeRemoved   = regexp.MustCompile(`[\w$.]*\.(?:remove|replaceChildren)\(\s*\)`)
)

// hidingStatements returns every expression in a fragment of the page's script that
// takes a node off the page an operator reads. AC9's word is "visible" and AC10's
// verb is "render", and every proof of either stopped at the function that BUILDS
// the row: `tr.hidden = true` beside setNoMatchRow's appendChild, or in emptyRow,
// or in capNote's `total > shown` branch, leaves the wording constructed exactly as
// pinned, inserted exactly as pinned, and shows an operator nothing (impl-gate
// finding F20).
//
// The membership is the CLASS rather than the one property the finding used, for
// the reason finding F18 gave about AC5's: a sweep narrower than the thing it
// grades is the same hole one spelling to the left. `hidden` set to the literal
// `false` is how this page puts a node back, so it is not one.
func hidingStatements(js string) []string {
	var out []string
	for _, m := range hiddenAssign.FindAllStringSubmatch(js, -1) {
		if strings.TrimSpace(m[1]) == "false" {
			continue
		}
		out = append(out, strings.TrimSpace(m[0]))
	}
	out = append(out, hiddenAttr.FindAllString(js, -1)...)
	for _, m := range displayAssign.FindAllStringSubmatch(js, -1) {
		if v := strings.TrimSpace(m[1]); v == `""` || v == "''" {
			continue
		}
		out = append(out, strings.TrimSpace(m[0]))
	}
	out = append(out, nodeRemoved.FindAllString(js, -1)...)
	return out
}

// invisibleValues are the declarations that paint a node out of existence. A
// degraded state hidden by the STYLESHEET is finding F20's shape one language to
// the left: the row is built, filled and appended exactly as every pin requires,
// and nobody sees it.
var invisibleValues = map[string]map[string]bool{
	"display":            {"none": true},
	"visibility":         {"hidden": true, "collapse": true},
	"content-visibility": {"hidden": true},
	"opacity":            {"0": true, "0%": true, "0.0": true},
	"font-size":          {"0": true, "0px": true},
}

// stateIsReachedBy reports whether a rule's selector group can apply to the node a
// degraded state is rendered on. The state's own selector (`.empty`, `#conn.down`)
// names the classes and the id that node carries, and a rule reaches it when its
// subject compound narrows past neither. The TAG is deliberately not compared:
// three of these nodes are built in the script rather than written in the markup,
// and over-inclusion here only ever holds MORE rules to the criterion.
func stateIsReachedBy(sel, state string) bool {
	want := parseCompound(lastCompound(state))
	for _, p := range selectorParts(sel) {
		c := parseCompound(lastCompound(p))
		if c.pseudoEl || c.root || (c.id != "" && c.id != want.id) {
			continue
		}
		reaches := true
		for _, cl := range c.classes {
			found := false
			for _, w := range want.classes {
				if w == cl {
					found = true
				}
			}
			if !found {
				reaches = false
			}
		}
		if reaches {
			return true
		}
	}
	return false
}

// The two readers' own proof, asserted against FIXTURES rather than against the
// shipped page - the same discipline TestPageComments_ uses - so it stays a proof
// whatever preludes and track lists the page happens to carry. Both readers exist
// because a substring test answered a question it was not asked, so both are
// proved to answer the question they ARE asked, in the exact shapes that broke
// their predecessors.
func TestPrelude_IsReadForWhatItIsAndAValueIsComparedNotSearched(t *testing.T) {
	for _, c := range []struct {
		at         string
		atWidth360 bool
		everyUser  bool
		decidable  bool
	}{
		{"", true, true, true},
		{"@media all", true, true, true},
		{"@media screen", true, false, true},
		{"@media print", false, false, true},
		{"@media (max-width: 640px)", true, false, true},
		{"@media only screen and (max-width: 640px)", true, false, true},
		{"@media (max-width: 320px)", false, false, true},
		{"@media (min-width: 99999px)", false, false, true},
		// The shape finding F14 broke the narrow-viewport proof with: it carries
		// the max-width the old regex found and applies at no viewport at all.
		{"@media (max-width: 640px) and (min-width: 99999px)", false, false, true},
		// The shapes finding F15 broke the focus proof with. Neither is DECIDABLE at
		// a viewport: a light-preference user is half the users AC6 is written about,
		// so a rule laying out a region under one of these reds rather than vanishing.
		{"@media (prefers-reduced-motion: reduce)", false, false, false},
		{"@media (prefers-color-scheme: light)", false, false, false},
		// A comma is a disjunction: one query matching is enough.
		{"@media (min-width: 99999px), (max-width: 640px)", true, false, true},
		// Shapes this reader does not model. It says so by declining to vouch for
		// them, never by assuming they hold - and, for AC6's regions, by declining to
		// vouch that they do NOT hold either.
		{"@media not all and (max-width: 640px)", false, false, false},
		{"@media (max-width: 40em)", false, false, false},
		{"@media (400px >= width)", false, false, false},
		{"@media (hover)", false, false, false},
		{"@supports (display: grid)", false, false, false},
		{"@-webkit-keyframes pulse", false, false, false},
	} {
		if got := atAppliesAtViewportWidth(c.at, 360); got != c.atWidth360 {
			t.Errorf("atAppliesAtViewportWidth(%q, 360) = %v, want %v", c.at, got, c.atWidth360)
		}
		if got := atAppliesToEveryUser(c.at); got != c.everyUser {
			t.Errorf("atAppliesToEveryUser(%q) = %v, want %v", c.at, got, c.everyUser)
		}
		if got := atDecidableAtViewportWidth(c.at); got != c.decidable {
			t.Errorf("atDecidableAtViewportWidth(%q) = %v, want %v - a prelude the reader cannot evaluate and one it evaluates to false are not the same answer", c.at, got, c.decidable)
		}
	}
	for _, c := range []string{
		"1fr", "100%", "none", "minmax(230px,1fr)", "repeat(1, 1fr)",
		"[full-start] 1fr [full-end]",
	} {
		if !isSingleColumnTrackList(c) {
			t.Errorf("isSingleColumnTrackList(%q) = false, want true", c)
		}
	}
	for _, c := range []string{
		"", "1fr 1fr", "repeat(3, 1fr)", "repeat(2, 1fr 2fr)",
		"repeat(auto-fit,minmax(230px,1fr))", "repeat(auto-fill, 1fr)",
	} {
		if isSingleColumnTrackList(c) {
			t.Errorf("isSingleColumnTrackList(%q) = true, want false - a check whose whole subject is that there is ONE column must not be satisfied by more", c)
		}
	}
	for _, c := range []struct {
		v    string
		want bool
	}{
		{"column", true}, {" column ", true}, {"row", false},
		{"column-reverse", false}, {"row-reverse", false}, {"", false},
	} {
		if got := isColumnFlexDirection(c.v); got != c.want {
			t.Errorf("isColumnFlexDirection(%q) = %v, want %v", c.v, got, c.want)
		}
	}
	for _, c := range []struct{ prop, want string }{
		{"animation", "animation"},
		{"-webkit-animation", "animation"},
		{"-MOZ-Animation-Duration", "animation-duration"},
		{"transition", "transition"},
		{"--paints-on", "--paints-on"},
		{"font-size", "font-size"},
	} {
		if got := unprefixed(c.prop); got != c.want {
			t.Errorf("unprefixed(%q) = %q, want %q", c.prop, got, c.want)
		}
	}
	// A value read as the IDENT LIST it is. The last case is the one finding F24
	// broke AC4 with: one word containing both of the two the check searched for.
	for _, c := range []struct {
		v                 string
		light, dark, both bool
	}{
		{"light dark", true, true, true},
		{"only light dark", true, true, true},
		{" LIGHT   DARK ", true, true, true},
		{"dark", false, true, false},
		{"light", true, false, false},
		{"lightdark", false, false, false},
		{"normal", false, false, false},
		{"", false, false, false},
	} {
		got := declaredIdents(c.v)
		if got["light"] != c.light || got["dark"] != c.dark {
			t.Errorf("declaredIdents(%q) reads light=%v dark=%v, want light=%v dark=%v - a scheme name is an IDENT, not a substring",
				c.v, got["light"], got["dark"], c.light, c.dark)
		}
		if (got["light"] && got["dark"]) != c.both {
			t.Errorf("declaredIdents(%q) declares both schemes = %v, want %v", c.v, got["light"] && got["dark"], c.both)
		}
	}
	// A RENDERED size, not an authored one. `ok=false` is the reader declining to
	// vouch for a declaration, which a floor sweep must treat as red.
	for _, c := range []struct {
		prop, value string
		want        float64
		ok          bool
	}{
		{"min-height", "32px", 1, true}, // not a scaling property at all
		{"text-transform", "uppercase", 1, true},
		{"transform", "none", 1, true},
		{"transform", "translateY(-2px)", 1, true},
		{"transform", "scale(0.4)", 0.4, true},
		{"transform", "scale(1.5)", 1, true},
		{"transform", "scaleX(0.5) translateY(2px)", 0.5, true},
		{"transform", "scale(2) scale(0.4)", 0.4, true},
		{"-webkit-transform", "scale(0.4)", 0.4, true},
		{"transform", "scale3d(1, 0.25, 1)", 0.25, true},
		{"transform", "matrix(0.4, 0, 0, 0.4, 0, 0)", 0, false},
		{"transform", "rotate(45deg)", 0, false},
		{"transform", "var(--shrink)", 0, false},
		{"zoom", "0.4", 0.4, true},
		{"zoom", "40%", 0.4, true},
		{"zoom", "normal", 1, true},
		{"zoom", "1.5", 1.5, true},
		{"zoom", "var(--z)", 0, false},
		{"scale", "0.4", 0.4, true},
		{"scale", "1 0.4", 0.4, true},
		{"scale", "none", 1, true},
		{"scale", "calc(1/2)", 0, false},
	} {
		got, ok := scaleFactorOf(c.prop, c.value)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("scaleFactorOf(%q, %q) = %g, %v; want %g, %v", c.prop, c.value, got, ok, c.want, c.ok)
		}
	}
	// How long a motion RUNS, read from the longhand and from the shorthand alike.
	for _, c := range []struct {
		prop, value string
		want        []float64
	}{
		{"transition-duration", "0.01ms", []float64{0.01}},
		{"transition-duration", "400ms", []float64{400}},
		{"transition-duration", "0.4s", []float64{400}},
		{"transition-duration", "0.01ms, 400ms", []float64{0.01, 400}},
		{"-webkit-transition-duration", "400ms", []float64{400}},
		{"transition-delay", "400ms", nil},
		{"transition-property", "color", nil},
		{"transition", "border-color 400ms ease", []float64{400}},
		{"transition", "border-color 400ms 200ms ease", []float64{400}},
		{"transition", "color 0s, background 400ms", []float64{0, 400}},
		{"animation-duration", "2s", []float64{2000}},
		{"animation", "pulse 2s infinite", []float64{2000}},
		{"font-size", "14px", nil},
		{"scroll-behavior", "auto", nil},
	} {
		got := motionDurationsMs(c.prop, c.value)
		if len(got) != len(c.want) {
			t.Errorf("motionDurationsMs(%q, %q) = %v, want %v", c.prop, c.value, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("motionDurationsMs(%q, %q) = %v, want %v", c.prop, c.value, got, c.want)
				break
			}
		}
	}
	// A node taken off the page an operator reads. The first four are the exact
	// edits finding F20 hid a degraded state with.
	for _, c := range []struct {
		js   string
		want int
	}{
		{"  tr.hidden = true;\n  body.appendChild(tr);", 1},
		{"  el.hidden = true;", 1},
		{`  tr.setAttribute("hidden", "");`, 1},
		{`  td.style.display = "none";`, 1},
		{"  el.hidden = false;", 0},
		{"  tr.hidden = hide;", 1},
		{"  if (existing) existing.remove();", 1},
		{`  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text != null) e.textContent = text;
  return e;`, 0},
		{`  el.textContent = "";
  el.hidden = true;`, 1},
	} {
		if got := hidingStatements(c.js); len(got) != c.want {
			t.Errorf("hidingStatements(%q) found %d hiding statements %v, want %d", c.js, len(got), got, c.want)
		}
	}
	// An EXEMPTION claimed by a class name, compared rather than searched for. The
	// `.tablewrapper` case is the substring leak: it is scoped to nothing and would
	// scroll the page body sideways at 360px.
	for _, c := range []struct {
		sel  string
		want bool
	}{
		{".tablewrap", true},
		{".tablewrap table", true},
		{".tablewrap > table th", true},
		{".tablewrap td[style]", true},
		{".tablewrap, .tablewrap table", true},
		{".tablewrapper", false},
		{".not-tablewrap", false},
		{".tablewrap, main", false},
		{"main", false},
		{"", false},
	} {
		if got := exemptTablewrap(c.sel); got != c.want {
			t.Errorf("exemptTablewrap(%q) = %v, want %v - AC6's one exemption belongs to the .tablewrap class, not to every selector spelled with those letters", c.sel, got, c.want)
		}
	}
	// Which rules can reach the node a degraded state is rendered on.
	for _, c := range []struct {
		sel, state string
		want       bool
	}{
		{".empty", ".empty", true},
		{"td.empty", ".empty", true},
		{".empty, .nr", ".empty", true},
		{"*", ".empty", true},
		{"section .note", ".note.cap", true},
		{".note.cap", ".note.cap", true},
		{"#conn.down", "#conn.down", true},
		{"#conn", "#conn.down", true},
		{"#msg.err", "#conn.down", false},
		{"header .grow", ".empty", false},
		{".nr", ".empty", false},
		{"#msg", ".empty", false},
		{".empty::before", ".empty", false},
	} {
		if got := stateIsReachedBy(c.sel, c.state); got != c.want {
			t.Errorf("stateIsReachedBy(%q, %q) = %v, want %v", c.sel, c.state, got, c.want)
		}
	}
}

// themeTokens returns the two token sets the page declares: the dark set (the
// default, in the top-level :root) and the light set (the same names, overridden
// under prefers-color-scheme: light, merged over the dark defaults).
//
// A :root block under any OTHER at-rule is fatal rather than skipped. Such a
// block declares tokens that paint the page under some condition and that no
// theme here measures, which is the "unreadable value scored as harmless" shape
// this file refuses everywhere else.
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
		case canonicalAt(r.at) == lightAt:
			target = over
		default:
			t.Fatalf("a :root token block sits under %q, which is neither the top level nor the light theme's exact `@media (prefers-color-scheme: light)`: the tokens it declares paint the page under a condition no theme in this file measures", r.at)
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
	// Every colour token is #rrggbb, because that is the only form contrastRatio
	// can measure. A token written any other way - `rgb()`, `#abc`, `#rrggbbaa` -
	// is not seen as a colour by colourTokens, so every pair it paints drops
	// silently out of the contrast derivation instead of being measured.
	for _, set := range []map[string]string{dark, light} {
		for name, v := range set {
			v = strings.TrimSpace(v)
			if colourFuncRe.MatchString(v) || (hexLiteral.MatchString(v) && !hex6.MatchString(v)) {
				t.Errorf("the token %s is %q; this file measures #rrggbb, so a colour written any other way would leave every pair it paints unmeasured", name, v)
			}
		}
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

// measuredPx resolves a length through any single token reference and reports
// whether it could be measured at all. It is deliberately the only reader of a
// length in this file: a sweep that cannot measure a value has to SAY so, because
// the alternative - scoring it and moving on - is how a 60em width scores 0 and
// clears a "wider than the breakpoint" test.
func measuredPx(tokens map[string]string, v string) (float64, bool) {
	v = strings.TrimSpace(v)
	// Only a value that is EXACTLY var(--x) resolves through its token. A var()
	// sitting inside a larger expression must not be read as the whole value:
	// substituting it there measured `calc(1000px + var(--sp-4))` as 8px, which
	// clears every ceiling in this file, and that is the F2/F11 shape once more -
	// a value the reader cannot evaluate scored as a harmless one. An expression is
	// UNMEASURABLE, and every caller here is built to red on that.
	if m := soleVarRef.FindStringSubmatch(v); m != nil {
		v = strings.TrimSpace(resolveToken(tokens, m[1]))
	}
	return cssPx(v)
}

// pxOf reads an unmeasurable length as 0. That is safe for a FLOOR sweep and only
// for a floor sweep: 0 is under every floor, so an unreadable value reds there. A
// sweep asking whether a length is too LARGE must not use it - 0 clears every
// ceiling - and the AC6 width sweep therefore reads its lengths itself and refuses
// the ones it cannot measure.
func pxOf(tokens map[string]string, v string) float64 {
	px, ok := measuredPx(tokens, v)
	if !ok {
		return 0
	}
	return px
}

// --- reading a SELECTOR, rather than the words it is spelled with --------------

// selectorParts splits a selector GROUP into its parts. Asking a question of the
// whole group as one string answers it for every part at once, which is exactly
// how an exemption written for one selector leaks onto another:
// `button:disabled, .empty { color:var(--line) }` is disabled-only by a
// strings.Contains and is not disabled-only at all.
func selectorParts(sel string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(sel); i++ {
		switch sel[i] {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(sel[start:i]))
				start = i + 1
			}
		}
	}
	out = append(out, strings.TrimSpace(sel[start:]))
	var keep []string
	for _, p := range out {
		if p != "" {
			keep = append(keep, p)
		}
	}
	return keep
}

// lastCompound returns the rightmost compound of one selector part - the piece
// that picks the element the rule actually applies TO. Everything to the left of
// the last top-level combinator is context.
func lastCompound(part string) string {
	depth, start := 0, 0
	for i := 0; i < len(part); i++ {
		switch part[i] {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ' ', '\t', '\n', '>', '+', '~':
			if depth == 0 {
				start = i + 1
			}
		}
	}
	return strings.TrimSpace(part[start:])
}

// compound is a parsed compound selector: the simple selectors that decide which
// elements it can apply to.
type compound struct {
	tag      string
	id       string
	classes  []string
	pseudoEl bool // a ::pseudo-element is not the element itself
	root     bool // :root is the document element, never a control
}

func isNameByte(c byte) bool {
	return c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func parseCompound(c string) compound {
	var out compound
	for i := 0; i < len(c); {
		switch c[i] {
		case '#', '.':
			j := i + 1
			for j < len(c) && isNameByte(c[j]) {
				j++
			}
			if c[i] == '#' {
				out.id = c[i+1 : j]
			} else {
				out.classes = append(out.classes, c[i+1:j])
			}
			i = j
		case ':':
			j := i + 1
			if j < len(c) && c[j] == ':' {
				out.pseudoEl = true
				j++
			}
			name := j
			for j < len(c) && isNameByte(c[j]) {
				j++
			}
			if strings.EqualFold(c[name:j], "root") {
				out.root = true
			}
			if j < len(c) && c[j] == '(' { // a functional pseudo-class: skip its argument
				depth := 0
				for ; j < len(c); j++ {
					if c[j] == '(' {
						depth++
					}
					if c[j] == ')' {
						depth--
						if depth == 0 {
							j++
							break
						}
					}
				}
			}
			i = j
		case '[': // an attribute selector: not decided here, see reaches()
			j := i
			for ; j < len(c) && c[j] != ']'; j++ {
			}
			if j < len(c) {
				j++
			}
			i = j
		case '*':
			i++
		default:
			j := i
			for j < len(c) && isNameByte(c[j]) {
				j++
			}
			if j == i {
				i++ // a byte this reader does not model; it narrows nothing
				continue
			}
			out.tag = strings.ToLower(c[i:j])
			i = j
		}
	}
	return out
}

// universal reports whether the compound names no element in particular, so it
// applies to everything on the page (`*`, `:focus-visible`).
func (c compound) universal() bool {
	return c.tag == "" && c.id == "" && len(c.classes) == 0 && !c.root && !c.pseudoEl
}

// reaches reports whether a rule with this compound as its subject CAN apply to
// el. It is deliberately generous where it cannot decide - an attribute selector,
// an unmodelled pseudo-class, an ancestor it does not walk - because for a FLOOR
// sweep an over-inclusive membership only ever holds MORE rules to the criterion,
// while an under-inclusive one is the hole finding F12 named.
func (c compound) reaches(el pageElement) bool {
	if c.pseudoEl || c.root {
		return false
	}
	if c.tag != "" && c.tag != el.tag {
		return false
	}
	if c.id != "" && c.id != el.id {
		return false
	}
	for _, cl := range c.classes {
		if !el.hasClass(cl) {
			return false
		}
	}
	return true
}

// selectorReaches reports whether any part of a selector group can apply to any
// of the given elements.
func selectorReaches(sel string, els []pageElement) bool {
	for _, p := range selectorParts(sel) {
		c := parseCompound(lastCompound(p))
		for _, el := range els {
			if c.reaches(el) {
				return true
			}
		}
	}
	return false
}

// selectorAddresses is selectorReaches for a rule that names one of the elements
// SPECIFICALLY. A universal rule applies to every element on the page and says
// nothing about a control in particular, so it is not one of the page's controls.
func selectorAddresses(sel string, els []pageElement) bool {
	for _, p := range selectorParts(sel) {
		c := parseCompound(lastCompound(p))
		if c.universal() {
			continue
		}
		for _, el := range els {
			if c.reaches(el) {
				return true
			}
		}
	}
	return false
}

// hasUniversalPart reports whether a selector group carries a bare `*` part, which
// is what makes a rule apply to every element rather than to a subtree.
func hasUniversalPart(sel string) bool {
	for _, p := range selectorParts(sel) {
		if p != lastCompound(p) {
			continue // `.foo *` is a descendant rule, not a universal one
		}
		if parseCompound(p).universal() {
			return true
		}
	}
	return false
}

// disabledOnly reports whether EVERY part of a selector group applies only to a
// disabled control, which WCAG 2.2 exempts from 1.4.3 and 1.4.11 alike.
// `:not(:disabled)` is the opposite statement and must not be caught by it, and
// one disabled-only part must not carry the exemption to the rest of its group.
func disabledOnly(sel string) bool {
	parts := selectorParts(sel)
	if len(parts) == 0 {
		return false
	}
	for _, p := range parts {
		if !strings.Contains(p, ":disabled") || strings.Contains(p, ":not(:disabled)") {
			return false
		}
	}
	return true
}

// compoundsIn splits one selector part into its compounds, dropping the
// combinators between them. lastCompound answers which element a rule applies TO;
// this answers which elements it is SCOPED BY, which is the question an exemption
// has to ask.
func compoundsIn(part string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(part); i++ {
		switch part[i] {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ' ', '\t', '\n', '>', '+', '~':
			if depth == 0 {
				if c := strings.TrimSpace(part[start:i]); c != "" {
					out = append(out, c)
				}
				start = i + 1
			}
		}
	}
	if c := strings.TrimSpace(part[start:]); c != "" {
		out = append(out, c)
	}
	return out
}

// exemptTablewrap reports whether EVERY part of a selector group is scoped to a
// .tablewrap, which is the one place AC6 lets a box be wider than the viewport.
//
// The class name is COMPARED, not searched for. `strings.Contains(p, "tablewrap")`
// hands the exemption to `.tablewrapper { min-width:960px }`, which scrolls the page
// body sideways at 360px and is scoped to nothing - an exemption claimed by a
// substring, which is the leak selectorParts was written for one level up.
func exemptTablewrap(sel string) bool {
	parts := selectorParts(sel)
	if len(parts) == 0 {
		return false
	}
	for _, p := range parts {
		scoped := false
		for _, c := range compoundsIn(p) {
			for _, cl := range parseCompound(c).classes {
				if cl == "tablewrap" {
					scoped = true
				}
			}
		}
		if !scoped {
			return false
		}
	}
	return true
}

// --- the MARKUP the stylesheet paints ------------------------------------------

// pageElement is one element of the page's markup: enough of it to say which
// selectors can reach it, what it declares inline, and where it sits.
type pageElement struct {
	tag         string
	id          string
	classes     []string
	style       string
	at          int
	inTablewrap bool
}

func (e pageElement) hasClass(c string) bool {
	for _, x := range e.classes {
		if x == c {
			return true
		}
	}
	return false
}

func (e pageElement) String() string {
	s := "<" + e.tag
	if e.id != "" {
		s += " id=\"" + e.id + "\""
	}
	for _, c := range e.classes {
		s += " ." + c
	}
	return s + ">"
}

// selector spells the element as the selector that would pick it, with [style]
// marking the attribute the declarations came from. Written this way so an
// inline rule is read by the same selector machinery as every other rule rather
// than by a shape only its own sweep understands.
func (e pageElement) selector() string {
	s := e.tag
	if e.id != "" {
		s += "#" + e.id
	}
	for _, c := range e.classes {
		s += "." + c
	}
	return s + "[style]"
}

var (
	tagOpen    = regexp.MustCompile(`<([a-zA-Z][a-zA-Z0-9-]*)((?:[^<>"']|"[^"]*"|'[^']*')*)>`)
	attrPair   = regexp.MustCompile(`([a-zA-Z-]+)\s*=\s*"([^"]*)"`)
	styleElem  = regexp.MustCompile(`(?s)<style>.*?</style>`)
	scriptElem = regexp.MustCompile(`(?s)<script>.*?</script>`)
	styleAttr  = regexp.MustCompile(`\bstyle\s*=`)
)

// blankOut replaces each span with spaces of the SAME LENGTH (newlines kept), so
// an index into the result is still an index into the page it came from.
func blankOut(s string, spans [][]int) string {
	b := []byte(s)
	for _, sp := range spans {
		for i := sp[0]; i < sp[1] && i < len(b); i++ {
			if b[i] != '\n' {
				b[i] = ' '
			}
		}
	}
	return string(b)
}

// pageMarkup is the page with every region a tag scanner must not read blanked:
// the HTML comments (prose is not markup - impl-gate finding F7 made that
// distinction once already), the stylesheet, and the script, whose JavaScript
// carries `<` in comparisons and in strings.
func pageMarkup(t *testing.T) string {
	t.Helper()
	s := string(indexHTML)
	style := styleElem.FindAllStringIndex(s, -1)
	script := scriptElem.FindAllStringIndex(s, -1)
	if len(style) != 1 || len(script) != 1 {
		t.Fatalf("the page has %d <style> and %d <script> elements; this reader models one of each", len(style), len(script))
	}
	spans := htmlComment.FindAllStringIndex(s, -1)
	spans = append(spans, style...)
	spans = append(spans, script...)
	return blankOut(s, spans)
}

// divSpans returns the [start,end) span of each <div> opened with the given tag
// text, ending at its closing </div>.
func divSpans(t *testing.T, s, open string) [][2]int {
	t.Helper()
	var spans [][2]int
	for i := 0; ; {
		k := strings.Index(s[i:], open)
		if k < 0 {
			break
		}
		start := i + k
		e := strings.Index(s[start:], "</div>")
		if e < 0 {
			t.Fatalf("a %s is never closed", open)
		}
		spans = append(spans, [2]int{start, start + e})
		i = start + e
	}
	return spans
}

// elementsIn returns every element the page's markup opens, in document order.
func elementsIn(t *testing.T, markup string) []pageElement {
	t.Helper()
	wraps := divSpans(t, markup, `<div class="tablewrap">`)
	var out []pageElement
	for _, m := range tagOpen.FindAllStringSubmatchIndex(markup, -1) {
		el := pageElement{
			tag:         strings.ToLower(markup[m[2]:m[3]]),
			at:          m[0],
			inTablewrap: inAnySpan(wraps, m[0]),
		}
		for _, a := range attrPair.FindAllStringSubmatch(markup[m[4]:m[5]], -1) {
			switch strings.ToLower(a[1]) {
			case "id":
				el.id = a[2]
			case "class":
				el.classes = strings.Fields(a[2])
			case "style":
				el.style = a[2]
			}
		}
		out = append(out, el)
	}
	if len(out) < 20 {
		t.Fatalf("the markup reader found %d elements; it is not reading the page", len(out))
	}
	return out
}

// inlineStyle is a style="..." attribute read as the rule it is: a declaration
// block that applies to exactly one element, at the highest precedence the
// cascade has short of !important.
type inlineStyle struct {
	el   pageElement
	rule cssRule
}

// inlineStyles returns the declarations the MARKUP carries in style= attributes.
// Every sweep in this file used to read the <style> element alone, so those
// declarations were ungraded by AC1, AC2, AC5 and AC6 alike (impl-gate finding
// F13): `<main style="min-width:900px">` scrolls the page body sideways at 360px
// with every test green. They are folded into the same sweeps here rather than
// checked separately, because the criteria do not distinguish where a declaration
// is written - AC1 says "no declaration", and an attribute is one.
//
// The synthetic selector names the element for the error message, and carries
// `.tablewrap` when the element sits inside one so AC6's own exemption still
// applies to a column of a scrolling table.
func inlineStyles(t *testing.T) []inlineStyle {
	t.Helper()
	markup := pageMarkup(t)
	var out []inlineStyle
	for _, el := range elementsIn(t, markup) {
		if strings.TrimSpace(el.style) == "" {
			continue
		}
		sel := el.selector()
		if el.inTablewrap {
			sel = ".tablewrap " + sel
		}
		out = append(out, inlineStyle{el: el, rule: cssRule{sel: sel, decls: parseDecls(el.style)}})
	}
	// This reader reads a double-quoted attribute. If the markup carries a style=
	// this loop did not pick up - unquoted, single-quoted, on a tag shape the
	// scanner does not model - that is a declaration no sweep here grades, so it
	// is fatal rather than absent.
	if n := len(styleAttr.FindAllString(markup, -1)); n != len(out) {
		t.Fatalf("the markup carries %d style= attributes and this reader read %d of them; the ones it missed are declarations no sweep in this file grades", n, len(out))
	}
	return out
}

// allRules is every declaration the PAGE carries: the stylesheet's rules and the
// markup's inline ones, as one set.
func allRules(t *testing.T) []cssRule {
	t.Helper()
	out := parseCSS(t)
	for _, in := range inlineStyles(t) {
		out = append(out, in.rule)
	}
	return out
}

// pointerTargets are the elements AC7 defines as the page's pointer targets:
// every button and every input in the two .controls rows. They are read out of
// the MARKUP because that is where the criterion's own set is defined, and
// because what a rule has to do to lower one of their floors is REACH one -
// whatever words its selector happens to be spelled with (impl-gate finding F12).
//
// Every button and input on the page is returned, not only the ones inside a
// .controls row: TestTargets_EveryPointerTargetClearsTheTwentyFourPixelFloor reds
// on one placed anywhere else, so the two sets agree, and holding a stray control
// to the floor as well is never the unsafe direction.
func pointerTargets(t *testing.T) []pageElement {
	t.Helper()
	var out []pageElement
	for _, el := range elementsIn(t, pageMarkup(t)) {
		if el.tag == "button" || el.tag == "input" {
			out = append(out, el)
		}
	}
	if len(out) < 5 {
		t.Fatalf("the markup declares %d buttons/inputs; the page's five pointer targets are not being read", len(out))
	}
	return out
}

// --- every var() reference names a declared token ------------------------------

// A var() that names nothing is the F11 shape at its ROOT: colourTokens finds no
// colour in it, measuredPx finds no length in it, and every sweep built on those
// two readers then passes over the declaration in silence.
// `.nr { color:var(--mutedd) }` paints unstyled text no contrast pair covers, and
// `#rescan { min-height:var(--tgt) }` holds a pointer target at its content height
// under a floor sweep that never sees a number. Neither is a colour literal, so
// AC1 does not catch either. This closes both at the reader they share.
func TestTokens_EveryVarReferenceNamesADeclaredToken(t *testing.T) {
	rules := allRules(t)
	dark, _ := themeTokens(t, rules)
	seen := 0
	for _, r := range rules {
		for _, d := range r.decls {
			for _, name := range varRefs(d.value) {
				seen++
				if strings.TrimSpace(dark[name]) == "" {
					t.Errorf("%s { %s: %s } references %s, which no token block declares; a var() that resolves to nothing is read as neither a colour nor a length, so every sweep in this file passes over the declaration in silence",
						r.sel, d.prop, d.value, name)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("the var() sweep examined no reference - it is not reading the stylesheet")
	}
}

// --- AC1: every colour comes from a token ------------------------------------

// isColourProp names the properties whose value can carry a colour. Custom
// properties are included, so a literal cannot be smuggled into a token defined
// outside the :root blocks and then referenced as if it were one.
func isColourProp(p string) bool {
	if strings.HasPrefix(p, "--") {
		return true
	}
	// Read for the property it IS: `-webkit-text-fill-color` is a colour by any
	// reading, and a list gated on the unprefixed spelling alone is the membership
	// finding F18 named in AC5's position.
	switch unprefixed(p) {
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
// references are removed is a colour written at the point of use, which is a
// whitelist, so `color:rebeccapurple` reds exactly as `color:red` does.
var nonColourKeywords = map[string]bool{
	"solid": true, "dashed": true, "dotted": true, "double": true, "groove": true,
	"ridge": true, "inset": true, "outset": true, "none": true, "hidden": true,
	"thin": true, "medium": true, "thick": true, "inherit": true, "initial": true,
	"unset": true, "revert": true, "!important": true, "underline": true,
	"overline": true, "line-through": true, "auto": true,
}

// cssNamedColours is the CSS Color 4 <named-color> set. A hex literal and a colour
// function are refused in EVERY declaration by the two patterns above; the whitelist
// sweep catches a bare word, but only where isColourProp says a colour may appear,
// and that list cannot name every property that accepts one (background-image, mask,
// filter, the border-block/border-inline family, a shorthand yet to be invented).
// So a named colour is refused everywhere too, by NAME rather than by exclusion -
// which is precise enough to run over a font stack or a url() without inventing a
// violation.
var cssNamedColours = map[string]bool{
	"aliceblue": true, "antiquewhite": true, "aqua": true, "aquamarine": true, "azure": true,
	"beige": true, "bisque": true, "black": true, "blanchedalmond": true, "blue": true,
	"blueviolet": true, "brown": true, "burlywood": true, "cadetblue": true, "chartreuse": true,
	"chocolate": true, "coral": true, "cornflowerblue": true, "cornsilk": true, "crimson": true,
	"cyan": true, "darkblue": true, "darkcyan": true, "darkgoldenrod": true, "darkgray": true,
	"darkgreen": true, "darkgrey": true, "darkkhaki": true, "darkmagenta": true,
	"darkolivegreen": true, "darkorange": true, "darkorchid": true, "darkred": true,
	"darksalmon": true, "darkseagreen": true, "darkslateblue": true, "darkslategray": true,
	"darkslategrey": true, "darkturquoise": true, "darkviolet": true, "deeppink": true,
	"deepskyblue": true, "dimgray": true, "dimgrey": true, "dodgerblue": true, "firebrick": true,
	"floralwhite": true, "forestgreen": true, "fuchsia": true, "gainsboro": true,
	"ghostwhite": true, "gold": true, "goldenrod": true, "gray": true, "green": true,
	"greenyellow": true, "grey": true, "honeydew": true, "hotpink": true, "indianred": true,
	"indigo": true, "ivory": true, "khaki": true, "lavender": true, "lavenderblush": true,
	"lawngreen": true, "lemonchiffon": true, "lightblue": true, "lightcoral": true,
	"lightcyan": true, "lightgoldenrodyellow": true, "lightgray": true, "lightgreen": true,
	"lightgrey": true, "lightpink": true, "lightsalmon": true, "lightseagreen": true,
	"lightskyblue": true, "lightslategray": true, "lightslategrey": true, "lightsteelblue": true,
	"lightyellow": true, "lime": true, "limegreen": true, "linen": true, "magenta": true,
	"maroon": true, "mediumaquamarine": true, "mediumblue": true, "mediumorchid": true,
	"mediumpurple": true, "mediumseagreen": true, "mediumslateblue": true,
	"mediumspringgreen": true, "mediumturquoise": true, "mediumvioletred": true,
	"midnightblue": true, "mintcream": true, "mistyrose": true, "moccasin": true,
	"navajowhite": true, "navy": true, "oldlace": true, "olive": true, "olivedrab": true,
	"orange": true, "orangered": true, "orchid": true, "palegoldenrod": true, "palegreen": true,
	"paleturquoise": true, "palevioletred": true, "papayawhip": true, "peachpuff": true,
	"peru": true, "pink": true, "plum": true, "powderblue": true, "purple": true,
	"rebeccapurple": true, "red": true, "rosybrown": true, "royalblue": true,
	"saddlebrown": true, "salmon": true, "sandybrown": true, "seagreen": true, "seashell": true,
	"sienna": true, "silver": true, "skyblue": true, "slateblue": true, "slategray": true,
	"slategrey": true, "snow": true, "springgreen": true, "steelblue": true, "tan": true,
	"teal": true, "thistle": true, "tomato": true, "turquoise": true, "violet": true,
	"wheat": true, "white": true, "whitesmoke": true, "yellow": true, "yellowgreen": true,
}

// identifierProps take an author-chosen NAME as a value (a font family, a grid
// area, an animation), where a word that happens to spell a colour is not one.
var identifierProps = map[string]bool{
	"font": true, "font-family": true, "grid-area": true, "grid-row": true,
	"grid-row-start": true, "grid-row-end": true, "grid-column": true,
	"grid-column-start": true, "grid-column-end": true, "grid-template-areas": true,
	"animation": true, "animation-name": true, "counter-reset": true,
	"counter-increment": true, "content": true, "will-change": true,
	"transition-property": true,
}

var quotedString = regexp.MustCompile(`"[^"]*"|'[^']*'`)

// namedColoursIn reports the CSS named colours a declaration writes at its point
// of use, ignoring anything inside a var() reference or a quoted string.
func namedColoursIn(prop, value string) []string {
	if identifierProps[unprefixed(prop)] {
		return nil
	}
	rest := quotedString.ReplaceAllString(varCall.ReplaceAllString(value, " "), " ")
	var out []string
	for _, w := range strings.FieldsFunc(rest, func(c rune) bool {
		return c == ' ' || c == ',' || c == '(' || c == ')' || c == '/'
	}) {
		if cssNamedColours[strings.ToLower(w)] {
			out = append(out, w)
		}
	}
	return out
}

func TestTokens_EveryColourIsDeclaredInATokenBlock(t *testing.T) {
	rules := allRules(t)
	checked := 0
	for _, r := range rules {
		for _, d := range r.decls {
			// The token block is where the literals are allowed to live - and only
			// its CUSTOM PROPERTIES are. Skipping the whole :root rule exempted
			// `:root { background:#ff0000 }` too, which is a colour painted at a
			// point of use like any other.
			if r.sel == ":root" && strings.HasPrefix(d.prop, "--") {
				continue
			}
			if hexLiteral.MatchString(d.value) {
				t.Errorf("%s { %s: %s } writes a colour literal at its point of use; declare it in :root and reference it as var(--...)", r.sel, d.prop, d.value)
			}
			if colourFuncRe.MatchString(d.value) {
				t.Errorf("%s { %s: %s } writes a colour function at its point of use; declare it in :root and reference it as var(--...)", r.sel, d.prop, d.value)
			}
			if !isColourProp(d.prop) {
				// A property this file does not list as colour-bearing may still take
				// one - background-image, mask, filter, a border-block shorthand. The
				// named-colour set is checked by name there, so `background-image:
				// linear-gradient(red, blue)` reds like `color:red` does.
				for _, w := range namedColoursIn(d.prop, d.value) {
					t.Errorf("%s { %s: %s } names the colour %q at its point of use; every colour must come from a token", r.sel, d.prop, d.value, w)
				}
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

// spaceProps names every property whose value AC2 governs as "a padding, margin
// or gap length". The clause is unqualified, so the list is the whole property
// family and not the subset the page happens to write today: the eight logical
// LONGHANDS are here as well as their two-value shorthands, because
// `margin-inline-start:37px` is a margin length by any reading and a sweep that
// did not name the property would pass over it.
var spaceProps = map[string]bool{
	"padding": true, "padding-top": true, "padding-right": true, "padding-bottom": true,
	"padding-left": true, "padding-inline": true, "padding-block": true,
	"padding-inline-start": true, "padding-inline-end": true,
	"padding-block-start": true, "padding-block-end": true,
	"margin": true, "margin-top": true, "margin-right": true, "margin-bottom": true,
	"margin-left": true, "margin-inline": true, "margin-block": true,
	"margin-inline-start": true, "margin-inline-end": true,
	"margin-block-start": true, "margin-block-end": true,
	"gap": true, "row-gap": true, "column-gap": true,
}

func TestTokens_TypeAndSpaceComeFromTheDeclaredScales(t *testing.T) {
	rules := allRules(t)
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
		for _, d := range r.decls {
			// As in AC1's sweep: inside :root only the custom properties are the
			// scale. A literal `font-size` written there is written at a point of
			// use, and skipping the whole rule passed over it.
			if r.sel == ":root" && strings.HasPrefix(d.prop, "--") {
				continue
			}
			// Read for the property it IS, not for the spelling it carries
			// (finding F18's class): `-webkit-padding-before` is a padding length
			// and `-moz-font-size` is a font size, and a sweep gated on the
			// unprefixed name alone would pass over both.
			prop := unprefixed(d.prop)
			if prop == "font" {
				t.Errorf("%s { font: %s } - the font shorthand hides a font-size; declare font-size from the type scale", r.sel, d.value)
				continue
			}
			want := ""
			switch {
			case prop == "font-size":
				want = "--fs-"
			case spaceProps[prop]:
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
		// A `color` this reader cannot resolve to a token is a RED, not a skip: the
		// text it paints would otherwise leave the derivation in silence. The one
		// exception is the inherit family, which takes an ancestor's colour and is
		// therefore measured where that ancestor declared it.
		if cv := r.get("color"); len(fg) == 0 && cv != "" && !inheritedColour(cv) {
			t.Errorf("%s declares color:%s, which resolves to no colour token, so the pair it paints cannot be measured against its floor", r.sel, cv)
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
		// EVERY foreground token the declaration names, against every surface it
		// names. Measuring fg[0] alone left a second colour in the same value -
		// `color:light-dark(var(--a), var(--b))` - painted and unmeasured.
		for _, f := range fg {
			for _, s := range surfaces {
				out = append(out, paintedPair{sel: r.sel, fg: f, bg: s, floor: floor, kind: kind})
			}
		}
	}
	return out
}

// inheritedColour reports whether a colour value takes an ancestor's colour
// rather than naming one of its own.
func inheritedColour(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "inherit", "initial", "unset", "revert":
		return true
	}
	return false
}

type theme struct {
	name   string
	tokens map[string]string
}

func themes(dark, light map[string]string) []theme {
	return []theme{{"dark", dark}, {"light", light}}
}

func TestContrast_EveryPaintedPairMeetsItsFloorInBothThemes(t *testing.T) {
	rules := allRules(t)
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
	// color-scheme is read as the ident LIST it is, and each scheme is COMPARED. Two
	// strings.Contains calls answered a different question: `color-scheme: lightdark`
	// is a single <custom-ident> naming a scheme no user agent implements, so the
	// document opts into NEITHER and every native control, scrollbar and form field
	// falls back to the UA's light default inside the dark page - which is verbatim
	// the harm this criterion names - while one word satisfies both searches
	// (impl-gate finding F24). It is "a value SEARCHED instead of compared", the
	// diagnosis gridTrackCount was written for, in AC4's position.
	// ...read off the LAST top-level :root that declares it, which is the one the
	// cascade keeps. Reading it off the last :root rule FULL STOP would let a second
	// block declaring only a token blank the check it is supposed to answer.
	cs := ""
	for _, r := range rules {
		if r.sel == ":root" && r.at == "" && r.get("color-scheme") != "" {
			cs = r.get("color-scheme")
		}
	}
	schemes := declaredIdents(cs)
	for _, want := range []string{"light", "dark"} {
		if !schemes[want] {
			t.Errorf("color-scheme is %q, which does not name %q among its schemes; it must declare `light dark` so native controls, scrollbars and form fields follow the page's theme instead of staying light inside a dark page", cs, want)
		}
	}
	// The dark set is the DEFAULT, so a user agent expressing no preference still
	// gets the page holdfast ships today.
	if got := resolveToken(dark, "--bg"); got != "#0f1115" {
		t.Errorf("the default (no-preference) --bg is %q; the dark set the page ships must remain the default", got)
	}
	// The light set is a real second set, not a copy of the first - over EVERY
	// colour token the dark set declares, derived from the dark :root block in
	// source order rather than from a list of ten names written by hand. A
	// hand-written list is a membership narrower than the criterion's own set: a
	// colour token added to the dark block and forgotten in the light one keeps its
	// DARK value inside the light theme, and no assertion here would ask about it.
	covered := 0
	for _, d := range root.decls {
		name := d.prop
		if !strings.HasPrefix(name, "--") || !hex6.MatchString(resolveToken(dark, name)) {
			continue
		}
		covered++
		dv, lv := resolveToken(dark, name), resolveToken(light, name)
		if !hex6.MatchString(lv) {
			t.Errorf("the light theme does not declare %s", name)
			continue
		}
		if dv == lv {
			t.Errorf("the light theme reuses the dark %s (%s) - that is not a light set", name, dv)
		}
	}
	if covered < 10 {
		t.Fatalf("only %d colour tokens derived from the dark :root block; this proof is not reading the token set", covered)
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
	// Which rules paint a control is decided from the MARKUP as well as from the
	// selector text: the words "button" and "input" are not how a stylesheet
	// reaches a control, they are only how this page happens to spell two of its
	// rules. `#rescan { background:var(--panel); border-color:var(--panel) }`
	// repaints a control into invisibility against the surface behind it and names
	// neither word (the AC7 half of the same gate is impl-gate finding F12).
	//
	// A UNIVERSAL rule is deliberately not a control rule: `*` and `:focus-visible`
	// apply to every element on the page and say nothing about a control in
	// particular, and the surface each element sits on is covered by AC3's own
	// painted-pair derivation.
	targets := pointerTargets(t)
	inputBase := func(r cssRule) bool {
		if strings.Contains(r.sel, "input") {
			return true
		}
		for _, el := range targets {
			if el.tag == "input" && selectorAddresses(r.sel, []pageElement{el}) {
				return true
			}
		}
		return false
	}
	var out []controlRule
	for _, r := range rules {
		reaches := controlSel.MatchString(r.sel) || selectorAddresses(r.sel, targets)
		if !reaches || disabledOnly(r.sel) || strings.Contains(r.sel, "::") {
			continue
		}
		b := base["button"]
		if inputBase(r) {
			b = base[".controls input"]
		}
		fill := first(r.get("background-color"), r.get("background"), b.get("background-color"), b.get("background"))
		edge := first(r.get("border-color"), r.get("border"), b.get("border-color"), b.get("border"))
		behind := first(r.get("--paints-on"), b.get("--paints-on"))
		// Unresolvable is RED, not skipped. Dropping a control rule this reader
		// could not resolve is the F11 shape on the non-text floor: the control
		// stops being measured and nothing says so.
		if len(fill) == 0 || len(edge) == 0 || len(behind) == 0 {
			t.Errorf("%s paints a control but this reader cannot resolve all three of its fill (%v), its edge (%v) and the surface behind it (%v) to colour tokens, so the control goes unmeasured against WCAG 2.2 1.4.11's 3:1 non-text floor",
				r.sel, fill, edge, behind)
			continue
		}
		// Every combination the declarations name, not the first of each: a value
		// carrying two colour tokens paints with both.
		for _, f := range fill {
			for _, e := range edge {
				for _, bh := range behind {
					out = append(out, controlRule{sel: r.sel, fill: f, edge: e, behind: bh})
				}
			}
		}
	}
	return out
}

func TestControls_AreIdentifiableAgainstTheSurfaceTheySitOn(t *testing.T) {
	rules := allRules(t)
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

// outlineSpec is the focus indicator ONE rule declares: what it says about the
// outline's style, width, colour and offset, and whether it says anything at all.
type outlineSpec struct {
	sel     string
	at      string
	style   string
	width   string
	offset  string
	colours []string
	focus   bool // the rule is a :focus-visible rule
	stated  bool // the rule declares an outline property at all
}

func outlineOf(tokens map[string]string, r cssRule) outlineSpec {
	o := outlineSpec{sel: r.sel, at: r.at, offset: r.get("outline-offset")}
	for _, p := range selectorParts(r.sel) {
		if strings.Contains(p, ":focus-visible") {
			o.focus = true
		}
	}
	if sh := r.get("outline"); sh != "" {
		o.stated = true
		o.colours = colourTokens(tokens, sh)
		for _, f := range strings.Fields(sh) {
			lf := strings.ToLower(f)
			if lf == "none" || lf == "hidden" {
				o.style = lf
			}
			if o.width == "" {
				if _, ok := measuredPx(tokens, f); ok {
					o.width = f
				} else if f == "0" {
					// `outline:0` is a zero width written without a unit, and it
					// removes the indicator exactly as `none` does.
					o.width = "0px"
				}
			}
		}
	}
	if v := r.get("outline-style"); v != "" {
		o.stated = true
		o.style = strings.ToLower(strings.TrimSpace(v))
	}
	if v := r.get("outline-width"); v != "" {
		o.stated = true
		o.width = v
	}
	if v := r.get("outline-color"); v != "" {
		o.stated = true
		o.colours = colourTokens(tokens, v)
	}
	return o
}

// hasBareFocus reports whether a selector group carries a :focus that is not
// :focus-visible or :focus-within - the shape that would show the ring on a mouse
// click. Asked of the PARSED selectors, so a comment mentioning `:focus {` is not
// mistaken for a rule and a rule spelled `:focus:hover` is not missed.
func hasBareFocus(sel string) bool {
	for _, p := range selectorParts(sel) {
		for i := 0; ; {
			k := strings.Index(p[i:], ":focus")
			if k < 0 {
				break
			}
			at := i + k + len(":focus")
			if rest := p[at:]; !strings.HasPrefix(rest, "-visible") && !strings.HasPrefix(rest, "-within") {
				return true
			}
			i = at
		}
	}
	return false
}

func TestFocus_RingClearsBothTheControlFillAndTheSurfaceBehindIt(t *testing.T) {
	rules := allRules(t)
	dark, light := themeTokens(t, rules)
	targets := pointerTargets(t)

	// EVERY rule that says anything about an outline is resolved, and one this
	// reader cannot resolve FAILS rather than being discarded.
	//
	// This loop used to fold every :focus-visible rule into one colour and one
	// width, and both assignments were conditional on the rule yielding a value.
	// `outline:none` yields neither - no colour token, and `none` is not a
	// measurable length - so a second, more specific rule switching the ring OFF
	// was read and thrown away while the test went on reporting the colour and
	// width of the base rule the cascade had already overridden (impl-gate finding
	// F11). `button.primary:focus-visible { outline:none }` is one line, it is the
	// standard prelude to a bespoke ring, and AC8 names that button by hand.
	//
	// The sweep is not restricted to :focus-visible rules either, because
	// specificity does not respect the distinction: `button.primary { outline:none }`
	// (0,1,1) outranks `:focus-visible` (0,1,0) and removes the same indicator
	// without ever mentioning focus. No rule on this page may switch an outline
	// off; a rule that needs to will red here and be looked at, which is the point.
	//
	// The 2px figure is WCAG 2.2 2.4.13 Focus Appearance, one of the success
	// criteria the spec's Constraints section carries at
	// work/specs/S0035-holdfast-dashboard-ui/sources/www.w3.org-TR-WCAG22: the
	// indicator has to be at least as large as a 2 CSS px thick perimeter.
	var rings []outlineSpec
	for _, r := range rules {
		o := outlineOf(dark, r)
		if !o.stated {
			continue
		}
		where := o.sel
		if o.at != "" {
			where = o.at + " { " + o.sel
		}
		if o.style == "none" || o.style == "hidden" {
			t.Errorf("%s switches the outline off (%s); on a control this draws no focus indicator at all, whatever a less specific rule declared", where, o.style)
			continue
		}
		w, ok := measuredPx(dark, o.width)
		if !ok {
			t.Errorf("%s declares an outline whose width (%q) this reader cannot measure, so nothing says an indicator is drawn at all", where, o.width)
			continue
		}
		if w < 2 {
			t.Errorf("%s draws a %gpx outline; a zero-width outline draws no indicator, and WCAG 2.2 2.4.13 puts the floor at a 2 CSS px perimeter", where, w)
			continue
		}
		if len(o.colours) == 0 {
			t.Errorf("%s draws an outline in no colour token, so the ring it draws cannot be measured against the control it marks", where)
			continue
		}
		if o.focus {
			off, ok := measuredPx(dark, o.offset)
			switch {
			case !ok:
				t.Errorf("%s declares no measurable outline-offset (%q), so the gap that puts the ring against the surface behind the control is gone", where, o.offset)
			case off < 0:
				// Checking the offset for MEASURABILITY alone let `-8px` through,
				// and a negative offset draws the ring INSIDE the control's border
				// box, where it never meets the surface behind (impl-gate finding
				// F19). AC8 requires the indicator to clear 3:1 against "the surface
				// immediately behind it", which a ring painted entirely on the
				// control's own fill is never adjacent to - so the half of the
				// criterion this file measures would be measuring a boundary the
				// page does not draw. Zero is allowed: the ring then sits flush
				// against the border edge, which IS against the surface behind.
				t.Errorf("%s draws its ring at outline-offset %gpx; a negative offset puts the indicator inside the control's own border box, where it never meets the surface immediately behind the control that AC8 measures it against", where, off)
			}
		}
		rings = append(rings, o)
	}
	if len(rings) == 0 {
		t.Fatal("no rule draws a focus indicator from a colour token")
	}
	// outline-offset is read on EVERY rule that declares it, whether or not that rule
	// also restates an outline. `outlineOf` sets `stated` from outline /
	// outline-style / outline-width / outline-color alone, and the loop above
	// discards an unstated rule before its offset is ever looked at - so
	// `:focus-visible { outline-offset:-8px }` won the cascade over the base rule,
	// pulled the ring INSIDE every control's border box where it never meets the
	// surface AC8 measures it against, and was thrown away first (impl-gate finding
	// F23). It is finding F19's violation spelled in one declaration instead of two,
	// and it is this loop's own class stated exactly: an assertion conditional on the
	// thing it checks being present.
	offsets := 0
	for _, r := range rules {
		v := r.get("outline-offset")
		if v == "" {
			continue
		}
		offsets++
		where := r.sel
		if r.at != "" {
			where = r.at + " { " + r.sel
		}
		off, ok := measuredPx(dark, v)
		switch {
		case !ok:
			t.Errorf("%s declares an outline-offset (%q) this reader cannot measure, so nothing says the focus ring is drawn where the surface behind the control is visible", where, v)
		case off < 0:
			t.Errorf("%s draws its ring at outline-offset %gpx; a negative offset puts the indicator inside the control's own border box, where it never meets the surface immediately behind the control that AC8 measures it against", where, off)
		}
	}
	if offsets == 0 {
		t.Error("no rule on the page declares an outline-offset, so nothing holds the focus ring clear of the control it marks")
	}
	// Every pointer target is REACHED by one of those rules - and by one that
	// APPLIES. A ring scoped to `button:focus-visible` draws nothing on the two
	// inputs, and folding the rules together hid which controls were covered at all.
	//
	// The at-rule the rule sits under used to be kept only as an error label, so
	// the page's single :focus-visible rule could be wrapped in ANY media query and
	// every control still counted as covered (impl-gate finding F15):
	// `@media (min-width: 99999px)` leaves nobody with an indicator, and
	// `@media (prefers-reduced-motion: reduce)` leaves it to one user preference.
	// AC8 says the ring is drawn "in both themes", so only a rule this reader can
	// show is live for EVERY user agent counts as drawing it - the way themeTokens
	// decides which :root block is the live one. A page that wants to declare the
	// ring once per theme will red here and be looked at; this one declares it once,
	// at the top level, in a token each theme redefines.
	for _, el := range targets {
		covered := false
		for _, o := range rings {
			if o.focus && atAppliesToEveryUser(o.at) && selectorReaches(o.sel, []pageElement{el}) {
				covered = true
			}
		}
		if !covered {
			t.Errorf("no :focus-visible rule that draws an indicator for every user agent reaches %s, so that control shows nothing on keyboard focus", el)
		}
	}
	ctrls := controlRules(t, rules, dark)
	if len(ctrls) < 4 {
		t.Fatalf("only %d control rules derived; the focus proof would cover almost nothing", len(ctrls))
	}
	for _, th := range themes(dark, light) {
		for _, o := range rings {
			for _, focus := range o.colours {
				for _, c := range ctrls {
					if got := ratio(t, th.tokens, focus, c.fill); got < 3.0 {
						t.Errorf("%s theme: the focus ring %s (%s) is %.2f:1 on %s's own fill %s - a ring you cannot see on the control it is marking is not an indicator",
							th.name, focus, o.sel, got, c.sel, c.fill)
					}
					if got := ratio(t, th.tokens, focus, c.behind); got < 3.0 {
						t.Errorf("%s theme: the focus ring %s (%s) is %.2f:1 on the surface behind %s (%s)",
							th.name, focus, o.sel, got, c.sel, c.behind)
					}
				}
			}
		}
	}
	// Keyboard focus shows it; a mouse click does not.
	for _, r := range rules {
		if hasBareFocus(r.sel) {
			t.Errorf("%s is a bare :focus rule and would show the ring on a mouse click; the ring is :focus-visible only", r.sel)
		}
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
	// The floor is not a property of the TOP-LEVEL rules alone. A rule in any
	// at-rule block can lower a minimum back under it, and the page already carries
	// a `@media (max-width: 640px)` block that restyles `.controls input` - so the
	// two base rules above would never see it (impl-gate finding F8). Every rule
	// anywhere in the stylesheet that can reach a pointer target and declares one of
	// those properties is held to the same floor, which also covers a more specific
	// top-level rule such as `button.primary`.
	//
	// WHICH rules the floor applies to is the criterion's own question, and it is
	// answered from the MARKUP: the pointer targets are the page's buttons and
	// inputs, and a rule lowers one of their floors if its selector can REACH one.
	// Gating the sweep on the words "button" and "input" appearing in the selector
	// text answered a different question, and missed every rule that addresses a
	// control by its id - which all five of them have, and which outranks the base
	// `button` rule (1,0,0 against 0,0,1). `#rescan { min-height:20px;
	// min-width:20px }` held the one control that starts a scan four pixels under
	// the floor with every test in this package green (impl-gate finding F12).
	//
	// The textual gate is kept as a UNION with the derived one, so nothing that was
	// swept before stops being swept, and the logical longhands are named for the
	// reason finding F10 gave: `min-block-size:20px` is a minimum height.
	targets := pointerTargets(t)
	swept := 0
	floorProps := []string{"min-height", "min-width", "min-block-size", "min-inline-size"}
	check := func(where, prop, v string) {
		swept++
		px, ok := measuredPx(dark, v)
		if !ok {
			t.Errorf("%s { %s: %s } is a length this sweep cannot measure, so it cannot be shown to clear the 24px target floor", where, prop, v)
			return
		}
		if px < 24 {
			t.Errorf("%s { %s: %s } holds the target at %gpx, under WCAG 2.2 2.5.8's 24px floor", where, prop, v, px)
		}
	}
	// AC7 measures what is RENDERED, and a minimum is not a rendered size. The four
	// property names above are a membership narrower than the criterion's own set by
	// exactly the properties that scale a box without changing a length declared on
	// it: `#rescan { transform:scale(0.4) }`, in this page's own id idiom, renders a
	// 32px control at 12.8px - half the floor in both dimensions, hit area included -
	// and every minimum the sweep reads is untouched (impl-gate finding F25).
	// Recorded reading 19 chose to be generous about which RULES reach a target; a
	// sweep that is then narrow about which DECLARATIONS is narrow in the unsafe
	// direction. So a rule reaching a target may not scale it below the size the
	// minimums are measured at, and a scale this reader cannot evaluate REDS rather
	// than passing - the same "resolvable or red" the contrast derivation is built
	// on.
	checkScale := func(where, prop, v string) {
		f, ok := scaleFactorOf(prop, v)
		switch {
		case !ok:
			t.Errorf("%s { %s: %s } scales the rendered box by a factor this sweep cannot evaluate, so the target cannot be SHOWN to still clear WCAG 2.2 2.5.8's 24px floor", where, prop, v)
		case f < 1:
			t.Errorf("%s { %s: %s } renders the target at %g of the size every minimum on this page is measured at, so a control held at the authored floor is drawn under WCAG 2.2 2.5.8's 24px floor", where, prop, v, f)
		}
	}
	for _, c := range rules {
		where := c.sel
		if c.at != "" {
			where = c.at + " { " + c.sel
		}
		// A scale is asked of EVERY rule, not only of the ones that reach a target,
		// because these three properties scale an element's whole SUBTREE:
		// `.controls { transform:scale(0.4) }` renders every button and input in that
		// row at 12.8px while reaching none of them, and this reader has no parent
		// chain to decide ancestry with. The page declares no transform, scale or zoom
		// at all, so refusing a shrink outright costs it nothing and closes the
		// ancestor case with the direct one - undecidable REDS, the strict direction
		// this file takes everywhere.
		for _, d := range c.decls {
			if scaleProps[unprefixed(d.prop)] {
				checkScale(where, d.prop, d.value)
			}
		}
		textual := strings.Contains(c.sel, "button") || strings.Contains(c.sel, "input")
		if !textual && !selectorReaches(c.sel, targets) {
			continue
		}
		for _, prop := range floorProps {
			v := c.get(prop)
			if v == "" {
				continue
			}
			check(where, prop, v)
		}
	}
	// A style= attribute on a control outranks every rule above it, so it is held
	// to the same floor (impl-gate finding F13).
	for _, in := range inlineStyles(t) {
		for _, d := range in.rule.decls {
			if scaleProps[unprefixed(d.prop)] {
				checkScale(in.rule.sel, d.prop, d.value)
			}
		}
		if in.el.tag != "button" && in.el.tag != "input" {
			continue
		}
		for _, prop := range floorProps {
			if v := in.rule.get(prop); v != "" {
				check(in.rule.sel, prop, v)
			}
		}
	}
	if swept < 4 {
		t.Fatalf("the target sweep examined %d minimum-size declarations on the page's controls; it expects at least the two base rules' four and is not reading the stylesheet", swept)
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

// keyframesAt matches a keyframes at-rule whatever vendor prefix it carries.
// `strings.Contains(css, "@keyframes")` is a membership narrower than AC5's own
// set - which says "any ... @keyframes" - by exactly the prefix WebKit and Blink
// still honour, so a page animating through `@-webkit-keyframes` declared no
// motion at all and the sweep took the criterion's no-motion exit (impl-gate
// finding F18).
var keyframesAt = regexp.MustCompile(`(?i)@\s*(?:-[a-zA-Z]+-)?keyframes\b`)

// motionFamily returns the motion property family a declaration belongs to -
// "transition" or "animation" - whatever vendor prefix it is spelled with, and ""
// for a property that declares no motion.
func motionFamily(prop string) string {
	p := unprefixed(prop)
	switch {
	case p == "transition" || strings.HasPrefix(p, "transition-"):
		return "transition"
	case p == "animation" || strings.HasPrefix(p, "animation-"):
		return "animation"
	}
	return ""
}

func TestMotion_NoStateDependsOnItAndAReducePreferenceStopsIt(t *testing.T) {
	// Comments stripped: a comment saying the page declares no keyframes is not a
	// keyframes block. Read for the at-rule it IS, prefix and all.
	if m := keyframesAt.FindString(cssComment.ReplaceAllString(styleBlock(t), " ")); m != "" {
		t.Errorf("the page declares %s; a state that arrives by animation is not readable from a static frame", strings.TrimSpace(m))
	}
	// Inline style= attributes are motion too, and the reduce block's !important
	// is what overrides them - so they are swept with the stylesheet, not apart
	// from it.
	rules := allRules(t)
	// The families the page actually declares motion in, so the reduce block below
	// is required to cut each of them and not only the one this page happens to
	// use. `animation` reds outright by the loop's first branch, but the
	// requirement is stated rather than left implied: AC5's set is "any transition,
	// animation or @keyframes", and a sweep that only ever cut transitions left a
	// user who asked for reduced motion watching an animation run (finding F18).
	moved := map[string]int{}
	for _, r := range rules {
		for _, d := range r.decls {
			fam := motionFamily(d.prop)
			if fam == "animation" {
				t.Errorf("%s { %s: %s } animates; every state on this page must be readable from a static frame", r.sel, d.prop, d.value)
			}
			if fam != "" && canonicalAt(r.at) != reduceAt {
				moved[fam]++
			}
		}
	}
	if len(moved) == 0 {
		return // the page declares no motion at all: the criterion is met with no media block
	}
	// The block that stops the motion has to be the block a reduce preference
	// actually turns on, and it has to reach every element. Matching the prelude on
	// the words "prefers-reduced-motion" and "reduce" accepted
	// `@media (prefers-reduced-motion: reduce) and (min-width: 99999px)`, which
	// never applies; matching the selector on the character `*` accepted `.foo *`,
	// which reaches a subtree.
	//
	// And the rule that decides is the LAST applying one, which is the reading AC6's
	// region loop takes 150 lines further down this file. `cut[fam] = true` was set
	// by the FIRST qualifying rule and never unset, so a second reduce block of
	// identical origin, importance and specificity, later in source order, restored
	// 400ms transitions while `cut["transition"]` stayed true - set by the block it
	// had just overridden (impl-gate finding F22). The duration is read from the
	// shorthand as well as the longhand, for the reason finding F18 gave: AC5's set
	// is the motion the page declares, not the one property name a sweep happened to
	// look up.
	cut := map[string]bool{}
	for _, r := range rules {
		if canonicalAt(r.at) != reduceAt || !hasUniversalPart(r.sel) {
			continue
		}
		for _, d := range r.decls {
			fam := motionFamily(d.prop)
			durs := motionDurationsMs(d.prop, d.value)
			if fam == "" || len(durs) == 0 {
				continue
			}
			longest := 0.0
			for _, ms := range durs {
				longest = math.Max(longest, ms)
			}
			cut[fam] = strings.Contains(d.value, "!important") && longest <= 1
		}
	}
	for _, fam := range []string{"transition", "animation"} {
		if moved[fam] > 0 && !cut[fam] {
			t.Errorf("the page declares motion in the %s family (%d declarations) but no @media (prefers-reduced-motion: reduce) rule cuts every %s-duration to no perceptible motion", fam, moved[fam], fam)
		}
	}
	// ...and NO rule anywhere on the page may declare a perceptible motion duration
	// as !important. The cut above is what makes every ordinary motion declaration on
	// this page harmless: an important universal declaration beats a normal one at
	// any specificity, in any block, which is why a normal 400ms transition is not a
	// violation and why the refuter discarded that mutation itself. An IMPORTANT one
	// is the exception, and it is the whole of AC5's remaining exposure - it beats
	// the cut whenever it is more specific or later, and this reader will not model
	// the cascade to decide which. Asking it of every rule, rather than of the rules
	// whose prelude compares equal to the reduce block's, is deliberate: the equality
	// is a prelude COMPARISON, and
	// `@media (prefers-reduced-motion: reduce) and (min-width: 0px)` is live for the
	// same user and equal to nothing (impl-gate finding F22's class, one prelude to
	// the left). A page that needs an important motion declaration will red here and
	// be looked at, which is what the criterion wants.
	for _, r := range rules {
		for _, d := range r.decls {
			if !strings.Contains(d.value, "!important") {
				continue
			}
			for _, ms := range motionDurationsMs(d.prop, d.value) {
				where := r.sel
				if r.at != "" {
					where = r.at + " { " + r.sel
				}
				if math.IsInf(ms, 1) {
					t.Errorf("%s { %s: %s } declares an important duration this reader cannot measure, so it cannot be shown to leave a user who asked for reduced motion with no perceptible motion", where, d.prop, d.value)
					break
				}
				if ms > 1 {
					t.Errorf("%s { %s: %s } runs for %gms and is IMPORTANT, so it beats the reduce block's own important cut wherever it is more specific or later; a user who asked for reduced motion must be left with no perceptible motion at all", where, d.prop, d.value, ms)
					break
				}
			}
		}
	}
}

// --- AC6: the narrow viewport --------------------------------------------------

// narrowColumnRegions is what "a single column" means for each of the three
// regions AC6 names, read as a VALUE rather than searched for as a substring:
// `grid-template-columns:repeat(3, 1fr)` CONTAINS "1fr" and is three columns at
// the very viewport the criterion says must show one (impl-gate finding F14).
var narrowColumnRegions = []struct {
	sel, prop, want string
	single          func(string) bool
}{
	{"header", "flex-direction", "column", isColumnFlexDirection},
	{".controls", "flex-direction", "column", isColumnFlexDirection},
	{".aggs", "grid-template-columns", "a single track", isSingleColumnTrackList},
}

// containerRelativeWidths are the width / min-width values that are not lengths
// and that cannot make a box wider than the one containing it, whatever the
// viewport - so the sweep below may pass over them without measuring one.
// max-content is deliberately absent: it is exactly how a wide box overflows.
var containerRelativeWidths = map[string]bool{
	"auto": true, "0": true, "min-content": true, "fit-content": true,
	"inherit": true, "initial": true, "unset": true, "revert": true,
}

// regionElements are the elements of the page's MARKUP that one of AC6's three
// regions names. A region that the markup does not carry is FATAL rather than
// empty: a proof cannot pass because the thing it grades is no longer on the page.
func regionElements(t *testing.T, sel string, els []pageElement) []pageElement {
	t.Helper()
	c := parseCompound(lastCompound(sel))
	var out []pageElement
	for _, el := range els {
		if c.reaches(el) {
			out = append(out, el)
		}
	}
	if len(out) == 0 {
		t.Fatalf("the markup carries no element that %s picks, so this proof grades a region the page does not have", sel)
	}
	return out
}

func TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody(t *testing.T) {
	rules := allRules(t)
	dark, _ := themeTokens(t, rules)
	narrow := pxOf(dark, "var(--bp-narrow)")
	if narrow < 360 {
		t.Fatalf("--bp-narrow is %gpx; the criterion is written at a 360px viewport", narrow)
	}
	// Which rule decides the layout at the narrow viewport is asked, and answered,
	// twice over - because the old check could fail neither question (impl-gate
	// finding F14).
	//
	// (a) The block has to APPLY there. `strings.Contains(r.at, "@media")` plus a
	// max-width regex read the rest of the prelude not at all, so `@media
	// (max-width: 640px) and (min-width: 99999px)` still yielded max-width 640,
	// still read as the narrow block, and applied at no viewport a person has. The
	// prelude is now read for the condition it IS, by the same reader AC8's ring
	// uses, and evaluated at 360px.
	//
	// (b) The VALUE has to be a single column, which means comparing it and not
	// searching it: `repeat(3, 1fr)` contains "1fr" and satisfied a check whose
	// whole subject is that there is ONE column.
	//
	// And the rule that decides is the LAST applying one that declares the
	// property, which is what the cascade gives these three equal-specificity
	// pairs. Accepting ANY applying rule would let a single-column declaration the
	// page overrides two lines later stand in for the layout an operator sees.
	//
	// WHICH rules those are is asked of the MARKUP, the way AC7's floor sweep and
	// AC8's coverage loop ask it: a rule decides this region's layout when its
	// selector can REACH the region's own element. `r.sel != region.sel` is string
	// equality on the whole selector GROUP, which answers a different question - add
	// one selector to the group and the identical override becomes invisible, so
	// `.aggs, .chips { grid-template-columns:repeat(2, 1fr) }` lays the aggregate
	// cards out in TWO columns at 360px, `header, footer { flex-direction:row }`
	// keeps the header a row, and the rule that decides is never even considered
	// (impl-gate finding F21). It is the leak selectorParts was written for, running
	// the other way: a group EVADING a check rather than claiming an exemption. It
	// also closes the same evasion spelled with a descendant combinator
	// (`body header { flex-direction:row }`), which outranks the region's own rule
	// and which part-by-part equality would still have missed.
	markup := elementsIn(t, pageMarkup(t))
	for _, region := range narrowColumnRegions {
		els := regionElements(t, region.sel, markup)
		decided, from, by := "", "", ""
		for _, r := range rules {
			v := r.get(region.prop)
			if v == "" || !selectorReaches(r.sel, els) {
				continue
			}
			// A prelude this reader cannot evaluate is RED here, not absent: dropping
			// the rule would be the same override surviving the sweep one field to the
			// left of the selector it was just fixed in.
			if !atDecidableAtViewportWidth(r.at) {
				t.Errorf("%s { %s: %s } lays out %s under a prelude this reader cannot evaluate at a %gpx viewport (%s), so it cannot be shown NOT to be the rule an operator sees there",
					r.sel, region.prop, v, region.sel, narrow, r.at)
				continue
			}
			if !atAppliesAtViewportWidth(r.at, narrow) {
				continue
			}
			decided, from, by = v, r.at, r.sel
		}
		switch {
		case decided == "":
			t.Errorf("at a %gpx viewport no rule that applies there declares %s on %s, so it is not laid out in a single column (expected %s: %s)",
				narrow, region.prop, region.sel, region.prop, region.want)
		case !region.single(decided):
			where := "the top level"
			if from != "" {
				where = from
			}
			t.Errorf("at a %gpx viewport, %s is laid out by %s { %s: %s } (from %s), which is not a single column (expected %s)",
				narrow, region.sel, by, region.prop, decided, where, region.want)
		}
	}
	// Nothing outside a .tablewrap may assert a width or a minimum width wider than
	// the narrow viewport, or the page BODY scrolls sideways. The two data tables
	// keep scrolling inside their own .tablewrap, which is how they behave today.
	//
	// RESOLVABLE OR RED, the same discipline the contrast derivation is built on: a
	// declared width is either measured against the breakpoint, or it is one of the
	// container-relative values that provably cannot exceed the box it sits in, or
	// the sweep FAILS on it. Reading an unmeasurable length as zero is what let a
	// `min-width:60em` - 840px at this page's base size - clear a 360px ceiling in
	// silence, and em-family lengths are already in this stylesheet's idiom
	// (`max-width:70ch`, `max-width:80ch`).
	// The exemption is per SELECTOR GROUP: `.tablewrap, main { min-width:960px }`
	// contains the word and scopes nothing. And the sweep covers the logical
	// longhands as well as the physical ones, for the reason finding F10 gave for
	// the space scale: `min-inline-size:960px` is a minimum width by any reading,
	// and a sweep that did not name the property would pass over it.
	swept := 0
	for _, r := range rules {
		if exemptTablewrap(r.sel) {
			continue
		}
		for _, prop := range []string{"width", "min-width", "inline-size", "min-inline-size"} {
			v := strings.TrimSpace(strings.ReplaceAll(r.get(prop), "!important", ""))
			if v == "" {
				continue
			}
			swept++
			if containerRelativeWidths[strings.ToLower(v)] {
				continue
			}
			if percentage.MatchString(v) {
				if pct, err := strconv.ParseFloat(strings.TrimSuffix(v, "%"), 64); err == nil && pct <= 100 {
					continue
				}
				t.Errorf("%s { %s: %s } is wider than the box that contains it, so the page body scrolls sideways at any viewport", r.sel, prop, v)
				continue
			}
			px, ok := measuredPx(dark, v)
			if !ok {
				t.Errorf("%s { %s: %s } is a length this sweep cannot measure, so it cannot be shown to fit the %gpx narrow viewport: express it in px, or as auto / a percentage / min-content / fit-content, which cannot exceed the box they sit in",
					r.sel, prop, v, narrow)
				continue
			}
			if px > narrow {
				t.Errorf("%s { %s: %s } is wider than the %gpx narrow viewport, so the page body itself scrolls sideways", r.sel, prop, v, narrow)
			}
		}
	}
	if swept == 0 {
		t.Fatal("the width sweep examined no declaration - it is not reading the stylesheet")
	}
}

// --- AC9: a filter term that matches nothing -----------------------------------

// funcBody returns the brace-matched body of the function the page declares with
// the given header, so an assertion can be made about that function's own text
// rather than about the whole page - which is what makes an ORDERED sweep mean
// anything. A header that is gone is fatal: a proof cannot pass because the thing
// it grades has been renamed out from under it.
func funcBody(t *testing.T, s, header string) string {
	t.Helper()
	i := strings.Index(s, header)
	if i < 0 {
		t.Fatalf("the page no longer declares %q", header)
	}
	depth, k := 0, i+len(header)-1 // the header ends with its opening brace
	for ; k < len(s); k++ {
		switch s[k] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[i+len(header) : k]
			}
		}
	}
	t.Fatalf("%q is never closed", header)
	return ""
}

// funcBodyIs asserts that a function's WHOLE body is the statements named, with
// whitespace normalised.
//
// An ordered sweep of a body proves every pinned step is present and in order. It
// proves nothing about a statement INSERTED between two of them, and one inserted
// statement is all it took to render every degraded state on this page to nobody:
// `tr.hidden = true` beside setNoMatchRow's appendChild, or in emptyRow, satisfies
// every step of both sweeps (impl-gate finding F20). These four functions are four
// to eleven lines each and exist to put a criterion's own wording in front of an
// operator, so the whole body is pinned rather than a subsequence of it - which is
// also the one form of this check that a hiding shape no reader here models
// (`style.cssText`, `parentNode.removeChild`, a class swap) cannot sit beside.
func funcBodyIs(t *testing.T, s, header string, want []string) {
	t.Helper()
	got, w := normSpace(funcBody(t, s, header)), normSpace(strings.Join(want, " "))
	if got != w {
		t.Errorf("the body of %q is\n  %s\nand the proof of the criterion it renders requires exactly\n  %s\n(an ordered sweep of its steps cannot see a statement inserted BETWEEN two of them, and one is enough to build the wording perfectly and show it to no one)",
			header, got, w)
	}
}

// jsConstString returns the VALUE of a `const NAME = "..." + "...";` declaration
// in the page's script: the string literals it concatenates, joined. It exists
// because AC9's wording was graded by a substring over the whole page while the
// render site was pinned by the constant's NAME, so the two halves were never
// tied together - set NO_MATCH_TEXT to "n/a", leave the sentences in a second
// constant nothing reads, and the row an operator sees on a non-matching term
// states neither of the two things the criterion exists to make it state, with
// every grader green (impl-gate finding F16). It does not even need a comment to
// hide in, so stripping comments does not close it; reading the declaration the
// render site actually reads does.
//
// A missing or duplicated declaration is FATAL: a proof cannot pass because the
// thing it grades was renamed out from under it, and it must not read one
// declaration while the page renders from another.
func jsConstString(t *testing.T, page, name string) string {
	t.Helper()
	header := "const " + name + " ="
	i := strings.Index(page, header)
	if i < 0 {
		t.Fatalf("the page no longer declares %q", header)
	}
	if k := strings.Index(page[i+len(header):], header); k >= 0 {
		t.Fatalf("the page declares %q more than once; this reader cannot say which one the render site reads", header)
	}
	var lits []string
	rest := page[i+len(header):]
	for k := 0; k < len(rest); {
		c := rest[k]
		if c == ';' {
			return strings.Join(lits, "")
		}
		if c != '"' && c != '\'' && c != '`' {
			k++
			continue
		}
		var b strings.Builder
		k++
		closed := false
		for k < len(rest) {
			if rest[k] == '\\' && k+1 < len(rest) {
				b.WriteByte(rest[k+1])
				k += 2
				continue
			}
			if rest[k] == c {
				k++
				closed = true
				break
			}
			b.WriteByte(rest[k])
			k++
		}
		if !closed {
			t.Fatalf("a string literal in the declaration of %s is never closed", name)
		}
		lits = append(lits, b.String())
	}
	t.Fatalf("the declaration of %s is never terminated", name)
	return ""
}

func TestFilter_ANonMatchingTermSaysSoAndSaysTheLoadedRowsAreCapped(t *testing.T) {
	// The comment-stripped page, for the reason AC10's sweep reads one: prose
	// ABOUT the page is not the page (impl-gate finding F7).
	s := pageWithoutComments(t)
	// The two sentences AC9 demands are asserted on the VALUE the render site
	// reads, not on the page at large - see jsConstString.
	noMatch := jsConstString(t, s, "NO_MATCH_TEXT")
	for _, sentence := range []string{
		"No loaded row matches this filter.",
		"themselves capped",
	} {
		if !strings.Contains(noMatch, sentence) {
			t.Errorf("NO_MATCH_TEXT - the value the no-match row is built from - is %q, which does not state %q; AC9 requires the row to state BOTH that no loaded row matches the filter AND that the loaded rows are themselves capped, so that \"no match\" is never read as \"not in the library\"",
				noMatch, sentence)
		}
	}
	for _, want := range []string{
		"const NO_MATCH_TEXT",
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
	// A list of substrings grades the PARTS and not the PATH: every fragment above
	// survives a page that builds the row, gives it its span, and then never puts it
	// into the table - which leaves both bodies silently blank on a non-matching
	// term, the exact state this criterion exists to end. So the whole construction
	// path is asserted as an ORDERED sequence inside setNoMatchRow's own body:
	// dropping any one step reds, and so does building the row after inserting it.
	const setNoMatchRowHeader = "function setNoMatchRow(body, want) {"
	setNoMatchRowSteps := []string{
		`const existing = body.querySelector("tr[data-nomatch]");`,
		`if (!want) { if (existing) existing.remove(); return; }`,
		`if (existing) return;`,
		`const table = body.closest("table");`,
		`const cols = table ? table.querySelectorAll("thead th").length : 1;`,
		`const tr = document.createElement("tr");`,
		`tr.dataset.nomatch = "1";`,
		`const td = mk("td", "empty", NO_MATCH_TEXT);`,
		`td.colSpan = cols;`,
		`tr.appendChild(td);`,
		`body.appendChild(tr);`,
	}
	fn := funcBody(t, s, setNoMatchRowHeader)
	at := 0
	for _, step := range setNoMatchRowSteps {
		k := strings.Index(fn[at:], step)
		if k < 0 {
			t.Errorf("setNoMatchRow no longer carries %q, in that order: the message the criterion demands never reaches the table body, so a non-matching term empties it in silence", step)
			continue
		}
		at += k + len(step)
	}
	// An ordered path proves the row is BUILT and INSERTED. AC9's word is "visible",
	// and one inserted statement satisfies every step above and shows an operator
	// nothing: `tr.hidden = true` beside the appendChild leaves a table that empties
	// itself in silence on a non-matching term, which is the exact state this
	// criterion exists to end (impl-gate finding F20). So the function may take
	// exactly one node off the page - the message itself, when the term is cleared,
	// which is the other half of the criterion ("WHEN the term is cleared, the
	// previously matching rows SHALL return").
	if got := hidingStatements(fn); len(got) != 1 || got[0] != "existing.remove()" {
		t.Errorf("setNoMatchRow takes nodes off the page an operator reads: %v. AC9 requires a VISIBLE message, and the only node this function may remove is the message itself when the filter no longer matches nothing", got)
	}
	funcBodyIs(t, s, setNoMatchRowHeader, setNoMatchRowSteps)
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
	// ...and it has to reach BOTH tables. The criterion is written per table ("a
	// visible message in that table's body"), and every assertion above is
	// satisfied by an applyFilter that walks one of them: the history table would
	// go back to emptying itself in silence with this whole test green. So the set
	// it iterates is pinned inside applyFilter's own body, in order, the way
	// setNoMatchRow's construction path is.
	af := funcBody(t, s, "function applyFilter() {")
	at = 0
	for _, step := range []string{
		`const term = $("filter").value.trim().toLowerCase();`,
		`for (const body of [$("queue"), $("history")]) {`,
		`setNoMatchRow(body, term !== "" && loaded > 0 && shown === 0);`,
	} {
		k := strings.Index(af[at:], step)
		if k < 0 {
			t.Errorf("applyFilter no longer carries %q, in that order: the no-match message does not reach both of the tables the filter hides rows in", step)
			continue
		}
		at += k + len(step)
	}
}

// --- AC10: the degraded-state copy, verbatim and legible -----------------------

var htmlComment = regexp.MustCompile(`(?s)<!--.*?-->`)

// replaceBlock rewrites the contents of the page's single <style> or <script>
// element through fn. A missing element is FATAL rather than a pass-through: a
// stripper that silently skipped the block holding the comments would be no
// stripper at all, and the sweep built on it would go back to reading prose.
func replaceBlock(t *testing.T, page, openTag, closeTag string, fn func(string) string) string {
	t.Helper()
	i := strings.Index(page, openTag)
	j := strings.Index(page, closeTag)
	if i < 0 || j < 0 || j < i {
		t.Fatalf("the page has no %s ... %s block", openTag, closeTag)
	}
	i += len(openTag)
	return page[:i] + fn(page[i:j]) + page[j:]
}

// stripJSComments removes line and block comments from JavaScript source. String
// and template literals are copied through verbatim, so a `//` or a `/*` inside
// one of the page's own strings is never mistaken for a comment. A `/` opening
// neither `//` nor `/*` is copied through as itself, which carries the page's one
// regex literal (/^version=/) and any division unharmed.
//
// Each comment becomes a single space, so two tokens either side of one cannot be
// read as a single token by an assertion downstream.
func stripJSComments(t *testing.T, js string) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < len(js); {
		c := js[i]
		switch {
		case c == '/' && i+1 < len(js) && js[i+1] == '/':
			for i < len(js) && js[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		case c == '/' && i+1 < len(js) && js[i+1] == '*':
			k := strings.Index(js[i+2:], "*/")
			if k < 0 {
				t.Fatal("a /* block comment in the page's script is never closed")
			}
			i += 2 + k + 2
			b.WriteByte(' ')
		case c == '"' || c == '\'' || c == '`':
			b.WriteByte(c)
			i++
			closed := false
			for i < len(js) {
				if js[i] == '\\' && i+1 < len(js) {
					b.WriteString(js[i : i+2])
					i += 2
					continue
				}
				b.WriteByte(js[i])
				i++
				if js[i-1] == c {
					closed = true
					break
				}
			}
			if !closed {
				t.Fatalf("a string literal opened with %q in the page's script is never closed", string(c))
			}
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// stripPageComments returns the page with every comment removed: the HTML
// comments, the CSS comments inside <style>, and the JS line and block comments
// inside <script>.
//
// It exists because a whole-page strings.Contains reads PROSE ABOUT the page as if
// it were the page. The comment above nrNode() says `not recorded` and satisfied
// AC10's wording sweep on its own, so the node underneath it could stop saying it
// with every test in this package green (impl-gate finding F7). This file already
// drew that distinction once, for @keyframes - "a comment saying the page declares
// no keyframes is not a keyframes block"; this makes it once, for the whole page.
func stripPageComments(t *testing.T, page string) string {
	t.Helper()
	out := htmlComment.ReplaceAllString(page, " ")
	out = replaceBlock(t, out, "<style>", "</style>", func(css string) string {
		return cssComment.ReplaceAllString(css, " ")
	})
	return replaceBlock(t, out, "<script>", "</script>", func(js string) string {
		return stripJSComments(t, js)
	})
}

func pageWithoutComments(t *testing.T) string {
	t.Helper()
	return stripPageComments(t, string(indexHTML))
}

// The stripper's own proof. It is asserted against a FIXTURE rather than against
// the shipped page, so it stays a proof whatever comments the page happens to
// carry: each of the three comment syntaxes must go, and the code around them -
// including a string literal that itself contains `//` and `/*`, and the regex
// literal - must stay.
func TestPageComments_AreStrippedSoNoProseCanStandInForTheRender(t *testing.T) {
	fixture := `<!DOCTYPE html><!-- html: gone -->` +
		"<style>/* css: gone */\n  .nr { color:var(--muted); } /* tail: gone */</style>" +
		"<script>\n// line: gone\nconst a = 1; /* block: gone */\n" +
		`const keep = "a // b /* c */ kept";` + "\n" +
		`const re = j.replace(/^version=/, "");` + "\n</script>"
	got := stripPageComments(t, fixture)
	for _, gone := range []string{"html: gone", "css: gone", "tail: gone", "line: gone", "block: gone"} {
		if strings.Contains(got, gone) {
			t.Errorf("stripPageComments left the comment %q on the page:\n%s", gone, got)
		}
	}
	for _, kept := range []string{
		".nr { color:var(--muted); }",
		"const a = 1;",
		`const keep = "a // b /* c */ kept";`,
		`j.replace(/^version=/, "")`,
	} {
		if !strings.Contains(got, kept) {
			t.Errorf("stripPageComments removed page CODE it must keep: %q\n%s", kept, got)
		}
	}
	// And it bites on the real page, where the comments actually are.
	if strings.Contains(string(indexHTML), "<!--") && strings.Contains(pageWithoutComments(t), "<!--") {
		t.Error("an HTML comment survived the stripper on the shipped page")
	}
}

// degradedCopy is AC10's nine strings, each paired with the expression on the page
// that RENDERS it. Both halves are asserted, and both against the comment-stripped
// page.
//
// The wording half alone is not a proof, and `not recorded` is why: it occurs
// three times - in the comment above nrNode(), in nrNode() itself, and inside the
// unrelated "reason not recorded" string - so a whole-page substring stays green
// while nrNode(), the honest-absence node behind every nil outcome field, renders
// "n/a" instead. Stripping comments does not close that on its own either:
// "reason not recorded" is a real render of a superstring and satisfies the sweep
// by itself. Only pinning the render site does, which is the shape the other
// assertions in this file already have. `unknown` and `unavailable` are the same
// case one step weaker - `<i>unknown</i>` is prose in the page's own explanatory
// copy, and an unavailable card's fallback sentence carries "unavailable" past a
// reworded node - so all nine are pinned, not the one the finding named.
var degradedCopy = []struct{ text, renderedBy string }{
	// The two empty-table states are pinned at the APPEND, not at the call: a row
	// built and never put into the table shows an operator nothing, which is
	// finding F1's shape in AC10's position. What emptyRow does with the text is
	// pinned separately, below - see finding F17.
	{"Nothing queued.", `qbody.appendChild(emptyRow(5, "Nothing queued."));`},
	{"No history yet.", `hbody.appendChild(emptyRow(7, "No history yet."));`},
	{"unavailable", `mk("span", "nr", "unavailable")`},
	{"not recorded", `mk("span", "nr", "not recorded")`},
	{"unknown", `mk("span", "nr", "unknown")`},
	{"this view is capped", `+ total.toLocaleString() + " — this view is capped.";`},
	{"reconnecting…", `conn.textContent = "reconnecting…";`},
	{"control disabled — set server_auth_token on the server.", `msg.textContent = "control disabled — set server_auth_token on the server.";`},
	{"unauthorized — check the control token.", `msg.textContent = "unauthorized — check the control token.";`},
}

func TestDegradedStates_CopySurvivesVerbatimAndStaysLegibleInBothThemes(t *testing.T) {
	s := pageWithoutComments(t)
	for _, c := range degradedCopy {
		if !strings.Contains(s, c.text) {
			t.Errorf("the degraded-state wording %q no longer appears on the page", c.text)
		}
		if !strings.Contains(s, c.renderedBy) {
			t.Errorf("the degraded-state wording %q is no longer rendered by %q: the page may still carry those words somewhere, but the node that shows them to an operator has stopped saying them",
				c.text, c.renderedBy)
		}
	}
	// AC10 says the page shall RENDER those words, and for the two empty-table
	// states the expression pinned above is the CALL. Nothing pinned what emptyRow
	// does with the argument, so `const td = mk("td", "empty", "")` rendered an
	// empty queue and an empty history each as a BLANK row - the operator shown a
	// table that says nothing when there is nothing to say - with both call sites
	// reading exactly as pinned (impl-gate finding F17). That is finding F1's
	// build-then-do-not-show shape one level further down the call, and it is
	// closed the same way it was closed for AC9: emptyRow's own body is asserted as
	// an ORDERED construction path, so dropping any step reds, and so does building
	// the row after inserting it.
	const emptyRowHeader = "function emptyRow(cols, text) {"
	emptyRowSteps := []string{
		`const tr = document.createElement("tr");`,
		`tr.dataset.empty = "1";`,
		`const td = mk("td", "empty", text);`,
		`td.colSpan = cols;`,
		`tr.appendChild(td);`,
		`return tr;`,
	}
	er := funcBody(t, s, emptyRowHeader)
	at := 0
	for _, step := range emptyRowSteps {
		k := strings.Index(er[at:], step)
		if k < 0 {
			t.Errorf("emptyRow no longer carries %q, in that order: the wording its callers hand it never reaches a cell in the table, so an empty queue and an empty history each render a row that says nothing", step)
			continue
		}
		at += k + len(step)
	}
	// Every pin so far - here, in AC9's test, and in degradedCopy above - stops at
	// the function that BUILDS the node. Four one-line edits satisfy all of them and
	// show an operator nothing (impl-gate finding F20), and this is where each is
	// closed.
	//
	// (a) mk() is the leaf that writes the text for emptyRow, for setNoMatchRow and
	// for nrNode alike. `if (text != null) e.textContent = "";` renders `Nothing
	// queued.`, `No history yet.`, `unavailable`, `not recorded`, `unknown` and AC9's
	// whole no-match sentence as EMPTY cells while every call site above reads
	// exactly as pinned - finding F17's own diagnosis, "the pins prove the CALL SITE,
	// not the render", one call deeper, at the single function all of them delegate
	// the render to. So mk's own body is pinned as an ordered path, the way
	// emptyRow's and setNoMatchRow's are.
	const mkHeader = "function mk(tag, cls, text) {"
	mkSteps := []string{
		`const e = document.createElement(tag);`,
		`if (cls) e.className = cls;`,
		`if (text != null) e.textContent = text;`,
		`return e;`,
	}
	mkBody := funcBody(t, s, mkHeader)
	at = 0
	for _, step := range mkSteps {
		k := strings.Index(mkBody[at:], step)
		if k < 0 {
			t.Errorf("mk no longer carries %q, in that order: mk is the one node factory every degraded state on this page renders through, so the wording each of them is handed reaches no operator", step)
			continue
		}
		at += k + len(step)
	}
	// (b, c, d) Construction is pinned; VISIBILITY was not. `tr.hidden = true` in
	// emptyRow or in mk hides both empty-table states, and capNote's `total > shown`
	// branch writing the notice and then hiding it takes `this view is capped` away
	// from an operator entirely - so a truncated view reads as a complete one, which
	// is the half of AC9's message this criterion also pins.
	for _, fn := range []struct{ name, body string }{
		{"mk", mkBody},
		{"emptyRow", er},
	} {
		if got := hidingStatements(fn.body); len(got) > 0 {
			t.Errorf("%s takes the node carrying a degraded state off the page an operator reads (%v); AC10's verb is RENDER, and a row built, filled and appended exactly as pinned shows nobody anything once it is hidden", fn.name, got)
		}
	}
	const capNoteHeader = "function capNote(id, shown, total) {"
	capNoteSteps := []string{
		`const el = $(id);`,
		`if (total > shown) {`,
		`el.textContent = "Showing the most recent " + shown.toLocaleString() + " of "`,
		`+ total.toLocaleString() + " — this view is capped.";`,
		`el.hidden = false;`,
		`} else {`,
		`el.textContent = "";`,
		`el.hidden = true;`,
		`}`,
	}
	cn := funcBody(t, s, capNoteHeader)
	at = 0
	for _, step := range capNoteSteps {
		k := strings.Index(cn[at:], step)
		if k < 0 {
			t.Errorf("capNote no longer carries %q, in that order: `this view is capped` is written into its element and never shown, so a truncated view reads as the whole ledger", step)
			continue
		}
		at += k + len(step)
	}
	if i, j := strings.Index(cn, "if (total > shown) {"), strings.Index(cn, "} else {"); i >= 0 && j > i {
		if got := hidingStatements(cn[i:j]); len(got) > 0 {
			t.Errorf("capNote hides the row-cap notice on the branch that exists to SHOW it (%v), so `this view is capped` never reaches an operator", got)
		}
	}
	// The three bodies whole, not a subsequence of each - see funcBodyIs. This is the
	// backstop under every ordered sweep above: an inserted statement satisfies all
	// of them, and one inserted statement is the whole of finding F20.
	funcBodyIs(t, s, mkHeader, mkSteps)
	funcBodyIs(t, s, emptyRowHeader, emptyRowSteps)
	funcBodyIs(t, s, capNoteHeader, capNoteSteps)
	// ...and each of those functions is REACHED. A function that renders the wording
	// perfectly renders nothing at all when nobody calls it, which is finding F1's
	// build-then-do-not-show shape one call further out; the two empty-table states
	// are pinned at their appendChild for exactly that reason, and the row-cap notice
	// and the honest-absence node are pinned here for it. degradedCopy pins the
	// expression INSIDE capNote, which survives both call sites being deleted.
	for _, call := range []struct{ what, expr string }{
		{"the queue's row-cap notice", `capNote("queue-cap", q.length, sumStatuses(sum, QUEUE_STATUSES));`},
		{"the history's row-cap notice", `capNote("hist-cap", h.length, sumStatuses(sum, TERMINAL_STATUSES));`},
	} {
		if !strings.Contains(s, call.expr) {
			t.Errorf("%s is never rendered: the page no longer carries %q, so capNote may write `this view is capped` exactly as pinned and no operator ever sees it", call.what, call.expr)
		}
	}
	if n := strings.Count(s, "appendChild(nrNode())"); n < 5 {
		t.Errorf("the honest-absence node is appended at %d sites; the page reports a nil size, VMAF, encoder, duration and aggregate that way, so `not recorded` stops reaching an operator wherever one of those calls is dropped", n)
	}
	rules := allRules(t)
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
	// ...and no rule paints one of those states out of existence. Hiding a degraded
	// state from the STYLESHEET is finding F20's shape one language to the left: the
	// row is built, filled and appended exactly as every pin above requires, its
	// colours clear their floor, and `.empty { display:none }` means nobody ever
	// reads it. The rules that can reach each state's node are decided by the
	// selector, not by its text, for the reason finding F21 gave about AC6's regions.
	for _, r := range rules {
		for _, st := range states {
			if !stateIsReachedBy(r.sel, st.sel) {
				continue
			}
			for _, d := range r.decls {
				bad := invisibleValues[unprefixed(d.prop)]
				v := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(normSpace(d.value), "!important", "")))
				if bad[strings.TrimSpace(v)] {
					t.Errorf("%s { %s: %s } reaches the node that renders %s and takes it off the page; AC10's verb is RENDER, so a state painted to a floor nobody can see is a state that is not reported",
						r.sel, d.prop, d.value, st.state)
				}
			}
		}
	}
}
