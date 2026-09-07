package webui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/sourceoffer"
	"github.com/NSchatz/holdfast/internal/version"
)

// The RENDERED-PAGE grader. AC1 says the source offer is "visible without
// interaction" and AC11 says the source URL "introduces no additional element,
// attribute or script into the served document". Both are claims about a DOCUMENT A
// BROWSER RENDERED, and no amount of string matching over HTML or CSS text can settle
// either: a text grader cannot decide which rule wins the cascade, cannot tell an
// attribute from the same characters shown as text, and cannot say whether an element
// with a computed style ended up on the screen. So this file asks a real browser.
//
// It costs no module dependency, which the spec puts out of scope: it drives the
// browser this container already ships (`chromium --headless --dump-dom`) with
// os/exec, and the measuring instrument is a probe page the TEST serves - never the
// shipped document, which is fetched and rendered exactly as a user's browser would
// fetch it, tight Content-Security-Policy and all. The probe loads the dashboard in a
// same-origin iframe, reads getComputedStyle and the layout geometry of the offer and
// every one of its ancestors, hit-tests the offer's centre point, counts the elements
// the value could have introduced, and writes a JSON verdict that --dump-dom returns.
//
// The byte-level assertions in sourceoffer_test.go remain the STANDING coverage
// inside `make check` on every machine; this grader SKIPS where chromium is absent,
// for the same reason AC13's docker observation is a manual one-off - `make check` is
// this repo's gate and must not red on a machine that lacks a browser.

// probeJS runs inside the probe page. It reaches into the same-origin iframe holding
// the real served document and reports what the browser actually did with it.
const probeJS = `
function ancestors(el) { const out = []; for (let n = el; n; n = n.parentElement) out.push(n); return out; }
function verdict(doc, win) {
  const el = doc.querySelector("p.source-offer");
  if (!el) return { found: false };
  const link = el.querySelector("a");
  const chain = ancestors(el).map(function (n) {
    const cs = win.getComputedStyle(n);
    return { tag: n.tagName.toLowerCase(), display: cs.display, visibility: cs.visibility,
             opacity: cs.opacity, hidden: n.hasAttribute("hidden") };
  });
  // Bring it into the viewport before hit-testing: elementFromPoint answers only
  // for coordinates inside the viewport, and the offer lives in the page footer.
  // Scrolling is not "interaction" in the sense the predicate forbids - a <details>
  // to open, a hidden attribute, a display:none - and the predicate makes no
  // above-the-fold claim. What the hit test settles is whether anything is PAINTED
  // OVER the offer, which no scan of rule text can decide.
  try { el.scrollIntoView({ block: "center" }); } catch (e) { el.scrollIntoView(); }
  const r = el.getBoundingClientRect();
  const cx = r.left + r.width / 2, cy = r.top + r.height / 2;
  const hit = doc.elementFromPoint(cx, cy);
  return {
    found: true,
    // What the offer SHOWS, after the cascade and after every script on the page ran.
    text: el.textContent,
    linkText: link ? link.textContent : null,
    linkHref: link ? link.getAttribute("href") : null,
    // Is it on the screen? Geometry, the computed style of every ancestor, whether
    // any ancestor is inside a <details>, and a hit test at the offer's own centre.
    width: r.width, height: r.height,
    offsetParent: el.offsetParent !== null,
    inDetails: !!el.closest("details"),
    chain: chain,
    hitIsSelfOrDescendant: !!hit && (hit === el || el.contains(hit)),
    // What the value could have introduced. Counted over the WHOLE document, so an
    // element or handler smuggled in anywhere is visible here.
    elementCount: doc.getElementsByTagName("*").length,
    imgCount: doc.getElementsByTagName("img").length,
    scriptCount: doc.getElementsByTagName("script").length,
    offerAttrs: Array.prototype.map.call(el.attributes, function (a) { return a.name; }).sort(),
    linkAttrs: link ? Array.prototype.map.call(link.attributes, function (a) { return a.name; }).sort() : null,
    offerChildTags: Array.prototype.map.call(el.children, function (c) { return c.tagName.toLowerCase(); }),
    onerrorAttrs: doc.querySelectorAll("[onerror],[onload],[onclick]").length
  };
}
`

