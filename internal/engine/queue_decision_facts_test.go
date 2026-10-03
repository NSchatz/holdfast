package engine

// S0171 - the in-flight row carries the facts its own decision established.
//
// While a job encodes, its row is what an operator reads to see how big the running job is
// and which drive it is on. Every case here reads the row BACK from the ledger at the moment
// the job enters each state - the queue view is a projection of exactly that row - and the
// values it is compared with are measured independently, before the engine runs.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// rowWatch records the ledger row of one path at the moment it enters each state: at the
// claim (probing), and on the encoding and verifying transitions. It reads the store, never
// the event, so what it holds is what the queue view would have served.
type rowWatch struct {
	t    *testing.T
	st   store.Store
	path string

	mu   sync.Mutex
	rows map[store.Status]store.Job
}

// watchRows wires a rowWatch into eng for path.
func watchRows(t *testing.T, eng *Engine, path string) *rowWatch {
	t.Helper()
	w := &rowWatch{t: t, st: eng.Store, path: path, rows: map[store.Status]store.Job{}}
	eng.onClaim = func(_, p string) {
		if p == path {
			w.capture(store.Probing)
		}
	}
	eng.Observer = func(ev Event) {
		if ev.Path != path || ev.Progress != nil {
			return
		}
		if ev.Status == store.Encoding || ev.Status == store.Verifying {
			w.capture(ev.Status)
		}
	}
	return w
}

func (w *rowWatch) capture(at store.Status) {
	rows, err := w.st.List(context.Background(), nil, 0)
	if err != nil {
		w.t.Errorf("List at %s: %v", at, err)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range rows {
		if r.Path == w.path {
			if _, seen := w.rows[at]; !seen {
				w.rows[at] = r
			}
		}
	}
}

// at returns the row captured as the job entered status, requiring that it was captured and
// that the ledger really had it in that state.
func (w *rowWatch) at(status store.Status) store.Job {
	w.t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	row, ok := w.rows[status]
	if !ok {
		w.t.Fatalf("the job was never observed in %q", status)
	}
	if row.Status != status {
		w.t.Fatalf("the row read as the job entered %q says %q", status, row.Status)
	}
	return row
}

// decision is the six decision facts of one row, rendered so two rows compare with ==.
type decision struct {
	bytes, codec, dims, root, digest string
}

// sizeText renders a recorded size, with `not recorded` where the row holds none - so a
// size nobody wrote never compares equal to a zero.
func sizeText(p *int64) string {
	if p == nil {
		return "not recorded"
	}
	return strconv.FormatInt(*p, 10)
}

// undecided is a row that records NO decision fact.
func undecided() decision { return decision{bytes: sizeText(nil), dims: pixels(nil, nil)} }

func decisionOf(o store.Outcome) decision {
	return decision{
		bytes: sizeText(o.SourceBytes), codec: o.SourceCodec,
		dims: pixels(o.SourceWidth, o.SourceHeight),
		root: o.LibraryRoot, digest: o.ProfileDigest,
	}
}

// wantDecided is what the decision for src MUST record, measured before the engine runs:
// the size on disk, ffprobe's own codec and dimensions, and the configured root's cleaned
// path and digest.
func wantDecided(t *testing.T, cfg config.Config, ffprobe, src string) decision {
	t.Helper()
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	size := fi.Size()
	w, h := dimsOf(t, ffprobe, src)
	root := cfg.RootProfiles()[0]
	if root.Clean == "" || root.Profile.Digest() == "" {
		t.Fatalf("precondition: the configured root has no cleaned path or digest: %+v", root)
	}
	return decision{
		bytes: sizeText(&size), codec: codecOf(t, ffprobe, src),
		dims: pixels(&w, &h), root: root.Clean, digest: root.Profile.Digest(),
	}
}

// wantNoProof requires an in-flight row to carry nothing that describes an ENCODE: the
// decision facts are the only outcome columns an active row may hold.
func wantNoProof(t *testing.T, row store.Job) {
	t.Helper()
	o := row.Outcome
	if o.Reason != "" || o.Encoder != "" || o.VmafMean != nil || o.VmafMin != nil ||
		o.OutputBytes != nil || o.EncodeMs != nil || o.TargetPath != "" || o.OutputWidth != nil ||
		o.OutputHeight != nil || o.DecisionInputs.Recorded() || o.Profile != "" {
		t.Errorf("the %s row carries proof of an encode that has not finished: %+v", row.Status, o)
	}
}

// decisionRun runs one real encode of an h264 source to done under a rowWatch and returns
// the watch, the store, the values the decision must record and the source path.
func decisionRun(t *testing.T) (*rowWatch, *testStore, decision, string) {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkH264At(t, ffmpeg, src, "8M", "352x288")
	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, nil)
	want := wantDecided(t, eng.Cfg, ffprobe, src)
	w := watchRows(t, eng, src)
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	ts := eng.Store.(*testStore)
	if got := codecOf(t, ffprobe, src); got != "hevc" {
		t.Fatalf("the fixture job did not complete (codec %q): %+v", got, rowFor(t, ts, src))
	}
	return w, ts, want, src
}

