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

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// S0158: the free-space pre-check for a job whose working file goes BESIDE its source
// (scratch_dir unset), driven through the real pipeline over real ffmpeg fixtures.
//
// The free-space lookup is substituted through the engine's seam, because a CI runner
// cannot fill a filesystem on demand. The filesystem IDENTITY is the real one, so two
// directories of one temp directory share it exactly as two directories of one library
// drive do.

// encoded reports whether the encoder was ever asked to encode src.
func encoded(rec *realEncode, src string) bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	_, ok := rec.outs[src]
	return ok
}

// holdsNow is a copy of the engine's account: the bytes it still holds, per filesystem.
func holdsNow(e *Engine) map[string]uint64 {
	e.room.mu.Lock()
	defer e.room.mu.Unlock()
	out := make(map[string]uint64, len(e.room.held))
	for k, v := range e.room.held {
		out[k] = v
	}
	return out
}

// refusedBeforeEncoding asserts the AC-1 shape of a refusal: a failed row whose reason names
// each of want, the source byte-identical to sum, and the encoder never asked about it.
func refusedBeforeEncoding(t *testing.T, ts *testStore, rec *realEncode, src, sum string, want ...string) {
	t.Helper()
	if encoded(rec, src) {
		t.Fatalf("the encoder was called for %s, a source that will not fit beside itself", src)
	}
	if got := sha256f(t, src); got != sum {
		t.Fatalf("the refused source %s was modified (sha256 %s, was %s)", src, got, sum)
	}
	out, status, ok := outcomeFor(t, ts, src)
	if !ok || status != store.Failed {
		t.Fatalf("%s: status = %q (found=%v), want failed", src, status, ok)
	}
	for _, w := range want {
		if !strings.Contains(out.Reason, w) {
			t.Errorf("the reason does not name %q: %q", w, out.Reason)
		}
	}
}

// [AC-1] [AC-3] A job whose source will not fit beside itself is refused before the encoder
// is invoked - no call, no new file in the source's directory, the source byte-identical,
// a failed row naming the directory and both figures - and the scan carries on with the
// rest of the run.
//
// The figure is fixed between the two fixtures' sizes, so the SAME run is both arms: the
// big source is refused and the small one is transcoded and swapped.
func TestSourceRoom_AJobThatCannotFitIsRefusedBeforeEncodingAndTheScanContinues(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, _ := scratchDirs(t)
	big := filepath.Join(root, "big.mkv")
	small := filepath.Join(root, "small.mkv")
	mkH264Long(t, ffmpeg, big, "8M")
	mkH264(t, ffmpeg, small, "8M")
	bigSize, smallSize := probe.FileSize(big), probe.FileSize(small)
	avail := uint64(bigSize) - 1
	if avail < uint64(smallSize) {
		t.Fatalf("the fixture sizes leave no room between them: small=%d big=%d", smallSize, bigSize)
	}
	bigSum := sha256f(t, big)

	eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, nil, nil)
	rec := &realEncode{enc: FFmpegEncoder{FFmpeg: ffmpeg, Cfg: eng.Cfg, Probe: eng.Probe}}
	eng.Enc = rec
	eng.freeBytes = func(path string) (uint64, error) {
		if path != root {
			t.Errorf("the free-space lookup was asked about %s, want the source's directory %s", path, root)
		}
		return avail, nil
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	refusedBeforeEncoding(t, ts, rec, big, bigSum, root, fmt.Sprint(avail), fmt.Sprint(bigSize))
	if got := listDir(t, root); !equalStrings(got, []string{"big.mkv", "small.mkv"}) {
		t.Fatalf("the source's directory holds %v, want only the two sources - a file appeared beside the refused one", got)
	}

	// [AC-3] the scan continued and the small one was transcoded and swapped.
	if codecOf(t, ffprobe, small) != "hevc" {
		t.Fatalf("the scan did not carry on: %s is still %q", small, codecOf(t, ffprobe, small))
	}
	if !ledgerHas(t, ts, store.Done, "small.mkv") {
		t.Fatalf("the source that fits was not recorded as swapped")
	}
}

