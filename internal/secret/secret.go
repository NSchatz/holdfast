// Package secret reaches a credential BY REFERENCE and resolves it at the point of
// use (secrets K1). Configuration carries the NAME of a secret - where it lives and
// what it is called - and never the value, so a plaintext credential cannot be read
// out of config.yaml, printed by `validate`, logged at debug, or inherited by an
// ffmpeg child through the environment.
//
// Two reference kinds, and deliberately no third:
//
//	file:/run/secrets/holdfast-token   the file's contents are the value
//	cmd:/usr/local/bin/fetch-token     the command's STDOUT is the value
//
// There is no `env:` kind. A value in holdfast's own environment is inherited by every
// child process it starts, which is the exact leak AC-3 forbids and the reason a literal
// in HOLDFAST_* is refused too: leaving that route open would make the invariant
// unprovable. An operator who must source a secret from the environment writes a `cmd:`
// resolver and owns that choice visibly.
//
// A resolver's stdout is THE SECRET CHANNEL. Nothing in this package writes it anywhere
// but into a Value, nothing reports it on a failure path, and a resolver's stderr is
// discarded unread rather than quoted into a diagnostic - a resolver that fails after
// printing the credential is the common shape, and "resolver said: ..." discloses it.
package secret

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ResolverTimeout is THE BOUND this repository documents for a `cmd:` resolver: a
// resolver that has not terminated within it is abandoned, killed, and reported as a
// timeout (distinctly from a resolver that exited non-zero).
//
// It is single-sourced here and docs/secrets.md must state the same number;
// TestResolverTimeout_IsTheBoundTheDocumentationStates holds the two in step, because a
// bound stated in two places is a bound that drifts. Five seconds is chosen to be longer
// than a local file read or a unix-socket agent round trip and far shorter than the
// startup a human is waiting on.
const ResolverTimeout = 5 * time.Second

// maxResolverOutput caps what a resolver may write to stdout. A credential is small; an
// unbounded read lets a broken resolver exhaust memory at startup.
const maxResolverOutput = 64 << 10

// waitDelay bounds how long we wait for a killed resolver's pipes to close before
// abandoning its output. It is not a second timeout: ResolverTimeout is the bound, and
// this only covers the window between the kill and the kernel reaping the group.
const waitDelay = time.Second

// Redacted is what a Value renders as through every formatting route there is.
const Redacted = "<redacted>"

// Reference kinds. The set is closed: anything else is refused as a literal.
const (
	KindFile = "file"
	KindCmd  = "cmd"
)

// Value is a resolved credential. Every way of rendering it - fmt, slog, JSON, text -
// yields Redacted, so a value cannot reach a log line, an HTTP body or a metric label by
// being passed to something that formats its arguments. Expose is the one accessor that
// returns the plaintext, and it is named so that a reviewer grepping for it sees every
// site that reads a credential.
type Value struct{ b []byte }

// NewValue wraps a plaintext credential. It exists for the consumers' own tests; the
// resolver is the only production producer.
func NewValue(s string) Value { return Value{b: []byte(s)} }

// Expose returns the plaintext. THE ONLY ROUTE TO IT.
func (v Value) Expose() string { return string(v.b) }

// Empty reports whether there is no credential here (an unconfigured key).
func (v Value) Empty() bool { return len(v.b) == 0 }

func (v Value) String() string   { return Redacted }
func (v Value) GoString() string { return Redacted }

// Format covers every fmt verb, including %v and %s on a struct that embeds a Value.
func (v Value) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

// LogValue covers slog at every level, including debug.
func (v Value) LogValue() slog.Value { return slog.StringValue(Redacted) }

// MarshalJSON covers an HTTP response body or a ledger row built by encoding/json.
func (v Value) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }

// MarshalText covers encoding/text marshalling, and YAML via it.
func (v Value) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// Ref is a parsed reference: the configuration key it came from, the kind of place the
// value lives, and the locator within it. It carries no credential, so its String is
// safe to print and is what every failure path names.
type Ref struct {
	key     string
	kind    string
	locator string
}

// Key is the configuration key this reference was written against.
func (r Ref) Key() string { return r.key }

// Kind is KindFile or KindCmd, or "" when the key is not configured.
func (r Ref) Kind() string { return r.kind }

// Configured reports whether the key carries a reference at all. An absent key leaves
// its feature disabled, which is the shipped default for all three secret-bearing keys.
func (r Ref) Configured() bool { return r.kind != "" }

// String is the reference AS WRITTEN, which by construction identifies the secret
// without disclosing it. Never a value.
func (r Ref) String() string {
	if !r.Configured() {
		return ""
	}
	return r.kind + ":" + r.locator
}

// ErrLiteral is the refusal for a secret-bearing key that carries something other than a
// reference. It names the key and how to convert it and NEVER any part of the value -
// including its length, which is itself a disclosure about a credential.
type ErrLiteral struct{ Key string }

