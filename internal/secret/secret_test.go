package secret

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sentinel is what a test looks for everywhere a credential must not be. It is
// deliberately not credential-SHAPED: the scanner reads this file, and a fixture that
// looked like an issued token would make `make secret-scan` red by construction.
const sentinel = "THE-RESOLVED-VALUE-MUST-NOT-APPEAR-HERE"

func fileRef(t *testing.T, key, value string) (Ref, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), key+".secret")
	if err := os.WriteFile(path, []byte(value+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	ref, err := ParseRef(key, "file:"+path)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	return ref, path
}

// script writes an executable shell script and returns a cmd: reference to it.
func script(t *testing.T, key, body string, args ...string) Ref {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resolver.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ref, err := ParseRef(key, "cmd:"+strings.Join(append([]string{path}, args...), " "))
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	return ref
}

// AC-7: a secret-bearing key carrying a literal credential rather than a reference
// refuses to start, naming the key and how to convert it, with NO PART of the value in the
// message - not the value, not a prefix of it, and not its length.
func TestSecret_AC7_ALiteralIsRefusedAndTheMessageNeverEchoesIt(t *testing.T) {
	literals := map[string]string{
		"a pasted bearer token":     sentinel,
		"a shoutrrr URL":            "ntfy://ntfy.sh/" + sentinel,
		"a discord URL":             "discord://" + sentinel + "@channel",
		"an unknown reference kind": "vault:" + sentinel,
		"a kind with no locator":    "file:",
		"a near-miss kind":          "files:/run/secrets/x",
		"a bare path":               "/run/secrets/" + sentinel,
	}
	for name, raw := range literals {
		t.Run(name, func(t *testing.T) {
			ref, err := ParseRef("server_auth_token", raw)
			if err == nil {
				t.Fatalf("ParseRef(%q) was ACCEPTED as %v; a literal must refuse to start", raw, ref)
			}
			var lit *ErrLiteral
			if !errors.As(err, &lit) {
				t.Fatalf("error is %T, want *ErrLiteral", err)
			}
			msg := err.Error()
			if strings.Contains(msg, sentinel) {
				t.Fatalf("the refusal echoed the value:\n%s", msg)
			}
			if !strings.Contains(msg, "server_auth_token") {
				t.Errorf("the refusal does not name the key:\n%s", msg)
			}
			for _, howTo := range []string{"file:", "cmd:", "docs/secrets.md"} {
				if !strings.Contains(msg, howTo) {
					t.Errorf("the refusal does not say how to convert it (missing %q):\n%s", howTo, msg)
				}
			}
		})
	}
}

// AC-7: an EMPTY key is not a literal. All three keys ship empty and their features are
// off; refusing an absent key would refuse every default installation.
func TestSecret_AC7_AnAbsentKeyIsNotALiteral(t *testing.T) {
	for _, raw := range []string{"", "   ", "\t\n"} {
		ref, err := ParseRef("notify_url", raw)
		if err != nil {
			t.Fatalf("ParseRef(%q) = %v; an absent key must be accepted", raw, err)
		}
		if ref.Configured() {
			t.Errorf("ParseRef(%q) reports configured", raw)
		}
		v, err := ref.Resolve(context.Background())
		if err != nil || !v.Empty() {
			t.Errorf("resolving an absent key = %v, %v; want an empty value and no error", v, err)
		}
	}
}

// AC-1 (the resolution half): a reference that resolves produces exactly the value, with a
// single trailing newline stripped so a secret written by an editor and one written by
// printf are the same credential.
func TestSecret_AC1_AResolvedReferenceProducesTheValue(t *testing.T) {
	ref, _ := fileRef(t, "server_auth_token", sentinel)
	v, err := ref.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Expose() != sentinel {
		t.Errorf("Expose() = %q, want %q", v.Expose(), sentinel)
	}

	cmdRef := script(t, "notify_url", `printf '%s\n' '`+sentinel+`'`)
	v, err = cmdRef.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve(cmd): %v", err)
	}
	if v.Expose() != sentinel {
		t.Errorf("cmd resolver Expose() = %q, want %q", v.Expose(), sentinel)
	}
}

