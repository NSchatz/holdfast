package playability

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// The decision logic on its own, over values built by hand: no process runs here except the
// one shell that dies of a signal.

func TestVerdict_HowAProcessEnded(t *testing.T) {
	exitWith := func(code string) error { return exec.Command("sh", "-c", "exit "+code).Run() }
	killed := exec.Command("sh", "-c", "kill -KILL $$").Run()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		err    error
		stderr string
		reason string
		unrun  bool
	}{
		{"a clean exit that printed nothing", context.Background(), nil, " \n", "", false},
		{"a clean exit that printed an error", context.Background(), nil, "first\nsecond\n", "first", false},
		{"a non-zero exit", context.Background(), exitWith("3"), "\n  boom  \nafter", "boom", false},
		{"a non-zero exit that printed nothing", context.Background(), exitWith("3"), "", "exited with status 3 and printed no error", false},
		{"a binary that did not start", context.Background(), exec.ErrNotFound, "", "", true},
		{"a process killed by a signal", context.Background(), killed, "", "", true},
		{"a cancelled context", cancelled, nil, "an error", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, unrun := verdict(tc.ctx, "the thing", tc.err, tc.stderr)
			if reason != tc.reason || (unrun != nil) != tc.unrun {
				t.Fatalf("verdict = %q, %v; want %q, unrun %v", reason, unrun, tc.reason, tc.unrun)
			}
			if unrun != nil && !strings.HasPrefix(unrun.Error(), "playability: the thing could not be run: ") {
				t.Errorf("unrun reads %q", unrun)
			}
		})
	}
	if exitCode(exitWith("7")) != 7 || exitCode(errors.New("x")) != 0 {
		t.Error("exitCode misreads a status")
	}
}

