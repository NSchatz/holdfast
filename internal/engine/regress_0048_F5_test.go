package engine

import (
	"context"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// F5 (impl-gate ordinal 2) - criterion 6:
//
//	WHEN retention has pruned rows for files that are still present in the library THE
//	SYSTEM SHALL NOT thereby cause any of those files to be encoded again
//
// PruneTerminal's only exclusion is a `failed` row parked at max_failures. But a `done` or
// `skipped` row is ALSO what holds a file out of the encoder - Claim refuses those states
// outright, and the skip guards that would re-derive the same verdict run AFTER Claim. So a
// terminal row is a decision, not only a record, and the decision only re-derives itself if
// the configuration it was taken under has not moved.
//
// Two supported settings move it. Both are ordinary operator tuning, neither is exotic:
//
//   - `encoder` changed to a different target codec family (cpu/hevc -> svtav1/av1). Every
//     already-transcoded hevc file is now below the target, and its `done` row is the only
//     thing keeping it out of the encoder.
//   - `min_bitrate_kbps` lowered. Files previously recorded `skipped / low-bitrate` no
//     longer trip that guard, and their `skipped` row is the only thing keeping them out.
//
// In both cases enabling history_retention_rows deletes those rows and the NEXT SCAN ENCODES
// THE FILES. The control - the identical fixture with retention disabled - encodes nothing,
// ever, which is what makes the prune the cause rather than the configuration change.
//
// This is not merely wasted CPU: holdfast deletes the source after a successful swap, so an
// unintended re-encode is a second lossy generation of the operator's media and the loss of
// the original that produced it. It is also SPLIT-BRAIN - the files whose rows survived the
// prune stay untouched while the files whose rows went get encoded, so which of two
// identical files is re-encoded depends on where the retention boundary happened to fall.
//
// The shipped grader for this criterion (internal/engine/retention_test.go) exercises only a
// library at the engine's CURRENT target codec, which is the one configuration in which the
// already-at-target-codec guard re-derives the pruned verdict. This test is that grader one
// supported configuration over.
//
// Remove this file when the finding is addressed.

// seedTerminalRowForRealFile writes a terminal row keyed on the file's REAL fingerprint,
// which is what Claim looks up. retention_test.go's seedRow uses a literal "seed" key and so
// never collides with a scanned file; here the collision is the whole point.
func seedTerminalRowForRealFile(t *testing.T, ts *testStore, path string, st store.Status, o *store.Outcome) {
	t.Helper()
	ctx := context.Background()
	key := probe.Fingerprint(path)
	ok, err := ts.Claim(ctx, path, key, "w0", 3)
	if err != nil || !ok {
		t.Fatalf("seed claim %s: ok=%v err=%v", path, ok, err)
	}
	if err := ts.Finish(ctx, path, key, st, o); err != nil {
		t.Fatalf("seed finish %s: %v", path, err)
	}
}

// f5Case is one fixture: a library, the rows it already carries, and the configuration the
// operator has since moved to. retention is the only thing that varies between the two runs.
type f5Case struct {
	name string
	// build writes the library files and returns their paths.
	build func(t *testing.T, ffmpeg, root string) []string
	// row is the terminal row each of those files already carries.
	row func(path string) (store.Status, *store.Outcome)
	// cfg is the configuration the operator has moved to (applied to both runs).
	cfg func(c *config.Config)
}

func f5Encodes(t *testing.T, c f5Case, retention int) int32 {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	files := c.build(t, ffmpeg, root)

	var encodes atomic.Int32
	eng := buildEngine(t, ffmpeg, ffprobe, root, countingEncoder(&encodes), func(cfg *config.Config) {
		c.cfg(cfg)
		cfg.HistoryRetentionRows = retention
	})
	ts := eng.Store.(*testStore)
	for _, f := range files {
		st, o := c.row(f)
		seedTerminalRowForRealFile(t, ts, f, st, o)
	}

	// Scan one: with retention on, the prune runs at the end of it and takes the rows.
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("first RunOneshot: %v", err)
	}
	rowsAfterOne := len(terminalRows(t, ts))
	// Scan two: every file is still on disk. Criterion 6 says none of them may be encoded.
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("second RunOneshot: %v", err)
	}
	t.Logf("%s / history_retention_rows=%d: %d terminal rows survived scan one, %d encode(s) by the end of scan two",
		c.name, retention, rowsAfterOne, encodes.Load())
	return encodes.Load()
}

func TestRegress0048F5_APrunedTerminalRowHandsAStillPresentFileBackToTheEncoder(t *testing.T) {
	for _, c := range []f5Case{
		{
			name: "the operator moved the target codec from hevc to av1",
			build: func(t *testing.T, ffmpeg, root string) []string {
				var out []string
				for i := 0; i < 3; i++ {
					p := filepath.Join(root, "already-hevc"+strconv.Itoa(i)+".mkv")
					mkHevc(t, ffmpeg, p, "800k")
					out = append(out, p)
				}
				return out
			},
			row: func(string) (store.Status, *store.Outcome) {
				src, dst := int64(4096), int64(1024)
				return store.Done, &store.Outcome{Encoder: "cpu", SourceBytes: &src, OutputBytes: &dst}
			},
			cfg: func(c *config.Config) { c.Encoder = "svtav1" },
		},
		{
			name: "the operator lowered min_bitrate_kbps",
			build: func(t *testing.T, ffmpeg, root string) []string {
				var out []string
				for i := 0; i < 3; i++ {
					p := filepath.Join(root, "was-low-bitrate"+strconv.Itoa(i)+".mkv")
					mkH264(t, ffmpeg, p, "8M")
					out = append(out, p)
				}
				return out
			},
			row: func(string) (store.Status, *store.Outcome) {
				return store.Skipped, because(SkipLowBitrate)
			},
			// baseCfg already sets MinBitrateKbps: 0 - the threshold the operator has
			// lowered TO. The rows were recorded under a higher one.
			cfg: func(c *config.Config) { c.MinBitrateKbps = 0 },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			off := f5Encodes(t, c, 0)
			if off != 0 {
				t.Fatalf("the control encoded %d file(s) with retention DISABLED; the fixture is wrong, "+
					"not the claim", off)
			}
			on := f5Encodes(t, c, 1)
			if on > 0 {
				t.Errorf("history_retention_rows=1 pruned the terminal rows of files still present in the "+
					"library and the next scan handed %d of them to the encoder; the identical fixture with "+
					"retention disabled encodes none, ever.\n"+
					"Criterion 6: WHEN retention has pruned rows for files that are still present in the "+
					"library THE SYSTEM SHALL NOT thereby cause any of those files to be encoded again.\n"+
					"PruneTerminal excludes only a `failed` row parked at max_failures, but a done/skipped "+
					"row is equally a decision Claim enforces - and the guards that would re-derive it run "+
					"AFTER Claim, under whatever configuration is current.", on)
			}
		})
	}
}
