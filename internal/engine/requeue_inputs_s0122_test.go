package engine

import (
	"context"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// S0122 - the half of the read-set rule that is NOT "re-offer the file": honouring a row
// whose inputs did not move.
//
// Re-offering a file whose inputs moved is what regress_0079_F9_test.go grades. These
// cases grade the other direction, and it is the dangerous one: holdfast deletes a source
// after a transcode, so a rule that re-offers a library on any configuration edit turns one
// YAML change into a library-wide re-encode, and every encode that passes the gates
// destroys its source. Each case therefore pairs its zero with a mutation that DOES move
// the count, so "nothing was re-opened" can never be read off machinery that re-opens
// nothing at all.
//
// Three observables, the ones the criteria name: the count the survey reports (which is
// what `run`, `serve` and `validate` print), whether a file reaches the encoder on the next
// scan, and whether the row itself is still the terminal row it was.

// profiledLibrary is a library whose sources an ENCODE PROFILE selects, scanned once for
// real, with the configuration that decided it. The first scan is a real ffmpeg pass
// because the rows have to be the ones a run actually writes: a seeded row proves nothing
// about what a decision records.
type profiledLibrary struct {
	ts              *testStore
	cfg             config.Config
	root            string
	ffmpeg, ffprobe string
}

// newProfiledLibrary scans one hevc source (skipped at the codec guard) and one h264 source
// (transcoded through the gates and swapped) under an encode profile that selects both, and
// asserts the rows really were decided under that profile before any case reads them.
func newProfiledLibrary(t *testing.T) profiledLibrary {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	mkHevc(t, ffmpeg, filepath.Join(root, "keep.mkv"), "800k")
	mkH264(t, ffmpeg, filepath.Join(root, "movie.mkv"), "8M")

	// crf 26 rather than the root's 22, so every recorded crf below says which layer it
	// came from. The encoder is NOT overridden: these rows are the ones a profile edit
	// must not disturb, and moving the target codec is the case F9 already owns.
	profiles := []config.EncodeProfile{{Name: "bulk", Match: "*.mkv", CRF: intp(26)}}
	ts := run(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.EncodeProfiles = profiles })

	cfg := baseCfg(root)
	cfg.EncodeProfiles = profiles
	lib := profiledLibrary{ts: ts, cfg: cfg, root: root, ffmpeg: ffmpeg, ffprobe: ffprobe}

	keep := lib.row(t, "keep.mkv")
	if keep.Status != store.Skipped || keep.Outcome.Reason != SkipAlreadyTargetCodec {
		t.Fatalf("keep.mkv is %q/%q, want skipped/%s - the fixture never reached the codec guard",
			keep.Status, keep.Outcome.Reason, SkipAlreadyTargetCodec)
	}
	movie := lib.row(t, "movie.mkv")
	if movie.Status != store.Done {
		t.Fatalf("movie.mkv is %q/%q, want done - the fixture never reached the swap",
			movie.Status, movie.Outcome.Reason)
	}
	// Both rows were decided under the PROFILE and say so in what they recorded, which is
	// what makes every "still matching" below a statement about encode profiles rather than
	// about a configuration that has none.
	for _, r := range []store.Job{keep, movie} {
		if r.Outcome.Profile != "bulk" {
			t.Fatalf("%s records profile %q, want \"bulk\"", r.Path, r.Outcome.Profile)
		}
	}
	if got, ok := movie.Outcome.DecisionInputs.Value(InputCRF); !ok || got != "26" {
		t.Fatalf("the done row records crf=%q (recorded=%v), want the profile's 26 - the rows are not "+
			"decided under the encode profile and nothing below is about one", got, ok)
	}
	return lib
}

func (l profiledLibrary) row(t *testing.T, base string) store.Job {
	t.Helper()
	path := l.ts.findPath(t, base)
	if path == "" {
		t.Fatalf("no file named %s under %s", base, l.root)
	}
	return rowFor(t, l.ts, path)
}

