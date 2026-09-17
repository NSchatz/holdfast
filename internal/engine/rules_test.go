package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// S0089 - resolution rules where they meet a real file.
//
// A rule is an ordered, first-match override of three knobs inside one library root, and
// what it decides is not a claim the configuration package can settle: whether a file was
// judged in its band is visible only in what the run DID to it - the argv the production
// encoder assembled, the guard that fired or did not, and the values the terminal row
// recorded as read. So every test here drives RunOneshot over real ffmpeg fixtures, the
// discipline the rest of this suite runs under.

// mkH264At writes an H.264 clip at a chosen frame SIZE, which is the whole point of it:
// a rule's band is matched on the source's height in pixels, so a fixture suite that
// could only produce one size could not tell a matching band from an unconditional rule.
func mkH264At(t *testing.T, ffmpeg, path, bitrate, size string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size="+size+":rate=10",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", bitrate, "-pix_fmt", "yuv420p", "--", path)
}

// [AC-2] WHEN more than one rule matches a file, the FIRST matching rule decides it and
// no other, and no value from a later matching rule is merged in.
//
// Both rules match the 480p source (480 <= 720 and 480 <= 1080), and they are built so
// that each of the two wrong answers is VISIBLE in a different place:
//
//   - a later rule WINNING shows up as crf 40 on the argv the real encoder assembled,
//     where first-match puts crf 30;
//   - a later rule MERGING shows up as the low-bitrate guard firing, because rule 2
//     carries a 50 Mbps floor this 2 Mbps source cannot clear, where first-match leaves
//     the floor at the root's own 0 and the file proceeds to the encoder.
//
// The second is the one a merging implementation passes by accident: a merge that took
// only the knobs the first rule did not name would still encode at crf 30 and look
// entirely correct on the argv alone.
//
// MUTATION: return the LAST matching rule instead of the first and the crf assertions
// red; merge the later rule's unnamed knobs over the first and the file skips
// `low-bitrate` instead of reaching the encoder.
func TestRules_FirstMatchWinsAndDoesNotMerge(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	_, roots := twoRoots(t, "tv")
	src := filepath.Join(roots[0], "ep.mkv")
	mkH264At(t, ffmpeg, src, "2M", "854x480")

	cfg := profileCfg(t, `
library_roots:
  - path: `+roots[0]+`
    crf: 22
    preset: ultrafast
    min_bitrate_kbps: 0
    rules:
      - when:
          max_source_height: 720
        crf: 30
      - when:
          max_source_height: 1080
        crf: 40
        min_bitrate_kbps: 50000
encoder: cpu
vmaf_enable: false
`)

	ts, log := runProfiles(t, ffmpeg, ffprobe, cfg)

	out, status, found := outcomeFor(t, ts, src)
	if found && out.Reason == SkipLowBitrate {
		t.Fatalf("the file skipped %q: the SECOND matching rule's 50000 kbps floor was merged in, "+
			"where first-match-no-merge leaves the floor at the root's own 0", out.Reason)
	}

	args := log.forSource(t, src)
	if !hasArgPair(args, "-crf", "30") {
		t.Errorf("the encode was not built at the FIRST matching rule's crf 30: %v", args)
	}
	if hasArgPair(args, "-crf", "40") {
		t.Errorf("the encode was built at the SECOND matching rule's crf 40, so a later matching "+
			"rule decided the file: %v", args)
	}
	if hasArgPair(args, "-crf", "22") {
		t.Errorf("the encode was built at the ROOT's crf 22, so no rule decided the file at all: %v", args)
	}

	// And the row records what the decision read: the crf the guard chain and the encoder
	// were actually decided under, never the root's.
	if !found {
		t.Fatalf("no terminal row was written for %s", src)
	}
	if status != store.Done {
		t.Logf("the job is %q (%s) rather than done; the criterion is which rule decided it, "+
			"and that is graded on the argv above", status, out.Reason)
	}
	if v, ok := out.DecisionInputs.Value(InputCRF); ok && v != "30" {
		t.Errorf("the row records crf=%q as read, want 30 - the first matching rule's value", v)
	}
}

