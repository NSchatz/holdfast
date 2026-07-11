package schedule

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// throttleTTL bounds how often MayRunThrottled re-evaluates the (possibly
// network-bound) signals. The engine consults the scheduler between files, so
// without a cache a Tautulli query could run per file; 15s keeps host-fairness
// responsive without hammering the monitor.
const throttleTTL = 15 * time.Second

// Scheduler answers "may new transcode work start right now?" from three host-fair
// signals, in cheapest-first order: a daily run-window, a per-core CPU-load cap, and
// an optional Tautulli-aware pause (don't transcode while someone is streaming).
// It is advisory: it only DELAYS work. A monitoring outage (e.g. Tautulli
// unreachable) fails OPEN — a broken monitor must never permanently halt the tool.
type Scheduler struct {
	window   Window
	maxLoad  float64 // per-core 1-min load-average cap; <= 0 disables the check
	tautulli *Tautulli
	log      *slog.Logger

	// seams for tests
	now  func() time.Time
	load func() (perCore float64, ok bool)

	// throttle cache for MayRunThrottled (the between-files hot path)
	cacheMu     sync.Mutex
	cacheAt     time.Time
	cacheOK     bool
	cacheReason string
}

// New builds a Scheduler. Any of the three signals may be inert: an unset window,
// maxLoad <= 0, and a nil tautulli each disable their check. maxLoad is a per-core
// figure (e.g. 0.8 = allow up to 80% of NumCPU worth of load average).
func New(window Window, maxLoad float64, tautulli *Tautulli, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{
		window:   window,
		maxLoad:  maxLoad,
		tautulli: tautulli,
		log:      log,
		now:      time.Now,
		load:     loadPerCore,
	}
}

// MayRun reports whether new work may start now, and — when it may not — a short
// human reason (for logs and the API refusal). A Scheduler with no signals
// configured always returns (true, "").
func (s *Scheduler) MayRun(ctx context.Context) (bool, string) {
	if !s.window.Contains(s.now()) {
		return false, "outside run window " + s.window.String()
	}
	if s.maxLoad > 0 {
		if l, ok := s.load(); ok && l > s.maxLoad {
			return false, fmt.Sprintf("cpu load %.2f/core over cap %.2f", l, s.maxLoad)
		}
	}
	if s.tautulli != nil {
		streaming, err := s.tautulli.Streaming(ctx)
		if err != nil {
			// Fail OPEN: a monitoring outage must not block transcoding forever.
			s.log.Warn("tautulli check failed (allowing work — scheduling only delays)", "err", err)
		} else if streaming {
			return false, "media is currently streaming (Tautulli)"
		}
	}
	return true, ""
}

// MayRunThrottled is MayRun with a short result cache (throttleTTL), for the
// between-files hot path where the engine consults the scheduler frequently — it
// avoids issuing a Tautulli query (or a /proc read) per file. Freshness within a few
// seconds is plenty for host-fair scheduling.
func (s *Scheduler) MayRunThrottled(ctx context.Context) (bool, string) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if !s.cacheAt.IsZero() && s.now().Sub(s.cacheAt) < throttleTTL {
		return s.cacheOK, s.cacheReason
	}
	ok, reason := s.MayRun(ctx)
	s.cacheAt = s.now()
	s.cacheOK = ok
	s.cacheReason = reason
	return ok, reason
}

// loadPerCore reads /proc/loadavg (Linux) and returns the 1-minute load average
// divided by the CPU count. Returns ok=false when it can't be read (non-Linux, or an
// unreadable /proc) so the caller skips the load check rather than blocking work.
func loadPerCore() (float64, bool) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, false
	}
	one, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	n := runtime.NumCPU()
	if n < 1 {
		n = 1
	}
	return one / float64(n), true
}