// AC-4: a reference naming a secret the resolver cannot produce refuses at start, naming
// the key and the reference, with no candidate value in the message.
func TestSecret_AC4_AnUnresolvableReferenceRefusesNamingTheKeyAndReference(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "not-there")

	cases := map[string]Ref{}
	for name, raw := range map[string]string{
		"a file that is not there": "file:" + missing,
		"a file that is empty":     "file:" + empty,
	} {
		ref, err := ParseRef("tautulli_api_key", raw)
		if err != nil {
			t.Fatalf("ParseRef(%q): %v", raw, err)
		}
		cases[name] = ref
	}
	cases["a resolver that is not there"] = mustRef(t, "tautulli_api_key",
		"cmd:"+filepath.Join(dir, "no-such-resolver"))
	cases["a resolver that prints nothing"] = script(t, "tautulli_api_key", "exit 0")

	for name, ref := range cases {
		t.Run(name, func(t *testing.T) {
			v, err := ref.Resolve(context.Background())
			if err == nil {
				t.Fatalf("Resolve succeeded with %v; an unresolvable reference must refuse", v)
			}
			var un *Unresolvable
			if !errors.As(err, &un) {
				t.Fatalf("error is %T (%v), want *Unresolvable", err, err)
			}
			if !v.Empty() {
				t.Error("a failed resolution returned a non-empty value")
			}
			msg := err.Error()
			if !strings.Contains(msg, "tautulli_api_key") {
				t.Errorf("the refusal does not name the key:\n%s", msg)
			}
			if !strings.Contains(msg, ref.String()) {
				t.Errorf("the refusal does not name the reference %q:\n%s", ref.String(), msg)
			}
			if strings.Contains(msg, sentinel) {
				t.Errorf("the refusal carries a candidate value:\n%s", msg)
			}
		})
	}
}

func mustRef(t *testing.T, key, raw string) Ref {
	t.Helper()
	ref, err := ParseRef(key, raw)
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", raw, err)
	}
	return ref
}

// AC-5: a resolver that exits non-zero is reported by IDENTITY and EXIT STATUS, and
// nothing it wrote to stdout is emitted, logged or stored.
//
// This is the shape the spec's hazard note names: a resolver that fails AFTER printing the
// credential is common, and the obvious diagnostic ("resolver said: ...") discloses it.
func TestSecret_AC5_AResolverExitingNonZeroReportsStatusAndNeverItsStdout(t *testing.T) {
	ref := script(t, "server_auth_token",
		`printf '%s\n' '`+sentinel+`'; printf 'stderr diagnostic `+sentinel+`\n' >&2; exit 7`)
	v, err := ref.Resolve(context.Background())
	if err == nil {
		t.Fatalf("Resolve succeeded with %v; a resolver exiting 7 must refuse", v)
	}
	if !v.Empty() {
		t.Fatal("a failed resolution returned a value anyway")
	}
	var failure *ResolverFailed
	if !errors.As(err, &failure) {
		t.Fatalf("error is %T (%v), want *ResolverFailed", err, err)
	}
	if failure.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", failure.ExitCode)
	}
	msg := err.Error()
	if strings.Contains(msg, sentinel) {
		t.Fatalf("THE RESOLVER'S OUTPUT REACHED THE ERROR - this is the leak AC-5 exists for:\n%s", msg)
	}
	for _, want := range []string{"server_auth_token", "resolver.sh", "status 7"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the report does not carry %q:\n%s", want, msg)
		}
	}
}

