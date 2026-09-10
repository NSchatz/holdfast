package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// What a terminal row records, what re-opening costs, and the requeue lever (S0080).
//
// These are driven through the REAL pipeline over real clips, because the claim is about
// which configuration values each guard actually reads - and the only thing that can
// answer that is the guard running. A hand-built Outcome would assert what the test
// author believed the guard read.

// rowForFile returns the terminal row for the file whose path contains sub.
func rowForFile(t *testing.T, ts *testStore, sub string) store.Job {
	t.Helper()
	path := ts.findPath(t, sub)
	if path == "" {
		t.Fatalf("no file under the fixture matching %q", sub)
	}
	return rowFor(t, ts, path)
}

// TestTerminalRow_RecordsTheDecisionInputsItsGuardRead is the recording criterion, over
// the three verdicts it names: a low-bitrate row records the threshold it compared the
// source against, an already-at-target-codec row records the target codec, and a done row
// records the target codec plus the encoder key, the crf and the preset the encode used.
//
// "AND NO OTHERS" is asserted as hard as the presence is, and it is the half that matters:
// a row carrying the whole configuration would tie every file to every key, so correcting
// a notification URL would offer a library to the encoder.
func TestTerminalRow_RecordsTheDecisionInputsItsGuardRead(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	// Two libraries, because the low-bitrate case needs a threshold the other two must
	// not be measured against: a 320x240 fixture does not reliably probe above 2500 kbps
	// however it was asked for, so a single run at that threshold would skip the file
	// whose DONE row is under test and prove nothing about it.
	quietRoot := t.TempDir()
	mkH264(t, ffmpeg, filepath.Join(quietRoot, "quiet.mkv"), "200k")
	quiet := run(t, ffmpeg, ffprobe, quietRoot, nil, func(c *config.Config) {
		c.MinBitrateKbps = 2500
	})

	root := t.TempDir()
	mkHevc(t, ffmpeg, filepath.Join(root, "at-codec.mkv"), "800k")
	mkH264(t, ffmpeg, filepath.Join(root, "fat.mkv"), "8M")
	ts := run(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) {
		c.CRF = 24
		c.Preset = "ultrafast"
	})

	for _, tc := range []struct {
		store  *testStore
		file   string
		status store.Status
		reason string
		want   map[string]string
	}{
		{
			store: quiet, file: "quiet.mkv", status: store.Skipped, reason: SkipLowBitrate,
			want: map[string]string{"min_bitrate_kbps": "2500"},
		},
		{
			store: ts, file: "at-codec.mkv", status: store.Skipped, reason: SkipAlreadyTargetCodec,
			want: map[string]string{"target_codec": "hevc"},
		},
		{
			store: ts, file: "fat.mkv", status: store.Done,
			want: map[string]string{
				"target_codec": "hevc", "encoder": "cpu", "crf": "24", "preset": "ultrafast",
			},
		},
	} {
		row := rowForFile(t, tc.store, tc.file)
		if row.Status != tc.status || row.Outcome.Reason != tc.reason {
			t.Errorf("%s is %q/%q, want %q/%q - the fixture did not reach the guard under test",
				tc.file, row.Status, row.Outcome.Reason, tc.status, tc.reason)
			continue
		}
		in := row.Outcome.DecisionInputs
		if !in.Recorded() {
			t.Errorf("%s recorded NO decision inputs; its verdict cannot be re-derived", tc.file)
			continue
		}
		keys := in.Keys()
		if len(keys) != len(tc.want) {
			t.Errorf("%s recorded %v, want exactly the keys its guard read %v", tc.file, keys, tc.want)
		}
		for k, want := range tc.want {
			if got, ok := in.Value(k); !ok || got != want {
				t.Errorf("%s recorded %s=%q (present=%v), want %q", tc.file, k, got, ok, want)
			}
		}
		for _, k := range keys {
			if _, ok := tc.want[k]; !ok {
				t.Errorf("%s recorded %s=%q, which its guard never read: a row tied to a value the "+
					"decision did not use is re-opened by an edit that changes nothing about it",
					tc.file, k, mustValue(in, k))
			}
		}
	}
}

