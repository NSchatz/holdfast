package engine

// The encode-count bound in the ENGINE (S0174): `holdfast run --limit-encodes N`.
//
// `--limit N` counts every terminal outcome, skips included, so a proving pass over a
// library whose first files are skips ends having encoded almost nothing. The encode bound
// counts only files that REACHED AN ENCODE - the job entered the encoding state, or under
// dry_run was decided would-transcode - and keeps the terminal bound's no-overshoot
// admission under several workers. Each case names the criterion it grades (testing T1).
//
// "Reached an encode" is read off two independent witnesses wherever it is asserted: the
// encoder itself (encodeCounter records every source it is handed) and the ledger (a
// terminal row that names its encoder). The two agreeing is what makes the count a fact
// about the run rather than about one of them.

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// encodeCounter records every source handed to the encoder, by basename, and can hold each
// encode open for a while or replace it with a copy of the source.
type encodeCounter struct {
	inner Encoder
	// delay holds each encode open, so several files are encoding at once when the bound is
	// reached (AC-2, AC-4).
	delay time.Duration
	// copySource writes the source's own bytes as the "encode", which every gate behind it
	// refuses: the codec has not moved and the output is not smaller (AC-3).
	copySource bool

	mu   sync.Mutex
	seen map[string]int
}

func (c *encodeCounter) Encode(ctx context.Context, in, out string, props *probe.VideoProps) error {
	c.mu.Lock()
	if c.seen == nil {
		c.seen = map[string]int{}
	}
	c.seen[filepath.Base(in)]++
	c.mu.Unlock()
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	if c.copySource {
		return copyBytes(in, out)
	}
	return c.inner.Encode(ctx, in, out, props)
}

// encoded is the sorted set of sources the encoder was handed.
func (c *encodeCounter) encoded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.seen))
	for k := range c.seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func copyBytes(in, out string) error {
	src, err := os.Open(in)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(out)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}

// limitEncodesEngine builds an engine over root whose encoder is an encodeCounter around
// the real ffmpeg encoder.
func limitEncodesEngine(t *testing.T, ffmpeg, ffprobe, root string, workers int,
	shape func(*encodeCounter), mutate func(*config.Config)) (*Engine, *encodeCounter) {
	t.Helper()
	cfg := baseCfg(root)
	cfg.Workers = workers
	if mutate != nil {
		mutate(&cfg)
	}
	prober := probe.New(ffmpeg, ffprobe)
	counter := &encodeCounter{inner: FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}}
	if shape != nil {
		shape(counter)
	}
	return New(cfg, prober, counter, newTestStore(t, root), discardLogger()), counter
}

