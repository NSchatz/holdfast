package webui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The ENGINE DRIVER, and why a Go test in this package has one at all.
//
// The prose graders decide what a reader is given: the page-copy budget, the per-block
// ceiling, the redundancy rules, the documentation-link rule and the preservation
// checklist. Those decisions are Go, they read a committed word list and a committed
// removal record, and they are mutation-proved in prose_mutation_test.go. What they cannot
// do from Go is operate a browser, and what they need from one is exactly the set no
// expression evaluated inside the page can produce:
//
//   - the operating system's colour-scheme preference, set at the ENGINE, never by a
//     class, an attribute or a stylesheet a grader injects into the page;
//   - the accessibility tree the engine computed, WITH the source of each name - the field
//     that separates a name built from an element's own visible text (copy the budget has
//     already counted) from one a reader meets only through a screen reader;
//   - an evaluation of the measuring script against the SERVED document, under the real
//     Content-Security-Policy, which forbids eval inside the page.
//
// Those are the same three the Playwright project drives, so they are driven through the
// same runner: internal/webui/e2e/driver.mjs holds one browser and answers one JSON
// command per line. This repository had a second, hand-written driver for them - chromium
// launched with the DevTools protocol on file descriptors 3 and 4, and a wire format,
// a read loop and a pending-call table to go with it. Keeping it would have meant
// maintaining a browser transport of this repository's own beside the runner it had just
// adopted to answer the same questions, and two drivers is two answers to "what did the
// engine say" whenever they disagree.
//
// Everything else is unchanged: the document under measurement is still the one the REAL
// handler serves from this test's own httptest server, so the graders still read the same
// bytes and the same policy `holdfast serve` puts on the wire.

// engineTimeout is this side's deadline on ONE command. Every deadline in this package is
// owned by the test for the same reason: a wedged browser must fail HERE, naming the call,
// rather than hang until the CI runner kills the job with nothing to read.
const engineTimeout = 60 * time.Second

type engineBrowser struct {
	t     *testing.T
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Reader
	stop  sync.Once

	mu     sync.Mutex
	nextID int

	errMu  sync.Mutex
	stderr strings.Builder
}

// enginePage is one tab. It is created per reading, so a suite of engine-driven graders
// costs one browser launch rather than one per reading.
type enginePage struct {
	b  *engineBrowser
	id int
}

// launchEngine starts the driver and returns it, with its own cleanup registered so a test
// never has to remember to stop a browser. Every runtime it needs is located the way the
// rest of this package locates one: named, and a skip that becomes a failure under
// required mode.
func launchEngine(t *testing.T) *engineBrowser {
	t.Helper()
	node := nodeRuntime(t)
	browser := chromium(t)

	dir := "e2e"
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "@playwright", "test")); err != nil {
		missingRuntime(t, "@playwright/test",
			"the prose graders operate a real engine through the dashboard's grader project: run `npm ci` (or `npm install`) in internal/webui/e2e")
		return nil
	}

	cmd := exec.Command(node, "driver.mjs")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOLDFAST_BROWSER="+browser, "NO_COLOR=1")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("engine driver: stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("engine driver: stdout: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("engine driver: stderr: %v", err)
	}

	b := &engineBrowser{t: t, cmd: cmd, stdin: stdin, out: bufio.NewReaderSize(stdout, 1<<20)}
	if err := cmd.Start(); err != nil {
		t.Fatalf("engine driver: starting %s: %v", node, err)
	}
	go b.drainStderr(stderr)
	t.Cleanup(b.close)

	// The driver announces itself before it will answer anything, so a browser that failed
	// to launch is reported HERE, with what the driver printed, rather than as a timeout on
	// whichever command a grader happened to send first.
	var ready struct {
		Ready bool `json:"ready"`
	}
	line, err := b.readLine()
	if err != nil {
		t.Fatalf("the engine driver did not start: %v\ndriver output:\n%s", err, b.browserLog())
	}
	if json.Unmarshal(line, &ready) != nil || !ready.Ready {
		t.Fatalf("the engine driver did not announce itself; it said %q\ndriver output:\n%s", line, b.browserLog())
	}
	return b
}