func (e *ErrLiteral) Error() string {
	return fmt.Sprintf("%s does not carry a secret reference. holdfast never accepts a credential "+
		"as a literal value, in this file or in HOLDFAST_%s, because a literal in the environment is "+
		"inherited by every ffmpeg child it starts. Write one of:\n"+
		"    %s: file:/run/secrets/%s      the file's contents are the value\n"+
		"    %s: cmd:/path/to/fetch-secret  the command's stdout is the value\n"+
		"then put the credential in that file (mode 0400) or have that command print it. "+
		"The offending value is deliberately not echoed here. See docs/secrets.md",
		e.Key, strings.ToUpper(e.Key), e.Key, e.Key, e.Key)
}

// Unresolvable is the refusal for a reference the resolver could not produce a value
// for: a missing or empty file, a resolver that could not be started. It names the key
// and the reference and carries no candidate value.
type Unresolvable struct {
	Ref    Ref
	Detail string
	Err    error
}

func (e *Unresolvable) Error() string {
	return fmt.Sprintf("%s (%s) could not be resolved: %s", e.Ref.key, e.Ref, e.Detail)
}
func (e *Unresolvable) Unwrap() error { return e.Err }

// ResolverFailed is the refusal for a resolver that RAN and exited non-zero. It reports
// the resolver's identity and its exit status and nothing the resolver wrote.
type ResolverFailed struct {
	Ref      Ref
	Program  string
	ExitCode int
}

func (e *ResolverFailed) Error() string {
	return fmt.Sprintf("%s (%s): resolver %q exited with status %d. Nothing it wrote to stdout or "+
		"stderr is reported, stored or logged: that stream is the secret channel",
		e.Ref.key, e.Ref, e.Program, e.ExitCode)
}

// ResolverTimedOut is the refusal for a resolver that did not terminate inside
// ResolverTimeout. Deliberately a different type and a different sentence from
// ResolverFailed: one means "the secret store said no", the other means "the secret
// store did not answer", and a caller retries exactly one of them.
type ResolverTimedOut struct {
	Ref     Ref
	Program string
	Bound   time.Duration
}

func (e *ResolverTimedOut) Error() string {
	return fmt.Sprintf("%s (%s): resolver %q did not terminate within %s and was killed. It produced "+
		"no usable value and no partial output is reported",
		e.Ref.key, e.Ref, e.Program, e.Bound)
}

// ResolverSignaled is the refusal for a resolver killed by a signal that was not our
// timeout - an OOM kill, an operator's SIGTERM, a crash. Distinct from both of the above.
type ResolverSignaled struct {
	Ref     Ref
	Program string
	State   string
}

func (e *ResolverSignaled) Error() string {
	return fmt.Sprintf("%s (%s): resolver %q was terminated by a signal (%s) before it finished. It "+
		"produced no usable value and no partial output is reported",
		e.Ref.key, e.Ref, e.Program, e.State)
}

// ParseRef reads the value a secret-bearing key carries. An empty value leaves the key
// unconfigured (its feature stays off); a recognised reference parses; anything else is
// an *ErrLiteral, which is how a pasted credential is refused at start.
func ParseRef(key, raw string) (Ref, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Ref{key: key}, nil
	}
	kind, locator, found := strings.Cut(raw, ":")
	kind = strings.ToLower(strings.TrimSpace(kind))
	locator = strings.TrimSpace(locator)
	if !found || locator == "" || (kind != KindFile && kind != KindCmd) {
		return Ref{key: key}, &ErrLiteral{Key: key}
	}
	return Ref{key: key, kind: kind, locator: locator}, nil
}

// Resolve produces the value this reference names. An unconfigured reference resolves to
// an empty Value and no error - the feature is simply off.
//
// Every failure path returns a typed error naming the key and the reference, and none of
// them carries a byte the resolver produced.
func (r Ref) Resolve(ctx context.Context) (Value, error) {
	switch r.kind {
	case "":
		return Value{}, nil
	case KindFile:
		return r.resolveFile()
	case KindCmd:
		return r.resolveCmd(ctx)
	default:
		return Value{}, &Unresolvable{Ref: r, Detail: "unknown reference kind " + r.kind}
	}
}

func (r Ref) resolveFile() (Value, error) {
	b, err := os.ReadFile(r.locator)
	if err != nil {
		// err names the path (which is the reference) and the errno, never content.
		return Value{}, &Unresolvable{Ref: r, Detail: "reading the secret file failed: " + err.Error(), Err: err}
	}
	v := trimOneNewline(b)
	if len(v) == 0 {
		zero(b)
		return Value{}, &Unresolvable{Ref: r, Detail: "the secret file is empty, so there is no value to use"}
	}
	return Value{b: v}, nil
}

