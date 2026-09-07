package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
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
// The verdict is computed SYNCHRONOUSLY in this page's own `load` handler, with no
// timer and no --virtual-time-budget. That is load-bearing, not tidiness: the parent's
// load event cannot fire until the iframe has loaded, so the iframe's document is
// there by then, and --dump-dom serialises after load handlers have run.
// getBoundingClientRect forces layout synchronously, so geometry is settled when it is
// read. The earlier shape (a timer plus --virtual-time-budget) hung on a CI runner,
// because virtual time does not advance while a page has network in flight and the
// dashboard's own EventSource retries forever against a probe server that is not the
// real API.
const probePage = `<!DOCTYPE html><html><head><meta charset="utf-8"><title>probe</title></head>
<body><iframe id="f" src="%SRC%" width="1200" height="900" style="border:0"></iframe>
<pre id="verdict">pending</pre>
<script>
%JS%
window.addEventListener("load", function () {
  const f = document.getElementById("f");
  let v;
  try { v = verdict(f.contentDocument, f.contentWindow); }
  catch (e) { v = { error: String(e) }; }
  document.getElementById("verdict").textContent = JSON.stringify(v);
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

// chromium locates the browser or skips. `make check` must stay green on a machine
// with no browser, exactly as it must stay green on one with no docker.
func chromium(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("no chromium on PATH: the rendered-page grader needs a browser; the byte-level assertions in sourceoffer_test.go still ran")
	return ""
}

// serveDocument stands the REAL handler up on a real listener, plus the probe page
// beside it. mutate is applied to the served document bytes to build a counterexample
// (nil serves the document exactly as it ships).
func serveDocument(t *testing.T, url string, mutate func([]byte) []byte) *httptest.Server {
	t.Helper()
	real := HandlerFor(offerFor(url))
	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mutate == nil {
			real.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		real.ServeHTTP(rec, r)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(mutate(rec.Body.Bytes()))
	}))
	mux.HandleFunc("/probe", func(w http.ResponseWriter, _ *http.Request) {
		page := strings.Replace(probePage, "%JS%", probeJS, 1)
		page = strings.Replace(page, "%SRC%", "/", 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
	// The dashboard's own API calls, answered just well enough that the page does not
	// sit in a reconnect loop against a probe server that is not the real API. The
	// offer is in the static bytes and depends on none of this (that is AC8); this is
	// here so the browser has nothing to keep retrying while it is being measured.
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Bounded, so this handler can never hold httptest.Server.Close open if the
		// browser goes away without closing the connection.
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	})
	for _, ep := range []string{"/api/summary", "/api/queue", "/api/history"} {
		mux.HandleFunc(ep, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{}`))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// hermeticFlags make the browser a MEASURING INSTRUMENT rather than a desktop
// application. Everything here is either "do not reach the network" or "do not need a
// session bus": a CI runner's Chrome will otherwise register with GCM, poll the
// component updater and fail to find dbus, which is what hung the first version of
// this test on the runner. There is no --virtual-time-budget: see probePage.
var hermeticFlags = []string{
	"--headless", "--disable-gpu", "--no-sandbox", "--disable-dev-shm-usage",
	"--hide-scrollbars", "--window-size=1280,1024",
	"--no-first-run", "--no-default-browser-check", "--disable-default-apps",
	"--disable-background-networking", "--disable-component-update",
	"--disable-client-side-phishing-detection", "--safebrowsing-disable-auto-update",
	"--disable-sync", "--disable-domain-reliability", "--disable-extensions",
	"--disable-breakpad", "--metrics-recording-only", "--mute-audio",
	"--password-store=basic", "--use-mock-keychain",
	"--disable-features=Translate,OptimizationHints,MediaRouter,DialMediaRouteProvider,InterestFeedContentSuggestions,AutofillServerCommunication,CalculateNativeWinOcclusion",
}

// renderAndRead drives the browser over the probe page and returns what it saw.
func renderAndRead(t *testing.T, bin, url string) renderVerdict {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	profile := t.TempDir()
	args := append(append([]string{}, hermeticFlags...), "--user-data-dir="+profile, "--dump-dom", url)
	cmd := exec.CommandContext(ctx, bin, args...)
	// A profile-local HOME and no session bus to look for: the runner has neither.
	cmd.Env = append(os.Environ(), "HOME="+profile, "DBUS_SESSION_BUS_ADDRESS=disabled:")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("chromium --dump-dom %s: %v\n%s", url, err, out)
	}
	dom := string(out)
	i := strings.Index(dom, `<pre id="verdict">`)
	if i < 0 {
		t.Fatalf("the probe page rendered no verdict element:\n%s", dom)
	}
	rest := dom[i+len(`<pre id="verdict">`):]
	j := strings.Index(rest, "</pre>")
	if j < 0 {
		t.Fatalf("the verdict element is not closed:\n%s", dom)
	}
	raw := unescapeDOM(rest[:j])
	if raw == "pending" {
		t.Fatalf("the probe never ran: the iframe did not load in time\n%s", dom)
	}
	var v renderVerdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("verdict is not JSON (%v): %s", err, raw)
	}
	if v.Error != "" {
		t.Fatalf("the probe failed inside the browser: %s", v.Error)
	}
	return v
}

// unescapeDOM undoes the entity escaping --dump-dom applies to text content.
func unescapeDOM(s string) string {
	r := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&amp;", "&")
	return r.Replace(s)
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
	srv := serveDocument(t, sourceoffer.Upstream, nil)
	v := renderAndRead(t, bin, srv.URL+"/probe")

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
	if probs := hiddenProblems(renderAndRead(t, bin, serveDocument(t, sourceoffer.Upstream, nil).URL+"/probe")); probs != nil {
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
		srv := serveDocument(t, sourceoffer.Upstream, mutate)
		v := renderAndRead(t, bin, srv.URL+"/probe")
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

	benign := renderAndRead(t, bin, serveDocument(t, sourceoffer.Upstream, nil).URL+"/probe")
	hostile := renderAndRead(t, bin, serveDocument(t, hostileValue, nil).URL+"/probe")

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
	v := renderAndRead(t, bin, serveDocument(t, forkValue, nil).URL+"/probe")

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