func (b *engineBrowser) drainStderr(r io.Reader) {
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

func (b *engineBrowser) readLine() ([]byte, error) {
	line, err := b.out.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return line, nil
}

func (b *engineBrowser) close() {
	b.stop.Do(func() {
		_, _ = b.stdin.Write([]byte(`{"cmd":"quit"}` + "\n"))
		_ = b.stdin.Close()
		done := make(chan struct{})
		go func() { _ = b.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = b.cmd.Process.Kill()
			<-done
		}
	})
}

// browserLog is everything the driver and the engine wrote to their own stderr, for a
// failure message. It takes no lock the command path holds, so it is safe to read from
// inside a failing command.
func (b *engineBrowser) browserLog() string {
	b.errMu.Lock()
	defer b.errMu.Unlock()
	return b.stderr.String()
}

// output is browserLog plus what the PAGE said - console messages, page errors and failed
// requests. The driver collects those as they arrive, because they fire during the initial
// render and a listener attached afterwards has already missed them. It issues a command,
// so it is for a caller that is not itself inside one.
func (b *engineBrowser) output() string {
	var page string
	if raw, err := b.call("log", nil); err == nil {
		_ = json.Unmarshal(raw, &page)
	}
	return b.browserLog() + page
}

// call sends one command and waits for its reply. Commands are serialized: one reader, one
// writer, one outstanding call, so there is no pending-call table to get wrong. A call that
// does not answer within the deadline FAILS the test, which is why a stray reply after one
// cannot confuse a later command - there is no later command.
func (b *engineBrowser) call(cmd string, args map[string]any) (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	id := b.nextID

	req := map[string]any{"id": id, "cmd": cmd}
	for k, v := range args {
		req[k] = v
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := b.stdin.Write(append(payload, '\n')); err != nil {
		return nil, fmt.Errorf("engine driver: writing %s: %w", cmd, err)
	}

	type read struct {
		line []byte
		err  error
	}
	ch := make(chan read, 1)
	go func() {
		line, err := b.readLine()
		ch <- read{line, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("engine driver: reading the reply to %s: %w", cmd, r.err)
		}
		var rep struct {
			ID    int             `json:"id"`
			OK    bool            `json:"ok"`
			Value json.RawMessage `json:"value"`
			Error string          `json:"error"`
		}
		if err := json.Unmarshal(r.line, &rep); err != nil {
			return nil, fmt.Errorf("engine driver: the reply to %s is not JSON (%v): %s", cmd, err, r.line)
		}
		if rep.ID != id {
			return nil, fmt.Errorf("engine driver: the reply to %s carries id %d, not %d", cmd, rep.ID, id)
		}
		if !rep.OK {
			return nil, fmt.Errorf("engine driver: %s: %s", cmd, rep.Error)
		}
		return rep.Value, nil
	case <-time.After(engineTimeout):
		b.t.Fatalf("the engine driver did not answer %s within %s\ndriver output:\n%s", cmd, engineTimeout, b.browserLog())
		return nil, nil
	}
}

func (b *engineBrowser) mustCall(cmd string, args map[string]any) json.RawMessage {
	b.t.Helper()
	res, err := b.call(cmd, args)
	if err != nil {
		b.t.Fatalf("%v\ndriver output:\n%s", err, b.browserLog())
	}
	return res
}

func (b *engineBrowser) newPage() *enginePage {
	b.t.Helper()
	var id int
	mustJSON(b.t, b.mustCall("newPage", nil), &id)
	return &enginePage{b: b, id: id}
}

// viewport fixes the CSS viewport the page lays out against, so 360 CSS pixels here is 360
// CSS pixels to every media query and every layout box the page computes.
func (p *enginePage) viewport(w, h int) {
	p.b.t.Helper()
	p.b.mustCall("viewport", map[string]any{"page": p.id, "width": w, "height": h})
}

// emulate sets the media features the page renders under, AT THE ENGINE.
//
// scheme is "light", "dark", or "" for NO PREFERENCE - and "" is not a third value the page
// is told about, it is the ABSENCE of the override, which is what an engine reports when
// the operating system expresses none. A grader that injected a class, an attribute or a
// stylesheet would be grading its own fixture.
func (p *enginePage) emulate(scheme string, reducedMotion bool) {
	p.b.t.Helper()
	p.b.mustCall("emulate", map[string]any{"page": p.id, "scheme": scheme, "reduce": reducedMotion})
}

// navigate loads a URL and returns once the document has finished loading.
func (p *enginePage) navigate(url string) {
	p.b.t.Helper()
	p.b.mustCall("navigate", map[string]any{"page": p.id, "url": url})
}

// eval evaluates a script in the page and decodes its value into out. The value comes back
// as data rather than as a remote handle, and a throw inside the page is returned as an
// error rather than as a missing value.
func (p *enginePage) eval(expr string, out any) error {
	raw, err := p.b.call("eval", map[string]any{"page": p.id, "expr": expr})
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (p *enginePage) mustEval(expr string, out any) {
	p.b.t.Helper()
	if err := p.eval(expr, out); err != nil {
		p.b.t.Fatalf("%v\nexpression:\n%s\ndriver output:\n%s", err, expr, p.b.browserLog())
	}
}

// waitUntil polls a boolean expression until it answers true. It is how a grader waits for
// the page to have RENDERED a snapshot, which is a different moment from the stream
// opening: the page reports "live" on connect and the snapshot that fills the tables
// arrives after it.
func (p *enginePage) waitUntil(expr string, d time.Duration) error {
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

func mustJSON(t *testing.T, raw json.RawMessage, out any) {
	t.Helper()
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("engine driver: cannot decode %s: %v", raw, err)
	}
}
