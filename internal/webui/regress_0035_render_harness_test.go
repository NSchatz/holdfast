package webui

// regress_0035_render_harness: a RENDERED oracle for the acceptance criteria that
// are about what the page paints.
//
// Every grader committed on this branch decides its criterion by parsing the CSS
// (or the JS) TEXT of the embedded page. AC3, AC4, AC5, AC6, AC7, AC8, AC9 and
// AC10 are properties of a RENDER - which rule wins the cascade, what a custom
// property resolves to at the element that uses it, what box a control actually
// occupies, whether a string an operator must read is on the screen. This file
// answers those questions by loading the served page in the container's own
// browser (/usr/bin/chromium, headless) and reading the values the engine
// computes.
//
// The harness serves four things from ONE origin, so the frame under test and its
// driver are same-origin and the page's own Content-Security-Policy still applies
// to the page being graded:
//
//	/           the page under test, with webui.Handler()'s exact headers and CSP
//	/probe      the driver (no CSP), which hosts / in an iframe of a chosen CSS
//	            width, waits for the snapshot to render, measures, and POSTs
//	/api/events an SSE stream carrying one snapshot, so the queue, the history,
//	            the aggregates and the cap notices are really drawn
//	/result     the driver's POST target; the Go side blocks on it
//
// Nothing here mutates the checkout. A mutated page is passed to render() as
// bytes, and the committed grader set is run against the same mutation in a CHILD
// process through renderMutEnv, exactly as the ordinal-1..5 artifacts do.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// chromiumBin is the browser every rendered grader in this package drives. It is
// /usr/bin/chromium here; renderBrowserPath (webui_test.go) tries that first and
// then the names the same binary carries on other hosts, so the committed
// rendered graders are not pinned to one container's layout. It is deliberately
// NOT a fall-back to "grade it from the CSS text instead": render() fails naming
// this path when no browser is found, because a criterion about what the page
// draws is UNGRADED without one.
var chromiumBin = renderBrowserPath()

// --- the mutations, and the child process that carries one --------------------

const renderMutEnv = "S0035_RENDER_MUT"

type renderMutation struct {
	criterion string
	old, new  string
	why       string
}

// renderHoles are edits that a RENDERED reading of the page shows to violate their
// criterion. Each is proved twice below: the browser is shown the violation, and
// the whole committed grader set is shown to stay green on it.
var renderHoles = map[string]renderMutation{
	"F27a-token-redefined-below-root-repaints-the-page": {
		criterion: "AC1, AC3",
		old:       "</style>",
		new:       "  body { --fg:var(--line); }\n</style>",
		why: "--fg is redefined on body, so every element that inherits it paints its text in --line " +
			"(#252a34 on #0f1115, 1.09:1) - while themeTokens reads the token set from the :root blocks " +
			"ALONE and every pair in the contrast derivation is still measured at the :root value",
	},
	"F27b-token-redefined-below-root-shrinks-a-target": {
		criterion: "AC7",
		old:       "</style>",
		new:       "  .controls { --target-min:8px; --sp-3:0px; --fs-md:8px; }\n</style>",
		why: "--target-min, --sp-3 and --fs-md are redefined on the control rows, so every button and " +
			"input inside them RENDERS under WCAG 2.2 2.5.8's 24px floor, while the sweep resolves " +
			"var(--target-min) from :root and reads 32px",
	},
	"F28a-single-column-undone-by-the-display-property": {
		criterion: "AC6",
		old:       "</style>",
		new: "  @media (max-width: 640px) {\n" +
			"    .controls { display:grid; grid-template-columns:1fr 1fr; }\n" +
			"    #aggregates { display:flex; }\n" +
			"  }\n</style>",
		why: "at 360px the control rows lay out in TWO columns and the aggregate cards in a ROW, because " +
			"`display` decides which of flex-direction and grid-template-columns is even live - and the " +
			"region loop reads only the property it expects, which both rules leave exactly as it was",
	},
	"F28b-body-scrolls-sideways-with-no-width-declared": {
		criterion: "AC6",
		old:       "</style>",
		new:       "  section .note { white-space:nowrap; }\n</style>",
		why: "the section prose is laid out on one unwrapped line, so the page BODY scrolls sideways at " +
			"360px - the harm AC6 names in its own words - through a property that is neither a width " +
			"nor a minimum width, which is all the sweep reads",
	},
	"F29a-degraded-state-hidden-by-its-own-id": {
		criterion: "AC10",
		old:       "</style>",
		new:       "  #queue-cap { display:none; }\n</style>",
		why: "`this view is capped` is written into the queue's cap notice and painted off the page by a " +
			"rule addressing that very node by the id the page gives it - and stateIsReachedBy discards " +
			"every id-carrying compound, because the state it compares against (`.note.cap`) has none",
	},
	"F29b-degraded-state-hidden-by-an-ancestor": {
		criterion: "AC9, AC10",
		old:       "</style>",
		new:       "  .tablewrap { display:none; }\n</style>",
		why: "`Nothing queued.`, `No history yet.` and AC9's whole no-match sentence are built, filled, " +
			"appended and never seen, because display:none on an ANCESTOR takes the subtree off the page " +
			"and the invisible-value sweep only ever asks about rules reaching the state's OWN node",
	},
}