// TestS0171_AC1_AnEncodingRowCarriesTheDecisionFactsOfItsAttempt grades [AC-1]: as the job
// enters encoding its ledger row holds a non-null size, codec, root and digest and the
// probe's dimensions, each equal to the decision - and nothing while it is still probing.
//
// MUTATION: skip the AdmitToEncoder call and every fact reds as not recorded; record
// anything but the keyed stat's size, the snapshot's facts or the decision's root and digest
// and the matching field reds.
func TestS0171_AC1_AnEncodingRowCarriesTheDecisionFactsOfItsAttempt(t *testing.T) {
	w, _, want, _ := decisionRun(t)

	encoding := w.at(store.Encoding)
	if got := decisionOf(encoding.Outcome); got != want {
		t.Errorf("the encoding row records %+v, want this attempt's decision %+v", got, want)
	}
	if want.bytes == sizeText(nil) || want.codec != "h264" || want.dims != "352x288" {
		t.Fatalf("precondition: the expected decision is %+v", want)
	}
	wantNoProof(t, encoding)

	// The attempt has not decided while it is probing, so the row says nothing yet.
	probing := w.at(store.Probing)
	if got := decisionOf(probing.Outcome); got != undecided() {
		t.Errorf("the probing row already records decision facts: %+v", got)
	}
}

// TestS0171_AC2_TheFactsStayOnTheRowThroughVerifying grades [AC-2]: the move from encoding
// to verifying leaves every decision fact on the row as it was.
//
// MUTATION: have Advance clear or rewrite any of the six columns and this reds.
func TestS0171_AC2_TheFactsStayOnTheRowThroughVerifying(t *testing.T) {
	w, _, want, _ := decisionRun(t)

	encoding, verifying := decisionOf(w.at(store.Encoding).Outcome), decisionOf(w.at(store.Verifying).Outcome)
	if verifying != encoding {
		t.Errorf("the verifying row records %+v, the encoding row recorded %+v", verifying, encoding)
	}
	if verifying != want {
		t.Errorf("the verifying row records %+v, want this attempt's decision %+v", verifying, want)
	}
	wantNoProof(t, w.at(store.Verifying))
}

// TestS0171_AC3_TheDoneRowRecordsTheSameDecisionTheEncodingRowCarried grades [AC-3]: the
// done row's size, dimensions, root and digest equal the ones the row carried while it was
// encoding - one decision, one set of facts.
//
// The done row's source codec is NOT one of the five: what a terminal row carries is
// unchanged, and it is pinned here as not recorded so that stays a decision rather than an
// accident.
func TestS0171_AC3_TheDoneRowRecordsTheSameDecisionTheEncodingRowCarried(t *testing.T) {
	w, ts, want, src := decisionRun(t)

	done := rowFor(t, ts, src)
	if done.Status != store.Done {
		t.Fatalf("status = %q, want done: %s", done.Status, done.Outcome.Reason)
	}
	got, encoding := decisionOf(done.Outcome), decisionOf(w.at(store.Encoding).Outcome)
	if done.Outcome.SourceCodec != "" {
		t.Errorf("the done row records source codec %q; a terminal row's facts are unchanged by this "+
			"work and a done row records none", done.Outcome.SourceCodec)
	}
	got.codec, encoding.codec, want.codec = "", "", ""
	if got != encoding {
		t.Errorf("the done row records %+v, the encoding row carried %+v", got, encoding)
	}
	if got != want {
		t.Errorf("the done row records %+v, want the decision %+v", got, want)
	}
}