// AC-6: a resolver that does not terminate within the documented bound is abandoned, and
// the report is DISTINGUISHABLE from AC-5's - a caller retries a store that did not answer
// and obeys a store that said no. Graded against the REAL ResolverTimeout, not a seam, so
// the number this repository documents is the number in force.
func TestSecret_AC6_AResolverThatNeverTerminatesIsBoundedAndDistinctFromAnExitStatus(t *testing.T) {
	ref := script(t, "notify_url", `printf '%s\n' '`+sentinel+`'; sleep 300`)
	start := time.Now()
	v, err := ref.Resolve(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Resolve succeeded with %v; a resolver that never terminates must refuse", v)
	}
	if !v.Empty() {
		t.Fatal("a timed-out resolution returned a value anyway")
	}
	var timeout *ResolverTimedOut
	if !errors.As(err, &timeout) {
		t.Fatalf("error is %T (%v), want *ResolverTimedOut", err, err)
	}
	var failure *ResolverFailed
	if errors.As(err, &failure) {
		t.Fatal("a timeout is reported as an exit status, so a caller cannot tell them apart")
	}
	if elapsed > 4*ResolverTimeout {
		t.Errorf("waited %s, which is not bounded by ResolverTimeout (%s)", elapsed, ResolverTimeout)
	}
	if elapsed < ResolverTimeout/2 {
		t.Errorf("returned after %s, well inside the %s bound - the wait is not the documented one",
			elapsed, ResolverTimeout)
	}
	msg := err.Error()
	if strings.Contains(msg, sentinel) {
		t.Fatalf("PARTIAL RESOLVER OUTPUT REACHED THE ERROR:\n%s", msg)
	}
	for _, want := range []string{"notify_url", ResolverTimeout.String(), "no partial output"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the report does not carry %q:\n%s", want, msg)
		}
	}
}

// AC-6: a resolver terminated BY A SIGNAL is its own report, distinct from both a non-zero
// exit and a timeout. A process killed by the OOM killer mid-read has not said no, and it
// has not failed to answer inside the bound either.
func TestSecret_AC6_AResolverKilledByASignalIsItsOwnReport(t *testing.T) {
	ref := script(t, "server_auth_token",
		`printf '%s\n' '`+sentinel+`'; kill -9 $$`)
	v, err := ref.Resolve(context.Background())
	if err == nil {
		t.Fatalf("Resolve succeeded with %v; a killed resolver must refuse", v)
	}
	if !v.Empty() {
		t.Fatal("a signalled resolution returned a value anyway")
	}
	var signaled *ResolverSignaled
	if !errors.As(err, &signaled) {
		t.Fatalf("error is %T (%v), want *ResolverSignaled", err, err)
	}
	var failure *ResolverFailed
	var timeout *ResolverTimedOut
	if errors.As(err, &failure) || errors.As(err, &timeout) {
		t.Fatal("a signal is reported as an exit status or a timeout, so a caller cannot tell them apart")
	}
	if msg := err.Error(); strings.Contains(msg, sentinel) {
		t.Fatalf("PARTIAL RESOLVER OUTPUT REACHED THE ERROR:\n%s", msg)
	}
}

// AC-6 (F3, the spec gate's one binding advisory): the bound is only gradeable if the
// repository writes it down, so docs/secrets.md must state the same number this constant
// holds. A bound stated in two places is a bound that drifts, and this is the check that
// stops it - the same discipline scripts/check-pins.sh applies to every cross-file pin.
func TestSecret_AC6_TheBoundIsTheOneTheDocumentationStates(t *testing.T) {
	path := filepath.Join(repoRoot(t), "docs", "secrets.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("docs/secrets.md must state the resolver bound: %v", err)
	}
	doc := string(b)
	want := ResolverTimeout.String()
	if !strings.Contains(doc, want) {
		t.Fatalf("docs/secrets.md does not state the resolver bound %q that internal/secret enforces. "+
			"AC-6 grades the timeout against the bound the repository documents, so an undocumented "+
			"bound has no referent", want)
	}
	if !strings.Contains(doc, "resolver") {
		t.Error("docs/secrets.md states a duration but never the word resolver, so the number is unattributed")
	}
}

