package probe

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// isoB is one box with a 32-bit size: its type, then parts as its payload.
func isoB(typ string, parts ...[]byte) []byte {
	body := bytes.Join(parts, nil)
	b := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(b, uint32(8+len(body)))
	copy(b[4:], typ)
	return append(b, body...)
}

// isoTkhd is a track header of the given version carrying track ID id.
func isoTkhd(version byte, id uint32) []byte {
	n, at := 84, 12
	if version == 1 {
		n, at = 96, 20
	}
	p := make([]byte, n)
	p[0] = version
	binary.BigEndian.PutUint32(p[at:], id)
	return isoB("tkhd", p)
}

// isoChap is a `chap` track reference naming ids.
func isoChap(ids ...uint32) []byte {
	p := make([]byte, 4*len(ids))
	for i, id := range ids {
		binary.BigEndian.PutUint32(p[4*i:], id)
	}
	return isoB("chap", p)
}

// isoMdia is a small media box with the containers a track's walk descends into.
func isoMdia() []byte {
	return isoB("mdia", isoB("mdhd", make([]byte, 24)), isoB("hdlr", make([]byte, 25)),
		isoB("minf", isoB("dinf", isoB("dref", make([]byte, 8))),
			isoB("stbl", isoB("stsd", make([]byte, 8)), isoB("stts", make([]byte, 8)))))
}

// isoTrak is a track with ID id whose `tref` `chap` names chap (no `tref` when chap is empty).
func isoTrak(id uint32, chap ...uint32) []byte {
	parts := [][]byte{isoTkhd(0, id)}
	if len(chap) > 0 {
		parts = append(parts, isoB("tref", isoChap(chap...)))
	}
	return isoB("trak", append(parts, isoMdia())...)
}

var (
	isoFtyp = isoB("ftyp", []byte("isom\x00\x00\x02\x00isomiso2mp41"))
	isoMdat = isoB("mdat", make([]byte, 64))
)

// isoMoov is a movie holding traks.
func isoMoov(traks ...[]byte) []byte {
	return isoB("moov", append([][]byte{isoB("mvhd", make([]byte, 100))}, traks...)...)
}

// syntheticMovie is a whole file, built box by box: ftyp, then the movie and the media in the
// order start says (the movie first where start).
func syntheticMovie(start bool, traks ...[]byte) []byte {
	if start {
		return bytes.Join([][]byte{isoFtyp, isoMoov(traks...), isoMdat}, nil)
	}
	return bytes.Join([][]byte{isoFtyp, isoMdat, isoMoov(traks...)}, nil)
}

// zeroed is b with n bytes from off zeroed (to the end where n < 0).
func zeroed(b []byte, off, n int) []byte {
	c := append([]byte(nil), b...)
	end := len(c)
	if n >= 0 {
		end = off + n
	}
	clear(c[off:end])
	return c
}

