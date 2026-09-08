package webui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// The CHROME DEVTOOLS PROTOCOL driver, and why this repository has one.
//
// S0053 brought three criteria the existing harness could not decide, and all three are
// engine OPERATIONS rather than page reads:
//
//   - emulating the operating system's colour-scheme preference, so the theme under test
//     is set at the ENGINE and never by a class, an attribute or a stylesheet a grader
//     injects into the page (a grader that injects the theme is grading its own fixture);
//   - reading the ACCESSIBILITY TREE the engine actually computed, which no expression
//     evaluated inside the page can reconstruct - an accessible name is the engine's
//     answer, not a property of the markup;
//   - dispatching REAL key presses. A KeyboardEvent constructed in the page is untrusted
//     and moves focus nowhere, so tab order cannot be observed from inside the document.
//
// All three are CDP. The spec allows a module dependency for this (chromedp is the
// Go-native route the render decision names) and equally allows driving the protocol
// directly; this is the direct route, and it costs NO module at all:
//
//	--remote-debugging-pipe makes the browser speak CDP over file descriptors 3 and 4
//	instead of a WebSocket, as NUL-terminated JSON. exec.Cmd.ExtraFiles hands the child
//	exactly those two descriptors, and encoding/json does the rest.
//
// That matters here beyond taste. This repository's build path is Go and the standard
// library by design - no bundler, no registry package, no lockfile - and `make check`
// runs govulncheck over every module in the graph, test-only ones included. A WebSocket
// client, a protocol-definition package and their transitive tree would be five or six
// new modules to carry for three protocol calls.
//
// What is NOT relaxed here: the graders still load the SERVED document, under the real
// response headers and the real Content-Security-Policy, and still decide every claim
// about what a reader sees from what the engine reported. Runtime.evaluate runs from the
// devtools side and is not subject to the page's script-src, so measuring the page does
// not itself produce the policy violations the F11 graders assert are absent.

// --- the wire ---------------------------------------------------------------------

// cdpFrame is what this side WRITES: one command.
type cdpFrame struct {
	ID        int            `json:"id"`
	Method    string         `json:"method"`
	Params    map[string]any `json:"params"`
	SessionID string         `json:"sessionId,omitempty"`
}

// cdpIncoming is what this side READS: either a reply to a command (it carries an id) or
// an event the browser raised on its own (it carries a method and no id).
type cdpIncoming struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *cdpError       `json:"error"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data"`
}

func (e *cdpError) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("%s (%s)", e.Message, e.Data)
	}
	return e.Message
}

type cdpReply struct {
	result json.RawMessage
	err    *cdpError
}

// cdpLogEntry is one entry of the browser's own log. The security-sourced ones are the
// engine's report of every Content-Security-Policy and Trusted Types refusal it made
// while rendering, which is a fact about the render no assertion inside the page can
// observe: a violation fires in the served document, and a listener attached afterwards
// arrives after the initial render has already happened.
type cdpLogEntry struct {
	Source string `json:"source"`
	Level  string `json:"level"`
	Text   string `json:"text"`
	URL    string `json:"url"`
}

type cdpBrowser struct {
	t    *testing.T
	cmd  *exec.Cmd
	w    *os.File
	stop sync.Once

	mu      sync.Mutex
	nextID  int
	pending map[int]chan cdpReply

	logMu   sync.Mutex
	entries []cdpLogEntry

	errMu  sync.Mutex
	stderr strings.Builder
}

// cdpTimeout is the deadline on ONE protocol call. The test owns every deadline in this
// file, exactly as the iframe harness does, so a wedged browser fails HERE, naming the
// call, rather than hanging until the CI runner kills the job with nothing to read.
const cdpTimeout = 60 * time.Second