// repoRoot walks up from the test's directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find the module root from the test's directory")
		}
		dir = parent
	}
}

// AC-2: a resolved value renders as <redacted> through EVERY formatting route there is, so
// a credential cannot reach a log line, a response body or a metric label by being handed
// to something that formats its arguments. This is the structural half of AC-2; the
// end-to-end half drives the real subcommands (see cmd/holdfast).
func TestSecret_AC2_AValueRedactsThroughEveryFormattingRoute(t *testing.T) {
	ref, _ := fileRef(t, "server_auth_token", sentinel)
	v, err := ref.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	renders := map[string]string{
		"String()":        v.String(),
		"%v":              fmt.Sprintf("%v", v),
		"%s":              fmt.Sprintf("%s", v),
		"%q":              fmt.Sprintf("%q", v),
		"%d":              fmt.Sprintf("%d", v),
		"%#v":             fmt.Sprintf("%#v", v),
		"%+v in a struct": fmt.Sprintf("%+v", struct{ Token Value }{v}),
		"Sprint":          fmt.Sprint(v),
		"errors.New":      fmt.Errorf("resolving: %w", fmt.Errorf("%v", v)).Error(),
	}
	for route, got := range renders {
		if strings.Contains(got, sentinel) {
			t.Errorf("%s leaked the value: %s", route, got)
		}
		if !strings.Contains(got, Redacted) {
			t.Errorf("%s = %q, which does not say it was redacted", route, got)
		}
	}

	j, err := json.Marshal(map[string]Value{"server_auth_token": v})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if bytes.Contains(j, []byte(sentinel)) {
		t.Errorf("JSON marshalling leaked the value: %s", j)
	}

	txt, err := v.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	if bytes.Contains(txt, []byte(sentinel)) {
		t.Errorf("text marshalling leaked the value: %s", txt)
	}

	// slog AT DEBUG, which is the level AC-2 calls out by name: a logger an operator may
	// legitimately turn all the way up must not be the one that discloses the credential.
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		buf := &bytes.Buffer{}
		log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		log.Log(context.Background(), level, "resolved", "token", v, "struct", struct{ T Value }{v})
		if strings.Contains(buf.String(), sentinel) {
			t.Errorf("slog at %s leaked the value: %s", level, buf.String())
		}
		if !strings.Contains(buf.String(), Redacted) {
			t.Errorf("slog at %s did not redact: %s", level, buf.String())
		}
	}
}

