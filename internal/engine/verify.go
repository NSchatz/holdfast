package engine

import (
	"context"
	"fmt"
	"math"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/deinterlace"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/vmaf"
)

// vmafProof is what the VMAF gate MEASURED - carried out of verifyOutput rather than
// discarded, so the terminal ledger row can keep it (TRANSCODE-13). Its zero value
// means the gate did not run (VMAF disabled), which is why the scores are pointers:
// nil is "not measured", and 0.0 is a real, terrible score. Collapsing the two is
// exactly how a store ends up displaying a fabricated fidelity number. Every other field
// is a FACT ABOUT THE MEASUREMENT held to the same discipline: nobody reading a stored
// 98.4 without them can say which pixels were compared or whether the colour survived.
type vmafProof struct {
	Mean  *float64
	Min   *float64
	Model string

	// PixFmt is the pixel format BOTH streams were converted to before scoring, named by
	// holdfast rather than negotiated by libavfilter. "" means no measurement.
	PixFmt string
	// Stream names WHICH video stream of each file was compared, in the specifier
	// vocabulary the probes use (vmaf.ScoredStream). "" means no measurement, and is
	// never filled in with a default: a job whose gate never ran scored no stream.
	Stream string
	// ChromaMin is the worst (sub)sampled frame's chroma PSNR in dB, and ChromaMetric
	// names what that number is. nil/"" means no measurement, never a zero: 0.0 dB is
	// an obliterated plane, the most important thing this field could ever report.
	ChromaMin    *float64
	ChromaMetric string

	// Deinterlace is the filter expression the REFERENCE was produced from the source by,
	// and "" where the reference was the source as it is - which is every job that applied
	// no deinterlace, and every job whose gate did not run.
	//
	// It is recorded for the reason PixFmt is: it is part of what was measured. A
	// deinterlaced encode scored against its interlaced source scores like a damaged
	// encode, so a number with no reference named beside it cannot be read at all - and
	// this row outlives the source it describes.
	Deinterlace string

	// Skipped names WHY the gate did not run on a job that reached it, and is "" on every
	// job whose gate ran. It is a stable token (see VmafSkippedRemuxOnly).
	//
	// A proof carrying it carries no figure at all - every field above is nil or "" -
	// which is the distinction it exists for: "the gate did not run, and here is why" is a
	// statement a reader can act on, where a zeroed score would be a fabricated
	// measurement of a gate nobody ran.
	Skipped string
}