// [AC-6] WHEN a source whose bitrate is below the root profile's min_bitrate_kbps is
// matched by a rule carrying a LOWER floor that the source clears, no low-bitrate skip is
// recorded and the file proceeds.
//
// This is the item's headline case: one min_bitrate_kbps for a 480p DVD rip and a 2160p
// remux alike either excludes most of the library or lets SD files through. The root sets
// a 50 Mbps floor - an 8K-remux library's floor - and a 2 Mbps 480p source cannot clear
// it; the band-specific 100 kbps floor is what offers it to the pipeline.
//
// The 1080p fixture is the anti-vacuity arm and it is not optional: a build that simply
// ignored the root floor whenever any rule existed would pass every assertion about the
// 480p file. It is the same 2 Mbps, under the same root, outside the rule's band, and it
// must still skip `low-bitrate` at the root's floor.
//
// MUTATION: apply the rule to every file regardless of its band and the 1080p arm reds;
// read the floor off root.Profile rather than off the file's effective profile and the
// 480p arm reds with a `low-bitrate` skip.
func TestRules_ABandOverridesTheProfileBitrateFloor(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	_, roots := twoRoots(t, "tv")
	sd := filepath.Join(roots[0], "dvd-rip.mkv")
	hd := filepath.Join(roots[0], "bluray-rip.mkv")
	mkH264At(t, ffmpeg, sd, "2M", "854x480")
	mkH264At(t, ffmpeg, hd, "2M", "1920x1080")

	cfg := profileCfg(t, `
library_roots:
  - path: `+roots[0]+`
    preset: ultrafast
    min_bitrate_kbps: 50000
    rules:
      - when:
          max_source_height: 576
        min_bitrate_kbps: 100
encoder: cpu
vmaf_enable: false
`)

	ts, log := runProfiles(t, ffmpeg, ffprobe, cfg)

	sdOut, _, sdFound := outcomeFor(t, ts, sd)
	if sdFound && sdOut.Reason == SkipLowBitrate {
		t.Errorf("the 480p source skipped %q: it was judged against the root's 50000 kbps floor "+
			"rather than against the 100 kbps floor its band supplies", sdOut.Reason)
	}
	// Proceeding is the criterion, and reaching the ENCODER is what proves it: the job may
	// still be rejected later on size or fidelity, which is a different question.
	if _, ok := log.byIn[sd]; !ok {
		t.Errorf("no encode was built for the 480p source, so it did not proceed past the guards "+
			"(row: reason=%q found=%v)", sdOut.Reason, sdFound)
	}

	hdOut, hdStatus, hdFound := outcomeFor(t, ts, hd)
	if !hdFound {
		t.Fatalf("no terminal row was written for the 1080p source")
	}
	if hdStatus != store.Skipped || hdOut.Reason != SkipLowBitrate {
		t.Errorf("the 1080p source is %q/%q, want skipped/%s - it lies OUTSIDE the rule's band, "+
			"so the root's own floor still decides it", hdStatus, hdOut.Reason, SkipLowBitrate)
	}
	// And the row records the floor it was actually compared against, which for this file
	// is the root's: editing the root floor offers it back, editing the rule does not.
	if v, ok := hdOut.DecisionInputs.Value(InputMinBitrateKbps); !ok || v != "50000" {
		t.Errorf("the 1080p row records min_bitrate_kbps=%q (recorded=%v), want 50000", v, ok)
	}
}

// [AC-3] WHEN a rule matches, each knob the rule NAMES comes from the rule and every knob
// it does not name comes from the root's resolved profile.
//
// All three rule-overridable knobs are exercised in one pass, because the failure this
// guards against is per-knob: a resolver that threaded crf and forgot min_savings_percent
// produces a perfectly good encode and a silently different acceptance bar.
//
// The rule names crf and min_savings_percent and says nothing about min_bitrate_kbps, so
// the third is the unnamed arm: it must still be the root's 100, which this 2 Mbps source
// clears.
//
// MUTATION: seed the effective profile from the top level rather than from the root's own
// and the preset assertion reds (the root's ultrafast becomes the top level's veryslow).
func TestRules_ANamedKnobComesFromTheRuleAndAnUnnamedOneFromTheRoot(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	_, roots := twoRoots(t, "tv")
	src := filepath.Join(roots[0], "ep.mkv")
	mkH264At(t, ffmpeg, src, "2M", "854x480")

	cfg := profileCfg(t, `
library_roots:
  - path: `+roots[0]+`
    crf: 22
    preset: ultrafast
    min_bitrate_kbps: 100
    min_savings_percent: 0
    rules:
      - when:
          max_source_height: 576
        crf: 31
        min_savings_percent: 90
encoder: cpu
preset: veryslow
vmaf_enable: false
`)

	ts, log := runProfiles(t, ffmpeg, ffprobe, cfg)
	args := log.forSource(t, src)

	// Named: the rule's crf reached the encoder.
	if !hasArgPair(args, "-crf", "31") {
		t.Errorf("the encode was not built at the rule's crf 31: %v", args)
	}
	// Unnamed: the preset is the ROOT's, never the top level's.
	if !hasArgPair(args, "-preset", "ultrafast") {
		t.Errorf("the encode was not built at the root's preset: %v", args)
	}
	// Unnamed: the root's own 100 kbps floor still applied, so the file was not skipped.
	out, _, found := outcomeFor(t, ts, src)
	if found && out.Reason == SkipLowBitrate {
		t.Errorf("the file skipped %q, but the rule names no floor and the root's is 100 kbps", out.Reason)
	}
	// Named: the rule's savings floor is the bar the output was held to. 90% is a bar this
	// encode cannot clear, so the job must be REJECTED on size - which is the only place
	// that knob is observable at all.
	if !found {
		t.Fatalf("no terminal row was written for %s", src)
	}
	if out.Reason == "" || !strings.Contains(out.Reason, "min_savings=90%") {
		t.Errorf("the row's reason is %q: the rule's min_savings_percent 90 was not the bar the "+
			"output was held to", out.Reason)
	}
}