// probePage is the harness document. It is served by the TEST, never shipped: the
// shipped document is what it loads into the iframe, unmodified, over HTTP.
//
// The verdict is computed SYNCHRONOUSLY in this page's own `load` handler and POSTed
// straight back to the test's own server. Both halves of that are load-bearing.
//
// Synchronous, in `load`: the parent's load event cannot fire until the iframe has
// loaded, so the iframe's document is there by then, and getBoundingClientRect forces
// layout, so geometry is settled when it is read. No timer, no --virtual-time-budget.
//
// POSTed rather than read out of --dump-dom: the browser then has no say in WHEN the
// measurement is available. Two CI runs were lost to exactly that - the runner's Chrome
// never returned from --dump-dom and was killed at the deadline, twice, for two
// different reasons (virtual time that could not advance while the dashboard's own
// EventSource was reconnecting, and then load-completion semantics that differ from the
// local browser's). The Go test now owns the deadline: it waits for the POST, and a
// browser that never sends one fails the test with its stderr attached.
// The one wrinkle the dashboard graders added to it: the shipped page fills its tables
// from an SSE snapshot, which lands AFTER load, so a verdict computed once in `load` can
// be computed before its subject exists. A probe may therefore report `ready: false` and
// be retried on a plain setTimeout - still no virtual time, and the TEST still owns the
// outer deadline. A probe that never sets `ready` (the source-offer graders below) posts
// on the first attempt, exactly as it always did.
const probePage = `<!DOCTYPE html><html><head><meta charset="utf-8"><title>probe</title></head>
<body><iframe id="f" src="%SRC%" width="1200" height="900" style="border:0"></iframe>
<script>
%JS%
window.addEventListener("load", function () {
  const f = document.getElementById("f");
  const deadline = Date.now() + %WAIT%;
  let sent = false;
  function post(v) {
    if (sent) return;
    sent = true;
    fetch("/verdict", { method: "POST", body: JSON.stringify(v) });
  }
  function attempt() {
    let v;
    try { v = verdict(f.contentDocument, f.contentWindow); }
    catch (e) { v = { error: String(e) }; }
    if (v && v.ready === false && Date.now() < deadline) { setTimeout(attempt, 50); return; }
    post(v);
  }
  attempt();
});
</script></body></html>`

type renderVerdict struct {
	Found                 bool     `json:"found"`
	Error                 string   `json:"error"`
	Text                  string   `json:"text"`
	LinkText              string   `json:"linkText"`
	LinkHref              string   `json:"linkHref"`
	Width                 float64  `json:"width"`
	Height                float64  `json:"height"`
	OffsetParent          bool     `json:"offsetParent"`
	InDetails             bool     `json:"inDetails"`
	HitIsSelfOrDescendant bool     `json:"hitIsSelfOrDescendant"`
	ElementCount          int      `json:"elementCount"`
	ImgCount              int      `json:"imgCount"`
	ScriptCount           int      `json:"scriptCount"`
	OnerrorAttrs          int      `json:"onerrorAttrs"`
	OfferAttrs            []string `json:"offerAttrs"`
	LinkAttrs             []string `json:"linkAttrs"`
	OfferChildTags        []string `json:"offerChildTags"`
	Chain                 []struct {
		Tag        string `json:"tag"`
		Display    string `json:"display"`
		Visibility string `json:"visibility"`
		Opacity    string `json:"opacity"`
		Hidden     bool   `json:"hidden"`
	} `json:"chain"`
}

// requiredMode reports whether a runtime this package needs and cannot find is a FAILURE
// rather than a skip. `make check` is the repository gate and must stay green on a
// machine with no browser (exactly as it must on one with no docker), so it leaves this
// unset; `make webui-check` sets it, which is what makes that target unable to come back
// green without having measured anything.
func requiredMode() bool { return os.Getenv("HOLDFAST_WEBUI_REQUIRED") == "1" }

