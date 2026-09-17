package engine

import (
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// The INTENDED STREAM MAP: the set of source streams this job means the output to carry.
//
// It is derived ONCE, from the source's own probe, before the encode is built - and the
// same derivation is what the encode's argv is built from AND what the verification gate
// is checked against. That is the whole point of the type: two derivations of this map
// would be two answers to "was a track lost", and the answer decides whether a source is
// deleted. A Plan carries the identity of the derivation that produced it (see ID) so
// that "the argv and the gate read ONE map" is a property something can CHECK rather
// than a property somebody asserts.
//
// Nothing here reads a stream's title, name or filename. A commentary track is one the
// CONTAINER marks as commentary, and an untagged stream is kept whatever a language list
// says: under-dropping costs space, over-dropping costs the track.

// planID mints one identity per derivation. It is monotonic and process-wide; nothing is
// persisted from it and nothing compares two IDs for order - the only question ever asked
// of it is whether two Plans came from THE SAME derivation.
var planID atomic.Int64

// StreamPlan is one job's intended stream map.
type StreamPlan struct {
	// id identifies the derivation. Unexported and set only by DeriveStreamPlan, so a
	// Plan cannot be forged with somebody else's identity.
	id int64

	// intended is every source stream the output must carry, in container order.
	intended []probe.Stream
	// dropped is every source stream it must not, in container order.
	dropped []probe.Stream

	// remuxOnly says the video is stream-copied too and nothing is re-encoded.
	remuxOnly bool
	// sourceVideoCodec is what the source's video was in, carried so the output-codec
	// gate knows what a remux is supposed to produce without probing the source a second
	// time.
	sourceVideoCodec string

	// audioFallback records that applying the audio selection would have left the output
	// with NO audio at all, so every audio stream was carried forward instead.
	audioFallback bool
}

// SelectionNotAppliedNoAudio is the stable token a row records when the audio selection
// was not applied because applying it would have produced a file with no audio. It is a
// WIRE FORMAT - it is written onto a terminal row, read back by later builds and printed
// by `holdfast export` - so it is a token and never prose.
const SelectionNotAppliedNoAudio = "audio-selection-would-leave-no-audio"

// VmafSkippedRemuxOnly is the stable token a row records when the perceptual gate did not
// run because the job was a remux whose carried video streams were established to be
// identical to the source's. Also a wire format.
const VmafSkippedRemuxOnly = "remux-only-video-identical"

// DeriveStreamPlan is THE derivation of an intended stream map, and the only one in this
// build. Everything that needs the map - the encoder's argv, the verification gate, the
// record of what was dropped - is handed the Plan this returns.
//
// streams is the source's own stream list as ffprobe established it; prof is the profile
// of the root that decides the file. sourceVideoCodec is what the source's video stream
// was in, already probed by the guards that ran before this.
//
// The rules, each the fail-safe reading of what an operator wrote:
//
//   - DATA streams are never carried, which is what `-map -0:d?` has always done.
//   - VIDEO and ATTACHMENT streams are always carried, attached pictures included. The
//     language keys are about audio and subtitles; a cover picture is neither, and the
//     multi-video parity the gate already holds is kept by carrying every one of them.
//   - An AUDIO or SUBTITLE stream is carried when its language matches its list
//     (case-insensitively) or the list is empty, and dropped when the container marks it
//     as commentary and commentary is not kept.
//   - A stream with NO language tag, an empty one, or the undefined code `und` is carried
//     whatever a language list says.
//   - If applying the audio selection would leave the output with NO audio at all, every
//     audio stream is carried instead and the fact is recorded.
func DeriveStreamPlan(streams []probe.Stream, prof config.Profile, sourceVideoCodec string) *StreamPlan {
	p := &StreamPlan{
		id:               planID.Add(1),
		remuxOnly:        prof.RemuxOnlyEnabled(),
		sourceVideoCodec: sourceVideoCodec,
	}
	audio := languageSet(prof.AudioLanguageCodes())
	subs := languageSet(prof.SubtitleLanguageCodes())
	keepCommentary := prof.CommentaryKept()

	sourceAudio := 0
	keptAudio := 0
	for _, s := range streams {
		if s.Type == probe.TypeAudio {
			sourceAudio++
		}
		if carriedStream(s, audio, subs, keepCommentary) {
			if s.Type == probe.TypeAudio {
				keptAudio++
			}
		}
	}
	// THE NEVER-A-SILENT-FILE FALLBACK. A file with no audio track is not a file anybody
	// wanted, and it cannot be recovered from the replacement - so a selection that would
	// produce one is not applied to audio at all. Only AUDIO gets this guard: a file with
	// no subtitle track is watchable, and extending the guard would make
	// `subtitle_languages: [eng]` silently a no-op on every foreign-language film.
	p.audioFallback = sourceAudio > 0 && keptAudio == 0

	for _, s := range streams {
		switch {
		case s.Type == probe.TypeData:
			// Not carried and NOT recorded as dropped: a data stream has never been
			// carried, so reporting one as dropped by this selection would attribute an
			// old behaviour to a new key.
			continue
		case p.audioFallback && s.Type == probe.TypeAudio:
			p.intended = append(p.intended, s)
		case carriedStream(s, audio, subs, keepCommentary):
			p.intended = append(p.intended, s)
		default:
			p.dropped = append(p.dropped, s)
		}
	}
	return p
}

// carriedStream is the per-stream decision, with the untagged rule applied before any
// language list is consulted.
func carriedStream(s probe.Stream, audio, subs map[string]struct{}, keepCommentary bool) bool {
	switch s.Type {
	case probe.TypeAudio, probe.TypeSubtitle:
	default:
		// Video and attachments are carried; data never reaches here (see
		// DeriveStreamPlan).
		return true
	}
	if !keepCommentary && s.Commentary {
		return false
	}
	want := audio
	if s.Type == probe.TypeSubtitle {
		want = subs
	}
	if len(want) == 0 {
		return true
	}
	if untaggedLanguage(s.Language) {
		return true
	}
	_, ok := want[s.Language]
	return ok
}

// untaggedLanguage reports whether a stream's language tag says nothing about the
// language. Containers spell "unknown" both as an ABSENT tag and as the undefined code
// `und`, and treating the code as a match candidate would drop tracks the absent form
// keeps, on the same file, for no reason an operator could predict.
func untaggedLanguage(lang string) bool {
	l := strings.ToLower(strings.TrimSpace(lang))
	return l == "" || l == "und"
}

// languageSet turns a canonical code list into a lookup. An EMPTY set means "carry every
// stream of that type", which is what an absent key resolves to.
func languageSet(codes []string) map[string]struct{} {
	if len(codes) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(codes))
	for _, c := range codes {
		set[c] = struct{}{}
	}
	return set
}

