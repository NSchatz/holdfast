package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// S0163: several jobs in flight at once on one drive. Every case forces its interleaving
// through a seam - the encoder, the room-wait hook, the target-wait hook, the retention-area
// hook - rather than leaving it to timing, and the free-space and filesystem-identity reads
// are substituted where a case needs a filesystem this runner does not have. Fixtures are one
// tiny real source per case, copied, so the guards probe real media without an encode each.

// s0163Guard bounds every wait a forced interleaving makes, so a regression reds instead of
// hanging the package for the whole test timeout.
const s0163Guard = 60 * time.Second

// s0163Copies writes one real 2-second source and copies it to each path, returning its size.
func s0163Copies(t *testing.T, ffmpeg string, paths ...string) int64 {
	t.Helper()
	seed := filepath.Join(t.TempDir(), "seed.mkv")
	mkH264(t, ffmpeg, seed, "8M")
	b, err := os.ReadFile(seed)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return int64(len(b))
}

// s0163Encoded is one real production encode of src, made once so a case can hand every job
// a gate-passing output without paying for an encode per job.
func s0163Encoded(t *testing.T, ffmpeg, ffprobe, src string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "encoded.mkv")
	enc := FFmpegEncoder{FFmpeg: ffmpeg, Cfg: baseCfg(filepath.Dir(src)), Probe: probe.New(ffmpeg, ffprobe)}
	if err := enc.Encode(context.Background(), src, out, nil); err != nil {
		t.Fatalf("the reference encode: %v", err)
	}
	return out
}

// copyInto writes the bytes at from to to.
func copyInto(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o644)
}

// inflight counts the encodes running at once and the most there ever were.
type inflight struct {
	mu        sync.Mutex
	now, most int
	calls     map[string]int // source -> encodes started
}

func (c *inflight) enter(src string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[src]++
	c.now++
	if c.now > c.most {
		c.most = c.now
	}
	return c.now
}

func (c *inflight) leave() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now--
}

func (c *inflight) peak() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.most
}

func (c *inflight) callsFor(src string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[src]
}

// closeOnce closes ch at most once, whoever gets there first.
type closeOnce struct {
	once sync.Once
	ch   chan struct{}
}

func newCloseOnce() *closeOnce { return &closeOnce{ch: make(chan struct{})} }
func (c *closeOnce) close()    { c.once.Do(func() { close(c.ch) }) }

// rowsByPath is every ledger row, grouped by path.
func rowsByPath(t *testing.T, st store.Store) map[string][]store.Job {
	t.Helper()
	rows, err := st.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	out := map[string][]store.Job{}
	for _, r := range rows {
		out[r.Path] = append(out[r.Path], r)
	}
	return out
}

// jsonRecords decodes a JSON log capture, one record per line.
func jsonRecords(t *testing.T, out string) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a log line is not one JSON record: %q: %v", line, err)
		}
		recs = append(recs, rec)
	}
	return recs
}

// TestS0163_AC1_TheEnginePoolRunsExactlyTheConfiguredWorkers grades AC-1's engine half:
// absent and 0 run ONE worker and a whole number runs exactly that many. The encoder holds
// every job until the configured number is inside it at once, so a pool narrower than the
// setting never gets there and one wider overshoots it.
func TestS0163_AC1_TheEnginePoolRunsExactlyTheConfiguredWorkers(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, tc := range []struct{ workers, want int }{{0, 1}, {1, 1}, {3, 3}} {
		t.Run(fmt.Sprintf("workers %d", tc.workers), func(t *testing.T) {
			root := t.TempDir()
			var srcs []string
			for i := 0; i < tc.want+2; i++ {
				srcs = append(srcs, filepath.Join(root, fmt.Sprintf("film%d.mkv", i)))
			}
			s0163Copies(t, ffmpeg, srcs...)
			count := &inflight{}
			full := newCloseOnce()
			enc := EncoderFunc(func(ctx context.Context, in, out string, props *probe.VideoProps) error {
				if count.enter(in) >= tc.want {
					full.close()
				}
				defer count.leave()
				select {
				case <-full.ch:
				case <-time.After(s0163Guard):
					return errors.New("the pool never had the configured number of encodes in flight at once")
				}
				return errFake
			})
			eng := buildEngine(t, ffmpeg, ffprobe, root, enc, func(c *config.Config) { c.Workers = tc.workers })
			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatalf("RunOneshot: %v", err)
			}
			if got := count.peak(); got != tc.want {
				t.Errorf("%d encodes ran at once, want exactly %d", got, tc.want)
			}
		})
	}
}

