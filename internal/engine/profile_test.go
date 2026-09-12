package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// Per-library profiles, where they meet a real file.
//
// The claim under test is that a file is decided by the profile of the root it was
// enumerated under, and by no other. It is not a claim any output file can settle: two
// roots at different CRFs both produce a valid, smaller, faithful encode of the same
// source, and both leave a done row. So the encode is graded on the ARGV the production
// encoder actually assembled and handed to the subprocess, and the guards are graded on
// what the same fixture DID under two roots that disagree about it.

// profileCfg loads a real configuration from YAML, so these tests exercise the same
// resolution an operator gets rather than a Config assembled by hand. The engine-suite
// defaults that have nothing to do with profiles (the extension list, the duration
// tolerance, the retry bound) come from Load's own defaults layer.
func profileCfg(t *testing.T, yaml string) config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config.Validate: %v", err)
	}
	return *cfg
}

// twoRoots creates two SIBLING library roots under one temp dir. Sibling, because nested
// roots are refused at validate time - which is what makes "the root a file came from"
// answerable at all.
func twoRoots(t *testing.T, names ...string) (dir string, roots []string) {
	t.Helper()
	dir = t.TempDir()
	for _, n := range names {
		r := filepath.Join(dir, n)
		if err := os.MkdirAll(r, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", r, err)
		}
		roots = append(roots, r)
	}
	return dir, roots
}

// argvLog records the argv of every encode a run performed, keyed by the source the
// encode read. It is fed by FFmpegEncoder's argvObserver seam, so what it holds is the
// real command line the production encoder built - not a re-derivation of one.
type argvLog struct {
	mu   sync.Mutex
	byIn map[string][]string
}

func newArgvLog() *argvLog { return &argvLog{byIn: map[string][]string{}} }

func (l *argvLog) record(args []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-i" {
			l.byIn[args[i+1]] = append([]string(nil), args...)
			return
		}
	}
}

func (l *argvLog) forSource(t *testing.T, in string) []string {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	args, ok := l.byIn[in]
	if !ok {
		var seen []string
		for k := range l.byIn {
			seen = append(seen, k)
		}
		t.Fatalf("no encode was built for %s (encodes seen: %v)", in, seen)
	}
	return args
}

// runProfiles builds an engine over a multi-root configuration with the REAL production
// encoder, recording every argv it assembles, and performs one oneshot pass.
func runProfiles(t *testing.T, ffmpeg, ffprobe string, cfg config.Config) (*testStore, *argvLog) {
	t.Helper()
	log := newArgvLog()
	prober := probe.New(ffmpeg, ffprobe)
	ts := newTestStore(t, filepath.Dir(cfg.LibraryRoots[0]))
	enc := FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober, argvObserver: log.record}
	eng := New(cfg, prober, enc, ts, discardLogger())
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	return ts, log
}