// missingRuntime is the ONE place the skip-or-fail decision is made, so the two modes
// cannot drift apart. Either way the message NAMES the runtime that was not there.
func missingRuntime(t *testing.T, runtime, need string) {
	t.Helper()
	if requiredMode() {
		t.Fatalf("required runtime %q is not on PATH: %s. "+
			"HOLDFAST_WEBUI_REQUIRED=1 (make webui-check) makes a missing runtime a failure, never a skip",
			runtime, need)
	}
	t.Skipf("no %q on PATH: %s. Run `make webui-check` to require it", runtime, need)
}

// chromium locates the browser or skips (fails, under required mode).
func chromium(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	missingRuntime(t, "chromium", "the rendered-page graders load the served document in a real browser engine, which no scan of source text can replace")
	return ""
}

// nodeRuntime locates node or skips (fails, under required mode). The dashboard's
// derivation units run in node's BUILT-IN test runner: no registry package, no lockfile.
func nodeRuntime(t *testing.T) string {
	t.Helper()
	if p, err := exec.LookPath("node"); err == nil {
		return p
	}
	missingRuntime(t, "node", "the dashboard's derivation units run in node's built-in test runner (node --test)")
	return ""
}

// probeServer is the test's own server: the REAL handler at "/", the probe page beside
// it, and the endpoint the probe POSTs its verdict back to.
type probeServer struct {
	url     string
	verdict chan []byte
}

// serveOpts is everything a grader can vary about the page under measurement. The
// DOCUMENT is always the one the real handler produces, under the real response headers;
// only the world around it moves.
type serveOpts struct {
	// url is the source-offer value the handler is built for.
	url string
	// mutate rewrites the served document bytes to build a counterexample. nil serves the
	// document exactly as it ships.
	mutate func([]byte) []byte
	// probe is the measuring script. probeJS when empty.
	probe string
	// wait is how long the probe retries a verdict that reports itself not ready.
	wait time.Duration
	// snapshot, when non-nil, is pushed to the page as a real SSE `snapshot` event on
	// /api/events, which is how the dashboard gets every value it renders.
	snapshot []byte
	// streamFails drops the event stream after the first snapshot and answers every
	// reconnection with 500, so the page's connection state must leave "live".
	streamFails bool
}

// serveDocument stands the REAL handler up on a real listener, plus the probe page
// beside it. mutate is applied to the served document bytes to build a counterexample
// (nil serves the document exactly as it ships).
func serveDocument(t *testing.T, url string, mutate func([]byte) []byte) *probeServer {
	t.Helper()
	return serveDocumentWith(t, serveOpts{url: url, mutate: mutate})
}