// rescan runs another oneshot over the SAME ledger under cfg and reports what that scan did
// to the rows: which files it CLAIMED, and how many reached the encoder.
//
// The claim is the sharper of the two and it is the transition the rule decides. A row that
// is re-opened is claimed, whatever verdict the guards then reach - so a fix that re-opened
// a library and re-recorded the same verdicts would leave every status, reason and recorded
// input identical and would be caught here and nowhere else. The encoder count is the cost
// when that re-decision is not the same verdict.
func (l profiledLibrary) rescan(t *testing.T, cfg config.Config) (claimed []string, encodes int32) {
	t.Helper()
	var n atomic.Int32
	var mu sync.Mutex
	eng := New(cfg, probe.New(l.ffmpeg, l.ffprobe), countingEncoder(&n), l.ts, discardLogger())
	eng.onClaim = func(_, path string) {
		mu.Lock()
		defer mu.Unlock()
		claimed = append(claimed, filepath.Base(path))
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	sort.Strings(claimed)
	return claimed, n.Load()
}

// survey is the count the startup report and `validate` print, asked the way they ask it:
// once per row, about that row's own path.
func (l profiledLibrary) survey(t *testing.T, cfg config.Config) store.DecisionInputsSurvey {
	t.Helper()
	got, err := l.ts.SurveyDecisionInputs(context.Background(), DecisionInputsPerPath(cfg))
	if err != nil {
		t.Fatalf("SurveyDecisionInputs: %v", err)
	}
	return got
}

// TestRequeueInputs_AnUnchangedConfigurationReopensNothing grades [AC-2]: a second scan over
// a ledger whose rows were decided under encode profiles, with the configuration unchanged,
// re-opens none of them and hands no file to the encoder.
//
// It is the symmetry property standing on its own feet. A row records what its decision
// read, resolved for its path; the next scan resolves the same keys for the same path and
// finds the same values. If either side could reach a value the other could not, this is
// the case that reds - and on a library-sized ledger that failure is a re-encode of
// everything.
func TestRequeueInputs_AnUnchangedConfigurationReopensNothing(t *testing.T) {
	lib := newProfiledLibrary(t)

	got := lib.survey(t, lib.cfg)
	if got.Reopening() != 0 || got.Matching != 2 {
		t.Errorf("survey = %+v, want 2 matching and 0 re-opening: every row was decided under the "+
			"configuration still in force, so a scan that offered one back would be re-deciding a "+
			"verdict nothing changed", got)
	}
	claimed, n := lib.rescan(t, lib.cfg)
	if len(claimed) != 0 {
		t.Errorf("the second scan re-opened %v under an unchanged configuration - every one of those "+
			"rows was decided under the settings still in force", claimed)
	}
	if n != 0 {
		t.Errorf("the second scan handed %d file(s) to the encoder under an unchanged configuration", n)
	}

	// ANTI-VACUITY. The same rows under a configuration that moves a key they DID read are
	// counted as moved, so the zero above is a real zero and not a survey that cannot count.
	moved := lib.cfg
	moved.EncodeProfiles = []config.EncodeProfile{{Name: "bulk", Match: "*.mkv", Encoder: strp("svtav1")}}
	if got := lib.survey(t, moved); got.Moved != 2 {
		t.Errorf("survey under a profile that targets av1 = %+v, want both rows moved - the rows are "+
			"not re-openable at all, so the unchanged-configuration zero above proves nothing", got)
	}
}

// TestRequeueInputs_AnEditThatMovesNoRecordedKeyHonoursEveryRow grades [AC-3]: a
// configuration edit that moves only keys no terminal row's decision read honours every
// such row, on the three observables the criterion names - counted as matching, not
// re-opened, and its file not offered back.
//
// This is minimality. A digest of the configuration would re-open a library over a
// notification URL; only the keys a decision actually read are recorded, so an edit that
// moves none of them is an edit no row can see.
func TestRequeueInputs_AnEditThatMovesNoRecordedKeyHonoursEveryRow(t *testing.T) {
	lib := newProfiledLibrary(t)

	for _, tc := range []struct {
		name string
		edit func(*config.Config)
	}{
		{"a notification URL", func(c *config.Config) { c.NotifyURL = "file:/run/secrets/notify" }},
		{"an added library root", func(c *config.Config) {
			c.LibraryRoots = append(append([]string{}, c.LibraryRoots...), t.TempDir())
		}},
		{"an encode profile whose match does not select the row's path", func(c *config.Config) {
			c.EncodeProfiles = append([]config.EncodeProfile{
				{Name: "shorts", Match: "*.mp4", CRF: intp(40), Encoder: strp("svtav1")},
			}, c.EncodeProfiles...)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := lib.cfg
			tc.edit(&cfg)

			if got := lib.survey(t, cfg); got.Reopening() != 0 || got.Matching != 2 {
				t.Errorf("survey after editing %s = %+v, want 2 matching and 0 re-opening", tc.name, got)
			}
			claimed, n := lib.rescan(t, cfg)
			if len(claimed) != 0 {
				t.Errorf("editing %s re-opened %v - that edit moved no key either row recorded",
					tc.name, claimed)
			}
			if n != 0 {
				t.Errorf("editing %s handed %d file(s) to the encoder", tc.name, n)
			}
		})
	}
}

// TestRequeueInputs_AProfileEditOnAKeyTheGuardNeverReadHonoursTheRow grades [AC-4]: an
// encode profile that DOES select a row's path, edited on a key that row's decision did not
// read, honours the row on the same three observables.
//
// The row is a `skipped / already-at-target-codec`, whose guard reads the resolved target
// codec and nothing else. Its profile's `crf` is therefore a key the decision never looked
// at, and minimality is what makes that edit free - without it the fix for F9 would re-offer
// every skipped file in the library the first time an operator tuned a quality setting.
func TestRequeueInputs_AProfileEditOnAKeyTheGuardNeverReadHonoursTheRow(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	mkHevc(t, ffmpeg, filepath.Join(root, "keep.mkv"), "800k")

	profiles := []config.EncodeProfile{{Name: "bulk", Match: "*.mkv", CRF: intp(26)}}
	ts := run(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.EncodeProfiles = profiles })
	cfg := baseCfg(root)
	cfg.EncodeProfiles = profiles
	lib := profiledLibrary{ts: ts, cfg: cfg, root: root, ffmpeg: ffmpeg, ffprobe: ffprobe}

	keep := lib.row(t, "keep.mkv")
	if keep.Status != store.Skipped || keep.Outcome.Reason != SkipAlreadyTargetCodec {
		t.Fatalf("keep.mkv is %q/%q, want skipped/%s", keep.Status, keep.Outcome.Reason, SkipAlreadyTargetCodec)
	}
	// The guard read the target codec AND NO OTHER KEY. That is the premise of the case, so
	// it is asserted rather than assumed: if this row ever recorded crf, the edit below would
	// be a key it read and the criterion would be about something else.
	if got := keep.Outcome.DecisionInputs.Keys(); len(got) != 1 || got[0] != InputTargetCodec {
		t.Fatalf("the codec skip recorded %v, want exactly [%s]", got, InputTargetCodec)
	}

	edited := cfg
	edited.EncodeProfiles = []config.EncodeProfile{{Name: "bulk", Match: "*.mkv", CRF: intp(18)}}
	if got := lib.survey(t, edited); got.Reopening() != 0 || got.Matching != 1 {
		t.Errorf("survey after editing the matching profile's crf = %+v, want 1 matching and 0 "+
			"re-opening: the guard that wrote this row never read crf", got)
	}
	claimed, n := lib.rescan(t, edited)
	if len(claimed) != 0 {
		t.Errorf("editing the matching profile's crf re-opened %v - the guard that wrote that row "+
			"read the target codec and nothing else", claimed)
	}
	if n != 0 {
		t.Errorf("editing the matching profile's crf handed %d file(s) to the encoder", n)
	}
	if r := lib.row(t, "keep.mkv"); r.Status != store.Skipped || r.Outcome.Reason != SkipAlreadyTargetCodec {
		t.Errorf("the row is now %q/%q, want the skipped/%s it was", r.Status, r.Outcome.Reason, SkipAlreadyTargetCodec)
	}

	// ANTI-VACUITY. The same profile edited on the key this guard DID read moves the count,
	// so the zero above is minimality and not a row nothing can re-open.
	moved := cfg
	moved.EncodeProfiles = []config.EncodeProfile{{Name: "bulk", Match: "*.mkv", Encoder: strp("svtav1")}}
	if got := lib.survey(t, moved); got.Moved != 1 {
		t.Errorf("survey under a profile that moves the target codec = %+v, want the row counted as "+
			"moved - it cannot be re-opened at all, so the crf case above proves nothing", got)
	}
}