func (r Ref) resolveCmd(ctx context.Context) (Value, error) {
	argv := strings.Fields(r.locator)
	if len(argv) == 0 {
		return Value{}, &Unresolvable{Ref: r, Detail: "the reference names no resolver program"}
	}
	tctx, cancel := context.WithTimeout(ctx, ResolverTimeout)
	defer cancel()

	cmd := exec.CommandContext(tctx, argv[0], argv[1:]...)
	// The resolver inherits holdfast's environment, which by construction holds no
	// resolved credential: there is no env: kind, and a literal in HOLDFAST_* is refused
	// at start. So resolving one key cannot show another key's value to a resolver.
	cmd.Env = os.Environ()
	var out bytes.Buffer
	cmd.Stdout = &capped{w: &out}
	// Discarded UNREAD. A resolver's stderr is untrusted bytes that may echo the value.
	cmd.Stderr = nil
	cmd.Stdin = nil

	// THE BOUND IS ENFORCED ON THE WHOLE PROCESS GROUP, not on the resolver alone, and
	// that is what makes it a bound at all. A realistic resolver is a wrapper - a shell
	// script around `vault`, an agent client - so killing only the direct child leaves a
	// GRANDCHILD alive holding the credential and holding the stdout pipe open, and Wait
	// then blocks on that pipe for as long as the grandchild lives. Measured: with the
	// default single-process kill, a resolver whose script ran `sleep 300` made this
	// function return after 300 seconds against a 5-second bound.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil // it exited on its own between the deadline and the signal
		}
		return err
	}
	// The backstop for a process the kernel will not reap promptly (an uninterruptible
	// read against a wedged secret store): give up on its output rather than on the bound.
	cmd.WaitDelay = waitDelay

	runErr := cmd.Run()

	if errors.Is(tctx.Err(), context.DeadlineExceeded) {
		zeroBuf(&out)
		return Value{}, &ResolverTimedOut{Ref: r, Program: argv[0], Bound: ResolverTimeout}
	}
	if runErr != nil {
		defer zeroBuf(&out)
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			if code := ee.ExitCode(); code >= 0 {
				return Value{}, &ResolverFailed{Ref: r, Program: argv[0], ExitCode: code}
			}
			return Value{}, &ResolverSignaled{Ref: r, Program: argv[0], State: ee.ProcessState.String()}
		}
		return Value{}, &Unresolvable{Ref: r,
			Detail: "the resolver could not be started: " + runErr.Error(), Err: runErr}
	}
	v := trimOneNewline(out.Bytes())
	if len(v) == 0 {
		zeroBuf(&out)
		return Value{}, &Unresolvable{Ref: r, Detail: "the resolver exited 0 but printed nothing, so there is no value to use"}
	}
	return Value{b: v}, nil
}

// Set is every secret-bearing key this run resolved, resolved ONCE at start. It is the
// only place a plaintext credential lives outside the consumer that was handed one.
type Set struct{ vals map[string]Value }

// Resolve resolves every configured reference, in order, and stops at the first failure.
// It runs at start, before any scan, encode or swap work, so a reference that cannot
// produce a value is a startup refusal rather than a surprise hours in (secrets K5).
func Resolve(ctx context.Context, refs []Ref) (*Set, error) {
	s := &Set{vals: make(map[string]Value, len(refs))}
	for _, r := range refs {
		v, err := r.Resolve(ctx)
		if err != nil {
			return nil, err
		}
		s.vals[r.key] = v
	}
	return s, nil
}

// Get is the resolved value for a key, or an empty Value when the key was unconfigured.
func (s *Set) Get(key string) Value {
	if s == nil {
		return Value{}
	}
	return s.vals[key]
}

// capped is a Writer that stops after maxResolverOutput rather than growing without
// bound on a resolver that streams.
type capped struct {
	w *bytes.Buffer
	n int
}

// Write reports the WHOLE offered length as written even when it kept less, which is what
// makes this a cap rather than a kill. Returning the truncated count is a short write:
// io.Copy - which is what exec runs against a non-file Stdout - treats that as
// io.ErrShortWrite, stops copying and closes the pipe, and the resolver then dies of
// SIGPIPE and is reported as having exited 141. Measured, before this comment existed: a
// resolver that streamed 2MB came back as a FAILED resolver instead of a capped value.
func (c *capped) Write(p []byte) (int, error) {
	offered := len(p)
	room := maxResolverOutput - c.n
	if room <= 0 {
		return offered, nil
	}
	if len(p) > room {
		p = p[:room]
	}
	n, err := c.w.Write(p)
	c.n += n
	if err != nil {
		return n, err
	}
	return offered, nil
}

// trimOneNewline strips one trailing newline (and a preceding CR). A secret written by
// `printf` and one written by an editor differ by exactly that byte, and a trailing
// newline in a bearer token is a 401 nobody can see.
func trimOneNewline(b []byte) []byte {
	b = bytes.TrimSuffix(b, []byte("\n"))
	b = bytes.TrimSuffix(b, []byte("\r"))
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func zeroBuf(buf *bytes.Buffer) {
	zero(buf.Bytes())
	buf.Reset()
}