// encodedRows is the set of terminal rows that name their encoder: the ledger's own record
// that the file reached an encode.
func encodedRows(rows map[string]store.Job) []string {
	var out []string
	for name, r := range rows {
		if r.Outcome.Encoder != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// everyRowByName is every row in the store, terminal or not, by basename.
func everyRowByName(t *testing.T, ts *testStore) map[string]store.Job {
	t.Helper()
	rows, err := ts.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	out := map[string]store.Job{}
	for _, r := range rows {
		out[filepath.Base(r.Path)] = r
	}
	return out
}

// skipSource writes a source every guard skips: it is already at the target codec.
func skipSource(t *testing.T, ffmpeg, root, name string) {
	t.Helper()
	mkHevc(t, ffmpeg, filepath.Join(root, name), "800k")
}

// encodeSource writes a source a run really encodes.
func encodeSource(t *testing.T, ffmpeg, root, name string) {
	t.Helper()
	mkH264(t, ffmpeg, filepath.Join(root, name), "8M")
}

// TestLimitEncodes_AC1_SkipsDoNotCountAndNothingIsOfferedAfterTheNth grades AC-1: every skip
// the run meets is recorded, exactly N files reach an encode, and the file after the Nth
// encode is never offered - in the spec's fixture (four skips, then three encodable files)
// and with the skips between the encodes, under one worker and under four.
func TestLimitEncodes_AC1_SkipsDoNotCountAndNothingIsOfferedAfterTheNth(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	layouts := []struct {
		name  string
		files []string // in path order; "s" skips, "e" encodes
	}{
		{"four skips then three encodable", []string{"p0_s", "p1_s", "p2_s", "p3_s", "p4_e", "p5_e", "p6_e"}},
		{"skips before and between the encodes", []string{"p0_s", "p1_s", "p2_e", "p3_s", "p4_s", "p5_e", "p6_e"}},
	}
	for _, lay := range layouts {
		for _, workers := range []int{1, 4} {
			t.Run(lay.name+"/workers="+strconv.Itoa(workers), func(t *testing.T) {
				root := t.TempDir()
				var skips, encodes []string
				for _, f := range lay.files {
					name := f + ".mkv"
					if strings.HasSuffix(f, "_s") {
						skipSource(t, ffmpeg, root, name)
						skips = append(skips, name)
					} else {
						encodeSource(t, ffmpeg, root, name)
						encodes = append(encodes, name)
					}
				}
				eng, counter := limitEncodesEngine(t, ffmpeg, ffprobe, root, workers, nil, nil)
				if err := eng.RunBounded(context.Background(), Bound{LimitEncodes: 2}); err != nil {
					t.Fatalf("RunBounded(limit_encodes 2): %v", err)
				}
				rows := rowsByName(t, eng.Store.(*testStore))
				for _, s := range skips {
					if r, ok := rows[s]; !ok || r.Status != store.Skipped {
						t.Errorf("%s: want a skip row, got %+v (present %v): every skip the run meets is recorded", s, r.Status, ok)
					}
				}
				want := encodes[:2]
				if got := counter.encoded(); strings.Join(got, ",") != strings.Join(want, ",") {
					t.Errorf("the encoder was handed %v, want exactly %v", got, want)
				}
				if got := encodedRows(rows); strings.Join(got, ",") != strings.Join(want, ",") {
					t.Errorf("the ledger names an encoder on %v, want exactly %v", got, want)
				}
				if r, ok := everyRowByName(t, eng.Store.(*testStore))[encodes[2]]; ok {
					t.Errorf("%s has a row (%s) although it comes after the second encode: it must never be offered",
						encodes[2], r.Status)
				}
				if len(rows) != len(skips)+2 {
					t.Errorf("the ledger holds %v, want the %d skips and the 2 encodes", names(rows), len(skips))
				}
			})
		}
	}
}

// TestLimitEncodes_AC2_SeveralWorkersNeverOvershoot grades AC-2: with four workers over
// eight encodable files, N in {1, 2, 3} each yields exactly N files that reached an encode.
// Each encode is held open a moment, so every worker is busy when the bound is reached -
// the state an admission that counted only finished encodes would overshoot in.
func TestLimitEncodes_AC2_SeveralWorkersNeverOvershoot(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, n := range []int{1, 2, 3} {
		t.Run("limit_encodes="+strconv.Itoa(n), func(t *testing.T) {
			root := t.TempDir()
			for i := 0; i < 8; i++ {
				encodeSource(t, ffmpeg, root, "f"+strconv.Itoa(i)+".mkv")
			}
			eng, counter := limitEncodesEngine(t, ffmpeg, ffprobe, root, 4,
				func(c *encodeCounter) { c.delay = 200 * time.Millisecond }, nil)
			if err := eng.RunBounded(context.Background(), Bound{LimitEncodes: n}); err != nil {
				t.Fatalf("RunBounded(limit_encodes %d): %v", n, err)
			}
			if got := counter.encoded(); len(got) != n {
				t.Errorf("four workers under --limit-encodes %d handed the encoder %d files (%v), want exactly %d",
					n, len(got), got, n)
			}
			rows := everyRowByName(t, eng.Store.(*testStore))
			if got := encodedRows(rows); len(got) != n || len(rows) != n {
				t.Errorf("the ledger holds %v with an encoder named on %v, want exactly %d rows, each one encoded",
					names(rows), got, n)
			}
		})
	}
}

// TestLimitEncodes_AC3_AFailedEncodeAndASwapIncidentCountAsADoneSwapDoes grades AC-3: a file
// whose encode started counts toward N whatever its outcome - a gate refusing the output,
// a swap incident - exactly as a done swap counts.
func TestLimitEncodes_AC3_AFailedEncodeAndASwapIncidentCountAsADoneSwapDoes(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	cases := []struct {
		name  string
		shape func(*encodeCounter)
		seam  func(*Engine)
		want  map[store.Status]bool
	}{
		{"a gate refuses the output", func(c *encodeCounter) { c.copySource = true }, nil,
			map[store.Status]bool{store.Failed: true}},
		{"the swap records an incident", nil, func(e *Engine) {
			e.renameFn = failingRename(errSwap)
			e.fsLookup = lookups("nfs")
		}, map[store.Status]bool{store.Indeterminate: true, store.AppliedDespiteError: true}},
		{"the control: a done swap", nil, nil, map[store.Status]bool{store.Done: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for i := 0; i < 4; i++ {
				encodeSource(t, ffmpeg, root, "film"+strconv.Itoa(i)+".mkv")
			}
			eng, counter := limitEncodesEngine(t, ffmpeg, ffprobe, root, 1, tc.shape, nil)
			if tc.seam != nil {
				tc.seam(eng)
			}
			if err := eng.RunBounded(context.Background(), Bound{LimitEncodes: 2}); err != nil {
				t.Fatalf("RunBounded(limit_encodes 2): %v", err)
			}
			rows := everyTerminalRow(t, eng.Store.(*testStore))
			if len(rows) != 2 {
				t.Fatalf("--limit-encodes 2 recorded %d terminal outcomes, want 2: %+v", len(rows), rows)
			}
			for _, r := range rows {
				if !tc.want[r.Status] {
					t.Errorf("%s recorded %s, so the fixture did not reach the outcome it grades", filepath.Base(r.Path), r.Status)
				}
			}
			if got := counter.encoded(); len(got) != 2 {
				t.Errorf("the encoder was handed %v, want exactly two files", got)
			}
		})
	}
}

// TestLimitEncodes_AC4_EveryStartedEncodeFinishesBeforeTheRunReturns grades AC-4: once the
// Nth encode has started the offer stops, but every encode already started is carried to
// its terminal outcome before the run returns - no row is left in an in-progress state.
// Each encode is held open long enough that the Nth starts while the others are running.
func TestLimitEncodes_AC4_EveryStartedEncodeFinishesBeforeTheRunReturns(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	for i := 0; i < 6; i++ {
		encodeSource(t, ffmpeg, root, "f"+strconv.Itoa(i)+".mkv")
	}
	eng, counter := limitEncodesEngine(t, ffmpeg, ffprobe, root, 3,
		func(c *encodeCounter) { c.delay = 500 * time.Millisecond }, nil)
	if err := eng.RunBounded(context.Background(), Bound{LimitEncodes: 3}); err != nil {
		t.Fatalf("RunBounded(limit_encodes 3): %v", err)
	}
	rows := everyRowByName(t, eng.Store.(*testStore))
	for _, name := range counter.encoded() {
		r, ok := rows[name]
		if !ok || !r.Status.Terminal() {
			t.Errorf("%s reached an encode and holds %q after the run returned, want a terminal row", name, r.Status)
		}
	}
	for name, r := range rows {
		if !r.Status.Terminal() {
			t.Errorf("%s is left in %q after the run returned", name, r.Status)
		}
	}
	if got := counter.encoded(); len(got) != 3 {
		t.Errorf("the encoder was handed %v, want exactly three files", got)
	}
}

// TestLimitEncodes_AC5_SaysWhetherTheBoundWasReachedAtInfo grades AC-5: a library where
// fewer than N files reach an encode - every file skipped, or some - is processed to the
// end, the run returns success, and an `info` record says the bound was not reached with
// the count reached; a run that reaches it says so with the same count. Nothing is `error`.
func TestLimitEncodes_AC5_SaysWhetherTheBoundWasReachedAtInfo(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	cases := []struct {
		name           string
		skips, encodes int
		limit          int
		wantRows       int
		fields         []string
	}{
		{"every file is skipped", 3, 0, 2, 3,
			[]string{"level=INFO", "bound=limit_encodes", "bound_value=2", "encodes_reached=0", "bound_reached=false"}},
		{"fewer files reach an encode than the bound", 2, 1, 3, 3,
			[]string{"level=INFO", "bound=limit_encodes", "bound_value=3", "encodes_reached=1", "bound_reached=false"}},
		{"the bound is reached", 0, 2, 1, 1,
			[]string{"level=INFO", "bound=limit_encodes", "bound_value=1", "encodes_reached=1", "bound_reached=true"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for i := 0; i < tc.skips; i++ {
				skipSource(t, ffmpeg, root, "a_skip"+strconv.Itoa(i)+".mkv")
			}
			for i := 0; i < tc.encodes; i++ {
				encodeSource(t, ffmpeg, root, "b_enc"+strconv.Itoa(i)+".mkv")
			}
			var buf bytes.Buffer
			eng, _ := limitEncodesEngine(t, ffmpeg, ffprobe, root, 2, nil, nil)
			eng.Log = captureLogger(&buf)
			if err := eng.RunBounded(context.Background(), Bound{LimitEncodes: tc.limit}); err != nil {
				t.Fatalf("RunBounded: %v", err)
			}
			if rows := rowsByName(t, eng.Store.(*testStore)); len(rows) != tc.wantRows {
				t.Errorf("the run recorded %v, want %d rows", names(rows), tc.wantRows)
			}
			mustRecordFields(t, buf.String(), tc.fields)
			if strings.Contains(buf.String(), "level=ERROR") {
				t.Errorf("a complete bounded run recorded at error level:\n%s", buf.String())
			}
		})
	}
}

// TestLimitEncodes_AC6_DryRunCountsWouldTranscodeDecisions grades AC-6: under dry_run a
// would-transcode decision counts toward N, the offer stops after N of them, and nothing
// is encoded, swapped or deleted. Skips before them still do not count.
func TestLimitEncodes_AC6_DryRunCountsWouldTranscodeDecisions(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	skipSource(t, ffmpeg, root, "a_skip0.mkv")
	skipSource(t, ffmpeg, root, "a_skip1.mkv")
	sums := map[string]string{}
	for i := 0; i < 4; i++ {
		name := "b_enc" + strconv.Itoa(i) + ".mkv"
		encodeSource(t, ffmpeg, root, name)
		sums[name] = md5f(t, filepath.Join(root, name))
	}
	cfg := baseCfg(root)
	cfg.Workers = 4
	cfg.DryRun = true
	eng := New(cfg, probe.New(ffmpeg, ffprobe), refusingEncoder{t}, newTestStore(t, root), discardLogger())
	if err := eng.RunBounded(context.Background(), Bound{LimitEncodes: 2}); err != nil {
		t.Fatalf("RunBounded(dry_run, limit_encodes 2): %v", err)
	}
	rows := everyRowByName(t, eng.Store.(*testStore))
	would := 0
	for name, r := range rows {
		switch r.Status {
		case store.WouldTranscode:
			would++
		case store.Skipped:
			if !strings.HasPrefix(name, "a_skip") {
				t.Errorf("%s was skipped; the fixture expects it to be decided would-transcode", name)
			}
		default:
			t.Errorf("%s holds %q under dry_run", name, r.Status)
		}
	}
	if would != 2 {
		t.Errorf("--limit-encodes 2 under dry_run recorded %d would-transcode decisions, want exactly 2: %v", would, names(rows))
	}
	for _, s := range []string{"a_skip0.mkv", "a_skip1.mkv"} {
		if rows[s].Status != store.Skipped {
			t.Errorf("%s: want a skip row, got %q", s, rows[s].Status)
		}
	}
	for name, sum := range sums {
		if got := md5f(t, filepath.Join(root, name)); got != sum {
			t.Errorf("%s changed under dry_run", name)
		}
	}
	if n := nTemp(t, root); n != 0 {
		t.Errorf("dry_run left %d temp file(s)", n)
	}
}

// TestLimitEncodes_AC8_EitherCountBoundStopsTheOffer grades AC-8: with both --limit M and
// --limit-encodes N, whichever is reached first stops the offer.
func TestLimitEncodes_AC8_EitherCountBoundStopsTheOffer(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	t.Run("over skip-only files --limit 3 --limit-encodes 5 records exactly 3", func(t *testing.T) {
		root := t.TempDir()
		for i := 0; i < 6; i++ {
			skipSource(t, ffmpeg, root, "s"+strconv.Itoa(i)+".mkv")
		}
		var buf bytes.Buffer
		eng, _ := limitEncodesEngine(t, ffmpeg, ffprobe, root, 4, nil, nil)
		eng.Log = captureLogger(&buf)
		if err := eng.RunBounded(context.Background(), Bound{Limit: 3, LimitEncodes: 5}); err != nil {
			t.Fatalf("RunBounded: %v", err)
		}
		if rows := everyTerminalRow(t, eng.Store.(*testStore)); len(rows) != 3 {
			t.Errorf("recorded %d terminal outcomes, want exactly 3", len(rows))
		}
		mustRecordFields(t, buf.String(), []string{"bound=limit_encodes", "bound_reached=false", "stopped_by=limit"})
		mustRecordFields(t, buf.String(), []string{"bound=limit,limit_encodes", "bound_value=3,5", "limit=3", "limit_encodes=5"})
	})

	t.Run("over encodable files --limit 10 --limit-encodes 1 encodes exactly 1", func(t *testing.T) {
		root := t.TempDir()
		for i := 0; i < 4; i++ {
			encodeSource(t, ffmpeg, root, "e"+strconv.Itoa(i)+".mkv")
		}
		var buf bytes.Buffer
		eng, counter := limitEncodesEngine(t, ffmpeg, ffprobe, root, 4, nil, nil)
		eng.Log = captureLogger(&buf)
		if err := eng.RunBounded(context.Background(), Bound{Limit: 10, LimitEncodes: 1}); err != nil {
			t.Fatalf("RunBounded: %v", err)
		}
		if got := counter.encoded(); len(got) != 1 {
			t.Errorf("the encoder was handed %v, want exactly one file", got)
		}
		if rows := everyTerminalRow(t, eng.Store.(*testStore)); len(rows) != 1 {
			t.Errorf("recorded %d terminal outcomes, want exactly 1", len(rows))
		}
		mustRecordFields(t, buf.String(), []string{"bound=limit", "bound_reached=false", "stopped_by=limit_encodes"})
	})
}

// TestLimitEncodes_AC10_IsABoundedRunAsALimitRunIs grades AC-10: a --limit-encodes run is a
// bounded run exactly as a --limit run is. Before any file is offered it writes the bound
// record naming `limit_encodes` and its value, and with retention ENABLED over provably
// prunable rows it removes none - where an unbounded pass over the same fixture does.
func TestLimitEncodes_AC10_IsABoundedRunAsALimitRunIs(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	t.Run("the bound record", func(t *testing.T) {
		root := t.TempDir()
		skipSource(t, ffmpeg, root, "one.mkv")
		var buf bytes.Buffer
		eng, _ := ownedEngine(t, ffmpeg, ffprobe, root, "ext4")
		eng.Log = captureLogger(&buf)
		if err := eng.RunBounded(context.Background(), Bound{LimitEncodes: 4}); err != nil {
			t.Fatalf("RunBounded: %v", err)
		}
		mustRecordFields(t, buf.String(), []string{
			"level=INFO", "bounded=true", "bound=limit_encodes", "bound_value=4",
			"stale_temp_sweep=owner-checked", "ledger_retention_pass=skipped",
		})
		// BEFORE any file is offered: the bound record precedes the file's own record.
		logged := buf.String()
		at, file := strings.Index(logged, "bound=limit_encodes"), strings.Index(logged, "one.mkv")
		if at < 0 || file < 0 || at > file {
			t.Errorf("the bound record does not come before the first record naming a file:\n%s", logged)
		}
	})

	t.Run("no ledger row is removed", func(t *testing.T) {
		root := t.TempDir()
		for i := 0; i < 2; i++ {
			skipSource(t, ffmpeg, root, "already"+strconv.Itoa(i)+".mkv")
		}
		eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.HistoryRetentionRows = 2 })
		if !eng.Cfg.RetentionEnabled() {
			t.Fatal("this fixture asserts nothing unless retention is ENABLED")
		}
		ts := eng.Store.(*testStore)
		for i := 0; i < 12; i++ {
			seedRow(t, ts, filepath.Join(root, "gone"+strconv.Itoa(i)+".mkv"), store.Skipped, &store.Outcome{Reason: SkipLowBitrate})
		}
		before := len(terminalRows(t, ts))
		if err := eng.RunBounded(context.Background(), Bound{LimitEncodes: 1}); err != nil {
			t.Fatalf("RunBounded: %v", err)
		}
		if after := len(terminalRows(t, ts)); after < before {
			t.Fatalf("a --limit-encodes run removed %d terminal row(s); it must remove none", before-after)
		}
		// The anti-vacuity arm: the same fixture DOES prune under an unbounded pass.
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		if got := len(terminalRows(t, ts)); got != 2 {
			t.Fatalf("an unbounded pass over the same fixture left %d terminal rows, want 2", got)
		}
	})
}