// verifyOutput checks a freshly-encoded temp before it may replace the source, and is the
// heart of the no-loss contract: the source is replaced only when this returns nil. Gates run
// cheap-to-expensive, so the full decode and VMAF (TRANSCODE-4) come last. It returns the VMAF
// proof, the GATE that refused and the CLASS of the rejection beside the pass/fail error, the
// proof on the REJECT paths too: a rejection whose score is thrown away re-commits the defect
// TRANSCODE-13 fixes.
//
// # The gate, and why it is produced HERE
//
// The gate is WHICH check refused, in the closed vocabulary the Gate* constants declare, and
// it is chosen at the line that returns the rejection for the same reason the class is: a
// consumer deriving it afterwards from the message would be matching on unbounded text that
// is reworded whenever the message is improved. It is "" beside a nil error - a job nothing
// refused was refused by no gate - and it changes nothing that is stored or decided: it
// travels out on the Event for the surfaces that count failures per gate.
//
// prof is the profile of the root the source was enumerated under: every threshold comes from
// it, and targetCodec is what its own `encoder` resolves to, so a film library and a
// grainy-anime library each meet the bar their operator set. WHAT the gates check never moves.
//
// # The class, and why it is produced HERE
//
// Most of what this function refuses is a pure function of three things that do not move
// between attempts: the source bytes, the configuration, and the pinned ffmpeg build. An
// output that came out bigger comes out bigger again, and retrying it buys a full encode, hours
// at preset slow, for a verdict that was final the first time - so each rejection says which
// kind it is and the recording site parks the final ones at once (see ProcessFile, store.Finish).
// The class travels WITH the verdict rather than being re-derived from the error text: a
// classifier matching on strings drifts silently, and a file that should park retries for ever.
//
// The class is meaningful ONLY beside a non-nil error. Every rejection this function does
// NOT classify explicitly is transient, the fail-safe direction: an unrecognised rejection
// costs CPU, where a wrongly-final one costs a file nobody revisits.
//
// targetCodec is what THIS JOB's effective encoder produces, passed in rather than derived
// from prof: a run can carry more than one target, and a check made against any other job's
// would reject an output that is exactly what this one's own settings asked for.
// plan is THIS JOB's intended stream map - the SAME value the encode's argv was built
// from, handed in rather than derived here. Gate 5 is checked against it, and that is the
// whole of why it is a parameter: a gate that derived its own map would be answering a
// different question than the encoder was asked, and the two answers would differ on
// exactly the file nobody tested.
// film is the deinterlace THIS JOB applied, resolved once by the engine and handed in for
// the same reason plan is: the perceptual gate has to build its reference through the filter
// the encoder actually ran, and a gate that re-derived its own would be a second answer to
// what the output should be compared against. A disabled filter is the ordinary case and
// leaves every gate below exactly as it was.
func (e *Engine) verifyOutput(ctx context.Context, in, tmp string, prof config.Profile, targetCodec string,
	plan *StreamPlan, film deinterlace.Filter) (vmafProof, string, store.FailureClass, error) {
	var none vmafProof

	// THE SEAM, announced before any gate runs: this is the map the checks below read.
	// The encode announced the map its argv was built from at its own seam, so a reader
	// holding both can ask whether they were ONE derivation (see Engine.planObserver).
	e.observePlan(planStageVerify, plan)

	// 1. exists & non-empty. TRANSIENT: an empty temp is what a full disk, a killed ffmpeg
	// or a write that never landed leaves behind, none of them properties of the source.
	if probe.FileSize(tmp) <= 0 {
		return none, GateEncode, store.FailureTransient, fmt.Errorf("temp missing or empty")
	}

	// 2. output codec must be the codec THIS JOB produces, so a hardware or AV1 encode is
	// held to the same bar as CPU libx265 and two encoders in one run are each held to
	// their own. DETERMINISTIC: the job's encoder produces the codec it produces.
	//
	// A REMUX produces the codec it COPIED, which is the source's - the gate is unchanged,
	// it is the expectation that follows what the job was asked to do. Holding a remux to
	// the configured target codec would reject every remux there is, and lowering the gate
	// to "whatever came out" would accept anything; the source's own codec is the only
	// expectation that is neither.
	wantCodec := targetCodec
	if plan.RemuxOnly() {
		wantCodec = plan.SourceVideoCodec()
	}
	if oc := e.Probe.VideoCodec(ctx, tmp); oc != wantCodec {
		return none, GateCodec, store.FailureDeterministic, fmt.Errorf("output codec is %q, not %s", oc, wantCodec)
	}

	// 3. length: the encode must not be truncated. lengthParity classifies its own two
	// rejections (both deterministic).
	if class, err := e.lengthParity(ctx, in, tmp); err != nil {
		return none, GateLength, class, err
	}

	// 4. size: reclaiming space is the whole point. DETERMINISTIC, and the headline case:
	// a source this configuration cannot beat produces the identical arithmetic every time.
	sin := probe.FileSize(in)
	sout := probe.FileSize(tmp)
	limit := float64(sin) * (1 - float64(prof.MinSavingsPercent)/100.0)
	if !(sout > 0 && float64(sout) <= limit && sout < sin) {
		return none, GateSize, store.FailureDeterministic, fmt.Errorf("size-increase reject (in=%dB out=%dB min_savings=%d%%)", sin, sout, prof.MinSavingsPercent)
	}

	// 5. THE INTENDED-STREAM CHECK: the output carries exactly the streams this job meant
	// it to carry, and nothing else. Size, duration and a clean decode can all pass while a
	// track was silently lost, and video is in the check with the most at stake - a second
	// angle or a cover picture can come out one stream short with every other check green.
	//
	// It REPLACES a per-type count that merely had not to fall below the source's, and it
	// is strictly stronger in both directions. A count passes an output that carries a
	// DIFFERENT stream of the same type, and it passes a selection that did not apply at
	// all; this rejects the first because the map names the language of each stream it
	// intends, and the second because an unintended stream present is a rejection here
	// where a count would have read it as "not fewer than the source".
	//
	// An output whose streams cannot be enumerated is REJECTED - AC-9's output half, and
	// the same posture the source half takes: an unknown shape is never read as the common
	// one, and the source is kept. DETERMINISTIC.
	outStreams, enumerated := e.Probe.Streams(ctx, tmp)
	if !enumerated {
		return none, GateStreamParity, store.FailureDeterministic, fmt.Errorf(
			"the output's streams could not be enumerated (ffprobe did not answer): refusing to "+
				"accept an output whose stream map cannot be checked against the %d stream(s) this job "+
				"intended to carry", len(plan.Intended()))
	}
	if err := plan.CheckOutput(outStreams); err != nil {
		return none, GateStreamParity, store.FailureDeterministic, err
	}

	// 6. decode-integrity healthcheck on EVERY encode. TRANSIENT: an output that does not
	// fully decode is a damaged FILE, and a filled disk, a killed process or a flipped bit
	// are conditions of the run rather than properties of the source.
	if !e.Probe.DecodeOK(ctx, tmp) {
		return none, GateDecode, store.FailureTransient, fmt.Errorf("decode-integrity check failed (output does not fully decode)")
	}

	// 7. VMAF perceptual-quality gate, last because it costs a second full decode. The
	// structural checks prove the output exists, decodes and carries the tracks; VMAF proves
	// it still LOOKS like the source. Unavailable libvmaf or a failed measurement REJECTS.
	//
	// A REMUX declines it, and pays for the skip with a STRONGER check rather than with
	// nothing. The ordering below is the whole of that bargain and is not an implementation
	// detail: identity is established FIRST, the output is rejected when it cannot be, and
	// only then is the gate skipped. An implementation that skipped first and checked
	// loosely afterwards would have removed a gate.
	if plan.RemuxOnly() {
		if err := e.videoIdentity(ctx, in, tmp); err != nil {
			// No member of the gate vocabulary names the remux identity check: it stands in
			// place of the perceptual gate on this path, and it is none of the three floors,
			// none of the structural gates, and not an unmeasured VMAF. So it is attributed
			// to the FALLBACK rather than to a gate it is not - which is what the fallback is
			// for - and the row's Reason still names the stream that differed.
			return none, GateOther, store.FailureDeterministic, err
		}
		// Nothing was measured, so NOTHING is reported: an empty proof carrying only the
		// reason the gate did not run. A zeroed score here would be a fabricated
		// measurement of a gate nobody ran, on the row an operator reads after the source
		// is gone.
		return vmafProof{Skipped: VmafSkippedRemuxOnly}, "", "", nil
	}
	if prof.VmafGate() {
		return e.vmafGate(ctx, tmp, in, prof, film)
	}
	return none, "", "", nil
}

