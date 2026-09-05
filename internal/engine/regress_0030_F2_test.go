// Refuter artifact for S0030-holdfast-dash-8, impl gate ordinal 1, finding F2.
// This file documents a defect; it is not a fix.
package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/probe"
)

// TestRegress0030F2_UnterminatedProgressLineStallsTheEncode drives the one thing the
// progress collector is never allowed to do: stall the encoder.
//
// scanProgressStream reads the pipe with a bufio.Scanner whose default 64 KiB token cap
// is kept deliberately. Its own doc comment states the property that cap breaks:
//
//	"It reads to EOF unconditionally, because the write end of that pipe is held by the
//	 running encoder and a reader that stopped early would eventually block it."
//
// A scanner that hits ErrTooLong stops early. The parent's read end is not closed until
// AFTER cmd.Wait() returns, so once the pipe buffer fills the encoder blocks on write,
// never exits, and cmd.Wait() never returns. The reporting path has then cost the encode
// itself - which the spec forbids in terms:
//
//	AC6: "IF the encoder emits malformed, partial or no progress output ... THEN THE
//	SYSTEM SHALL finish the job with the same outcome, exit status and reason text it
//	reaches with no progress collection"
//	Notes: "dropping a progress update is granularity lost, and it is never allowed to
//	be correctness lost or an encode stalled."
//
// The same fake, with a SMALL unterminated payload, is already exercised and passes
// (TestEncodeWithProgress_SilentOrMalformedStreamChangesNothing/"malformed"); the size is
// the whole difference, which is why the existing suite does not see this.
func TestRegress0030F2_UnterminatedProgressLineStallsTheEncode(t *testing.T) {
	realFFmpeg, realFFprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, realFFmpeg, src, "3M")

	// 512 KiB with no newline anywhere: past bufio.Scanner's 64 KiB token cap AND past
	// the kernel's 64 KiB pipe buffer, so the writer blocks once the reader gives up.
	fake := progressFake(t, d, "unterminated", strings.Repeat("a", 512*1024), "", "", 0)

	cfg := baseCfg(d)
	prober := probe.New(realFFmpeg, realFFprobe)
	enc := FFmpegEncoder{FFmpeg: fake, Cfg: cfg, Probe: prober}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // reaps the stuck child when this test fails

	var c collectProgress
	done := make(chan error, 1)
	go func() {
		done <- enc.EncodeWithProgress(ctx, src, filepath.Join(d, "out.mkv"), nil, c.sink)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a zero-exit encode must succeed regardless of its progress stream: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("EncodeWithProgress never returned: the progress reader stopped at the scanner's " +
			"64 KiB token cap, the encoder blocked writing to a full pipe, and cmd.Wait() will " +
			"not return. A reporting path has stalled the encode.")
	}
}
