package engine

// S0167 - a skipped row records what its source was.
//
// The ledger is the record of what was decided about each source, and a skipped row that
// left out the codec and the dimensions the probe had already read could only be asked about
// by probing the whole library again. Every case here reads the row BACK from the store, and
// every case about cost counts the ffprobe and ffmpeg subprocesses spawned around the one
// ProcessFile call for the one file, through counting wrappers, so "no extra probe" is a
// number and not an argument.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// factsRig is one engine whose every ffprobe and ffmpeg subprocess is counted. The fixtures
// are written and measured with the REAL binaries, which are never counted.
type factsRig struct {
	eng     *Engine
	ts      *testStore
	probes  func() int
	ffmpegs func() int
}

// factsEngine builds an engine over cfg with counting wrappers in front of both tools.
// ffprobe is the binary the wrapper delegates to, so a case can hand in one that already
// misreports something.
func factsEngine(t *testing.T, ffmpeg, ffprobe, root string, cfg config.Config) factsRig {
	t.Helper()
	d := t.TempDir()
	countedFFmpeg, ffmpegLog := countingWrapper(t, d, "ffmpeg", ffmpeg)
	countedFFprobe, ffprobeLog := countingWrapper(t, d, "ffprobe", ffprobe)
	ts := newTestStore(t, root)
	prober := probe.New(countedFFmpeg, countedFFprobe)
	eng := New(cfg, prober, FFmpegEncoder{FFmpeg: countedFFmpeg, Cfg: cfg, Probe: prober}, ts, discardLogger())
	return factsRig{
		eng: eng, ts: ts,
		probes:  func() int { return countCalls(t, ffprobeLog) },
		ffmpegs: func() int { return countCalls(t, ffmpegLog) },
	}
}

// process runs ProcessFile for one file and returns how many ffprobe and ffmpeg
// subprocesses THAT call spawned.
func (r factsRig) process(t *testing.T, f string) (probes, ffmpegs int) {
	t.Helper()
	p0, f0 := r.probes(), r.ffmpegs()
	if err := r.eng.ProcessFile(context.Background(), "w0", f); err != nil {
		t.Fatalf("ProcessFile(%s): %v", f, err)
	}
	return r.probes() - p0, r.ffmpegs() - f0
}

// plainCfg is a root with no resolution rules and no ceiling, mutated by the case.
func plainCfg(root string, mutate func(*config.Config)) config.Config {
	cfg := baseCfg(root)
	if mutate != nil {
		mutate(&cfg)
	}
	return cfg
}

// bandedCfg is a root whose rules select on the source height, which is the other
// configuration that takes the snapshot before the pre-claim guards.
func bandedCfg(t *testing.T, root string) config.Config {
	t.Helper()
	return profileCfg(t, `
library_roots:
  - path: `+root+`
    preset: ultrafast
    min_bitrate_kbps: 0
    rules:
      - when:
          max_source_height: 576
        crf: 31
encoder: cpu
vmaf_enable: false
`)
}

// skippedRow reads the one row for path back from the store and requires it to be the
// named skip.
func skippedRow(t *testing.T, ts *testStore, path, reason string) store.Outcome {
	t.Helper()
	row := rowFor(t, ts, path)
	if row.Status != store.Skipped || row.Outcome.Reason != reason {
		t.Fatalf("%s is %q/%q, want skipped/%s", path, row.Status, row.Outcome.Reason, reason)
	}
	return row.Outcome
}

// wantSourceFacts requires the row to carry exactly the codec and the dimensions the real
// ffprobe reports for the file's first video stream.
func wantSourceFacts(t *testing.T, o store.Outcome, ffprobe, path string) {
	t.Helper()
	wantCodec := codecOf(t, ffprobe, path)
	if wantCodec == "" {
		t.Fatalf("precondition: ffprobe names no codec for %s", path)
	}
	wantW, wantH := dimsOf(t, ffprobe, path)
	if o.SourceCodec != wantCodec {
		t.Errorf("source codec = %q, want %q (ffprobe's codec_name for the first video stream)",
			o.SourceCodec, wantCodec)
	}
	if o.SourceWidth == nil || o.SourceHeight == nil {
		t.Fatalf("source dimensions = %s, want %dx%d", pixels(o.SourceWidth, o.SourceHeight), wantW, wantH)
	}
	if *o.SourceWidth != wantW || *o.SourceHeight != wantH {
		t.Errorf("source dimensions = %dx%d, want %dx%d", *o.SourceWidth, *o.SourceHeight, wantW, wantH)
	}
}

