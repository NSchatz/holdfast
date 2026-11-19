package playability

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// The HEVC NAL unit types the GOP check reads (ITU-T H.265, table 7-1).
const (
	nalRASLN    = 8
	nalRASLR    = 9
	nalBLAWLP   = 16
	nalBLAWRADL = 17
	nalBLANLP   = 18
	nalIDRWRADL = 19
	nalIDRNLP   = 20
	nalCRA      = 21
	nalVCLEnd   = 32 // types below it are coded slices
)

// nalNames names the picture types a message can report.
var nalNames = map[uint8]string{
	0: "TRAIL_N", 1: "TRAIL_R", 2: "TSA_N", 3: "TSA_R", 4: "STSA_N", 5: "STSA_R",
	6: "RADL_N", 7: "RADL_R", nalRASLN: "RASL_N", nalRASLR: "RASL_R",
	nalBLAWLP: "BLA_W_LP", nalBLAWRADL: "BLA_W_RADL", nalBLANLP: "BLA_N_LP", nalIDRWRADL: "IDR_W_RADL",
	nalIDRNLP: "IDR_N_LP", nalCRA: "CRA",
}

func nalName(t uint8) string {
	if n, ok := nalNames[t]; ok {
		return n
	}
	return fmt.Sprintf("NAL type %d", t)
}

// ReadGOP reads the NAL unit type of every picture of file's first video stream, an HEVC
// stream, in decode order: the stream copied (never decoded) into an Annex B byte stream,
// and the type of each picture's first slice read off it. One picture is one access unit of
// the base layer, so the list lines up with the stream's packets.
func (c Checker) ReadGOP(ctx context.Context, file string) ([]uint8, error) {
	cmd := exec.CommandContext(ctx, c.FFmpeg, "-hide_banner", "-nostdin", "-v", "error", "-i", "file:"+file,
		"-map", "0:V:0", "-c:v", "copy", "-bsf:v", "hevc_mp4toannexb", "-f", "hevc", "pipe:1")
	var scan nalScanner
	var errs head
	cmd.Stdout, cmd.Stderr = &scan, &errs
	cmd.WaitDelay = waitDelay
	reason, unrun := verdict(ctx, "the read of the stream's NAL units", cmd.Run(), errs.String())
	if unrun != nil {
		return nil, unrun
	}
	if reason != "" {
		return nil, &Failure{Check: "stream", Reason: "the HEVC stream could not be copied out to read its NAL units: " + reason}
	}
	return scan.pictures, nil
}

// nalScanner reads an Annex B byte stream as it is written and records the NAL unit type of
// each picture's first slice: a coded-slice NAL unit of the base layer whose
// first_slice_segment_in_pic_flag (the first bit after the two-byte header) is set. Emulation
// prevention guarantees a start code never occurs inside a NAL unit, so every 0x000001 is
// one.
type nalScanner struct {
	carry    []byte
	pictures []uint8
}

var startCode = []byte{0, 0, 1}

func (s *nalScanner) Write(p []byte) (int, error) {
	buf := p
	if len(s.carry) > 0 {
		buf = append(s.carry, p...)
	}
	i := 0
	for {
		j := bytes.Index(buf[i:], startCode)
		if j < 0 {
			break
		}
		at := i + j + len(startCode)
		if at+3 > len(buf) {
			// The NAL header and the slice's first byte are not all here yet.
			s.carry = append([]byte(nil), buf[i+j:]...)
			return len(p), nil
		}
		s.nal(buf[at], buf[at+1], buf[at+2])
		i = at
	}
	// A start code may straddle this write and the next: keep its first two bytes.
	keep := max(len(buf)-2, i)
	s.carry = append(s.carry[:0:0], buf[keep:]...)
	return len(p), nil
}

// nal reads one NAL unit from its two header bytes and its first payload byte.
func (s *nalScanner) nal(h0, h1, first byte) {
	typ := (h0 >> 1) & 0x3f
	layer := (h0&1)<<5 | h1>>3
	if h0&0x80 == 0 && typ < nalVCLEnd && layer == 0 && first&0x80 != 0 {
		s.pictures = append(s.pictures, typ)
	}
}

var _ io.Writer = (*nalScanner)(nil)

// ClosedGOP holds an HEVC stream to a closed GOP structure: no RASL picture (a picture that
// references across its keyframe, which a decoder starting there must drop), and every
// keyframe the container flags an IDR or BLA picture (a CRA is the open-GOP keyframe). types
// are the stream's pictures in decode order (ReadGOP), matched one to one with s's packets.
// It returns "" where the stream is closed, and what it found otherwise.
func ClosedGOP(s *Stream, types []uint8) string {
	if len(types) != len(s.Packets) {
		return fmt.Sprintf("its %d pictures cannot be matched to its %d packets to read which are keyframes",
			len(types), len(s.Packets))
	}
	var findings []string
	keys, open, firstOpen := 0, 0, -1
	for i, p := range s.Packets {
		if !p.Key {
			continue
		}
		keys++
		if t := types[i]; t < nalBLAWLP || t > nalIDRNLP {
			if open++; firstOpen < 0 {
				firstOpen = i
			}
		}
	}
	if open > 0 {
		findings = append(findings, fmt.Sprintf("%d of its %d keyframes are not IDR or BLA pictures (the first, %s, is %s)",
			open, keys, s.where(firstOpen), nalName(types[firstOpen])))
	}
	rasl, firstRASL := 0, -1
	for i, t := range types {
		if t == nalRASLN || t == nalRASLR {
			if rasl++; firstRASL < 0 {
				firstRASL = i
			}
		}
	}
	if rasl > 0 {
		findings = append(findings, fmt.Sprintf("%d RASL pictures (the first %s)", rasl, s.where(firstRASL)))
	}
	return strings.Join(findings, "; ")
}

// where places the packet at decode index i for a message.
func (s *Stream) where(i int) string {
	if p := s.Packets[i]; p.HasPTS {
		return "at " + seconds(s.seconds(p.PTS))
	}
	return fmt.Sprintf("picture %d in decode order", i+1)
}
