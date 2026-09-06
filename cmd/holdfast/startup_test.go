package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/startup"
	"github.com/NSchatz/holdfast/internal/store"
)

// TestMain substitutes ONE thing for this package's tests: the filesystem-type
// lookup the startup check reads.
//
// It has to. The gate this repo is tested on hands out temp directories on
// `tmpfs` or `overlay`, and the check classifies both `undetermined` - correctly,
// because neither type says what storage is underneath it - so every end-to-end
// case here would be refused for a property of the RUNNER rather than of the
// code. Substituting the lookup, and only it, answers a recognised-local type for
// the fixture tree, which is what those fixtures actually are. The mount layout
// and the directory tree are still the real ones.
func TestMain(m *testing.M) {
	startupPlatform = func() startup.Platform { return startup.System(fixedType("ext4"), nil) }
	os.Exit(m.Run())
}

func fixedType(t string) func(string) (string, error) {
	return func(string) (string, error) { return t, nil }
}

// typesByPrefix substitutes a filesystem type per subtree, longest prefix
// winning, defaulting to a recognised-local type. It is how a test puts a
// network filesystem under one path without a network mount.
func typesByPrefix(types map[string]string) func(string) (string, error) {
	return func(path string) (string, error) {
		best, bestLen := "ext4", -1
		for prefix, t := range types {
			if (path == prefix || strings.HasPrefix(path, prefix+string(filepath.Separator))) && len(prefix) > bestLen {
				best, bestLen = t, len(prefix)
			}
		}
		return best, nil
	}
}

func substitute(t *testing.T, lookup func(string) (string, error)) {
	t.Helper()
	old := startupPlatform
	startupPlatform = func() startup.Platform { return startup.System(lookup, nil) }
	t.Cleanup(func() { startupPlatform = old })
}

// nasLayout builds a library on local storage and a state directory on
// substituted network storage. It returns the config path and the state
// directory, which does NOT exist yet.
func nasLayout(t *testing.T, extra string) (cfgPath, state string) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	nas := filepath.Join(dir, "nas")
	for _, d := range []string{lib, nas} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	state = filepath.Join(nas, "state")
	substitute(t, typesByPrefix(map[string]string{nas: "nfs"}))

	cfgPath = filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + state + "\n" + extra
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, state
}

// TestRun_RefusesAStateDirectoryOnNetworkStorageAndLeavesNothingBehind. The
// decision is taken before the job store is opened, so a refused run leaves no
// jobs.db, no journal and no sidecar on the storage it just refused - which is
// checked by looking at the directory, not by reading a log line (AC2a, AC8).
func TestRun_RefusesAStateDirectoryOnNetworkStorageAndLeavesNothingBehind(t *testing.T) {
	cfgPath, state := nasLayout(t, "")

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"run", "--config", cfgPath}, &out, &errOut); code != 1 {
		t.Fatalf("run code = %d, want 1 (stderr: %s)", code, errOut.String())
	}
	if _, err := os.Stat(state); err == nil {
		t.Fatal("the refused run created the state directory")
	}
	if entries, err := os.ReadDir(filepath.Dir(state)); err == nil {
		if len(entries) != 0 {
			t.Fatalf("the refused run left %v behind on the storage it refused", entries)
		}
	}
	if !strings.Contains(errOut.String(), state) {
		t.Fatalf("the refusal did not name the path:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), "nfs") {
		t.Fatalf("the refusal did not name the detected filesystem:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), startup.ConfigKey) {
		t.Fatalf("the refusal did not print the declaration that would permit it:\n%s", errOut.String())
	}
}

// TestServe_TakesTheDecisionBeforeItOpensTheStoreOrServes: serve is governed
// exactly as run is. It refuses the daemon rather than starting it, so nothing
// binds and nothing is written (Definitions/The system starts, AC2a).
func TestServe_TakesTheDecisionBeforeItOpensTheStoreOrServes(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfgPath, state := nasLayout(t, "server_addr: "+addr+"\n")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	var errOut bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- runServer(context.Background(), cfg, discardLog(), &errOut) }()
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("serve code = %d, want 1", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not refuse: it is still running")
	}

	if _, err := os.Stat(state); err == nil {
		t.Fatal("the refused daemon created the state directory")
	}
	// Nothing ever bound: the address is still free.
	l2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the refused daemon served anyway: %v", err)
	}
	_ = l2.Close()
}

