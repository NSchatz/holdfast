package hdr

import (
	"regexp"
	"strconv"
	"strings"
)

// THE OUTPUT FIDELITY GATE's comparison (docs/design/encode-plan.md#fidelity): what an
// output must carry to be a faithful replacement of its source - its bit depth, chroma
// subsampling, the four colour tags and the HDR10 static metadata - and the one function
// that holds an output to it.
//
// None of these is visible to the perceptual gate. The VMAF model extracts luma features
// only, and it scores pictures, not metadata: an output that lost its mastering-display
// block, or that was encoded 8-bit where the plan said 10, or whose transfer tag now says
// bt709 over PQ samples, can score as well as a faithful one, pass every other gate, and
// replace a source this tool then deletes. So each is compared directly, field by field.

// Field names one property the fidelity gate compares. The values are stable tokens: a
// rejection names the field that differed, and an operator reads it off the row.
type Field string

// The fields, in the order Check compares them.
const (
	FieldBitDepth     Field = "bit-depth"
	FieldChroma       Field = "chroma-subsampling"
	FieldPrimaries    Field = "colour-primaries"
	FieldTransfer     Field = "transfer"
	FieldMatrix       Field = "matrix"
	FieldRange        Field = "range"
	FieldMastering    Field = "mastering-display"
	FieldContentLight Field = "content-light-level"
)

// Fields is every field the gate compares, in the order it compares them.
var Fields = []Field{
	FieldBitDepth, FieldChroma, FieldPrimaries, FieldTransfer, FieldMatrix, FieldRange,
	FieldMastering, FieldContentLight,
}

// The side-data types ffprobe names for the two HDR10 static-metadata blocks, as they
// appear in its flat output (`side_data_type="..."`).
const (
	masteringDisplayType = "Mastering display metadata"
	contentLightType     = "Content light level metadata"
)

// Fidelity is what an output must carry: the declaration an encode plan makes about its
// output, derived once from the source and the plan (FidelityOf) and compared with what the
// output is observed to carry (Check).
type Fidelity struct {
	// Depth and Chroma are the bit depth and chroma subsampling ("420", "422", "444") of
	// the pixel format the plan encodes to. Depth 0 means the plan's format could not be
	// taken apart, and then no output can be shown to match it.
	Depth  int
	Chroma string
	// Primaries, Transfer, Matrix and Range are the colour tags the output must carry, in
	// ffmpeg's spelling, each "" where the plan writes none - a source that signals none
	// has nothing to lose, and there is no tag to hold the output to.
	Primaries, Transfer, Matrix, Range string
	// Mastering and ContentLight are the source's HDR10 static metadata blocks, each
	// present when the source carries one. A carried block must reach the output with the
	// same values.
	Mastering, ContentLight Block
}

// Block is one HDR10 static-metadata block: whether it is present, and its values in
// libx265's spelling (MasterDisplay, MaxCLL). A present block whose values could not be
// read whole has Value "", and no output can be shown to carry it faithfully.
type Block struct {
	Present bool
	Value   string
}

// String renders a block for a rejection: "absent", or its values, or a note that it is
// present but could not be read.
func (b Block) String() string {
	switch {
	case !b.Present:
		return "absent"
	case b.Value == "":
		return "present but unreadable"
	default:
		return b.Value
	}
}

// FidelityOf is the declaration an encode makes about its output: the pixel format it
// encodes to (pixFmt), the colour description it writes (c), and the HDR10 static metadata
// the source carries (sourceFlat, the source's frame and stream side data in ffprobe's flat
// output). Everything the encode changes on purpose is already in pixFmt and c - the
// bit-depth floor, a forced pixel format, the HDR10 tag defaults - so the declaration is
// what the plan says, and nothing a gate re-derives.
func FidelityOf(pixFmt string, c Color, sourceFlat string) Fidelity {
	f := Fidelity{
		Primaries: c.Primaries, Transfer: c.Transfer, Matrix: c.Matrix, Range: c.Range,
		Mastering:    blockOf(sourceFlat, masteringDisplayType, MasterDisplay),
		ContentLight: blockOf(sourceFlat, contentLightType, MaxCLL),
	}
	if p, ok := PixelLayout(pixFmt); ok {
		f.Depth, f.Chroma = p.Depth, p.Chroma
	}
	return f
}

// Observed is what an output is found to carry, read by the same probe readers holdfast
// uses on sources.
type Observed struct {
	// PixFmt is the output video stream's pix_fmt as ffprobe reports it.
	PixFmt string
	// Stream are its stream-level colour tags - what a container such as Matroska answers
	// from its own colour elements - and Frame the tags of its first decoded frame, which
	// the decoder reads out of the bitstream. Each is normalised ("" where it signals none).
	Stream, Frame Tags
	// SideData is its frame and stream side data in ffprobe's flat output.
	SideData string
}

// Tags are the four colour tags at one level of a file.
type Tags struct {
	Primaries, Transfer, Matrix, Range string
}

// Mismatch is one field an output does not carry as declared.
type Mismatch struct {
	Field     Field
	Want, Got string
}

func (m Mismatch) String() string {
	return string(m.Field) + ": want " + m.Want + ", got " + m.Got
}