// TestS0171_AC4_AClaimServesNoneOfAnEarlierAttemptsFacts grades [AC-4]: a row seeded with
// an earlier attempt's or a dry-run decision's facts is taken over by a claim, and none of
// the seeded values is served - nothing while probing, only this attempt's while encoding -
// with the rejected attempt's reason and VMAF figures still cleared.
//
// MUTATION: have the claim preserve any decision column, or have AdmitToEncoder skip one,
// and the seeded value shows through.
func TestS0171_AC4_AClaimServesNoneOfAnEarlierAttemptsFacts(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, seeded := range []store.Status{store.Failed, store.WouldTranscode} {
		t.Run(string(seeded), func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			src := filepath.Join(root, "movie.mkv")
			mkH264At(t, ffmpeg, src, "8M", "352x288")
			eng := buildEngine(t, ffmpeg, ffprobe, root, nil, nil)
			ts := eng.Store.(*testStore)
			want := wantDecided(t, eng.Cfg, ffprobe, src)

			key := probe.Fingerprint(src)
			if claimed, err := ts.Claim(ctx, src, key, "earlier", eng.Cfg.MaxFailures, store.DecisionInputs{}); err != nil || !claimed {
				t.Fatalf("seed claim: claimed=%v err=%v", claimed, err)
			}
			staleBytes, stalePx, mean, low := int64(999), 7, 87.5, 41.0
			stale := &store.Outcome{
				Reason: "VMAF worst-frame below floor", VmafMean: &mean, VmafMin: &low,
				SourceCodec: "vp9", SourceBytes: &staleBytes, SourceWidth: &stalePx, SourceHeight: &stalePx,
				Decision: store.Decision{LibraryRoot: "/stale-root", ProfileDigest: "stale"},
			}
			if err := ts.Finish(ctx, src, key, seeded, stale, eng.Cfg.MaxFailures); err != nil {
				t.Fatalf("seed finish: %v", err)
			}
			if got := decisionOf(rowFor(t, ts, src).Outcome); got.root != "/stale-root" || got.digest != "stale" || got.codec != "vp9" {
				t.Fatalf("precondition: the seeded row records %+v", got)
			}

			w := watchRows(t, eng, src)
			if err := eng.RunOneshot(ctx); err != nil {
				t.Fatalf("RunOneshot: %v", err)
			}

			probing := w.at(store.Probing)
			if got := decisionOf(probing.Outcome); got != undecided() {
				t.Errorf("the probing row serves an earlier attempt's facts: %+v", got)
			}
			wantNoProof(t, probing)

			encoding := w.at(store.Encoding)
			if got := decisionOf(encoding.Outcome); got != want {
				t.Errorf("the encoding row records %+v, want only this attempt's decision %+v", got, want)
			}
			wantNoProof(t, encoding)
		})
	}
}

// dimensionBlindFFprobe wraps the real ffprobe so that every answer about a path containing
// pathSub leaves out the width and height lines: the state of a source whose dimensions the
// probe did not establish, with everything else it says left as it is.
func dimensionBlindFFprobe(t *testing.T, dir, real, pathSub string) string {
	t.Helper()
	wrapper := filepath.Join(dir, "dimension-blind-ffprobe.sh")
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *\"" + pathSub + "\"*) \"" + real + "\" \"$@\" | grep -v -e '^width=' -e '^height=' -e '^coded_width=' -e '^coded_height='; exit 0 ;;\n" +
		"esac\n" +
		"exec \"" + real + "\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write the ffprobe wrapper: %v", err)
	}
	return wrapper
}

// TestS0171_AC6_UnestablishedDimensionsAreNullOnTheInFlightRow grades [AC-6]: where the
// guard pass's probe did not establish the source's dimensions and the job still reaches the
// encoder, the in-flight row carries no width and no height - never 0 - while the other
// decision facts are carried.
//
// MUTATION: record a zero for an unestablished dimension, or drop the whole record when the
// dimensions are missing, and one half of this reds.
func TestS0171_AC6_UnestablishedDimensionsAreNullOnTheInFlightRow(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	// An mp4 source, so the mkv the encoder writes is never a path the blind probe answers
	// about and every gate reads the output as it is.
	src := filepath.Join(root, "blindsource.mp4")
	mkH264At(t, ffmpeg, src, "8M", "352x288")
	blind := dimensionBlindFFprobe(t, t.TempDir(), ffprobe, "blindsource.mp4")
	if _, _, ok := probe.New(ffmpeg, blind).VideoProps(context.Background(), src).Dimensions(); ok {
		t.Fatal("precondition: the blind probe still establishes the source's dimensions")
	}

	eng := buildEngine(t, ffmpeg, blind, root, nil, nil)
	want := wantDecided(t, eng.Cfg, ffprobe, src)
	w := watchRows(t, eng, src)
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	encoding := w.at(store.Encoding)
	o := encoding.Outcome
	if o.SourceWidth != nil || o.SourceHeight != nil {
		t.Errorf("the in-flight row records dimensions %s the probe never established; want not "+
			"recorded, never 0", pixels(o.SourceWidth, o.SourceHeight))
	}
	want.dims = pixels(nil, nil)
	if got := decisionOf(o); got != want {
		t.Errorf("the in-flight row records %+v, want the other decision facts carried: %+v", got, want)
	}
}

// admitRefusingStore is a real store whose AdmitToEncoder fails, counting how often it was
// asked. Every other call is the real one.
type admitRefusingStore struct {
	store.Store
	mu    sync.Mutex
	calls int
}

var errAdmitRefused = errors.New("the ledger refused the decision facts")

func (s *admitRefusingStore) AdmitToEncoder(context.Context, string, string, store.DecisionFacts) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return errAdmitRefused
}

