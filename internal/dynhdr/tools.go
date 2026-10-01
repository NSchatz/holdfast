package dynhdr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// The environment variables that name the two tools' binaries, mirroring HOLDFAST_FFMPEG and
// HOLDFAST_FFPROBE: unset, each is looked up on PATH by its own name.
const (
	EnvDoviTool      = "HOLDFAST_DOVI_TOOL"
	EnvHDR10PlusTool = "HOLDFAST_HDR10PLUS_TOOL"
	DefaultDoviTool  = "dovi_tool"
	// DefaultHDR10PlusTool is hdr10plus_tool's own binary name.
	DefaultHDR10PlusTool = "hdr10plus_tool"
)

// Tools are the binaries the pre-passes and the gates run.
type Tools struct {
	FFmpeg, FFprobe     string
	DoviTool, HDR10Plus string
}

// ToolsFromEnv names the two tools from the environment, each defaulting to its own name on
// PATH; ffmpeg and ffprobe are the caller's.
func ToolsFromEnv(getenv func(string) string, ffmpeg, ffprobe string) Tools {
	or := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	return Tools{FFmpeg: ffmpeg, FFprobe: ffprobe,
		DoviTool: or(EnvDoviTool, DefaultDoviTool), HDR10Plus: or(EnvHDR10PlusTool, DefaultHDR10PlusTool)}
}

// Missing names the first tool the intent's pre-pass runs that cannot be found, and "" when
// every one it needs is there. A tool named by an empty string is missing.
func (t Tools) Missing(in Intent) string {
	need := []string{}
	if in.NeedsDoviTool() {
		need = append(need, t.DoviTool)
	}
	if in.NeedsHDR10PlusTool() {
		need = append(need, t.HDR10Plus)
	}
	for _, bin := range need {
		if bin == "" {
			return "(unnamed)"
		}
		if _, err := exec.LookPath(bin); err != nil {
			return bin
		}
	}
	return ""
}

// tail keeps the end of a tool's diagnostics for an error: the last lines are where a tool
// says why it stopped.
func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		s = "..." + s[len(s)-400:]
	}
	return s
}

// annexB is the ffmpeg invocation that writes src's first video stream as raw Annex B HEVC on
// its stdout: the input both tools read, in the form their READMEs give
// (https://github.com/quietvoid/dovi_tool/blob/2.3.4/README.md and
// https://github.com/quietvoid/hdr10plus_tool/blob/1.7.2/README.md , read 2026-10-01). The
// stream is copied, never decoded.
func annexB(ctx context.Context, ffmpeg, src string) *exec.Cmd {
	return exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error",
		"-i", src, "-map", "0:v:0", "-c", "copy", "-bsf:v", "hevc_mp4toannexb", "-f", "hevc", "-")
}

// pipeline runs producer | consumer and reports the first failure of either, with what it
// wrote to stderr. A consumer that stops reading early makes the producer fail on a broken
// pipe, so the consumer's failure is reported first: it is the cause.
func pipeline(producer, consumer *exec.Cmd) error {
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	var perr, cerr bytes.Buffer
	producer.Stdout, producer.Stderr = w, &perr
	consumer.Stdin, consumer.Stderr = r, &cerr
	consumer.Stdout = &cerr
	if err := consumer.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return fmt.Errorf("%s: %w", consumer.Path, err)
	}
	_ = r.Close()
	if err := producer.Start(); err != nil {
		_ = w.Close()
		_ = consumer.Wait()
		return fmt.Errorf("%s: %w", producer.Path, err)
	}
	_ = w.Close()
	pe := producer.Wait()
	ce := consumer.Wait()
	switch {
	case ce != nil:
		return fmt.Errorf("%s: %w: %s", consumer.Path, ce, tail(cerr.Bytes()))
	case pe != nil:
		return fmt.Errorf("%s: %w: %s", producer.Path, pe, tail(perr.Bytes()))
	}
	return nil
}

// run runs one command and returns its stdout, or an error carrying its stderr.
func run(cmd *exec.Cmd) ([]byte, error) {
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", cmd.Path, err, tail(errb.Bytes()))
	}
	return out.Bytes(), nil
}

// RefusalError is a pre-pass that could not complete: the source is skipped with Reason, and
// nothing was encoded.
type RefusalError struct {
	Reason string
	Err    error
}

func (e *RefusalError) Error() string { return e.Reason + ": " + e.Err.Error() }
func (e *RefusalError) Unwrap() error { return e.Err }

// refuse wraps err as a refusal under reason.
func refuse(reason string, err error) error { return &RefusalError{Reason: reason, Err: err} }

// AsRefusal reports the refusal err carries.
func AsRefusal(err error) (*RefusalError, bool) {
	var r *RefusalError
	ok := errors.As(err, &r)
	return r, ok
}
