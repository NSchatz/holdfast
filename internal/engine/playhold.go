package engine

import (
	"context"
	"time"
)

// DefaultPlayHoldPoll is how often a swap that is waiting for a playback to end asks again.
const DefaultPlayHoldPoll = 15 * time.Second

// playHeld is the play hold at the door of a job: it reports whether f is being played, and
// says so in one info record naming the path and the reason. A file it holds is not claimed
// and gets no row, so the next scan offers it again.
func (e *Engine) playHeld(ctx context.Context, f string) bool {
	if e.PlayHold == nil {
		return false
	}
	held, why := e.PlayHold(ctx, f)
	if !held {
		return false
	}
	e.Log.Info("held (not started; no row is written and the file is offered again on the next scan)",
		"file", f, "why", why)
	return true
}

// waitWhilePlayed is the play hold in front of the swap: it returns nil once f is not being
// played, asking again every PlayHoldPoll while it is, and returns the context's error if
// the run is interrupted first. It says that it is waiting once, and that it stopped once.
// It decides nothing else: every gate has already ruled, and the swap that follows is the
// swap that would have run.
func (e *Engine) waitWhilePlayed(ctx context.Context, f string) error {
	if e.PlayHold == nil {
		return nil
	}
	poll := e.PlayHoldPoll
	if poll <= 0 {
		poll = DefaultPlayHoldPoll
	}
	started := time.Now()
	waiting := false
	for {
		held, why := e.PlayHold(ctx, f)
		if err := ctx.Err(); err != nil {
			return err
		}
		if !held {
			if waiting {
				e.Log.Info("the swap proceeds: the file is no longer held", "file", f,
					"waited", time.Since(started).Round(time.Second).String())
			}
			return nil
		}
		if !waiting {
			waiting = true
			e.Log.Info("held (every gate passed; the swap waits until the file is no longer held)",
				"file", f, "why", why, "asking_again_every", poll.String())
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}
