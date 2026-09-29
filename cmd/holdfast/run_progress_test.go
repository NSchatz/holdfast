package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// S0173's criteria are graded on a REAL `holdfast run`: the command, its engine, its job store
// and its reporter all run as an operator runs them, over real media the pinned ffmpeg builds
// here and the real ffprobe reads. Two things are stood in for, and only two.
//
//   - THE ENCODER SUBPROCESS. HOLDFAST_FFMPEG names a script that is the pinned ffmpeg for
//     every invocation except a job's encode - the one invocation carrying `-progress pipe:3`.
//     That one writes the progress reports a test tells it to on the descriptor the engine
//     reads, and then fails, or hands the same argv to the pinned ffmpeg for a real encode.
//   - THE CLOCK. runProgressClock is a clock the test advances by hand, so the shipped
//     thirty-second interval and a speed measured over it are graded exactly, and fast.
//
// What is left asynchronous is the engine's own: its progress drain runs on its own goroutine,
// and it publishes at most one report per job per second (engine.progressEmitInterval). A test
// therefore gives a report s0173Settle to arrive before advancing the clock, and sends a second
// position of one encode over a window longer than that throttle (see deliverPastThrottle).

const (
	// s0173Settle is the margin a report the stand-in has written is given to cross the
	// engine's progress drain and reach the reporter.
	s0173Settle = 500 * time.Millisecond
	// s0173Quiet is how long a test waits before concluding that nothing more was written.
	s0173Quiet = 300 * time.Millisecond
	// s0173Deadline bounds every wait for something that must happen.
	s0173Deadline = 60 * time.Second
)

// Fixture kinds: real media, each built by the pinned ffmpeg under t.TempDir().
const (
	s0173Clip        = "clip"         // 2 s at 320x240, a real encode of which takes well under a second
	s0173TenMinutes  = "ten-minutes"  // 600 s at one frame a second
	s0173FiveMinutes = "five-minutes" // 300 s at one frame a second
	s0173NoDuration  = "no-duration"  // Matroska written through a pipe: no Duration element at all
	s0173NegDuration = "neg-duration" // Matroska whose Duration element says -5 s
)

// s0173Tools are the ffmpeg and ffprobe this test binary was started with, read once when the
// package is initialised. A library built later in a test must hand its stand-in these, never
// the stand-in an earlier library in the same test has pointed HOLDFAST_FFMPEG at.
var s0173Tools = struct{ ffmpeg, ffprobe string }{envOr("HOLDFAST_FFMPEG", "ffmpeg"), envOr("HOLDFAST_FFPROBE", "ffprobe")}

// s0173Epoch is where the stood-in clock starts; any fixed instant would do.
var s0173Epoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

// s0173Clock is the clock stood in for the reporter's. It moves only when a test advances it,
// and it delivers each tick that falls due the way time.Ticker does, dropping one the reporter
// has not yet taken.
type s0173Clock struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*s0173Ticker
}

type s0173Ticker struct {
	every   time.Duration
	next    time.Time
	c       chan time.Time
	stopped bool
}

func (c *s0173Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *s0173Clock) Ticker(every time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tk := &s0173Ticker{every: every, next: c.now.Add(every), c: make(chan time.Time, 1)}
	c.tickers = append(c.tickers, tk)
	return tk.c, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		tk.stopped = true
	}
}

// advance moves the clock on by d and delivers every tick that falls due.
func (c *s0173Clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for _, tk := range c.tickers {
		for !tk.stopped && !tk.next.After(c.now) {
			select {
			case tk.c <- tk.next:
			default:
			}
			tk.next = tk.next.Add(tk.every)
		}
	}
}

// running is how many of the tickers this clock handed out have not been stopped.
func (c *s0173Clock) running() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, tk := range c.tickers {
		if !tk.stopped {
			n++
		}
	}
	return n
}

// asked is the interval of every ticker this clock handed out, in order.
func (c *s0173Clock) asked() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []time.Duration
	for _, tk := range c.tickers {
		out = append(out, tk.every)
	}
	return out
}

// useS0173Clock stands the clock in for the reporter's for the rest of the test.
func useS0173Clock(t *testing.T) *s0173Clock {
	t.Helper()
	c := &s0173Clock{now: s0173Epoch}
	was := runProgressClock
	runProgressClock = c
	t.Cleanup(func() { runProgressClock = was })
	return c
}

// s0173Capture is a stream the reporter's goroutine and the test can share.
type s0173Capture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *s0173Capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *s0173Capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// s0173Line is one progress line as the spec defines one: a line carrying the labelled tokens
// file=, position=, speed= and eta=. Any other text on it is the implementer's and is not read.
type s0173Line struct {
	raw                        string
	file, position, speed, eta string
}

// s0173Lines is every progress line in text.
func s0173Lines(text string) []s0173Line {
	var out []s0173Line
	for _, raw := range strings.Split(text, "\n") {
		l := s0173Line{raw: raw}
		var okFile, okPos, okSpeed, okETA bool
		l.file, okFile = s0173Token(raw, "file=")
		l.position, okPos = s0173Token(raw, "position=")
		l.speed, okSpeed = s0173Token(raw, "speed=")
		l.eta, okETA = s0173Token(raw, "eta=")
		if okFile && okPos && okSpeed && okETA {
			out = append(out, l)
		}
	}
	return out
}

// s0173Token reads the value labelled key: a quoted value whole, or a bare one up to the next
// space.
func s0173Token(line, key string) (string, bool) {
	at := -1
	if strings.HasPrefix(line, key) {
		at = 0
	} else if i := strings.Index(line, " "+key); i >= 0 {
		at = i + 1
	}
	if at < 0 {
		return "", false
	}
	rest := line[at+len(key):]
	if strings.HasPrefix(rest, `"`) {
		q, err := strconv.QuotedPrefix(rest)
		if err != nil {
			return "", false
		}
		v, err := strconv.Unquote(q)
		return v, err == nil
	}
	if end := strings.IndexByte(rest, ' '); end >= 0 {
		rest = rest[:end]
	}
	return rest, rest != ""
}

// s0173For is the progress lines in text that name file.
func s0173For(text, file string) []s0173Line {
	var out []s0173Line
	for _, l := range s0173Lines(text) {
		if l.file == file {
			out = append(out, l)
		}
	}
	return out
}

// waitS0173Lines waits until w carries at least n progress lines naming file, and returns them.
func waitS0173Lines(t *testing.T, w *s0173Capture, file string, n int) []s0173Line {
	t.Helper()
	deadline := time.Now().Add(s0173Deadline)
	for {
		if got := s0173For(w.String(), file); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %d progress line(s) naming %s; stderr so far:\n%s", s0173Deadline, n, file, w.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// s0173Seconds reads an HH:MM:SS figure.
func s0173Seconds(t *testing.T, v string) float64 {
	t.Helper()
	parts := strings.Split(v, ":")
	if len(parts) != 3 {
		t.Fatalf("%q is not an HH:MM:SS figure", v)
	}
	var sec float64
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			t.Fatalf("%q is not an HH:MM:SS figure", v)
		}
		sec = sec*60 + float64(n)
	}
	return sec
}

