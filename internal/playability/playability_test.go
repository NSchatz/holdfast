package playability

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/playability/playabilitytest"
	"github.com/NSchatz/holdfast/internal/probe"
)

// ---- the pure parts ----------------------------------------------------------

// The packets of a real open-GOP encode (playabilitytest.OpenGOPParams), in decode order: the
// keyframe at 1.000 is followed by two packets shown before it.
const openGOPPackets = `0.000000,K__
0.208000,___
0.125000,___
0.042000,___
0.833000,___
1.000000,K__
0.958000,___
0.917000,___
1.083000,___
2.000000,K__
2.083000,___
3.000000,K__
2.958000,___
`

func TestReadKeyframes_MarksTheKeyframesFollowedByLeadingPictures(t *testing.T) {
	kfs, firstKey, err := ReadKeyframes(strings.NewReader(openGOPPackets))
	if err != nil {
		t.Fatal(err)
	}
	want := []Keyframe{
		{Ordinal: 0, PTS: 0, Leading: false},
		{Ordinal: 1, PTS: 1, Leading: true},
		{Ordinal: 2, PTS: 2, Leading: false},
		{Ordinal: 3, PTS: 3, Leading: true},
	}
	if !firstKey || !reflect.DeepEqual(kfs, want) {
		t.Fatalf("got %+v (first key %v), want %+v", kfs, firstKey, want)
	}
}

func TestReadKeyframes_EdgeCases(t *testing.T) {
	// A packet whose pts equals its keyframe's is not a leading picture; one with no pts is
	// not compared; extra fields and blank lines are ignored.
	kfs, firstKey, err := ReadKeyframes(strings.NewReader("0.5,___\n\n1.0,K__,\n1.0,___\nN/A,___\n1.5,___\n"))
	if err != nil {
		t.Fatal(err)
	}
	if firstKey || len(kfs) != 1 || kfs[0].Leading || kfs[0].PTS != 1 {
		t.Fatalf("got %+v (first key %v)", kfs, firstKey)
	}
	for name, in := range map[string]string{
		"no flags":            "1.0\n",
		"an unreadable pts":   "x,K__\n",
		"a keyframe, no pts":  "N/A,K__\n",
		"a non-finite pts":    "NaN,___\n",
		"an infinite pts":     "+Inf,K__\n",
		"an unreadable later": "0,K__\nbad,___\n",
	} {
		if _, _, err := ReadKeyframes(strings.NewReader(in)); err == nil {
			t.Errorf("%s: %q was read without an error", name, in)
		}
	}
}

func TestSpread(t *testing.T) {
	for _, tc := range []struct {
		n, want int
		out     []int
	}{
		{0, 4, nil},
		{3, 0, nil},
		{3, 4, []int{0, 1, 2}},
		{4, 4, []int{0, 1, 2, 3}},
		{8, 4, []int{1, 3, 5, 7}},
		{10, 3, []int{1, 5, 8}},
	} {
		if got := spread(tc.n, tc.want); !reflect.DeepEqual(got, tc.out) {
			t.Errorf("spread(%d, %d) = %v, want %v", tc.n, tc.want, got, tc.out)
		}
	}
}

func keyframes(n int, leading func(int) bool) []Keyframe {
	out := make([]Keyframe, n)
	for i := range out {
		out[i] = Keyframe{Ordinal: i, PTS: float64(i), Leading: leading(i)}
	}
	return out
}

func ordinals(kfs []Keyframe) []int {
	var out []int
	for _, k := range kfs {
		out = append(out, k.Ordinal)
	}
	return out
}

