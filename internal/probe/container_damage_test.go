package probe

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestVideoProps_ContainerDamage grades S0156's [AC-13] and [AC-14] where the attribution is
// made: which of the lines the snapshot probe wrote to stderr are the DEMUXER's, and that a
// probe which did not run to completion reports none. The engine suite grades the same two
// criteria through the guard chain against the pinned ffprobe; this one names each way a line
// can be misattributed, against a scripted ffprobe, so each rule is held by a case of its own.
// The benign-line cases hold benignDemuxerLines to its exact match: the whole message from the
// demuxer it is allowed for is set aside where the file proves its shape, and anything else - a
// longer or shorter message, the same words from another demuxer, another line beside it, a file
// that does not prove it, no stream count to prove it against - is damage. The probed file is a
// movie built box by box whose chapter reference dangles (syntheticMovie), so the proof holds
// for it unless a case names another.
func TestVideoProps_ContainerDamage(t *testing.T) {
	const (
		mkv       = "codec_name=h264\nformat_name=matroska,webm\n"
		demuxLine = "[matroska,webm @ 0x55d5c3c0a2c0] 0x00 at pos 63542 (0xf836) invalid as first byte of an EBML number"
		demuxMsg  = "0x00 at pos 63542 (0xf836) invalid as first byte of an EBML number"
		decoder   = "[h264 @ 0x55d5c3c0b100] error while decoding MB 15 12"

		movDemuxer   = "mov,mp4,m4a,3gp,3g2,mj2"
		mp4          = "codec_name=h264\nnb_streams=2\nformat_name=" + movDemuxer + "\n"
		qtChapMsg    = "Referenced QT chapter track not found"
		qtChap       = "[" + movDemuxer + " @ 0x558d371a53c0] " + qtChapMsg
		movBrokenMsg = "stream 1, missing mandatory atoms, broken header"
		movBroken    = "[" + movDemuxer + " @ 0x558d371a53c0] " + movBrokenMsg
	)
	long := strings.Repeat("x", 5000)
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		f := filepath.Join(dir, name)
		if err := os.WriteFile(f, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return f
	}
	dangling := write("dangling.mp4", syntheticMovie(false, isoTrak(1, 3), isoTrak(2, 3)))
	present := write("present.mp4", syntheticMovie(false, isoTrak(1, 2), isoTrak(2, 2)))
	qtAlone := ContainerDiagnostic{Demuxer: movDemuxer, First: qtChapMsg, Count: 1}
	cases := []struct {
		name, stdout, stderr, then, file string
		cancelAfter                      time.Duration
		want                             ContainerDiagnostic
	}{
		{name: "the demuxer's lines among a decoder's", stdout: mkv,
			stderr: decoder + "\n" + demuxLine + "\n[matroska,webm @ 0x55d5c3c0a2c0] a second one\n",
			then:   "exit 0",
			want:   ContainerDiagnostic{Demuxer: "matroska,webm", First: demuxMsg, Count: 2}},
		{name: "a decoder's line alone", stdout: mkv, stderr: decoder + "\n", then: "exit 0"},
		{name: "a decoder logging with the demuxer as its parent", stdout: mkv,
			stderr: "[matroska,webm @ 0x55d5c3c0a2c0] " + decoder + "\n", then: "exit 0"},
		{name: "a raw stream whose demuxer and decoder share a name",
			stdout: "codec_name=h264\nformat_name=h264\n", stderr: decoder + "\n", then: "exit 0"},
		{name: "FLV1, decoded by the decoder named for the demuxer",
			stdout: "codec_name=flv1\nformat_name=flv\n", stderr: "[flv @ 0x1] illegal ac vlc code\n", then: "exit 0"},
		{name: "a last line with no newline", stdout: mkv, stderr: demuxLine, then: "exit 0",
			want: ContainerDiagnostic{Demuxer: "matroska,webm", First: demuxMsg, Count: 1}},
		{name: "a line longer than the bound", stdout: mkv,
			stderr: "[matroska,webm @ 0x1] " + long + "\n", then: "exit 0",
			want: ContainerDiagnostic{Demuxer: "matroska,webm",
				First: long[:maxDiagnosticLine-len("[matroska,webm @ 0x1] ")], Count: 1}},
		{name: "the mov demuxer's dangling chapter reference alone", stdout: mp4, stderr: qtChap + "\n", then: "exit 0"},
		{name: "the dangling chapter reference logged twice", stdout: mp4,
			stderr: qtChap + "\n" + qtChap + "\n", then: "exit 0"},
		{name: "the dangling chapter reference beside a decoder's line", stdout: mp4,
			stderr: decoder + "\n" + qtChap + "\n", then: "exit 0"},
		{name: "the dangling chapter reference, then another demuxer line", stdout: mp4,
			stderr: qtChap + "\n" + movBroken + "\n", then: "exit 0",
			want: ContainerDiagnostic{Demuxer: movDemuxer, First: movBrokenMsg, Count: 1}},
		{name: "another demuxer line, then the dangling chapter reference", stdout: mp4,
			stderr: movBroken + "\n" + qtChap + "\n", then: "exit 0",
			want: ContainerDiagnostic{Demuxer: movDemuxer, First: movBrokenMsg, Count: 1}},
		{name: "the benign message with more after it", stdout: mp4,
			stderr: qtChap + ": 3\n", then: "exit 0",
			want: ContainerDiagnostic{Demuxer: movDemuxer, First: qtChapMsg + ": 3", Count: 1}},
		{name: "a line that only begins like the benign message", stdout: mp4,
			stderr: "[" + movDemuxer + " @ 0x1] Referenced QT chapter track\n", then: "exit 0",
			want: ContainerDiagnostic{Demuxer: movDemuxer, First: "Referenced QT chapter track", Count: 1}},
		{name: "the benign message with words before it", stdout: mp4,
			stderr: "[" + movDemuxer + " @ 0x1] stream 2: " + qtChapMsg + "\n", then: "exit 0",
			want: ContainerDiagnostic{Demuxer: movDemuxer, First: "stream 2: " + qtChapMsg, Count: 1}},
		{name: "the dangling chapter reference alone, from a movie whose reference does not dangle", stdout: mp4,
			stderr: qtChap + "\n", then: "exit 0", file: present, want: qtAlone},
		{name: "the dangling chapter reference alone, from no readable file", stdout: mp4,
			stderr: qtChap + "\n", then: "exit 0", file: filepath.Join(dir, "missing.mp4"), want: qtAlone},
		{name: "the dangling chapter reference alone, ffprobe reporting no stream count",
			stdout: "codec_name=h264\nformat_name=" + movDemuxer + "\n", stderr: qtChap + "\n", then: "exit 0", want: qtAlone},
		{name: "the dangling chapter reference alone, ffprobe reporting a stream the movie does not carry",
			stdout: "codec_name=h264\nnb_streams=3\nformat_name=" + movDemuxer + "\n", stderr: qtChap + "\n", then: "exit 0",
			want: qtAlone},
		{name: "the dangling chapter reference twice, from a movie whose reference does not dangle", stdout: mp4,
			stderr: qtChap + "\n" + qtChap + "\n", then: "exit 0", file: present,
			want: ContainerDiagnostic{Demuxer: movDemuxer, First: qtChapMsg, Count: 2}},
		{name: "the benign message from a demuxer it is not allowed for", stdout: mkv,
			stderr: "[matroska,webm @ 0x1] " + qtChapMsg + "\n", then: "exit 0",
			want: ContainerDiagnostic{Demuxer: "matroska,webm", First: qtChapMsg, Count: 1}},
		{name: "killed by a signal after writing the demuxer's line", stdout: mkv,
			stderr: demuxLine + "\n", then: "kill -9 $$"},
		{name: "its context cancelled after writing the demuxer's line", stdout: mkv,
			stderr: demuxLine + "\n", then: "exec sleep 30", cancelAfter: 300 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := filepath.Join(t.TempDir(), "ffprobe.sh")
			script := "#!/bin/sh\nprintf '%s' '" + tc.stdout + "'\nprintf '%s' '" + tc.stderr + "' >&2\n" + tc.then + "\n"
			if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if tc.cancelAfter > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.cancelAfter)
				defer cancel()
			}
			f := tc.file
			if f == "" {
				f = dangling
			}
			vp := (&Prober{FFprobe: fake}).VideoProps(ctx, f)
			brief := func(d ContainerDiagnostic) string {
				first := d.First
				if len(first) > 80 {
					first = first[:80] + "..."
				}
				return fmt.Sprintf("{Demuxer:%q First:%q (%d bytes) Count:%d}", d.Demuxer, first, len(d.First), d.Count)
			}
			got, damaged := vp.ContainerDamage()
			if got != tc.want || damaged != (tc.want.Count > 0) {
				t.Errorf("ContainerDamage() = %s, %v; want %s, %v", brief(got), damaged, brief(tc.want), tc.want.Count > 0)
			}
		})
	}
}