// TestS0163_AC8_EachSourceIsAdmittedOnceAcrossWorkersAndAcrossTwoEngines grades AC-8: with 4
// workers over 3 sources per worker - and again with two engines over one shared store and one
// library running passes at once - every source is started exactly once, encoded at most once
// and ends with exactly one terminal ledger row. -race reports any data race either way.
func TestS0163_AC8_EachSourceIsAdmittedOnceAcrossWorkersAndAcrossTwoEngines(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, engines := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d engine(s)", engines), func(t *testing.T) {
			root := t.TempDir()
			const workers, perWorker = 4, 3
			var srcs []string
			for i := 0; i < workers*perWorker; i++ {
				srcs = append(srcs, filepath.Join(root, fmt.Sprintf("dir%d", i%3), fmt.Sprintf("ep%02d.mkv", i)))
			}
			s0163Copies(t, ffmpeg, srcs...)
			count := &inflight{}
			enc := EncoderFunc(func(ctx context.Context, in, out string, props *probe.VideoProps) error {
				count.enter(in)
				defer count.leave()
				return errFake
			})
			var mu sync.Mutex
			started := map[string][]string{} // path -> the workers that started it
			observe := func(label string) Observer {
				return func(ev Event) {
					if ev.Status == store.Probing && ev.Worker != "" {
						mu.Lock()
						started[ev.Path] = append(started[ev.Path], label+"/"+ev.Worker)
						mu.Unlock()
					}
				}
			}

			cfg := baseCfg(root)
			cfg.Workers = workers
			// One failure parks a file, so a row one engine failed is terminal for the
			// other too and "admitted once" is not muddied by the ledger's own retry.
			cfg.MaxFailures = 1
			ts := newTestStore(t, root)
			prober := probe.New(ffmpeg, ffprobe)
			var engs []*Engine
			for i := 0; i < engines; i++ {
				e := New(cfg, prober, enc, ts, discardLogger())
				e.Observer = observe(fmt.Sprintf("engine%d", i))
				engs = append(engs, e)
			}
			// Both passes start together, so the claims race for every file.
			var wg sync.WaitGroup
			begin := make(chan struct{})
			errs := make([]error, len(engs))
			for i, e := range engs {
				wg.Add(1)
				go func(i int, e *Engine) {
					defer wg.Done()
					<-begin
					errs[i] = e.RunOneshot(context.Background())
				}(i, e)
			}
			close(begin)
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("engine %d: RunOneshot: %v", i, err)
				}
			}

			rows := rowsByPath(t, ts)
			for _, src := range srcs {
				if got := started[src]; len(got) != 1 {
					t.Errorf("%s was started %d time(s) (%v), want exactly once", src, len(got), got)
				}
				if n := count.callsFor(src); n > 1 {
					t.Errorf("%s was encoded %d times", src, n)
				}
				r := rows[src]
				if len(r) != 1 || !r[0].Status.Terminal() {
					t.Errorf("%s has %d ledger row(s) %v, want exactly one terminal row", src, len(r), r)
				}
			}
			if len(rows) != len(srcs) {
				t.Errorf("the ledger holds rows for %d paths, want %d", len(rows), len(srcs))
			}
		})
	}
}

// twoJobEncoder is the AC-9 forced interleaving: the FIRST job to reach it writes its encode,
// then waits until the SECOND job has entered it - which is after the second picked its
// working path - and checks its own working file is still there with the bytes it wrote.
type twoJobEncoder struct {
	t       *testing.T
	encoded map[string]string // source -> the reference encode this job writes
	second  *closeOnce
	mu      sync.Mutex
	outs    map[string]string // source -> the working path it was told to write
	written map[string]string // source -> sha256 of what it wrote
	damaged []string          // what the first job found wrong with its file afterwards
	order   []string
}

func (g *twoJobEncoder) Encode(ctx context.Context, in, out string, props *probe.VideoProps) error {
	g.mu.Lock()
	if g.outs == nil {
		g.outs, g.written = map[string]string{}, map[string]string{}
	}
	g.outs[in] = out
	g.order = append(g.order, in)
	first := len(g.order) == 1
	g.mu.Unlock()
	if err := copyInto(g.encoded[in], out); err != nil {
		return err
	}
	sum := sha256f(g.t, out)
	g.mu.Lock()
	g.written[in] = sum
	g.mu.Unlock()
	if !first {
		g.second.close()
		return nil
	}
	select {
	case <-g.second.ch:
	case <-time.After(s0163Guard):
		return errors.New("the second job never reached its encode while the first was in flight")
	}
	// The second job has picked its working path by now. Ours must be exactly as we left it.
	if b, err := os.ReadFile(out); err != nil {
		g.mu.Lock()
		g.damaged = append(g.damaged, "gone: "+err.Error())
		g.mu.Unlock()
	} else if got := sha256f(g.t, out); got != sum || len(b) == 0 {
		g.mu.Lock()
		g.damaged = append(g.damaged, "its bytes changed")
		g.mu.Unlock()
	}
	return nil
}