func TestSample(t *testing.T) {
	none := func(int) bool { return false }
	if got := Sample(keyframes(1, func(int) bool { return true })); got != nil {
		t.Errorf("one keyframe sampled %v: the decode-integrity gate already started it cold", got)
	}
	if got := ordinals(Sample(keyframes(5, none))); !reflect.DeepEqual(got, []int{1, 2, 3, 4}) {
		t.Errorf("five keyframes sampled %v, want every one after the first", got)
	}

	// A long stream: SpreadSamples spread over it, and LeadingSamples more from among the
	// keyframes followed by leading pictures, none chosen twice.
	kfs := keyframes(2001, func(i int) bool { return i%7 == 3 })
	got := Sample(kfs)
	if len(got) != SpreadSamples+LeadingSamples {
		t.Fatalf("sampled %d keyframes, want %d", len(got), SpreadSamples+LeadingSamples)
	}
	seen, leading := map[int]bool{}, 0
	for i, k := range got {
		if seen[k.Ordinal] || k.Ordinal == 0 || (i > 0 && got[i-1].Ordinal >= k.Ordinal) {
			t.Fatalf("the sample is not distinct, in order and after the first keyframe: %v", ordinals(got))
		}
		seen[k.Ordinal] = true
		if k.Leading {
			leading++
		}
	}
	if leading < LeadingSamples {
		t.Errorf("the sample holds %d keyframes followed by leading pictures, want at least %d", leading, LeadingSamples)
	}
	if first, last := got[0].Ordinal, got[len(got)-1].Ordinal; first > 2001/SpreadSamples || last < 2000-2001/SpreadSamples {
		t.Errorf("the sample runs %d..%d and does not span the stream", first, last)
	}

	// Fewer leading keyframes than the cap: every one of them is sampled.
	few := keyframes(1000, func(i int) bool { return i == 500 || i == 501 })
	gotFew := Sample(few)
	if !seen500(gotFew) || len(gotFew) != SpreadSamples+2 {
		t.Errorf("sample %v: want the spread and both leading keyframes", ordinals(gotFew))
	}
}

func seen500(kfs []Keyframe) bool {
	a, b := false, false
	for _, k := range kfs {
		a = a || k.Ordinal == 500
		b = b || k.Ordinal == 501
	}
	return a && b
}

func TestSeekTarget(t *testing.T) {
	k := Keyframe{PTS: 10}
	if got := SeekTarget(k, 0, false); got != 10.001 {
		t.Errorf("no reorder delay: %v, want 10.001", got)
	}
	if got, want := SeekTarget(k, 0.5, true), 10-0.5+0.001+3.0/23.0; got != want {
		t.Errorf("with a reorder delay and a start time: %v, want %v", got, want)
	}
	if got := SeekTarget(Keyframe{PTS: -1}, 0, false); got != 0 {
		t.Errorf("a keyframe before the start: %v, want 0", got)
	}
}

func TestParseSeekBase(t *testing.T) {
	b, err := ParseSeekBase([]byte(`{"streams":[{"codec_type":"audio"},{"codec_type":"video","has_b_frames":2}],
		"format":{"start_time":"0.083000"}}`))
	if err != nil || b != (SeekBase{Start: 0.083, Delayed: true}) {
		t.Fatalf("got %+v, %v", b, err)
	}
	b, err = ParseSeekBase([]byte(`{"streams":[{"codec_type":"video","has_b_frames":0}],"format":{"start_time":"N/A"}}`))
	if err != nil || b != (SeekBase{}) {
		t.Fatalf("got %+v, %v", b, err)
	}
	for _, bad := range []string{`not json`, `{"format":{"start_time":"x"}}`} {
		var u *Unmeasured
		if _, err := ParseSeekBase([]byte(bad)); !errors.As(err, &u) {
			t.Errorf("%q: %v, want an Unmeasured", bad, err)
		}
	}
}

