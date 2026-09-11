package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// The per-job settings half of S0079, driven end to end through RunOneshot over real
// ffmpeg fixtures - the same discipline the rest of this suite runs under, because a
// criterion about which settings a JOB was decided under is not proved by asking the
// resolver what it would say.

func strp(s string) *string { return &s }
func intp(i int) *int       { return &i }

// outcomeFor returns the recorded Outcome for the file at path, resolved against its
// CURRENT on-disk fingerprint - which for a done row is the post-swap file's, exactly
// as the engine keys it.
func outcomeFor(t *testing.T, ts *testStore, path string) (store.Outcome, store.Status, bool) {
	t.Helper()
	rows, err := ts.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	for _, r := range rows {
		if r.Path == path {
			return r.Outcome, r.Status, true
		}
	}
	return store.Outcome{}, "", false
}

// AC-A6: the FIRST matching profile's overrides are used and a later matching
// profile has no effect; a setting the matching profile does not override keeps its
// top-level value.
//
// The two profiles both match, and they disagree about the encoder - so the codec of
// the file left on disk IS the answer to which one won. The container extension is
// the other half: neither profile mentions it, so the top-level `mkv` must still
// decide, and the source is deliberately an mp4 so that "kept its top-level value"
// is visible in the filename rather than inferred.
func TestProfiles_FirstMatchWinsAndAnUnmentionedSettingKeepsItsTopLevelValue(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	sub := filepath.Join(d, "4K")
	if err := exec.Command("mkdir", "-p", sub).Run(); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(sub, "film.mp4")
	mkH264(t, ffmpeg, src, "8M")

	ts := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		c.ContainerExt = "mkv" // top-level, mentioned by NEITHER profile
		c.EncodeProfiles = []config.EncodeProfile{
			{Name: "first", Match: "**/4K/**", Encoder: strp("svtav1"), Preset: strp("fast"), CRF: intp(30)},
			{Name: "second", Match: "**/4K/**", Encoder: strp("cpu"), CRF: intp(10)},
		}
	})

	final := filepath.Join(sub, "film.mkv")
	if !exists(final) {
		t.Fatalf("no output at %s - the top-level container_ext did not decide the extension", final)
	}
	if exists(src) {
		t.Fatalf("the source %s survived the swap", src)
	}
	if got := codecOf(t, ffprobe, final); got != "av1" {
		t.Fatalf("output codec = %q, want av1 - the FIRST matching profile did not decide the encoder", got)
	}
	out, status, ok := outcomeFor(t, ts, final)
	if !ok {
		t.Fatalf("no ledger row for %s", final)
	}
	if status != store.Done {
		t.Fatalf("status = %q, want done", status)
	}
	if out.Profile != "first" {
		t.Fatalf("the row records profile %q, want %q - a later matching profile decided or nothing did", out.Profile, "first")
	}
	if out.Encoder != "svtav1" {
		t.Fatalf("the row records encoder %q, want svtav1", out.Encoder)
	}
}

// AC-A7: a source NO profile matches is transcoded under the top-level settings -
// not skipped, not failed - and its row records an empty profile.
func TestProfiles_NoMatchTranscodesUnderTheTopLevelSettings(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "film.mkv")
	mkH264(t, ffmpeg, src, "8M")

	ts := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		c.EncodeProfiles = []config.EncodeProfile{
			{Name: "only-4k", Match: "**/4K/**", Encoder: strp("svtav1")},
		}
	})

	if got := codecOf(t, ffprobe, src); got != "hevc" {
		t.Fatalf("codec = %q, want hevc - the unmatched source was not transcoded under the top-level encoder", got)
	}
	out, status, ok := outcomeFor(t, ts, src)
	if !ok {
		t.Fatalf("no ledger row for %s", src)
	}
	if status != store.Done {
		t.Fatalf("status = %q, want done - an unmatched source must be transcoded, not skipped or failed", status)
	}
	if out.Profile != "" {
		t.Fatalf("the row records profile %q, want empty", out.Profile)
	}
}

