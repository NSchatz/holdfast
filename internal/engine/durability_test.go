package engine

// TRANSCODE-17 — durable swap: survive power loss, not just a clean crash. os.Rename
// is atomic w.r.t. a concurrent reader, but atomicity is not PERSISTENCE: the rename
// (and the temp's own data) may live only in the page cache until fsync'd, so a POWER
// LOSS right after rename() returns can lose the rename — and in the ext-change case the
// source was already removed, so that window can leave the library entry pointing at
// nothing. The durable-rename discipline is: fsync the temp's DATA before the rename,
// then fsync the parent DIRECTORY after it (and, in the ext-change case, only remove the
// source once that directory fsync has made the rename durable).
//
// Power-loss durability itself CANNOT be proven in CI — it needs a power-cut/crash
// harness (which is exactly why the roadmap sequenced this after the fixture-bound -16
// safety work and marks the guarantee a documented limitation). What IS provable, and is
// proven here through the REAL swap path via the fsync seam, is the discipline's shape:
//
//   (1) the swap fsyncs the temp BEFORE the rename and the parent dir AFTER it, with the
//       dir fsync landing BEFORE the source is removed (order is the whole point — a dir
//       fsync after the remove would not close the "points at nothing" window);
//   (2) if the post-rename parent-dir fsync fails, the ext-change swap does NOT remove
//       the source — it leaves BOTH files (a duplicate, never a loss), identical to a
//       crash in that window; and
//   (3) if the temp cannot be fsync'd before the rename, the swap is refused outright and
//       the source is left byte-for-byte intact.
//
// Each reds on the un-hardened code (which fsyncs nothing, so the seam never fires): (1)
// records no fsyncs, (2) never fails the swap so the source is deleted, (3) never refuses
// so the source is replaced.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// buildEngineWithStore builds an engine over root and returns it together with its
// backing store, so a durability test can set an fsync seam AND assert on the recorded
// rows. Mirrors fixture (3)'s direct construction in hardening_test.go.
func buildEngineWithStore(t *testing.T, ffmpeg, ffprobe, root string) (*Engine, *testStore) {
	t.Helper()
	cfg := baseCfg(root) // ContainerExt "mkv": a movie.mp4 source is an ext-change swap
	ts := newTestStore(t, root)
	prober := probe.New(ffmpeg, ffprobe)
	eng := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, ts, discardLogger())
	return eng, ts
}

// TestDurability_SwapFsyncsTempBeforeRenameAndDirBeforeRemove is proof (1). It runs a
// normal, successful ext-change swap (movie.mp4 -> movie.mkv) with an fsync seam that
// RECORDS every fsync (and still performs the real one, so the swap completes), plus the
// hookAfterRename seam to mark the exact instant between the rename and the source
// removal. It asserts the durable-rename ordering: the temp is fsync'd first, and the
// parent directory is fsync'd AFTER the rename but BEFORE the source is removed.
//
// RED on the un-hardened code: it fsyncs nothing, so the seam records an empty sequence
// and the "temp fsync first" / "dir fsync before remove" assertions both fail.
func TestDurability_SwapFsyncsTempBeforeRenameAndDirBeforeRemove(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	mp4 := filepath.Join(d, "movie.mp4")
	mkv := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, mp4, "8M") // h264 source, inflated so a real encode shrinks it; ext change forces the two-step

	eng, _ := buildEngineWithStore(t, ffmpeg, ffprobe, d)

	var mu sync.Mutex
	var seq []string
	eng.fsyncPath = func(path string) error {
		mu.Lock()
		switch {
		case strings.Contains(path, TempMarker):
			seq = append(seq, "fsync-temp")
		case path == d:
			seq = append(seq, "fsync-dir")
		default:
			seq = append(seq, "fsync-other:"+path)
		}
		mu.Unlock()
		return fsyncPath(path) // perform the real fsync so the swap really completes
	}
	eng.hookAfterRename = func() error {
		mu.Lock()
		seq = append(seq, "between-rename-and-remove")
		mu.Unlock()
		return nil // proceed to the remove
	}

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	// The swap must have completed normally: source gone, final is the new HEVC file.
	if exists(mp4) {
		t.Error("ext-change swap left the original source in place")
	}
	if got := codecOf(t, ffprobe, mkv); got != "hevc" {
		t.Errorf("final file codec = %q, want hevc", got)
	}
	if nTemp(t, d) != 0 {
		t.Error("temp left behind after a successful durable swap")
	}

	// The load-bearing ordering.
	if len(seq) == 0 {
		t.Fatal("the swap fsync'd nothing — the durability discipline is absent (un-hardened code)")
	}
	if seq[0] != "fsync-temp" {
		t.Errorf("first durability op = %q, want fsync-temp (the temp's data must be durable before the rename); full sequence: %v", seq[0], seq)
	}
	firstDir, betweenIdx := -1, -1
	for i, op := range seq {
		if op == "fsync-dir" && firstDir == -1 {
			firstDir = i
		}
		if op == "between-rename-and-remove" {
			betweenIdx = i
		}
	}
	if firstDir == -1 {
		t.Fatalf("the parent directory was never fsync'd after the rename; sequence: %v", seq)
	}
	if betweenIdx == -1 {
		t.Fatalf("the rename/remove window marker never fired — the ext-change two-step did not run; sequence: %v", seq)
	}
	if firstDir > betweenIdx {
		t.Errorf("the parent dir was fsync'd (idx %d) AFTER the source was removed (window idx %d) — the rename is not made durable before the source disappears; sequence: %v", firstDir, betweenIdx, seq)
	}
}