// [AC-2] The host's case. /mnt/plex had 3800000 bytes available at deploy and its largest
// candidate was 21600000000 bytes.
func TestSourceRoom_TheHostsFiguresAreARefusal(t *testing.T) {
	const hostAvail = 3800000
	const hostSource = 21600000000

	t.Run("a candidate larger than 3800000 bytes never starts its encode", func(t *testing.T) {
		ffmpeg, ffprobe := tools(t)
		root, _ := scratchDirs(t)
		src := filepath.Join(root, "film.mkv")
		mkH264Tall(t, ffmpeg, src, "8M")
		size := probe.FileSize(src)
		if size <= hostAvail {
			t.Fatalf("the fixture is %d byte(s), which is not larger than the host's %d", size, hostAvail)
		}
		sum := sha256f(t, src)

		eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, nil, nil)
		rec := &realEncode{enc: FFmpegEncoder{FFmpeg: ffmpeg, Cfg: eng.Cfg, Probe: eng.Probe}}
		eng.Enc = rec
		eng.freeBytes = func(string) (uint64, error) { return hostAvail, nil }
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		refusedBeforeEncoding(t, ts, rec, src, sum, fmt.Sprint(hostAvail), fmt.Sprint(size))
	})

	t.Run("3800000 available against a 21600000000-byte source is a refusal naming both", func(t *testing.T) {
		dir := t.TempDir()
		eng := &Engine{Log: discardLogger(), freeBytes: func(string) (uint64, error) { return hostAvail, nil }}
		release, err := eng.sourceRoomFor(dir, filepath.Join(dir, "film.mkv"), hostSource)
		if err == nil {
			release()
			t.Fatalf("a %d-byte source was let through with %d byte(s) available", int64(hostSource), hostAvail)
		}
		for _, want := range []string{"3800000", "21600000000"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not carry %s: %q", want, err)
			}
		}
		if held := holdsNow(eng); len(held) != 0 {
			t.Fatalf("a refused job holds bytes: %v", held)
		}
	})
}

// [AC-4] The boundary: an available figure EQUAL to the need proceeds to the encode and the
// swap, and one byte short is refused.
func TestSourceRoom_ExactlyEnoughProceedsAndOneByteShortIsRefused(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, tc := range []struct {
		name  string
		short uint64
	}{
		{"available equals the need: encoded and swapped", 0},
		{"one byte short of the need: refused", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := scratchDirs(t)
			src := filepath.Join(root, "film.mkv")
			mkH264(t, ffmpeg, src, "8M")
			size := uint64(probe.FileSize(src))
			avail := size - tc.short
			sum := sha256f(t, src)

			eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, nil, nil)
			rec := &realEncode{enc: FFmpegEncoder{FFmpeg: ffmpeg, Cfg: eng.Cfg, Probe: eng.Probe}}
			eng.Enc = rec
			eng.freeBytes = func(string) (uint64, error) { return avail, nil }
			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatalf("RunOneshot: %v", err)
			}

			if tc.short == 0 {
				if !encoded(rec, src) {
					t.Fatalf("a source exactly the size of the available figure never reached the encoder")
				}
				if codecOf(t, ffprobe, src) != "hevc" || !ledgerHas(t, ts, store.Done, "film.mkv") {
					t.Fatalf("a source exactly the size of the available figure was not swapped (codec %q)", codecOf(t, ffprobe, src))
				}
				return
			}
			refusedBeforeEncoding(t, ts, rec, src, sum, fmt.Sprint(avail), fmt.Sprint(size))
		})
	}
}

// [AC-5] With the undo window open and a retained original on the source's filesystem, the
// measured figure is compared against the need with nothing added back for it: a source of
// S bytes with A < S <= A + R is refused.
//
// The retention is a REAL one, taken by a real swap in the first pass, so the second pass
// meets exactly what a library in the window holds: a second link in .holdfast-undo beside
// the source and a record of it in the ledger.
func TestSourceRoom_RetainedOriginalsAreNeverCreditedBack(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, _ := scratchDirs(t)
	first := filepath.Join(root, "first.mkv")
	mkH264Long(t, ffmpeg, first, "8M")

	eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.UndoWindowHours = 1 })
	rec := &realEncode{enc: FFmpegEncoder{FFmpeg: ffmpeg, Cfg: eng.Cfg, Probe: eng.Probe}}
	eng.Enc = rec
	avail := uint64(1) << 50
	eng.freeBytes = func(path string) (uint64, error) {
		if path != root {
			t.Errorf("the free-space lookup was asked about %s, want the source's directory %s", path, root)
		}
		return avail, nil
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot (the retaining swap): %v", err)
	}
	rows, err := ts.ListRetained(context.Background())
	if err != nil {
		t.Fatalf("ListRetained: %v", err)
	}
	var retained uint64
	for _, r := range rows {
		if exists(r.RetainedPath) && filepath.Dir(filepath.Dir(r.RetainedPath)) == root {
			retained += uint64(r.SourceBytes)
		}
	}
	if retained == 0 {
		t.Fatalf("the first pass retained no original beside the source (%d record(s)) - the case would prove nothing", len(rows))
	}

	second := filepath.Join(root, "second.mkv")
	mkH264(t, ffmpeg, second, "8M")
	size := uint64(probe.FileSize(second))
	avail = size - 1
	if !(avail < size && size <= avail+retained) {
		t.Fatalf("the figures do not place the source inside A < S <= A + R: A=%d S=%d R=%d", avail, size, retained)
	}
	sum := sha256f(t, second)
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	refusedBeforeEncoding(t, ts, rec, second, sum, fmt.Sprint(avail), fmt.Sprint(size))
}

