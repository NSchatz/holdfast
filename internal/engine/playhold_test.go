package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// The play hold (docs/design/media-clients.md#play-hold), graded on the engine itself with a
// real encode behind it: a file being played is not started and gets no row; a swap whose
// file starts playing during the encode waits and then proceeds unchanged; and an interrupt
// during that wait is the interrupt of any job in flight.

// TestPlayHold_AFileBeingPlayedIsNotStartedAndGetsNoRow: held at the door, the file is not
// claimed, not encoded and not recorded, and once it is no longer held the same call
// processes it.
func TestPlayHold_AFileBeingPlayedIsNotStartedAndGetsNoRow(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, _ := scratchDirs(t)
	src := filepath.Join(root, "a.mkv")
	mkH264(t, ffmpeg, src, "8M")
	before, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, nil, nil)
	var logs bytes.Buffer
	eng.Log = slog.New(slog.NewTextHandler(&logs, nil))
	production := eng.Enc
	var encodes atomic.Int64
	eng.Enc = EncoderFunc(func(c context.Context, in, out string, props *probe.VideoProps) error {
		encodes.Add(1)
		return production.Encode(c, in, out, props)
	})
	var playing atomic.Bool
	playing.Store(true)
	var asked []string
	eng.PlayHold = func(_ context.Context, path string) (bool, string) {
		asked = append(asked, path)
		if playing.Load() {
			return true, "the file is being played in a test"
		}
		return false, ""
	}

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	if encodes.Load() != 0 {
		t.Fatal("a file being played was encoded")
	}
	if after, _ := os.ReadFile(src); !bytes.Equal(before, after) {
		t.Fatal("a file being played was modified")
	}
	if got := listDir(t, root); len(got) != 1 || got[0] != "a.mkv" {
		t.Fatalf("the library holds %v after a held pass, want only the source", got)
	}
	rows, err := ts.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("a held file was given a ledger row: %+v", rows)
	}
	if len(asked) != 1 || asked[0] != src {
		t.Errorf("the hold was asked about %v, want the source path once", asked)
	}
	if got := strings.Count(logs.String(), "held (not started"); got != 1 ||
		!strings.Contains(logs.String(), "level=INFO") || !strings.Contains(logs.String(), src) ||
		!strings.Contains(logs.String(), "the file is being played in a test") {
		t.Errorf("want one info record naming the path and the reason, got %d:\n%s", got, logs.String())
	}

	// No longer played: the next pass offers the same file, and it is swapped.
	playing.Store(false)
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	if codecOf(t, ffprobe, src) != "hevc" || !ledgerHas(t, ts, store.Done, "a.mkv") {
		t.Fatalf("the file was not transcoded and swapped once it stopped playing (codec %q)", codecOf(t, ffprobe, src))
	}
}

