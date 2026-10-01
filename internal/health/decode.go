package health

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode"
)

// ErrDecoderUnavailable is a decoder that could not be run at all. It stops the sweep
// rather than recording every file as failing a decode nobody ran.
var ErrDecoderUnavailable = errors.New("the decoder could not be run")

// FFmpeg is the production Decoder: the same full decode `holdfast analyze --health` and
// the engine's decode-integrity gate run (probe.Prober.DecodeOK) - every video stream to
// the null muxer, `-xerror` with `-err_detect +explode` so a concealable error fails it -
// with ffmpeg's error output kept as the reason. One reading is stricter than the gate's:
// any error ffmpeg prints fails the file, even when it exits 0.
//
// It writes nothing: the output is the null muxer and `-nostdin` keeps it off the
// terminal. The input is named through the `file:` protocol so a path is always read as a
// path and never as some other protocol's URL.
type FFmpeg struct {
	Bin string
}

// Args is the argument vector one decode of path runs.
func (f FFmpeg) Args(path string) []string {
	return []string{"-hide_banner", "-nostdin", "-v", "error", "-xerror", "-err_detect", "+explode",
		"-i", "file:" + path, "-map", "0:v", "-f", "null", "-"}
}

// Decode implements Decoder.
func (f FFmpeg) Decode(ctx context.Context, path string) (bool, string, error) {
	cmd := exec.CommandContext(ctx, f.Bin, f.Args(path)...)
	var stderr tail
	cmd.Stderr = &stderr
	// A cancelled decode is killed, and Wait then waits at most this long for its output
	// to close, so a child it left behind cannot hold the daemon's shutdown.
	cmd.WaitDelay = waitDelay
	if err := cmd.Start(); err != nil {
		return false, "", fmt.Errorf("%w: %s: %v", ErrDecoderUnavailable, f.Bin, err)
	}
	lowerPriority(cmd.Process.Pid)
	err := cmd.Wait()
	if ctx.Err() != nil {
		return false, "", ctx.Err()
	}
	if err == nil {
		// A clean exit that still printed an error is NOT a clean decode. At `-v error`
		// ffmpeg prints only errors, and a truncated file is exactly this case: it decodes
		// to where the bytes stop, says "File ended prematurely" and exits 0. The
		// encode-side gate has its own length checks for that; a sweep has nothing else,
		// so here the error is the verdict.
		if out := stderr.String(); strings.TrimSpace(out) != "" {
			return false, reasonOf(out, 0), nil
		}
		return true, "", nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false, "", fmt.Errorf("%w: %s: %v", ErrDecoderUnavailable, f.Bin, err)
	}
	return false, reasonOf(stderr.String(), exit.ExitCode()), nil
}

// waitDelay bounds how long a killed decode's output may stay open (exec.Cmd.WaitDelay).
const waitDelay = 2 * time.Second

// maxReason bounds a recorded reason. ffmpeg can print an error per frame, and the ledger
// row, the read surface and the notification all carry the reason.
const maxReason = 300

// reasonOf is the last line ffmpeg wrote, which is the error it stopped on, made
// printable and bounded; or the exit status when it wrote nothing.
func reasonOf(stderr string, code int) string {
	var last string
	for _, line := range strings.Split(stderr, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			last = line
		}
	}
	if last == "" {
		return fmt.Sprintf("ffmpeg exited with status %d and printed no error", code)
	}
	last = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return '?'
	}, last)
	if r := []rune(last); len(r) > maxReason {
		last = string(r[:maxReason]) + "..."
	}
	return last
}

// maxTail is how much of ffmpeg's error output is kept. Only the last line is reported.
const maxTail = 4096

// tail keeps the last maxTail bytes written to it.
type tail struct{ b []byte }

func (t *tail) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if over := len(t.b) - maxTail; over > 0 {
		t.b = bytes.Clone(t.b[over:])
	}
	return len(p), nil
}

func (t *tail) String() string { return string(t.b) }