// TestS0163_AC9_TwoSourcesSharingAStemAndAContainerGetDistinctWorkingFiles grades AC-9: two
// sources in one directory that share a stem and encode to one container, in flight at once,
// with scratch_dir unset and set. The second picks its working path while the first's encode
// runs; the first's file is untouched; each outcome is computed from the file its own encoder
// wrote.
//
// The pair is ep.mp4 and ep.mov under container_ext: mkv. The spec's own example, ep.mkv
// beside ep.mp4, never has both in flight: ep.mkv IS ep.mp4's target, so ep.mp4 is skipped
// target-already-exists before a working path is picked. Two sources NEITHER of which carries
// the target extension are the pair that really constructs one working name.
func TestS0163_AC9_TwoSourcesSharingAStemAndAContainerGetDistinctWorkingFiles(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, scratched := range []bool{false, true} {
		t.Run(fmt.Sprintf("scratch_dir set %v", scratched), func(t *testing.T) {
			root, scratch := scratchDirs(t)
			short, long := filepath.Join(root, "ep.mp4"), filepath.Join(root, "ep.mov")
			mkH264(t, ffmpeg, short, "8M")    // 2 seconds
			mkH264Long(t, ffmpeg, long, "8M") // 10 seconds
			sums := map[string]string{short: sha256f(t, short), long: sha256f(t, long)}
			enc := &twoJobEncoder{t: t, second: newCloseOnce(), encoded: map[string]string{
				short: s0163Encoded(t, ffmpeg, ffprobe, short),
				long:  s0163Encoded(t, ffmpeg, ffprobe, long),
			}}
			eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, enc, func(c *config.Config) {
				c.Workers = 2
				if scratched {
					c.ScratchDir = scratch
				}
			})
			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatalf("RunOneshot: %v", err)
			}

			enc.mu.Lock()
			outs, written, damaged, order := enc.outs, enc.written, enc.damaged, enc.order
			enc.mu.Unlock()
			if len(order) != 2 {
				t.Fatalf("%d encodes ran (%v), want both sources in flight", len(order), order)
			}
			if outs[short] == outs[long] {
				t.Fatalf("both jobs were given the working file %s", outs[short])
			}
			for _, d := range damaged {
				t.Errorf("the first job's working file %s after the second picked its path: %s", outs[order[0]], d)
			}
			wantDir := root
			if scratched {
				wantDir = scratch
			}
			for _, src := range order {
				if filepath.Dir(outs[src]) != wantDir || !isTempName(filepath.Base(outs[src])) {
					t.Errorf("the working file of %s is %s, want a temp construction in %s", src, outs[src], wantDir)
				}
			}

			// One target, so one swap: the job that reached it first is DONE with the
			// bytes ITS encoder wrote, and the other found the target there and refused to
			// clobber it - which it can only reach by passing every gate against its OWN
			// file, since the two sources' durations differ and a gate reading the other
			// job's encode would have failed duration parity instead.
			final := filepath.Join(root, "ep.mkv")
			rows := rowsByPath(t, ts)
			var done, refused []string
			for _, src := range order {
				if _, status, ok := outcomeFor(t, ts, src); ok && status == store.Failed {
					refused = append(refused, src)
				}
			}
			for _, r := range rows[final] {
				if r.Status == store.Done {
					done = append(done, final)
				}
			}
			if len(done) != 1 || len(refused) != 1 {
				t.Fatalf("done %v and refused %v, want one swap onto %s and one refusal: %v", done, refused, final, rows)
			}
			swapped := order[0]
			if swapped == refused[0] {
				swapped = order[1]
			}
			if got := sha256f(t, final); got != written[swapped] {
				t.Errorf("%s holds bytes its own job's encoder (%s) did not write", final, swapped)
			}
			out, _, _ := outcomeFor(t, ts, refused[0])
			if !strings.Contains(out.Reason, "target appeared during encode") {
				t.Errorf("%s was refused for %q, want the target check after its own gates passed", refused[0], out.Reason)
			}
			if got := sha256f(t, refused[0]); got != sums[refused[0]] {
				t.Errorf("the refused source %s was modified", refused[0])
			}
			if exists(swapped) {
				t.Errorf("the swapped source %s is still there beside its replacement", swapped)
			}
			if n := nTemp(t, root); n != 0 {
				t.Errorf("%d working file(s) left beside the sources", n)
			}
		})
	}
}