// TestPlayHold_TheSwapWaitsWhilePlayedThenProceedsAndAnInterruptLeavesTheSourceUntouched:
// a playback that starts after the encode began holds the rename. Interrupted while it
// waits, the job returns the cancellation, discards its working file, writes no terminal
// row and leaves the source byte for byte; left to run, it waits for as long as the file is
// played, then swaps exactly as it would have.
func TestPlayHold_TheSwapWaitsWhilePlayedThenProceedsAndAnInterruptLeavesTheSourceUntouched(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, _ := scratchDirs(t)
	src := filepath.Join(root, "a.mkv")
	mkH264(t, ffmpeg, src, "8M")
	before, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}

	eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, nil, nil)
	var logs bytes.Buffer
	eng.Log = slog.New(slog.NewTextHandler(&logs, nil))
	eng.PlayHoldPoll = 10 * time.Millisecond

	// The first question is the door's, answered "not played"; every later one is the swap's.
	// holdFor is how many of those say "played" before the playback stops; atPoll runs on each.
	var questions atomic.Int64
	hold := func(holdFor int64, atPoll func(n int64)) {
		questions.Store(0)
		eng.PlayHold = func(context.Context, string) (bool, string) {
			n := questions.Add(1)
			if n == 1 {
				return false, ""
			}
			if atPoll != nil {
				atPoll(n - 1)
			}
			if n-1 <= holdFor {
				return true, "the file is being played in a test"
			}
			return false, ""
		}
	}

	// Interrupted on the third question of the wait.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hold(1<<40, func(n int64) {
		// While the swap waits, the source is still the source.
		if now, _ := os.ReadFile(src); !bytes.Equal(before, now) {
			t.Error("the source changed while its swap was held")
		}
		if n == 3 {
			cancel()
		}
	})
	if err := eng.ProcessFile(ctx, "w0", src); !errors.Is(err, context.Canceled) {
		t.Fatalf("ProcessFile interrupted during the hold = %v, want the cancellation", err)
	}
	if after, _ := os.ReadFile(src); !bytes.Equal(before, after) {
		t.Fatal("the source was modified by a swap that was interrupted while held")
	}
	if got := listDir(t, root); len(got) != 1 || got[0] != "a.mkv" {
		t.Fatalf("the interrupted job left %v beside the source, want its working file discarded", got)
	}
	for _, terminal := range []store.Status{store.Done, store.Failed, store.Skipped, store.Indeterminate, store.AppliedDespiteError} {
		if ledgerHas(t, ts, terminal, "a.mkv") {
			t.Fatalf("the interrupted job wrote a terminal row (%s)", terminal)
		}
	}
	if got := questions.Load(); got != 4 {
		t.Errorf("the hold was asked %d time(s), want the door and three polls", got)
	}
	if strings.Count(logs.String(), "the swap waits until the file is no longer held") != 1 {
		t.Errorf("want one record saying the swap waits:\n%s", logs.String())
	}

	// The same file again, as the next start would offer it once the stale row is recovered:
	// held for five polls, then swapped, unchanged.
	if _, err := ts.RecoverStale(context.Background()); err != nil {
		t.Fatalf("RecoverStale: %v", err)
	}
	logs.Reset()
	hold(5, nil)
	began := time.Now()
	if err := eng.ProcessFile(context.Background(), "w0", src); err != nil {
		t.Fatalf("ProcessFile: %v", err)
	}
	if got := questions.Load(); got != 7 {
		t.Errorf("the hold was asked %d time(s), want the door, five held polls and the one that released", got)
	}
	if waited := time.Since(began); waited < 5*eng.PlayHoldPoll {
		t.Errorf("the job took %s: it did not wait a poll interval between questions", waited)
	}
	if codecOf(t, ffprobe, src) != "hevc" || !ledgerHas(t, ts, store.Done, "a.mkv") {
		t.Fatalf("the swap did not proceed once the playback stopped (codec %q)", codecOf(t, ffprobe, src))
	}
	for _, want := range []string{"the swap waits until the file is no longer held", "the swap proceeds: the file is no longer held"} {
		if strings.Count(logs.String(), want) != 1 {
			t.Errorf("want exactly one %q record:\n%s", want, logs.String())
		}
	}
}

// TestPlayHold_NoHoldConfiguredAsksNothingAndWaitsForNothing: an engine with no play hold -
// every engine built without a configured media server - neither holds nor waits.
func TestPlayHold_NoHoldConfiguredAsksNothingAndWaitsForNothing(t *testing.T) {
	eng := &Engine{Log: discardLogger()}
	if eng.playHeld(context.Background(), "/media/a.mkv") {
		t.Error("an engine with no play hold held a file")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := eng.waitWhilePlayed(ctx, "/media/a.mkv"); err != nil {
		t.Errorf("an engine with no play hold waited: %v", err)
	}
	// With one, a context cancelled before the first answer is the cancellation, not a swap.
	eng.PlayHold = func(context.Context, string) (bool, string) { return false, "" }
	if err := eng.waitWhilePlayed(ctx, "/media/a.mkv"); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait = %v, want the cancellation", err)
	}
	if err := eng.waitWhilePlayed(context.Background(), "/media/a.mkv"); err != nil {
		t.Errorf("a file that is not played = %v, want nil", err)
	}
	if DefaultPlayHoldPoll != 15*time.Second {
		t.Errorf("the default poll is %s; docs/post-swap-hook.md states 15 seconds", DefaultPlayHoldPoll)
	}
}
