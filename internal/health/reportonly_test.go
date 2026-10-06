package health

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/store"
)

// ffmpegBin finds the pinned ffmpeg, failing loud rather than skipping: a skipped safety
// proof is a false green (docs/development.md, "a grader that skips is a false green").
func ffmpegBin(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("HOLDFAST_FFMPEG")
	if bin == "" {
		bin = "ffmpeg"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("::error:: %q not found - the health sweep fixtures require the pinned ffmpeg (set HOLDFAST_FFMPEG): %v", bin, err)
	}
	return bin
}

func lavfi(t *testing.T, bin, out, src, codec string) {
	t.Helper()
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", src,
		"-c:v", codec, "-pix_fmt", "yuv420p"}
	if codec == "libx264" {
		args = append(args, "-preset", "ultrafast")
	}
	if b, err := exec.Command(bin, append(args, out)...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %s: %v\n%s", out, err, b)
	}
}

// entry is everything about one directory entry the sweep could conceivably change.
type entry struct {
	Name   string
	Size   int64
	Mtime  int64
	Mode   fs.FileMode
	Inode  uint64
	SHA256 string
}

// snapshot reads every entry under dir, recursively, without following a link and without
// opening anything that is not a regular file.
func snapshot(t *testing.T, dir string) []entry {
	t.Helper()
	var out []entry
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		e := entry{Name: strings.TrimPrefix(p, dir), Size: fi.Size(), Mtime: fi.ModTime().UnixNano(), Mode: fi.Mode()}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			e.Inode = st.Ino
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			h := sha256.New()
			_, err = io.Copy(h, f)
			_ = f.Close()
			if err != nil {
				return err
			}
			e.SHA256 = hex.EncodeToString(h.Sum(nil))
		}
		out = append(out, e)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", dir, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// TestHealthSweep_IsReportOnly_NoFileMovedRenamedOrDeleted is the report-only proof. A