func TestFirstPacketTime(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want float64
		ok   bool
	}{
		{"#tb 0: 1/1000\n#media_type 0: video\n0,        917,       1000,       41,     4406, 0x5a76a1fc\n", 1, true},
		{"#tb 0: 1/16000\n0,      13328,      16000,      672,     4406, 0x5a76a1fc\n0, 1, 2, 3, 4, 5\n", 1, true},
		{"#tb 0: 1/1000\n0,        -83,          0,       41,     3642, 0xbdbb17e8\n", 0, true},
		{"", 0, false},
		{"#tb 0: 1/1000\n", 0, false},
		{"0, 1, 2, 3\n", 0, false},
		{"#tb 0: 1\n0, 1, 2\n", 0, false},
		{"#tb 0: 0/1000\n0, 1, 2\n", 0, false},
		{"#tb 0: 1/x\n0, 1, 2\n", 0, false},
		{"#tb 0: 1/1000\n0, 1\n", 0, false},
		{"#tb 0: 1/1000\n0, 1, x\n", 0, false},
	} {
		got, ok := FirstPacketTime(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("FirstPacketTime(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestReadBoxHeader(t *testing.T) {
	r := bytes.NewReader(append(
		[]byte{0, 0, 0, 12, 'f', 't', 'y', 'p', 1, 2, 3, 4},
		[]byte{0, 0, 0, 1, 'm', 'd', 'a', 't', 0, 0, 0, 0, 0, 0, 0, 18, 9, 9}...))
	h, err := ReadBoxHeader(r)
	if err != nil || h.Type != "ftyp" || h.Size != 4 || len(h.Raw) != 8 {
		t.Fatalf("got %+v, %v", h, err)
	}
	_, _ = io.CopyN(io.Discard, r, h.Size)
	h, err = ReadBoxHeader(r)
	if err != nil || h.Type != "mdat" || h.Size != 2 || len(h.Raw) != 16 {
		t.Fatalf("a 64-bit size: got %+v, %v", h, err)
	}
	_, _ = io.CopyN(io.Discard, r, h.Size)
	if _, err := ReadBoxHeader(r); err != io.EOF {
		t.Fatalf("a clean end: %v, want io.EOF", err)
	}
	for name, in := range map[string][]byte{
		"to the end":          {0, 0, 0, 0, 'm', 'd', 'a', 't'},
		"smaller than itself": {0, 0, 0, 7, 'm', 'o', 'o', 'f'},
		"a truncated header":  {0, 0, 0},
		"a truncated large":   {0, 0, 0, 1, 'm', 'd', 'a', 't', 0, 0},
		"a large too small":   {0, 0, 0, 1, 'm', 'd', 'a', 't', 0, 0, 0, 0, 0, 0, 0, 15},
		"a large too large":   {0, 0, 0, 1, 'm', 'd', 'a', 't', 0x80, 0, 0, 0, 0, 0, 0, 0},
	} {
		if _, err := ReadBoxHeader(bytes.NewReader(in)); err == nil || err == io.EOF {
			t.Errorf("%s: %v, want an error", name, err)
		}
	}
}

func TestErrors_SayWhereAndWhat(t *testing.T) {
	k := Keyframe{Ordinal: 7, PTS: 12.5, Leading: true}
	for stage, want := range map[string]string{
		StageColdStart: "a decode started cold at keyframe 7 of 40 (at 12.500s, followed by leading pictures) failed (POC 3)",
		StageHardware:  "a hardware decode started cold at keyframe 7 of 40",
		StageFragment:  "the fragment that starts at keyframe 7 of 40 (at 12.500s, followed by leading pictures) of the output remuxed to fragmented MP4",
	} {
		got := (&Rejection{Stage: stage, Keyframe: k, Keyframes: 40, Detail: "POC 3"}).Error()
		if !strings.Contains(got, want) || !strings.Contains(got, "cannot decode it") {
			t.Errorf("%s: %q does not say %q", stage, got, want)
		}
	}
	if got := (&Rejection{Stage: StageColdStart, Keyframe: Keyframe{Ordinal: 1, PTS: 1}, Keyframes: 2}).Error(); strings.Contains(got, "leading") {
		t.Errorf("%q names leading pictures the keyframe does not have", got)
	}
	inner := errors.New("boom")
	u := &Unmeasured{What: "listed", Err: inner}
	if u.Error() != "listed: boom" || !errors.Is(u, inner) || (&Unmeasured{What: "x"}).Error() != "x" {
		t.Errorf("Unmeasured renders %q", u.Error())
	}
}

func TestCapped(t *testing.T) {
	var c capped
	n, err := c.Write(bytes.Repeat([]byte("a"), stderrCap-1))
	if n != stderrCap-1 || err != nil {
		t.Fatal(n, err)
	}
	if n, _ := c.Write([]byte("bcd")); n != 3 || c.String() != strings.Repeat("a", stderrCap-1)+"b" {
		t.Fatalf("kept %d bytes", len(c.String()))
	}
	if n, _ := c.Write([]byte("e")); n != 1 || len(c.String()) != stderrCap {
		t.Fatal("wrote past the cap")
	}
	var empty capped
	if got := detail(errors.New("exit status 1"), &empty); got != "exit status 1" {
		t.Errorf("detail with no stderr = %q", got)
	}
	var some capped
	_, _ = some.Write([]byte("\n  first line \nsecond\n"))
	if got := detail(errors.New("exit status 1"), &some); got != "first line" {
		t.Errorf("detail = %q", got)
	}
}

func TestArgs(t *testing.T) {
	got := strings.Join(ColdStartArgs([]string{"-hwaccel", "cuda"}, "/o.mkv", 2, 1.5), " ")
	want := "-hide_banner -nostdin -v error -hwaccel cuda -xerror -err_detect +explode -ss 1.500000 -noaccurate_seek " +
		"-copyts -i /o.mkv -map 0:2 -frames:v 48 -f null - -map 0:2 -c copy -frames:v 1 -f framecrc -"
	if got != want {
		t.Errorf("ColdStartArgs:\n got %s\nwant %s", got, want)
	}
	if got := strings.Join(RemuxArgs("/o.mkv", 0), " "); got != "-hide_banner -nostdin -v error -i /o.mkv -map 0:0 -c copy -f mp4 "+
		"-movflags frag_keyframe+empty_moov+default_base_moof -" {
		t.Errorf("RemuxArgs: %s", got)
	}
	if got := strings.Join(PacketArgs("/o.mkv", 1), " "); got != "-v error -select_streams 1 -show_entries packet=pts_time,flags -of csv=p=0 -- /o.mkv" {
		t.Errorf("PacketArgs: %s", got)
	}
	if got := strings.Join(HardwareOpenArgs([]string{"-hwaccel", "cuda"}, "/o.mkv", 0), " "); got != "-hide_banner -nostdin -v error "+
		"-hwaccel cuda -xerror -err_detect +explode -i /o.mkv -map 0:0 -frames:v 48 -f null -" {
		t.Errorf("HardwareOpenArgs: %s", got)
	}
	if got := strings.Join(FragmentArgs(), " "); got != "-hide_banner -v error -xerror -err_detect +explode -f mp4 -i pipe:0 "+
		"-map 0:v:0 -frames:v 250 -f null -" {
		t.Errorf("FragmentArgs: %s", got)
	}
}

// ---- the real fixtures -------------------------------------------------------

// tools finds ffmpeg and ffprobe, failing loud rather than skipping: a skipped safety proof is a
// false green (docs/development.md).
func tools(t *testing.T) (string, string) {
	t.Helper()
	ffmpeg, ffprobe := os.Getenv("HOLDFAST_FFMPEG"), os.Getenv("HOLDFAST_FFPROBE")
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
	for _, b := range []string{ffmpeg, ffprobe} {
		if _, err := exec.LookPath(b); err != nil {
			t.Fatalf("::error:: %q not found - the playability fixtures require the pinned ffmpeg (set HOLDFAST_FFMPEG/FFPROBE): %v", b, err)
		}
	}
	return ffmpeg, ffprobe
}

// The fixtures are built once per test binary: each is a real encode, and every case below reads
// them and writes nothing.
var (
	fixtureOnce sync.Once
	fixtureDir  string
	fixtureErr  error
)

type fixtures struct {
	open, openMP4, closed, noB, broken string
	relabelled                         int
}

func fixture(t *testing.T) fixtures {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	fixtureOnce.Do(func() {
		fixtureDir, fixtureErr = os.MkdirTemp("", "holdfast-playability-")
		if fixtureErr != nil {
			return
		}
		for name, params := range map[string]string{
			"open.mkv":   playabilitytest.OpenGOPParams,
			"open.mp4":   playabilitytest.OpenGOPParams,
			"closed.mkv": playabilitytest.ClosedGOPParams,
			"nob.mkv":    playabilitytest.NoBFramesParams,
		} {
			if fixtureErr = playabilitytest.Encode(ffmpeg, filepath.Join(fixtureDir, name), params); fixtureErr != nil {
				return
			}
		}
		var n int
		n, fixtureErr = playabilitytest.Mislabel(ffmpeg, ffprobe, filepath.Join(fixtureDir, "open.mkv"), filepath.Join(fixtureDir, "broken.mkv"), "matroska")
		if fixtureErr == nil {
			fixtureErr = os.WriteFile(filepath.Join(fixtureDir, "relabelled"), []byte(strings.Repeat("x", n)), 0o644)
		}
	})
	if fixtureErr != nil {
		t.Fatalf("building the fixtures: %v", fixtureErr)
	}
	b, err := os.ReadFile(filepath.Join(fixtureDir, "relabelled"))
	if err != nil {
		t.Fatal(err)
	}
	return fixtures{
		open: filepath.Join(fixtureDir, "open.mkv"), openMP4: filepath.Join(fixtureDir, "open.mp4"),
		closed: filepath.Join(fixtureDir, "closed.mkv"), noB: filepath.Join(fixtureDir, "nob.mkv"),
		broken: filepath.Join(fixtureDir, "broken.mkv"), relabelled: len(b),
	}
}

func TestMain(m *testing.M) {
	code := m.Run()
	if fixtureDir != "" {
		_ = os.RemoveAll(fixtureDir)
	}
	os.Exit(code)
}

func checker(t *testing.T) Checker {
	ffmpeg, ffprobe := tools(t)
	return Checker{FFmpeg: ffmpeg, FFprobe: ffprobe}
}

// A VALID stream passes, open GOP included: a decoder starting at a CRA drops its RASL pictures,
// as the standard requires, and the gate must not refuse a file every conforming player plays.
// Every sampled seek is proved to have landed on its keyframe (coldStart), in Matroska and MP4,
// with a reorder delay and without one.
func TestCheck_ValidStreamsPass(t *testing.T) {
	fx := fixture(t)
	c := checker(t)
	for name, f := range map[string]string{
		"open GOP, Matroska": fx.open, "open GOP, MP4": fx.openMP4,
		"closed GOP": fx.closed, "no B-frames": fx.noB,
	} {
		t.Run(name, func(t *testing.T) {
			rep, err := c.Check(context.Background(), f, 0)
			if err != nil {
				t.Fatalf("a valid stream was refused: %v", err)
			}
			if rep.Keyframes != 10 || len(rep.Sampled) != 9 || rep.Hardware {
				t.Fatalf("report %+v: want 10 keyframes, the 9 after the first sampled, no hardware", rep)
			}
			if strings.Contains(name, "open") && rep.Leading != 9 {
				t.Errorf("the open-GOP fixture reports %d keyframes with leading pictures, want 9: the case "+
					"would not exercise the RASL drop", rep.Leading)
			}
		})
	}
}

// THE PROOF (issue #200): an output whose leading pictures cannot be decoded from a cold start at
// their keyframe passes the decode-integrity gate - one linear decode under -xerror and
// -err_detect +explode, exactly as the engine runs it - and fails this one.
func TestCheck_LeadingPicturesThatNeedAPriorPictureAreRefused(t *testing.T) {
	fx := fixture(t)
	if fx.relabelled == 0 {
		t.Fatal("the fixture relabelled no RASL picture: the case would prove nothing")
	}
	ffmpeg, ffprobe := tools(t)
	if !probe.New(ffmpeg, ffprobe).DecodeOK(context.Background(), fx.broken) {
		t.Fatal("the decode-integrity gate refuses the fixture: it must pass every gate but this one")
	}
	_, err := checker(t).Check(context.Background(), fx.broken, 0)
	var rej *Rejection
	if !errors.As(err, &rej) {
		t.Fatalf("the gate returned %v, want a Rejection", err)
	}
	if rej.Stage != StageColdStart || rej.Keyframe.Ordinal != 1 || !rej.Keyframe.Leading || rej.Keyframes != 10 ||
		!strings.Contains(rej.Detail, "ref") {
		t.Errorf("rejection %+v: want the cold start at keyframe 1 of 10, followed by leading pictures, "+
			"failing on a missing reference", rej)
	}
}

// The fragment stage refuses the same fixture on its own: each sampled fragment of the
// fragmented-MP4 remux, decoded with only the init segment before it, fails where the valid
// encode's do not.
func TestFragments_EachDecodedOnItsOwn(t *testing.T) {
	fx := fixture(t)
	c := checker(t)
	ctx := context.Background()
	for _, tc := range []struct {
		file   string
		refuse bool
	}{{fx.open, false}, {fx.closed, false}, {fx.broken, true}} {
		kfs, firstKey, err := c.keyframes(ctx, tc.file, 0)
		if err != nil {
			t.Fatal(err)
		}
		err = c.fragments(ctx, tc.file, 0, kfs, firstKey, Sample(kfs))
		var rej *Rejection
		switch {
		case tc.refuse && (!errors.As(err, &rej) || rej.Stage != StageFragment || rej.Keyframe.Ordinal != 1):
			t.Errorf("%s: %v, want the fragment at keyframe 1 refused", tc.file, err)
		case !tc.refuse && err != nil:
			t.Errorf("%s: a valid stream's fragments were refused: %v", tc.file, err)
		}
	}

	// A sample naming a fragment past the end proves every fragment was counted against the
	// keyframes: the same stream with one keyframe fewer listed than it has is refused as
	// unmeasured, never passed.
	kfs, firstKey, err := c.keyframes(ctx, fx.open, 0)
	if err != nil {
		t.Fatal(err)
	}
	var u *Unmeasured
	if err := c.fragments(ctx, fx.open, 0, kfs[:len(kfs)-1], firstKey, Sample(kfs[:len(kfs)-1])); !errors.As(err, &u) {
		t.Errorf("a fragment count that does not match the keyframes: %v, want Unmeasured", err)
	}
	// Not opening on a keyframe shifts every fragment by one: the count is then one short.
	if err := c.fragments(ctx, fx.open, 0, kfs, false, Sample(kfs)); !errors.As(err, &u) {
		t.Errorf("a stream said not to open on a keyframe: %v, want Unmeasured", err)
	}
	if err := c.fragments(ctx, fx.open, 0, kfs, firstKey, nil); err != nil {
		t.Errorf("an empty sample: %v", err)
	}
}

// The landing check is not vacuous: a seek computed without the reorder-delay compensation lands
// on the keyframe before the one it was aimed at, and is refused as unmeasured rather than
// counted as a cold start at the keyframe it never reached.
func TestColdStart_ASeekThatDoesNotLandIsUnmeasured(t *testing.T) {
	fx := fixture(t)
	c := checker(t)
	k := Keyframe{Ordinal: 2, PTS: 2}
	var u *Unmeasured
	err := c.coldStart(context.Background(), c.FFmpeg, nil, StageColdStart, fx.open, 0, k, 10, SeekBase{})
	if !errors.As(err, &u) || !strings.Contains(err.Error(), "landed at 1.000000s") {
		t.Fatalf("an uncompensated seek: %v, want Unmeasured naming where it landed", err)
	}
	if err := c.coldStart(context.Background(), c.FFmpeg, nil, StageColdStart, fx.open, 0, k, 10, SeekBase{Delayed: true}); err != nil {
		t.Fatalf("the compensated seek: %v", err)
	}
}

// stub writes a shell script standing in for ffmpeg: body runs first, then the real binary.
func stub(t *testing.T, real, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\nexec "+real+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// The hardware stage: its options reach the opening decode and every cold start, a device that
// cannot decode the opening frames is unmeasured rather than a verdict on the output, and a cold
// start that fails on the hardware alone is a rejection of its own stage.
func TestCheck_Hardware(t *testing.T) {
	fx := fixture(t)
	ffmpeg, _ := tools(t)
	real, err := exec.LookPath(ffmpeg)
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "hw.log")
	c := checker(t)
	c.Hardware = []string{"-hwaccel", "none"}

	c.HardwareFFmpeg = stub(t, real, `printf '%s\n' "$*" >> `+log)
	rep, err := c.Check(context.Background(), fx.open, 0)
	if err != nil || !rep.Hardware {
		t.Fatalf("a valid stream on a working decoder: %+v, %v", rep, err)
	}
	b, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1+len(rep.Sampled) || !strings.Contains(lines[0], "-hwaccel none") || strings.Contains(lines[0], "-ss") {
		t.Fatalf("the hardware binary ran %d command(s), want the opening decode and %d cold starts:\n%s",
			len(lines), len(rep.Sampled), b)
	}
	for _, l := range lines[1:] {
		if !strings.Contains(l, "-hwaccel none") || !strings.Contains(l, "-noaccurate_seek") {
			t.Errorf("a hardware cold start without the hardware options or the seek: %s", l)
		}
	}

	c.HardwareFFmpeg = stub(t, real, "exit 1")
	var u *Unmeasured
	if _, err := c.Check(context.Background(), fx.open, 0); !errors.As(err, &u) || !strings.Contains(err.Error(), "opening frames") {
		t.Errorf("a decoder that cannot open the output: %v, want Unmeasured", err)
	}

	c.HardwareFFmpeg = stub(t, real, `for a in "$@"; do [ "$a" = -ss ] && { echo "hw decode error" >&2; exit 1; }; done`)
	var rej *Rejection
	if _, err := c.Check(context.Background(), fx.open, 0); !errors.As(err, &rej) || rej.Stage != StageHardware ||
		rej.Detail != "hw decode error" || rej.Keyframe.Ordinal != 1 {
		t.Errorf("a hardware cold start that fails: %v, want a hardware Rejection at keyframe 1", err)
	}

	c.Hardware = nil
	c.HardwareFFmpeg = stub(t, real, "exit 1")
	if rep, err := c.Check(context.Background(), fx.open, 0); err != nil || rep.Hardware {
		t.Errorf("no hardware configured: %+v, %v - the hardware binary must not run", rep, err)
	}
}

// Every way the measurement can fail to be taken is Unmeasured, never a pass.
func TestCheck_UnmeasuredNeverPasses(t *testing.T) {
	fx := fixture(t)
	ffmpeg, _ := tools(t)
	real, err := exec.LookPath(ffmpeg)
	if err != nil {
		t.Fatal(err)
	}
	var u *Unmeasured
	c := checker(t)
	if _, err := c.Check(context.Background(), filepath.Join(t.TempDir(), "missing.mkv"), 0); !errors.As(err, &u) {
		t.Errorf("a missing file: %v, want Unmeasured", err)
	}
	if _, err := c.Check(context.Background(), fx.open, 5); !errors.As(err, &u) {
		t.Errorf("a stream that is not there: %v, want Unmeasured", err)
	}
	noProbe := c
	noProbe.FFprobe = filepath.Join(t.TempDir(), "no-ffprobe")
	if _, err := noProbe.Check(context.Background(), fx.open, 0); !errors.As(err, &u) {
		t.Errorf("no ffprobe: %v, want Unmeasured", err)
	}
	// A remux that fails after the cold starts passed.
	remuxFails := c
	remuxFails.FFmpeg = stub(t, real, `for a in "$@"; do [ "$a" = -movflags ] && { echo "remux failed" >&2; exit 1; }; done`)
	if _, err := remuxFails.Check(context.Background(), fx.open, 0); !errors.As(err, &u) || !strings.Contains(err.Error(), "remux") {
		t.Errorf("a remux that fails: %v, want Unmeasured", err)
	}
	// A cold start that prints no packet cannot say where it landed.
	silent := c
	silent.FFmpeg = stub(t, real, `for a in "$@"; do [ "$a" = -noaccurate_seek ] && exit 0; done`)
	if _, err := silent.Check(context.Background(), fx.open, 0); !errors.As(err, &u) || !strings.Contains(err.Error(), "read no packet") {
		t.Errorf("a cold start with no landing: %v, want Unmeasured", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Check(ctx, fx.open, 0); !errors.As(err, &u) {
		t.Errorf("a cancelled context: %v, want Unmeasured", err)
	}
}