// TestRequeueInputs_TheThreeRowsNothingReopensSurviveAnyConfigurationChange grades [AC-8]:
// a job parked `indeterminate`, a swap recorded `applied-despite-error` and a
// `skipped / restored-original` row are still held out after any configuration change
// whatsoever, and none of those files is handed to the encoder.
//
// What holds each of them out is not a configuration question. Whether the swap was applied
// is exactly what is unknown for the parked job; the rename took effect for the second; and
// the third is an operator's rescued bytes, which re-opening would feed to the very gates
// that passed the encode they rejected. The read-set rule must not reach any of them, and
// the configuration here moves as far as it can: the top-level encoder AND an encode profile
// both target a different codec, which is the largest move of the read set there is.
func TestRequeueInputs_TheThreeRowsNothingReopensSurviveAnyConfigurationChange(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	for _, base := range []string{"parked.mkv", "applied.mkv", "rescued.mkv", "ordinary.mkv"} {
		mkHevc(t, ffmpeg, filepath.Join(root, base), "800k")
	}
	cfg := baseCfg(root)
	ts := newTestStore(t, root)
	ctx := context.Background()

	parked := filepath.Join(root, "parked.mkv")
	applied := filepath.Join(root, "applied.mkv")
	rescued := filepath.Join(root, "rescued.mkv")
	ordinary := filepath.Join(root, "ordinary.mkv")

	seedRowForRealFile(t, ts, parked, store.Indeterminate,
		&store.Outcome{Reason: "the swap did not complete cleanly"})
	seedRowForRealFile(t, ts, applied, store.AppliedDespiteError,
		&store.Outcome{Reason: "the rename took effect after the gate failed"})
	if _, err := ts.RecordSkip(ctx, rescued, probe.Fingerprint(rescued), SkipRestoredOriginal, store.Decision{}, ""); err != nil {
		t.Fatalf("RecordSkip(restored-original): %v", err)
	}
	// The fourth row is the anti-vacuity arm: an ordinary codec skip, recorded under the
	// configuration below as the guard records it. The same change that must not move the
	// three above MUST move this one, or the case is measuring a scan that does nothing.
	top := cfg.TopLevelProfile()
	seedRowForRealFile(t, ts, ordinary, store.Skipped,
		(&Engine{Cfg: cfg}).because(SkipAlreadyTargetCodec, store.Decision{}, top,
			cfg.TranscodeIn(top, ordinary), InputTargetCodec))

	moved := cfg
	moved.Encoder = "svtav1"
	moved.EncodeProfiles = []config.EncodeProfile{{Name: "av1", Match: "*.mkv", Encoder: strp("svtav1")}}

	var encodes atomic.Int32
	eng := New(moved, probe.New(ffmpeg, ffprobe), countingEncoder(&encodes), ts, discardLogger())
	if err := eng.RunOneshot(ctx); err != nil {
		t.Fatalf("RunOneshot under the moved configuration: %v", err)
	}

	for _, tc := range []struct {
		path   string
		status store.Status
		reason string
	}{
		{parked, store.Indeterminate, "the swap did not complete cleanly"},
		{applied, store.AppliedDespiteError, "the rename took effect after the gate failed"},
		{rescued, store.Skipped, SkipRestoredOriginal},
	} {
		got, _, exists, err := ts.Get(ctx, tc.path, probe.Fingerprint(tc.path))
		if err != nil {
			t.Fatalf("Get(%s): %v", tc.path, err)
		}
		if !exists || got != tc.status {
			t.Errorf("%s is %q (exists=%v), want %q - a configuration change re-opened a row no "+
				"configuration question holds", tc.path, got, exists, tc.status)
		}
		if r := rowFor(t, ts, tc.path); r.Outcome.Reason != tc.reason {
			t.Errorf("%s now reads %q, want %q", tc.path, r.Outcome.Reason, tc.reason)
		}
	}

	// The encoder saw exactly the anti-vacuity file and none of the three.
	if n := encodes.Load(); n != 1 {
		t.Errorf("%d file(s) reached the encoder, want exactly 1 (the ordinary re-opened row). "+
			"0 means the configuration change moved nothing and this case grades nothing; more than "+
			"1 means a held-out row was handed to the encoder", n)
	}
	if r := rowFor(t, ts, ordinary); r.Status == store.Skipped && r.Outcome.Reason == SkipAlreadyTargetCodec {
		t.Errorf("the ordinary codec skip was not re-opened by a configuration that moved its target "+
			"codec, so the three rows above were not tested against a change that reaches anything")
	}
}