func init() {
	name := os.Getenv(renderMutEnv)
	if name == "" {
		return
	}
	m, ok := renderHoles[name]
	if !ok {
		panic("regress_0035 render artifacts: no case named " + name)
	}
	indexHTML = applyHole(string(indexHTML), m)
}

func applyHole(s string, m renderMutation) []byte {
	if !strings.Contains(s, m.old) {
		panic("regress_0035 render artifacts: mutation anchor absent from index.html: " + m.old)
	}
	return []byte(strings.Replace(s, m.old, m.new, 1))
}

// renderIsChild reports whether this process already carries a mutated page, put
// there by this loop's harness or by any earlier one.
func renderIsChild() bool { return os.Getenv(renderMutEnv) != "" || v5IsChild() }

// mutantPage returns the shipped page with one hole cut in it, for the browser.
func mutantPage(t *testing.T, name string) []byte {
	t.Helper()
	m, ok := renderHoles[name]
	if !ok {
		t.Fatalf("no case named %s", name)
	}
	return applyHole(string(indexHTML), m)
}

// committedSetStaysGreen runs the WHOLE committed grader set against the mutated
// page in a child process and fails when nothing reds. It is the second half of
// every finding here: the browser has already shown the criterion violated, so a
// green suite is the criterion going ungraded.
func committedSetStaysGreen(t *testing.T, name string) {
	t.Helper()
	m := renderHoles[name]
	cmd := exec.Command(os.Args[0], "-test.run", v5Run, "-test.v")
	cmd.Env = append(os.Environ(), renderMutEnv+"="+name)
	b, err := cmd.CombinedOutput()
	out := string(b)
	if !strings.Contains(out, "=== RUN   Test") {
		t.Fatalf("the child ran no committed test at all, so nothing was graded\n%s", out)
	}
	if err == nil {
		t.Errorf("%s is ungraded here: every committed test in this package stayed GREEN on a page the browser has just shown violates it, where %s\nmutation: %q -> %q",
			m.criterion, m.why, m.old, m.new)
	}
}

// --- driving the browser -------------------------------------------------------

// renderCase is one rendered measurement: a page, a viewport width, browser-level
// media emulation, and the JS that measures the result.
type renderCase struct {
	page          []byte // nil = the page as shipped
	width, height int
	// chromiumArgs are appended to the launch line; this is how a browser-level
	// user preference (prefers-color-scheme, prefers-reduced-motion) is set,
	// because those are properties of the user agent and not of the document.
	chromiumArgs []string
	// snapshot is the JSON pushed on the SSE stream as the `snapshot` event.
	snapshot string
	// prelude is JS run against the loaded page before the measurement, with (w, d)
	// bound to the page-under-test's window and document.
	prelude string
	// measure is a JS function body with the same bindings; it returns any
	// JSON-serialisable value.
	measure string
}