// synthetic library - lavfi clips, a truncated copy, a copy with its middle overwritten, a
// zero-byte file, a text file under a video name, a FIFO and a subdirectory - is
// snapshotted (every entry's name, size, mtime, mode, inode and sha256), swept in full
// through the REAL decode (the pinned ffmpeg), and snapshotted again. The two snapshots are
// identical: no file was written, renamed, moved, deleted, truncated or touched, and no
// entry - no sidecar, no temp - appeared. And the sweep reported what is there: the clips
// ok, the damaged ones corrupt, the FIFO unreadable.
func TestHealthSweep_IsReportOnly_NoFileMovedRenamedOrDeleted(t *testing.T) {
	bin := ffmpegBin(t)
	lib := t.TempDir()
	if err := os.Mkdir(filepath.Join(lib, "season"), 0o755); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(lib, "good.mkv")
	lavfi(t, bin, good, "testsrc2=duration=2:size=160x120:rate=24", "libx264")
	lavfi(t, bin, filepath.Join(lib, "season", "episode.mp4"), "testsrc=duration=2:size=160x120:rate=24", "libx264")
	lavfi(t, bin, filepath.Join(lib, "mpeg4.avi"), "testsrc2=duration=1:size=160x120:rate=24", "mpeg4")
	whole, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(lib, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("truncated.mkv", whole[:len(whole)/2])
	damaged := append([]byte(nil), whole...)
	for i := len(damaged) / 3; i < len(damaged)/3+2000; i++ {
		damaged[i] = byte(i * 7919 % 251)
	}
	write("damaged.mkv", damaged)
	write("empty.mkv", nil)
	write("notes.mkv", []byte("not a video\n"))
	if err := syscall.Mkfifo(filepath.Join(lib, "pipe.mkv"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Some time in the past, so a touch by the sweep could not land on the same instant.
	past := time.Now().Add(-48 * time.Hour)
	_ = filepath.WalkDir(lib, func(p string, d fs.DirEntry, err error) error {
		if err == nil {
			_ = os.Chtimes(p, past, past)
		}
		return nil
	})

	before := snapshot(t, lib)

	// The ledger lives OUTSIDE the library, as the state directory does in the daemon.
	st := openLedger(t, filepath.Join(t.TempDir(), "jobs.db"))
	defer func() { _ = st.Close() }()
	var sources []string
	s := New(time.Hour, 2, st, func(stop func() bool, offer func(string) bool) {
		_ = filepath.WalkDir(lib, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || stop() {
				return nil
			}
			sources = append(sources, p)
			if !offer(p) {
				return fs.SkipAll
			}
			return nil
		})
	}, FFmpeg{Bin: bin}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ran, err := s.RunDue(ctx)
	if err != nil || !ran {
		t.Fatalf("RunDue: ran=%v err=%v", ran, err)
	}

	after := snapshot(t, lib)
	if len(after) != len(before) {
		t.Errorf("the library held %d entries before the sweep and %d after", len(before), len(after))
	}
	for i := 0; i < len(before) && i < len(after); i++ {
		if before[i] != after[i] {
			t.Errorf("an entry changed under a report-only sweep:\n  before %+v\n  after  %+v", before[i], after[i])
		}
	}
	t.Logf("%d entries snapshotted before and after a full sweep (name, size, mtime, mode, inode, sha256): identical=%v",
		len(before), !t.Failed())

	sw, ok, err := st.LastFinishedHealthSweep(ctx)
	if err != nil || !ok {
		t.Fatalf("no finished sweep: ok=%v err=%v", ok, err)
	}
	want := map[string]store.HealthResult{
		"/good.mkv": store.HealthOK, "/season/episode.mp4": store.HealthOK, "/mpeg4.avi": store.HealthOK,
		"/truncated.mkv": store.HealthCorrupt, "/damaged.mkv": store.HealthCorrupt,
		"/empty.mkv": store.HealthCorrupt, "/notes.mkv": store.HealthCorrupt,
		"/pipe.mkv": store.HealthUnreadable,
	}
	if len(sources) != len(want) {
		t.Fatalf("the sweep was offered %d file(s), want %d: %v", len(sources), len(want), sources)
	}
	for name, res := range want {
		c, ok, err := st.HealthCheckOf(ctx, sw.ID, lib+name)
		if err != nil || !ok {
			t.Errorf("%s: no result recorded (ok=%v err=%v)", name, ok, err)
			continue
		}
		t.Logf("%-20s %-10s %s", name, c.Result, c.Reason)
		if c.Result != res {
			t.Errorf("%s was reported %s, want %s", name, c.Result, res)
		}
		if res != store.HealthOK && c.Reason == "" {
			t.Errorf("%s was reported %s with no reason", name, c.Result)
		}
	}
	if sw.Counts != (store.HealthCounts{OK: 3, Corrupt: 4, Unreadable: 1}) {
		t.Errorf("the finished sweep counted %+v", sw.Counts)
	}
}

// TestFFmpeg_ADecoderThatCannotStartIsNoVerdict: a missing binary is ErrDecoderUnavailable,
// never a corrupt file.
func TestFFmpeg_ADecoderThatCannotStartIsNoVerdict(t *testing.T) {
	ok, reason, err := FFmpeg{Bin: filepath.Join(t.TempDir(), "no-such-ffmpeg")}.Decode(context.Background(), "/x.mkv")
	if !errors.Is(err, ErrDecoderUnavailable) || ok || reason != "" {
		t.Errorf("ok=%v reason=%q err=%v, want ErrDecoderUnavailable and no verdict", ok, reason, err)
	}
}

// TestFFmpeg_ACancelledDecodeIsNoVerdict: a decode the daemon's shutdown cut short records
// nothing - it is ctx's error, not a corrupt file.
func TestFFmpeg_ACancelledDecodeIsNoVerdict(t *testing.T) {
	dir := t.TempDir()
	slow := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	began := time.Now()
	ok, reason, err := FFmpeg{Bin: slow}.Decode(ctx, "/x.mkv")
	if took := time.Since(began); took > 10*time.Second {
		t.Errorf("a cancelled decode held its caller for %v; its output wait is bounded at %v", took, waitDelay)
	}
	if !errors.Is(err, context.DeadlineExceeded) || ok || reason != "" {
		t.Errorf("ok=%v reason=%q err=%v, want the context's error and no verdict", ok, reason, err)
	}
}

// TestFFmpeg_RunsAtTheLowestPriorityAndReadsThePathAsAFile: the decode runs niced to 19,
// and is handed the path through the file protocol with the gate's own decode flags.
func TestFFmpeg_RunsAtTheLowestPriorityAndReadsThePathAsAFile(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "ffmpeg")
	out := filepath.Join(dir, "seen")
	script := "#!/bin/sh\nsleep 0.3\necho \"$@\" > " + out + "\ncut -d' ' -f19 /proc/$$/stat >> " + out + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ok, reason, err := FFmpeg{Bin: fake}.Decode(context.Background(), "/lib/a:b.mkv")
	if err != nil || !ok || reason != "" {
		t.Fatalf("ok=%v reason=%q err=%v", ok, reason, err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("the fake saw %q", b)
	}
	if want := "-hide_banner -nostdin -v error -xerror -err_detect +explode -i file:/lib/a:b.mkv -map 0:v -f null -"; lines[0] != want {
		t.Errorf("argv %q, want %q", lines[0], want)
	}
	if lines[1] != "19" {
		t.Errorf("the decode ran at nice %s, want 19", lines[1])
	}
}

// TestFFmpeg_TheReasonIsTheLastErrorLinePrintableAndBounded checks the reason the ledger,
// the API and the notification carry.
func TestFFmpeg_TheReasonIsTheLastErrorLinePrintableAndBounded(t *testing.T) {
	for _, tc := range []struct {
		in   string
		code int
		want string
	}{
		{"first\n  last error  \n\n", 1, "last error"},
		{"", 183, "ffmpeg exited with status 183 and printed no error"},
		{"  \n", 1, "ffmpeg exited with status 1 and printed no error"},
		{"bad\x1b[0mline", 1, "bad?[0mline"},
		{strings.Repeat("é", maxReason+5), 1, strings.Repeat("é", maxReason) + "..."},
		{strings.Repeat("a", maxReason), 1, strings.Repeat("a", maxReason)},
	} {
		if got := reasonOf(tc.in, tc.code); got != tc.want {
			t.Errorf("reasonOf(%q, %d) = %q, want %q", tc.in, tc.code, got, tc.want)
		}
	}
	var tl tail
	_, _ = tl.Write([]byte(strings.Repeat("x", maxTail)))
	n, _ := tl.Write([]byte("end"))
	if n != 3 || len(tl.String()) != maxTail || !strings.HasSuffix(tl.String(), "xend") {
		t.Errorf("tail kept %d bytes ending %q", len(tl.String()), tl.String()[len(tl.String())-4:])
	}
}

// TestFFmpeg_AnErrorPrintedOnACleanExitFailsTheFile: the strict reading.
func TestFFmpeg_AnErrorPrintedOnACleanExitFailsTheFile(t *testing.T) {
	dir := t.TempDir()
	for name, script := range map[string]string{
		"clean": "#!/bin/sh\nexit 0\n",
		"warn":  "#!/bin/sh\necho 'File ended prematurely' >&2\nexit 0\n",
		"fail":  "#!/bin/sh\necho 'Invalid data found' >&2\nexit 183\n",
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]struct {
		ok     bool
		reason string
	}{
		"clean": {true, ""}, "warn": {false, "File ended prematurely"}, "fail": {false, "Invalid data found"},
	} {
		ok, reason, err := FFmpeg{Bin: filepath.Join(dir, name)}.Decode(context.Background(), "/x.mkv")
		if err != nil || ok != want.ok || reason != want.reason {
			t.Errorf("%s: ok=%v reason=%q err=%v, want %v %q", name, ok, reason, err, want.ok, want.reason)
		}
	}
}
