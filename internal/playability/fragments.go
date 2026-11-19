package playability

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// windowSeconds is how far past its keyframe a cold start decodes, in decode time: the
// fragment its keyframe starts (the keyframe's whole group, with its leading pictures) and
// every fragment after it that starts within this window.
const windowSeconds = 3

// segmentSeconds is the shortest segment the segment check cuts: a segment starts at the
// first keyframe whose decode time is at least this far past the previous segment's start,
// which is how an HLS or DASH segmenter cuts a stream it copies (6 s is Apple's recommended
// HLS target duration).
const segmentSeconds = 6

// ColdStarts decodes file from a cold start at each of s.ColdStartTargets(n): a fresh decoder
// handed the stream from that keyframe, with nothing before it, for windowSeconds. Each must
// decode without an error and produce a picture. The first that does not is returned as a
// *Failure.
func (c Checker) ColdStarts(ctx context.Context, file string, s *Stream, n int) error {
	return c.decodeFragments(ctx, file, s, n, false)
}

// Segments cuts file's first video stream into segments of at least segmentSeconds at its
// keyframes and decodes each on its own: a fresh decoder handed the initialisation segment and
// that segment's fragments, exactly what a player handed one HLS or DASH segment after a seek
// decodes. Every segment must decode without an error and produce a picture.
func (c Checker) Segments(ctx context.Context, file string) error {
	return c.decodeFragments(ctx, file, nil, 0, true)
}

// decodeFragments runs the cold starts (where s is given and n is above 0) and the segments
// (where segments is set) over ONE remux of the stream.
//
// The remux copies file's first video stream, never re-encoding it, into fragmented MP4 on
// stdout, a fragment starting at every keyframe the container flags: an MP4 initialisation
// segment and then one fragment per group of pictures. It reads the file from its first
// byte to its last and seeks nothing, so it works on every container, including those
// with no keyframe index to seek by (MPEG-TS). Each fragment is routed as it arrives into
// every decoder whose range it falls in: the current segment's, and those of the cold starts
// whose window it starts inside. A decoder is never handed anything from before its first
// fragment, so each one starts cold at a keyframe. Nothing is written to disk.
func (c Checker) decodeFragments(ctx context.Context, file string, s *Stream, n int, segments bool) error {
	targets := map[int]bool{}
	if s != nil && n > 0 {
		keys := s.ColdStartTargets(n)
		if len(keys) == 0 {
			return &Failure{Check: "cold start", Reason: "the container flags no keyframe with a presentation " +
				"time, so a player has nowhere to start the video"}
		}
		for _, k := range keys {
			targets[k] = true
		}
	}
	if len(targets) == 0 && !segments {
		return nil
	}

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	remux := exec.CommandContext(cctx, c.FFmpeg, remuxArgs(file)...)
	var remuxErrs head
	remux.Stderr = &remuxErrs
	remux.WaitDelay = waitDelay
	stdout, err := remux.StdoutPipe()
	if err != nil {
		return &Unrun{What: "the remux into fragmented MP4", Err: err}
	}
	if err := remux.Start(); err != nil {
		return &Unrun{What: "the remux into fragmented MP4", Err: err}
	}
	rt := &router{c: c, ctx: cctx, s: s, targets: targets, segments: segments}
	readErr := rt.read(bufio.NewReaderSize(stdout, 1<<20))
	var decoded *decodeError
	if errors.As(readErr, &decoded) {
		// A decoder's own verdict ends the check; the remux has nothing more to say.
		cancel()
		_ = remux.Wait()
		return decoded.err
	}
	if readErr != nil {
		// Reading stopped early, and the remux may be blocked on a full pipe. One that had
		// already failed on its own keeps its exit status, which is read below first.
		cancel()
	}
	reason, unrun := verdict(ctx, "the remux into fragmented MP4", remux.Wait(), remuxErrs.String())
	switch {
	case reason != "":
		return &Failure{Check: "remux", Reason: "the video could not be remuxed into fragmented MP4, the shape " +
			"HLS and DASH stream it in: " + reason}
	case readErr != nil:
		return &Unrun{What: "the read of the fragmented MP4 the remux wrote", Err: readErr}
	case unrun != nil:
		return unrun
	case rt.fragments == 0:
		return &Failure{Check: "remux", Reason: "the remux into fragmented MP4 wrote no fragment"}
	case len(rt.targets) > 0:
		return &Unrun{What: "a cold start", Err: fmt.Errorf("%d keyframe(s) the cold starts were aimed at did "+
			"not start a fragment of the remux", len(rt.targets))}
	}
	return nil
}