// ID identifies the derivation that produced this plan. Two plans share an ID only when
// they are the same derivation - a plan derived a second time from the same source and
// the same profile carries a DIFFERENT ID, which is exactly what makes "the argv and the
// gate read one map" checkable rather than assumed.
func (p *StreamPlan) ID() int64 {
	if p == nil {
		return 0
	}
	return p.id
}

// The two stages that read an intended stream map. They are the seam AC-6 is graded at:
// the argv is built at one and the output is checked at the other, and a reader holding
// both can ask whether they were handed the same derivation.
const (
	planStageEncode = "encode"
	planStageVerify = "verify"
)

// SameDerivation reports whether two plans came from ONE derivation. It compares
// IDENTITY and never content: two independently-derived maps that happen to agree on a
// fixture are still two answers waiting to disagree on a file that is not the fixture.
func SameDerivation(a, b *StreamPlan) bool {
	return a != nil && b != nil && a.id == b.id
}

// observePlan announces the plan one stage is about to read. Production leaves the
// observer nil and this is a nil check.
func (e *Engine) observePlan(stage string, plan *StreamPlan) {
	if e.planObserver != nil {
		e.planObserver(stage, plan)
	}
}

// RemuxOnly reports whether this job stream-copies the video too.
func (p *StreamPlan) RemuxOnly() bool { return p != nil && p.remuxOnly }

// SourceVideoCodec is what the source's video was in - what a remux's output must still
// be in, since a stream copy produces the codec it copied.
func (p *StreamPlan) SourceVideoCodec() string {
	if p == nil {
		return ""
	}
	return p.sourceVideoCodec
}

