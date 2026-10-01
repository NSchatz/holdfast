package audio

import (
	"fmt"
	"strconv"
	"strings"
)

// Settings are the audio keys one job's profile resolved to. The zero value is every key
// off, which derives the zero Plan: every carried track copied, exactly what this build did
// before the keys existed.
type Settings struct {
	// Reencode re-encodes every carried lossless track (Lossless) to Codec.
	Reencode bool
	// Codec is the codec a re-encode and a downmix are written in (Codecs).
	Codec string
	// BitratesKbps are the configured bitrates per family, kb/s; a family absent or 0 takes
	// DefaultBitrateKbps.
	BitratesKbps map[Family]int
	// KeepOriginal keeps a re-encoded track's original beside the re-encode instead of
	// replacing it.
	KeepOriginal bool
	// Downmix adds a stereo downmix of each surround track whose language carries no stereo
	// track (see Derive).
	Downmix bool
	// Loudness normalises every re-encoded and added track to EBU R 128 in two passes.
	Loudness bool
}

// Active reports whether these settings transform anything at all. Keep and loudness only
// qualify what a re-encode or a downmix does, so neither is active alone.
func (s Settings) Active() bool { return s.Reencode || s.Downmix }

// bitrate is the bitrate family is written at.
func (s Settings) bitrate(f Family) int {
	if b := s.BitratesKbps[f]; b > 0 {
		return b
	}
	return DefaultBitrateKbps(s.Codec, f)
}

// Source is one carried source audio track as the probe established it, at its position
// among the carried audio tracks.
type Source struct {
	// Index is the stream's absolute index in the source (what -map 0:<index> addresses).
	Index int
	// Codec, Profile, Channels, Layout and SampleRate are ffprobe's, verbatim.
	Codec, Profile string
	Channels       int
	Layout         string
	SampleRate     int
	// Language is the folded language tag ("" for none or und); Commentary the container's
	// comment disposition.
	Language   string
	Commentary bool
}

// Action is what the plan does with one output audio track.
type Action string

// The actions, a wire format: they are recorded on the terminal row.
const (
	// ActionCopied: the track is stream-copied; Reason says why it was not re-encoded.
	ActionCopied Action = "copied"
	// ActionReencoded: the track is re-encoded in its own place, replacing the original.
	ActionReencoded Action = "reencoded"
	// ActionKept: the original is stream-copied in its own place because keep_original_audio
	// keeps it beside its re-encode.
	ActionKept Action = "kept"
	// ActionAdded: a re-encode of a kept original, appended after every carried track.
	ActionAdded Action = "added"
	// ActionDownmix: a stereo downmix of a surround track, appended.
	ActionDownmix Action = "downmix"
	// ActionDownmixSkipped: no downmix was added for this surround track; Reason says why.
	// The output carries no track for it.
	ActionDownmixSkipped Action = "downmix-skipped"
)

// The reasons a track is copied or a downmix not added, a wire format like the actions.
const (
	ReasonReencodeOff     = "reencode-off"
	ReasonNotLossless     = "not-lossless"
	ReasonLayout          = "layout-not-carried"
	ReasonNoSampleRate    = "sample-rate-unknown"
	ReasonBitrate         = "bitrate-beyond-codec"
	ReasonContainer       = "container-not-supported"
	ReasonStereoCarried   = "stereo-track-carried"
	ReasonDownmixAdded    = "downmix-already-added"
	ReasonLoudnessSilence = "loudness-unmeasurable"
)

// Op is what the plan does with one output audio track, or - for a skipped downmix - the
// record that no track was added.
type Op struct {
	Source Source
	Action Action
	// Reason is why a track is copied or a downmix skipped, and why loudness was not applied
	// to a transformed track; "" otherwise.
	Reason string
	// Output is the track's audio-relative index in the output (the N of a:N), -1 for a
	// skipped downmix.
	Output int
	// Codec, Layout, Channels, SampleRate and BitrateKbps are what a transformed track is
	// written as, and what the gate requires; empty on a copy.
	Codec       string
	Layout      string
	Channels    int
	SampleRate  int
	BitrateKbps int
	// Loudness are the first pass's figures where the track is normalised, and nil where it
	// is not.
	Loudness *Stats
	// LoudnessIndex numbers the normalised tracks in plan order from 0: the k of the k-th
	// report channel (StatsFD). -1 on a track not normalised.
	LoudnessIndex int
}