// [AC-4] WHEN a file matches no rule, or its root carries no `rules`, it is decided from
// the root's resolved profile exactly as a build without rules decides it.
//
// Two roots, one fixture each, and the SAME configuration would have produced these two
// rows before rules existed. It is the regression arm of the whole item: every other test
// here proves a rule changed something, and this one proves nothing changed where no rule
// applies.
//
// MUTATION: apply the first rule whatever the band and the banded root's file is encoded
// at crf 33 rather than at its root's 28.
func TestRules_AFileMatchingNoRuleIsDecidedByItsRootsProfile(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	_, roots := twoRoots(t, "banded", "plain")
	banded := filepath.Join(roots[0], "ep.mkv")
	plain := filepath.Join(roots[1], "film.mkv")
	mkH264At(t, ffmpeg, banded, "2M", "854x480")
	mkH264At(t, ffmpeg, plain, "2M", "854x480")

	cfg := profileCfg(t, `
library_roots:
  - path: `+roots[0]+`
    crf: 28
    preset: ultrafast
    min_bitrate_kbps: 100
    rules:
      - when:
          min_source_height: 2160
        crf: 33
  - path: `+roots[1]+`
    crf: 29
    preset: ultrafast
    min_bitrate_kbps: 100
encoder: cpu
vmaf_enable: false
`)

	_, log := runProfiles(t, ffmpeg, ffprobe, cfg)

	// Matched no rule: the banded root's own crf decided it.
	bandedArgs := log.forSource(t, banded)
	if !hasArgPair(bandedArgs, "-crf", "28") {
		t.Errorf("the 480p file under the banded root was not encoded at its root's crf 28: %v", bandedArgs)
	}
	if hasArgPair(bandedArgs, "-crf", "33") {
		t.Errorf("the 480p file was encoded at the crf of a rule whose band starts at 2160: %v", bandedArgs)
	}
	// No rules at all: unchanged, which is what every configuration written before this
	// item must still get.
	plainArgs := log.forSource(t, plain)
	if !hasArgPair(plainArgs, "-crf", "29") {
		t.Errorf("the file under the root with no rules was not encoded at its root's crf 29: %v", plainArgs)
	}
}