// darkUA and reduceUA are the two user preferences the criteria name. Proved,
// not assumed: TestRegress0035_RenderHarness_TheBrowserAnswersTheQuestionsWeAsk
// reads them back off matchMedia inside the page under test.
var (
	darkUA   = []string{"--force-dark-mode"}
	reduceUA = []string{"--force-prefers-reduced-motion"}
)

// defaultSnapshot draws every state the degraded-copy criteria name: a queue row
// encoding with no progress figure ("unknown"), a done history row with no
// recorded VMAF ("not recorded"), summary counts ABOVE the shipped row counts so
// both cap notices fire ("this view is capped"), and one unavailable aggregate.
const defaultSnapshot = `{
  "now": 1757200000,
  "paused": false,
  "scanning": false,
  "bytes_reclaimed_lifetime": 123456789,
  "bytes_reclaimed_session": 4096,
  "summary": {"pending": 40, "probing": 1, "encoding": 1, "verifying": 0,
              "done": 90, "skipped": 5, "failed": 5},
  "queue": [
    {"path": "/media/alpha.mkv", "status": "encoding", "updated_at": 1757199900, "worker": "w1"}
  ],
  "history": [
    {"path": "/media/bravo.mkv", "status": "done", "updated_at": 1757199000,
     "source_bytes": 200, "output_bytes": 100, "encoder": "cpu", "encode_ms": 1000}
  ],
  "aggregates": {
    "outcomes": {"available": true, "counted": 100, "covers": "every terminal row",
                 "buckets": [{"key": "done", "count": 90}]},
    "size_ratio": {"available": false, "unavailable": "the ledger could not be read",
                   "covers": "every done row"},
    "encode_ms": {"available": true, "counted": 0, "covers": "every done row"}
  }
}`

// emptySnapshot draws the two empty-table states.
const emptySnapshot = `{
  "now": 1757200000, "paused": false, "scanning": false,
  "bytes_reclaimed_lifetime": 0, "bytes_reclaimed_session": 0,
  "summary": {}, "queue": [], "history": [], "aggregates": {}
}`

// probePage is the driver. It is served WITHOUT a CSP (it is the instrument, not
// the artefact) and touches the page under test only through same-origin reads.
const probePage = `<!doctype html>
<html><head><meta charset="utf-8"><title>probe</title>
<style>html,body{margin:0;padding:0;background:#777}</style></head>
<body>
<iframe id="f" src="/" style="border:0;display:block;width:__W__px;height:__H__px"></iframe>
<script>
var sent = false;
function post(o) {
  if (sent) { return; }
  sent = true;
  fetch("/result", {method: "POST", body: JSON.stringify(o)});
}
window.onerror = function (m, s, l) { post({harnessError: String(m) + " @" + s + ":" + l}); };
setTimeout(function () { post({harnessError: "timed out waiting for the page to render"}); }, 25000);
function prelude(w, d) {
__PRELUDE__
}
function measure(w, d) {
__MEASURE__
}
var f = document.getElementById("f");
f.addEventListener("load", function () {
  var w = f.contentWindow, dd = f.contentDocument;
  var tries = 0;
  (function wait() {
    tries++;
    var conn = dd.getElementById("conn");
    var chips = dd.getElementById("chips");
    var drawn = conn && conn.textContent === "live" && chips && chips.children.length > 0;
    if (!drawn && tries < 400) { setTimeout(wait, 20); return; }
    if (!drawn) { post({harnessError: "the snapshot never rendered"}); return; }
    try { prelude(w, dd); } catch (e) { post({harnessError: "prelude: " + String((e && e.stack) || e)}); return; }
    w.requestAnimationFrame(function () {
      w.requestAnimationFrame(function () {
        try { post({ok: measure(w, dd)}); } catch (e) { post({harnessError: String((e && e.stack) || e)}); }
      });
    });
  })();
});
</script></body></html>`

type probeResult struct {
	OK           json.RawMessage `json:"ok"`
	HarnessError string          `json:"harnessError"`
}