// TestVideoProps_DanglingQTChapterReference holds the benign-line rule against files the pinned
// ffmpeg wrote, with the movie after the media and before it (`-movflags +faststart`). An MP4
// muxed with chapters carries them twice: as a QuickTime chapter track every other track
// references (`tref` `chap`), and as Nero `chpl` chapters. Freeing the chapter track (its box
// renamed `free`, which moves no byte) leaves the reference dangling, the shape a real library's
// MP4s were found in: the pinned ffprobe logs exactly one line, the mov demuxer's "Referenced QT
// chapter track not found", the movie's boxes prove the shape, and the file is not damaged, with
// its Nero chapters or without them.
//
// The same line is the ONLY thing ffprobe says about two damaged files, and both stay damaged:
// the audio track's first 146 bytes zeroed (a zero-size box that swallows the audio track and
// the chapter track behind it, so ffprobe reads one video stream), and the audio track zeroed
// from its first child to the end of the movie (ffprobe then reads the audio as a data stream,
// so the stream count still matches and only the walk inside the track sees it). A second,
// real demuxer error beside the line (the audio track's `stts` gone) is damage too.
func TestVideoProps_DanglingQTChapterReference(t *testing.T) {
	ffmpeg, err := exec.LookPath(envOrDefault("HOLDFAST_FFMPEG", "ffmpeg"))
	if err != nil {
		t.Fatalf("the pinned ffmpeg is required: %v", err)
	}
	p := realProber()
	dir := t.TempDir()
	meta := filepath.Join(dir, "chapters.txt")
	if err := os.WriteFile(meta, []byte(";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=1000\ntitle=Opening\n"+
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=1000\nEND=2000\ntitle=Close\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	layouts := map[string][]byte{}
	for name, flags := range map[string][]string{"the movie after the media": nil,
		"the movie before the media": {"-movflags", "+faststart"}} {
		src := filepath.Join(dir, "source.mp4")
		args := append([]string{"-hide_banner", "-loglevel", "error", "-y",
			"-f", "lavfi", "-i", "testsrc2=duration=2:size=64x48:rate=5", "-f", "lavfi", "-i", "sine=duration=2",
			"-i", meta, "-map", "0:v", "-map", "1:a", "-map_chapters", "2",
			"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac"}, flags...)
		if out, err := exec.Command(ffmpeg, append(args, "--", src)...).CombinedOutput(); err != nil {
			t.Fatalf("make fixture: %v\n%s", err, out)
		}
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		layouts[name] = b
	}

	const (
		movDemuxer = "mov,mp4,m4a,3gp,3g2,mj2"
		qtChap     = "Referenced QT chapter track not found"
		broken     = "stream 1, missing mandatory atoms, broken header"
	)
	qtDamage := ContainerDiagnostic{Demuxer: movDemuxer, First: qtChap, Count: 1}
	for _, tc := range []struct {
		name     string
		edit     func(*testing.T, []byte)
		lines    []string
		decodes  bool // a strict full decode exits 0
		chapters int  // what ffprobe reads as the chapter table; -1 is not asked
		want     ContainerDiagnostic
	}{
		{name: "intact", decodes: true, chapters: 2},
		{name: "a dangling chapter reference, the Nero chapters kept",
			edit:  func(t *testing.T, b []byte) { freeChapterTrack(t, b, false, false) },
			lines: []string{qtChap}, decodes: true, chapters: 2},
		{name: "a dangling chapter reference and no chapters at all",
			edit:  func(t *testing.T, b []byte) { freeChapterTrack(t, b, true, false) },
			lines: []string{qtChap}, decodes: true},
		{name: "a dangling chapter reference beside a broken audio track",
			edit:  func(t *testing.T, b []byte) { freeChapterTrack(t, b, true, true) },
			lines: []string{broken, qtChap}, chapters: -1,
			want: ContainerDiagnostic{Demuxer: movDemuxer, First: broken, Count: 1}},
		{name: "the audio track's first 146 bytes zeroed",
			edit: func(t *testing.T, b []byte) {
				trak, _ := audioTrack(t, b)
				clear(b[trak.off : trak.off+146])
			},
			lines: []string{qtChap}, chapters: -1, want: qtDamage},
		{name: "the audio track zeroed from its first child to the end of the movie",
			edit: func(t *testing.T, b []byte) {
				trak, moov := audioTrack(t, b)
				clear(b[trak.off+8 : moov.end])
			},
			lines: []string{qtChap}, chapters: -1, want: qtDamage},
	} {
		for layout, intact := range layouts {
			t.Run(tc.name+", "+layout, func(t *testing.T) {
				b := append([]byte(nil), intact...)
				if tc.edit != nil {
					tc.edit(t, b)
				}
				f := filepath.Join(t.TempDir(), "source.mp4")
				if err := os.WriteFile(f, b, 0o644); err != nil {
					t.Fatal(err)
				}

				// Preconditions, read off the pinned tools directly so no parser under test can
				// satisfy them: exactly these demuxer lines, and nothing else on stderr.
				out, err := exec.Command(p.FFprobe, "-v", "error", "-show_entries", "stream=codec_name",
					"-of", "default=nw=1", "--", f).CombinedOutput()
				if err != nil {
					t.Fatalf("precondition: ffprobe did not run to completion: %v\n%s", err, out)
				}
				var got []string
				for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
					if strings.HasPrefix(line, "codec_name=") {
						continue
					}
					m := movLine.FindStringSubmatch(line)
					if m == nil {
						t.Fatalf("precondition: ffprobe wrote a line the mov demuxer did not: %q", line)
					}
					got = append(got, m[1])
				}
				if strings.Join(got, "\n") != strings.Join(tc.lines, "\n") {
					t.Fatalf("precondition: the mov demuxer logged %q, want %q", got, tc.lines)
				}
				if tc.decodes {
					if out, err := exec.Command(ffmpeg, "-hide_banner", "-v", "error", "-xerror", "-err_detect", "+explode",
						"-i", f, "-map", "0", "-f", "null", "-").CombinedOutput(); err != nil {
						t.Fatalf("precondition: a strict full decode failed: %v\n%s", err, out)
					}
				}

				vp := p.VideoProps(context.Background(), f)
				if vp.Codec() != "h264" {
					t.Fatalf("Codec() = %q, want h264", vp.Codec())
				}
				dmg, damaged := vp.ContainerDamage()
				if dmg != tc.want || damaged != (tc.want.Count > 0) {
					t.Errorf("ContainerDamage() = %+v, %v; want %+v, %v", dmg, damaged, tc.want, tc.want.Count > 0)
				}
				// What the chapter-parity gate reads from this source, and so holds an output to.
				if tc.chapters >= 0 {
					if ct, ok := p.Chapters(context.Background(), f); !ok || len(ct.Chapters) != tc.chapters {
						t.Errorf("Chapters() = %+v, %v; want %d chapter(s)", ct.Chapters, ok, tc.chapters)
					}
				}
			})
		}
	}
}

// audioTrack finds, in the MP4 ffmpeg wrote, its audio track and the movie holding it.
func audioTrack(t *testing.T, b []byte) (trak, moov mp4Box) {
	t.Helper()
	for _, x := range mp4Boxes(t, b, 0, len(b)) {
		if x.typ == "moov" {
			moov = x
		}
	}
	for _, x := range mp4Boxes(t, b, moov.off+8, moov.end) {
		if x.typ != "trak" {
			continue
		}
		if hdlr, ok := mp4Path(t, b, x, "mdia", "hdlr"); ok && string(b[hdlr.off+16:hdlr.off+20]) == "soun" {
			return x, moov
		}
	}
	t.Fatal("fixture: ffmpeg wrote no audio track")
	return
}

// movLine is one line the mov demuxer logged, its message captured.
var movLine = regexp.MustCompile(`^\[mov,mp4,m4a,3gp,3g2,mj2 @ 0x[0-9a-f]+\] (.*)$`)

// freeChapterTrack frees, in the MP4 ffmpeg wrote, the track each `tref` `chap` names, and
// the Nero chapters (nero) and the audio track's `stts` (stts) if asked. Every edit renames a
// box to `free`, which every reader skips as padding, so no byte moves. The engine suite
// carries the same helper, since test files do not share code across packages.
func freeChapterTrack(t *testing.T, b []byte, nero, stts bool) {
	t.Helper()
	var moov mp4Box
	for _, x := range mp4Boxes(t, b, 0, len(b)) {
		if x.typ == "moov" {
			moov = x
		}
	}
	var traks []mp4Box
	for _, x := range mp4Boxes(t, b, moov.off+8, moov.end) {
		if x.typ == "trak" {
			traks = append(traks, x)
		}
	}
	refs := map[uint32]bool{}
	for _, trak := range traks {
		if chap, ok := mp4Path(t, b, trak, "tref", "chap"); ok {
			for i := chap.off + 8; i+4 <= chap.end; i += 4 {
				refs[binary.BigEndian.Uint32(b[i:])] = true
			}
		}
	}
	if len(refs) == 0 {
		t.Fatal("fixture: ffmpeg wrote no chapter reference, so there is none to leave dangling")
	}
	freed := 0
	for _, trak := range traks {
		tkhd, okT := mp4Path(t, b, trak, "tkhd")
		hdlr, okH := mp4Path(t, b, trak, "mdia", "hdlr")
		if !okT || !okH {
			t.Fatal("fixture: a track without a tkhd or a hdlr")
		}
		id := tkhd.off + 20 // size, type, version and flags, creation and modification times
		if b[tkhd.off+8] == 1 {
			id = tkhd.off + 28 // version 1 times are 64-bit
		}
		if refs[binary.BigEndian.Uint32(b[id:])] {
			mp4Free(b, trak)
			freed++
		}
		if stts && string(b[hdlr.off+16:hdlr.off+20]) == "soun" {
			x, ok := mp4Path(t, b, trak, "mdia", "minf", "stbl", "stts")
			if !ok {
				t.Fatal("fixture: the audio track has no stts")
			}
			mp4Free(b, x)
		}
	}
	if freed != len(refs) {
		t.Fatalf("fixture: freed %d track(s) of the %d the chapter references name", freed, len(refs))
	}
	if nero {
		x, ok := mp4Path(t, b, moov, "udta", "chpl")
		if !ok {
			t.Fatal("fixture: ffmpeg wrote no Nero chapters")
		}
		mp4Free(b, x)
	}
}

// mp4Box is one box of an ISO BMFF file: the offset of its header, the end of its payload.
type mp4Box struct {
	typ      string
	off, end int
}

// mp4Boxes lists the boxes laid end to end in b[off:end]. A size this reader does not walk (0,
// "to the end of the file", or 1, a 64-bit size) fails the test rather than being guessed at.
func mp4Boxes(t *testing.T, b []byte, off, end int) []mp4Box {
	t.Helper()
	var out []mp4Box
	for off < end {
		if end-off < 8 {
			t.Fatalf("fixture: %d stray byte(s) at %d", end-off, off)
		}
		size := int(binary.BigEndian.Uint32(b[off:]))
		if size < 8 || off+size > end {
			t.Fatalf("fixture: the box at %d has size %d, which this reader does not walk", off, size)
		}
		out = append(out, mp4Box{typ: string(b[off+4 : off+8]), off: off, end: off + size})
		off += size
	}
	return out
}

// mp4Path follows typs down from parent, each the first child of its type.
func mp4Path(t *testing.T, b []byte, parent mp4Box, typs ...string) (mp4Box, bool) {
	t.Helper()
	x := parent
	for _, typ := range typs {
		found := false
		for _, c := range mp4Boxes(t, b, x.off+8, x.end) {
			if c.typ == typ {
				x, found = c, true
				break
			}
		}
		if !found {
			return mp4Box{}, false
		}
	}
	return x, true
}

// mp4Free renames x to `free`.
func mp4Free(b []byte, x mp4Box) { copy(b[x.off+4:x.off+8], "free") }