func TestReasonOf_IsTheFirstLineBoundedAndPrintable(t *testing.T) {
	long := strings.Repeat("\u00e9", maxReason+5)
	for _, tc := range []struct{ in, want string }{
		{"[hevc @ 0x55d0c1a2b3c0] Could not find ref with POC 24\nlater", "[hevc] Could not find ref with POC 24"},
		{"[vist#0:0/hevc @ 0xABCDEF] [dec:hevc @ 0x1] bad", "[vist#0:0/hevc] [dec:hevc] bad"},
		{"tab\there\x01", "tab?here?"},
		{long, strings.Repeat("\u00e9", maxReason) + "..."},
		{strings.Repeat("\u00e9", maxReason), strings.Repeat("\u00e9", maxReason)},
	} {
		if got := reasonOf(tc.in, 1); got != tc.want {
			t.Errorf("reasonOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHead_KeepsTheFirstBytesOnly(t *testing.T) {
	var h head
	if n, _ := h.Write(bytes.Repeat([]byte{'a'}, maxHead-1)); n != maxHead-1 {
		t.Fatal("a write reported a short count")
	}
	if n, _ := h.Write([]byte("bcd")); n != 3 {
		t.Fatal("a write past the bound reported a short count")
	}
	_, _ = h.Write([]byte("e"))
	if got := h.String(); len(got) != maxHead || !strings.HasSuffix(got, "ab") {
		t.Fatalf("kept %d bytes ending %q", len(got), got[len(got)-2:])
	}
}

func TestFailureAndUnrun_Read(t *testing.T) {
	f := &Failure{Check: "cold start", At: "the keyframe at 1.000 s", Reason: "bad"}
	if f.Error() != "playability cold start at the keyframe at 1.000 s: bad" {
		t.Errorf("%q", f.Error())
	}
	if g := (&Failure{Check: "stream", Reason: "none"}); g.Error() != "playability stream: none" {
		t.Errorf("%q", g.Error())
	}
	u := &Unrun{What: "the remux", Err: context.Canceled}
	if !errors.Is(u, context.Canceled) || IsFailure(u) || !IsFailure(error(f)) {
		t.Error("the two kinds do not tell themselves apart")
	}
}

func TestCheck_AsksForNothingRunsNothing(t *testing.T) {
	broken := Checker{FFmpeg: "/nonexistent/ffmpeg", FFprobe: "/nonexistent/ffprobe"}
	if err := broken.Check(context.Background(), "/nonexistent/file", nil, Options{}); err != nil {
		t.Fatalf("no check asked for, and still: %v", err)
	}
	if err := broken.Check(context.Background(), "/nonexistent/file", nil, Options{ColdStarts: 1}); err == nil || IsFailure(err) {
		t.Fatalf("a cold start with no ffprobe: %v", err)
	}
}

func TestParseStream(t *testing.T) {
	good := "packet|pts=0|flags=K__\npacket|pts=N/A|flags=___|side_datum/x:side_data_type=y\n" +
		"packet|pts=42|flags=K_D\n\nstream|time_base=1/1000|side_datum/a=b\n"
	s, err := parseStream(good)
	if err != nil {
		t.Fatal(err)
	}
	want := &Stream{TimeBase: Rational{1, 1000}, Packets: []Packet{
		{PTS: 0, HasPTS: true, Key: true}, {}, {PTS: 42, HasPTS: true, Key: true}}}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("parsed %+v, want %+v", s, want)
	}
	if s, _ := parseStream("stream|time_base=1/90000"); s == nil || s.TimeBase != (Rational{1, 90000}) || len(s.Packets) != 0 {
		t.Fatalf("a stream with no packet: %+v", s)
	}
	for name, bad := range map[string]string{
		"no stream":         "packet|pts=0|flags=K__",
		"two streams":       "stream|time_base=1/1000\nstream|time_base=1/1000",
		"no flags":          "packet|pts=0\nstream|time_base=1/1000",
		"empty flags":       "packet|pts=0|flags=\nstream|time_base=1/1000",
		"a pts that is not": "packet|pts=x|flags=K__\nstream|time_base=1/1000",
		"no time base":      "stream|codec_type=video",
		"a bad time base":   "stream|time_base=1/0",
	} {
		if _, err := parseStream(bad); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestParseRational(t *testing.T) {
	for in, want := range map[string]bool{"1/1000": true, "1001/30000": true, "0/1": false, "1/0": false,
		"-1/2": false, "1/-2": false, "x/2": false, "1/x": false, "1000": false, "": false} {
		if _, ok := parseRational(in); ok != want {
			t.Errorf("parseRational(%q) ok=%v", in, ok)
		}
	}
	if r, _ := parseRational("1001/30000"); r != (Rational{1001, 30000}) {
		t.Errorf("%v", r)
	}
}

// pkts builds a stream from presentation times in decode order; a negative time is a packet
// with none, and a time followed by "k" in keys is a keyframe.
func pkts(pts []int64, keys ...int) *Stream {
	s := &Stream{TimeBase: Rational{1, 1000}}
	for _, p := range pts {
		s.Packets = append(s.Packets, Packet{PTS: p, HasPTS: p >= 0})
	}
	for _, k := range keys {
		s.Packets[k].Key = true
	}
	return s
}

func TestKeyframesAndLeads(t *testing.T) {
	// Decode order of two open GOPs and a closed one: keyframe 0 at 0; keyframe 4 at 400 with
	// leading pictures 300 and 350; keyframe 7 at 700 with only trailing pictures, and a
	// picture after the NEXT keyframe that is earlier than 700, which is not 7's.
	s := pkts([]int64{0, 100, 200, 250, 400, 300, 350, 700, 800, 900, 1000, 650, -1}, 0, 4, 7, 10, 12)
	if got := s.Keyframes(); !reflect.DeepEqual(got, []int{0, 4, 7, 10}) {
		t.Fatalf("keyframes %v: a keyframe without a time is not one a cold start can aim at", got)
	}
	for k, want := range map[int]bool{0: false, 4: true, 7: false, 10: true} {
		if s.Leads(k) != want {
			t.Errorf("Leads(%d) = %v", k, !want)
		}
	}
	// A packet with no time between a keyframe and the next is not a leading picture.
	if pkts([]int64{500, -1, 600}, 0).Leads(0) {
		t.Error("a packet with no time read as leading")
	}
	// The last keyframe's group runs to the end of the stream.
	if !pkts([]int64{0, 500, 400}, 0, 1).Leads(1) {
		t.Error("the last keyframe's leading picture was missed")
	}
	// A picture presented at the keyframe's own time is not before it.
	if pkts([]int64{500, 500, 600}, 0).Leads(0) {
		t.Error("a picture at the keyframe's own time read as leading")
	}
}

func TestSpread(t *testing.T) {
	xs := []int{10, 11, 12, 13, 14, 15, 16, 17, 18, 19}
	for _, tc := range []struct {
		n    int
		want []int
	}{
		{0, nil}, {-1, nil},
		{1, []int{15}},
		{3, []int{11, 15, 18}},
		{4, []int{11, 13, 16, 18}},
		{9, []int{10, 11, 12, 13, 15, 16, 17, 18, 19}},
		{10, xs}, {11, xs},
	} {
		if got := spread(xs, tc.n); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("spread(n=%d) = %v, want %v", tc.n, got, tc.want)
		}
	}
	all := spread(xs, 20)
	all[0] = 99
	if xs[0] != 10 {
		t.Error("spread handed back the caller's own slice")
	}
}

func TestColdStartTargets(t *testing.T) {
	// Twelve keyframes at 0, 100, ... 1100, each followed by one picture; those at 300, 500,
	// 700, 900 and 1100 are followed by a leading picture.
	var pts []int64
	var keys []int
	for i := int64(0); i < 12; i++ {
		keys = append(keys, len(pts))
		lead := i*100 + 50
		if i%2 == 1 && i >= 3 {
			lead = i*100 - 50
		}
		pts = append(pts, i*100, lead)
	}
	s := pkts(pts, keys...)
	// n=2: the spread sample is keyframes 3 and 9 (packets 6 and 18); up to eight leading
	// keyframes, and there are five, so every one of them: packets 6, 10, 14, 18, 22.
	if got, want := s.ColdStartTargets(2), []int{6, 10, 14, 18, 22}; !reflect.DeepEqual(got, want) {
		t.Errorf("ColdStartTargets(2) = %v, want %v", got, want)
	}
	// n=1: keyframe 6 (packet 12), and up to four leading keyframes spread across the five.
	if got, want := s.ColdStartTargets(1), []int{6, 10, 12, 18, 22}; !reflect.DeepEqual(got, want) {
		t.Errorf("ColdStartTargets(1) = %v, want %v", got, want)
	}
	if got := s.ColdStartTargets(0); len(got) != 0 {
		t.Errorf("ColdStartTargets(0) = %v", got)
	}
	if got := s.ColdStartTargets(100); len(got) != 12 {
		t.Errorf("ColdStartTargets(100) = %v, want every keyframe", got)
	}
}

// annexB writes NAL units, each after a start code (four bytes for the first, three after).
func annexB(nals ...[]byte) []byte {
	var b []byte
	for i, n := range nals {
		if i == 0 {
			b = append(b, 0)
		}
		b = append(b, 0, 0, 1)
		b = append(b, n...)
	}
	return b
}

// nal is a NAL unit of type typ in layer layer whose payload starts with first, then a body.
func nal(typ, layer, first byte) []byte {
	return []byte{typ<<1 | layer>>5, layer<<3 | 1, first, 0xaa, 0x55, 0x00, 0x03, 0x01}
}

func TestNALScanner(t *testing.T) {
	stream := annexB(
		nal(32, 0, 0x8c), nal(33, 0, 0x01), nal(34, 0, 0xc1), // VPS (its first bit set too), SPS, PPS
		nal(nalIDRWRADL, 0, 0x80), nal(nalIDRWRADL, 0, 0x40), // an IDR's first slice and a second slice
		nal(39, 0, 0x80), // SEI: not a picture
		nal(1, 0, 0xc0),  // TRAIL_R
		nal(1, 1, 0x80),  // the same in layer 1: not a base-layer picture
		nal(nalCRA, 0, 0x80),
		nal(nalRASLN, 0, 0x80),
		append([]byte{0x80 | 1<<1, 0x01, 0x80}, 0xaa), // forbidden bit set: not a NAL unit
		nal(0, 0, 0x80),
		[]byte{36 << 1, 1}, // EOS at the very end, two bytes
	)
	want := []uint8{nalIDRWRADL, 1, nalCRA, nalRASLN, 0}
	var whole nalScanner
	_, _ = whole.Write(stream)
	if !reflect.DeepEqual(whole.pictures, want) {
		t.Fatalf("pictures %v, want %v", whole.pictures, want)
	}
	// The same stream written in two pieces split at every byte, and a byte at a time.
	for cut := 0; cut <= len(stream); cut++ {
		var s nalScanner
		if n, _ := s.Write(stream[:cut]); n != cut {
			t.Fatal("a write reported a short count")
		}
		_, _ = s.Write(stream[cut:])
		if !reflect.DeepEqual(s.pictures, want) {
			t.Fatalf("split at %d: pictures %v, want %v", cut, s.pictures, want)
		}
	}
	var bytewise nalScanner
	for i := range stream {
		_, _ = bytewise.Write(stream[i : i+1])
	}
	if !reflect.DeepEqual(bytewise.pictures, want) {
		t.Fatalf("a byte at a time: pictures %v, want %v", bytewise.pictures, want)
	}
	// A stream whose last NAL unit is a slice of exactly its header and one byte.
	var last nalScanner
	_, _ = last.Write(annexB(nal(nalIDRNLP, 0, 0x80), nal(1, 0, 0x80)[:3]))
	if !reflect.DeepEqual(last.pictures, []uint8{nalIDRNLP, 1}) {
		t.Fatalf("a slice ending the stream: pictures %v", last.pictures)
	}
}

func TestClosedGOP(t *testing.T) {
	s := pkts([]int64{0, 100, 1000, 900, 1100, 2000, 2100}, 0, 2, 5)
	idr, cra, trail, rasl := uint8(nalIDRWRADL), uint8(nalCRA), uint8(1), uint8(nalRASLR)
	for _, tc := range []struct {
		name  string
		types []uint8
		want  string
	}{
		{"closed", []uint8{idr, trail, uint8(nalIDRNLP), 6, trail, uint8(nalBLAWLP), trail}, ""},
		{"BLA keyframes of every kind", []uint8{uint8(nalBLAWRADL), trail, uint8(nalBLANLP), 7, trail, idr, trail}, ""},
		{"open", []uint8{idr, trail, cra, rasl, trail, cra, 8},
			"2 of its 3 keyframes are not IDR or BLA pictures (the first, at 1.000 s, is CRA); 2 RASL pictures (the first at 0.900 s)"},
		{"a keyframe on a trailing picture", []uint8{idr, trail, idr, 6, trail, trail, trail},
			"1 of its 3 keyframes are not IDR or BLA pictures (the first, at 2.000 s, is TRAIL_R)"},
		{"a RASL picture in a closed stream", []uint8{idr, trail, idr, rasl, trail, idr, trail},
			"1 RASL pictures (the first at 0.900 s)"},
		{"a keyframe on a reserved type", []uint8{22, trail, idr, 6, trail, idr, trail},
			"1 of its 3 keyframes are not IDR or BLA pictures (the first, at 0.000 s, is NAL type 22)"},
		{"the first keyframe open, and a later one", []uint8{cra, trail, cra, 6, trail, idr, trail},
			"2 of its 3 keyframes are not IDR or BLA pictures (the first, at 0.000 s, is CRA)"},
		{"the first picture RASL, and a later one", []uint8{idr, rasl, idr, rasl, trail, idr, trail},
			"2 RASL pictures (the first at 0.100 s)"},
		{"a picture short", []uint8{idr, trail, idr, 6, trail, idr},
			"its 6 pictures cannot be matched to its 7 packets to read which are keyframes"},
	} {
		if got := ClosedGOP(s, tc.types); got != tc.want {
			t.Errorf("%s: ClosedGOP = %q, want %q", tc.name, got, tc.want)
		}
	}
	untimed := pkts([]int64{-1, 0}, 0)
	if got := ClosedGOP(untimed, []uint8{cra, trail}); !strings.Contains(got, "(the first, picture 1 in decode order, is CRA)") {
		t.Errorf("a keyframe with no time: %q", got)
	}
}

// box writes one box of type typ around body.
func box(typ string, body ...[]byte) []byte {
	b := bytes.Join(body, nil)
	out := binary.BigEndian.AppendUint32(nil, uint32(8+len(b)))
	return append(append(out, typ...), b...)
}

func TestBoxes(t *testing.T) {
	mdhd0 := append(make([]byte, 12), 0, 0, 0x5d, 0xc0) // version 0, timescale 24000
	mdhd1 := append(append([]byte{1}, make([]byte, 19)...), 0, 0, 0x3e, 0x80)
	init0 := append(box("ftyp", []byte("isom")), box("moov", box("mvhd"), box("trak", box("tkhd"), box("mdia", box("hdlr"), box("mdhd", mdhd0))))...)
	if ts, ok := mediaTimescale(init0); !ok || ts != 24000 {
		t.Errorf("timescale v0 = %d %v", ts, ok)
	}
	init1 := box("moov", box("trak", box("mdia", box("mdhd", mdhd1))))
	if ts, ok := mediaTimescale(init1); !ok || ts != 16000 {
		t.Errorf("timescale v1 = %d %v", ts, ok)
	}
	for name, bad := range map[string][]byte{
		"no moov":    box("ftyp"),
		"short mdhd": box("moov", box("trak", box("mdia", box("mdhd", make([]byte, 15))))),
		"short v1":   box("moov", box("trak", box("mdia", box("mdhd", append([]byte{1}, make([]byte, 22)...))))),
		"empty mdhd": box("moov", box("trak", box("mdia", box("mdhd")))),
	} {
		if _, ok := mediaTimescale(bad); ok {
			t.Errorf("%s: a timescale read", name)
		}
	}

	tfdt0 := box("tfdt", []byte{0, 0, 0, 0}, []byte{0, 0x02, 0x32, 0x80}) // 144000
	tfdt1 := box("tfdt", []byte{1, 0, 0, 0}, []byte{0, 0, 0, 0, 0, 0x02, 0x32, 0x80})
	moof0 := box("moof", box("mfhd", make([]byte, 8)), box("traf", box("tfhd", make([]byte, 8)), tfdt0))
	if d, ok := fragmentDecodeTime(moof0); !ok || d != 144000 {
		t.Errorf("tfdt v0 = %d %v", d, ok)
	}
	if d, ok := fragmentDecodeTime(box("moof", box("traf", tfdt1))); !ok || d != 144000 {
		t.Errorf("tfdt v1 = %d %v", d, ok)
	}
	for name, bad := range map[string][]byte{
		"no tfdt":  box("moof", box("traf")),
		"short":    box("moof", box("traf", box("tfdt", []byte{0, 0, 0, 0, 1}))),
		"short v1": box("moof", box("traf", box("tfdt", []byte{1, 0, 0, 0, 0, 0, 0, 0, 1}))),
	} {
		if _, ok := fragmentDecodeTime(bad); ok {
			t.Errorf("%s: a decode time read", name)
		}
	}
	trun := func(n uint32) []byte { return box("trun", []byte{0, 0, 0, 1}, binary.BigEndian.AppendUint32(nil, n)) }
	if n, ok := fragmentSamples(box("moof", box("traf", box("tfhd"), tfdt0, trun(24), trun(3)))); !ok || n != 27 {
		t.Errorf("samples = %d %v", n, ok)
	}
	for name, bad := range map[string][]byte{
		"no traf":      box("moof", box("mfhd")),
		"no trun":      box("moof", box("traf", tfdt0)),
		"a short trun": box("moof", box("traf", box("trun", []byte{0, 0, 0, 1, 0, 0}))),
		"a bad size":   box("moof", box("traf", append(binary.BigEndian.AppendUint32(nil, 99), "trun"...))),
		"a tiny size":  box("moof", box("traf", append(binary.BigEndian.AppendUint32(nil, 4), "trunxxxx"...))),
	} {
		if _, ok := fragmentSamples(bad); ok {
			t.Errorf("%s: a sample count read", name)
		}
	}

	// A 64-bit size, a size of 0 (to the end), and sizes that cannot be.
	large := append(binary.BigEndian.AppendUint32(nil, 1), "free"...)
	large = binary.BigEndian.AppendUint64(large, 20)
	large = append(large, 1, 2, 3, 4)
	if b, ok := child(append(large, box("moov", []byte("x"))...), "moov"); !ok || string(b) != "x" {
		t.Errorf("past a 64-bit box: %q %v", b, ok)
	}
	if b, ok := child(append(box("free"), append([]byte{0, 0, 0, 0}, "moov123"...)...), "moov"); !ok || string(b) != "123" {
		t.Errorf("a box to the end: %q %v", b, ok)
	}
	if b, ok := child(append(box("free", []byte("abc")), box("moov")...), "moov"); !ok || len(b) != 0 {
		t.Errorf("an empty box last: %q %v", b, ok)
	}
	for name, bad := range map[string][]byte{
		"past the end":      append(binary.BigEndian.AppendUint32(nil, 100), "moov"...),
		"smaller than head": append(binary.BigEndian.AppendUint32(nil, 4), "moov"...),
		"short 64-bit":      append(binary.BigEndian.AppendUint32(nil, 1), "moov1234"...),
		"too short":         []byte("moov"),
	} {
		if _, ok := child(bad, "moov"); ok {
			t.Errorf("%s: found", name)
		}
	}
}

func TestReadBoxHeader(t *testing.T) {
	r := bytes.NewReader(append(box("moof", []byte("abc")), append(binary.BigEndian.AppendUint32(nil, 0), "mdat"...)...))
	h, size, err := readBoxHeader(r)
	if err != nil || h.typ != "moof" || size != 3 || len(h.raw) != 8 {
		t.Fatalf("%+v %d %v", h, size, err)
	}
	body, err := readBody(r, h, size)
	if err != nil || string(body) != "abc" {
		t.Fatalf("%q %v", body, err)
	}
	if h, size, err = readBoxHeader(r); err != nil || h.typ != "mdat" || size != -1 {
		t.Fatalf("a box to the end: %+v %d %v", h, size, err)
	}
	if _, _, err = readBoxHeader(r); err != io.EOF {
		t.Fatalf("a clean end: %v", err)
	}
	large := binary.BigEndian.AppendUint64(append(binary.BigEndian.AppendUint32(nil, 1), "mdat"...), 26)
	if h, size, err := readBoxHeader(bytes.NewReader(large)); err != nil || size != 10 || len(h.raw) != 16 {
		t.Fatalf("a 64-bit size: %+v %d %v", h, size, err)
	}
	for name, bad := range map[string][]byte{
		"a short header":       []byte("abc"),
		"a short 64-bit size":  append(binary.BigEndian.AppendUint32(nil, 1), "mdat1234"...),
		"smaller than itself":  append(binary.BigEndian.AppendUint32(nil, 7), "mdat"...),
		"64-bit, smaller than": binary.BigEndian.AppendUint64(append(binary.BigEndian.AppendUint32(nil, 1), "mdat"...), 15),
	} {
		if _, _, err := readBoxHeader(bytes.NewReader(bad)); err == nil || err == io.EOF {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := readBody(bytes.NewReader([]byte("ab")), boxHeader{typ: "moov"}, 3); err == nil {
		t.Error("a short body read")
	}
	if _, err := readBody(bytes.NewReader(nil), boxHeader{typ: "mdat"}, maxBoxBuffer+1); err == nil {
		t.Error("a body past the bound read")
	}
	if _, err := readBody(bytes.NewReader(nil), boxHeader{typ: "mdat"}, -1); err == nil {
		t.Error("a body to the end held")
	}
}

// TestRouter_TheRemuxsOutputThatDoesNotRead: the reader's own refusals, which run no decoder.
func TestRouter_TheRemuxsOutputThatDoesNotRead(t *testing.T) {
	c := Checker{FFmpeg: "/nonexistent/ffmpeg"}
	mdhd := box("moov", box("trak", box("mdia", box("mdhd", append(make([]byte, 12), 0, 0, 0x03, 0xe8)))))
	tfdt := box("tfdt", make([]byte, 8))
	trun := box("trun", []byte{0, 0, 0, 1}, []byte{0, 0, 0, 24})
	for name, in := range map[string][]byte{
		"a moof before any moov":      box("moof", box("traf", tfdt, trun)),
		"a timescale of zero":         append(box("moov", box("trak", box("mdia", box("mdhd", make([]byte, 16))))), box("moof", box("traf", tfdt, trun))...),
		"a moof with no decode time":  append(mdhd, box("moof", box("traf", trun))...),
		"a moof with no sample count": append(mdhd, box("moof", box("traf", tfdt))...),
		"a box cut short":             box("ftyp")[:6],
		"an oversized init":           append(binary.BigEndian.AppendUint32(nil, maxBoxBuffer+16), "moov"...),
		"an init past the bound":      append(box("free", make([]byte, maxBoxBuffer-16)), box("free", make([]byte, 64))...),
		"an mdat cut short":           append(append(mdhd, box("moof", box("traf", tfdt, trun))...), box("mdat", []byte("abc"))[:9]...),
		"an oversized moof":           append(mdhd, append(binary.BigEndian.AppendUint32(nil, maxBoxBuffer+16), "moof"...)...),
	} {
		rt := &router{c: c, ctx: context.Background()}
		err := rt.read(bytes.NewReader(in))
		var d *decodeError
		if err == nil || errors.As(err, &d) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// With no fragment at all the reader decodes nothing and has nothing to refuse.
	rt := &router{c: c, ctx: context.Background(), segments: true}
	if err := rt.read(bytes.NewReader(box("ftyp"))); err != nil || rt.fragments != 0 {
		t.Errorf("no fragment: %d %v", rt.fragments, err)
	}
	// A decoder that cannot start is a decoder's verdict, and not a failure of the file.
	rt = &router{c: c, ctx: context.Background(), segments: true}
	err := rt.read(bytes.NewReader(append(mdhd, box("moof", box("traf", tfdt, trun))...)))
	var d *decodeError
	if !errors.As(err, &d) || rt.fragments != 1 || IsFailure(d.err) {
		t.Errorf("a decoder that cannot start: %d %v", rt.fragments, err)
	}
	// A fragment no check reaches is read past, and counted.
	rt = &router{c: c, ctx: context.Background()}
	in := append(append(mdhd, box("moof", box("traf", tfdt, trun))...), box("mdat", []byte("abc"))...)
	if err := rt.read(bytes.NewReader(in)); err != nil || rt.fragments != 1 || rt.next != 24 {
		t.Errorf("a fragment routed nowhere: %d %d %v", rt.fragments, rt.next, err)
	}
}

func TestDecodeFragments_NothingAskedForRunsNothing(t *testing.T) {
	c := Checker{FFmpeg: "/nonexistent/ffmpeg"}
	if err := c.decodeFragments(context.Background(), "/nonexistent", nil, 16, false); err != nil {
		t.Errorf("no stream and no segments: %v", err)
	}
	if err := c.decodeFragments(context.Background(), "/nonexistent", pkts([]int64{0, 1}, 0), 0, false); err != nil {
		t.Errorf("no cold start and no segments: %v", err)
	}
	err := c.decodeFragments(context.Background(), "/nonexistent", pkts([]int64{0, 1}), 4, false)
	if !IsFailure(err) || !strings.Contains(err.Error(), "flags no keyframe") {
		t.Errorf("a stream with no keyframe: %v", err)
	}
	if err := c.decodeFragments(context.Background(), "/nonexistent", nil, 0, true); IsFailure(err) || err == nil {
		t.Errorf("a remux that cannot start: %v", err)
	}
}
