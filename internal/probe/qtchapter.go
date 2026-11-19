package probe

import (
	"encoding/binary"
	"io"
	"os"
)

// chapterReferenceDangles is the positive evidence the mov demuxer's "Referenced QT chapter
// track not found" needs before it is excused (see benignDemuxerLines): proof, read off the
// file's own boxes, that the line describes a movie whose chapter reference names a track the
// movie does not carry, and not damage that produces the same line.
//
// The line alone proves nothing. The mov demuxer reads a box of size 0 as running to the end of
// its parent, so a zeroed stretch at the start of a track swallows that track and every box
// after it in the movie without a further word: a lost audio track and the chapter track behind
// it read as one video stream and this one line, and every later gate measures against what
// ffprobe reads, so none of them can see the track the movie lost. So the line is excused only
// where the movie is whole and the reference really dangles:
//
//   - the file's top-level boxes walk to its end exactly, each with a valid size, and exactly one
//     of them is the movie (`moov`);
//   - inside the movie, every container the walk descends into (the movie, each track, and each
//     track's `tref`, `edts`, `mdia`, `minf`, `dinf` and `stbl`) is filled exactly by boxes of a
//     valid, non-zero size: no zero-size box, none overrunning its parent, no stray bytes;
//   - the movie holds as many tracks (`trak`) as ffprobe reported streams of every type, so no
//     track was read as something else or not read at all;
//   - every track carries exactly one `tkhd`, from which its track ID is read;
//   - the chapter references (`tref` `chap`) name at least one track ID, and no track carries
//     any ID they name.
//
// Anything else, or a box this walk cannot read, is false: an unproven line stays damage. The
// walk reads box headers, each `tkhd`'s ID and each `chap`'s IDs with bounded reads, never a
// payload it does not need and never the whole file, and it gives up past maxISOBoxes boxes.
func chapterReferenceDangles(f string, streams int) bool {
	file, err := os.Open(f)
	if err != nil {
		return false
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	w := &isoWalk{r: file}
	top, ok := w.children(0, st.Size(), true)
	if !ok {
		return false
	}
	var moov []isoBox
	for _, b := range top {
		if b.typ == "moov" {
			moov = append(moov, b)
		}
	}
	if len(moov) != 1 {
		return false
	}
	movie, ok := w.children(moov[0].body, moov[0].end, false)
	if !ok {
		return false
	}
	ids, refs, traks := map[uint32]bool{}, map[uint32]bool{}, 0
	for _, trak := range movie {
		if trak.typ != "trak" {
			continue
		}
		traks++
		id, chap, ok := w.track(trak)
		if !ok {
			return false
		}
		ids[id] = true
		for _, r := range chap {
			refs[r] = true
		}
	}
	if traks != streams || len(refs) == 0 {
		return false
	}
	for r := range refs {
		if ids[r] {
			return false
		}
	}
	return true
}

// maxISOBoxes bounds how many box headers one walk reads, so a file of millions of tiny boxes
// costs a bounded number of reads and is not proven.
const maxISOBoxes = 1 << 16

// maxChapterRefs bounds how many track IDs one `chap` box may name before it is not read.
const maxChapterRefs = 1024

// isoBox is one box: its type, where its payload starts and where it ends.
type isoBox struct {
	typ       string
	body, end int64
}

// isoWalk reads boxes out of a file with bounded reads.
type isoWalk struct {
	r     io.ReaderAt
	boxes int
}

// children lists the boxes laid end to end in [off, end), and false unless they fill it
// exactly, each with a valid size. A size of 0 (the box runs to the end of the file) is valid
// only for a top-level `mdat`; a size of 1 takes the 64-bit size after the type.
func (w *isoWalk) children(off, end int64, top bool) ([]isoBox, bool) {
	var out []isoBox
	for off < end {
		if w.boxes++; w.boxes > maxISOBoxes || end-off < 8 {
			return nil, false
		}
		var h [16]byte
		if _, err := w.r.ReadAt(h[:8], off); err != nil {
			return nil, false
		}
		size, typ, hdr := int64(binary.BigEndian.Uint32(h[:4])), string(h[4:8]), int64(8)
		switch {
		case size == 1:
			if end-off < 16 {
				return nil, false
			}
			if _, err := w.r.ReadAt(h[8:16], off+8); err != nil {
				return nil, false
			}
			large := binary.BigEndian.Uint64(h[8:16])
			if large > uint64(end-off) {
				return nil, false
			}
			size, hdr = int64(large), 16
		case size == 0 && top && typ == "mdat":
			size = end - off
		}
		if size < hdr || size > end-off {
			return nil, false
		}
		out = append(out, isoBox{typ: typ, body: off + hdr, end: off + size})
		off += size
	}
	return out, true
}

// isoContainers are the boxes inside a track that the walk descends into, each of which holds
// nothing but boxes.
var isoContainers = map[string]bool{"tref": true, "edts": true, "mdia": true, "minf": true, "dinf": true, "stbl": true}

// track reads one `trak`: its track ID from its one `tkhd`, the IDs its `tref` `chap` boxes
// name, and false unless the track and every container under it walks exactly.
func (w *isoWalk) track(trak isoBox) (id uint32, chap []uint32, ok bool) {
	kids, ok := w.children(trak.body, trak.end, false)
	if !ok {
		return 0, nil, false
	}
	tkhds := 0
	for _, b := range kids {
		switch b.typ {
		case "tkhd":
			tkhds++
			if id, ok = w.trackID(b); !ok {
				return 0, nil, false
			}
		case "tref":
			refs, ok := w.children(b.body, b.end, false)
			if !ok {
				return 0, nil, false
			}
			for _, r := range refs {
				if r.typ != "chap" {
					continue
				}
				ids, ok := w.chapterRefs(r)
				if !ok {
					return 0, nil, false
				}
				chap = append(chap, ids...)
			}
		default:
			if isoContainers[b.typ] && !w.container(b, 0) {
				return 0, nil, false
			}
		}
	}
	return id, chap, tkhds == 1
}

// container reports whether every container under b, b included, is filled exactly by boxes
// of a valid size. depth bounds the descent.
func (w *isoWalk) container(b isoBox, depth int) bool {
	if depth > 8 {
		return false
	}
	kids, ok := w.children(b.body, b.end, false)
	if !ok {
		return false
	}
	for _, k := range kids {
		if isoContainers[k.typ] && !w.container(k, depth+1) {
			return false
		}
	}
	return true
}

// trackID reads the track ID out of a `tkhd`: after the version and flags and the creation
// and modification times, 32-bit in version 0 and 64-bit in version 1.
func (w *isoWalk) trackID(b isoBox) (uint32, bool) {
	var v [1]byte
	if b.end-b.body < 4 {
		return 0, false
	}
	if _, err := w.r.ReadAt(v[:], b.body); err != nil {
		return 0, false
	}
	var off int64
	switch v[0] {
	case 0:
		off = 12
	case 1:
		off = 20
	default:
		return 0, false
	}
	if b.end-b.body < off+4 {
		return 0, false
	}
	var id [4]byte
	if _, err := w.r.ReadAt(id[:], b.body+off); err != nil {
		return 0, false
	}
	return binary.BigEndian.Uint32(id[:]), true
}

// chapterRefs reads the track IDs a `chap` box names: a whole number of 32-bit IDs, at least
// one and at most maxChapterRefs.
func (w *isoWalk) chapterRefs(b isoBox) ([]uint32, bool) {
	n := b.end - b.body
	if n == 0 || n%4 != 0 || n/4 > maxChapterRefs {
		return nil, false
	}
	buf := make([]byte, n)
	if _, err := w.r.ReadAt(buf, b.body); err != nil {
		return nil, false
	}
	ids := make([]uint32, 0, n/4)
	for i := int64(0); i < n; i += 4 {
		ids = append(ids, binary.BigEndian.Uint32(buf[i:]))
	}
	return ids, true
}