// AudioSelectionNotApplied reports whether the never-a-silent-file fallback fired.
func (p *StreamPlan) AudioSelectionNotApplied() bool { return p != nil && p.audioFallback }

// Intended is every source stream the output must carry, in container order.
func (p *StreamPlan) Intended() []probe.Stream {
	if p == nil {
		return nil
	}
	return append([]probe.Stream(nil), p.intended...)
}

// Dropped is every source stream this job selected away, in container order.
func (p *StreamPlan) Dropped() []probe.Stream {
	if p == nil {
		return nil
	}
	return append([]probe.Stream(nil), p.dropped...)
}

// SelectsEverything reports whether nothing was selected away - the state every
// configuration that names none of the four keys is in.
func (p *StreamPlan) SelectsEverything() bool { return p != nil && len(p.dropped) == 0 }

// MapArgs is the ffmpeg stream-selection argv this plan produces.
//
// When nothing was selected away it is BYTE-IDENTICAL to the argv this repository has
// always built - `-map 0 -map -0:d?`, every stream but data - so a configuration that
// names none of the four keys builds exactly the command it built before they existed.
// Only a plan that actually drops something spells the map out stream by stream.
func (p *StreamPlan) MapArgs() []string {
	if p == nil || len(p.dropped) == 0 {
		return []string{"-map", "0", "-map", "-0:d?"}
	}
	args := make([]string, 0, 2*len(p.intended))
	for _, s := range p.intended {
		args = append(args, "-map", "0:"+strconv.Itoa(s.Index))
	}
	return args
}

// AttachedPictureIndexes are the OUTPUT video-relative indexes (the N of `v:N`) of the
// attached pictures this plan carries - the streams a blanket `-c:v` would re-encode and
// which must be pinned back to copy by their own per-stream option.
//
// They are positions among the INTENDED video streams, not among the source's, because
// that is what the output's `v:N` addresses. A single carried video stream yields none,
// so a source with no cover art produces the argv it always did.
func (p *StreamPlan) AttachedPictureIndexes() []int {
	if p == nil {
		return nil
	}
	var video []probe.Stream
	for _, s := range p.intended {
		if s.Type == probe.TypeVideo {
			video = append(video, s)
		}
	}
	if len(video) <= 1 {
		return nil
	}
	var idx []int
	for i, s := range video {
		if s.AttachedPicture {
			idx = append(idx, i)
		}
	}
	return idx
}

// CheckOutput is the intended-map gate: the output carries EXACTLY the streams this plan
// intends, and nothing else.
//
// It is strictly stronger than the per-type count it replaces. A count that merely may
// not fall below the source's passes an output that carries a different stream of the
// same type, and passes every selection that did not actually apply - which is the half
// that catches a build that dropped nothing when it was told to drop something. This
// compares the MULTISET of (type, language, commentary, attached picture): every fact
// the plan itself decided on, so no substitution the selection ruled on can pass as the
// stream it replaced. It names what it counted on both sides, because a rejection an
// operator cannot read is one they cannot act on.
//
// out is the output's own stream list as ffprobe established it. A caller that could not
// establish one must REJECT rather than call this: an unknown shape is never read as the
// common one.
func (p *StreamPlan) CheckOutput(out []probe.Stream) error {
	want := streamTally(p.Intended())
	got := streamTally(out)
	for key, n := range want {
		if got[key] < n {
			return fmt.Errorf("intended-stream check failed: the output is missing %s "+
				"(intended %d, carries %d) - a track this job meant to keep was lost. "+
				"Intended: %s. Output: %s",
				describeTallyKey(key), n, got[key], describeTally(want), describeTally(got))
		}
	}
	for key, n := range got {
		if want[key] < n {
			return fmt.Errorf("intended-stream check failed: the output carries %s this job did "+
				"not intend (intended %d, carries %d) - the stream selection did not apply. "+
				"Intended: %s. Output: %s",
				describeTallyKey(key), want[key], n, describeTally(want), describeTally(got))
		}
	}
	return nil
}

// tallyKey is one kind of stream a tally counts: its type, the language the source tagged
// it with, and the two container dispositions a selection rules on. Data streams are
// excluded by the caller, never here.
type tallyKey struct {
	typ             string
	lang            string
	commentary      bool
	attachedPicture bool
}