// [AC-5] WHEN a rule carries `when`, matching is on the SOURCE height in pixels against
// min_source_height and max_source_height, INCLUSIVE at both ends; an absent bound leaves
// that side unbounded and an absent `when` matches every file.
//
// The inclusivity is graded ON THE BOUNDARY and from both sides, because an off-by-one is
// exactly the defect a fixture at the middle of a band cannot see: 720 is both the max of
// the first band and the min of the second, and only one of them may claim it.
//
// The four fixtures are the four readings: below the boundary, ON it, above it, and a
// file no bounded rule reaches at all, which the trailing unconditional rule must take.
//
// MUTATION: make either comparison exclusive and the 720p file moves out of the first
// band; drop the unbounded-side handling and the 2160p file stops reaching rule 3.
func TestRules_BandBoundsAreInclusiveAndAnAbsentBoundIsUnbounded(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	_, roots := twoRoots(t, "tv")
	below := filepath.Join(roots[0], "a-576.mkv")
	onBound := filepath.Join(roots[0], "b-720.mkv")
	above := filepath.Join(roots[0], "c-1080.mkv")
	unbounded := filepath.Join(roots[0], "d-2160.mkv")
	mkH264At(t, ffmpeg, below, "2M", "1024x576")
	mkH264At(t, ffmpeg, onBound, "2M", "1280x720")
	mkH264At(t, ffmpeg, above, "2M", "1920x1080")
	mkH264At(t, ffmpeg, unbounded, "2M", "3840x2160")

	cfg := profileCfg(t, `
library_roots:
  - path: `+roots[0]+`
    crf: 20
    preset: ultrafast
    min_bitrate_kbps: 100
    rules:
      - when:
          max_source_height: 720
        crf: 31
      - when:
          min_source_height: 721
          max_source_height: 1080
        crf: 32
      - crf: 33
encoder: cpu
vmaf_enable: false
`)

	_, log := runProfiles(t, ffmpeg, ffprobe, cfg)

	for _, tc := range []struct {
		path, crf, why string
	}{
		{below, "31", "576 is under the first band's max, so the first rule takes it"},
		{onBound, "31", "720 IS the first band's max and the bound is inclusive"},
		{above, "32", "1080 IS the second band's max and its min is 721, both inclusive"},
		{unbounded, "33", "no bounded rule reaches 2160, so the rule with no `when` takes it"},
	} {
		args := log.forSource(t, tc.path)
		if !hasArgPair(args, "-crf", tc.crf) {
			t.Errorf("%s: not encoded at crf %s (%s): %v", filepath.Base(tc.path), tc.crf, tc.why, args)
		}
	}
}

// [AC-7] WHEN a terminal row is written by a guard whose threshold a RULE supplied, the
// row records the EFFECTIVE value the guard compared against and never the root
// profile's.
//
// The point of the field is re-derivation: a recorded value is what the next scan's claim
// is measured against, so a row recording the root's 50000 while the guard compared 3000
// would be re-opened on every scan for ever - and a row recording the root's value would
// also tell an operator that editing the root offers the file back, when only editing the
// rule does.
//
// So it is graded BOTH ways in one pass: the value on the row, and what actually happens
// to the file when the rule that supplied it is edited. The second run raises the band's
// floor above the source, and the row must move from done-or-encoded to a low-bitrate
// skip - which it can only do if the claim re-derived the file through the rule.
//
// MUTATION: record prof.MinBitrateKbps off root.Profile and the first assertion reds at
// 50000; resolve the claim's inputs without the rules and the second run leaves the row
// exactly where the first run put it.
func TestRules_ATerminalRowRecordsTheEffectiveThresholdTheGuardCompared(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	_, roots := twoRoots(t, "tv")
	src := filepath.Join(roots[0], "ep.mkv")
	mkH264At(t, ffmpeg, src, "2M", "854x480")

	// The band's floor is 3000 kbps and this 2 Mbps source does not clear it, so the guard
	// that fires is the RULE's - which is what puts a rule-supplied threshold on the row.
	cfg := profileCfg(t, `
library_roots:
  - path: `+roots[0]+`
    preset: ultrafast
    min_bitrate_kbps: 50000
    rules:
      - when:
          max_source_height: 576
        min_bitrate_kbps: 3000
encoder: cpu
vmaf_enable: false
`)

	ts, _ := runProfiles(t, ffmpeg, ffprobe, cfg)
	out, status, found := outcomeFor(t, ts, src)
	if !found || status != store.Skipped || out.Reason != SkipLowBitrate {
		t.Fatalf("the job is %q/%q (found=%v), want skipped/%s", status, out.Reason, found, SkipLowBitrate)
	}
	v, ok := out.DecisionInputs.Value(InputMinBitrateKbps)
	if !ok {
		t.Fatalf("the row recorded no min_bitrate_kbps at all, so nothing can re-derive it")
	}
	if v != "3000" {
		t.Errorf("the row records min_bitrate_kbps=%q, want 3000 - the EFFECTIVE floor the guard "+
			"compared against, never the root's 50000", v)
	}

	// The other half: editing the RULE offers the file to the pipeline again. The floor
	// the band supplies drops below the source, and the same ledger must now let the file
	// through rather than holding it on the skip the first run recorded.
	relaxed := profileCfg(t, `
library_roots:
  - path: `+roots[0]+`
    preset: ultrafast
    min_bitrate_kbps: 50000
    rules:
      - when:
          max_source_height: 576
        min_bitrate_kbps: 100
encoder: cpu
vmaf_enable: false
`)
	log := newArgvLog()
	prober := probe.New(ffmpeg, ffprobe)
	enc := FFmpegEncoder{FFmpeg: ffmpeg, Cfg: relaxed, Probe: prober, argvObserver: log.record}
	if err := New(relaxed, prober, enc, ts, discardLogger()).RunOneshot(context.Background()); err != nil {
		t.Fatalf("second RunOneshot: %v", err)
	}
	if _, ok := log.byIn[src]; !ok {
		after, _, _ := outcomeFor(t, ts, src)
		t.Errorf("editing the rule did not offer the file to the pipeline again: the row still "+
			"reads %q, and no encode was built", after.Reason)
	}
}