// TestProcessFile_UsesTheProfileOfTheRootTheFileCameFrom.
//
// Two roots, five knobs different between them, one identical fixture under each - and
// the encode is graded on the argv the real FFmpegEncoder assembled. That is the only
// evidence that settles it: a valid hevc file and a valid av1 file are both what a
// working build produces, so the output cannot say which profile chose it.
//
// The anti-vacuity half is the second root. If the profile were not threaded, BOTH
// encodes would be built from the top level - so every assertion below would still find
// a well-formed argv, and the pair of them is what makes the test bite.
func TestProcessFile_UsesTheProfileOfTheRootTheFileCameFrom(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir, roots := twoRoots(t, "tv", "movies")
	tvSrc := filepath.Join(roots[0], "ep.mkv")
	movieSrc := filepath.Join(roots[1], "film.mkv")
	mkH264(t, ffmpeg, tvSrc, "8M")
	mkH264(t, ffmpeg, movieSrc, "8M")

	cfg := profileCfg(t, `
library_roots:
  - path: `+roots[0]+`
    crf: 30
    preset: ultrafast
    pixel_format: yuv420p10le
    container_ext: mkv
  - path: `+roots[1]+`
    encoder: svtav1
    crf: 40
    preset: fast
    pixel_format: yuv420p
    container_ext: mp4
encoder: cpu
crf: 22
preset: slow
vmaf_enable: false
min_bitrate_kbps: 0
`)
	_, log := runProfiles(t, ffmpeg, ffprobe, cfg)

	tvArgs := log.forSource(t, tvSrc)
	movieArgs := log.forSource(t, movieSrc)

	for _, tc := range []struct {
		name, flag, tvWant, movieWant string
	}{
		{"encoder", "-c:v", "libx265", "libsvtav1"},
		{"crf", "-crf", "30", "40"},
		{"preset", "-preset", "ultrafast", "10"}, // svtav1 maps the word "fast" to 10
		{"pixel_format", "-pix_fmt", "yuv420p10le", "yuv420p"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !hasArgPair(tvArgs, tc.flag, tc.tvWant) {
				t.Errorf("the encode of %s was built with %s != %q - it did not use its own root's "+
					"profile: %v", filepath.Base(tvSrc), tc.flag, tc.tvWant, tvArgs)
			}
			if !hasArgPair(movieArgs, tc.flag, tc.movieWant) {
				t.Errorf("the encode of %s was built with %s != %q - it did not use its own root's "+
					"profile: %v", filepath.Base(movieSrc), tc.flag, tc.movieWant, movieArgs)
			}
		})
	}

	// container_ext is the fifth knob and it decides the OUTPUT PATH rather than a flag,
	// so it is asserted on the argv's trailing operand.
	if got := movieArgs[len(movieArgs)-1]; filepath.Ext(got) != ".mp4" {
		t.Errorf("the encode of %s wrote to %q - its root forces container_ext: mp4", filepath.Base(movieSrc), got)
	}
	if got := tvArgs[len(tvArgs)-1]; filepath.Ext(got) != ".mkv" {
		t.Errorf("the encode of %s wrote to %q - its root forces container_ext: mkv", filepath.Base(tvSrc), got)
	}

	// Nothing was left half-written under either root.
	if n := nTemp(t, dir); n != 0 {
		t.Errorf("%d temp file(s) left behind", n)
	}
}

// TestProcessFile_BitrateGuardUsesTheRootsProfile.
//
// The same file, byte for byte, under two roots that disagree about min_bitrate_kbps:
// one skips it and the other does not. That is the whole of the criterion, and it is the
// guard whose wrong answer is cheapest to miss - a root that inherited the wrong floor
// either lets through 480p rips it should have left alone, or excludes 1080p files it
// should not have.
func TestProcessFile_BitrateGuardUsesTheRootsProfile(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	_, roots := twoRoots(t, "low", "any")
	guarded := filepath.Join(roots[0], "movie.mkv")
	allowed := filepath.Join(roots[1], "movie.mkv")
	mkH264(t, ffmpeg, guarded, "8M")
	mkH264(t, ffmpeg, allowed, "8M")

	cfg := profileCfg(t, `
library_roots:
  - path: `+roots[0]+`
    min_bitrate_kbps: 100000
  - path: `+roots[1]+`
    min_bitrate_kbps: 0
crf: 30
preset: ultrafast
container_ext: mkv
pixel_format: yuv420p10le
vmaf_enable: false
`)
	ts, _ := runProfiles(t, ffmpeg, ffprobe, cfg)

	skipped := rowFor(t, ts, guarded)
	if skipped.Status != store.Skipped || skipped.Outcome.Reason != SkipLowBitrate {
		t.Errorf("the file under the root with min_bitrate_kbps: 100000 ended %q/%q, want skipped/%s - "+
			"its guard read some other root's floor", skipped.Status, skipped.Outcome.Reason, SkipLowBitrate)
	}
	if !exists(guarded) {
		t.Errorf("the skipped source %s is gone", guarded)
	}

	// The SAME source under a root whose floor is 0 proceeds. Without this half the test
	// would pass against a build whose guard skipped everything.
	proceeded := rowFor(t, ts, allowed)
	if proceeded.Outcome.Reason == SkipLowBitrate {
		t.Errorf("the identical file under the root with min_bitrate_kbps: 0 was ALSO skipped by the " +
			"low-bitrate guard - the guard is not reading its own root's floor")
	}
	if proceeded.Status != store.Done {
		t.Errorf("the file under the root with min_bitrate_kbps: 0 ended %q (%s), want done",
			proceeded.Status, proceeded.Outcome.Reason)
	}
}