// TestDurability_ParentDirFsyncFailureLeavesBothFiles is proof (2). The post-rename
// parent-directory fsync fails; because the rename is then not provably durable, the
// ext-change swap must NOT remove the source. Both files survive (a duplicate, never a
// loss), exactly as a crash in that window would leave them.
//
// RED on the un-hardened code: it never fsyncs the dir, so the failure never occurs, the
// swap runs to completion and the source is deleted — the mp4 is gone.
func TestDurability_ParentDirFsyncFailureLeavesBothFiles(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	mp4 := filepath.Join(d, "movie.mp4")
	mkv := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, mp4, "8M")
	origMD5 := md5f(t, mp4)

	eng, ts := buildEngineWithStore(t, ffmpeg, ffprobe, d)

	// Let the temp fsync (before the rename) succeed for real; fail the directory fsync
	// (after the rename), which is the durability step the source removal depends on.
	eng.fsyncPath = func(path string) error {
		if path == d {
			return errors.New("simulated power-unsafe directory: fsync refused")
		}
		return fsyncPath(path)
	}

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err) // the dir-fsync failure is logged + recorded, not surfaced
	}

	// No loss: both files present, the original byte-for-byte intact.
	if !exists(mkv) {
		t.Error("the renamed encode is missing after the dir-fsync failure")
	}
	if !exists(mp4) {
		t.Fatal("the source was removed under an un-fsync'd (not provably durable) rename — a power loss here could lose both")
	}
	if md5f(t, mp4) != origMD5 {
		t.Error("the source was modified despite the swap being aborted")
	}
	if got := codecOf(t, ffprobe, mp4); got != "h264" {
		t.Errorf("source codec = %q, want h264 (untouched)", got)
	}
	if nTemp(t, d) != 0 {
		t.Error("temp left behind (it should have been consumed by the rename)")
	}
	// The job row is left ACTIVE for the next RunOneshot's RecoverStale (it must not be
	// recorded Done — the swap did not complete safely).
	if ledgerHas(t, ts, store.Done, "movie") {
		t.Error("the aborted swap was recorded Done — it must stay active for reconciliation")
	}
}

// TestDurability_TempFsyncFailureRefusesSwap is proof (3). The temp cannot be fsync'd
// before the rename, so its data is not provably on disk; the swap is refused outright
// and the source is left byte-for-byte intact.
//
// RED on the un-hardened code: it never fsyncs the temp, so the failure never occurs and
// the swap replaces the source with the (un-fsync'd) encode.
func TestDurability_TempFsyncFailureRefusesSwap(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	mp4 := filepath.Join(d, "movie.mp4")
	mkv := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, mp4, "8M")
	origMD5 := md5f(t, mp4)

	eng, ts := buildEngineWithStore(t, ffmpeg, ffprobe, d)

	eng.fsyncPath = func(path string) error {
		if strings.Contains(path, TempMarker) {
			return errors.New("simulated: cannot fsync the temp")
		}
		return fsyncPath(path)
	}

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	// The swap was refused: source untouched, no final file, temp discarded.
	if !exists(mp4) || md5f(t, mp4) != origMD5 {
		t.Fatal("the source was mutated after the temp fsync failed — the swap should have been refused")
	}
	if got := codecOf(t, ffprobe, mp4); got != "h264" {
		t.Errorf("source codec = %q, want h264 (untouched)", got)
	}
	if exists(mkv) {
		t.Error("a final file was created despite the temp fsync failing before the rename")
	}
	if nTemp(t, d) != 0 {
		t.Error("temp not discarded after refusing the swap")
	}
	if r := failReason(t, ts, "movie.mp4"); !strings.Contains(r, "fsync temp before swap") {
		t.Errorf("expected a failed row citing the temp fsync, got %q", r)
	}
}