// wantNoSourceFacts requires all three facts to be NOT RECORDED.
func wantNoSourceFacts(t *testing.T, o store.Outcome) {
	t.Helper()
	if o.SourceCodec != "" {
		t.Errorf("source codec = %q, want not recorded", o.SourceCodec)
	}
	if o.SourceWidth != nil || o.SourceHeight != nil {
		t.Errorf("source dimensions = %s, want not recorded: a dimension nobody read is null, never 0",
			pixels(o.SourceWidth, o.SourceHeight))
	}
}

// skipCase is one source-side skip: the fixture that provokes it and the reason it records.
type skipCase struct {
	name   string
	reason string
	mutate func(*config.Config)
	// build writes the fixture under root and returns the file ProcessFile is handed.
	build func(t *testing.T, ffmpeg, root string) string
}

// postSnapshotSkips are skips decided off the probe snapshot, under a root that takes that
// snapshot in the guard chain. They share one fixture size that is not square, so a row
// with its width and height exchanged is caught.
func postSnapshotSkips() []skipCase {
	return []skipCase{
		{
			name: "already-at-target-codec", reason: SkipAlreadyTargetCodec,
			build: func(t *testing.T, ffmpeg, root string) string {
				src := filepath.Join(root, "done.mkv")
				mkHevc(t, ffmpeg, src, "2M")
				return src
			},
		},
		{
			name: "low-bitrate", reason: SkipLowBitrate,
			mutate: func(c *config.Config) { c.MinBitrateKbps = 50000 },
			build: func(t *testing.T, ffmpeg, root string) string {
				src := filepath.Join(root, "quiet.mkv")
				mkH264At(t, ffmpeg, src, "200k", "352x288")
				return src
			},
		},
		{
			name: "interlaced", reason: SkipInterlaced,
			build: func(t *testing.T, ffmpeg, root string) string {
				src := filepath.Join(root, "fields.mkv")
				mkH264Interlaced(t, ffmpeg, src, "8M")
				return src
			},
		},
		{
			name: "target-exists", reason: SkipTargetExists,
			build: func(t *testing.T, ffmpeg, root string) string {
				src := filepath.Join(root, "movie.mp4")
				mkH264(t, ffmpeg, src, "8M")
				if err := os.WriteFile(filepath.Join(root, "movie.mkv"), []byte("already here"), 0o600); err != nil {
					t.Fatal(err)
				}
				return src
			},
		},
	}
}

// TestS0167_AC1_ASkipDecidedOffTheSnapshotRecordsTheSourceCodecAndDimensions grades
// [AC-1]: a real run's skipped row, written after the snapshot, carries the codec_name and
// the coded dimensions ffprobe reports for the first video stream.
//
// MUTATION: drop the codec from `because` and every arm reds on the codec; return the
// dimensions exchanged from sourceFactsOf and the 352x288 arm reds.
func TestS0167_AC1_ASkipDecidedOffTheSnapshotRecordsTheSourceCodecAndDimensions(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, tc := range postSnapshotSkips() {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			src := tc.build(t, ffmpeg, root)
			before := md5f(t, src)
			r := factsEngine(t, ffmpeg, ffprobe, root, plainCfg(root, tc.mutate))
			r.process(t, src)
			wantSourceFacts(t, skippedRow(t, r.ts, src, tc.reason), ffprobe, src)
			if md5f(t, src) != before {
				t.Error("the source changed on a skip")
			}
		})
	}

	// The unreadable-stream-list skip is the one written past the guard chain, off the same
	// snapshot: the probe answers everything except the stream list.
	t.Run("unreadable-stream-list", func(t *testing.T) {
		root := t.TempDir()
		src := filepath.Join(root, "ep.mkv")
		mkH264(t, ffmpeg, src, "8M")
		blind := blindToStreamList(t, t.TempDir(), ffprobe, "")
		r := factsEngine(t, ffmpeg, blind, root, plainCfg(root, nil))
		r.process(t, src)
		wantSourceFacts(t, skippedRow(t, r.ts, src, SkipUnreadableStreamList), ffprobe, src)
	})
}