// videoIdentity establishes that every video stream the output carries is IDENTICAL to
// the stream it came from. It is what pays for the skipped perceptual gate on a remux.
//
// Skipping a gate is only safe when the property that gate proved is established another
// way. VMAF proves an encode still looks like its source; bit-identity of the copied
// stream is strictly stronger than any perceptual score, and it is the property a stream
// copy is supposed to have. So this asks for it directly rather than assuming the copy
// copied.
//
// Every way of NOT establishing it rejects. An unreadable source, an unreadable output, a
// different number of video streams, one hash that differs - each leaves the output
// unproved, and an unproved output must not replace a file this tool is about to delete.
// The rejection names which stream differed, because "not identical" without it sends an
// operator to a diff they cannot run.
func (e *Engine) videoIdentity(ctx context.Context, in, out string) error {
	want, okIn := e.Probe.VideoStreamHashes(ctx, in)
	if !okIn {
		return fmt.Errorf("remux-only: the SOURCE's video streams could not be hashed, so this " +
			"output cannot be established as an identical copy - and the perceptual gate is skipped " +
			"on this path, so there is nothing else standing here. The source is kept")
	}
	got, okOut := e.Probe.VideoStreamHashes(ctx, out)
	if !okOut {
		return fmt.Errorf("remux-only: the OUTPUT's video streams could not be hashed, so it cannot " +
			"be established as an identical copy of the source - and the perceptual gate is skipped " +
			"on this path, so there is nothing else standing here. The source is kept")
	}
	if len(want) != len(got) {
		return fmt.Errorf("remux-only: the output carries %d video stream(s) and the source carries "+
			"%d - a remux copies the video, so a different count means the video was not copied. "+
			"The source is kept", len(got), len(want))
	}
	for i := range want {
		if want[i] != got[i] {
			return fmt.Errorf("remux-only: video stream v:%d is NOT identical to the source's "+
				"(source %s, output %s) - a remux re-encodes nothing, so a stream that changed was "+
				"not copied, and the perceptual gate that would have measured the difference is "+
				"skipped on this path. The source is kept", i, want[i], got[i])
		}
	}
	return nil
}

