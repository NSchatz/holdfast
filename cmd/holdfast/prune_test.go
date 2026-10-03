//go:build linux

// Linux only: AC-9 observes listings through inotify and the watch through /proc/self/fdinfo,
// which is where this repository's gate runs.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/startup"
)

// S0168 at the level its two whole-system criteria are written about: the set of files a
// run offers (AC-3) and every listing a run issues (AC-9), through the one start-or-refuse
// construction and the real engine.

// pruneLibrary lays out one library root holding a file for every filter shape AC-3 names:
// a relative directory, an absolute one, a name at any depth, a trash directory, and a
// directory whose name is a whole-path near miss of an excluded one.
func pruneLibrary(t *testing.T, root string) {
	t.Helper()
	for _, rel := range []string{
		"top.mkv",
		"movies/4k/d.mkv",
		"movies/4k/remux/e.mkv",
		"movies/Extras/c.mkv",
		"movies/tv/a.mkv",
		"movies/tv-archive/b.mkv",
		"import/new.mkv",
		".Trash-0/old.mkv",
		".Trash-0/sub/older.mkv",
		"shows/S1/ep1.mkv",
		"shows/S1/Extras/x.mkv",
	} {
		writeCensusFile(t, filepath.Join(root, filepath.FromSlash(rel)), "not media: "+rel+"\n")
	}
	writeCensusFile(t, filepath.Join(root, "notes.txt"), "not a source\n")
}