// TestS0163_AC9_TwoJobsOntoOneTargetAreCheckedAndSwappedOneAtATime grades the swap half of
// AC-9's isolation: two jobs with ONE target cannot both be between the pre-swap target check
// and the rename. The first holds there (after its retention, before its rename) until the
// second is seen waiting for the target; the second then finds the first's replacement and
// refuses to clobber it rather than renaming over it after the first's source is gone.
func TestS0163_AC9_TwoJobsOntoOneTargetAreCheckedAndSwappedOneAtATime(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	a, b := filepath.Join(root, "ep.mp4"), filepath.Join(root, "ep.mov")
	mkH264(t, ffmpeg, a, "8M")
	if err := copyInto(a, b); err != nil {
		t.Fatal(err)
	}
	encoded := s0163Encoded(t, ffmpeg, ffprobe, a)
	both := newCloseOnce()
	count := &inflight{}
	enc := EncoderFunc(func(ctx context.Context, in, out string, props *probe.VideoProps) error {
		if count.enter(in) == 2 {
			both.close()
		}
		defer count.leave()
		select {
		case <-both.ch:
		case <-time.After(s0163Guard):
			return errors.New("the two jobs never encoded together")
		}
		return copyInto(encoded, out)
	})
	eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, enc, func(c *config.Config) {
		c.Workers = 2
		c.UndoWindowHours = 1
	})
	waiting := newCloseOnce()
	eng.hookTargetWait = func(string) { waiting.close() }
	var mu sync.Mutex
	inside := 0
	var overlapped bool
	eng.hookAfterRetain = func(string) error {
		mu.Lock()
		inside++
		if inside > 1 {
			overlapped = true
		}
		mu.Unlock()
		defer func() { mu.Lock(); inside--; mu.Unlock() }()
		select {
		case <-waiting.ch:
		case <-time.After(s0163Guard):
		}
		return nil
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if overlapped {
		t.Fatal("both jobs were between the target check and the rename at once")
	}
	select {
	case <-waiting.ch:
	default:
		t.Fatal("the second job never waited for the first job's swap onto the shared target")
	}
	final := filepath.Join(root, "ep.mkv")
	if got, want := sha256f(t, final), sha256f(t, encoded); got != want {
		t.Errorf("%s is not the replacement the first job swapped in", final)
	}
	var refusals int
	for _, src := range []string{a, b} {
		if out, status, ok := outcomeFor(t, ts, src); ok && status == store.Failed {
			refusals++
			if !strings.Contains(out.Reason, "target appeared during encode") {
				t.Errorf("%s failed for %q, want the target refusal", src, out.Reason)
			}
			if !exists(src) {
				t.Errorf("the refused source %s is gone", src)
			}
		}
	}
	if refusals != 1 {
		t.Errorf("%d job(s) refused the target, want exactly the second", refusals)
	}
}

// roomEngine builds an engine over root whose free-space reads return free for every path
// and whose filesystem identity is fsOf.
func roomEngine(t *testing.T, ffmpeg, ffprobe, root string, enc Encoder, free uint64, fsOf func(string) string,
	mutate func(*config.Config)) (*Engine, *testStore) {
	t.Helper()
	eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, enc, mutate)
	eng.freeBytes = func(string) (uint64, error) { return free, nil }
	eng.fsID = func(p string) (string, error) { return fsOf(p), nil }
	return eng, ts
}