// TestS0167_AC2_ADryRunSkipRecordsWhatARealRunRecords grades [AC-2]: with dry_run on, the
// skipped row carries the same codec, width and height a real run records for that file.
//
// The two runs are over the same untouched file with separate ledgers, and the dry run's row
// is compared with the real run's own row as well as with ffprobe, so a dry run that recorded
// something plausible and different is caught.
func TestS0167_AC2_ADryRunSkipRecordsWhatARealRunRecords(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	// The two guards the criterion names, one that reads no threshold and one that does:
	// every other skip leaves through the same `because`, which the real-run arm covers.
	for _, tc := range postSnapshotSkips()[:2] {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			src := tc.build(t, ffmpeg, root)

			realRun := factsEngine(t, ffmpeg, ffprobe, root, plainCfg(root, tc.mutate))
			realRun.process(t, src)
			want := skippedRow(t, realRun.ts, src, tc.reason)

			dry := factsEngine(t, ffmpeg, ffprobe, root, plainCfg(root, func(c *config.Config) {
				if tc.mutate != nil {
					tc.mutate(c)
				}
				c.DryRun = true
			}))
			if !dry.eng.Cfg.DryRun {
				t.Fatal("precondition: the dry arm is not a dry run")
			}
			dry.process(t, src)
			got := skippedRow(t, dry.ts, src, tc.reason)

			wantSourceFacts(t, got, ffprobe, src)
			if got.SourceCodec != want.SourceCodec ||
				pixels(got.SourceWidth, got.SourceHeight) != pixels(want.SourceWidth, want.SourceHeight) {
				t.Errorf("the dry run recorded %q %s, the real run %q %s",
					got.SourceCodec, pixels(got.SourceWidth, got.SourceHeight),
					want.SourceCodec, pixels(want.SourceWidth, want.SourceHeight))
			}
		})
	}
}

// preClaimCase is one of the guards that fire before the claim, with the arrangement that
// provokes it.
type preClaimCase struct {
	name   string
	reason string
	// arrange makes the guard fire for src. outside is a directory under no library root.
	arrange func(t *testing.T, r factsRig, src, outside string)
}

func preClaimCases() []preClaimCase {
	return []preClaimCase{
		{
			name: "hardlinked", reason: SkipHardlinked,
			arrange: func(t *testing.T, _ factsRig, src, outside string) {
				if err := os.Link(src, filepath.Join(outside, "seed.mkv")); err != nil {
					t.Fatalf("link: %v", err)
				}
			},
		},
		{
			name: "operator-withheld", reason: SkipOperatorExcluded,
			arrange: func(t *testing.T, r factsRig, src, _ string) {
				if _, err := r.ts.ExcludePath(context.Background(), src); err != nil {
					t.Fatalf("ExcludePath: %v", err)
				}
			},
		},
	}
}

// fixtureDirs returns a library root and a sibling directory outside it on the same
// filesystem, so a hard link can be made that no scan would meet.
func fixtureDirs(t *testing.T) (root, outside string) {
	t.Helper()
	_, dirs := twoRoots(t, "library", "outside")
	return dirs[0], dirs[1]
}

// TestS0167_AC3_APreClaimSkipRecordsTheSnapshotItsRootAlreadyTook grades [AC-3]: under a
// root whose configuration takes the snapshot before the hardlink and the withheld guards (a
// ceiling, or rules that band on the source height), the skipped row carries that snapshot's
// codec and dimensions, and the file costs that ONE ffprobe and nothing else.
//
// MUTATION: hand RecordSkip an empty SourceFacts and every arm reds on the codec; take a
// second snapshot to fill the row and the count reds.
func TestS0167_AC3_APreClaimSkipRecordsTheSnapshotItsRootAlreadyTook(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	roots := []struct {
		name string
		cfg  func(t *testing.T, root string) config.Config
	}{
		{"max_height ceiling", func(_ *testing.T, root string) config.Config { return plainCfg(root, capping(240)) }},
		{"height-banded rules", bandedCfg},
	}
	for _, rc := range roots {
		for _, tc := range preClaimCases() {
			t.Run(rc.name+"/"+tc.name, func(t *testing.T) {
				root, outside := fixtureDirs(t)
				src := filepath.Join(root, "movie.mkv")
				mkH264At(t, ffmpeg, src, "8M", "352x288")
				r := factsEngine(t, ffmpeg, ffprobe, root, rc.cfg(t, root))
				tc.arrange(t, r, src, outside)

				probes, ffmpegs := r.process(t, src)
				wantSourceFacts(t, skippedRow(t, r.ts, src, tc.reason), ffprobe, src)
				if probes != 1 || ffmpegs != 0 {
					t.Errorf("the file spawned %d ffprobe and %d ffmpeg, want exactly the 1 snapshot "+
						"this root already takes and no ffmpeg", probes, ffmpegs)
				}
			})
		}
	}
}

