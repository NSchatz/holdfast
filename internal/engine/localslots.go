package engine

import (
	"context"
	"sync"
	"time"
)

// The engine-wide local job bound: `workers` is how many files this process encodes ITSELF at
// once, across every pool that feeds ProcessFile - the scan's workers, the targeted-submission
// queue's and the watch's. Each pool is still `workers` wide, so none of them waits on its own
// width for a file another pool is not using, but every local job holds one of these slots for
// the whole of ProcessFile, from the guards and the claim through every gate and the swap. A
// job leased to a node does not take one: it is bounded by the node gate (nodes.go) instead.

// DefaultPausePoll is how often a submission or watch file waiting on Paused asks again.
// Paused itself is throttled where it reads the host (schedule.MayRunThrottled), so this is a
// bound on how late a resume is noticed, not on how often the host is read. ASSUMED.
const DefaultPausePoll = 2 * time.Second

// localSlots is the engine's slot pool, built on first use from the configured workers.
func (e *Engine) localSlots() chan struct{} {
	e.localSlotsOnce.Do(func() { e.localSlotPool = make(chan struct{}, e.Cfg.EffectiveWorkers()) })
	return e.localSlotPool
}

// holdLocalSlot takes one local job slot and returns the one way to give it back; calling that
// more than once is harmless. It blocks while every slot is held, and returns ctx's error,
// holding nothing, when ctx ends first.
//
// gated is for a file no scan feed stopped: a targeted submission's or the watch's. Such a
// file first WAITS while Paused says stop - pause, run_window or max_load, the same answer the
// scan feed stops on - and is asked again once it holds a slot, so a stop that landed while it
// waited for one gives the slot back and waits again rather than starting. Nothing is dropped
// and nothing is recorded while it waits: a cancellation then leaves the file exactly as it
// was. A scan's file is not gated here, because the feed already stopped handing files out
// and a file it handed out finishes, as it always has.
func (e *Engine) holdLocalSlot(ctx context.Context, gated bool) (release func(), err error) {
	slots := e.localSlots()
	for {
		if gated {
			if err := e.waitMayRun(ctx); err != nil {
				return nil, err
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case slots <- struct{}{}:
		}
		if gated && e.Paused != nil && e.Paused() {
			<-slots
			continue
		}
		var once sync.Once
		return func() { once.Do(func() { <-slots }) }, nil
	}
}

// waitMayRun blocks while Paused says stop, saying so once, and returns ctx's error if ctx
// ends first.
func (e *Engine) waitMayRun(ctx context.Context) error {
	said := false
	for e.Paused != nil && e.Paused() {
		if !said {
			said = true
			e.Log.Info("paused - a submitted or watched file waits to start; it is not dropped")
		}
		t := time.NewTimer(e.pausePollEvery())
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	return ctx.Err()
}

// pausePollEvery is how often waitMayRun asks Paused again.
func (e *Engine) pausePollEvery() time.Duration {
	if e.pausePoll > 0 {
		return e.pausePoll
	}
	return DefaultPausePoll
}
