package schedule

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func at(hh, mm int) time.Time {
	return time.Date(2026, 7, 11, hh, mm, 0, 0, time.Local)
}

func TestParseWindow(t *testing.T) {
	if w, err := ParseWindow(""); err != nil || w.Set {
		t.Fatalf("empty = %+v, %v; want unset, nil", w, err)
	}
	if _, err := ParseWindow("bogus"); err == nil {
		t.Fatal("bogus window should error")
	}
	if _, err := ParseWindow("25:00-06:00"); err == nil {
		t.Fatal("hour 25 should error")
	}
	if _, err := ParseWindow("01:60-06:00"); err == nil {
		t.Fatal("minute 60 should error")
	}
	if _, err := ParseWindow("02:00-02:00"); err == nil {
		t.Fatal("equal start/end should error")
	}
	w, err := ParseWindow("01:30-06:15")
	if err != nil || !w.Set || w.StartMin != 90 || w.EndMin != 375 {
		t.Fatalf("parse = %+v, %v", w, err)
	}
}

func TestWindow_Contains(t *testing.T) {
	unset, _ := ParseWindow("")
	if !unset.Contains(at(3, 0)) || !unset.Contains(at(15, 0)) {
		t.Fatal("unset window must always contain")
	}

	day, _ := ParseWindow("09:00-17:00")
	cases := []struct {
		t    time.Time
		want bool
	}{
		{at(8, 59), false},
		{at(9, 0), true}, // half-open: start included
		{at(12, 0), true},
		{at(16, 59), true},
		{at(17, 0), false}, // half-open: end excluded
		{at(23, 0), false},
	}
	for _, c := range cases {
		if got := day.Contains(c.t); got != c.want {
			t.Errorf("day.Contains(%02d:%02d) = %v, want %v", c.t.Hour(), c.t.Minute(), got, c.want)
		}
	}

	// Wrap-around window (overnight).
	night, _ := ParseWindow("22:00-06:00")
	wrap := []struct {
		t    time.Time
		want bool
	}{
		{at(22, 0), true},
		{at(23, 30), true},
		{at(0, 0), true},
		{at(5, 59), true},
		{at(6, 0), false},
		{at(12, 0), false},
		{at(21, 59), false},
	}
	for _, c := range wrap {
		if got := night.Contains(c.t); got != c.want {
			t.Errorf("night.Contains(%02d:%02d) = %v, want %v", c.t.Hour(), c.t.Minute(), got, c.want)
		}
	}
}

func TestMayRun_WindowGate(t *testing.T) {
	w, _ := ParseWindow("01:00-05:00")
	s := New(w, 0, nil, discard())
	s.now = func() time.Time { return at(3, 0) } // inside
	if ok, why := s.MayRun(context.Background()); !ok {
		t.Fatalf("inside window should run, got refuse: %q", why)
	}
	s.now = func() time.Time { return at(12, 0) } // outside
	if ok, why := s.MayRun(context.Background()); ok || why == "" {
		t.Fatalf("outside window should refuse with a reason, got ok=%v why=%q", ok, why)
	}
}

func TestMayRun_LoadCap(t *testing.T) {
	s := New(Window{}, 0.8, nil, discard())
	s.now = time.Now
	s.load = func() (float64, bool) { return 1.5, true } // over cap
	if ok, why := s.MayRun(context.Background()); ok || why == "" {
		t.Fatalf("over-cap load should refuse, got ok=%v", ok)
	}
	s.load = func() (float64, bool) { return 0.3, true } // under cap
	if ok, _ := s.MayRun(context.Background()); !ok {
		t.Fatal("under-cap load should run")
	}
	s.load = func() (float64, bool) { return 0, false } // unreadable → skip check
	if ok, _ := s.MayRun(context.Background()); !ok {
		t.Fatal("unreadable load must not block work")
	}
}

func TestMayRun_TautulliStreamingBlocksButOutageFailsOpen(t *testing.T) {
	// Streaming → refuse.
	taut := &Tautulli{baseURL: "x", apiKey: "y"}
	taut.get = func(ctx context.Context, u string) ([]byte, error) {
		return []byte(`{"response":{"result":"success","data":{"stream_count":"2"}}}`), nil
	}
	s := New(Window{}, 0, taut, discard())
	if ok, why := s.MayRun(context.Background()); ok || why == "" {
		t.Fatalf("active stream should refuse, got ok=%v", ok)
	}

	// No streams → run.
	taut.get = func(ctx context.Context, u string) ([]byte, error) {
		return []byte(`{"response":{"result":"success","data":{"stream_count":"0"}}}`), nil
	}
	if ok, _ := s.MayRun(context.Background()); !ok {
		t.Fatal("no streams should run")
	}

	// Outage → fail OPEN (allow work; a monitor failure must not halt the tool).
	taut.get = func(ctx context.Context, u string) ([]byte, error) {
		return nil, errors.New("connection refused")
	}
	if ok, _ := s.MayRun(context.Background()); !ok {
		t.Fatal("tautulli outage must fail open (allow work)")
	}
}

func TestTautulli_NewRequiresBoth(t *testing.T) {
	if NewTautulli("", "key") != nil {
		t.Error("empty base URL must yield nil")
	}
	if NewTautulli("http://host", "") != nil {
		t.Error("empty api key must yield nil")
	}
	if NewTautulli("http://host/", "key") == nil {
		t.Error("both provided must yield a client")
	}
}