func mustValue(in store.DecisionInputs, k string) string {
	v, _ := in.Value(k)
	return v
}

// TestReopen_AFileThatSkipsAgainIsNeverEncoded. Re-opening is not re-encoding, and this is
// the assertion that says so: under a configuration that HAS moved, the rows are offered
// to the pipeline again, the guards decide them again, and nothing reaches the encoder,
// nothing is written beside the source, and the source is byte-for-byte what it was.
func TestReopen_AFileThatSkipsAgainIsNeverEncoded(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	for _, n := range []string{"a.mkv", "b.mkv"} {
		mkHevc(t, ffmpeg, filepath.Join(root, n), "800k")
	}

	var encodes atomic.Int32
	// Scan one under one crf, scan two under another: every row's record has moved, so
	// every row is re-opened. They are hevc against an hevc target, so the
	// already-at-target-codec guard decides them again before anything is encoded.
	eng := buildEngine(t, ffmpeg, ffprobe, root, countingEncoder(&encodes), func(c *config.Config) { c.CRF = 22 })
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("first RunOneshot: %v", err)
	}
	ts := eng.Store.(*testStore)
	before := treeState(t, root)
	if n := encodes.Load(); n != 0 {
		t.Fatalf("the first scan encoded %d file(s); this fixture has nothing to encode", n)
	}

	eng.Cfg.CRF = 23
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("second RunOneshot: %v", err)
	}

	if n := encodes.Load(); n != 0 {
		t.Errorf("re-opening handed %d file(s) to the encoder; a file that reaches the same verdict "+
			"must reach it before anything is encoded", n)
	}
	for path, sum := range before {
		now, ok := treeState(t, root)[path]
		if !ok {
			t.Errorf("%s is GONE after a re-open", path)
			continue
		}
		if now != sum {
			t.Errorf("%s changed under a re-open that encoded nothing", path)
		}
	}
	if extra := len(treeState(t, root)) - len(before); extra != 0 {
		t.Errorf("a re-open that encoded nothing left %d new file(s) beside the sources", extra)
	}

	// And the rows it reached now carry the configuration in force, so the scan after
	// this one leaves them alone - the property that stops this being every scan for ever.
	for _, n := range []string{"a.mkv", "b.mkv"} {
		row := rowForFile(t, ts, n)
		if got, ok := row.Outcome.DecisionInputs.Value("target_codec"); !ok || got != "hevc" {
			t.Errorf("%s did not record the inputs its second decision read: %+v", n, row.Outcome.DecisionInputs)
		}
	}
}

