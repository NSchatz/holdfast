package playability

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// waitDelay bounds how long a killed process's pipes may stay open (exec.Cmd.WaitDelay), so a
// child it left behind cannot hold a cancelled check.
const waitDelay = 2 * time.Second

// command is one ffmpeg or ffprobe process: stdout kept whole (the checks' answers are a
// line per packet or per frame), stderr kept to its head.
func command(ctx context.Context, bin string, args ...string) (*exec.Cmd, *bytes.Buffer, *head) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var out bytes.Buffer
	var errs head
	cmd.Stdout, cmd.Stderr = &out, &errs
	cmd.WaitDelay = waitDelay
	return cmd, &out, &errs
}

// verdict reads how a process ended. what names the check for an Unrun. It returns an
// *Unrun where the process did not run to an exit of its own (it did not start, it was
// killed by a signal, or ctx was cancelled) or exited non-zero without printing an error,
// and otherwise the reason it failed: its first error line where it exited non-zero or exited
// 0 having printed an error, "" where it exited 0 and printed nothing.
//
// A non-zero exit with nothing printed says nothing about the file: ffmpeg catches SIGTERM and
// SIGINT and exits 255 having printed nothing at -v error, and a binary that dies before it
// reads its input does the same. Only an error the process printed is a verdict.
//
// An error printed beside a clean exit fails it. At -v error ffmpeg prints only errors,
// and a decoder that conceals a broken reference ("Could not find ref with POC") still
// exits 0 without -xerror catching it on every path; the health sweep's decode reads its
// output the same way.
func verdict(ctx context.Context, what string, err error, stderr string) (string, error) {
	if ctx.Err() != nil {
		return "", &Unrun{What: what, Err: ctx.Err()}
	}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || !exit.Exited() {
			return "", &Unrun{What: what, Err: err}
		}
		if strings.TrimSpace(stderr) == "" {
			return "", &Unrun{What: what, Err: fmt.Errorf("it exited with status %d and printed no error", exit.ExitCode())}
		}
		return reasonOf(stderr), nil
	}
	if strings.TrimSpace(stderr) != "" {
		return reasonOf(stderr), nil
	}
	return "", nil
}

// maxReason bounds a reason: ffmpeg can print an error per frame, and the reason reaches the
// ledger row and every surface that reads it.
const maxReason = 300

// reasonOf is the first line a process printed, made printable and bounded; verdict calls it
// only where the process printed one. The first line is the cause: a decoder that cannot find a
// reference says so first, and what ffmpeg prints after it is the run winding down ("Task
// finished with error", "Nothing was written into output file").
func reasonOf(stderr string) string {
	var first string
	for _, line := range strings.Split(stderr, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			first = line
			break
		}
	}
	// ffmpeg names the component that printed a line with its address in memory, which
	// differs on every run and says nothing to a reader: "[hevc @ 0x55d0c1a2b3c0]" is
	// "[hevc]".
	first = address.ReplaceAllString(first, "")
	first = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return '?'
	}, first)
	if r := []rune(first); len(r) > maxReason {
		first = string(r[:maxReason]) + "..."
	}
	return first
}

// address is a component's address in an ffmpeg log line.
var address = regexp.MustCompile(` @ 0x[0-9a-fA-F]+`)

// maxHead is how much of a process's error output is kept; only its first line is reported.
const maxHead = 4096

// head keeps the first maxHead bytes written to it and drops the rest.
type head struct{ b []byte }

func (h *head) Write(p []byte) (int, error) {
	if room := maxHead - len(h.b); room > 0 {
		h.b = append(h.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (h *head) String() string { return string(h.b) }