// TestStartupCheck_AFreshInstallStarts is the bootstrap regression: a library
// root and a state directory that does not exist yet, over local storage, with no
// declarations at all. It must START, the state directory must be classified by
// the storage it would be created on, and it must not be created in order to
// classify it (AC5, AC5a).
func TestStartupCheck_AFreshInstallStarts(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state", "nested")
	substitute(t, fixedType("ext4"))

	cfg := &config.Config{LibraryRoots: []string{lib}, StateDir: state, VideoExts: []string{"mkv"}}
	res, code := startupCheck(cfg, discardLog(), io.Discard)
	if code != 0 || !res.Start {
		t.Fatalf("a fresh install was refused: code %d, causes %+v", code, res.Causes)
	}
	if _, err := os.Stat(state); err == nil {
		t.Fatal("the check created the state directory in order to classify it")
	}
	if _, err := os.Stat(filepath.Dir(state)); err == nil {
		t.Fatal("the check created an ancestor of the state directory")
	}
	var local bool
	for _, rec := range res.Records {
		if rec.Path == state {
			local = rec.Class == startup.Local
		}
	}
	if !local {
		t.Fatalf("the state directory was not classified by the storage it would be created on: %+v", res.Records)
	}
}

// TestStartupCheck_TheStateDirectoryIsCreatedOnlyAfterTheDecision drives the two
// calls buildEngine makes, in the order it makes them, over both storages an
// opt-in can start on: the check first (which classifies the storage the
// directory WOULD be created on and creates nothing), the store second. That
// ordering is the whole of AC2a - classification precedes the first byte written
// to that storage, including any file the job store brings with it - and it is
// what makes a refused run leave nothing behind (AC5, AC5a, AC9).
func TestStartupCheck_TheStateDirectoryIsCreatedOnlyAfterTheDecision(t *testing.T) {
	tests := []struct {
		name    string
		fsType  string
		declare bool
	}{
		{name: "over local storage, with no declarations at all", fsType: "ext4"},
		{name: "over network storage, with an opt-in naming exactly it", fsType: "nfs", declare: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			lib := filepath.Join(dir, "media")
			// The state directory does not exist yet; its nearest EXISTING
			// ancestor is this one, and that is the storage it would be created
			// on and therefore the storage the check classifies.
			anchor := filepath.Join(dir, "storage")
			for _, d := range []string{lib, anchor} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			state := filepath.Join(anchor, "holdfast", "state")
			substitute(t, typesByPrefix(map[string]string{anchor: tc.fsType, lib: "ext4"}))

			cfg := &config.Config{LibraryRoots: []string{lib}, StateDir: state, VideoExts: []string{"mkv"}}
			if tc.declare {
				cfg.AllowNonLocal = []string{state}
			}

			res, code := startupCheck(cfg, discardLog(), io.Discard)
			if code != 0 || !res.Start {
				t.Fatalf("refused: %+v", res.Causes)
			}
			if _, err := os.Stat(state); err == nil {
				t.Fatal("the decision had not been taken yet and the state directory already existed")
			}
			if tc.declare && !hasReducedGuarantee(res, state) {
				t.Fatalf("no reduced-guarantee record for the covered state directory: %+v", res.Notices)
			}

			// Only now, after the decision, does the job store bring its files.
			st, err := store.Open(filepath.Join(state, "jobs.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			if _, err := os.Stat(filepath.Join(state, "jobs.db")); err != nil {
				t.Fatalf("the store did not create its database after the decision: %v", err)
			}
		})
	}
}

func hasReducedGuarantee(res startup.Result, path string) bool {
	for _, n := range res.Notices {
		if n.Kind == startup.NoticeReducedGuarantee && n.Path == path {
			return true
		}
	}
	return false
}