// lengthParity is gate 3 on its own: an encode of `in` must not be TRUNCATED. It is a named
// function because the stale-temp sweep asks the identical question of a temp it finds lying
// around (strayReplacementHold), and the two must not drift: that sweep's licence to delete
// rests on anything that ever passed THIS check still passing it.
//
// It measures length twice over, deliberately. Duration is the direct measure, used whenever
// both files report one; a container that reports none (MPEG-TS is the standing example) falls
// back to video-packet count, since transcoding preserves frame count and a truncated encode
// has far fewer packets. Neither measurable is a PASS: this gate cannot convict on evidence it
// does not have, and the layers around it cover that case. Both rejections are DETERMINISTIC.
func (e *Engine) lengthParity(ctx context.Context, in, out string) (store.FailureClass, error) {
	din, okIn := e.Probe.DurationSec(ctx, in)
	dout, okOut := e.Probe.DurationSec(ctx, out)
	if okIn && okOut {
		// Both durations known: strict parity — a truncated encode is shorter.
		if math.Abs(din-dout) > e.Cfg.DurationToleranceSec {
			return store.FailureDeterministic, fmt.Errorf("duration parity failed (in=%.3fs out=%.3fs tol=%gs)", din, dout, e.Cfg.DurationToleranceSec)
		}
		return "", nil
	}
	// Duration unknown: fall back to packet parity, enforced only when the source counts.
	pin, okp := e.Probe.PacketCount(ctx, in)
	pout, okpo := e.Probe.PacketCount(ctx, out)
	if okp && pin > 0 && okpo {
		// Same tolerance as the bash: |pin-pout| <= pin*0.02 + 2.
		if math.Abs(float64(pin-pout)) > float64(pin)*0.02+2 {
			return store.FailureDeterministic, fmt.Errorf("packet-count parity failed (in=%d out=%d — truncated encode?)", pin, pout)
		}
	}
	return "", nil
}