// AC-A9, first limb: a source ALREADY in the top-level target codec, whose matching
// profile targets a DIFFERENT codec, is transcoded rather than skipped.
//
// The control arm is what makes it an experiment rather than an assertion: the
// identical fixture with no profile IS skipped as already-at-target-codec, so the
// test measures the profile and nothing else.
func TestProfiles_AlreadyAtTheTopLevelCodecButTheProfileTargetsAnother_IsTranscoded(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	t.Run("control: no profile, so it is skipped", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "film.mkv")
		mkHevc(t, ffmpeg, src, "8M")
		before := md5f(t, src)

		ts := run(t, ffmpeg, ffprobe, d, nil, nil)
		if !ledgerHas(t, ts, store.Skipped, "film.mkv") {
			t.Fatalf("an already-hevc source under an hevc target was not skipped")
		}
		if got := skipReason(t, ts, "film.mkv"); got != SkipAlreadyTargetCodec {
			t.Fatalf("skip reason = %q, want %q", got, SkipAlreadyTargetCodec)
		}
		if md5f(t, src) != before {
			t.Fatalf("the skipped source was modified")
		}
	})

	t.Run("a profile targeting av1 transcodes it", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "film.mkv")
		mkHevc(t, ffmpeg, src, "8M")

		ts := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
			c.MinVmaf = 90
			c.EncodeProfiles = []config.EncodeProfile{
				{Name: "to-av1", Match: "*.mkv", Encoder: strp("svtav1"), Preset: strp("fast"), CRF: intp(30)},
			}
		})

		if got := skipReason(t, ts, "film.mkv"); got == SkipAlreadyTargetCodec {
			t.Fatalf("the source was skipped as already-at-target-codec against the RUN's target rather than the JOB's: "+
				"a job whose profile targets av1 has real work to do on an hevc source (row: %q)", got)
		}
		if got := codecOf(t, ffprobe, src); got != "av1" {
			t.Fatalf("codec = %q, want av1", got)
		}
		out, status, ok := outcomeFor(t, ts, src)
		if !ok || status != store.Done {
			t.Fatalf("status = %q (found=%v), want done", status, ok)
		}
		if out.Profile != "to-av1" {
			t.Fatalf("the row records profile %q, want to-av1", out.Profile)
		}
	})
}

// AC-A9, second limb: the output-codec acceptance check is decided against the
// codec THIS JOB's encoder produces.
//
// The encoder is faked so the output's codec is chosen by the test rather than by
// the encoder: it writes a perfectly good, smaller HEVC file for a job whose profile
// targets av1. Every other gate passes it - it decodes cleanly, carries the right
// duration and streams, and is smaller - so the ONLY thing that can reject it is the
// output-codec check, decided against the profile's target.
func TestProfiles_TheOutputCodecCheckIsDecidedAgainstTheJobsOwnTarget(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	t.Run("an hevc output under an av1 profile is rejected", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "film.mkv")
		mkH264(t, ffmpeg, src, "8M")
		before := md5f(t, src)

		writesHevc := EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
			ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", in,
				"-c:v", "libx265", "-x265-params", "log-level=error", "-preset", "ultrafast",
				"-crf", "35", "-pix_fmt", "yuv420p", "--", out)
			return nil
		})

		ts := run(t, ffmpeg, ffprobe, d, writesHevc, func(c *config.Config) {
			c.EncodeProfiles = []config.EncodeProfile{
				{Name: "to-av1", Match: "*.mkv", Encoder: strp("svtav1")},
			}
		})

		out, status, ok := outcomeFor(t, ts, src)
		if !ok || status != store.Failed {
			t.Fatalf("status = %q (found=%v), want failed - an hevc output was accepted for a job targeting av1", status, ok)
		}
		if !strings.Contains(out.Reason, "output codec") || !strings.Contains(out.Reason, "av1") {
			t.Fatalf("the failure reason does not name the codec check against av1: %q", out.Reason)
		}
		if out.Profile != "to-av1" {
			t.Fatalf("the failed row records profile %q, want to-av1", out.Profile)
		}
		if md5f(t, src) != before {
			t.Fatalf("the source was modified by a rejected encode")
		}
		if n := nTemp(t, d); n != 0 {
			t.Fatalf("%d temp file(s) left behind after a rejected encode", n)
		}
	})

	// The mirror image, and it is the arm that stops the check being a blanket
	// "reject anything not hevc": the SAME hevc output is ACCEPTED for a job whose
	// profile targets hevc, in the same run shape.
	t.Run("the same hevc output under an hevc profile is accepted", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "film.mkv")
		mkH264(t, ffmpeg, src, "8M")

		writesHevc := EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
			ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", in,
				"-c:v", "libx265", "-x265-params", "log-level=error", "-preset", "ultrafast",
				"-crf", "35", "-pix_fmt", "yuv420p", "--", out)
			return nil
		})

		ts := run(t, ffmpeg, ffprobe, d, writesHevc, func(c *config.Config) {
			c.Encoder = "svtav1" // the RUN's target is av1
			c.EncodeProfiles = []config.EncodeProfile{
				{Name: "to-hevc", Match: "*.mkv", Encoder: strp("cpu")}, // the JOB's is hevc
			}
		})

		_, status, ok := outcomeFor(t, ts, src)
		if !ok || status != store.Done {
			t.Fatalf("status = %q (found=%v), want done - an hevc output was rejected for a job whose profile targets hevc, "+
				"which means the check read the RUN's target and not the JOB's", status, ok)
		}
		if got := codecOf(t, ffprobe, src); got != "hevc" {
			t.Fatalf("codec = %q, want hevc", got)
		}
	})
}