// remuxArgs is the remux: the first video stream copied into fragmented MP4 on stdout, a
// fragment starting at every keyframe.
func remuxArgs(file string) []string {
	return []string{"-hide_banner", "-nostdin", "-v", "error", "-i", "file:" + file, "-map", "0:V:0", "-c", "copy",
		"-f", "mp4", "-movflags", "+frag_keyframe+empty_moov+default_base_moof", "pipe:1"}
}

// decoderArgs is one decoder: an initialisation segment and fragments on stdin, decoded with
// -xerror and -err_detect +explode, one -stats_enc_pre line written per decoded picture (the
// wrapped_avframe encoder costs nothing).
func decoderArgs() []string {
	return []string{"-hide_banner", "-nostdin", "-v", "error", "-xerror", "-err_detect", "+explode",
		"-f", "mp4", "-i", "pipe:0", "-map", "0:v:0", "-c:v", "wrapped_avframe",
		"-stats_enc_pre", "pipe:1", "-stats_enc_pre_fmt", "{n}", "-f", "null", "-"}
}

// decodeError carries a decoder's own verdict out of the reader, apart from the errors of
// reading the remux.
type decodeError struct{ err error }

func (d *decodeError) Error() string { return d.err.Error() }

// router reads the remux and feeds each decoder its range of fragments.
type router struct {
	c        Checker
	ctx      context.Context
	s        *Stream
	targets  map[int]bool // the cold starts not yet started, by decode index
	segments bool

	init      []byte
	timescale uint64
	fragments int
	next      int // the decode index of the next fragment's first picture

	segment      *decoder
	segmentStart uint64
	segmentCount int
	cold         []*decoder
}