// vmafGate measures the output (distorted) against the source (reference), in ONE pixel
// format holdfast names rather than one libavfilter negotiates, and rejects an encode on
// ANY of three independent conditions:
//
//  1. pooled harmonic mean < MinVmaf - the average is too low;
//  2. worst (sub)sampled frame < VmafMinPool - some part of the output collapsed, however
//     good the average is; and
//  3. worst (sub)sampled frame's chroma PSNR < VmafMinChroma - the COLOUR planes were
//     damaged, which (1) and (2) cannot see at all because the VMAF model extracts luma
//     features only.
//
// All three are needed. The mean is an average, so it hides local damage - Netflix documents
// exactly this ("mean pooling has the risk of hiding poor quality frames") - and measured on
// real libvmaf an encode with 4 of 240 frames destroyed to VMAF ~43 pools to ~97.5 and sails
// through (1) with every structural gate passing it too, after which the source would be
// swapped and DELETED. Only (2) sees it, and TestVmaf_MeanOnlyGateIsBlindToLocalDamage asserts
// that the mean-only gate accepts it, so the worst-frame test is not vacuous.
//
// The measured proof is returned on BOTH outcomes: a rejected encode is exactly the case where
// an operator most wants the numbers that rejected it, so it is persisted onto the failed row
// rather than living only in a log line. The three FLOORS are deterministic, as is a comparison
// format that cannot be named; the two rejections about the MEASUREMENT ITSELF, libvmaf missing
// from the build or a measurement that failed, are transient, since installing a libvmaf-capable
// build changes exactly the thing that rejected. Every floor is read off prof, so a root that
// set a stricter bar is held to its own and not the top-level one.
//
// The GATE it returns keeps the three floors apart - vmaf-mean, vmaf-min and vmaf-chroma are
// three different things to act on - and gives the three unmeasurable cases one token of their
// own (vmaf-unmeasured): those say the instrument was missing, not that the encode was bad.
// film is the deinterlace this job applied. Where it is set, the REFERENCE is produced from
// the source by that same filter at those same parameters before either stream is compared:
// the encode removed interlacing the source carried, so a score against the source as it is
// would measure the difference the FILTER made and read as a damaged encode. It is HANDED
// IN, never re-derived, and there is no path here that scores against an unfiltered
// reference when one was owed - a measurement that cannot be taken is a rejection, which is
// the same posture every other unmeasurable case in this function has.
func (e *Engine) vmafGate(ctx context.Context, distorted, reference string, prof config.Profile,
	film deinterlace.Filter) (vmafProof, string, store.FailureClass, error) {
	model := resolveVmafModel(prof.VmafModel, e.Probe.Height(ctx, distorted))

	// Name the comparison format BEFORE anything is measured, from the two streams' own pixel
	// formats: `pixel_format: auto` floors output depth at 10, so an 8-bit source routinely
	// meets a 10-bit output and leaving the conversion to libavfilter would make the score
	// depend on an undocumented choice nobody recorded. An unnameable pair is REJECTED.
	pixFmt, ok := vmaf.ComparisonFormat(e.Probe.PixFmt(ctx, reference), e.Probe.PixFmt(ctx, distorted))
	if !ok {
		return vmafProof{}, GateVmafUnmeasured, store.FailureDeterministic, fmt.Errorf(
			"cannot name a comparison pixel format for source pix_fmt %q and output pix_fmt %q "+
				"(refusing to score a pair whose comparison format would be chosen by filter negotiation)",
			e.Probe.PixFmt(ctx, reference), e.Probe.PixFmt(ctx, distorted))
	}

	score := e.vmafScore
	if score == nil {
		if !vmaf.Available(ctx, e.Probe.FFmpeg) {
			return vmafProof{}, GateVmafUnmeasured, store.FailureTransient, fmt.Errorf("VMAF gate enabled but libvmaf is not available in the ffmpeg build (refusing to accept an unmeasured encode)")
		}
		score = func(ctx context.Context, req vmaf.Request) (vmaf.Result, error) {
			return vmaf.Score(ctx, e.Probe.FFmpeg, req)
		}
	}

	res, err := score(ctx, vmaf.Request{
		Distorted:       distorted,
		Reference:       reference,
		Subsample:       prof.VmafSubsample,
		Model:           model,
		PixelFormat:     pixFmt,
		ReferenceFilter: film.Spec,
	})
	if err != nil {
		// Nothing was measured, so there is nothing to record: an empty proof, NOT a zeroed
		// one. vmaf.Score already refuses a log missing ANY pooled statistic (TRANSCODE-11
		// for the luma pair, GATE-4 for the chroma planes), so this is never a partial one.
		//
		// A reference that could not be PRODUCED arrives here as exactly this error - the
		// filter is part of the scoring graph, so a graph ffmpeg cannot build is a
		// measurement that did not happen - and it is refused on the same terms. There is
		// deliberately no second attempt without the filter: that attempt would score this
		// encode against a source it no longer resembles and call the result a measurement.
		return vmafProof{}, GateVmafUnmeasured, store.FailureTransient, fmt.Errorf("VMAF measurement failed (refusing to accept an unmeasured encode): %w", err)
	}
	proof := vmafProof{
		Mean: &res.HarmonicMean, Min: &res.Min, Model: model,
		PixFmt: res.PixelFormat, ChromaMin: &res.ChromaMin, ChromaMetric: res.ChromaMetric,
		Stream: res.Stream, Deinterlace: res.ReferenceFilter,
	}

	if res.HarmonicMean < prof.MinVmaf {
		return proof, GateVmafMean, store.FailureDeterministic, fmt.Errorf("VMAF below threshold (harmonic_mean=%.2f < min_vmaf=%.2f)", res.HarmonicMean, prof.MinVmaf)
	}
	// The worst-frame floor. On by default (vmaf_min_pool=60): a locally-broken
	// encode is invisible to the mean above and to every structural check, so this
	// is the only gate standing between it and the deletion of the source.
	if prof.VmafMinPool > 0 && res.Min < prof.VmafMinPool {
		return proof, GateVmafMin, store.FailureDeterministic, fmt.Errorf(
			"VMAF worst-frame below floor (min=%.2f < vmaf_min_pool=%.2f) — the encode is locally broken: "+
				"its average is fine (harmonic_mean=%.2f) but at least one frame collapsed, so the source is kept",
			res.Min, prof.VmafMinPool, res.HarmonicMean)
	}
	// The chroma floor (GATE-4). Everything above this line is LUMA: the VMAF model
	// extracts luma features only, so an output whose colour planes were flattened or
	// desaturated clears both floors above and every structural check too. Measured on
	// real libvmaf, a 15% chroma desaturation pools to a harmonic mean of ~99 with a
	// worst frame of ~97 while its chroma PSNR falls from ~40 dB to ~26 dB. Only this
	// sees it, and the reason NAMES the metric and the floor so an operator need not
	// go to the logs to learn which of three gates fired.
	if prof.VmafMinChroma > 0 && res.ChromaMin < prof.VmafMinChroma {
		return proof, GateVmafChroma, store.FailureDeterministic, fmt.Errorf(
			"chroma below floor (%s=%.2f < vmaf_min_chroma=%.2f) - the encode is damaged in its COLOUR "+
				"planes: its luma is fine (harmonic_mean=%.2f, worst frame=%.2f) and the VMAF model is "+
				"luma-only, so nothing else would have seen this; the source is kept",
			res.ChromaMetric, res.ChromaMin, prof.VmafMinChroma, res.HarmonicMean, res.Min)
	}
	return proof, "", "", nil
}

// resolveVmafModel maps the config VmafModel to a libvmaf model spec. The rule lives in
// internal/vmaf, beside the startup preflight that proves each candidate loads, and has to be
// ONE rule: a preflight checking other models than the gate resolves to passes, then dies hours
// into the run.
func resolveVmafModel(cfg string, height int) string {
	return vmaf.ResolveModel(cfg, height)
}