// AC-A10's engine half: the profile that supplied a job's settings reaches the
// ledger on a SKIP as well as on a swap. A skip is a terminal state, and which
// profile decided it is the same question - a source is skipped as
// already-at-target-codec against its own profile's target codec.
func TestProfiles_ASkippedJobRecordsTheProfileThatDecidedIt(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "film.mkv")
	mkAV1(t, ffmpeg, src, "40")

	ts := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		c.EncodeProfiles = []config.EncodeProfile{
			{Name: "to-av1", Match: "*.mkv", Encoder: strp("svtav1")},
		}
	})

	out, status, ok := outcomeFor(t, ts, src)
	if !ok || status != store.Skipped {
		t.Fatalf("status = %q (found=%v), want skipped", status, ok)
	}
	if out.Reason != SkipAlreadyTargetCodec {
		t.Fatalf("skip reason = %q, want %q", out.Reason, SkipAlreadyTargetCodec)
	}
	if out.Profile != "to-av1" {
		t.Fatalf("the skipped row records profile %q, want to-av1", out.Profile)
	}
}

// AC-A10 on the terminal state a dry run produces: `would-transcode` is a terminal
// row like `done` and `skipped`, so it records which profile supplied the settings
// the decision was taken under, and "" when the top-level ones did.
//
// One run, two sources, so the matched and the unmatched arm are decided by the same
// configuration and the same pass: an implementation that hard-coded either answer
// fails one of them. Nothing is encoded on this path, which is the other half of what
// is asserted here - both files must still be on disk, unchanged, in their original
// container.
func TestProfiles_ADryRunDecisionRecordsTheProfileItWouldHaveUsed(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	sub := filepath.Join(d, "4K")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	matched := filepath.Join(sub, "film.mkv")
	unmatched := filepath.Join(d, "show.mkv")
	mkH264(t, ffmpeg, matched, "8M")
	mkH264(t, ffmpeg, unmatched, "8M")

	ts := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		c.DryRun = true
		c.EncodeProfiles = []config.EncodeProfile{
			{Name: "4k-av1", Match: "**/4K/**", Encoder: strp("svtav1")},
		}
	})

	for _, tc := range []struct{ path, profile string }{
		{matched, "4k-av1"},
		{unmatched, ""},
	} {
		out, status, ok := outcomeFor(t, ts, tc.path)
		if !ok || status != store.WouldTranscode {
			t.Fatalf("%s: status = %q (found=%v), want would-transcode", tc.path, status, ok)
		}
		if out.Profile != tc.profile {
			t.Errorf("%s: the dry-run row records profile %q, want %q", tc.path, out.Profile, tc.profile)
		}
		if !exists(tc.path) || codecOf(t, ffprobe, tc.path) != "h264" {
			t.Errorf("%s: a dry run encoded or replaced the source", tc.path)
		}
	}
}