// TestS0163_AC10_JobsOnOneFilesystemNeverOverlapAndOnTwoTheyDo grades AC-10: two sources of
// size s, 2 workers, each check reading 1.5s free. On ONE filesystem - beside the sources, and
// again in a scratch directory - the two encodes never overlap and both reach an outcome that
// is not the not-enough-room failure. On TWO filesystems both are in flight at once.
func TestS0163_AC10_JobsOnOneFilesystemNeverOverlapAndOnTwoTheyDo(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, tc := range []struct {
		name    string
		scratch bool
		split   bool
	}{{"one filesystem beside the sources", false, false}, {"one scratch filesystem", true, false},
		{"two filesystems", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			root, scratch := scratchDirs(t)
			a, b := filepath.Join(root, "showA", "a.mkv"), filepath.Join(root, "showB", "b.mkv")
			s := s0163Copies(t, ffmpeg, a, b)
			count := &inflight{}
			other := newCloseOnce() // the other job waited for room, entered, or reached its outcome
			enc := EncoderFunc(func(ctx context.Context, in, out string, props *probe.VideoProps) error {
				if count.enter(in) > 1 {
					other.close()
				}
				defer count.leave()
				select {
				case <-other.ch:
				case <-time.After(s0163Guard):
					return errors.New("the other job neither waited nor ran")
				}
				return errFake
			})
			fsOf := func(dir string) string {
				if tc.split {
					return "fs:" + filepath.Base(dir)
				}
				return "fs:one"
			}
			eng, ts := roomEngine(t, ffmpeg, ffprobe, root, enc, uint64(s)*3/2, fsOf, func(c *config.Config) {
				c.Workers = 2
				if tc.scratch {
					c.ScratchDir = scratch
				}
			})
			eng.hookRoomWait = func(string) { other.close() }
			eng.Observer = func(ev Event) {
				if ev.Status.Terminal() {
					other.close()
				}
			}
			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatalf("RunOneshot: %v", err)
			}
			want := 1
			if tc.split {
				want = 2
			}
			if got := count.peak(); got != want {
				t.Errorf("%d encodes were in flight at once, want %d", got, want)
			}
			for _, src := range []string{a, b} {
				out, status, ok := outcomeFor(t, ts, src)
				if !ok || !status.Terminal() || strings.Contains(out.Reason, "not enough room") {
					t.Errorf("%s ended %q (%q), want a terminal outcome that is not the room failure", src, status, out.Reason)
				}
				if count.callsFor(src) != 1 {
					t.Errorf("%s reached the encoder %d times, want once", src, count.callsFor(src))
				}
			}
			if held := holdsNow(eng); len(held) != 0 {
				t.Errorf("bytes are still reserved after the pass: %v", held)
			}
		})
	}
}