// TestStartupCheck_TheRelativeDefaultStateDirectoryIsPrintedAsTheAbsolutePathIt-
// InterpretsTo, and what is printed round-trips (AC11b).
func TestStartupCheck_TheRelativeDefaultStateDirectoryIsPrintedAbsolute(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir) // the shipped default `state` interprets against the working directory
	substitute(t, typesByPrefix(map[string]string{dir: "nfs", lib: "ext4"}))

	// The shipped default: state_dir is the RELATIVE string "state".
	cfg := &config.Config{LibraryRoots: []string{lib}, StateDir: "state", VideoExts: []string{"mkv"}}
	var errOut bytes.Buffer
	res, code := startupCheck(cfg, discardLog(), &errOut)
	if code == 0 {
		t.Fatal("a state directory on network storage started the run")
	}
	printed := printedDeclarations(errOut.String())
	want := filepath.Join(dir, "state")
	if len(printed) != 1 || printed[0] != want {
		t.Fatalf("declarations printed = %v, want exactly the absolute path %q, never the configured string",
			printed, want)
	}
	_ = res

	// Feed back exactly the text it printed: accepted, and it starts.
	cfg.AllowNonLocal = printed
	res2, code2 := startupCheck(cfg, discardLog(), io.Discard)
	if code2 != 0 || !res2.Start {
		t.Fatalf("the declaration holdfast printed did not start the run: %+v", res2.Causes)
	}
}

func printedDeclarations(refusal string) []string {
	var out []string
	for _, line := range strings.Split(refusal, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "- /") {
			out = append(out, strings.TrimPrefix(t, "- "))
		}
	}
	return out
}

// TestStartupCheck_TheScanIsBoundedByTheWalk wires the two halves together: the
// coverage the startup walk established is what the engine is given, so the scan
// cannot go behind the decision.
func TestStartupCheck_TheScanIsBoundedByTheWalk(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(filepath.Join(lib, "tv"), 0o755); err != nil {
		t.Fatal(err)
	}
	substitute(t, fixedType("ext4"))

	cfg := &config.Config{LibraryRoots: []string{lib}, StateDir: filepath.Join(dir, "state"), VideoExts: []string{"mkv"}}
	res, code := startupCheck(cfg, discardLog(), io.Discard)
	if code != 0 {
		t.Fatalf("refused: %+v", res.Causes)
	}
	want := map[string]bool{lib: true, filepath.Join(lib, "tv"): true}
	if len(res.Coverage) != len(want) {
		t.Fatalf("coverage = %v, want exactly %v", res.Coverage, want)
	}
	for _, dir := range res.Coverage {
		if !want[dir] {
			t.Fatalf("coverage names %s, which is not beneath the library root", dir)
		}
	}
}

// TestValidate_TakesNoDecisionAndPaysNoWalk: `validate` opens no store and
// touches no media, so none of this fires on it - it neither classifies nor
// refuses, and a configuration that would be refused at `run` still validates.
func TestValidate_TakesNoDecisionAndPaysNoWalk(t *testing.T) {
	cfgPath, state := nasLayout(t, "")
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"validate", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("validate code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "config OK") {
		t.Fatalf("validate stdout = %q", out.String())
	}
	if _, err := os.Stat(state); err == nil {
		t.Fatal("validate created the state directory")
	}
}

// TestConfig_TheOptInIsPartOfTheDeclarativeConfiguration (AC12): the opt-in is
// taken from the same YAML the rest of the run is declared in, so two starts with
// identical configuration and no interactive input decide identically.
func TestConfig_TheOptInIsPartOfTheDeclarativeConfiguration(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - /srv/media\n" + startup.ConfigKey + ":\n  - /srv/media/tv\n  - /srv/media/nas\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("the opt-in key is not accepted by the configuration loader: %v", err)
	}
	if len(cfg.AllowNonLocal) != 2 || cfg.AllowNonLocal[0] != "/srv/media/tv" {
		t.Fatalf("allow_non_local = %v, want both declarations in order", cfg.AllowNonLocal)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