// TestS0167_AC3_ASymlinkSkipRecordsTheSnapshotItsRootAlreadyTook is [AC-3]'s rule on the one
// guard inside the chain that fires before the chain takes a snapshot: under a ceiling the
// rule resolution has already probed the file, so the symlink skip's row records what that
// one snapshot read and the file costs nothing more. Under a root that probes nothing first
// the same skip records nulls, which TestS0167_AC5 grades.
func TestS0167_AC3_ASymlinkSkipRecordsTheSnapshotItsRootAlreadyTook(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, outside := fixtureDirs(t)
	target := filepath.Join(outside, "real.mkv")
	mkH264At(t, ffmpeg, target, "8M", "352x288")
	link := filepath.Join(root, "movie.mkv")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	r := factsEngine(t, ffmpeg, ffprobe, root, plainCfg(root, capping(240)))

	probes, ffmpegs := r.process(t, link)
	wantSourceFacts(t, skippedRow(t, r.ts, link, SkipSymlink), ffprobe, link)
	if probes != 1 || ffmpegs != 0 {
		t.Errorf("the file spawned %d ffprobe and %d ffmpeg, want exactly the 1 snapshot this root "+
			"already takes and no ffmpeg", probes, ffmpegs)
	}
}

// TestS0167_AC4_AnAlreadyAtTargetSkipCostsExactlyOneProbe grades [AC-4]: under a root with
// no rules and no ceiling, a file skipped already-at-target-codec spawns exactly one ffprobe
// and no ffmpeg - the one snapshot the build already spent - and the row carries the facts
// that one snapshot read.
func TestS0167_AC4_AnAlreadyAtTargetSkipCostsExactlyOneProbe(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "done.mkv")
	mkHevc(t, ffmpeg, src, "2M")
	r := factsEngine(t, ffmpeg, ffprobe, root, plainCfg(root, nil))

	probes, ffmpegs := r.process(t, src)
	if probes != 1 {
		t.Errorf("the skipped file spawned %d ffprobe, want exactly 1", probes)
	}
	if ffmpegs != 0 {
		t.Errorf("the skipped file spawned %d ffmpeg, want none", ffmpegs)
	}
	// The count is over a file whose row DOES carry the facts: one probe for a row that
	// recorded nothing would prove only that nothing was done.
	wantSourceFacts(t, skippedRow(t, r.ts, src, SkipAlreadyTargetCodec), ffprobe, src)
}

// TestS0167_AC5_ASkipDecidedBeforeAnySnapshotRecordsNullsAndProbesNothing grades [AC-5]:
// under a root with no rules and no ceiling, the hardlink, the withheld and the symlink
// guards decide before any snapshot, so the row records null codec, width and height and the
// file spawns no ffprobe and no ffmpeg.
//
// MUTATION: take a snapshot to fill the pre-claim row and the count reds; record the codec
// of a nil snapshot as anything but nothing and the null assertion reds.
func TestS0167_AC5_ASkipDecidedBeforeAnySnapshotRecordsNullsAndProbesNothing(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	check := func(t *testing.T, r factsRig, f, reason string) {
		t.Helper()
		probes, ffmpegs := r.process(t, f)
		wantNoSourceFacts(t, skippedRow(t, r.ts, f, reason))
		if probes != 0 || ffmpegs != 0 {
			t.Errorf("the file spawned %d ffprobe and %d ffmpeg, want none of either", probes, ffmpegs)
		}
	}
	for _, tc := range preClaimCases() {
		t.Run(tc.name, func(t *testing.T) {
			root, outside := fixtureDirs(t)
			src := filepath.Join(root, "movie.mkv")
			mkH264(t, ffmpeg, src, "8M")
			r := factsEngine(t, ffmpeg, ffprobe, root, plainCfg(root, nil))
			tc.arrange(t, r, src, outside)
			check(t, r, src, tc.reason)
		})
	}
	t.Run("symlink", func(t *testing.T) {
		root, outside := fixtureDirs(t)
		target := filepath.Join(outside, "real.mkv")
		mkH264(t, ffmpeg, target, "8M")
		link := filepath.Join(root, "movie.mkv")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		r := factsEngine(t, ffmpeg, ffprobe, root, plainCfg(root, nil))
		check(t, r, link, SkipSymlink)
	})
}