// TestS0163_AC11_AJobThatFitsOnlyWithoutTheReservationsWaitsAndOneThatNeverFitsFails grades
// AC-11: the job that fits the free space but not beside the in-flight reservation is held
// without encoding or a failure until the first job ends, is checked again, and says so in one
// info record; one that does not fit even with nothing reserved fails exactly as before; and
// one cancelled while it waits returns without encoding, without a failure, source untouched.
func TestS0163_AC11_AJobThatFitsOnlyWithoutTheReservationsWaitsAndOneThatNeverFitsFails(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	t.Run("waits, then is checked again", func(t *testing.T) {
		root := t.TempDir()
		a, b := filepath.Join(root, "showA", "a.mkv"), filepath.Join(root, "showB", "b.mkv")
		s := s0163Copies(t, ffmpeg, a, b)
		free := uint64(s) * 3 / 2
		logs := &lockedBuffer{}
		count := &inflight{}
		waited := newCloseOnce()
		var mu sync.Mutex
		var whileWaiting []string // what the waiting job's row said while it waited
		lookups := map[string]int{}
		var eng *Engine
		var ts *testStore
		enc := EncoderFunc(func(ctx context.Context, in, out string, props *probe.VideoProps) error {
			if count.enter(in) > 1 {
				waited.close()
			}
			defer count.leave()
			select {
			case <-waited.ch:
			case <-time.After(s0163Guard):
				return errors.New("the second job never waited")
			}
			return errFake
		})
		eng, ts = roomEngine(t, ffmpeg, ffprobe, root, enc, free, func(string) string { return "fs:one" },
			func(c *config.Config) { c.Workers = 2 })
		eng.freeBytes = func(p string) (uint64, error) {
			mu.Lock()
			lookups[p]++
			mu.Unlock()
			return free, nil
		}
		eng.Log = slog.New(slog.NewJSONHandler(logs, nil))
		eng.hookRoomWait = func(src string) {
			_, status, ok := outcomeFor(t, ts, src)
			mu.Lock()
			whileWaiting = append(whileWaiting, fmt.Sprintf("%s ok=%v", status, ok))
			mu.Unlock()
			waited.close()
		}
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		var waits []map[string]any
		for _, r := range jsonRecords(t, logs.String()) {
			if r["level"] == "INFO" && strings.HasPrefix(fmt.Sprint(r["msg"]), "waiting for room") {
				waits = append(waits, r)
			}
		}
		if len(waits) != 1 {
			t.Fatalf("%d waiting records, want exactly one: %v", len(waits), waits)
		}
		w := waits[0]
		src := fmt.Sprint(w["file"])
		if (src != a && src != b) || w["free_bytes"] != float64(free) || w["reserved_bytes"] != float64(s) {
			t.Errorf("the waiting record does not name the source, the free space and the reserved bytes: %v", w)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(whileWaiting) != 1 || !strings.HasPrefix(whileWaiting[0], string(store.Probing)) {
			t.Errorf("while it waited the job's row said %v, want it still claimed and not failed", whileWaiting)
		}
		if n := lookups[filepath.Dir(src)]; n < 2 {
			t.Errorf("the waiting job's free space was read %d time(s), want it read again after the wait", n)
		}
		if got := count.peak(); got != 1 || count.callsFor(src) != 1 {
			t.Errorf("peak %d, the waiting job encoded %d time(s): want it encoded once, after the first", got, count.callsFor(src))
		}
	})

	t.Run("never fits: fails as before", func(t *testing.T) {
		root := t.TempDir()
		src := filepath.Join(root, "film.mkv")
		s := s0163Copies(t, ffmpeg, src)
		sum := sha256f(t, src)
		count := &inflight{}
		enc := EncoderFunc(func(ctx context.Context, in, out string, props *probe.VideoProps) error {
			count.enter(in)
			defer count.leave()
			return errFake
		})
		eng, ts := roomEngine(t, ffmpeg, ffprobe, root, enc, uint64(s)-1, func(string) string { return "fs:one" }, nil)
		waitedFor := false
		eng.hookRoomWait = func(string) { waitedFor = true }
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		out, status, ok := outcomeFor(t, ts, src)
		if !ok || status != store.Failed {
			t.Fatalf("status %q (found %v), want failed", status, ok)
		}
		for _, want := range []string{root, fmt.Sprint(s - 1), fmt.Sprint(s)} {
			if !strings.Contains(out.Reason, want) {
				t.Errorf("the failure does not name %q: %q", want, out.Reason)
			}
		}
		if waitedFor || count.callsFor(src) != 0 || sha256f(t, src) != sum {
			t.Errorf("waited %v, encoded %d time(s), source intact %v: want a failure before anything",
				waitedFor, count.callsFor(src), sha256f(t, src) == sum)
		}
	})

	t.Run("cancelled while waiting", func(t *testing.T) {
		root := t.TempDir()
		src := filepath.Join(root, "film.mkv")
		s := s0163Copies(t, ffmpeg, src)
		sum := sha256f(t, src)
		count := &inflight{}
		enc := EncoderFunc(func(ctx context.Context, in, out string, props *probe.VideoProps) error {
			count.enter(in)
			defer count.leave()
			return errFake
		})
		eng, ts := roomEngine(t, ffmpeg, ffprobe, root, enc, uint64(s)*3/2, func(string) string { return "fs:one" }, nil)
		// Another job in flight on the same filesystem holds its source's worth.
		hold, err := eng.sourceRoomFor(context.Background(), root, filepath.Join(root, "other.mkv"), s)
		if err != nil {
			t.Fatalf("the sibling's reservation: %v", err)
		}
		defer hold()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		eng.hookRoomWait = func(string) { cancel() }
		if err := eng.ProcessFile(ctx, "w0", src); !errors.Is(err, context.Canceled) {
			t.Fatalf("ProcessFile = %v, want the cancellation", err)
		}
		if count.callsFor(src) != 0 || sha256f(t, src) != sum || nTemp(t, root) != 0 {
			t.Errorf("a job cancelled while it waited encoded %d time(s), left the source intact %v and %d temp(s)",
				count.callsFor(src), sha256f(t, src) == sum, nTemp(t, root))
		}
		if _, status, ok := outcomeFor(t, ts, src); ok && (status == store.Failed || status.Terminal()) {
			t.Errorf("a job cancelled while it waited recorded %q against the file", status)
		}
		if got := holdsNow(eng)["fs:one"]; got != uint64(s) {
			t.Errorf("the cancelled job left %d byte(s) reserved, want only the sibling's %d", got, s)
		}
	})
}

// storeFailingFinish is a store whose terminal writes fail, the "store error" way out of a job.
type storeFailingFinish struct{ *testStore }

func (s storeFailingFinish) Finish(context.Context, string, string, store.Status, *store.Outcome, int) error {
	return errors.New("simulated store error")
}

// TestS0163_AC12_EveryWayOutOfAJobReleasesItsReservation grades AC-12: a job that ends done,
// skipped, failed at the encode, failed at a gate, cancelled, or with its store write failing
// gives its reservation back - and after a pass whose admitted jobs failed at the encode, a
// job whose source is the whole of the free space the check reads is admitted without waiting.
func TestS0163_AC12_EveryWayOutOfAJobReleasesItsReservation(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	srcs := map[string]string{}
	var paths []string
	for _, route := range []string{"done", "skipped", "encode", "gate", "cancelled", "store", "pass1", "pass2", "pass3", "after"} {
		srcs[route] = filepath.Join(root, route, route+".mkv")
		paths = append(paths, srcs[route])
	}
	s := s0163Copies(t, ffmpeg, paths...)
	encoded := s0163Encoded(t, ffmpeg, ffprobe, srcs["done"])
	// The retention area of the skipped job's directory is a FILE, so its original cannot be
	// retained and it is skipped undo-retention-failed after every gate passed.
	if err := os.WriteFile(filepath.Join(root, "skipped", UndoDirName), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	encoderCalls := &inflight{}
	var cancelRoute context.CancelFunc
	enc := EncoderFunc(func(ctx context.Context, in, out string, props *probe.VideoProps) error {
		encoderCalls.enter(in)
		defer encoderCalls.leave()
		switch filepath.Base(filepath.Dir(in)) {
		case "done", "skipped", "store":
			return copyInto(encoded, out)
		case "gate":
			return os.WriteFile(out, []byte("not a video"), 0o644)
		case "cancelled":
			cancelRoute()
			<-ctx.Done()
			return ctx.Err()
		}
		return errFake
	})
	eng, ts := roomEngine(t, ffmpeg, ffprobe, root, enc, uint64(s), func(string) string { return "fs:one" },
		func(c *config.Config) { c.UndoWindowHours = 1 })
	waited := map[string]bool{}
	var mu sync.Mutex
	eng.hookRoomWait = func(src string) { mu.Lock(); waited[src] = true; mu.Unlock() }

	for _, route := range []string{"done", "skipped", "encode", "gate", "cancelled", "store"} {
		ctx, cancel := context.WithCancel(context.Background())
		cancelRoute = cancel
		e := eng
		if route == "store" {
			e.Store = storeFailingFinish{ts}
		}
		err := e.ProcessFile(ctx, "w0", srcs[route])
		cancel()
		e.Store = ts
		if (route == "cancelled") != errors.Is(err, context.Canceled) {
			t.Errorf("%s: ProcessFile = %v", route, err)
		}
		if held := holdsNow(eng); len(held) != 0 {
			t.Errorf("after the %s route %v bytes are still reserved", route, held)
		}
		if w := tempPath(filepath.Join(root, route), route, "mkv", 0); workingPathHeld(w) {
			t.Errorf("after the %s route its working path %s is still held", route, w)
		}
		if encoderCalls.callsFor(srcs[route]) != 1 {
			t.Errorf("the %s route never reached the encoder, so it proves nothing", route)
		}
	}
	checks := map[string]store.Status{"done": store.Done, "skipped": store.Skipped, "encode": store.Failed, "gate": store.Failed}
	for route, want := range checks {
		p := srcs[route]
		if route == "done" {
			p = filepath.Join(root, "done", "done.mkv")
		}
		if _, status, ok := outcomeFor(t, ts, p); !ok || status != want {
			t.Errorf("the %s route ended %q, want %q", route, status, want)
		}
	}

	// A pass whose admitted jobs all fail at the encode, then one more job the size of the
	// whole free space: admitted at once.
	if err := os.RemoveAll(filepath.Join(root, "after")); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"done", "skipped", "encode", "gate", "cancelled", "store"} {
		_ = os.RemoveAll(filepath.Join(root, route))
	}
	eng.Cfg.Workers = 3
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	for _, route := range []string{"pass1", "pass2", "pass3"} {
		if _, status, ok := outcomeFor(t, ts, srcs[route]); !ok || status != store.Failed {
			t.Errorf("%s ended %q, want the encode failure", route, status)
		}
	}
	if held := holdsNow(eng); len(held) != 0 {
		t.Fatalf("after the pass %v bytes are still reserved", held)
	}
	s0163Copies(t, ffmpeg, srcs["after"])
	mu.Lock()
	waited = map[string]bool{}
	mu.Unlock()
	if err := eng.ProcessFile(context.Background(), "w0", srcs["after"]); err != nil {
		t.Fatalf("ProcessFile: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if waited[srcs["after"]] || encoderCalls.callsFor(srcs["after"]) != 1 {
		t.Errorf("a job the size of the whole free space waited %v and encoded %d time(s), want admitted at once",
			waited[srcs["after"]], encoderCalls.callsFor(srcs["after"]))
	}
}

// TestS0163_AC13_ConcurrentSwapsInOneDirectoryKeepTheUndoAccountingWhole grades AC-13's
// engine half: three jobs whose sources share one directory all retain before any of them
// renames, and afterwards there is exactly one live retention per source, the held figure is
// the sum of their pre-encode sizes, and any one of them restores to its original bytes. A
// sibling's prune landing between another job creating the retention area and linking into
// it does not skip that job.
func TestS0163_AC13_ConcurrentSwapsInOneDirectoryKeepTheUndoAccountingWhole(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	t.Run("three swaps into one directory at once", func(t *testing.T) {
		root := t.TempDir()
		srcs := []string{filepath.Join(root, "a.mkv"), filepath.Join(root, "b.mkv"), filepath.Join(root, "c.mkv")}
		s := s0163Copies(t, ffmpeg, srcs...)
		sums := map[string]string{}
		for _, p := range srcs {
			sums[p] = sha256f(t, p)
		}
		encoded := s0163Encoded(t, ffmpeg, ffprobe, srcs[0])
		enc := EncoderFunc(func(ctx context.Context, in, out string, props *probe.VideoProps) error {
			return copyInto(encoded, out)
		})
		eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, enc, func(c *config.Config) {
			c.Workers = 3
			c.UndoWindowHours = 1
		})
		all := newCloseOnce()
		var mu sync.Mutex
		retained := 0
		eng.hookAfterRetain = func(string) error {
			mu.Lock()
			if retained++; retained == len(srcs) {
				all.close()
			}
			mu.Unlock()
			select {
			case <-all.ch:
				return nil
			case <-time.After(s0163Guard):
				return errors.New("the three retentions were never in one directory at once")
			}
		}
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		rows, err := ts.ListRetained(context.Background())
		if err != nil {
			t.Fatalf("ListRetained: %v", err)
		}
		per := map[string]int{}
		for _, r := range rows {
			per[r.SourcePath]++
			if !exists(r.RetainedPath) || filepath.Dir(r.RetainedPath) != filepath.Join(root, UndoDirName) {
				t.Errorf("the retention of %s is not in the shared area: %s", r.SourcePath, r.RetainedPath)
			}
		}
		for _, p := range srcs {
			if per[p] != 1 {
				t.Errorf("%s has %d live retention(s), want exactly 1", p, per[p])
			}
		}
		held, err := ts.HeldByUndoWindow(context.Background())
		if err != nil {
			t.Fatalf("HeldByUndoWindow: %v", err)
		}
		if held != s*int64(len(srcs)) {
			t.Errorf("the held figure is %d, want the sum of the sources' pre-encode sizes %d", held, s*int64(len(srcs)))
		}
		if _, err := eng.Restore(context.Background(), srcs[1]); err != nil {
			t.Fatalf("Restore(%s): %v", srcs[1], err)
		}
		if got := sha256f(t, srcs[1]); got != sums[srcs[1]] {
			t.Errorf("the restored %s is not its original bytes", srcs[1])
		}
	})

	t.Run("a sibling's prune between the create and the link", func(t *testing.T) {
		root := t.TempDir()
		src := filepath.Join(root, "film.mkv")
		s0163Copies(t, ffmpeg, src)
		sum := sha256f(t, src)
		encoded := s0163Encoded(t, ffmpeg, ffprobe, src)
		enc := EncoderFunc(func(ctx context.Context, in, out string, props *probe.VideoProps) error {
			return copyInto(encoded, out)
		})
		eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, enc, func(c *config.Config) { c.UndoWindowHours = 1 })
		pruned := 0
		eng.hookUndoArea = func(area string) {
			if pruned > 0 {
				return
			}
			pruned++
			// A sibling in the same directory abandons ITS retention, which empties the area
			// this job has just created, and discard prunes it - the real path, not a stand-in.
			sibling := filepath.Join(area, "sibling.000-000."+UndoMarker+".mkv"+UndoSuffix)
			if err := os.WriteFile(sibling, []byte("x"), 0o644); err != nil {
				t.Errorf("staging the sibling's retention: %v", err)
				return
			}
			eng.undo().discard(sibling)
			if _, err := os.Stat(area); !os.IsNotExist(err) {
				t.Errorf("the sibling's discard did not prune the area (%v), so the case proves nothing", err)
			}
		}
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		if reason := skipReason(t, ts, "film.mkv"); reason == SkipUndoRetentionFailed {
			t.Fatalf("the job was skipped %s after a sibling's prune", reason)
		}
		if !ledgerHas(t, ts, store.Done, "film.mkv") {
			t.Fatal("the job did not swap after a sibling's prune")
		}
		rows, err := ts.ListRetained(context.Background())
		if err != nil || len(rows) != 1 || !exists(rows[0].RetainedPath) || sha256f(t, rows[0].RetainedPath) != sum {
			t.Fatalf("want one live retention holding the original: %v %v", rows, err)
		}
		if pruned != 1 {
			t.Fatalf("the prune landed %d time(s), want once", pruned)
		}
	})
}
