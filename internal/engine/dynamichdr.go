package engine

import (
	"context"
	"errors"
	"os"
	"strconv"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/dynhdr"
	"github.com/NSchatz/holdfast/internal/store"
)

// THE DYNAMIC-HDR JOB HALF (docs/design/dynamic-hdr.md): the pre-pass a carried source runs
// after its claim, beside its working file, and the gate its output is held to. What is
// carried was decided by the guards (dynamicVerdict); what a carried plan's command line is
// lives on the plan (EncodePlan.dynamic).

// The working files the pre-pass writes, each named after the working file with a
// ".dynhdr<i>" suffix (auxTempPath): beside it, under its temp marker, owned by its owner
// record (ownerKey reads the suffix), so a killed run's leftovers are swept with it.
const (
	dynamicJSON = iota // the extracted HDR10+ metadata
	dynamicRaw         // the converted profile 8.1 stream
	dynamicHead        // the head of a profile 7 source's RPU, read for its enhancement-layer type
)

// dynamicTempPath is the i-th pre-pass file of the job writing out. The HDR10+ file ends in
// ".json", the one extension x265 reads dhdr10-info from (dynhdr.Request.JSONPath).
func dynamicTempPath(out string, i int) string {
	tail := ".dynhdr" + strconv.Itoa(i)
	if i == dynamicJSON {
		tail += ".json"
	}
	return auxTempPath(out, tail)
}

// dynamicRoom is what a job carrying intent needs free beside its working file: the source's
// size, which bounds any output the gates accept (scratchRoomFor), and - where a profile 7
// source is converted - its size again, which bounds the converted stream, a lossless
// rewrite of the source's video. The HDR10+ metadata file is not added: it is a few hundred
// bytes a frame, and the output it sits beside is strictly smaller than the source
// (ASSUMED: that slack covers it; a write that fails anyway is a refused pre-pass, and the
// source is untouched).
func dynamicRoom(sourceBytes int64, intent dynhdr.Intent) int64 {
	if intent.Convert {
		return 2 * sourceBytes
	}
	return sourceBytes
}

// removeDynamicTemps removes every pre-pass file of the job writing work.
func removeDynamicTemps(work string) {
	for i := dynamicJSON; i <= dynamicHead; i++ {
		_ = os.Remove(dynamicTempPath(work, i))
	}
}

// prepareDynamic runs the pre-pass of a job carrying intent. A pre-pass that could not
// complete is the skip it names, with the configuration keys it read: the encoder (only cpu
// runs a pre-pass at all) and, for a conversion, dolby_vision_p7.
func (e *Engine) prepareDynamic(ctx context.Context, f, work string, intent dynhdr.Intent) (*dynhdr.Prepared, string, []string, error) {
	prep, err := dynhdr.Prepare(ctx, dynhdr.Request{
		Tools: e.dynamicTools(), Intent: intent, Source: absolutePath(f),
		JSONPath: absolutePath(dynamicTempPath(work, dynamicJSON)),
		RawPath:  absolutePath(dynamicTempPath(work, dynamicRaw)),
		HeadPath: absolutePath(dynamicTempPath(work, dynamicHead)),
	})
	if err == nil {
		return prep, "", nil, nil
	}
	reason := SkipHDR10PlusUnreadable
	if r, ok := dynhdr.AsRefusal(err); ok {
		reason = r.Reason
	}
	read := []string{InputEncoder}
	if intent.Convert {
		read = append(read, InputDolbyVisionP7)
	}
	return nil, reason, read, err
}

// logDynamic says what a job is about to carry.
func (e *Engine) logDynamic(f string, prof config.Profile, p *dynhdr.Prepared) {
	args := []any{"file", f, "dolby_vision", p.Intent.DolbyVision, "hdr10_plus", p.Intent.HDR10Plus}
	if p.Intent.DolbyVision {
		args = append(args, "source_profile", p.Intent.SourceProfile, "vbv_level", p.VBV.Level,
			"vbv_maxrate_kbps", p.VBV.MaxrateKbps, "vbv_bufsize_kbit", p.VBV.BufsizeKbit)
	}
	if p.Intent.Convert {
		el := p.ELType
		if el == "" {
			el = "not established"
		}
		args = append(args, "converted_to", "8.1", "dolby_vision_p7", prof.DolbyVisionP7Mode(),
			"enhancement_layer", el, "frame_rate", p.FrameRate.String())
	}
	e.Log.Info("dynamic HDR carried", args...)
}

// dynamicGate holds the output of a job carrying dynamic HDR to its plan
// (docs/design/dynamic-hdr.md#gates): a DOVI configuration record naming profile 8 and
// compatibility id 1 with an RPU on every frame, and HDR10+ on every frame, as the plan
// declares. Counts that could not be taken are TRANSIENT, as the decode gate's are; a record
// or a count that differs is DETERMINISTIC: the same source, configuration and build make the
// same output.
func (e *Engine) dynamicGate(ctx context.Context, job *EncodePlan) (string, store.FailureClass, error) {
	want := job.dynamic.Expect()
	counts, err := dynhdr.Count(ctx, e.Probe.FFprobe, job.Output)
	if err != nil {
		gate := GateHDR10Plus
		if want.DolbyVision {
			gate = GateDolbyVisionRPU
		}
		return gate, store.FailureTransient, &dynhdr.GateError{Gate: gate,
			Why: "the output's per-frame metadata could not be counted (" + err.Error() + ")"}
	}
	rec := dynhdr.RecordFrom(e.Probe.OutputFacts(ctx, job.Output).SideData)
	if err := dynhdr.Check(want, rec, counts); err != nil {
		gate := GateDolbyVisionRecord
		var ge *dynhdr.GateError
		if errors.As(err, &ge) {
			gate = ge.Gate
		}
		return gate, store.FailureDeterministic, err
	}
	return "", "", nil
}
