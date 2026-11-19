package playability

import (
	"encoding/binary"
	"fmt"
	"io"
)

// The ISO base media boxes the fragment reader reads: just enough of fragmented MP4 to cut a
// remux into its initialisation segment and its fragments, and to read each fragment's
// decode time and sample count.

// maxBoxBuffer bounds the boxes held in memory: the initialisation segment and each moof. Both
// are small (a moof is a table of its fragment's samples); the media itself is streamed.
const maxBoxBuffer = 64 << 20

// boxHeader is a box's header as read: its type and its raw bytes.
type boxHeader struct {
	typ string
	raw []byte
}

// readBoxHeader reads one top-level box header. size is the body's length, or -1 for a box
// that runs to the end of the stream (a size field of 0). io.EOF is returned only where the
// stream ends cleanly between boxes.
func readBoxHeader(r io.Reader) (boxHeader, int64, error) {
	raw := make([]byte, 8)
	if _, err := io.ReadFull(r, raw); err != nil {
		if err == io.ErrUnexpectedEOF {
			return boxHeader{}, 0, fmt.Errorf("a box header ends early")
		}
		return boxHeader{}, 0, err
	}
	h := boxHeader{typ: string(raw[4:8]), raw: raw}
	size := int64(binary.BigEndian.Uint32(raw))
	switch size {
	case 0:
		return h, -1, nil
	case 1:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(r, ext); err != nil {
			return boxHeader{}, 0, fmt.Errorf("a %s box's 64-bit size ends early", h.typ)
		}
		h.raw = append(h.raw, ext...)
		size = int64(binary.BigEndian.Uint64(ext))
	}
	if size < int64(len(h.raw)) {
		return boxHeader{}, 0, fmt.Errorf("a %s box declares %d bytes, less than its own header", h.typ, size)
	}
	return h, size - int64(len(h.raw)), nil
}

// readBody reads a box body to be held in memory, bounded by maxBoxBuffer.
func readBody(r io.Reader, h boxHeader, size int64) ([]byte, error) {
	if size < 0 || size > maxBoxBuffer {
		return nil, fmt.Errorf("a %s box of %d bytes is too large to hold", h.typ, size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("a %s box ends early: %w", h.typ, err)
	}
	return body, nil
}

// child is the body of the first box of type typ directly inside b, a run of boxes.
func child(b []byte, typ string) ([]byte, bool) {
	for len(b) >= 8 {
		size, hdr := uint64(binary.BigEndian.Uint32(b)), uint64(8)
		if size == 1 {
			if len(b) < 16 {
				return nil, false
			}
			size, hdr = binary.BigEndian.Uint64(b[8:]), 16
		}
		if size == 0 {
			size = uint64(len(b))
		}
		if size < hdr || size > uint64(len(b)) {
			return nil, false
		}
		if string(b[4:8]) == typ {
			return b[hdr:size], true
		}
		b = b[size:]
	}
	return nil, false
}

// descend follows a path of box types down from b.
func descend(b []byte, path ...string) ([]byte, bool) {
	for _, typ := range path {
		var ok bool
		if b, ok = child(b, typ); !ok {
			return nil, false
		}
	}
	return b, true
}

// mediaTimescale is the timescale of the (one) track of an initialisation segment, from its
// mdhd: the unit of every fragment's decode time.
func mediaTimescale(init []byte) (uint32, bool) {
	mdhd, ok := descend(init, "moov", "trak", "mdia", "mdhd")
	if !ok || len(mdhd) < 4 {
		return 0, false
	}
	off := 12 // version 0: version and flags, creation and modification times (32-bit)
	if mdhd[0] == 1 {
		off = 20 // version 1: 64-bit times
	}
	if len(mdhd) < off+4 {
		return 0, false
	}
	return binary.BigEndian.Uint32(mdhd[off:]), true
}

// fragmentDecodeTime is a moof's base media decode time, from its first traf's tfdt.
func fragmentDecodeTime(moof []byte) (uint64, bool) {
	tfdt, ok := descend(moof, "moof", "traf", "tfdt")
	if !ok || len(tfdt) < 8 {
		return 0, false
	}
	if tfdt[0] == 1 {
		if len(tfdt) < 12 {
			return 0, false
		}
		return binary.BigEndian.Uint64(tfdt[4:]), true
	}
	return uint64(binary.BigEndian.Uint32(tfdt[4:])), true
}

// fragmentSamples is how many samples a moof's fragment carries: the sample counts of every
// trun of its traf.
func fragmentSamples(moof []byte) (int, bool) {
	traf, ok := descend(moof, "moof", "traf")
	if !ok {
		return 0, false
	}
	n, found := 0, false
	for len(traf) >= 8 {
		size := int(binary.BigEndian.Uint32(traf))
		if size < 8 || size > len(traf) {
			return 0, false
		}
		if string(traf[4:8]) == "trun" {
			if size < 16 {
				return 0, false
			}
			n += int(binary.BigEndian.Uint32(traf[12:]))
			found = true
		}
		traf = traf[size:]
	}
	return n, found
}