// factsWarnings are the warn records about the decision facts not being recorded.
func factsWarnings(t *testing.T, log *lockedBuffer) []map[string]any {
	t.Helper()
	var b bytes.Buffer
	b.WriteString(log.String())
	var out []map[string]any
	for _, rec := range logRecords(t, &b) {
		if rec["attempted"] == "record decision facts" {
			out = append(out, rec)
		}
	}
	return out
}

// TestS0171_AC7_AFailedFactsWriteChangesNothingAboutTheJob grades [AC-7]: a store error
// writing the decision facts leaves the encode, the gates and the terminal write exactly as
// they are when it succeeds - the same terminal status - emits one warn record naming the
// file, the dependency, what was attempted and that the job continues, and the queue shows
// those facts as not recorded rather than as anything made up.
//
// The control arm is the same job through a store that takes the write, so "the same" is
// compared and not assumed, and so a build that warned on every job is caught.
func TestS0171_AC7_AFailedFactsWriteChangesNothingAboutTheJob(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	type result struct {
		done     store.Job
		encoding store.Job
		warns    []map[string]any
		codec    string
	}
	run := func(t *testing.T, refuse bool) (result, *admitRefusingStore) {
		t.Helper()
		root := t.TempDir()
		src := filepath.Join(root, "movie.mkv")
		mkH264At(t, ffmpeg, src, "8M", "352x288")
		cfg := baseCfg(root)
		ts := newTestStore(t, root)
		var st store.Store = ts
		refusing := &admitRefusingStore{Store: ts}
		if refuse {
			st = refusing
		}
		log := &lockedBuffer{}
		prober := probe.New(ffmpeg, ffprobe)
		eng := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, st,
			slog.New(slog.NewJSONHandler(log, nil)))
		w := watchRows(t, eng, src)
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		return result{
			done: rowFor(t, ts, src), encoding: w.at(store.Encoding),
			warns: factsWarnings(t, log), codec: codecOf(t, ffprobe, src),
		}, refusing
	}

	control, _ := run(t, false)
	refused, st := run(t, true)

	if st.calls != 1 {
		t.Fatalf("the facts write was attempted %d times, want exactly once", st.calls)
	}
	if control.done.Status != store.Done || control.codec != "hevc" {
		t.Fatalf("precondition: the control job ended %q/%q, codec %q", control.done.Status,
			control.done.Outcome.Reason, control.codec)
	}
	if refused.done.Status != control.done.Status || refused.codec != control.codec {
		t.Errorf("with the facts write refused the job ended %q (codec %q), want what it ends as when "+
			"the write succeeds: %q (codec %q): %s", refused.done.Status, refused.codec,
			control.done.Status, control.codec, refused.done.Outcome.Reason)
	}
	// The terminal write defines the row's whole proof either way.
	got, want := decisionOf(refused.done.Outcome), decisionOf(control.done.Outcome)
	got.root, want.root, got.digest, want.digest = "", "", "", ""
	if got != want || refused.done.Outcome.Encoder != control.done.Outcome.Encoder {
		t.Errorf("the done row records %+v (encoder %q), the control's %+v (encoder %q)", got,
			refused.done.Outcome.Encoder, want, control.done.Outcome.Encoder)
	}

	// The queue shows the facts as not recorded, on a row that is still in flight.
	if got := decisionOf(refused.encoding.Outcome); got != undecided() {
		t.Errorf("the in-flight row records %+v after its facts write failed, want nothing recorded", got)
	}
	if got := decisionOf(control.encoding.Outcome); got.codec != "h264" || got.root == "" {
		t.Fatalf("precondition: the control's in-flight row records %+v", got)
	}

	if len(control.warns) != 0 {
		t.Errorf("a job whose facts were recorded logged %d warnings about them: %v", len(control.warns), control.warns)
	}
	if len(refused.warns) != 1 {
		t.Fatalf("got %d warn records about the facts write, want exactly 1: %v", len(refused.warns), refused.warns)
	}
	rec := refused.warns[0]
	if rec["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", rec["level"])
	}
	if file, _ := rec["file"].(string); !strings.HasSuffix(file, "movie.mkv") {
		t.Errorf("the record names file %v, want the job's source", rec["file"])
	}
	if rec["dependency"] != "store" {
		t.Errorf("dependency = %v, want store", rec["dependency"])
	}
	if msg, _ := rec["msg"].(string); !strings.Contains(msg, "the job continues") {
		t.Errorf("the record does not say the job continues: %q", msg)
	}
	if next, _ := rec["next"].(string); next == "" {
		t.Errorf("the record does not say what happens next: %v", rec)
	}
	if errText, _ := rec["err"].(string); !strings.Contains(errText, errAdmitRefused.Error()) {
		t.Errorf("the record carries err %q, want the store's own error", errText)
	}
}