// gatedEncode lets the FIRST job to reach it through to the real encoder, but only once the
// other job's outcome has been recorded, which is the one moment both jobs have been in
// flight together and the second has had its answer. A second job reaching it while the
// first still waits is the defect under test, and it releases the first rather than
// deadlocking the pair, so the case reds instead of hanging.
type gatedEncode struct {
	enc      FFmpegEncoder
	recorded chan struct{} // closed on the first terminal outcome of the run
	second   chan struct{} // closed when a second job reaches the encoder
	mu       sync.Mutex
	entered  []string
}

func (g *gatedEncode) Encode(ctx context.Context, in, out string, props *probe.VideoProps) error {
	g.mu.Lock()
	g.entered = append(g.entered, in)
	n := len(g.entered)
	g.mu.Unlock()
	if n > 1 {
		close(g.second)
		return errors.New("a second job reached the encoder while the first was in flight")
	}
	select {
	case <-g.recorded:
	case <-g.second:
	}
	return g.enc.Encode(ctx, in, out, props)
}

// [AC-6] Two workers, two sources in DIFFERENT directories of one filesystem, and an
// available figure that fits either alone but not both: at most one reaches the encoder,
// and the other is refused before it writes a byte, naming a need that includes the first
// job's source bytes.
func TestSourceRoom_TwoJobsOnOneFilesystemCannotBothPassOnTheSameFreeBytes(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, _ := scratchDirs(t)
	dirA, dirB := filepath.Join(root, "showA"), filepath.Join(root, "showB")
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	a, b := filepath.Join(dirA, "a.mkv"), filepath.Join(dirB, "b.mkv")
	mkH264Long(t, ffmpeg, a, "8M")
	mkH264(t, ffmpeg, b, "8M")
	sizeA, sizeB := uint64(probe.FileSize(a)), uint64(probe.FileSize(b))
	avail := sizeA + sizeB - 1
	sums := map[string]string{a: sha256f(t, a), b: sha256f(t, b)}

	eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.Workers = 2 })
	gate := &gatedEncode{
		enc:      FFmpegEncoder{FFmpeg: ffmpeg, Cfg: eng.Cfg, Probe: eng.Probe},
		recorded: make(chan struct{}),
		second:   make(chan struct{}),
	}
	eng.Enc = gate
	var once sync.Once
	eng.Observer = func(ev Event) {
		if ev.Status.Terminal() {
			once.Do(func() { close(gate.recorded) })
		}
	}
	eng.freeBytes = func(path string) (uint64, error) {
		if path != dirA && path != dirB {
			t.Errorf("the free-space lookup was asked about %s, want a source's directory", path)
		}
		return avail, nil
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	gate.mu.Lock()
	entered := append([]string(nil), gate.entered...)
	gate.mu.Unlock()
	if len(entered) != 1 {
		t.Fatalf("%d jobs reached the encoder (%v) on free bytes that fit only one of them", len(entered), entered)
	}
	through, refused := entered[0], a
	if refused == through {
		refused = b
	}
	if codecOf(t, ffprobe, through) != "hevc" {
		t.Errorf("the job that passed the check was not swapped: %s is %q", through, codecOf(t, ffprobe, through))
	}

	if got := sha256f(t, refused); got != sums[refused] {
		t.Fatalf("the refused source %s was modified", refused)
	}
	if got := listDir(t, filepath.Dir(refused)); !equalStrings(got, []string{filepath.Base(refused)}) {
		t.Fatalf("the refused job's directory holds %v, want only its source - it wrote before it was refused", got)
	}
	out, status, ok := outcomeFor(t, ts, refused)
	if !ok || status != store.Failed {
		t.Fatalf("%s: status = %q (found=%v), want failed", refused, status, ok)
	}
	for _, want := range []string{filepath.Dir(refused), fmt.Sprint(avail), fmt.Sprint(sizeA + sizeB)} {
		if !strings.Contains(out.Reason, want) {
			t.Errorf("the refusal does not name %q (the need must include the other job's source bytes): %q", want, out.Reason)
		}
	}
}

