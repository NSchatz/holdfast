// Package playabilitytest builds the playability gate's fixtures: real HEVC encodes whose
// keyframes are followed by leading pictures, and the same encode with those pictures made
// undecodable from a cold start while a linear decode of it stays clean.
package playabilitytest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// OpenGOPParams are libx265 parameters that put a CRA keyframe every second (at 24 fps) with
// four B-frames and open GOPs, so every keyframe after the first is followed by RASL pictures:
// pictures after it in decode order, shown before it, that reference the picture before it.
// x265's own default is open-gop=1 as well; the fixed keyframe interval and the disabled
// scenecut make the structure the same on every run.
const OpenGOPParams = "keyint=24:min-keyint=24:open-gop=1:bframes=4:scenecut=0"

// ClosedGOPParams are OpenGOPParams with closed GOPs: no picture after a keyframe references one
// before it.
const ClosedGOPParams = "keyint=24:min-keyint=24:open-gop=0:bframes=4:scenecut=0"

// NoBFramesParams put a keyframe every second and no B-frames at all: no reorder delay, so
// fftools does not move an input seek.
const NoBFramesParams = "keyint=24:min-keyint=24:bframes=0:scenecut=0"

// Encode writes ten seconds of 320x240 testsrc2 at 24 fps as HEVC under params to dst (the
// container from its extension).
func Encode(ffmpeg, dst, params string) error {
	return run(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=size=320x240:rate=24:duration=10",
		"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error:"+params, "-pix_fmt", "yuv420p", "--", dst)
}

// Mislabel writes to dst, in the container format named (an ffmpeg muxer name, such as
// "matroska"), a copy of src whose RASL pictures (NAL types 8 and 9) are relabelled RADL (6 and
// 7), and returns how many NAL units it relabelled.
//
// Nothing else changes: every picture keeps its bytes and its references, so a linear decode -
// which has the picture before each keyframe in its buffer when it reaches the pictures that
// reference it - decodes the same frames as from src. A decoder that STARTS at one of those
// keyframes must drop its RASL pictures, and decodes its RADL pictures from that keyframe alone;
// relabelled, they are decoded, and reference a picture that decoder never saw. That is the
// output the playability gate exists for: it passes every faithfulness gate and does not play
// from anywhere but its start.
//
// The NAL headers are rewritten in a copy of src's bytes, at the offsets ffprobe reports for its
// packets, and the copy is then remuxed (-c copy) so the container recomputes any checksum it
// carries over the rewritten bytes and every stream property is the container's own.
func Mislabel(ffmpeg, ffprobe, src, dst, format string) (int, error) {
	dir, err := os.MkdirTemp("", "holdfast-mislabel-")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	data, err := os.ReadFile(src)
	if err != nil {
		return 0, err
	}
	n, err := relabel(ffprobe, src, data)
	if err != nil {
		return 0, err
	}
	edited := filepath.Join(dir, "relabelled")
	if err := os.WriteFile(edited, data, 0o644); err != nil {
		return 0, err
	}
	if err := run(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", edited, "-map", "0", "-c", "copy",
		"-f", format, "--", dst); err != nil {
		return 0, err
	}
	return n, nil
}

// relabel rewrites, in data (the bytes of file), the NAL header of every RASL unit in file's
// first video stream, whose packets carry 4-byte length-prefixed NAL units (hvcC, as ffmpeg
// writes it).
func relabel(ffprobe, file string, data []byte) (int, error) {
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "packet=pos,size", "-of", "default=nw=1", "--", file).Output()
	if err != nil {
		return 0, fmt.Errorf("list packets: %w", err)
	}
	var size, pos int64 = -1, -1
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		x, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("packet line %q: %w", line, err)
		}
		switch k {
		case "size":
			size = x
		case "pos":
			pos = x
		}
		if size < 0 || pos < 0 {
			continue
		}
		m, err := relabelPacket(data, pos, size)
		if err != nil {
			return 0, err
		}
		n += m
		size, pos = -1, -1
	}
	return n, nil
}

// relabelPacket finds the packet's payload - at pos in an MP4, where pos is the sample's own
// offset, or after the block header in Matroska, where pos is the block's (a track-number vint,
// a 2-byte timecode and a flags byte; ffmpeg does not lace video) - as the offset at which the
// NAL lengths chain exactly to size, and relabels it.
func relabelPacket(data []byte, pos, size int64) (int, error) {
	if pos < 0 || pos >= int64(len(data)) {
		return 0, fmt.Errorf("packet at %d lies outside the file", pos)
	}
	starts := []int64{pos}
	if b := data[pos]; b != 0 {
		vint := int64(1)
		for mask := byte(0x80); b&mask == 0; mask >>= 1 {
			vint++
		}
		starts = append(starts, pos+vint+3)
	}
	for _, start := range starts {
		if chains(data, start, start+size) {
			return relabelNALs(data, start, start+size), nil
		}
	}
	return 0, fmt.Errorf("packet at %d (+%d): no length-prefixed NAL units found", pos, size)
}

// chains reports whether data[p:end] is exactly a run of 4-byte length-prefixed NAL units.
func chains(data []byte, p, end int64) bool {
	if end > int64(len(data)) || p >= end {
		return false
	}
	for p < end {
		if p+5 > end {
			return false
		}
		l := int64(data[p])<<24 | int64(data[p+1])<<16 | int64(data[p+2])<<8 | int64(data[p+3])
		if l < 2 || p+4+l > end {
			return false
		}
		p += 4 + l
	}
	return true
}

func relabelNALs(data []byte, p, end int64) int {
	n := 0
	for p < end {
		l := int64(data[p])<<24 | int64(data[p+1])<<16 | int64(data[p+2])<<8 | int64(data[p+3])
		h := p + 4
		if t := (data[h] >> 1) & 0x3f; t == 8 || t == 9 {
			data[h] = (data[h] & 0x81) | (t-2)<<1
			n++
		}
		p = h + l
	}
	return n
}

func run(ffmpeg string, args ...string) error {
	if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg %v: %w\n%s", args, err, out)
	}
	return nil
}