// treeState is every regular file under root against its size:mtime, which is all these
// cases need: an encode beside a source is a new entry, and a swap moves an existing one.
func treeState(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		out[path] = probe.Fingerprint(path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// hardlink makes link a second name for src, which is what an *arr import that is also an
// active seed looks like to the hardlink guard.
func hardlink(t *testing.T, src, link string) {
	t.Helper()
	if err := os.Link(src, link); err != nil {
		t.Fatalf("link %s -> %s: %v", link, src, err)
	}
}

// removeFileForTest removes one file, or fails the test saying which.
func removeFileForTest(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
}

// asErr is errors.As, kept local so the import list of this file stays about what it is
// testing.
func asErr[T error](err error, target *T) bool { return errors.As(err, target) }

func itoa(n int) string { return strconv.Itoa(n) }

// TestReopen_LeavesTheTwoMutableGuardsClearingAsBefore. The hardlink and
// undo-retention-failed guards are cleared and re-derived on every pass, and the recorded
// inputs must not change that in either direction: a stale skip is still dropped the
// moment its condition lifts, and a live one is still recorded.
func TestReopen_LeavesTheTwoMutableGuardsClearingAsBefore(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "seeded.mkv")
	mkH264(t, ffmpeg, src, "8M")
	link := filepath.Join(root, "seed-link.mkv")
	hardlink(t, src, link)

	var encodes atomic.Int32
	eng := buildEngine(t, ffmpeg, ffprobe, root, countingEncoder(&encodes), nil)
	ts := eng.Store.(*testStore)

	// Pass one: hardlinked, so the guard records the skip and the file never enters the
	// pipeline. RecordSkip writes it, and it records no decision inputs.
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("first RunOneshot: %v", err)
	}
	if got := skipReason(t, ts, "seeded.mkv"); got != SkipHardlinked {
		t.Fatalf("seeded.mkv is skipped %q, want %q - the fixture did not reach the hardlink guard", got, SkipHardlinked)
	}
	if n := encodes.Load(); n != 0 {
		t.Fatalf("a hardlinked file reached the encoder %d time(s)", n)
	}

	// The seed finishes: the link goes away, and the guard's condition with it. The
	// stale row must be CLEARED and the file reclaimed on the normal path - which is
	// exactly what it did before this column existed, and must still do even though the
	// row records nothing.
	removeFileForTest(t, link)
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("second RunOneshot: %v", err)
	}
	if n := encodes.Load(); n == 0 {
		t.Error("the hardlink went away and the file was not reclaimed: the mutable guard stopped " +
			"clearing, so a file is parked for the life of the install")
	}
	if got := skipReason(t, ts, "seeded.mkv"); got == SkipHardlinked {
		t.Error("the stale hardlinked row survived the pass after its condition lifted")
	}
}

// ---- the requeue lever -------------------------------------------------------

// requeueStore is a bare ledger with no library behind it: requeue is about ROWS, and
// what it does to them is observable without an encode.
func requeueStore(t *testing.T) *testStore { return newTestStore(t, t.TempDir()) }

func seedTerminal(t *testing.T, ts *testStore, path string, st store.Status, o *store.Outcome) {
	t.Helper()
	ctx := context.Background()
	if ok, err := ts.Claim(ctx, path, "fp", "seed", 3, store.DecisionInputs{}); err != nil || !ok {
		t.Fatalf("seed claim %s: ok=%v err=%v", path, ok, err)
	}
	if err := ts.Finish(ctx, path, "fp", st, o, 3); err != nil {
		t.Fatalf("seed finish %s: %v", path, err)
	}
}