func serveDocumentWith(t *testing.T, o serveOpts) *probeServer {
	t.Helper()
	if o.probe == "" {
		o.probe = probeJS
	}
	if o.wait <= 0 {
		o.wait = 15 * time.Second
	}
	// An SSE `data:` field is ONE line. The fixtures are written readably, so they are
	// compacted here - and a fixture that is not valid JSON fails now, loudly, rather than
	// as a page that quietly never rendered.
	if o.snapshot != nil {
		var compact bytes.Buffer
		if err := json.Compact(&compact, o.snapshot); err != nil {
			t.Fatalf("the snapshot fixture is not valid JSON: %v\n%s", err, o.snapshot)
		}
		o.snapshot = compact.Bytes()
	}
	ps := &probeServer{verdict: make(chan []byte, 4)}
	real := HandlerFor(offerFor(o.url))
	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o.mutate == nil {
			real.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		real.ServeHTTP(rec, r)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(o.mutate(rec.Body.Bytes()))
	}))
	mux.HandleFunc("/probe", func(w http.ResponseWriter, _ *http.Request) {
		page := strings.Replace(probePage, "%JS%", o.probe, 1)
		page = strings.Replace(page, "%SRC%", "/", 1)
		page = strings.Replace(page, "%WAIT%", strconv.Itoa(int(o.wait/time.Millisecond)), 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
	mux.HandleFunc("/verdict", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		select {
		case ps.verdict <- b:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// The dashboard's own API calls. The read endpoints are answered so the page is not
	// reconnecting while it is being measured, and are deliberately NOT held open: a
	// hanging response would keep httptest.Server.Close waiting.
	for _, ep := range []string{"/api/summary", "/api/queue", "/api/history"} {
		mux.HandleFunc(ep, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{}`))
		})
	}

	// The event stream. With no snapshot this is the old behaviour (a JSON body, which the
	// page treats as a failed stream and does not render from). With one it is a REAL SSE
	// stream: the snapshot the dashboard renders every value from.
	//
	// The stream is held open until the test finishes, because a stream that ends is a
	// stream the page reports as down - which is a different criterion (B13) and would
	// otherwise contaminate every happy-path grader. `done` is closed BEFORE the server is
	// closed (t.Cleanup runs last-registered-first), so nothing is left hanging.
	var conns atomic.Int32
	done := make(chan struct{})
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		if o.snapshot == nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if o.streamFails && conns.Add(1) > 1 {
			http.Error(w, "the event stream is unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", o.snapshot)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if o.streamFails {
			return // drop it: the page must leave its connected state and keep its rows
		}
		select {
		case <-done:
		case <-r.Context().Done():
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(done) })
	ps.url = srv.URL
	return ps
}

// hermeticFlags make the browser a MEASURING INSTRUMENT rather than a desktop
// application. Everything here is either "do not reach the network" or "do not need a
// session bus": a CI runner's Chrome will otherwise register with GCM, poll the
// component updater and fail to find dbus, which is what hung the first version of
// this test on the runner. There is no --virtual-time-budget: see probePage.
//
// --enable-logging=stderr is not hermeticity, it is INSTRUMENTATION: it is what makes the
// engine write its console log - every Content-Security-Policy and Trusted Types refusal
// included - where the test can read it. B17 is graded on that log, because a violation
// fires inside the SERVED document and a listener attached from the probe at load time
// arrives after the initial render has already happened.
var hermeticFlags = []string{
	"--headless", "--disable-gpu", "--no-sandbox", "--disable-dev-shm-usage",
	"--enable-logging=stderr", "--log-level=0",
	"--hide-scrollbars", "--window-size=1280,1024",
	"--no-first-run", "--no-default-browser-check", "--disable-default-apps",
	"--disable-background-networking", "--disable-component-update",
	"--disable-client-side-phishing-detection", "--safebrowsing-disable-auto-update",
	"--disable-sync", "--disable-domain-reliability", "--disable-extensions",
	"--disable-breakpad", "--metrics-recording-only", "--mute-audio",
	"--password-store=basic", "--use-mock-keychain",
	"--disable-features=Translate,OptimizationHints,MediaRouter,DialMediaRouteProvider,InterestFeedContentSuggestions,AutofillServerCommunication,CalculateNativeWinOcclusion",
}

// verdictDeadline is the TEST's own deadline for a measurement. It exists so a browser
// that wedges fails HERE, naming the page and carrying the browser's output, rather than
// hanging until the CI runner kills the whole job with nothing to read (B21).
const verdictDeadline = 90 * time.Second

// runProbe points the browser at the probe page and returns the raw verdict together with
// everything the browser wrote to stdout/stderr. The browser is killed as soon as the
// verdict arrives (after a short grace, so a console message raised during the render is
// not truncated out of the log), and the process is REAPED before the log is read, so the
// caller never races the copying goroutines for it.
//
// It returns an error rather than failing the test, so the deadline itself is gradeable.
func runProbe(bin string, ps *probeServer, deadline time.Duration, profile string) (raw []byte, browserLog string, err error) {
	args := append(append([]string{}, hermeticFlags...), "--user-data-dir="+profile, ps.url+"/probe")
	cmd := exec.Command(bin, args...)
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	// A profile-local HOME and no session bus to look for: the runner has neither.
	cmd.Env = append(os.Environ(), "HOME="+profile, "DBUS_SESSION_BUS_ADDRESS=disabled:")
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("starting %s: %w", bin, err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})
	}
	defer stop()

	select {
	case raw = <-ps.verdict:
		time.Sleep(250 * time.Millisecond)
		stop()
		return raw, log.String(), nil
	case <-time.After(deadline):
		stop()
		return nil, log.String(), fmt.Errorf("the browser never posted a verdict for %s within %s\nbrowser output:\n%s",
			ps.url, deadline, log.String())
	}
}

// renderAndRead is runProbe plus the source-offer verdict decoding, and it fails the test
// on anything that went wrong.
func renderAndRead(t *testing.T, bin string, ps *probeServer) renderVerdict {
	t.Helper()
	v, _ := renderAndReadLog(t, bin, ps)
	return v
}

// renderAndReadLog also hands back what the browser itself printed. That log is the
// engine's own report of every Content-Security-Policy and Trusted Types refusal it made
// while rendering, which is a fact about the render that no assertion inside the page can
// observe: a violation fires in the SERVED document, and a listener attached from the
// parent at load time arrives after the initial render has already happened.
func renderAndReadLog(t *testing.T, bin string, ps *probeServer) (renderVerdict, string) {
	t.Helper()
	raw, log, err := runProbe(bin, ps, verdictDeadline, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var v renderVerdict
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("verdict is not JSON (%v): %s\nbrowser output:\n%s", err, raw, log)
	}
	if v.Error != "" {
		t.Fatalf("the probe failed inside the browser: %s\nbrowser output:\n%s", v.Error, log)
	}
	return v, log
}

// hiddenProblems reports every way the browser says the offer did NOT reach the
// screen. This is the rendered counterpart of the byte-level predicate: computed
// style after the real cascade, real layout geometry, and a hit test at the offer's
// own centre point.
func hiddenProblems(v renderVerdict) []string {
	var out []string
	if !v.Found {
		return []string{"the browser found no source offer in the rendered document"}
	}
	if v.InDetails {
		out = append(out, "the offer is inside a <details> element")
	}
	if v.Width <= 0 || v.Height <= 0 {
		out = append(out, "the offer has no rendered box")
	}
	if !v.OffsetParent {
		out = append(out, "the offer has no offsetParent, so it or an ancestor is display:none")
	}
	if !v.HitIsSelfOrDescendant {
		out = append(out, "a hit test at the offer's own centre does not reach it")
	}
	for _, n := range v.Chain {
		if n.Display == "none" {
			out = append(out, "the computed display of the "+n.Tag+" ancestor is none")
		}
		if n.Visibility == "hidden" || n.Visibility == "collapse" {
			out = append(out, "the computed visibility of the "+n.Tag+" ancestor is "+n.Visibility)
		}
		if n.Opacity == "0" {
			out = append(out, "the computed opacity of the "+n.Tag+" ancestor is 0")
		}
		if n.Hidden {
			out = append(out, "the "+n.Tag+" ancestor carries a hidden attribute")
		}
	}
	return out
}

// AC1 in a browser: the offer is SHOWN. It renders, it has a box, nothing in the
// cascade hides it or any ancestor, a hit test at its own centre reaches it, and the
// text a reader sees carries the label, the source URL, the licence name and the
// build identity.
func TestRendered_OfferIsShownToAReaderWithoutInteraction(t *testing.T) {
	bin := chromium(t)
	v := renderAndRead(t, bin, serveDocument(t, sourceoffer.Upstream, nil))

	if probs := hiddenProblems(v); probs != nil {
		t.Errorf("the browser did not show the offer: %v", probs)
	}
	// AC1, AC2, AC12, AC5: what the reader actually sees.
	for _, want := range []string{
		sourceoffer.Label + ": " + sourceoffer.Upstream,
		sourceoffer.License,
		version.String(),
	} {
		if !strings.Contains(v.Text, want) {
			t.Errorf("the rendered offer does not show %q\nshown: %q", want, v.Text)
		}
	}
	// The link the browser resolved: displayed text and target are both the value.
	if v.LinkText != sourceoffer.Upstream {
		t.Errorf("the rendered link shows %q, want the source URL in effect", v.LinkText)
	}
	if v.LinkHref != sourceoffer.Upstream {
		t.Errorf("the rendered link targets %q, want the source URL in effect", v.LinkHref)
	}
	// AC12: the licence is text, not a link - it is not inside the only link there is.
	if strings.Contains(v.LinkText, sourceoffer.License) {
		t.Errorf("the licence is inside the link: %q", v.LinkText)
	}
	if len(v.OfferChildTags) != 1 || v.OfferChildTags[0] != "a" {
		t.Errorf("the rendered offer contains %v, want exactly one anchor", v.OfferChildTags)
	}
}

// The rendered grader must BITE. Each mutation is served to the SAME browser through
// the same probe, and each is a way an offer can be in the bytes and still never
// reach a reader - including the two a byte-level scan of rule text cannot settle:
// a rule that wins the cascade from a DIFFERENT selector, and an overlay painted on
// top of the offer.
func TestRendered_GraderFailsAgainstEveryHidingMutation(t *testing.T) {
	bin := chromium(t)

	// The unmutated document first, so a report below is a signal.
	if probs := hiddenProblems(renderAndRead(t, bin, serveDocument(t, sourceoffer.Upstream, nil))); probs != nil {
		t.Fatalf("the shipped document was reported as hiding the offer: %v", probs)
	}

	inject := func(css string) func([]byte) []byte {
		return func(b []byte) []byte {
			return []byte(strings.Replace(string(b), "</style>", css+"\n</style>", 1))
		}
	}
	for name, mutate := range map[string]func([]byte) []byte{
		"display:none on its own class":    inject(".source-offer { display:none; }"),
		"visibility:hidden on an ancestor": inject("footer { visibility:hidden; }"),
		"opacity:0 on an ancestor":         inject("footer { opacity:0; }"),
		// The cases a byte-level scan of rule text cannot decide. The selector names
		// NEITHER the offer's class NOR any element type it carries, so the byte-level
		// predicate reads clean - and the browser still never shows the offer.
		"a descendant selector nothing on the offer names": inject("main ~ * > p { display:none; }"),
		"a specificity fight the hiding rule wins":         inject("body footer p.source-offer { display: none !important; }"),
		"an opaque overlay painted over the whole page": inject(
			"body::after { content:''; position:fixed; inset:0; background:#000; z-index:9999; }"),
		"zero-size box": inject(".source-offer { position:absolute; width:0; height:0; overflow:hidden; }"),
		// And the DOM-level ones.
		"a hidden attribute on the offer": func(b []byte) []byte {
			return []byte(strings.Replace(string(b), `<p class="source-offer">`, `<p class="source-offer" hidden>`, 1))
		},
		"the offer is moved behind a details element": func(b []byte) []byte {
			s := strings.Replace(string(b), `<p class="source-offer">`, `<details><summary>legal</summary><p class="source-offer">`, 1)
			return []byte(strings.Replace(s, "</p></footer>", "</p></details></footer>", 1))
		},
	} {
		_, plain := fetchRoot(t, HandlerFor(offerFor(sourceoffer.Upstream)))
		if string(mutate([]byte(plain))) == plain {
			t.Fatalf("the mutation %q did not change the served document - the assertion below would be vacuous", name)
		}
		v := renderAndRead(t, bin, serveDocument(t, sourceoffer.Upstream, mutate))
		if probs := hiddenProblems(v); probs == nil {
			t.Errorf("the rendered grader passed a mutation that hides the offer from a reader (%s): %+v", name, v)
		}
	}
}

// AC11 in a browser, which is where "introduces no additional element, attribute or
// script" is actually decidable. The value carries `"><img src=x onerror=1>`; the
// question is whether the browser PARSED any of that as markup. It did not if the
// element count is identical to a benign build's, no img element exists, no event
// handler attribute exists, and the offer and its link carry exactly the attributes
// this package writes - while the link still resolves to the value character for
// character.
func TestRendered_HostileValueIntroducesNoElementOrAttribute(t *testing.T) {
	bin := chromium(t)

	benign := renderAndRead(t, bin, serveDocument(t, sourceoffer.Upstream, nil))
	hostile := renderAndRead(t, bin, serveDocument(t, hostileValue, nil))

	if !hostile.Found {
		t.Fatal("the hostile-value document rendered no offer at all")
	}
	if hostile.ElementCount != benign.ElementCount {
		t.Errorf("the value introduced %d element(s) into the rendered document",
			hostile.ElementCount-benign.ElementCount)
	}
	if hostile.ImgCount != 0 {
		t.Errorf("the rendered document contains %d img element(s); the value introduced markup", hostile.ImgCount)
	}
	if hostile.ScriptCount != benign.ScriptCount {
		t.Errorf("the rendered document has %d scripts, a benign build has %d", hostile.ScriptCount, benign.ScriptCount)
	}
	if hostile.OnerrorAttrs != 0 {
		t.Errorf("the value introduced %d event-handler attribute(s)", hostile.OnerrorAttrs)
	}
	if got, want := strings.Join(hostile.OfferAttrs, ","), strings.Join(benign.OfferAttrs, ","); got != want {
		t.Errorf("the offer's attributes changed with the value: %q, want %q", got, want)
	}
	if got, want := strings.Join(hostile.LinkAttrs, ","), strings.Join(benign.LinkAttrs, ","); got != want {
		t.Errorf("the link's attributes changed with the value: %q, want %q", got, want)
	}
	// AC3/AC11: the link the browser resolved is the value, character for character,
	// as both target and displayed text - and it is still shown.
	if hostile.LinkHref != hostileValue {
		t.Errorf("the rendered link targets %q, want %q", hostile.LinkHref, hostileValue)
	}
	if hostile.LinkText != hostileValue {
		t.Errorf("the rendered link shows %q, want %q", hostile.LinkText, hostileValue)
	}
	if probs := hiddenProblems(hostile); probs != nil {
		t.Errorf("the hostile-value offer did not reach the screen: %v", probs)
	}
}

// AC3 in a browser: a fork build's rendered offer shows the fork's tree and the
// upstream URL appears nowhere in the text a reader sees.
func TestRendered_ForkBuildShowsItsOwnTreeAndNeverUpstream(t *testing.T) {
	bin := chromium(t)
	v := renderAndRead(t, bin, serveDocument(t, forkValue, nil))

	if probs := hiddenProblems(v); probs != nil {
		t.Fatalf("the fork build's offer did not reach the screen: %v", probs)
	}
	if v.LinkHref != forkValue || v.LinkText != forkValue {
		t.Errorf("the rendered link is target=%q text=%q, want both %q", v.LinkHref, v.LinkText, forkValue)
	}
	if !strings.Contains(v.Text, sourceoffer.Label+": "+forkValue) {
		t.Errorf("the rendered offer does not show the label immediately before the fork value: %q", v.Text)
	}
	if strings.Contains(v.Text, sourceoffer.Upstream) {
		t.Errorf("the upstream URL is shown inside a fork build's offer: %q", v.Text)
	}
	if strings.Contains(v.LinkHref, sourceoffer.Upstream) {
		t.Errorf("the fork build's link targets upstream: %q", v.LinkHref)
	}
}

// --- WEBUI-10: the graders' own failure modes -----------------------------------

// B21. A rendered grader whose browser never returns a measurement must fail on ITS OWN
// deadline, with the browser's output attached - never hang until the CI runner kills the
// job, which leaves nobody anything to read. Two CI runs were lost to exactly that before
// the verdict was moved off --dump-dom and onto a POST the test waits for; this proves the
// deadline that replaced it still bites.
func TestRendered_ABrowserThatNeverAnswersFailsOnTheGradersOwnDeadline(t *testing.T) {
	bin := chromium(t)
	// A probe that is never ready: the browser loads the page and keeps retrying for far
	// longer than the deadline this grader gives it.
	const neverReady = `function verdict(doc, win) { return { ready: false }; }`
	ps := serveDocumentWith(t, serveOpts{url: sourceoffer.Upstream, probe: neverReady, wait: 10 * time.Minute})

	start := time.Now()
	raw, _, err := runProbe(bin, ps, 5*time.Second, t.TempDir())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("the grader accepted a verdict (%s) from a browser that was never going to answer", raw)
	}
	if elapsed > 60*time.Second {
		t.Errorf("the grader waited %s on a browser that never answered; its deadline was 5s", elapsed)
	}
	for _, want := range []string{"never posted a verdict", "5s", "browser output"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not carry %q, so a reader cannot tell what happened: %v", want, err)
		}
	}
	// And the deadline the real graders use is the TEST's, well inside any CI job budget.
	if verdictDeadline <= 0 || verdictDeadline > 5*time.Minute {
		t.Errorf("verdictDeadline is %s; a rendered grader must fail on its own deadline, not the runner's", verdictDeadline)
	}
}

// B8. A runtime this package needs and cannot find is a SKIP under `make check` (which
// must stay green on a machine with no browser) and a FAILURE under `make webui-check`.
// Both halves are proven for real: the package's own test binary is built once and run
// with an EMPTY PATH, so neither chromium nor node can be found, first without the
// required-mode switch and then with it.
func TestRuntimeGate_SkipsWhenARuntimeIsAbsentAndFailsUnderRequiredMode(t *testing.T) {
	if os.Getenv("HOLDFAST_WEBUI_GATE_CHILD") == "1" {
		t.Skip("this is the child of the runtime-gate self-test; it does not recurse")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("no go toolchain on PATH to build the child test binary: %v", err)
	}
	bin := filepath.Join(t.TempDir(), "webui.test")
	if out, err := exec.Command(goBin, "test", "-c", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("building this package's test binary: %v\n%s", err, out)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	run := func(required bool, name string) (string, error) {
		cmd := exec.Command(bin, "-test.run", "^"+name+"$", "-test.v")
		cmd.Dir = wd
		env := []string{
			"PATH=", // the point of the exercise: no runtime can be found
			"HOME=" + t.TempDir(),
			"HOLDFAST_WEBUI_GATE_CHILD=1",
		}
		if required {
			env = append(env, "HOLDFAST_WEBUI_REQUIRED=1")
		}
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	for _, c := range []struct{ runtime, test string }{
		{"chromium", "TestRendered_OfferIsShownToAReaderWithoutInteraction"},
		{"node", "TestUnit_DerivationsRunInNodesOwnTestRunner"},
	} {
		out, err := run(false, c.test)
		if err != nil {
			t.Errorf("with no %s on PATH and no required mode, `make check` must stay green; it exited %v:\n%s", c.runtime, err, out)
		}
		if !strings.Contains(out, "--- SKIP: "+c.test) {
			t.Errorf("with no %s on PATH the suite did not report itself SKIPPED:\n%s", c.runtime, out)
		}
		if strings.Contains(out, "--- PASS: "+c.test) {
			t.Errorf("with no %s on PATH the suite reported a PASS, which is a false green:\n%s", c.runtime, out)
		}
		if !strings.Contains(out, c.runtime) {
			t.Errorf("the skip message does not name the missing runtime %q:\n%s", c.runtime, out)
		}

		out, err = run(true, c.test)
		if err == nil {
			t.Errorf("under required mode the suite passed with no %s on PATH:\n%s", c.runtime, out)
		}
		if !strings.Contains(out, "--- FAIL: "+c.test) {
			t.Errorf("under required mode the suite did not FAIL with no %s on PATH:\n%s", c.runtime, out)
		}
		if !strings.Contains(out, "required runtime") || !strings.Contains(out, c.runtime) {
			t.Errorf("the required-mode failure does not name the missing runtime %q:\n%s", c.runtime, out)
		}
		if strings.Contains(out, "--- SKIP: "+c.test) {
			t.Errorf("under required mode the suite still SKIPPED with no %s on PATH:\n%s", c.runtime, out)
		}
	}
}