// launchCDP starts the browser speaking CDP over its own pipe and returns a driver. It
// registers its own cleanup, so a test never has to remember to kill a browser.
func launchCDP(t *testing.T) *cdpBrowser {
	t.Helper()
	bin := chromium(t)

	toChildR, toChildW, err := os.Pipe()
	if err != nil {
		t.Fatalf("cdp: pipe to the browser: %v", err)
	}
	fromChildR, fromChildW, err := os.Pipe()
	if err != nil {
		t.Fatalf("cdp: pipe from the browser: %v", err)
	}

	// A profile directory of our own rather than t.TempDir: the browser's helper
	// processes can outlive the kill by a moment, and a directory that was not empty yet
	// when the framework tried to remove it would fail an otherwise passing test.
	profile, err := os.MkdirTemp("", "holdfast-cdp-*")
	if err != nil {
		t.Fatalf("cdp: profile directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(profile) })
	args := append(append([]string{}, hermeticFlags...),
		"--user-data-dir="+profile, "--remote-debugging-pipe", "about:blank")
	cmd := exec.Command(bin, args...)
	// ExtraFiles[0] is the child's fd 3 (it READS commands from there) and ExtraFiles[1]
	// is its fd 4 (it WRITES replies and events to there). That mapping is the whole of
	// --remote-debugging-pipe's transport.
	cmd.ExtraFiles = []*os.File{toChildR, fromChildW}
	cmd.Env = append(os.Environ(), "HOME="+profile, "DBUS_SESSION_BUS_ADDRESS=disabled:")

	b := &cdpBrowser{t: t, cmd: cmd, w: toChildW, pending: map[int]chan cdpReply{}}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatalf("cdp: pipe for the browser's stderr: %v", err)
	}
	cmd.Stdout, cmd.Stderr = stderrW, stderrW
	if err := cmd.Start(); err != nil {
		t.Fatalf("cdp: starting %s: %v", bin, err)
	}
	// The parent keeps only its own ends.
	_ = toChildR.Close()
	_ = fromChildW.Close()
	_ = stderrW.Close()

	go b.drainStderr(stderrR)
	go b.readLoop(fromChildR)

	t.Cleanup(b.close)
	return b
}

func (b *cdpBrowser) drainStderr(r *os.File) {
	defer func() { _ = r.Close() }()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		b.errMu.Lock()
		if b.stderr.Len() < 1<<18 {
			b.stderr.WriteString(sc.Text())
			b.stderr.WriteByte('\n')
		}
		b.errMu.Unlock()
	}
}

func (b *cdpBrowser) readLoop(r *os.File) {
	defer func() { _ = r.Close() }()
	br := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := br.ReadString(0)
		if err != nil {
			b.failAllPending(err)
			return
		}
		var f cdpIncoming
		if json.Unmarshal([]byte(strings.TrimRight(line, "\x00")), &f) != nil {
			continue
		}
		if f.ID != 0 {
			b.mu.Lock()
			ch := b.pending[f.ID]
			delete(b.pending, f.ID)
			b.mu.Unlock()
			if ch != nil {
				ch <- cdpReply{result: f.Result, err: f.Error}
			}
			continue
		}
		if f.Method == "Log.entryAdded" {
			var p struct {
				Entry cdpLogEntry `json:"entry"`
			}
			if json.Unmarshal(f.Params, &p) == nil {
				b.logMu.Lock()
				b.entries = append(b.entries, p.Entry)
				b.logMu.Unlock()
			}
		}
	}
}

func (b *cdpBrowser) failAllPending(err error) {
	b.mu.Lock()
	for id, ch := range b.pending {
		delete(b.pending, id)
		ch <- cdpReply{err: &cdpError{Message: "the browser's pipe closed: " + err.Error()}}
	}
	b.mu.Unlock()
}

func (b *cdpBrowser) close() {
	b.stop.Do(func() {
		_ = b.w.Close()
		_ = b.cmd.Process.Kill()
		_ = b.cmd.Wait()
	})
}

// browserLog is everything the browser wrote to its own stderr, for a failure message.
func (b *cdpBrowser) browserLog() string {
	b.errMu.Lock()
	defer b.errMu.Unlock()
	return b.stderr.String()
}

// call sends one command and waits for its reply. Params are marshalled as given; a nil
// params is sent as an empty object, which every domain accepts.
func (b *cdpBrowser) call(session, method string, params map[string]any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	b.mu.Lock()
	b.nextID++
	id := b.nextID
	ch := make(chan cdpReply, 1)
	b.pending[id] = ch
	payload, err := json.Marshal(cdpFrame{ID: id, Method: method, Params: params, SessionID: session})
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	_, werr := b.w.Write(append(payload, 0))
	b.mu.Unlock()
	if werr != nil {
		return nil, fmt.Errorf("cdp: writing %s: %w", method, werr)
	}
	select {
	case rep := <-ch:
		if rep.err != nil {
			return nil, fmt.Errorf("cdp: %s: %w", method, rep.err)
		}
		return rep.result, nil
	case <-time.After(cdpTimeout):
		return nil, fmt.Errorf("cdp: %s did not answer within %s", method, cdpTimeout)
	}
}

func (b *cdpBrowser) mustCall(session, method string, params map[string]any) json.RawMessage {
	b.t.Helper()
	res, err := b.call(session, method, params)
	if err != nil {
		b.t.Fatalf("%v\nbrowser output:\n%s", err, b.browserLog())
	}
	return res
}

// --- one page under measurement ---------------------------------------------------

// cdpPage is a browser tab with the domains these graders need enabled. It is created
// once per test and re-navigated per case, which is what keeps a suite of a dozen
// engine-driven graders to one browser launch instead of a dozen.
type cdpPage struct {
	b   *cdpBrowser
	sid string
}