// streamTally counts streams by (type, language, commentary, attached picture).
//
// It counts LANGUAGE as well as type because a per-type count cannot tell "kept the
// English track" from "kept the Japanese one" - and a selection that carried the wrong
// track has lost exactly as much as one that carried none.
//
// It counts the two DISPOSITIONS for the same reason one step further in. They are the
// other facts the plan decided on, and the source carries both on every stream on both
// sides of this gate: without them an English commentary track and the English main track
// are one kind, so an output that kept the commentary the plan dropped and lost the main
// track the plan intends tallies identically to the map and is accepted - on the ordinary
// `keep_commentary: false` configuration, with the source then deleted. The same holds for
// a cover picture standing in for a second video angle, which is the multi-video coverage
// this check carries forward.
func streamTally(streams []probe.Stream) map[tallyKey]int {
	t := make(map[tallyKey]int, len(streams))
	for _, s := range streams {
		if s.Type == probe.TypeData {
			continue
		}
		lang := s.Language
		if untaggedLanguage(lang) {
			// An absent tag and `und` are one fact for this comparison, because a muxer
			// legitimately writes `und` where the source carried no tag at all. Treating
			// them as different kinds would reject a faithful remux of an untagged track.
			lang = ""
		}
		t[tallyKey{
			typ:             s.Type,
			lang:            lang,
			commentary:      s.Commentary,
			attachedPicture: s.AttachedPicture,
		}]++
	}
	return t
}

// describeTallyKey names one kind of stream the way a rejection has to name it: the type,
// then the language, then whichever dispositions distinguish it from a plain stream of
// that type. A kind carrying neither reads as `type (language)`, which is all there is to
// say about an ordinary stream and all a rejection about one should make an operator read.
func describeTallyKey(k tallyKey) string {
	parts := make([]string, 0, 3)
	if k.lang == "" {
		parts = append(parts, "no language tag")
	} else {
		parts = append(parts, k.lang)
	}
	if k.commentary {
		parts = append(parts, "commentary")
	}
	if k.attachedPicture {
		parts = append(parts, "attached picture")
	}
	return k.typ + " (" + strings.Join(parts, ", ") + ")"
}

// describeTally renders a tally in a stable order, so two rejections of the same shape
// read the same way.
func describeTally(t map[tallyKey]int) string {
	keys := make([]tallyKey, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sortTallyKeys(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s x%d", describeTallyKey(k), t[k]))
	}
	if len(parts) == 0 {
		return "no streams"
	}
	return strings.Join(parts, ", ")
}

func sortTallyKeys(keys []tallyKey) {
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0; j-- {
			if tallyKeyOrdered(keys[j-1], keys[j]) {
				break
			}
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
}

// tallyKeyOrdered reports whether a sorts at or before b. Every field of the key takes
// part, because two kinds that differ only in a disposition are two kinds and a renderer
// that left their order to the map's iteration would print the same rejection two ways.
func tallyKeyOrdered(a, b tallyKey) bool {
	switch {
	case a.typ != b.typ:
		return a.typ < b.typ
	case a.lang != b.lang:
		return a.lang < b.lang
	case a.commentary != b.commentary:
		return !a.commentary
	case a.attachedPicture != b.attachedPicture:
		return !a.attachedPicture
	default:
		return true
	}
}

// DroppedRecord is what this plan's dropped streams are recorded AS on a terminal row:
// each by its source index, its type and its language AS THE SOURCE TAGGED IT. The
// dropped bytes are not recoverable from the replacement, so the row is the only record
// there is, and a record of evidence reports what the source said rather than the folded
// form this build compared against (probe.Stream.SourceLanguageTag).
//
// A plan that dropped nothing records an EMPTY set, which is a record and not an absence:
// "this job dropped nothing" and "nothing is known about what this job dropped" are two
// different statements and must stay two (see store.DroppedStreams).
func (p *StreamPlan) DroppedRecord() store.DroppedStreams {
	if p == nil {
		return store.DroppedStreams{}
	}
	out := make([]store.DroppedStream, 0, len(p.dropped))
	for _, s := range p.dropped {
		out = append(out, store.DroppedStream{
			Index:    s.Index,
			Type:     s.Type,
			Language: s.SourceLanguageTag,
		})
	}
	return store.RecordDroppedStreams(out)
}