// render loads a page in chromium and returns what `measure` computed.
func render(t *testing.T, c renderCase) json.RawMessage {
	t.Helper()
	if _, err := os.Stat(chromiumBin); err != nil {
		t.Fatalf("this grader needs a browser: %s is not available (%v). A criterion about what the page RENDERS cannot be graded from CSS text; install a browser and re-run.", chromiumBin, err)
	}
	if c.width == 0 {
		c.width = 1200
	}
	if c.height == 0 {
		c.height = 900
	}
	if c.snapshot == "" {
		c.snapshot = defaultSnapshot
	}
	if c.prelude == "" {
		c.prelude = "return;"
	}
	page := c.page
	if page == nil {
		page = indexHTML
	}

	results := make(chan []byte, 4)
	driver := strings.NewReplacer(
		"__W__", fmt.Sprint(c.width),
		"__H__", fmt.Sprint(c.height),
		"__PRELUDE__", c.prelude,
		"__MEASURE__", c.measure,
	).Replace(probePage)

	mux := http.NewServeMux()
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, driver)
	})
	mux.HandleFunc("/result", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case results <- b:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		_, _ = fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", strings.Join(strings.Fields(c.snapshot), " "))
		flusher.Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	})
	// The page under test, with the headers webui.Handler() sends - the CSP and
	// the Trusted Types enforcement included, so nothing here grades a page that
	// is served more permissively than the real one.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, r)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		_, _ = w.Write(page)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	args := []string{
		"--headless=new",
		"--no-sandbox",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--disable-component-update",
		"--hide-scrollbars",
		"--user-data-dir=" + t.TempDir(),
		fmt.Sprintf("--window-size=%d,%d", c.width+500, c.height+200),
	}
	args = append(args, c.chromiumArgs...)
	args = append(args, srv.URL+"/probe")

	cmd := exec.CommandContext(ctx, chromiumBin, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("could not start %s: %v", chromiumBin, err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	select {
	case b := <-results:
		var pr probeResult
		if err := json.Unmarshal(b, &pr); err != nil {
			t.Fatalf("the probe returned unparseable JSON: %v\n%s", err, b)
		}
		if pr.HarnessError != "" {
			t.Fatalf("the probe failed in the browser: %s", pr.HarnessError)
		}
		return pr.OK
	case <-ctx.Done():
		t.Fatalf("the browser never reported a result\nchromium stderr (tail):\n%s", tail(stderr.String(), 2000))
	}
	return nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// renderInto runs a case and decodes its result into v.
func renderInto(t *testing.T, c renderCase, v any) {
	t.Helper()
	raw := render(t, c)
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("could not decode the probe result: %v\n%s", err, raw)
	}
}

// --- shared measurement JS -----------------------------------------------------

// jsHelpers is prepended to every measurement: sRGB relative luminance and WCAG
// contrast over the colours the ENGINE computed, the real painted surface behind
// an element (walked up the box tree until an opaque background is found, which is
// what a browser does and what a --paints-on annotation only claims), a short
// selector for reporting, and the text an operator can actually SEE.
const jsHelpers = `
  var rgbOf = function (s) {
    var m = String(s).match(/[-\d.]+/g);
    if (!m) { return null; }
    return [ +m[0], +m[1], +m[2], m.length > 3 ? +m[3] : 1 ];
  };
  var lin = function (v) { v /= 255; return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); };
  var lum = function (c) { return 0.2126 * lin(c[0]) + 0.7152 * lin(c[1]) + 0.0722 * lin(c[2]); };
  var over = function (fg, bg) {
    var a = fg[3];
    return [fg[0] * a + bg[0] * (1 - a), fg[1] * a + bg[1] * (1 - a), fg[2] * a + bg[2] * (1 - a), 1];
  };
  var contrast = function (a, b) {
    var la = lum(a), lb = lum(b), hi = Math.max(la, lb), lo = Math.min(la, lb);
    return (hi + 0.05) / (lo + 0.05);
  };
  var surfaceOf = function (el) {
    var n = el, acc = [255, 255, 255, 1], stack = [];
    while (n && n.nodeType === 1) { stack.push(n); n = n.parentElement; }
    for (var i = stack.length - 1; i >= 0; i--) {
      var c = rgbOf(w.getComputedStyle(stack[i]).backgroundColor);
      if (c && c[3] > 0) { acc = over(c, acc); }
    }
    return acc;
  };
  var sel = function (el) {
    var s = el.tagName.toLowerCase();
    if (el.id) { s += "#" + el.id; }
    if (el.className && typeof el.className === "string" && el.className.trim()) {
      s += "." + el.className.trim().split(/\s+/).join(".");
    }
    return s;
  };
  var isShown = function (el) {
    if (!el || el.nodeType !== 1) { return false; }
    if (el.closest(".visually-hidden")) { return false; }
    var cs = w.getComputedStyle(el);
    if (cs.visibility !== "visible" || cs.display === "none") { return false; }
    if (el.offsetParent === null && cs.position !== "fixed" && el.tagName !== "BODY") { return false; }
    var r = el.getBoundingClientRect();
    return r.width > 0 && r.height > 0;
  };
  var shownText = function () {
    var out = [];
    var walk = d.createTreeWalker(d.body, w.NodeFilter.SHOW_TEXT, null);
    var n;
    while ((n = walk.nextNode())) {
      if (!n.nodeValue.trim()) { continue; }
      if (!isShown(n.parentElement)) { continue; }
      out.push(n.nodeValue);
    }
    return out.join(" ").replace(/\s+/g, " ");
  };
`

// --- the emulation switches, proved rather than assumed ------------------------

type mediaProbe struct {
	Dark   bool    `json:"dark"`
	Light  bool    `json:"light"`
	Reduce bool    `json:"reduce"`
	BodyBg string  `json:"bodyBg"`
	Width  float64 `json:"width"`
}

const mediaMeasure = `
  var mm = function (q) { return w.matchMedia(q).matches; };
  return {
    dark:   mm("(prefers-color-scheme: dark)"),
    light:  mm("(prefers-color-scheme: light)"),
    reduce: mm("(prefers-reduced-motion: reduce)"),
    bodyBg: w.getComputedStyle(d.body).backgroundColor,
    width:  d.documentElement.clientWidth
  };
`

// TestRegress0035_RenderHarness_TheBrowserAnswersTheQuestionsWeAsk proves the
// instrument before anything is measured with it: that the two user preferences
// AC4 and AC5 are written in terms of really change inside the page under test,
// that the viewport really is the width the case asks for, and that the page
// really does repaint from the other token set when the preference flips. A
// launch flag that silently did nothing would otherwise make every "in both
// themes" measurement below a measurement of one theme twice.
func TestRegress0035_RenderHarness_TheBrowserAnswersTheQuestionsWeAsk(t *testing.T) {
	if renderIsChild() {
		t.Skip("child run: this process carries a mutated page")
	}
	var light, dark, reduce mediaProbe
	renderInto(t, renderCase{width: 900, measure: mediaMeasure}, &light)
	renderInto(t, renderCase{width: 900, chromiumArgs: darkUA, measure: mediaMeasure}, &dark)
	renderInto(t, renderCase{width: 360, chromiumArgs: reduceUA, measure: mediaMeasure}, &reduce)

	if !light.Light || light.Dark {
		t.Errorf("the default user agent reports light=%v dark=%v; the light theme cannot be graded", light.Light, light.Dark)
	}
	if !dark.Dark || dark.Light {
		t.Errorf("--force-dark-mode reports light=%v dark=%v; the dark theme cannot be graded", dark.Light, dark.Dark)
	}
	if !reduce.Reduce {
		t.Error("--force-prefers-reduced-motion does not make (prefers-reduced-motion: reduce) match; AC5 cannot be graded")
	}
	if light.BodyBg == dark.BodyBg {
		t.Errorf("the page paints the same body background (%s) under both colour preferences, so the two themes are not being told apart", light.BodyBg)
	}
	if got := reduce.Width; got != 360 {
		t.Errorf("the page under test reports a %gpx viewport where the case asked for 360; AC6 cannot be graded", got)
	}
	t.Logf("light: bodyBg=%s | dark: bodyBg=%s | 360px viewport: width=%g reduce=%v",
		light.BodyBg, dark.BodyBg, reduce.Width, reduce.Reduce)
}