// filterOracle is what AC-3 measures against: every source-named regular file under the
// roots, found by LISTING EVERY DIRECTORY - excluded ones included - and then the path-filter
// decision asked of each, under the root a scan assigns it to (the first containing it).
func filterOracle(t *testing.T, cfg *config.Config) map[string]bool {
	t.Helper()
	roots := cfg.RootProfiles()
	accepted := map[string]bool{}
	for _, r := range roots {
		err := filepath.WalkDir(r.Clean, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() || !engine.IsSourceName(d.Name(), cfg.VideoExts) {
				return nil
			}
			for _, owner := range roots {
				if owner.Contains(p) {
					if owner.Offers(p) {
						accepted[p] = true
					}
					break
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("listing every directory of the fixture: %v", err)
		}
	}
	return accepted
}

// offeredBy is the set a run offers over cfg: the startup walk the one construction takes,
// then the engine's own enumeration over its coverage - the read-only pass drives the same
// enumeration a scan streams, and a path it declines outright was offered all the same.
func offeredBy(t *testing.T, cfg *config.Config) (map[string]bool, startup.Result) {
	t.Helper()
	res := startupDecision(cfg)
	if !res.Start {
		t.Fatalf("the startup check refused the fixture at row %d: %+v", res.Row, res.Causes)
	}
	var errOut bytes.Buffer
	eng, ledger, _, code := planEngine(context.Background(), cfg, res, &errOut)
	if code != 0 {
		t.Fatalf("building the pass failed with code %d: %s", code, errOut.String())
	}
	if ledger != nil {
		defer func() { _ = ledger.Close() }()
	}
	pass := eng.Plan(context.Background(), engine.PlanOptions{Snapshot: planSnapshot(eng)})
	offered := map[string]bool{}
	for _, f := range pass.Files {
		offered[f.Path] = true
	}
	for _, d := range pass.Declined {
		offered[d.Path] = true
	}
	return offered, res
}

// TestS0168AC3_ARunOffersExactlyWhatThePathFiltersAccept is S0168 [AC-3], and it is the
// criterion that holds the one unrecoverable outcome: with every filter shape the criterion
// names in force, the files a run offers are EXACTLY the source files the path-filter decision
// accepts among every source-named file under the roots, found by listing every directory. No
// file beneath a pruned directory is offered, and no file outside one is dropped.
//
// Nested roots are refused by Validate, and are graded anyway in both orders: the prune must
// agree with the scan about which root's filters decide a path whatever refused them earlier.
func TestS0168AC3_ARunOffersExactlyWhatThePathFiltersAccept(t *testing.T) {
	substitute(t, fixedType("ext4"))
	type layout struct {
		name   string
		roots  []string // relative to the fixture directory; the first holds the library
		body   func(abs []string) string
		nested bool
		prunes bool
	}
	entry := func(root string, lines ...string) string {
		s := "  - path: " + root + "\n"
		for _, l := range lines {
			s += "    " + l + "\n"
		}
		return s
	}
	for _, tc := range []layout{
		{name: "a top-level list", roots: []string{"a"}, prunes: true, body: func(abs []string) string {
			return "exclude_paths: [\"movies/4k\", \"**/Extras\"]\nlibrary_roots:\n" + entry(abs[0])
		}},
		{name: "per-root lists", roots: []string{"a", "b", "c"}, prunes: true, body: func(abs []string) string {
			return "exclude_paths: [\"**/Extras\"]\nlibrary_roots:\n" +
				entry(abs[0], "exclude_paths: [\"**/.Trash-*/**\", \"movies/tv\"]") +
				entry(abs[1]) +
				entry(abs[2], "exclude_paths: []")
		}},
		{name: "a relative pattern", roots: []string{"a"}, prunes: true, body: func(abs []string) string {
			return "exclude_paths: [\"movies/4k\"]\nlibrary_roots:\n" + entry(abs[0])
		}},
		{name: "an absolute pattern", roots: []string{"a"}, prunes: true, body: func(abs []string) string {
			return "exclude_paths: [\"" + abs[0] + "/import\"]\nlibrary_roots:\n" + entry(abs[0])
		}},
		{name: "a name at any depth", roots: []string{"a"}, prunes: true, body: func(abs []string) string {
			return "exclude_paths: [\"**/Extras\"]\nlibrary_roots:\n" + entry(abs[0])
		}},
		{name: "a trash directory", roots: []string{"a"}, prunes: true, body: func(abs []string) string {
			return "exclude_paths: [\"**/.Trash-*/**\"]\nlibrary_roots:\n" + entry(abs[0])
		}},
		{name: "a whole-path near miss", roots: []string{"a"}, prunes: true, body: func(abs []string) string {
			return "exclude_paths: [\"movies/tv\"]\nlibrary_roots:\n" + entry(abs[0])
		}},
		{name: "an include list with no exclude", roots: []string{"a"}, body: func(abs []string) string {
			return "include_paths: [\"movies/**\"]\nlibrary_roots:\n" + entry(abs[0])
		}},
		{name: "exclude and include together", roots: []string{"a"}, prunes: true, body: func(abs []string) string {
			return "include_paths: [\"movies/**\", \"shows/**\"]\nexclude_paths: [\"**/Extras\", \"movies/4k\"]\n" +
				"library_roots:\n" + entry(abs[0])
		}},
		{name: "a nested root after its outer root", roots: []string{"a", "a/movies"}, nested: true, prunes: true,
			body: func(abs []string) string {
				return "library_roots:\n" + entry(abs[0], "exclude_paths: [\"movies/tv\", \"shows\"]") +
					entry(abs[1], "exclude_paths: [\"4k\"]")
			}},
		{name: "a nested root before its outer root", roots: []string{"a/movies", "a"}, nested: true, prunes: true,
			body: func(abs []string) string {
				return "library_roots:\n" + entry(abs[0], "exclude_paths: [\"4k\"]") +
					entry(abs[1], "exclude_paths: [\"movies/tv\", \"shows\"]")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var abs []string
			for _, r := range tc.roots {
				abs = append(abs, filepath.Join(dir, filepath.FromSlash(r)))
			}
			for _, r := range abs {
				if !strings.HasPrefix(r, filepath.Join(dir, "a")+string(filepath.Separator)) {
					pruneLibrary(t, r)
				}
			}
			cfgPath := filepath.Join(dir, "config.yaml")
			body := tc.body(abs) + "state_dir: " + filepath.Join(dir, "state") + "\n"
			if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatalf("Load: %v\n%s", err, body)
			}
			if err := cfg.Validate(); (err != nil) != tc.nested {
				t.Fatalf("Validate = %v; a nested layout is exactly what it refuses, and nothing else is", err)
			}

			want := filterOracle(t, cfg)
			got, res := offeredBy(t, cfg)
			if !sameSet(got, want) {
				t.Fatalf("offered a different set than the path filters accept:\n  offered:  %v\n  accepted: %v\n%s",
					sortedSet(got), sortedSet(want), body)
			}
			var pruned []string
			for _, n := range res.Notices {
				if n.Kind == startup.NoticeExcluded {
					pruned = append(pruned, n.Path)
				}
			}
			for p := range got {
				for _, d := range pruned {
					if strings.HasPrefix(p, d+string(filepath.Separator)) {
						t.Errorf("%s was offered from beneath the pruned directory %s", p, d)
					}
				}
			}
			// Anti-vacuity both ways: the walk really pruned where an exclude list reaches a
			// directory, and never where there is only an include list.
			if tc.prunes != (len(pruned) > 0) {
				t.Fatalf("pruned %v; this layout %s prune", pruned, map[bool]string{true: "must", false: "must not"}[tc.prunes])
			}
			if len(want) == 0 {
				t.Fatal("the filters accept nothing at all, so the equality above proves nothing")
			}
		})
	}
}

// inotifyEvent is one decoded record from an inotify descriptor.
type inotifyEvent struct {
	dir  string
	mask uint32
	name string
}

// openWatch reports every OPEN and every READ of each directory given - which is what a
// listing of it is, and the only thing that raises either event on a directory: inspecting a
// directory (a stat) raises neither, so the walk inspecting a pruned directory is not a
// listing of it, and registering a watch on one raises neither too (see watchedInodes).
type openWatch struct {
	fd  int
	wds map[int32]string
}

func watchOpens(t *testing.T, dirs []string) *openWatch {
	t.Helper()
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		t.Skipf("this platform cannot observe directory listings through inotify: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Close(fd) })
	w := &openWatch{fd: fd, wds: map[int32]string{}}
	for _, d := range dirs {
		wd, err := syscall.InotifyAddWatch(fd, d, syscall.IN_OPEN|syscall.IN_ACCESS)
		if err != nil {
			t.Fatalf("watching %s: %v", d, err)
		}
		w.wds[int32(wd)] = d
	}
	return w
}

// drain returns every event queued so far.
func (w *openWatch) drain(t *testing.T) []inotifyEvent {
	t.Helper()
	var out []inotifyEvent
	buf := make([]byte, 64*1024)
	for {
		n, err := syscall.Read(w.fd, buf)
		if err == syscall.EAGAIN || n <= 0 {
			return out
		}
		if err != nil {
			t.Fatalf("reading inotify events: %v", err)
		}
		for off := 0; off+syscall.SizeofInotifyEvent <= n; {
			wd := int32(binary.NativeEndian.Uint32(buf[off:]))
			mask := binary.NativeEndian.Uint32(buf[off+4:])
			nameLen := int(binary.NativeEndian.Uint32(buf[off+12:]))
			name := strings.TrimRight(string(buf[off+syscall.SizeofInotifyEvent:off+syscall.SizeofInotifyEvent+nameLen]), "\x00")
			out = append(out, inotifyEvent{dir: w.wds[wd], mask: mask, name: name})
			off += syscall.SizeofInotifyEvent + nameLen
		}
	}
}

// watchedInodes is every inode an inotify descriptor of THIS process other than skip holds
// a watch on, read from the kernel's own account of each one in /proc/self/fdinfo. It is how
// "the watch registered nothing beneath a pruned directory" is asserted from outside the
// watch: registering one is not a listing, so the open watch above cannot see it.
func watchedInodes(t *testing.T, skip int) map[uint64]bool {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd on this platform: %v", err)
	}
	out := map[uint64]bool{}
	for _, e := range fds {
		fd, err := strconv.Atoi(e.Name())
		if err != nil || fd == skip {
			continue
		}
		if target, err := os.Readlink("/proc/self/fd/" + e.Name()); err != nil || target != "anon_inode:inotify" {
			continue
		}
		info, err := os.ReadFile("/proc/self/fdinfo/" + e.Name())
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(info))
		for sc.Scan() {
			for _, field := range strings.Fields(sc.Text()) {
				if v, ok := strings.CutPrefix(field, "ino:"); ok {
					if ino, err := strconv.ParseUint(v, 16, 64); err == nil {
						out[ino] = true
					}
				}
			}
		}
	}
	return out
}

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