// read reads the remux's top-level boxes: those before the first moof are the initialisation
// segment, and each moof starts a fragment that runs to the next. It returns a decoder's
// verdict as a *decodeError, and any other error as the remux's output not reading.
func (rt *router) read(r io.Reader) (err error) {
	defer func() {
		// Decoders still running when reading stops early are ended and waited for, so no
		// process outlives the check.
		if err != nil {
			for _, d := range rt.active() {
				d.abort()
			}
		}
	}()
	for {
		h, size, err := readBoxHeader(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.typ != "moof" && rt.fragments == 0 {
			body, err := readBody(r, h, size)
			if err != nil {
				return err
			}
			if rt.init = append(append(rt.init, h.raw...), body...); len(rt.init) > maxBoxBuffer {
				return fmt.Errorf("the initialisation segment exceeds %d bytes", maxBoxBuffer)
			}
			continue
		}
		if h.typ == "moof" {
			body, err := readBody(r, h, size)
			if err != nil {
				return err
			}
			if err := rt.fragment(append(h.raw, body...)); err != nil {
				return err
			}
			continue
		}
		// Any other box after the first moof (the fragment's mdat) streams straight into the
		// decoders the fragment was routed to.
		rt.write(h.raw)
		if size < 0 {
			_, err = io.Copy(rt, r)
		} else {
			_, err = io.CopyN(rt, r, size)
		}
		if err != nil {
			return fmt.Errorf("a %s box ends early: %w", h.typ, err)
		}
	}
	for _, d := range rt.active() {
		if err := d.finish(rt.ctx); err != nil {
			return &decodeError{err}
		}
	}
	rt.segment, rt.cold = nil, nil
	return nil
}

// fragment routes one fragment, starting at moof: it ends the decoders whose range ends
// before it, starts those whose range starts with it, and hands it to every decoder whose
// range it is in.
func (rt *router) fragment(moof []byte) error {
	if rt.fragments == 0 {
		ts, ok := mediaTimescale(rt.init)
		if !ok || ts == 0 {
			return fmt.Errorf("the initialisation segment carries no readable track timescale")
		}
		rt.timescale = uint64(ts)
	}
	at, ok := fragmentDecodeTime(moof)
	if !ok {
		return fmt.Errorf("fragment %d carries no readable decode time", rt.fragments+1)
	}
	samples, ok := fragmentSamples(moof)
	if !ok {
		return fmt.Errorf("fragment %d carries no readable sample count", rt.fragments+1)
	}
	first := rt.next
	rt.fragments++
	rt.next += samples

	// The cold starts whose window this fragment starts past.
	open := rt.cold[:0]
	for _, d := range rt.cold {
		if at-d.start >= windowSeconds*rt.timescale {
			if err := d.finish(rt.ctx); err != nil {
				return &decodeError{err}
			}
			continue
		}
		open = append(open, d)
	}
	rt.cold = open

	if rt.segments && (rt.segment == nil || at-rt.segmentStart >= segmentSeconds*rt.timescale) {
		if rt.segment != nil {
			done := rt.segment
			rt.segment = nil
			if err := done.finish(rt.ctx); err != nil {
				return &decodeError{err}
			}
		}
		rt.segmentStart = at
		rt.segmentCount++
		where := fmt.Sprintf("segment %d (decode time %s)", rt.segmentCount, seconds(float64(at)/float64(rt.timescale)))
		d, err := rt.c.startDecoder(rt.ctx, "segment", where, at, rt.init)
		if err != nil {
			return &decodeError{err}
		}
		rt.segment = d
	}
	if rt.targets[first] {
		delete(rt.targets, first)
		where := "the keyframe at " + seconds(rt.s.seconds(rt.s.Packets[first].PTS))
		d, err := rt.c.startDecoder(rt.ctx, "cold start", where, at, rt.init)
		if err != nil {
			return &decodeError{err}
		}
		rt.cold = append(rt.cold, d)
	}
	rt.write(moof)
	return nil
}

// active is every decoder running: the segment's and the cold starts'.
func (rt *router) active() []*decoder {
	ds := append([]*decoder(nil), rt.cold...)
	if rt.segment != nil {
		ds = append(ds, rt.segment)
	}
	return ds
}

// Write hands p to every decoder running, and never fails: see decoder.Write.
func (rt *router) Write(p []byte) (int, error) {
	rt.write(p)
	return len(p), nil
}

func (rt *router) write(p []byte) {
	for _, d := range rt.active() {
		_, _ = d.Write(p)
	}
}

// decoder is one cold start's or one segment's decoder, fed through its stdin.
type decoder struct {
	check, at string
	start     uint64 // the decode time of its first fragment, in the track's timescale
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	out       *bytes.Buffer
	errs      *head
	broken    bool
}

func (c Checker) startDecoder(ctx context.Context, check, at string, start uint64, init []byte) (*decoder, error) {
	cmd, out, errs := command(ctx, c.FFmpeg, decoderArgs()...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, &Unrun{What: "the decoder of " + at, Err: err}
	}
	if err := cmd.Start(); err != nil {
		return nil, &Unrun{What: "the decoder of " + at, Err: err}
	}
	d := &decoder{check: check, at: at, start: start, cmd: cmd, stdin: stdin, out: out, errs: errs}
	_, _ = d.Write(init)
	return d, nil
}

// Write feeds the decoder and never fails: a decoder that stopped reading (it exited on an
// error) is recorded, and the rest of its range is read from the remux and dropped, so the
// remux is never left blocked. finish reads the decoder's own verdict.
func (d *decoder) Write(p []byte) (int, error) {
	if !d.broken {
		if _, err := d.stdin.Write(p); err != nil {
			d.broken = true
		}
	}
	return len(p), nil
}

// finish ends the decoder's input and reads its verdict: no error printed, an exit of 0, and
// at least one picture decoded.
func (d *decoder) finish(ctx context.Context) error {
	_ = d.stdin.Close()
	reason, unrun := verdict(ctx, "the decoder of "+d.at, d.cmd.Wait(), d.errs.String())
	switch {
	case unrun != nil:
		return unrun
	case reason != "":
		return &Failure{Check: d.check, At: d.at, Reason: reason}
	case d.broken:
		return &Unrun{What: "the decoder of " + d.at, Err: errors.New("it exited cleanly before reading all of its input")}
	case strings.TrimSpace(d.out.String()) == "":
		return &Failure{Check: d.check, At: d.at, Reason: "decoded on its own, it produced no picture"}
	}
	return nil
}

// abort ends a decoder whose verdict is not wanted, and waits for it.
func (d *decoder) abort() {
	_ = d.stdin.Close()
	_ = d.cmd.Wait()
}