// TestTerminalRowCarriesTheDecidingProfile.
//
// Every terminal row - done, skipped and failed alike - records the cleaned root whose
// profile decided it and a digest of that profile's resolved values. Without both, a
// ledger read after a profile has been edited cannot say what the file was judged by,
// and the ledger is the evidence an operator audits after this tool has deleted their
// originals.
//
// Three roots, one per terminal status, so the claim is proved on each of them rather
// than on the happy path alone.
func TestTerminalRowCarriesTheDecidingProfile(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	_, roots := twoRoots(t, "done", "skipped", "failed")
	doneSrc := filepath.Join(roots[0], "a.mkv")
	skipSrc := filepath.Join(roots[1], "b.mkv")
	failSrc := filepath.Join(roots[2], "c.mkv")
	for _, p := range []string{doneSrc, skipSrc, failSrc} {
		mkH264(t, ffmpeg, p, "8M")
	}

	cfg := profileCfg(t, `
library_roots:
  - path: `+roots[0]+`
    crf: 30
  - path: `+roots[1]+`
    min_bitrate_kbps: 100000
  - path: `+roots[2]+`
    min_savings_percent: 99
crf: 28
preset: ultrafast
container_ext: mkv
pixel_format: yuv420p10le
vmaf_enable: false
min_bitrate_kbps: 0
`)
	ts, _ := runProfiles(t, ffmpeg, ffprobe, cfg)

	byRoot := map[string]config.Root{}
	for _, r := range cfg.RootProfiles() {
		byRoot[r.Clean] = r
	}
	// The three profiles really are different, or matching digests would prove nothing.
	digests := map[string]bool{}
	for _, r := range byRoot {
		digests[r.Profile.Digest()] = true
	}
	if len(digests) != 3 {
		t.Fatalf("the three roots resolved to %d distinct profiles, want 3 - the fixture cannot "+
			"distinguish them", len(digests))
	}

	for _, tc := range []struct {
		name   string
		path   string
		root   string
		status store.Status
	}{
		{"done", doneSrc, filepath.Clean(roots[0]), store.Done},
		{"skipped", skipSrc, filepath.Clean(roots[1]), store.Skipped},
		{"failed", failSrc, filepath.Clean(roots[2]), store.Failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := rowFor(t, ts, tc.path)
			if row.Status != tc.status {
				t.Fatalf("row is %q (%s), want %q - the fixture did not produce the terminal state "+
					"this case is about", row.Status, row.Outcome.Reason, tc.status)
			}
			if row.Outcome.LibraryRoot != tc.root {
				t.Errorf("library_root = %q, want the cleaned root %q", row.Outcome.LibraryRoot, tc.root)
			}
			want := byRoot[tc.root].Profile.Digest()
			if row.Outcome.ProfileDigest != want {
				t.Errorf("profile_digest = %q, want %q - the row names a root but not what that root "+
					"resolved to, so it stops meaning anything the moment the profile is edited",
					row.Outcome.ProfileDigest, want)
			}
		})
	}

	// A digest that is the same everywhere would satisfy every assertion above while
	// carrying no information at all.
	seen := map[string]string{}
	for _, p := range []string{doneSrc, skipSrc, failSrc} {
		row := rowFor(t, ts, p)
		if other, dup := seen[row.Outcome.ProfileDigest]; dup {
			t.Errorf("%s and %s recorded the same profile_digest %q under roots with different "+
				"profiles", filepath.Base(p), filepath.Base(other), row.Outcome.ProfileDigest)
		}
		seen[row.Outcome.ProfileDigest] = p
	}
}