// TestS0167_AC6_AFactTheSnapshotDidNotEstablishIsNullAndNotProbedFor grades [AC-6]: a file
// with a video extension and no video in it, under a ceiling, skips
// undetermined-source-height off a snapshot that read neither a codec nor dimensions. The
// row records all three as not recorded and the file costs exactly the one ffprobe.
func TestS0167_AC6_AFactTheSnapshotDidNotEstablishIsNullAndNotProbedFor(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "truncated.mkv")
	if err := os.WriteFile(src, []byte("this is not a matroska file"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	r := factsEngine(t, ffmpeg, ffprobe, root, plainCfg(root, capping(240)))

	probes, ffmpegs := r.process(t, src)
	wantNoSourceFacts(t, skippedRow(t, r.ts, src, SkipUndeterminedSourceHeight))
	if probes != 1 {
		t.Errorf("the file spawned %d ffprobe, want exactly the 1 snapshot: a fact it did not "+
			"establish is not probed for again", probes)
	}
	if ffmpegs != 0 {
		t.Errorf("the file spawned %d ffmpeg, want none", ffmpegs)
	}
}

// TestS0167_AC7_ARowFromAnEarlierBuildIsNeitherBackfilledNorReprobed grades [AC-7]: a
// skipped row that records no codec and no dimensions, met again by a scan under a root with
// no rules and no ceiling while its fingerprint and its recorded inputs still match, keeps
// its nulls and costs no probe.
//
// The row is the one this build writes for the file with its three facts taken off again,
// so its fingerprint and its decision inputs are exactly the ones the next claim compares.
func TestS0167_AC7_ARowFromAnEarlierBuildIsNeitherBackfilledNorReprobed(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	ctx := context.Background()
	root := t.TempDir()
	src := filepath.Join(root, "done.mkv")
	mkHevc(t, ffmpeg, src, "2M")
	r := factsEngine(t, ffmpeg, ffprobe, root, plainCfg(root, nil))

	r.process(t, src)
	row := rowFor(t, r.ts, src)
	// Anti-vacuity: this build DOES record the facts for this file, so their absence below
	// is the row being left alone and not a file that never had any.
	wantSourceFacts(t, skippedRow(t, r.ts, src, SkipAlreadyTargetCodec), ffprobe, src)

	earlier := row.Outcome
	earlier.SourceCodec, earlier.SourceWidth, earlier.SourceHeight = "", nil, nil
	if err := r.ts.Finish(ctx, src, row.Fingerprint, store.Skipped, &earlier, r.eng.Cfg.MaxFailures); err != nil {
		t.Fatalf("seed the earlier build's row: %v", err)
	}
	seeded := rowFor(t, r.ts, src)
	wantNoSourceFacts(t, seeded.Outcome)

	probes, ffmpegs := r.process(t, src)
	if probes != 0 || ffmpegs != 0 {
		t.Errorf("meeting the row again spawned %d ffprobe and %d ffmpeg, want none: no re-probe", probes, ffmpegs)
	}
	after := rowFor(t, r.ts, src)
	wantNoSourceFacts(t, skippedRow(t, r.ts, src, SkipAlreadyTargetCodec))
	if after.Fingerprint != seeded.Fingerprint || after.UpdatedAt != seeded.UpdatedAt {
		t.Errorf("the row was rewritten (fingerprint %q -> %q, updated_at %d -> %d), want it left as it was",
			seeded.Fingerprint, after.Fingerprint, seeded.UpdatedAt, after.UpdatedAt)
	}
}

// TestS0167_AC3_SourceFactsOfReadsOnlyWhatTheSnapshotHolds pins the one reading every skip
// takes its facts from, at the level of the function: no snapshot is nothing at all, and a
// snapshot yields its codec and both dimensions or neither.
func TestS0167_AC3_SourceFactsOfReadsOnlyWhatTheSnapshotHolds(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	if got := sourceFactsOf(nil); got.Codec != "" || got.Width != nil || got.Height != nil {
		t.Errorf("no snapshot yields %+v, want nothing recorded", got)
	}

	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264At(t, ffmpeg, src, "8M", "352x288")
	got := sourceFactsOf(probe.New(ffmpeg, ffprobe).VideoProps(context.Background(), src))
	if got.Codec != "h264" {
		t.Errorf("codec = %q, want h264", got.Codec)
	}
	if got.Width == nil || got.Height == nil || *got.Width != 352 || *got.Height != 288 {
		t.Errorf("dimensions = %s, want 352x288", pixels(got.Width, got.Height))
	}

	blank := filepath.Join(d, "blank.mkv")
	if err := os.WriteFile(blank, []byte("this is not a matroska file"), 0o600); err != nil {
		t.Fatal(err)
	}
	got = sourceFactsOf(probe.New(ffmpeg, ffprobe).VideoProps(context.Background(), blank))
	if got.Codec != "" || got.Width != nil || got.Height != nil {
		t.Errorf("a snapshot that read nothing yields %q %s, want nothing recorded",
			got.Codec, pixels(got.Width, got.Height))
	}
}