// Check holds an output to its declaration and returns every field it does not carry as
// declared, in Fields order; none means the output is faithful.
//
// Every way of not establishing a field is a mismatch, never a pass: a declaration whose
// pixel format could not be taken apart, an output pix_fmt that cannot be, a carried block
// whose values could not be read on either side. A gate that cannot show the output kept a
// property must not license deleting the source that has it.
func (f Fidelity) Check(o Observed) []Mismatch {
	var out []Mismatch
	add := func(field Field, want, got string) {
		out = append(out, Mismatch{Field: field, Want: want, Got: got})
	}

	p, ok := PixelLayout(o.PixFmt)
	switch {
	case f.Depth == 0:
		add(FieldBitDepth, "a pixel format this build can take apart", "the plan's pixel format")
	case !ok:
		add(FieldBitDepth, strconv.Itoa(f.Depth)+"-bit", quoted(o.PixFmt))
	default:
		if p.Depth != f.Depth {
			add(FieldBitDepth, strconv.Itoa(f.Depth)+"-bit", strconv.Itoa(p.Depth)+"-bit ("+o.PixFmt+")")
		}
		if p.Chroma != f.Chroma {
			add(FieldChroma, chromaRatio(f.Chroma), chromaRatio(p.Chroma)+" ("+o.PixFmt+")")
		}
	}

	// A declared tag is carried when some level of the output signals it and no level
	// signals anything else. Either level alone is how some player reads the file - a
	// demuxer that trusts the container, a decoder that reads only the bitstream - so a
	// level that contradicts the declaration misleads that player, and an output that
	// signals it at neither level has lost it. A level that signals nothing is not a
	// contradiction: the pinned ffmpeg writes a Matroska output's primaries and transfer
	// only where the decoded frames carry them, while libx265 writes them into the
	// bitstream from its own parameters.
	for _, t := range []struct {
		field               Field
		want, stream, frame string
	}{
		{FieldPrimaries, f.Primaries, o.Stream.Primaries, o.Frame.Primaries},
		{FieldTransfer, f.Transfer, o.Stream.Transfer, o.Frame.Transfer},
		{FieldMatrix, f.Matrix, o.Stream.Matrix, o.Frame.Matrix},
		{FieldRange, f.Range, o.Stream.Range, o.Frame.Range},
	} {
		if t.want == "" {
			continue
		}
		lost := t.stream == "" && t.frame == ""
		contradicted := (t.stream != "" && t.stream != t.want) || (t.frame != "" && t.frame != t.want)
		if lost || contradicted {
			add(t.field, t.want, levels(t.stream, t.frame))
		}
	}

	for _, b := range []struct {
		field Field
		want  Block
		got   Block
	}{
		{FieldMastering, f.Mastering, blockOf(o.SideData, masteringDisplayType, MasterDisplay)},
		{FieldContentLight, f.ContentLight, blockOf(o.SideData, contentLightType, MaxCLL)},
	} {
		if b.want.Present && (b.want.Value == "" || b.got != b.want) {
			add(b.field, b.want.String(), b.got.String())
		}
	}
	return out
}

// blockOf reads one static-metadata block out of flat side data: present when ffprobe
// names its type, with its values read by the same parser the encode's own command line
// is built with.
func blockOf(flat, typ string, read func(string) string) Block {
	if !strings.Contains(flat, `side_data_type="`+typ+`"`) {
		return Block{}
	}
	return Block{Present: true, Value: read(flat)}
}

// levels renders what the two levels of an output signal: one value where they agree, both
// where they do not.
func levels(stream, frame string) string {
	if stream == frame {
		return quoted(stream)
	}
	return "stream " + quoted(stream) + ", frame " + quoted(frame)
}

func quoted(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// chromaRatio spells a subsampling token as a ratio: "420" -> "4:2:0".
func chromaRatio(c string) string {
	return c[:1] + ":" + c[1:2] + ":" + c[2:]
}

// semiPlanarRe matches the semi-planar YUV layouts an encoder can be handed: nv12, nv16
// and nv24 (8-bit 4:2:0, 4:2:2, 4:4:4) and pNNN with a chroma digit and a depth
// (p010le = 4:2:0 10-bit, p210le = 4:2:2 10-bit, p412le = 4:4:4 12-bit).
var semiPlanarRe = regexp.MustCompile(`^(?:nv(12|16|24)|p([024])(10|12|16)(le|be))$`)

// PixelLayout takes apart any pixel format a plan or an output can name into its chroma
// subsampling and bit depth: the planar YUV family ParsePixFmt reads, and the semi-planar
// layouts hardware encoders are handed. ok=false for anything else.
func PixelLayout(pixFmt string) (PixFmt, bool) {
	if p, ok := ParsePixFmt(pixFmt); ok {
		return p, true
	}
	m := semiPlanarRe.FindStringSubmatch(pixFmt)
	if m == nil {
		return PixFmt{}, false
	}
	if m[1] != "" {
		chroma := map[string]string{"12": "420", "16": "422", "24": "444"}[m[1]]
		return PixFmt{Chroma: chroma, Depth: 8}, true
	}
	chroma := map[string]string{"0": "420", "2": "422", "4": "444"}[m[2]]
	depth, _ := strconv.Atoi(m[3])
	return PixFmt{Chroma: chroma, Depth: depth, Endian: m[4]}, true
}