// Transformed reports whether this op writes a new encode: a re-encode, an added re-encode
// or a downmix.
func (o Op) Transformed() bool {
	return o.Action == ActionReencoded || o.Action == ActionAdded || o.Action == ActionDownmix
}

// Appended reports whether this op's track is mapped after every carried track.
func (o Op) Appended() bool { return o.Action == ActionAdded || o.Action == ActionDownmix }

// InOutput reports whether this op's track is in the output at all.
func (o Op) InOutput() bool { return o.Action != ActionDownmixSkipped }

// Prefilter is what runs ahead of the loudness filter: the downmix on a downmix, nothing
// otherwise. The first pass measures through the same prefilter, so it measures what the
// second pass normalises.
func (o Op) Prefilter() string {
	if o.Action == ActionDownmix {
		return DownmixFilter
	}
	return ""
}

// Plan is what one encode does to the audio. The zero Plan copies every carried track and
// adds none, which is the encode this build made before the audio keys existed.
type Plan struct {
	// Ops are one per carried track in output order, then one per appended track, then one
	// per skipped downmix.
	Ops []Op
	// carried is how many carried tracks the plan was derived over.
	carried int
}

// Transforms reports whether the plan writes any new encode.
func (p Plan) Transforms() bool {
	for _, o := range p.Ops {
		if o.Transformed() {
			return true
		}
	}
	return false
}

// Normalised are the ops whose tracks are loudness-normalised, in LoudnessIndex order.
func (p Plan) Normalised() []Op {
	var out []Op
	for _, o := range p.Ops {
		if o.LoudnessIndex >= 0 && o.Loudness != nil {
			out = append(out, o)
		}
	}
	return out
}

// Outputs are the ops whose tracks are in the output, by output index.
func (p Plan) Outputs() []Op {
	var out []Op
	for _, o := range p.Ops {
		if o.InOutput() {
			out = append(out, o)
		}
	}
	return out
}

// MeasureFunc is the first loudness pass over one source track through prefilter: the
// engine runs it on the real source (Measure), a test hands in figures.
type MeasureFunc func(src Source, prefilter string) (Stats, error)

// Derive is THE derivation of an audio plan from the settings, the carried audio tracks in
// output order and the output's container extension. It runs the first loudness pass through
// measure for each track it normalises, and nothing else; inactive settings derive the zero
// Plan without calling it. A first pass that fails refuses the plan, so nothing is encoded
// on figures nobody measured.
func Derive(set Settings, carried []Source, container string, measure MeasureFunc) (Plan, error) {
	if !set.Active() {
		return Plan{}, nil
	}
	p := Plan{carried: len(carried)}
	supported := SupportedContainer(container)
	var appended []Op
	for i, src := range carried {
		op := Op{Source: src, Action: ActionCopied, Output: i, LoudnessIndex: -1}
		switch {
		case !set.Reencode:
			op.Reason = ReasonReencodeOff
		case !Lossless(src.Codec, src.Profile):
			op.Reason = ReasonNotLossless
		case !supported:
			op.Reason = ReasonContainer
		default:
			enc, reason := set.encodeAs(src, false)
			if reason != "" {
				op.Reason = reason
				break
			}
			if set.KeepOriginal {
				op.Action = ActionKept
				enc.Action = ActionAdded
				appended = append(appended, enc)
				break
			}
			enc.Action, enc.Output = ActionReencoded, i
			op = enc
		}
		p.Ops = append(p.Ops, op)
	}
	var skipped []Op
	if set.Downmix {
		added := map[string]bool{}
		for _, src := range carried {
			f, ok := FamilyOf(src.Layout)
			if src.Commentary || src.Channels <= 2 || (ok && f <= Stereo) {
				continue
			}
			skip := Op{Source: src, Action: ActionDownmixSkipped, Output: -1, LoudnessIndex: -1}
			switch {
			case !supported:
				skip.Reason = ReasonContainer
			case stereoCarried(carried, src.Language):
				skip.Reason = ReasonStereoCarried
			case added[src.Language]:
				skip.Reason = ReasonDownmixAdded
			default:
				enc, reason := set.encodeAs(src, true)
				if reason != "" {
					skip.Reason = reason
					break
				}
				enc.Action = ActionDownmix
				added[src.Language] = true
				appended = append(appended, enc)
				continue
			}
			skipped = append(skipped, skip)
		}
	}
	for j := range appended {
		appended[j].Output = len(carried) + j
	}
	p.Ops = append(append(p.Ops, appended...), skipped...)
	if set.Loudness {
		if err := p.measure(measure); err != nil {
			return Plan{}, err
		}
	}
	return p, nil
}