// s0173Rate reads a speed figure, "<decimal>x".
func s0173Rate(t *testing.T, v string) float64 {
	t.Helper()
	num, ok := strings.CutSuffix(v, "x")
	if !ok {
		t.Fatalf("speed %q does not end in x", v)
	}
	r, err := strconv.ParseFloat(num, 64)
	if err != nil || math.IsNaN(r) || math.IsInf(r, 0) {
		t.Fatalf("speed %q is not a decimal", v)
	}
	return r
}

// within10 reports whether got is within ten per cent of want.
func within10(got, want float64) bool { return math.Abs(got-want) <= 0.1*math.Abs(want) }

// s0173Library is one library root, its state directory and its configuration, with the
// stand-in encoder `run` finds through HOLDFAST_FFMPEG and a logging ffprobe it finds through
// HOLDFAST_FFPROBE.
type s0173Library struct {
	t                       *testing.T
	dir, lib, state, ctl    string
	cfgPath                 string
	realFFmpeg, realFFprobe string
	probeLog, toolLog       string

	mu    sync.Mutex
	fifos map[string]*os.File // the write end of each source's command channel, by base name
}

// s0173EncoderScript is the stand-in encoder. A job's encode is recognised by the progress
// channel the engine asks for, and waits for commands on its source's channel:
//
//	at <us>            one progress report at that position, in microseconds
//	blank              one progress report carrying no position, as ffmpeg writes before its first frame
//	close              close the progress channel without having written to it
//	burst <us> <n>     n reports from that position on, back to back, then touch <name>.burst
//	fail <code>        write an error and exit with that code
//	encode             hand the same argv to the pinned ffmpeg: a real encode
const s0173EncoderScript = `#!/bin/sh
case " $* " in
*" -progress pipe:3 "*) ;;
*) printf '%s\n' "$*" >> @TOOLLOG@; exec @REAL@ "$@" ;;
esac
in=; prev=; out=
for arg in "$@"; do
	if [ "$prev" = -i ]; then in=$arg; fi
	prev=$arg
	out=$arg
done
ctl=@CTL@/${in##*/}
printf '%s\n' "$out" > "$ctl.tmp" && mv "$ctl.tmp" "$ctl.started"
exec 4< "$ctl.fifo"
while read -r verb a b <&4; do
	case $verb in
	at) printf 'out_time_us=%s\nprogress=continue\n' "$a" >&3 ;;
	blank) printf 'out_time_us=N/A\nout_time=N/A\nprogress=continue\n' >&3 ;;
	close) exec 3>&- ;;
	burst)
		i=0
		while [ "$i" -lt "$b" ]; do
			printf 'out_time_us=%s\nprogress=continue\n' "$((a + i))" >&3
			i=$((i + 1))
		done
		: > "$ctl.burst" ;;
	fail) echo "the stand-in encoder failed on purpose" >&2; exit "$a" ;;
	encode) exec 4<&-; exec @REAL@ "$@" ;;
	esac
done
exit 70
`

// s0173ProbeScript is the real ffprobe, with every argv it is given written down first.
const s0173ProbeScript = `#!/bin/sh
printf '%s\n' "$*" >> @PROBELOG@
exec @REAL@ "$@"
`