func (b *cdpBrowser) newPage() *cdpPage {
	b.t.Helper()
	var created struct {
		TargetID string `json:"targetId"`
	}
	mustJSON(b.t, b.mustCall("", "Target.createTarget", map[string]any{"url": "about:blank"}), &created)
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	mustJSON(b.t, b.mustCall("", "Target.attachToTarget",
		map[string]any{"targetId": created.TargetID, "flatten": true}), &attached)

	p := &cdpPage{b: b, sid: attached.SessionID}
	// Log.enable is the INSTRUMENT for clause F11: every Content-Security-Policy and
	// Trusted Types refusal the engine makes arrives as a Log.entryAdded with
	// source "security".
	for _, d := range []string{"Page.enable", "Runtime.enable", "Log.enable", "DOM.enable", "Accessibility.enable"} {
		b.mustCall(p.sid, d, nil)
	}
	return p
}

// viewport fixes the CSS viewport the page lays out against. It is the engine's own
// device-metrics override, so 360 CSS pixels here is 360 CSS pixels to every media query
// and every layout box the page computes (clause F9).
func (p *cdpPage) viewport(w, h int) {
	p.b.t.Helper()
	p.b.mustCall(p.sid, "Emulation.setDeviceMetricsOverride", map[string]any{
		"width": w, "height": h, "deviceScaleFactor": 1, "mobile": false,
	})
}

// emulate sets the media features the page renders under, AT THE ENGINE.
//
// scheme is "light", "dark", or "" for NO PREFERENCE - and "" is not a third value the
// page is told about, it is the ABSENCE of the override, which is what an engine reports
// when the operating system expresses none. That is the only honest way to grade the
// third arm of clause F10: a grader that injected a class, an attribute or a stylesheet
// would be grading its own fixture, and one that passed "no-preference" as a value would
// be grading a value the platform never emits.
func (p *cdpPage) emulate(scheme string, reducedMotion bool) {
	p.b.t.Helper()
	features := []map[string]string{}
	if scheme != "" {
		features = append(features, map[string]string{"name": "prefers-color-scheme", "value": scheme})
	}
	if reducedMotion {
		features = append(features, map[string]string{"name": "prefers-reduced-motion", "value": "reduce"})
	}
	p.b.mustCall(p.sid, "Emulation.setEmulatedMedia", map[string]any{"features": features})
}

// navigate loads a URL and waits until the document has finished loading. Readiness is
// polled from the document itself rather than from a load event, because an evaluation
// issued while the old execution context is being torn down errors rather than answering,
// and retrying that is simpler than reasoning about which context a stale event named.
func (p *cdpPage) navigate(url string) {
	p.b.t.Helper()
	p.b.mustCall(p.sid, "Page.navigate", map[string]any{"url": url})
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		if err := p.eval(`document.readyState`, &state); err == nil && state == "complete" {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	p.b.t.Fatalf("the page never finished loading %s\nbrowser output:\n%s", url, p.b.browserLog())
}

// eval evaluates an expression in the page and decodes its value into out. The expression
// is evaluated by value, so what comes back is data rather than a remote handle.
func (p *cdpPage) eval(expr string, out any) error {
	res, err := p.b.call(p.sid, "Runtime.evaluate", map[string]any{
		"expression": expr, "returnByValue": true, "awaitPromise": true,
	})
	if err != nil {
		return err
	}
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(res, &r); err != nil {
		return err
	}
	if r.Exception != nil {
		return fmt.Errorf("the expression threw inside the page: %s", r.Exception.Text)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(r.Result.Value, out)
}

func (p *cdpPage) mustEval(expr string, out any) {
	p.b.t.Helper()
	if err := p.eval(expr, out); err != nil {
		p.b.t.Fatalf("%v\nexpression:\n%s\nbrowser output:\n%s", err, expr, p.b.browserLog())
	}
}

// waitUntil polls a boolean expression until it answers true. It is how a grader waits
// for the page to have RENDERED a snapshot, which is a different moment from the stream
// opening: the page reports "live" on connect and the snapshot that fills the tables
// arrives after it.
func (p *cdpPage) waitUntil(expr string, d time.Duration) error {
	deadline := time.Now().Add(d)
	var last error
	for time.Now().Before(deadline) {
		var ok bool
		if err := p.eval(expr, &ok); err != nil {
			last = err
		} else if ok {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	if last != nil {
		return fmt.Errorf("the page never satisfied %q within %s (last error: %v)", expr, d, last)
	}
	return fmt.Errorf("the page never satisfied %q within %s", expr, d)
}

// --- small helpers ----------------------------------------------------------------

func mustJSON(t *testing.T, raw json.RawMessage, out any) {
	t.Helper()
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("cdp: cannot decode %s: %v", raw, err)
	}
}