// TestChapterReferenceDangles holds each rule of the proof the dangling-chapter line needs
// before it is excused, on movies built box by box: the shape it proves, in each layout a real
// file takes, and one case for every way a file can fall short of it. The damaged shapes are the
// ones the mov demuxer reads silently: a zero-size box swallowing a track and the chapter track
// behind it, and a track zeroed from inside to the end of the file.
func TestChapterReferenceDangles(t *testing.T) {
	dangling := syntheticMovie(false, isoTrak(1, 3), isoTrak(2, 3))
	second := bytes.Index(dangling, isoTrak(2, 3))
	moov := isoMoov(isoTrak(1, 3), isoTrak(2, 3))
	largeMdat := append([]byte{0, 0, 0, 1, 'm', 'd', 'a', 't', 0, 0, 0, 0, 0, 0, 0, 16 + 64}, make([]byte, 64)...)
	openMdat := append([]byte{0, 0, 0, 0, 'm', 'd', 'a', 't'}, make([]byte, 64)...)
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	overrun := append([]byte(nil), dangling...)
	binary.BigEndian.PutUint32(overrun[second:], uint32(len(isoTrak(2, 3))+4))
	strayInMovie := cat(isoFtyp, isoMdat, isoB("moov", isoB("mvhd", make([]byte, 100)), isoTrak(1, 3), isoTrak(2, 3), make([]byte, 4)))

	for _, tc := range []struct {
		name    string
		file    []byte
		streams int
		want    bool
	}{
		{"the reference dangling, the movie after the media", dangling, 2, true},
		{"the reference dangling, the movie before the media", syntheticMovie(true, isoTrak(1, 3), isoTrak(2, 3)), 2, true},
		{"a version 1 track header", syntheticMovie(false, isoB("trak", isoTkhd(1, 1), isoB("tref", isoChap(9)))), 1, true},
		{"a 64-bit media box", cat(isoFtyp, largeMdat, moov), 2, true},
		{"a media box running to the end of the file", cat(isoFtyp, moov, openMdat), 2, true},

		{"the reference naming a track the movie carries", syntheticMovie(false, isoTrak(1, 2), isoTrak(2, 2)), 2, false},
		{"one of the references naming a track the movie carries", syntheticMovie(false, isoTrak(1, 2), isoTrak(2, 3)), 2, false},
		{"no chapter reference", syntheticMovie(false, isoTrak(1), isoTrak(2)), 2, false},
		{"a track reference that is not a chapter one",
			syntheticMovie(false, isoB("trak", isoTkhd(0, 1), isoB("tref", isoB("hint", []byte{0, 0, 0, 3})))), 1, false},
		{"a stream ffprobe read that the movie has no track for", dangling, 3, false},
		{"a track ffprobe did not read", dangling, 1, false},
		{"a track without a header", syntheticMovie(false, isoTrak(1, 3), isoB("trak", isoB("tref", isoChap(3)))), 2, false},
		{"a track with two headers", syntheticMovie(false, isoB("trak", isoTkhd(0, 1), isoTkhd(0, 2), isoB("tref", isoChap(3)))), 1, false},
		{"a track header of an unknown version", syntheticMovie(false, isoB("trak", isoTkhd(2, 1), isoB("tref", isoChap(3)))), 1, false},
		{"a track header too short for its ID", syntheticMovie(false, isoB("trak", isoB("tkhd", make([]byte, 12)), isoB("tref", isoChap(3)))), 1, false},
		{"a chapter reference of 6 bytes", syntheticMovie(false, isoB("trak", isoTkhd(0, 1), isoB("tref", isoB("chap", make([]byte, 6))))), 1, false},
		{"an empty chapter reference", syntheticMovie(false, isoB("trak", isoTkhd(0, 1), isoB("tref", isoB("chap")))), 1, false},

		{"a track's first 146 bytes zeroed, swallowing it and the rest of the movie", zeroed(dangling, second, 146), 1, false},
		{"a track zeroed from inside to the end of the file", zeroed(dangling, second+8, -1), 2, false},
		{"a zero-size box inside a track's sample table", syntheticMovie(false, isoB("trak", isoTkhd(0, 1), isoB("tref", isoChap(3)),
			isoB("mdia", isoB("minf", isoB("stbl", isoB("stsd", make([]byte, 8)), make([]byte, 16)))))), 1, false},
		{"a track overrunning the movie", overrun, 2, false},
		{"stray bytes at the end of the movie", strayInMovie, 2, false},
		{"a file cut short inside the movie", dangling[:len(dangling)-10], 2, false},
		{"stray bytes after the last box", cat(dangling, []byte{0, 0, 0}), 2, false},
		{"two movies", cat(dangling, moov), 2, false},
		{"no movie", cat(isoFtyp, isoMdat), 0, false},
		{"a box of size 0 at the top that is not the media", cat(isoFtyp, moov, []byte{0, 0, 0, 0, 'f', 'r', 'e', 'e'}, make([]byte, 8)), 2, false},
		{"a 64-bit size smaller than its own header", cat(isoFtyp, moov, []byte{0, 0, 0, 1, 'm', 'd', 'a', 't', 0, 0, 0, 0, 0, 0, 0, 8}), 2, false},
		{"a 64-bit size past the end of the file", cat(isoFtyp, moov, []byte{0, 0, 0, 1, 'm', 'd', 'a', 't', 0, 0, 0, 0, 0, 0, 1, 0}), 2, false},
		{"a box size below its own header", cat(isoFtyp, moov, []byte{0, 0, 0, 4, 'f', 'r', 'e', 'e'}), 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := filepath.Join(t.TempDir(), "movie.mp4")
			if err := os.WriteFile(f, tc.file, 0o644); err != nil {
				t.Fatal(err)
			}
			if got := chapterReferenceDangles(f, tc.streams); got != tc.want {
				t.Errorf("chapterReferenceDangles = %v, want %v", got, tc.want)
			}
		})
	}

	dir := t.TempDir()
	if chapterReferenceDangles(filepath.Join(dir, "missing.mp4"), 2) {
		t.Error("a file that does not exist was proven")
	}
	if chapterReferenceDangles(dir, 2) {
		t.Error("a directory was proven")
	}
}