func s0173ShQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// newS0173Library lays out a library holding sources (base name to fixture kind), a
// configuration with extra appended, and the stand-in tools, and points HOLDFAST_FFMPEG and
// HOLDFAST_FFPROBE at them for the rest of the test.
func newS0173Library(t *testing.T, extra string, sources map[string]string) *s0173Library {
	t.Helper()
	realFFmpeg, err := exec.LookPath(s0173Tools.ffmpeg)
	if err != nil {
		t.Fatalf("::error:: ffmpeg required for the run progress criteria: %v", err)
	}
	realFFprobe, err := exec.LookPath(s0173Tools.ffprobe)
	if err != nil {
		t.Fatalf("::error:: ffprobe required for the run progress criteria: %v", err)
	}
	dir := t.TempDir()
	l := &s0173Library{
		t: t, dir: dir,
		lib: filepath.Join(dir, "media"), state: filepath.Join(dir, "state"), ctl: filepath.Join(dir, "ctl"),
		cfgPath: filepath.Join(dir, "config.yaml"), realFFmpeg: realFFmpeg, realFFprobe: realFFprobe,
		probeLog: filepath.Join(dir, "ffprobe.log"), toolLog: filepath.Join(dir, "ffmpeg.log"),
		fifos: map[string]*os.File{},
	}
	for _, d := range []string{l.lib, l.ctl} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, kind := range sources {
		l.build(filepath.Join(l.lib, name), kind)
		fifo := filepath.Join(l.ctl, name+".fifo")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatalf("mkfifo %s: %v", fifo, err)
		}
		// Read and write, so opening it never waits for the stand-in and it never sees EOF
		// until the test lets go of it.
		f, err := os.OpenFile(fifo, os.O_RDWR, 0)
		if err != nil {
			t.Fatalf("open %s: %v", fifo, err)
		}
		l.fifos[name] = f
	}
	t.Cleanup(l.releaseAll)

	encoder := strings.NewReplacer("@TOOLLOG@", s0173ShQuote(l.toolLog), "@REAL@", s0173ShQuote(realFFmpeg),
		"@CTL@", s0173ShQuote(l.ctl)).Replace(s0173EncoderScript)
	prober := strings.NewReplacer("@PROBELOG@", s0173ShQuote(l.probeLog), "@REAL@", s0173ShQuote(realFFprobe)).
		Replace(s0173ProbeScript)
	encoderPath, proberPath := filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "ffprobe")
	for path, body := range map[string]string{encoderPath: encoder, proberPath: prober} {
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOLDFAST_FFMPEG", encoderPath)
	t.Setenv("HOLDFAST_FFPROBE", proberPath)

	body := "library_roots:\n  - " + l.lib + "\nstate_dir: " + l.state +
		"\nvmaf_enable: false\nmin_bitrate_kbps: 0\npreset: ultrafast\nqueue_order: path\n" + extra
	if err := os.WriteFile(l.cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return l
}

// build writes one fixture of the given kind to path.
func (l *s0173Library) build(path, kind string) {
	l.t.Helper()
	clip := []string{"-f", "lavfi", "-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M", "-pix_fmt", "yuv420p"}
	long := func(seconds string) []string {
		return []string{"-f", "lavfi", "-i", "testsrc2=duration=" + seconds + ":size=160x120:rate=1",
			"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p"}
	}
	switch kind {
	case s0173Clip:
		l.ffmpeg(append(clip, "--", path)...)
	case s0173TenMinutes:
		l.ffmpeg(append(long("600"), "--", path)...)
	case s0173FiveMinutes:
		l.ffmpeg(append(long("300"), "--", path)...)
	case s0173NoDuration:
		// A Matroska muxer that cannot seek back writes no Duration element, so the
		// container reports none.
		f, err := os.Create(path)
		if err != nil {
			l.t.Fatal(err)
		}
		cmd := exec.Command(l.realFFmpeg, append([]string{"-hide_banner", "-loglevel", "error", "-y"},
			append(clip, "-f", "matroska", "-")...)...)
		cmd.Stdout = f
		var errb bytes.Buffer
		cmd.Stderr = &errb
		runErr := cmd.Run()
		if cerr := f.Close(); runErr == nil {
			runErr = cerr
		}
		if runErr != nil {
			l.t.Fatalf("building %s: %v\n%s", path, runErr, errb.String())
		}
	case s0173NegDuration:
		// A seekable Matroska file carries its length as an 8-byte float in the Duration
		// element (ID 0x4489, size byte 0x88), in milliseconds at the default timecode scale.
		// Overwriting it with -5000 gives a real container whose reported length is -5 s.
		l.ffmpeg(append(clip, "--", path)...)
		b, err := os.ReadFile(path)
		if err != nil {
			l.t.Fatal(err)
		}
		i := bytes.Index(b, []byte{0x44, 0x89, 0x88})
		if i < 0 {
			l.t.Fatalf("no 8-byte Duration element in %s", path)
		}
		binary.BigEndian.PutUint64(b[i+3:], math.Float64bits(-5000))
		if err := os.WriteFile(path, b, 0o644); err != nil {
			l.t.Fatal(err)
		}
	default:
		l.t.Fatalf("unknown fixture kind %q", kind)
	}
}

// ffmpeg runs the pinned ffmpeg to build a fixture.
func (l *s0173Library) ffmpeg(args ...string) {
	l.t.Helper()
	out, err := exec.Command(l.realFFmpeg, append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...).
		CombinedOutput()
	if err != nil {
		l.t.Fatalf("building a fixture: %v\n%s", err, out)
	}
}

// probedDuration is the container duration the real ffprobe reports for a source, as the text
// it prints.
func (l *s0173Library) probedDuration(name string) string {
	l.t.Helper()
	out, err := exec.Command(l.realFFprobe, "-v", "error", "-show_entries", "format=duration",
		"-of", "default=nw=1:nk=1", "--", l.source(name)).Output()
	if err != nil {
		l.t.Fatalf("probing %s: %v", name, err)
	}
	return strings.TrimSpace(string(out))
}

// source is the path the engine knows a source by.
func (l *s0173Library) source(name string) string { return filepath.Join(l.lib, name) }

// started waits until the encode of the named source has started, and returns its working file.
func (l *s0173Library) started(name string) string {
	l.t.Helper()
	marker := filepath.Join(l.ctl, name+".started")
	deadline := time.Now().Add(s0173Deadline)
	for {
		if b, err := os.ReadFile(marker); err == nil {
			return strings.TrimSpace(string(b))
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("the encode of %s never started", name)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// forget removes what an earlier encode of the named source left in the control directory, so
// the next encode of it is waited for afresh.
func (l *s0173Library) forget(name string) {
	l.t.Helper()
	for _, suffix := range []string{".started", ".burst"} {
		if err := os.Remove(filepath.Join(l.ctl, name+suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
			l.t.Fatal(err)
		}
	}
}

// send hands one command to the stand-in encoding the named source.
func (l *s0173Library) send(name, command string) {
	l.t.Helper()
	l.mu.Lock()
	f := l.fifos[name]
	l.mu.Unlock()
	if f == nil {
		l.t.Fatalf("no command channel for %s", name)
	}
	if _, err := f.WriteString(command + "\n"); err != nil {
		l.t.Fatalf("sending %q to the encode of %s: %v", command, name, err)
	}
}

// deliverPastThrottle sends one position report again and again for longer than the engine's
// one-report-per-second throttle, so the first copy it lets through is certain to carry this
// position rather than the one before it, and then gives that copy s0173Settle to arrive.
func (l *s0173Library) deliverPastThrottle(name, command string) {
	l.t.Helper()
	for end := time.Now().Add(2500 * time.Millisecond); time.Now().Before(end); {
		l.send(name, command)
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(s0173Settle)
}

// waitFile waits until the named marker exists in the control directory.
func (l *s0173Library) waitFile(name, what string) {
	l.t.Helper()
	deadline := time.Now().Add(s0173Deadline)
	for {
		if _, err := os.Stat(filepath.Join(l.ctl, name)); err == nil {
			return
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("waited %s for %s", s0173Deadline, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// releaseAll lets go of every command channel: a stand-in still waiting reads end of input and
// fails its encode, so a run a failed test left behind still finishes.
func (l *s0173Library) releaseAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for name, f := range l.fifos {
		_ = f.Close()
		delete(l.fifos, name)
	}
}

// s0173Run is one `holdfast run` in flight on its own goroutine.
type s0173Run struct {
	done chan struct{}
	code int
}

// start runs `holdfast run` over this library, writing to stdout and stderr, and makes sure the
// test does not end while it is still running.
func (l *s0173Library) start(stdout, stderr io.Writer, args ...string) *s0173Run {
	l.t.Helper()
	r := &s0173Run{done: make(chan struct{})}
	argv := append([]string{"run", "--config", l.cfgPath}, args...)
	go func() {
		defer close(r.done)
		r.code = dispatch(argv, stdout, stderr)
	}()
	l.t.Cleanup(func() {
		l.releaseAll()
		select {
		case <-r.done:
		case <-time.After(s0173Deadline):
			l.t.Errorf("run did not return within %s of the test ending", s0173Deadline)
		}
	})
	return r
}

// wait waits for the run to return and gives its exit code.
func (r *s0173Run) wait(t *testing.T) int {
	t.Helper()
	select {
	case <-r.done:
		return r.code
	case <-time.After(s0173Deadline):
		t.Fatalf("run did not return within %s", s0173Deadline)
		return -1
	}
}

// returned reports whether the run has returned.
func (r *s0173Run) returned() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// s0173Outcome is what a run left behind, in terms another library can be compared on: every
// ledger row by file name with its status and reason, and every file left in the library with
// the codec of its video.
type s0173Outcome struct {
	rows, files []string
}

func (l *s0173Library) outcome() s0173Outcome {
	l.t.Helper()
	var o s0173Outcome
	for _, j := range l.jobs() {
		o.rows = append(o.rows, filepath.Base(j.Path)+" "+string(j.Status)+" "+j.Outcome.Reason)
	}
	entries, err := os.ReadDir(l.lib)
	if err != nil {
		l.t.Fatal(err)
	}
	for _, e := range entries {
		out, err := exec.Command(l.realFFprobe, "-v", "error", "-select_streams", "v:0", "-show_entries",
			"stream=codec_name", "-of", "default=nw=1:nk=1", "--", filepath.Join(l.lib, e.Name())).Output()
		if err != nil {
			l.t.Fatalf("probing %s: %v", e.Name(), err)
		}
		o.files = append(o.files, e.Name()+" "+strings.TrimSpace(string(out)))
	}
	sort.Strings(o.rows)
	sort.Strings(o.files)
	return o
}

// jobs is every ledger row, read through the read-only door while the run may still hold the
// write handle.
func (l *s0173Library) jobs() []store.Job {
	l.t.Helper()
	st, err := store.OpenReadOnly(filepath.Join(l.state, "jobs.db"))
	if err != nil {
		l.t.Fatalf("opening the ledger: %v", err)
	}
	defer func() { _ = st.Close() }()
	jobs, err := st.List(context.Background(), nil, 0)
	if err != nil {
		l.t.Fatalf("reading the ledger: %v", err)
	}
	return jobs
}

// waitTerminal waits until the ledger holds rows and every one of them is terminal.
func (l *s0173Library) waitTerminal() {
	l.t.Helper()
	active := map[store.Status]bool{store.Pending: true, store.Probing: true, store.Encoding: true, store.Verifying: true}
	deadline := time.Now().Add(s0173Deadline)
	for {
		jobs := l.jobs()
		settled := len(jobs) > 0
		for _, j := range jobs {
			if active[j.Status] || j.Status == "" {
				settled = false
			}
		}
		if settled {
			return
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("the ledger never settled into terminal rows: %+v", jobs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// s0173Unknown asserts a line says nothing it did not measure: every figure unknown.
func s0173Unknown(t *testing.T, l s0173Line) {
	t.Helper()
	if l.position != "unknown" || l.speed != "unknown" || l.eta != "unknown" {
		t.Errorf("an encode with no reported position must read position=unknown speed=unknown eta=unknown, "+
			"never a fabricated zero; got %q", l.raw)
	}
}

// TestS0173AC1_OneLinePerEncodeAtEachInterval grades AC-1: with an encode in flight, each
// interval that elapses writes exactly one progress line for it, naming its source, and the
// next interval writes another. The source's name holds a space, and the line naming it is
// still exactly one line. It is graded exactly on a stood-in clock, and then on the clock the
// build ships, at a shortened interval, so the lines are known to come on real time too.
func TestS0173AC1_OneLinePerEncodeAtEachInterval(t *testing.T) {
	const name = "a movie.mkv"
	t.Run("exactly one line at each interval", func(t *testing.T) {
		clock := useS0173Clock(t)
		lib := newS0173Library(t, "", map[string]string{name: s0173Clip})
		var stdout, stderr s0173Capture
		run := lib.start(&stdout, &stderr)
		lib.started(name)
		lib.send(name, "at 1500000")
		time.Sleep(s0173Settle)

		for i := 1; i <= 3; i++ {
			clock.advance(runProgressEvery)
			waitS0173Lines(t, &stderr, lib.source(name), i)
			time.Sleep(s0173Quiet)
			if got := s0173For(stderr.String(), lib.source(name)); len(got) != i {
				t.Fatalf("%d interval(s) elapsed with the encode in flight and %d progress lines name it, want %d:\n%s",
					i, len(got), i, stderr.String())
			}
		}
		lib.send(name, "fail 1")
		if code := run.wait(t); code != exitOK {
			t.Fatalf("run exited %d, want %d", code, exitOK)
		}
		if all := s0173Lines(stderr.String()); len(all) != 3 {
			t.Fatalf("%d progress lines in all, want the 3 naming %s:\n%s", len(all), name, stderr.String())
		}
	})

	t.Run("on the system clock", func(t *testing.T) {
		if _, ok := runProgressClock.(systemClock); !ok {
			t.Fatalf("run ships the progress clock %T, want the system clock", runProgressClock)
		}
		was := runProgressEvery
		runProgressEvery = 100 * time.Millisecond
		t.Cleanup(func() { runProgressEvery = was })
		lib := newS0173Library(t, "", map[string]string{name: s0173Clip})
		var stdout, stderr s0173Capture
		run := lib.start(&stdout, &stderr)
		lib.started(name)
		lib.send(name, "at 1500000")
		for _, l := range waitS0173Lines(t, &stderr, lib.source(name), 3) {
			if l.position != "unknown" && l.position != "00:00:01" {
				t.Errorf("position = %q, want 00:00:01 or, before the report arrived, unknown: %q", l.position, l.raw)
			}
		}
		lib.send(name, "fail 1")
		if code := run.wait(t); code != exitOK {
			t.Fatalf("run exited %d, want %d", code, exitOK)
		}
	})
}

// TestS0173AC2_ShippedIntervalIsThirtySeconds grades AC-2: with no override of the interval, no
// progress line is written less than thirty seconds after the run starts, and successive lines
// for one encode are thirty seconds apart.
func TestS0173AC2_ShippedIntervalIsThirtySeconds(t *testing.T) {
	if runProgressEvery != 30*time.Second {
		t.Fatalf("run ships a progress interval of %s, want 30s", runProgressEvery)
	}
	clock := useS0173Clock(t)
	const name = "a.mkv"
	lib := newS0173Library(t, "", map[string]string{name: s0173Clip})
	var stdout, stderr s0173Capture
	run := lib.start(&stdout, &stderr)
	lib.started(name)
	lib.send(name, "at 1000000")
	time.Sleep(s0173Settle)

	quietUntil := func(elapsed time.Duration, want int) {
		t.Helper()
		time.Sleep(s0173Quiet)
		if got := s0173For(stderr.String(), lib.source(name)); len(got) != want {
			t.Fatalf("%d progress line(s) %s after the run started, want %d:\n%s", len(got), elapsed, want, stderr.String())
		}
	}
	clock.advance(30*time.Second - time.Nanosecond)
	quietUntil(30*time.Second-time.Nanosecond, 0)
	clock.advance(time.Nanosecond)
	waitS0173Lines(t, &stderr, lib.source(name), 1)
	clock.advance(30*time.Second - time.Nanosecond)
	quietUntil(60*time.Second-time.Nanosecond, 1)
	clock.advance(time.Nanosecond)
	waitS0173Lines(t, &stderr, lib.source(name), 2)

	lib.send(name, "fail 1")
	if code := run.wait(t); code != exitOK {
		t.Fatalf("run exited %d, want %d", code, exitOK)
	}
	if got := s0173For(stderr.String(), lib.source(name)); len(got) != 2 {
		t.Fatalf("%d progress lines over sixty seconds, want 2:\n%s", len(got), stderr.String())
	}
	if asked := clock.asked(); len(asked) != 1 || asked[0] != 30*time.Second {
		t.Fatalf("the reporter ticked at %v, want one ticker at 30s", asked)
	}
}

// TestS0173AC3_PositionIsTheEncodersNotTheWorkingFiles grades AC-3: the encoder reports a
// position of 01:23:45 while its working file holds nothing at all, the line says 01:23:45, and
// no ffprobe or other ffmpeg invocation touches the working file to produce it.
func TestS0173AC3_PositionIsTheEncodersNotTheWorkingFiles(t *testing.T) {
	clock := useS0173Clock(t)
	const name = "a.mkv"
	lib := newS0173Library(t, "", map[string]string{name: s0173Clip})
	var stdout, stderr s0173Capture
	run := lib.start(&stdout, &stderr)
	work := lib.started(name)
	lib.send(name, "at 5025400000")
	time.Sleep(s0173Settle)
	if fi, err := os.Stat(work); err == nil && fi.Size() > 0 {
		t.Fatalf("the working file %s holds %d bytes; the stand-in wrote none", work, fi.Size())
	}

	clock.advance(runProgressEvery)
	line := waitS0173Lines(t, &stderr, lib.source(name), 1)[0]
	if line.position != "01:23:45" {
		t.Errorf("position = %q, want the encoder's reported 01:23:45: %q", line.position, line.raw)
	}
	lib.send(name, "fail 1")
	if code := run.wait(t); code != exitOK {
		t.Fatalf("run exited %d, want %d", code, exitOK)
	}
	for _, record := range []string{lib.probeLog, lib.toolLog} {
		b, err := os.ReadFile(record)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		for _, argv := range strings.Split(string(b), "\n") {
			if strings.Contains(argv, work) {
				t.Errorf("%s was run against the working file: %s", filepath.Base(record), argv)
			}
		}
	}
}

// TestS0173AC4_SpeedAndETAFollowASteadyRate grades AC-4: a position advancing at a steady 2.5
// source-seconds per wall-second through a source of known length reads as speed=2.50x, and as
// an ETA of (duration - position) / 2.5, on every line.
func TestS0173AC4_SpeedAndETAFollowASteadyRate(t *testing.T) {
	clock := useS0173Clock(t)
	const name = "a.mkv"
	lib := newS0173Library(t, "", map[string]string{name: s0173TenMinutes})
	duration, err := strconv.ParseFloat(lib.probedDuration(name), 64)
	if err != nil || duration <= 0 {
		t.Fatalf("the fixture's duration is %q, want a positive length", lib.probedDuration(name))
	}
	var stdout, stderr s0173Capture
	run := lib.start(&stdout, &stderr)
	lib.started(name)

	const rate = 2.5
	step := runProgressEvery.Seconds() * rate
	lib.send(name, "at "+strconv.Itoa(int(step*1e6)))
	time.Sleep(s0173Settle)
	clock.advance(runProgressEvery)
	waitS0173Lines(t, &stderr, lib.source(name), 1)
	lib.deliverPastThrottle(name, "at "+strconv.Itoa(int(2*step*1e6)))
	clock.advance(runProgressEvery)
	lines := waitS0173Lines(t, &stderr, lib.source(name), 2)

	for i, l := range lines {
		position := s0173Seconds(t, l.position)
		if want := float64(i+1) * step; position != want {
			t.Errorf("line %d: position %s, want %v s", i+1, l.position, want)
		}
		if got := s0173Rate(t, l.speed); !within10(got, rate) {
			t.Errorf("line %d: speed %s, want within 10%% of %vx", i+1, l.speed, rate)
		}
		if l.eta == "unknown" {
			t.Errorf("line %d: eta unknown for a known duration and a steady rate: %q", i+1, l.raw)
			continue
		}
		if got, want := s0173Seconds(t, l.eta), (duration-position)/rate; !within10(got, want) {
			t.Errorf("line %d: eta %s (%v s), want within 10%% of %v s", i+1, l.eta, got, want)
		}
	}
	lib.send(name, "fail 1")
	if code := run.wait(t); code != exitOK {
		t.Fatalf("run exited %d, want %d", code, exitOK)
	}
}

// s0173UnknownDuration is AC-5 over one source of the given kind, whose container length as the
// real ffprobe prints it is one reported accepts: the line still carries the measured position
// and speed, and eta=unknown.
func s0173UnknownDuration(t *testing.T, kind string, reported func(string) bool) {
	t.Helper()
	clock := useS0173Clock(t)
	const name = "a.mkv"
	lib := newS0173Library(t, "", map[string]string{name: kind})
	if d := lib.probedDuration(name); !reported(d) {
		t.Fatalf("the fixture's container reports duration %q, which is not the case under test", d)
	}
	var stdout, stderr s0173Capture
	run := lib.start(&stdout, &stderr)
	lib.started(name)
	lib.send(name, "at 60000000")
	time.Sleep(s0173Settle)
	clock.advance(runProgressEvery)
	line := waitS0173Lines(t, &stderr, lib.source(name), 1)[0]
	if line.position != "00:01:00" {
		t.Errorf("position = %q, want the measured 00:01:00: %q", line.position, line.raw)
	}
	if got := s0173Rate(t, line.speed); !within10(got, 2) {
		t.Errorf("speed = %q, want the measured 2.00x: %q", line.speed, line.raw)
	}
	if line.eta != "unknown" {
		t.Errorf("eta = %q with no usable source duration, want unknown: %q", line.eta, line.raw)
	}
	lib.send(name, "fail 1")
	if code := run.wait(t); code != exitOK {
		t.Fatalf("run exited %d, want %d", code, exitOK)
	}
}

// TestS0173AC5_ETAUnknownWhenTheProbeReportsNoDuration grades AC-5 where the probe reports no
// duration at all.
func TestS0173AC5_ETAUnknownWhenTheProbeReportsNoDuration(t *testing.T) {
	s0173UnknownDuration(t, s0173NoDuration, func(d string) bool { return d == "N/A" })
}

// TestS0173AC5_ETAUnknownWhenTheProbeReportsANegativeDuration grades AC-5 where the probe reports
// a duration below zero.
func TestS0173AC5_ETAUnknownWhenTheProbeReportsANegativeDuration(t *testing.T) {
	s0173UnknownDuration(t, s0173NegDuration, func(d string) bool {
		v, err := strconv.ParseFloat(d, 64)
		return err == nil && v <= 0
	})
}

// s0173NoPosition is AC-6 over one encode in flight whose stand-in did what prepare says before
// any position reached the reporter: two intervals, two lines, both naming the file and both
// unknown throughout.
func s0173NoPosition(t *testing.T, prepare string) {
	t.Helper()
	clock := useS0173Clock(t)
	const name = "a.mkv"
	lib := newS0173Library(t, "", map[string]string{name: s0173Clip})
	var stdout, stderr s0173Capture
	run := lib.start(&stdout, &stderr)
	lib.started(name)
	if prepare != "" {
		lib.send(name, prepare)
	}
	time.Sleep(s0173Settle)
	for i := 1; i <= 2; i++ {
		clock.advance(runProgressEvery)
		waitS0173Lines(t, &stderr, lib.source(name), i)
	}
	lib.send(name, "fail 1")
	if code := run.wait(t); code != exitOK {
		t.Fatalf("run exited %d, want %d", code, exitOK)
	}
	lines := s0173For(stderr.String(), lib.source(name))
	if len(lines) != 2 {
		t.Fatalf("%d progress lines over two intervals, want 2:\n%s", len(lines), stderr.String())
	}
	for _, l := range lines {
		s0173Unknown(t, l)
	}
}

// TestS0173AC6_UnknownFiguresWhenTheEncodeHasJustStarted grades AC-6 for an encode that has
// entered `encoding` and reported nothing yet.
func TestS0173AC6_UnknownFiguresWhenTheEncodeHasJustStarted(t *testing.T) {
	s0173NoPosition(t, "")
}

// TestS0173AC6_UnknownFiguresWhenTheProgressChannelIsClosed grades AC-6 for an encode whose
// progress channel was closed before anything was written to it, so that no report can arrive.
func TestS0173AC6_UnknownFiguresWhenTheProgressChannelIsClosed(t *testing.T) {
	s0173NoPosition(t, "close")
}

// TestS0173AC6_UnknownFiguresWhenTheEncoderReportsNoPosition grades AC-6 for an encoder whose
// reports carry no position, as ffmpeg's do before its first frame.
func TestS0173AC6_UnknownFiguresWhenTheEncoderReportsNoPosition(t *testing.T) {
	s0173NoPosition(t, "blank")
}

// TestS0173AC7_StalledEncodeReadsZeroSpeed grades AC-7: once the reported position stops
// advancing, each line repeats that position with speed=0.00x and eta=unknown.
func TestS0173AC7_StalledEncodeReadsZeroSpeed(t *testing.T) {
	clock := useS0173Clock(t)
	const name = "a.mkv"
	lib := newS0173Library(t, "", map[string]string{name: s0173TenMinutes})
	var stdout, stderr s0173Capture
	run := lib.start(&stdout, &stderr)
	lib.started(name)
	lib.send(name, "at 75000000")
	time.Sleep(s0173Settle)
	for i := 1; i <= 3; i++ {
		clock.advance(runProgressEvery)
		waitS0173Lines(t, &stderr, lib.source(name), i)
	}
	lib.send(name, "fail 1")
	if code := run.wait(t); code != exitOK {
		t.Fatalf("run exited %d, want %d", code, exitOK)
	}
	lines := s0173For(stderr.String(), lib.source(name))
	if len(lines) != 3 {
		t.Fatalf("%d progress lines, want 3:\n%s", len(lines), stderr.String())
	}
	if lines[0].speed == "0.00x" {
		t.Fatalf("the first line had advanced from the start and must not read as stalled: %q", lines[0].raw)
	}
	for _, l := range lines[1:] {
		if l.position != lines[0].position || l.speed != "0.00x" || l.eta != "unknown" {
			t.Errorf("a stalled encode must read position=%s speed=0.00x eta=unknown, got %q", lines[0].position, l.raw)
		}
	}
}

// TestS0173AC7_SlowEncodeNeverReadsAsStalled is AC-7's other side, and the objective's: a
// position that did advance, by one ten-thousandth of a second over thirty, is a slow encode and
// never prints the stalled encode's speed=0.00x.
func TestS0173AC7_SlowEncodeNeverReadsAsStalled(t *testing.T) {
	clock := useS0173Clock(t)
	const name = "a.mkv"
	lib := newS0173Library(t, "", map[string]string{name: s0173TenMinutes})
	var stdout, stderr s0173Capture
	run := lib.start(&stdout, &stderr)
	lib.started(name)
	lib.send(name, "at 75000000")
	time.Sleep(s0173Settle)
	clock.advance(runProgressEvery)
	waitS0173Lines(t, &stderr, lib.source(name), 1)
	lib.deliverPastThrottle(name, "at 75000100")
	clock.advance(runProgressEvery)
	slow := waitS0173Lines(t, &stderr, lib.source(name), 2)[1]
	if slow.speed == "0.00x" || !(s0173Rate(t, slow.speed) > 0) {
		t.Errorf("an encode that advanced reads speed=%s, which is the stalled figure: %q", slow.speed, slow.raw)
	}
	lib.send(name, "fail 1")
	if code := run.wait(t); code != exitOK {
		t.Fatalf("run exited %d, want %d", code, exitOK)
	}
}

// TestS0173AC8_EachEncodeInFlightGetsItsOwnLine grades AC-8: two encodes in flight at once each
// get their own line at the interval, carrying their own file and their own figures.
func TestS0173AC8_EachEncodeInFlightGetsItsOwnLine(t *testing.T) {
	clock := useS0173Clock(t)
	lib := newS0173Library(t, "workers: 2\n", map[string]string{"a.mkv": s0173TenMinutes, "b.mkv": s0173FiveMinutes})
	var stdout, stderr s0173Capture
	run := lib.start(&stdout, &stderr)
	lib.started("a.mkv")
	lib.started("b.mkv")
	lib.send("a.mkv", "at 30000000")
	lib.send("b.mkv", "at 90000000")
	time.Sleep(s0173Settle)
	clock.advance(runProgressEvery)

	want := map[string]s0173Line{
		"a.mkv": {position: "00:00:30", speed: "1.00x", eta: "00:09:30"},
		"b.mkv": {position: "00:01:30", speed: "3.00x", eta: "00:01:10"},
	}
	for name, w := range want {
		got := waitS0173Lines(t, &stderr, lib.source(name), 1)[0]
		if got.position != w.position || got.speed != w.speed || got.eta != w.eta {
			t.Errorf("%s: position=%s speed=%s eta=%s, want its own position=%s speed=%s eta=%s",
				name, got.position, got.speed, got.eta, w.position, w.speed, w.eta)
		}
	}
	if all := s0173Lines(stderr.String()); len(all) != 2 {
		t.Errorf("%d progress lines for two encodes at one interval, want 2:\n%s", len(all), stderr.String())
	}
	lib.send("a.mkv", "fail 1")
	lib.send("b.mkv", "fail 1")
	if code := run.wait(t); code != exitOK {
		t.Fatalf("run exited %d, want %d", code, exitOK)
	}
}

// s0173AfterLeaving is AC-9 for an encode that leaves `encoding` by the command given: a.mkv is
// reported on, leaves, and b.mkv - the one worker's next encode - is in flight at the next
// interval. That interval writes one line, for b.mkv, and it carries nothing of a.mkv's. Lines
// are written in path order, so a line for a.mkv would already be there when b.mkv's appears.
func s0173AfterLeaving(t *testing.T, leave string) {
	t.Helper()
	clock := useS0173Clock(t)
	lib := newS0173Library(t, "", map[string]string{"a.mkv": s0173Clip, "b.mkv": s0173Clip})
	var stdout, stderr s0173Capture
	run := lib.start(&stdout, &stderr)
	lib.started("a.mkv")
	lib.send("a.mkv", "at 1500000")
	time.Sleep(s0173Settle)
	clock.advance(runProgressEvery)
	waitS0173Lines(t, &stderr, lib.source("a.mkv"), 1)

	lib.send("a.mkv", leave)
	lib.started("b.mkv")
	clock.advance(runProgressEvery)
	b := waitS0173Lines(t, &stderr, lib.source("b.mkv"), 1)[0]
	if a := s0173For(stderr.String(), lib.source("a.mkv")); len(a) != 1 {
		t.Errorf("%d progress lines name a.mkv after its encode left encoding, want only the 1 before:\n%s",
			len(a), stderr.String())
	}
	s0173Unknown(t, b)
	lib.send("b.mkv", "fail 1")
	if code := run.wait(t); code != exitOK {
		t.Fatalf("run exited %d, want %d", code, exitOK)
	}
}

// TestS0173AC9_NoLineAfterTheEncodeFinished grades AC-9 for an encode that finished: a real
// encode, verified and swapped in.
func TestS0173AC9_NoLineAfterTheEncodeFinished(t *testing.T) {
	s0173AfterLeaving(t, "encode")
}

// TestS0173AC9_NoLineAfterTheEncodeFailed grades AC-9 for an encode that failed.
func TestS0173AC9_NoLineAfterTheEncodeFailed(t *testing.T) {
	s0173AfterLeaving(t, "fail 1")
}

// TestS0173AC9_NoLineAfterTheEncodeWasCancelled grades AC-9 for an encode a SIGTERM cancelled:
// nothing more is written about it, and when the next run encodes the same file again, its
// first line carries nothing of the cancelled encode's.
func TestS0173AC9_NoLineAfterTheEncodeWasCancelled(t *testing.T) {
	clock := useS0173Clock(t)
	const name = "a.mkv"
	lib := newS0173Library(t, "", map[string]string{name: s0173Clip})
	var stdout, stderr s0173Capture
	run := lib.start(&stdout, &stderr)
	lib.started(name)
	lib.send(name, "at 1500000")
	time.Sleep(s0173Settle)
	clock.advance(runProgressEvery)
	waitS0173Lines(t, &stderr, lib.source(name), 1)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := run.wait(t); code != exitOK {
		t.Fatalf("interrupted run exited %d, want %d", code, exitOK)
	}
	for i := 0; i < 3; i++ {
		clock.advance(runProgressEvery)
	}
	time.Sleep(s0173Quiet)
	if got := s0173For(stderr.String(), lib.source(name)); len(got) != 1 {
		t.Fatalf("%d progress lines name the cancelled encode, want only the 1 before the interrupt:\n%s",
			len(got), stderr.String())
	}

	lib.forget(name)
	var again s0173Capture
	next := lib.start(&stdout, &again)
	lib.started(name)
	clock.advance(runProgressEvery)
	s0173Unknown(t, waitS0173Lines(t, &again, lib.source(name), 1)[0])
	lib.send(name, "fail 1")
	if code := next.wait(t); code != exitOK {
		t.Fatalf("the next run exited %d, want %d", code, exitOK)
	}
}

// TestS0173AC10_ProgressGoesToStderrOnly grades AC-10: the progress lines are on stderr, and
// stdout carries none.
func TestS0173AC10_ProgressGoesToStderrOnly(t *testing.T) {
	clock := useS0173Clock(t)
	const name = "a.mkv"
	lib := newS0173Library(t, "", map[string]string{name: s0173Clip})
	var stdout, stderr s0173Capture
	run := lib.start(&stdout, &stderr)
	lib.started(name)
	lib.send(name, "at 1000000")
	time.Sleep(s0173Settle)
	for i := 1; i <= 2; i++ {
		clock.advance(runProgressEvery)
		waitS0173Lines(t, &stderr, lib.source(name), i)
	}
	lib.send(name, "fail 1")
	if code := run.wait(t); code != exitOK {
		t.Fatalf("run exited %d, want %d", code, exitOK)
	}
	if got := s0173Lines(stdout.String()); len(got) != 0 {
		t.Fatalf("stdout carries %d progress line(s):\n%s", len(got), stdout.String())
	}
}

// TestS0173AC11_ProgressIgnoresTheLogLevel grades AC-11: at log_level warn and at log_level
// error, the progress lines are still written.
func TestS0173AC11_ProgressIgnoresTheLogLevel(t *testing.T) {
	for _, level := range []string{"warn", "error"} {
		t.Run(level, func(t *testing.T) {
			clock := useS0173Clock(t)
			const name = "a.mkv"
			lib := newS0173Library(t, "log_level: "+level+"\n", map[string]string{name: s0173Clip})
			var stdout, stderr s0173Capture
			run := lib.start(&stdout, &stderr)
			lib.started(name)
			lib.send(name, "at 1000000")
			time.Sleep(s0173Settle)
			clock.advance(runProgressEvery)
			line := waitS0173Lines(t, &stderr, lib.source(name), 1)[0]
			if line.position != "00:00:01" {
				t.Errorf("position = %q, want 00:00:01: %q", line.position, line.raw)
			}
			lib.send(name, "fail 1")
			if code := run.wait(t); code != exitOK {
				t.Fatalf("run exited %d, want %d", code, exitOK)
			}
		})
	}
}

// s0173StoppedAtReturn asserts the reporter was stopped by the time run returned: no ticker of
// its is still running, and intervals that elapse afterwards write nothing.
func s0173StoppedAtReturn(t *testing.T, clock *s0173Clock, stderr *s0173Capture) {
	t.Helper()
	if n := clock.running(); n != 0 {
		t.Errorf("run returned with %d progress ticker(s) still running", n)
	}
	before := stderr.String()
	for i := 0; i < 3; i++ {
		clock.advance(runProgressEvery)
	}
	time.Sleep(s0173Quiet)
	if after := stderr.String(); after != before {
		t.Errorf("progress was written after run returned:\n%s", strings.TrimPrefix(after, before))
	}
}

// TestS0173AC12_ReporterStoppedWhenTheScanCompletes grades AC-12 for a run whose scan completed.
func TestS0173AC12_ReporterStoppedWhenTheScanCompletes(t *testing.T) {
	clock := useS0173Clock(t)
	const name = "a.mkv"
	lib := newS0173Library(t, "", map[string]string{name: s0173Clip})
	var stdout, stderr s0173Capture
	run := lib.start(&stdout, &stderr)
	lib.started(name)
	lib.send(name, "at 1000000")
	time.Sleep(s0173Settle)
	clock.advance(runProgressEvery)
	waitS0173Lines(t, &stderr, lib.source(name), 1)
	lib.send(name, "fail 1")
	if code := run.wait(t); code != exitOK {
		t.Fatalf("run exited %d, want %d", code, exitOK)
	}
	s0173StoppedAtReturn(t, clock, &stderr)
}

// TestS0173AC12_ReporterStoppedOnARunError grades AC-12 for a run that returns an error. The
// engine's pass surfaces no error but an interrupt, so the run errors an operator can meet are
// the ones before the pass: here, the configured ffmpeg does not exist.
func TestS0173AC12_ReporterStoppedOnARunError(t *testing.T) {
	clock := useS0173Clock(t)
	const name = "a.mkv"
	lib := newS0173Library(t, "", map[string]string{name: s0173Clip})
	t.Setenv("HOLDFAST_FFMPEG", filepath.Join(lib.dir, "no-such-ffmpeg"))
	var stdout, stderr s0173Capture
	if code := dispatch([]string{"run", "--config", lib.cfgPath}, &stdout, &stderr); code != exitError {
		t.Fatalf("run with no ffmpeg exited %d, want %d (stderr: %s)", code, exitError, stderr.String())
	}
	s0173StoppedAtReturn(t, clock, &stderr)
	if got := s0173Lines(stderr.String()); len(got) != 0 {
		t.Errorf("a run that encoded nothing wrote %d progress line(s):\n%s", len(got), stderr.String())
	}
}

// TestS0173AC12_ReporterStoppedOnAnInterrupt grades AC-12 for a run a SIGINT interrupted with an
// encode in flight. That encode leaves `encoding` without a transition the engine reports, so a
// reporter still running after the return would go on naming it.
func TestS0173AC12_ReporterStoppedOnAnInterrupt(t *testing.T) {
	clock := useS0173Clock(t)
	const name = "a.mkv"
	lib := newS0173Library(t, "", map[string]string{name: s0173Clip})
	var stdout, stderr s0173Capture
	run := lib.start(&stdout, &stderr)
	lib.started(name)
	lib.send(name, "at 1000000")
	time.Sleep(s0173Settle)
	clock.advance(runProgressEvery)
	waitS0173Lines(t, &stderr, lib.source(name), 1)
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if code := run.wait(t); code != exitOK {
		t.Fatalf("interrupted run exited %d, want %d", code, exitOK)
	}
	s0173StoppedAtReturn(t, clock, &stderr)
}

// s0173CostlyWrite is AC-13's scenario over one real encode: a position reported, two intervals
// with a line due at each, and the encode carried through the engine's gates and swap. It
// returns the run so the caller can hold it. midway, when non-nil, runs after the first
// interval and before the encode is finished.
func s0173CostlyWrite(t *testing.T, clock *s0173Clock, lib *s0173Library, stderr io.Writer, midway func()) *s0173Run {
	t.Helper()
	var stdout s0173Capture
	run := lib.start(&stdout, stderr)
	lib.started("a.mkv")
	lib.send("a.mkv", "at 1000000")
	time.Sleep(s0173Settle)
	clock.advance(runProgressEvery)
	if midway != nil {
		midway()
	}
	clock.advance(runProgressEvery)
	lib.send("a.mkv", "encode")
	return run
}

// s0173Baseline is the outcome AC-13's scenario reaches with a stderr that writes.
func s0173Baseline(t *testing.T, clock *s0173Clock) s0173Outcome {
	t.Helper()
	lib := newS0173Library(t, "", map[string]string{"a.mkv": s0173Clip})
	var stderr s0173Capture
	run := s0173CostlyWrite(t, clock, lib, &stderr, nil)
	if code := run.wait(t); code != exitOK {
		t.Fatalf("run exited %d, want %d", code, exitOK)
	}
	o := lib.outcome()
	if len(o.rows) != 1 || strings.Fields(o.rows[0])[1] != string(store.Done) || len(o.files) != 1 ||
		o.files[0] != "a.mkv hevc" {
		t.Fatalf("the baseline did not transcode and swap: rows %q, files %q", o.rows, o.files)
	}
	return o
}

// s0173FailingWriter is a stderr every write to which fails.
type s0173FailingWriter struct{ attempts atomic.Int64 }

func (w *s0173FailingWriter) Write([]byte) (int, error) {
	w.attempts.Add(1)
	return 0, errors.New("stderr is gone")
}

// TestS0173AC13_FailingStderrCostsNoEncode grades AC-13 for a stderr whose every write fails:
// the encode in flight runs to the same outcome, a transcoded and swapped file, it reaches with
// a stderr that writes.
func TestS0173AC13_FailingStderrCostsNoEncode(t *testing.T) {
	clock := useS0173Clock(t)
	want := s0173Baseline(t, clock)

	lib := newS0173Library(t, "", map[string]string{"a.mkv": s0173Clip})
	var stderr s0173FailingWriter
	run := s0173CostlyWrite(t, clock, lib, &stderr, func() {
		deadline := time.Now().Add(s0173Deadline)
		for stderr.attempts.Load() == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("no progress line was attempted in %s", s0173Deadline)
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	if code := run.wait(t); code != exitOK {
		t.Fatalf("run exited %d with a failing stderr, want %d", code, exitOK)
	}
	if got := lib.outcome(); !slices.Equal(got.rows, want.rows) || !slices.Equal(got.files, want.files) {
		t.Fatalf("with a failing stderr the run reached rows %q, files %q; with a writable one, rows %q, files %q",
			got.rows, got.files, want.rows, want.files)
	}
}

// s0173HeldWriter is a stderr whose first write is held open until the test frees it; every
// later write goes through.
type s0173HeldWriter struct {
	s0173Capture
	entered, freed    chan struct{}
	enterOnce, freeOn sync.Once
}

func (w *s0173HeldWriter) Write(p []byte) (int, error) {
	w.enterOnce.Do(func() {
		close(w.entered)
		<-w.freed
	})
	return w.s0173Capture.Write(p)
}

func (w *s0173HeldWriter) free() { w.freeOn.Do(func() { close(w.freed) }) }

// TestS0173AC13_HeldStderrCostsNoEncode grades AC-13 for a stderr write held open until the job
// has reached its terminal status. While it is held, the encoder writes more progress than its
// pipe can buffer - which completes only if the drain is still reading - and the encode is
// carried through its gates and swap to the outcome a writable stderr gets. Freed, the write
// completes, and only then does run return.
func TestS0173AC13_HeldStderrCostsNoEncode(t *testing.T) {
	clock := useS0173Clock(t)
	want := s0173Baseline(t, clock)

	lib := newS0173Library(t, "", map[string]string{"a.mkv": s0173Clip})
	stderr := &s0173HeldWriter{entered: make(chan struct{}), freed: make(chan struct{})}
	t.Cleanup(stderr.free)
	run := s0173CostlyWrite(t, clock, lib, stderr, func() {
		select {
		case <-stderr.entered:
		case <-time.After(s0173Deadline):
			t.Fatalf("no progress line was written in %s", s0173Deadline)
		}
		// Past the engine's one-report-per-second throttle since the last report, so reports
		// in this burst reach the reporter while its write is held.
		time.Sleep(time.Second)
		lib.send("a.mkv", "burst 1000001 4000")
		lib.waitFile("a.mkv.burst", "the stand-in to write 4000 progress reports while a progress line was held")
	})
	lib.waitTerminal()
	time.Sleep(s0173Quiet)
	if run.returned() {
		t.Fatal("run returned while its progress line was still being written")
	}
	stderr.free()
	if code := run.wait(t); code != exitOK {
		t.Fatalf("run exited %d, want %d", code, exitOK)
	}
	if got := lib.outcome(); !slices.Equal(got.rows, want.rows) || !slices.Equal(got.files, want.files) {
		t.Fatalf("with a held stderr the run reached rows %q, files %q; with a writable one, rows %q, files %q",
			got.rows, got.files, want.rows, want.files)
	}
}

// TestS0173AC15_ServeWritesNoRunProgressLine grades AC-15: `serve` running an encode, with
// intervals elapsing and a position reported, writes no run-mode progress line to stderr and
// never starts run's reporter.
func TestS0173AC15_ServeWritesNoRunProgressLine(t *testing.T) {
	clock := useS0173Clock(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	const name = "a.mkv"
	lib := newS0173Library(t, "server_addr: "+addr+"\n", map[string]string{name: s0173Clip})
	cfg, err := config.Load(lib.cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var stderr s0173Capture
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var code int
	done := make(chan struct{})
	go func() {
		defer close(done)
		code = runServer(ctx, cfg, discardLog(), &stderr)
	}()
	t.Cleanup(func() {
		lib.releaseAll()
		cancel()
		select {
		case <-done:
		case <-time.After(s0173Deadline):
			t.Errorf("serve did not shut down within %s", s0173Deadline)
		}
	})
	lib.started(name)
	lib.send(name, "at 1000000")
	time.Sleep(s0173Settle)
	for i := 0; i < 3; i++ {
		clock.advance(runProgressEvery)
	}
	time.Sleep(s0173Quiet)
	lib.send(name, "fail 1")
	cancel()
	select {
	case <-done:
		if code != 0 {
			t.Fatalf("serve exited %d, want 0", code)
		}
	case <-time.After(s0173Deadline):
		t.Fatalf("serve did not shut down within %s", s0173Deadline)
	}
	if got := s0173Lines(stderr.String()); len(got) != 0 {
		t.Errorf("serve wrote %d run-mode progress line(s) to stderr:\n%s", len(got), stderr.String())
	}
	if asked := clock.asked(); len(asked) != 0 {
		t.Errorf("serve started run's progress reporter (tickers at %v)", asked)
	}
}