// measure runs the first pass for every transformed op, once per source track and prefilter.
func (p *Plan) measure(measure MeasureFunc) error {
	type key struct {
		index int
		pre   string
	}
	seen := map[key]Stats{}
	k := 0
	for i := range p.Ops {
		o := &p.Ops[i]
		if !o.Transformed() {
			continue
		}
		id := key{o.Source.Index, o.Prefilter()}
		st, ok := seen[id]
		if !ok {
			var err error
			if st, err = measure(o.Source, o.Prefilter()); err != nil {
				return fmt.Errorf("measuring the loudness of source stream %d (first pass): %w", o.Source.Index, err)
			}
			seen[id] = st
		}
		if !st.Usable() {
			o.Reason = ReasonLoudnessSilence
			continue
		}
		m := st
		o.Loudness, o.LoudnessIndex = &m, k
		k++
	}
	return nil
}

// encodeAs is the encode one source track gets in the settings' codec - as itself, or folded
// to stereo - and the reason it gets none.
func (s Settings) encodeAs(src Source, downmix bool) (Op, string) {
	f, ok := FamilyOf(src.Layout)
	if !ok || src.Channels != int(f) {
		return Op{}, ReasonLayout
	}
	if downmix {
		f = Stereo
	}
	layout, ok := OutputLayout(s.Codec, f)
	if !ok {
		return Op{}, ReasonLayout
	}
	if src.SampleRate <= 0 {
		return Op{}, ReasonNoSampleRate
	}
	rate := SampleRateFor(s.Codec, src.SampleRate)
	b := s.bitrate(f)
	if b <= 0 || b > MaxBitrateKbps(s.Codec, int(f), rate) || (s.Codec == CodecAC3 && !ValidAC3Bitrate(b)) {
		return Op{}, ReasonBitrate
	}
	return Op{Source: src, Codec: s.Codec, Layout: layout, Channels: int(f), SampleRate: rate,
		BitrateKbps: b, LoudnessIndex: -1}, ""
}

// stereoCarried reports whether a non-commentary two-channel track of language lang is
// among the carried tracks.
func stereoCarried(carried []Source, lang string) bool {
	for _, s := range carried {
		if !s.Commentary && s.Channels == 2 && s.Language == lang {
			return true
		}
	}
	return false
}

// FirstStatsFD is the file descriptor the first normalised track's second-pass report is
// written to: descriptor 3 is the encode's progress channel, and each report gets its own
// descriptor after it, so every report is read back matched to its track by construction.
const FirstStatsFD = 4

// StatsFD is the file loudnorm writes the k-th normalised track's report to: a descriptor
// the encode inherits, named through /proc so nothing is written to disk and no path needs
// quoting in the filter graph.
func StatsFD(k int) string { return "/proc/self/fd/" + strconv.Itoa(FirstStatsFD+k) }

// Args is the plan's part of the encode's command line, after the video's: the appended maps,
// then each transformed track's encoder, bitrate, layout, rate and filter chain, then the
// dispositions of the appended tracks. The zero Plan has none, so a command line without the
// audio keys is the one this build always assembled.
func (p Plan) Args() []string {
	var maps, opts []string
	for _, o := range p.Ops {
		if !o.InOutput() {
			continue
		}
		n := strconv.Itoa(o.Output)
		if o.Appended() {
			maps = append(maps, "-map", "0:"+strconv.Itoa(o.Source.Index))
		}
		if !o.Transformed() {
			continue
		}
		opts = append(opts, "-c:a:"+n, Encoder(o.Codec), "-b:a:"+n, strconv.Itoa(o.BitrateKbps)+"k",
			"-ch_layout:a:"+n, o.Layout, "-ar:a:"+n, strconv.Itoa(o.SampleRate))
		var chain []string
		if pre := o.Prefilter(); pre != "" {
			chain = append(chain, pre)
		}
		if o.Loudness != nil {
			chain = append(chain, NormalizeFilter(*o.Loudness, StatsFD(o.LoudnessIndex), o.SampleRate))
		}
		if len(chain) > 0 {
			opts = append(opts, "-filter:a:"+n, strings.Join(chain, ","))
		}
		if o.Appended() {
			// An added track is never the default one: the original keeps whatever the source
			// gave it. A commentary track's re-encode stays commentary, so the intended map
			// and the output tally it under the same kind.
			disp := "0"
			if o.Source.Commentary && o.Action == ActionAdded {
				disp = "comment"
			}
			opts = append(opts, "-disposition:a:"+n, disp)
		}
	}
	return append(maps, opts...)
}