// [AC-7] A job stops being held the moment it exits, by any path.
func TestSourceRoom_AJobStopsBeingHeldTheMomentItExits(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	t.Run("a one-worker run transcodes two sources that fit alone but not together", func(t *testing.T) {
		root, _ := scratchDirs(t)
		a, b := filepath.Join(root, "a.mkv"), filepath.Join(root, "b.mkv")
		mkH264Long(t, ffmpeg, a, "8M")
		mkH264(t, ffmpeg, b, "8M")
		avail := uint64(probe.FileSize(a)) + uint64(probe.FileSize(b)) - 1

		eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.Workers = 1 })
		eng.freeBytes = func(string) (uint64, error) { return avail, nil }
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		for _, p := range []string{a, b} {
			if codecOf(t, ffprobe, p) != "hevc" || !ledgerHas(t, ts, store.Done, filepath.Base(p)) {
				t.Errorf("%s was not transcoded and swapped (codec %q) - the job before it is still held", p, codecOf(t, ffprobe, p))
			}
		}
		if held := holdsNow(eng); len(held) != 0 {
			t.Fatalf("bytes are still held after every job exited: %v", held)
		}
	})

	t.Run("a job cancelled mid-encode leaves nothing held for the next job", func(t *testing.T) {
		root, _ := scratchDirs(t)
		x, y := filepath.Join(root, "x.mkv"), filepath.Join(root, "y.mkv")
		mkH264Long(t, ffmpeg, x, "8M")
		mkH264(t, ffmpeg, y, "8M")
		avail := uint64(probe.FileSize(x)) + uint64(probe.FileSize(y)) - 1

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		eng, _ := buildEngineAndStore(t, ffmpeg, ffprobe, root, nil, nil)
		production := FFmpegEncoder{FFmpeg: ffmpeg, Cfg: eng.Cfg, Probe: eng.Probe}
		eng.Enc = EncoderFunc(func(c context.Context, in, out string, props *probe.VideoProps) error {
			if in != x {
				return production.Encode(c, in, out, props)
			}
			// Mid-encode: part of an output is on disk when the context goes.
			if err := os.WriteFile(out, []byte("half an encode"), 0o644); err != nil {
				return err
			}
			cancel()
			<-c.Done()
			return c.Err()
		})
		eng.freeBytes = func(string) (uint64, error) { return avail, nil }

		if err := eng.ProcessFile(ctx, "w0", x); !errors.Is(err, context.Canceled) {
			t.Fatalf("ProcessFile(%s) = %v, want the cancellation", x, err)
		}
		if held := holdsNow(eng); len(held) != 0 {
			t.Fatalf("a cancelled job left bytes held: %v", held)
		}
		if err := eng.ProcessFile(context.Background(), "w0", y); err != nil {
			t.Fatalf("ProcessFile(%s): %v", y, err)
		}
		if codecOf(t, ffprobe, y) != "hevc" {
			t.Fatalf("the next job on that filesystem was not transcoded (codec %q) - the cancelled job is still held",
				codecOf(t, ffprobe, y))
		}
	})
}

// [AC-8] A free-space lookup that FAILS logs one warn record naming the dependency, the
// directory, the error and that the job continues, and the job proceeds to the encode.
func TestSourceRoom_AFailedFreeSpaceLookupWarnsOnceAndTheJobContinues(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, _ := scratchDirs(t)
	src := filepath.Join(root, "film.mkv")
	mkH264(t, ffmpeg, src, "8M")

	eng, _ := buildEngineAndStore(t, ffmpeg, ffprobe, root, nil, nil)
	logs := &lockedBuffer{}
	eng.Log = slog.New(slog.NewJSONHandler(logs, nil))
	lookupErr := errors.New("statfs: input/output error")
	eng.freeBytes = func(string) (uint64, error) { return 0, lookupErr }
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	if codecOf(t, ffprobe, src) != "hevc" {
		t.Fatalf("an unreadable free-space figure stopped the job: %s is still %q", src, codecOf(t, ffprobe, src))
	}
	var warns []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a log line is not one JSON record: %q: %v", line, err)
		}
		if rec["level"] == "WARN" && rec["dependency"] == "free-space lookup" {
			warns = append(warns, rec)
		}
	}
	if len(warns) != 1 {
		t.Fatalf("%d warn record(s) name the free-space lookup, want exactly one: %v", len(warns), warns)
	}
	w := warns[0]
	if w["dir"] != root {
		t.Errorf("the warn names directory %v, want %s", w["dir"], root)
	}
	if errText, _ := w["err"].(string); !strings.Contains(errText, lookupErr.Error()) {
		t.Errorf("the warn does not carry the lookup's error: %v", w["err"])
	}
	if next, _ := w["next"].(string); !strings.Contains(next, "continue") {
		t.Errorf("the warn does not say the job continues: %v", w["next"])
	}
}