// TestRequeue_RefusesAGuardTokenThatMatchesNothing. A requeue that reported success
// against an empty set would leave an operator believing a file is back in the pipeline
// when nothing about it has changed, so an empty match set is a REFUSAL that names what
// it looked for - and it has changed no row on the way to saying so.
func TestRequeue_RefusesAGuardTokenThatMatchesNothing(t *testing.T) {
	ts := requeueStore(t)
	ctx := context.Background()
	recorded := DecisionInputsFor(baseCfg(ts.root))
	seedTerminal(t, ts, "/lib/quiet.mkv", store.Skipped,
		&store.Outcome{Reason: SkipLowBitrate, DecisionInputs: recorded})

	before := ledgerSnapshot(t, ts)
	res, err := Requeue(ctx, ts, RequeueSelector{Guard: SkipInterlaced}, 3)
	if err == nil {
		t.Fatal("a guard token no row carries must be REFUSED, not reported as a requeue of nothing")
	}
	var nothing ErrNothingReopened
	if !asErr(err, &nothing) {
		t.Fatalf("want an ErrNothingReopened, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), SkipInterlaced) {
		t.Errorf("the refusal does not name what it looked for: %v", err)
	}
	if len(res.Reopened) != 0 {
		t.Errorf("a refused requeue reported re-opening %v", res.Reopened)
	}
	assertLedgerUnchanged(t, ts, before)

	// The anti-vacuity half: the same command against a token rows DO carry re-opens
	// them, so the refusal above is about the empty set and not about requeue being
	// unable to do anything at all.
	if _, err := Requeue(ctx, ts, RequeueSelector{Guard: SkipLowBitrate}, 3); err != nil {
		t.Fatalf("requeue of a guard rows DO carry: %v", err)
	}
}

// TestRequeue_ReopensAMultiVideoStreamRow. The source-SHAPE guard reads no configuration
// key - what video streams a file carries is a property of the file - so its rows record
// nothing read and no configuration change will ever offer one back to the pipeline. That
// makes `requeue --guard multi-video-stream` the ONLY lever over them, and a token absent
// from SkipGuards is not an inconvenience but a permanent exclusion: requeue answers
// ErrUnknownGuard for one and the operator has nothing else to reach for. The rule over
// the whole vocabulary is asserted in internal/docscheck; this is the behaviour.
func TestRequeue_ReopensAMultiVideoStreamRow(t *testing.T) {
	ts := requeueStore(t)
	ctx := context.Background()
	cfg := baseCfg(ts.root)
	const path = "/lib/two-angles.mkv"

	// The row exactly as the guard writes it, built by the guard's own helper: the token,
	// and the empty set of decision inputs that a verdict no key can move records.
	seedTerminal(t, ts, path, store.Skipped,
		(&Engine{Cfg: cfg}).because(SkipMultiVideoStream, store.Decision{}, cfg.TopLevelProfile()))

	// The precondition that makes this the case requeue exists for: a MOVED configuration
	// re-opens nothing here, because the row read nothing for a new value to disagree with.
	moved := cfg
	moved.MinBitrateKbps = cfg.MinBitrateKbps + 1000
	moved.CRF = cfg.CRF + 3
	if ok, err := ts.Claim(ctx, path, "fp", "w0", 3, DecisionInputsFor(moved)); err != nil || ok {
		t.Fatalf("a moved configuration re-opened the row (ok=%v err=%v), so this row is not the "+
			"kind requeue is the only lever for and the rest of this test proves nothing", ok, err)
	}

	res, err := Requeue(ctx, ts, RequeueSelector{Guard: SkipMultiVideoStream}, 3)
	if err != nil {
		t.Fatalf("requeue --guard %s: %v - the token an operator reads off the row has to be one "+
			"this build accepts, or the exclusion is permanent", SkipMultiVideoStream, err)
	}
	if len(res.Reopened) != 1 || res.Reopened[0] != path {
		t.Fatalf("re-opened %v, want exactly %s", res.Reopened, path)
	}
	if ok, err := ts.Claim(ctx, path, "fp", "w0", 3, DecisionInputsFor(cfg)); err != nil || !ok {
		t.Errorf("the re-opened row was not handed to the next claim: ok=%v err=%v", ok, err)
	}

	// And the refusal is still a refusal: a near-miss of the token is not quietly accepted
	// as the token, which is what makes the acceptance above mean something.
	if _, err := Requeue(ctx, requeueStore(t), RequeueSelector{Guard: SkipMultiVideoStream + "s"}, 3); err == nil {
		t.Error("a guard token this build does not know must be refused by name")
	} else {
		var unknown ErrUnknownGuard
		if !asErr(err, &unknown) {
			t.Errorf("want an ErrUnknownGuard for a near-miss of the token, got %T: %v", err, err)
		}
	}
}

// TestRequeue_LeavesRestoredOriginalAlone. That row is what stands between an operator's
// rescued bytes and the gates that passed the encode they rejected, so a requeue that
// matches it leaves it exactly as it is AND says that it did.
func TestRequeue_LeavesRestoredOriginalAlone(t *testing.T) {
	ts := requeueStore(t)
	ctx := context.Background()
	if _, err := ts.RecordSkip(ctx, "/lib/rescued.mkv", "fp", SkipRestoredOriginal, store.Decision{}); err != nil {
		t.Fatalf("RecordSkip: %v", err)
	}
	seedTerminal(t, ts, "/lib/ordinary.mkv", store.Skipped,
		&store.Outcome{Reason: SkipRestoredOriginal, DecisionInputs: DecisionInputsFor(baseCfg(ts.root))})

	before := ledgerSnapshot(t, ts)
	res, err := Requeue(ctx, ts, RequeueSelector{Guard: SkipRestoredOriginal}, 3)
	if err == nil {
		t.Fatal("every row matching restored-original is protected, so the requeue re-opened " +
			"nothing and must say so with a non-zero result")
	}
	if len(res.Reopened) != 0 {
		t.Fatalf("a restored-original row was re-opened: %v", res.Reopened)
	}
	if len(res.Protected) != 2 {
		t.Fatalf("the requeue reported %d protected row(s), want 2: %+v", len(res.Protected), res.Protected)
	}
	for _, p := range res.Protected {
		if !strings.Contains(p.Why, "undo window") {
			t.Errorf("%s was left alone without saying why: %q", p.Path, p.Why)
		}
	}
	assertLedgerUnchanged(t, ts, before)

	// And the protection is the STORE's too, not only this command's: even asked
	// directly, the row does not move.
	if changed, err := ts.Reopen(ctx, "/lib/rescued.mkv", "fp", false); err != nil || changed {
		t.Errorf("store.Reopen moved a restored-original row: changed=%v err=%v", changed, err)
	}
}

// TestRequeue_LeavesParkedAndAppliedDespiteErrorRowsAlone. Neither is a configuration
// question and neither is an operator-instruction question either: what is unknown about
// a parked job is unknown, and an applied-despite-error swap already happened.
func TestRequeue_LeavesParkedAndAppliedDespiteErrorRowsAlone(t *testing.T) {
	ts := requeueStore(t)
	ctx := context.Background()
	for _, st := range []store.Status{store.Indeterminate, store.AppliedDespiteError} {
		path := "/lib/" + string(st) + ".mkv"
		seedTerminal(t, ts, path, st, &store.Outcome{Reason: "the swap did not complete cleanly"})

		before := ledgerSnapshot(t, ts)
		res, err := Requeue(ctx, ts, RequeueSelector{Path: path}, 3)
		if err == nil {
			t.Fatalf("a %q row must never be re-opened, so this requeue re-opened nothing and must refuse", st)
		}
		if len(res.Reopened) != 0 {
			t.Fatalf("a %q row was re-opened: %v", st, res.Reopened)
		}
		if len(res.Protected) != 1 || res.Protected[0].Path != path {
			t.Fatalf("the requeue did not report leaving the %q row alone: %+v", st, res.Protected)
		}
		if res.Protected[0].Why == "" {
			t.Errorf("the %q row was left alone without saying why", st)
		}
		assertLedgerUnchanged(t, ts, before)

		if changed, err := ts.Reopen(ctx, path, "fp", true); err != nil || changed {
			t.Errorf("store.Reopen moved a %q row: changed=%v err=%v", st, changed, err)
		}
	}
}

// TestRequeue_ReopensTheRowsItNames covers the three selectors at the engine level: what
// each one matches, and that a re-opened row is one the next Claim hands over.
func TestRequeue_ReopensTheRowsItNames(t *testing.T) {
	ctx := context.Background()
	recorded := func(ts *testStore) store.DecisionInputs { return DecisionInputsFor(baseCfg(ts.root)) }

	t.Run("a path re-opens that one row", func(t *testing.T) {
		ts := requeueStore(t)
		seedTerminal(t, ts, "/lib/one.mkv", store.Skipped,
			&store.Outcome{Reason: SkipLowBitrate, DecisionInputs: recorded(ts)})
		seedTerminal(t, ts, "/lib/two.mkv", store.Skipped,
			&store.Outcome{Reason: SkipLowBitrate, DecisionInputs: recorded(ts)})

		res, err := Requeue(ctx, ts, RequeueSelector{Path: "/lib/one.mkv"}, 3)
		if err != nil {
			t.Fatalf("Requeue: %v", err)
		}
		if len(res.Reopened) != 1 || res.Reopened[0] != "/lib/one.mkv" {
			t.Fatalf("re-opened %v, want exactly /lib/one.mkv", res.Reopened)
		}
		if ok, err := ts.Claim(ctx, "/lib/one.mkv", "fp", "w0", 3, recorded(ts)); err != nil || !ok {
			t.Errorf("the re-opened row was not claimable: ok=%v err=%v", ok, err)
		}
		if ok, _ := ts.Claim(ctx, "/lib/two.mkv", "fp", "w0", 3, recorded(ts)); ok {
			t.Error("a row the requeue did not name was re-opened anyway")
		}
	})

	t.Run("a guard token re-opens every row carrying it", func(t *testing.T) {
		ts := requeueStore(t)
		seedTerminal(t, ts, "/lib/quiet-a.mkv", store.Skipped,
			&store.Outcome{Reason: SkipLowBitrate, DecisionInputs: recorded(ts)})
		seedTerminal(t, ts, "/lib/quiet-b.mkv", store.Skipped,
			&store.Outcome{Reason: SkipLowBitrate, DecisionInputs: recorded(ts)})
		seedTerminal(t, ts, "/lib/at-codec.mkv", store.Skipped,
			&store.Outcome{Reason: SkipAlreadyTargetCodec, DecisionInputs: recorded(ts)})

		res, err := Requeue(ctx, ts, RequeueSelector{Guard: SkipLowBitrate}, 3)
		if err != nil {
			t.Fatalf("Requeue: %v", err)
		}
		if len(res.Reopened) != 2 {
			t.Fatalf("re-opened %v, want both low-bitrate rows and nothing else", res.Reopened)
		}
		if ok, _ := ts.Claim(ctx, "/lib/at-codec.mkv", "fp", "w0", 3, recorded(ts)); ok {
			t.Error("a row carrying a DIFFERENT guard was re-opened")
		}
	})

	t.Run("failed re-opens a row parked at max_failures", func(t *testing.T) {
		ts := requeueStore(t)
		seedTerminal(t, ts, "/lib/parked.mkv", store.Failed, &store.Outcome{Reason: "ffmpeg died"})
		for i := 0; i < 2; i++ { // three failures in all: the bound is 3
			if err := ts.Finish(ctx, "/lib/parked.mkv", "fp", store.Failed,
				&store.Outcome{Reason: "ffmpeg died"}, 3); err != nil {
				t.Fatalf("Finish: %v", err)
			}
		}
		if ok, _ := ts.Claim(ctx, "/lib/parked.mkv", "fp", "w0", 3, store.DecisionInputs{}); ok {
			t.Fatal("the fixture is wrong: the row is not parked, so re-opening it proves nothing")
		}

		res, err := Requeue(ctx, ts, RequeueSelector{Failed: true}, 3)
		if err != nil {
			t.Fatalf("Requeue: %v", err)
		}
		if len(res.Reopened) != 1 {
			t.Fatalf("re-opened %v, want the parked row", res.Reopened)
		}
		if ok, err := ts.Claim(ctx, "/lib/parked.mkv", "fp", "w0", 3, store.DecisionInputs{}); err != nil || !ok {
			t.Errorf("--failed did not make the parked row claimable: ok=%v err=%v", ok, err)
		}
	})

	t.Run("a path re-opens a parked row and hands back its attempts", func(t *testing.T) {
		ts := requeueStore(t)
		seedTerminal(t, ts, "/lib/parked.mkv", store.Failed, &store.Outcome{Reason: "ffmpeg died"})
		for i := 0; i < 2; i++ { // three failures in all: the bound is 3
			if err := ts.Finish(ctx, "/lib/parked.mkv", "fp", store.Failed,
				&store.Outcome{Reason: "ffmpeg died"}, 3); err != nil {
				t.Fatalf("Finish: %v", err)
			}
		}
		if ok, _ := ts.Claim(ctx, "/lib/parked.mkv", "fp", "w0", 3, store.DecisionInputs{}); ok {
			t.Fatal("the fixture is wrong: the row is not parked, so re-opening it proves nothing")
		}

		res, err := Requeue(ctx, ts, RequeueSelector{Path: "/lib/parked.mkv"}, 3)
		if err != nil {
			t.Fatalf("Requeue: %v", err)
		}
		// The attempt count is what holds this row, so the selector that reached it is
		// not what decides whether the count is cleared - the row is.
		if ok, err := ts.Claim(ctx, "/lib/parked.mkv", "fp", "w0", 3, store.DecisionInputs{}); err != nil || !ok {
			t.Errorf("a path requeue reported the parked row re-opened and the next scan still "+
				"refuses it: ok=%v err=%v", ok, err)
		}
		if res.AttemptsCleared != 1 {
			t.Errorf("the requeue cleared %d attempt count(s), want 1 - an operator whose file "+
				"will be ENCODED again is owed that in the output", res.AttemptsCleared)
		}
	})

	t.Run("two selectors are refused", func(t *testing.T) {
		ts := requeueStore(t)
		seedTerminal(t, ts, "/lib/quiet.mkv", store.Skipped,
			&store.Outcome{Reason: SkipLowBitrate, DecisionInputs: recorded(ts)})
		before := ledgerSnapshot(t, ts)

		var many ErrTooManySelectors
		_, err := Requeue(ctx, ts, RequeueSelector{Guard: SkipLowBitrate, Failed: true}, 3)
		if !asErr(err, &many) {
			t.Fatalf("want an ErrTooManySelectors, got %T: %v", err, err)
		}
		if !strings.Contains(err.Error(), "--failed") || !strings.Contains(err.Error(), SkipLowBitrate) {
			t.Errorf("the refusal does not name both selectors it was given: %v", err)
		}
		assertLedgerUnchanged(t, ts, before)

		// And the narrowing it refuses to do silently is a real one: the guard alone
		// matches this row, so acting on the first selector in a switch would have
		// re-opened it and reported success.
		if _, err := Requeue(ctx, ts, RequeueSelector{Guard: SkipLowBitrate}, 3); err != nil {
			t.Fatalf("requeue of the guard alone: %v", err)
		}
	})

	t.Run("no selector is refused", func(t *testing.T) {
		ts := requeueStore(t)
		seedTerminal(t, ts, "/lib/one.mkv", store.Skipped,
			&store.Outcome{Reason: SkipLowBitrate, DecisionInputs: recorded(ts)})
		before := ledgerSnapshot(t, ts)
		if _, err := Requeue(ctx, ts, RequeueSelector{}, 3); err == nil {
			t.Fatal("a requeue with no selector must refuse rather than re-open the whole ledger")
		}
		assertLedgerUnchanged(t, ts, before)
	})

	t.Run("an unknown guard token is refused by name", func(t *testing.T) {
		ts := requeueStore(t)
		var unknown ErrUnknownGuard
		_, err := Requeue(ctx, ts, RequeueSelector{Guard: "low_bitrate"}, 3)
		if !asErr(err, &unknown) {
			t.Fatalf("want an ErrUnknownGuard, got %T: %v", err, err)
		}
		if !strings.Contains(err.Error(), SkipLowBitrate) {
			t.Errorf("the refusal does not name the guards this build recognises: %v", err)
		}
	})
}

// ledgerSnapshot is every row's status, reason, fail count and recorded inputs, so a
// refusal can be proved to have changed NOTHING rather than merely to have reported one.
func ledgerSnapshot(t *testing.T, ts *testStore) map[string]string {
	t.Helper()
	rows, err := ts.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	out := map[string]string{}
	for _, r := range rows {
		out[r.Path] = string(r.Status) + "|" + r.Outcome.Reason + "|" +
			r.Outcome.DecisionInputs.Encode() + "|" + itoa(r.FailCount)
	}
	return out
}

func assertLedgerUnchanged(t *testing.T, ts *testStore, before map[string]string) {
	t.Helper()
	after := ledgerSnapshot(t, ts)
	for path, want := range before {
		got, ok := after[path]
		if !ok {
			t.Errorf("%s was REMOVED by an operation that refused", path)
			continue
		}
		if got != want {
			t.Errorf("%s changed under an operation that refused:\n  before %s\n  after  %s", path, want, got)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("%s was CREATED by an operation that refused", path)
		}
	}
}