// Check refuses a plan its own derivation could not have produced from carried: an op over a
// track that is not carried, an output index out of place, a codec, layout, rate or bitrate
// the matrix does not give, or normalisation without usable first-pass figures. The encode's
// builder calls it, so a plan that says one thing is never built into a command line that
// does another.
func (p Plan) Check(carried []Source) error {
	if len(p.Ops) == 0 {
		return nil
	}
	if p.carried != len(carried) {
		return fmt.Errorf("an audio plan derived over %d carried track(s) for an output carrying %d", p.carried, len(carried))
	}
	byIndex := map[int]Source{}
	for _, s := range carried {
		byIndex[s.Index] = s
	}
	next, k := len(carried), 0
	for i, o := range p.Ops {
		src, ok := byIndex[o.Source.Index]
		if !ok || src != o.Source {
			return fmt.Errorf("an audio operation on source stream %d, which this output does not carry as declared", o.Source.Index)
		}
		switch {
		case i < len(carried):
			if o.Appended() || !o.InOutput() || o.Output != i || carried[i].Index != o.Source.Index {
				return fmt.Errorf("the audio operation %q at output a:%d is out of place", o.Action, o.Output)
			}
		case o.Appended():
			if o.Output != next {
				return fmt.Errorf("the appended audio track %q is at a:%d, not a:%d", o.Action, o.Output, next)
			}
			next++
		case o.Action != ActionDownmixSkipped:
			return fmt.Errorf("the audio operation %q is not one an appended position takes", o.Action)
		}
		switch o.Action {
		case ActionCopied, ActionKept, ActionDownmixSkipped:
			if o.Loudness != nil {
				return fmt.Errorf("loudness declared on a track the encode copies (source stream %d)", o.Source.Index)
			}
			continue
		case ActionReencoded, ActionAdded, ActionDownmix:
		default:
			return fmt.Errorf("the audio action %q", o.Action)
		}
		f, ok := FamilyOf(o.Source.Layout)
		if o.Action == ActionDownmix {
			f = Stereo
		}
		layout, lok := OutputLayout(o.Codec, f)
		switch {
		case !ok || !lok || o.Layout != layout || o.Channels != int(f):
			return fmt.Errorf("the layout %q (%d channels) in %q for source stream %d", o.Layout, o.Channels, o.Codec, o.Source.Index)
		case o.SampleRate != SampleRateFor(o.Codec, o.Source.SampleRate):
			return fmt.Errorf("the sample rate %d in %q for source stream %d", o.SampleRate, o.Codec, o.Source.Index)
		case o.BitrateKbps <= 0 || o.BitrateKbps > MaxBitrateKbps(o.Codec, o.Channels, o.SampleRate):
			return fmt.Errorf("the bitrate %dk in %q for source stream %d", o.BitrateKbps, o.Codec, o.Source.Index)
		case o.Codec == CodecAC3 && !ValidAC3Bitrate(o.BitrateKbps):
			return fmt.Errorf("the AC-3 bitrate %dk, which AC-3 cannot carry", o.BitrateKbps)
		}
		if o.Loudness != nil {
			if !o.Loudness.Usable() || o.LoudnessIndex != k {
				return fmt.Errorf("loudness normalisation of source stream %d without usable first-pass figures", o.Source.Index)
			}
			k++
		} else if o.LoudnessIndex != -1 {
			return fmt.Errorf("a loudness report channel for source stream %d, which is not normalised", o.Source.Index)
		}
	}
	return nil
}