// AC-3: the resolved value is in NO child process's environment or argv - not an encoder
// or probe invocation, and not the resolver for another key.
//
// The property holds structurally rather than by scrubbing: there is no env: reference
// kind and a literal in HOLDFAST_* is refused, so nothing ever puts a credential into this
// process's environment for a child to inherit. This drives REAL children to prove it,
// because "we never do that" is the claim every leak is made of.
func TestSecret_AC3_NoChildProcessSeesAResolvedValueInItsEnvironmentOrArgv(t *testing.T) {
	first, path := fileRef(t, "server_auth_token", sentinel)
	v, err := first.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if v.Expose() != sentinel {
		t.Fatalf("the fixture did not resolve: %q", v.Expose())
	}

	// The resolved value is not in THIS process's environment, which is the whole reason
	// no child can inherit it.
	for _, kv := range os.Environ() {
		if strings.Contains(kv, sentinel) {
			t.Fatalf("the resolved value is in holdfast's own environment (%s), so every child inherits it",
				strings.SplitN(kv, "=", 2)[0])
		}
	}

	// A real child, started the way the encoder and the probe are started: exec.Command
	// with an inherited environment. It prints its own environment and argv.
	dump := filepath.Join(t.TempDir(), "dump.sh")
	if err := os.WriteFile(dump, []byte("#!/bin/sh\nenv\necho \"argv: $0 $*\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(dump, "-i", path, "-c:v", "libx265").CombinedOutput()
	if err != nil {
		t.Fatalf("stub child: %v: %s", err, out)
	}
	if strings.Contains(string(out), sentinel) {
		t.Fatalf("A CHILD PROCESS SAW THE RESOLVED VALUE in its environment or argv:\n%s", out)
	}

	// The resolver for ANOTHER key, which AC-3 names explicitly: resolving notify_url must
	// not show it server_auth_token's value. The resolver reports its own environment and
	// argv, so this is graded on what the child actually received.
	seen := filepath.Join(t.TempDir(), "seen")
	second := script(t, "notify_url",
		"{ env; echo \"argv: $0 $*\"; } > "+seen+"\nprintf 'ntfy://localhost/topic\\n'")
	if _, err := second.Resolve(context.Background()); err != nil {
		t.Fatalf("resolving the second key: %v", err)
	}
	dumped, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("the resolver did not report its environment: %v", err)
	}
	if strings.Contains(string(dumped), sentinel) {
		t.Fatalf("THE RESOLVER FOR ANOTHER KEY SAW server_auth_token's VALUE:\n%s", dumped)
	}
}

// AC-4/AC-5: Resolve over a SET stops at the first refusal and returns nothing, so a
// process cannot start half-configured with one good credential and one broken reference.
func TestSecret_AC4_ResolvingASetStopsAtTheFirstRefusal(t *testing.T) {
	good, _ := fileRef(t, "server_auth_token", sentinel)
	bad := mustRef(t, "notify_url", "cmd:"+filepath.Join(t.TempDir(), "no-such-resolver"))
	set, err := Resolve(context.Background(), []Ref{good, bad})
	if err == nil {
		t.Fatal("Resolve over a set with a broken reference succeeded")
	}
	if set != nil {
		t.Error("a failed set resolution returned a Set, which a caller could read from")
	}
	if got := (*Set)(nil).Get("server_auth_token"); !got.Empty() {
		t.Error("a nil Set returned a value")
	}
}

// The resolver's stdout is bounded: a broken resolver that streams must not exhaust memory
// at startup, and what is kept is still exactly a credential's worth.
//
// The fixture writes ~2MB and exits 0 on its own. Deliberately NOT a `yes | head -c`
// pipeline: `head` closing the pipe kills `yes` with SIGPIPE, the shell reports 141, and the
// case would then be graded on a resolver failure rather than on the cap.
func TestSecret_AResolverStdoutIsBounded(t *testing.T) {
	ref := script(t, "notify_url",
		`awk 'BEGIN { for (i = 0; i < 100000; i++) printf "%s", "aaaaaaaaaaaaaaaaaaaa" }'`)
	v, err := ref.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(v.Expose()) > maxResolverOutput {
		t.Errorf("kept %d bytes, over the %d-byte cap", len(v.Expose()), maxResolverOutput)
	}
	if len(v.Expose()) != maxResolverOutput {
		t.Errorf("kept %d bytes; a 2MB stream should fill the %d-byte cap exactly",
			len(v.Expose()), maxResolverOutput)
	}
}

// A reference renders as itself and never as a value, because it is what every failure
// path and every log line is allowed to name.
func TestSecret_ARefRendersAsItselfAndNamesItsKey(t *testing.T) {
	ref := mustRef(t, "tautulli_api_key", "  FILE:/run/secrets/tautulli  ")
	if got, want := ref.String(), "file:/run/secrets/tautulli"; got != want {
		t.Errorf("String() = %q, want %q (the kind is normalised, the locator is not)", got, want)
	}
	if got, want := ref.Key(), "tautulli_api_key"; got != want {
		t.Errorf("Key() = %q, want %q", got, want)
	}
	if got, want := ref.Kind(), KindFile; got != want {
		t.Errorf("Kind() = %q, want %q", got, want)
	}
	if got := (Ref{}).String(); got != "" {
		t.Errorf("an unconfigured Ref renders as %q, want the empty string", got)
	}
}
