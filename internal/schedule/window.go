// Package schedule implements host-fair scheduling for the transcoder
// (TRANSCODE-8): a daily run-window, a CPU-load cap, and an optional Tautulli-aware
// pause. It only ever answers "may new work start now?" — it can DELAY work, never
// bypass a gate and never touch a file. When it says no, the engine simply stops
// feeding NEW files (an in-flight encode always finishes).
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Window is a daily time-of-day window during which work may run, in local time.
// A zero Window (Set == false) means "always in window". Start == End is rejected at
// parse time (an all-or-nothing window is almost certainly a mistake — use an empty
// string for always-on). When Start > End the window wraps past midnight
// (e.g. 22:00–06:00).
type Window struct {
	StartMin int // minutes since local midnight, [0,1440)
	EndMin   int // minutes since local midnight, [0,1440)
	Set      bool
}

// ParseWindow parses "HH:MM-HH:MM" (24-hour, local time). An empty string yields an
// unset Window (always in-window). Start == End is rejected (an all-or-nothing
// window is almost certainly a mistake; use "" for always-on).
func ParseWindow(s string) (Window, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Window{}, nil
	}
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return Window{}, fmt.Errorf("run_window %q must be HH:MM-HH:MM", s)
	}
	start, err := parseHHMM(strings.TrimSpace(a))
	if err != nil {
		return Window{}, fmt.Errorf("run_window start: %w", err)
	}
	end, err := parseHHMM(strings.TrimSpace(b))
	if err != nil {
		return Window{}, fmt.Errorf("run_window end: %w", err)
	}
	if start == end {
		return Window{}, fmt.Errorf("run_window %q has equal start and end (use an empty run_window for always-on)", s)
	}
	return Window{StartMin: start, EndMin: end, Set: true}, nil
}

func parseHHMM(s string) (int, error) {
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("%q must be HH:MM", s)
	}
	hh, err := strconv.Atoi(h)
	if err != nil || hh < 0 || hh > 23 {
		return 0, fmt.Errorf("%q has an invalid hour", s)
	}
	mm, err := strconv.Atoi(m)
	if err != nil || mm < 0 || mm > 59 {
		return 0, fmt.Errorf("%q has an invalid minute", s)
	}
	return hh*60 + mm, nil
}

// Contains reports whether local time t falls within the window. An unset window
// always contains t. A wrap-around window (Start > End) contains t if it is at/after
// Start OR before End. The window is half-open [Start, End).
func (w Window) Contains(t time.Time) bool {
	if !w.Set {
		return true
	}
	cur := t.Hour()*60 + t.Minute()
	if w.StartMin < w.EndMin {
		return cur >= w.StartMin && cur < w.EndMin
	}
	// wrap past midnight
	return cur >= w.StartMin || cur < w.EndMin
}

// String renders the window as HH:MM-HH:MM (or "always" when unset).
func (w Window) String() string {
	if !w.Set {
		return "always"
	}
	return fmt.Sprintf("%02d:%02d-%02d:%02d", w.StartMin/60, w.StartMin%60, w.EndMin/60, w.EndMin%60)
}