// [AC-16] IF a file's source height cannot be determined AND its root carries at least
// one rule with a `when`, the file is decided under NO rule and under no profile: a
// terminal skip naming the undetermined source height is recorded, warned about with the
// file path, and recorded so that removing every `when`-carrying rule offers the file
// back.
//
// This is the fail-safe rule applied to a band: a height nobody could read would
// otherwise fall into whichever band the zero value lands in, and on a tool that deletes
// sources the wrong band is a file re-encoded against a threshold its operator set for
// something else.
//
// The fixture is a file with a video EXTENSION and no video in it, which is the shape a
// truncated download and a mis-named file both take. The second run is the re-derivation
// arm: with every `when` removed the root needs no height at all, and the file must be
// offered back rather than held on a skip nothing can move.
//
// MUTATION: default an unreadable height to 0 and the file is decided under whichever
// band admits 0; record no inputs on the skip and the second run leaves the row where it
// was (a record of nothing is re-opened once, so this arm reds only if the row then
// settles, which is why the assertion is on the reason and not on the row's existence).
func TestRules_AnUndeterminedSourceHeightUnderABandedRootSkips(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	_, roots := twoRoots(t, "tv")
	src := filepath.Join(roots[0], "truncated.mkv")
	if err := os.WriteFile(src, []byte("this is not a matroska file"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := profileCfg(t, `
library_roots:
  - path: `+roots[0]+`
    preset: ultrafast
    min_bitrate_kbps: 0
    rules:
      - when:
          max_source_height: 576
        crf: 31
encoder: cpu
vmaf_enable: false
`)

	ts, _ := runProfiles(t, ffmpeg, ffprobe, cfg)
	out, status, found := outcomeFor(t, ts, src)
	if !found {
		t.Fatalf("no terminal row was written for a source whose height could not be determined")
	}
	if status != store.Skipped || out.Reason != SkipUndeterminedSourceHeight {
		t.Fatalf("the job is %q/%q, want skipped/%s", status, out.Reason, SkipUndeterminedSourceHeight)
	}
	if !out.DecisionInputs.Recorded() {
		t.Errorf("the skip recorded no decision inputs, so no configuration change can ever offer " +
			"the file back")
	}
	if v, ok := out.DecisionInputs.Value(InputRules); !ok || v == "" {
		t.Errorf("the skip records rules=%q (recorded=%v): the rules are what it read, and the "+
			"record is what removing them re-derives", v, ok)
	}

	// Remove every `when`: the root no longer needs a height, so the file must be offered
	// to the pipeline again rather than held on a verdict nothing can move.
	unbanded := profileCfg(t, `
library_roots:
  - path: `+roots[0]+`
    preset: ultrafast
    min_bitrate_kbps: 0
encoder: cpu
vmaf_enable: false
`)
	prober := probe.New(ffmpeg, ffprobe)
	enc := FFmpegEncoder{FFmpeg: ffmpeg, Cfg: unbanded, Probe: prober}
	if err := New(unbanded, prober, enc, ts, discardLogger()).RunOneshot(context.Background()); err != nil {
		t.Fatalf("second RunOneshot: %v", err)
	}
	after, _, ok := outcomeFor(t, ts, src)
	if !ok {
		t.Fatalf("the row for %s disappeared", src)
	}
	if after.Reason == SkipUndeterminedSourceHeight {
		t.Errorf("removing every `when`-carrying rule left the row on %q: the skip was not "+
			"re-derivable from the configuration that produced it", after.Reason)
	}
	// What it becomes is the ordinary answer for a file with no video stream, which is the
	// point: the height refusal stood in front of the guard chain, and now it does not.
	if after.Reason != FailUnreadable {
		t.Errorf("the re-opened row reads %q, want %s - the verdict a file with no video stream "+
			"gets under a root that needs no height", after.Reason, FailUnreadable)
	}
}

// [AC-17] WHEN a terminal row is written, the SOURCE width and height in pixels are
// recorded, and the OUTPUT width and height when an output was produced and measured.
// Each unrecorded value is NULL and never 0.
//
// Graded on the rows a real pass wrote, across the two shapes that differ: a file that
// reached the encoder (four dimensions) and a file a guard stopped before any output
// existed (two dimensions and two nulls). The null is the load-bearing half - 0 is a legal
// pixel dimension for nothing, and a fabricated 0 would claim a measurement nobody took.
//
// MUTATION: record 0 instead of nil for an output nobody produced and the skipped row's
// assertion reds.
func TestRules_ATerminalRowRecordsTheSourceAndOutputResolution(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	_, roots := twoRoots(t, "tv")
	encoded := filepath.Join(roots[0], "encoded.mkv")
	skipped := filepath.Join(roots[0], "skipped.mkv")
	mkH264At(t, ffmpeg, encoded, "2M", "854x480")
	mkH264At(t, ffmpeg, skipped, "2M", "1920x1080")

	cfg := profileCfg(t, `
library_roots:
  - path: `+roots[0]+`
    preset: ultrafast
    min_bitrate_kbps: 50000
    rules:
      - when:
          max_source_height: 576
        min_bitrate_kbps: 100
encoder: cpu
vmaf_enable: false
`)

	ts, _ := runProfiles(t, ffmpeg, ffprobe, cfg)

	enc, _, found := outcomeFor(t, ts, encoded)
	if !found {
		t.Fatalf("no terminal row was written for %s", encoded)
	}
	if enc.SourceWidth == nil || enc.SourceHeight == nil || *enc.SourceWidth != 854 || *enc.SourceHeight != 480 {
		t.Errorf("the encoded row records source %v x %v, want 854 x 480",
			dim(enc.SourceWidth), dim(enc.SourceHeight))
	}
	if enc.OutputWidth == nil || enc.OutputHeight == nil || *enc.OutputWidth != 854 || *enc.OutputHeight != 480 {
		t.Errorf("the encoded row records output %v x %v, want 854 x 480 - this item downscales "+
			"nothing, so the output carries the source's dimensions",
			dim(enc.OutputWidth), dim(enc.OutputHeight))
	}

	skp, _, found := outcomeFor(t, ts, skipped)
	if !found {
		t.Fatalf("no terminal row was written for %s", skipped)
	}
	if skp.SourceWidth == nil || skp.SourceHeight == nil || *skp.SourceWidth != 1920 || *skp.SourceHeight != 1080 {
		t.Errorf("the skipped row records source %v x %v, want 1920 x 1080",
			dim(skp.SourceWidth), dim(skp.SourceHeight))
	}
	if skp.OutputWidth != nil || skp.OutputHeight != nil {
		t.Errorf("the skipped row records an output resolution of %v x %v: no output was ever "+
			"produced, and a recorded dimension would be a measurement nobody took",
			dim(skp.OutputWidth), dim(skp.OutputHeight))
	}
}

// dim renders an optional pixel dimension for a failure message: the number, or the words
// a nil carries. Handing a *int to a format verb would print the pointer's address, which
// is exactly the reading these assertions exist to keep honest.
func dim(p *int) any {
	if p == nil {
		return "not recorded"
	}
	return *p
}

// Anti-vacuity for the whole file: the configuration above really does resolve rules onto
// the root, in the order they were written. A resolver that dropped them silently would
// leave every end-to-end test here grading a build with no rules at all - each of them
// would still red, but on the wrong evidence, and this says so in one line.
func TestRules_AreResolvedOntoTheRootInWrittenOrder(t *testing.T) {
	_, roots := twoRoots(t, "tv")
	cfg := profileCfg(t, `
library_roots:
  - path: `+roots[0]+`
    rules:
      - when:
          max_source_height: 576
        crf: 31
      - when:
          min_source_height: 2160
        crf: 18
encoder: cpu
`)
	root, ok := cfg.RootFor(filepath.Join(roots[0], "x.mkv"))
	if !ok {
		t.Fatalf("no root resolved for a path under %s", roots[0])
	}
	rules := root.Profile.Rules
	if len(rules) != 2 {
		t.Fatalf("the root resolved %d rule(s), want 2", len(rules))
	}
	if rules[0].CRF == nil || *rules[0].CRF != 31 || rules[1].CRF == nil || *rules[1].CRF != 18 {
		t.Errorf("the rules did not resolve in written order: %v", rules)
	}
	var _ config.Rules = rules
}