// countRecords counts the records whose message is msg.
func countRecords(b *syncBuffer, msg string) int {
	n := 0
	for _, line := range strings.Split(b.String(), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg {
			n++
		}
	}
	return n
}

// TestS0168AC9_NoPassSweepOrWatchOfAServeRunListsAPrunedDirectory is S0168 [AC-9], graded on
// a real `serve` daemon - the one command whose run has a second and later scan pass and a
// watch - over a library whose pruned directory holds sources, a subdirectory and an orphaned
// temp file of this tool's own. Across the startup walk, three scan passes with their temp
// sweeps and the watch registration, the kernel reports no open or read of the pruned
// directory or of anything beneath it, the watch holds no descriptor on it, and the orphaned
// temp inside it is still there - the sweep never saw it.
func TestS0168AC9_NoPassSweepOrWatchOfAServeRunListsAPrunedDirectory(t *testing.T) {
	if _, err := exec.LookPath(envOr("HOLDFAST_FFMPEG", "ffmpeg")); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	trash := filepath.Join(lib, ".Trash-0")
	orphan := filepath.Join(trash, "Old."+engine.TempMarker+".mkv")
	writeCensusFile(t, filepath.Join(lib, "shows", "notes.txt"), "not a source\n")
	writeCensusFile(t, filepath.Join(trash, "Old.mkv"), "a binned source\n")
	writeCensusFile(t, filepath.Join(trash, "sub", "Older.mkv"), "a binned source\n")
	writeCensusFile(t, orphan, "a temp a killed run left\n")

	cfgPath := filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - path: " + lib + "\n    watch: true\n    watch_settle_sec: 60\n" +
		"exclude_paths: [\"**/.Trash-*/**\"]\n" +
		"state_dir: " + filepath.Join(dir, "state") + "\n" +
		"server_addr: " + addr + "\nscan_interval_sec: 1\nvmaf_enable: false\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	opens := watchOpens(t, []string{trash, filepath.Join(trash, "sub")})
	_ = opens.drain(t) // anything this test did itself before the daemon started

	sink := &syncBuffer{}
	log := slog.New(slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- runServer(ctx, cfg, log, sink) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Error("runServer did not shut down after its context was cancelled")
		}
	}()
	waitHTTP(t, "http://"+addr+"/api/summary", serverReady)

	deadline := time.Now().Add(60 * time.Second)
	for countRecords(sink, "scan finished") < 3 || len(watchRecords(t, sink, lib)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the daemon did not finish three scan passes and decide its watch: %s", sink.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	held := watchedInodes(t, opens.fd)
	if held[inodeOf(t, trash)] || held[inodeOf(t, filepath.Join(trash, "sub"))] {
		t.Error("the watch holds a descriptor on the pruned directory or beneath it")
	}
	started := false
	for _, rec := range watchRecords(t, sink, lib) {
		if rec["watch_descriptors"] != nil {
			started = true
		}
	}
	if started && !held[inodeOf(t, lib)] {
		t.Fatal("the watch reports itself started and holds no descriptor on the root, so the descriptor " +
			"account above proves nothing")
	}
	if !started {
		t.Logf("the watch fell back on this host's storage, so only its absence from the pruned directory is shown")
	}

	if ev := opens.drain(t); len(ev) != 0 {
		var got []string
		for _, e := range ev {
			got = append(got, fmt.Sprintf("%s %q mask %#x", e.dir, e.name, e.mask))
		}
		sort.Strings(got)
		t.Errorf("the pruned directory was opened or read during the run:\n  %s", strings.Join(got, "\n  "))
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Errorf("the orphaned temp inside the pruned directory is gone (%v): a sweep reached it", err)
	}
	// Anti-vacuity for the event half: the same watch DOES see a listing of the directory.
	if _, err := os.ReadDir(trash); err != nil {
		t.Fatal(err)
	}
	if len(opens.drain(t)) == 0 {
		t.Fatal("listing the pruned directory raised no event, so the silence above proves nothing")
	}
}
