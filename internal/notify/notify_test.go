package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newWithSink builds a Notifier whose send captures messages on a channel.
func newWithSink(t *testing.T, url string) (*Notifier, chan string) {
	t.Helper()
	sink := make(chan string, 16)
	n := New(url, discard())
	n.send = func(u, msg string) error {
		if u != url {
			t.Errorf("send called with url %q, want %q", u, url)
		}
		sink <- msg
		return nil
	}
	return n, sink
}

func recv(t *testing.T, sink chan string, within time.Duration) (string, bool) {
	t.Helper()
	select {
	case m := <-sink:
		return m, true
	case <-time.After(within):
		return "", false
	}
}

func TestNotify_PerFileFailure(t *testing.T) {
	n, sink := newWithSink(t, "generic://example")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	n.Observe(engine.Event{Status: store.Failed, Path: "/lib/broken.mkv"})

	msg, ok := recv(t, sink, 2*time.Second)
	if !ok {
		t.Fatal("no failure notification sent")
	}
	if !strings.Contains(msg, "/lib/broken.mkv") || !strings.Contains(msg, "FAILED") {
		t.Fatalf("failure message unexpected: %q", msg)
	}
}

func TestNotify_ScanSummary(t *testing.T) {
	n, sink := newWithSink(t, "generic://example")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	n.ScanStarted()
	n.Observe(engine.Event{Status: store.Done, BytesReclaimed: 3 * 1024 * 1024})
	n.Observe(engine.Event{Status: store.Done, BytesReclaimed: 1 * 1024 * 1024})
	n.Observe(engine.Event{Status: store.Skipped})
	n.Observe(engine.Event{Status: store.Failed, Path: "/x"}) // also fires a per-file msg
	n.ScanFinished()

	// Drain up to a few messages; find the summary.
	var summary string
	deadline := time.After(2 * time.Second)
	for summary == "" {
		select {
		case m := <-sink:
			if strings.Contains(m, "scan complete") {
				summary = m
			}
		case <-deadline:
			t.Fatal("no scan summary sent")
		}
	}
	for _, want := range []string{"2 transcoded", "4.0 MiB reclaimed", "1 skipped", "1 failed"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q missing %q", summary, want)
		}
	}
}

func TestNotify_DisabledSendsNothing(t *testing.T) {
	n, sink := newWithSink(t, "") // empty URL disables
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	if n.Enabled() {
		t.Fatal("empty URL must be disabled")
	}
	n.Observe(engine.Event{Status: store.Failed, Path: "/x"})
	n.ScanStarted()
	n.Observe(engine.Event{Status: store.Done, BytesReclaimed: 100})
	n.ScanFinished()

	if _, ok := recv(t, sink, 300*time.Millisecond); ok {
		t.Fatal("disabled notifier sent a message")
	}
}

// syncBuf is a concurrency-safe io.Writer for capturing log output.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The credential in a shoutrrr URL must NEVER reach the logs — shoutrrr's own error
// quotes the raw URL, so the notifier must log only a scheme-redacted label.
func TestNotify_SendFailureNeverLeaksURLCredential(t *testing.T) {
	const secretURL = "discord://SUPERSECRETTOKEN@channel12345"
	buf := &syncBuf{}
	n := New(secretURL, slog.New(slog.NewTextHandler(buf, nil)))
	// Simulate shoutrrr: its error embeds the raw URL verbatim.
	n.send = func(u, msg string) error {
		return fmt.Errorf("sending message via service at %q: connection refused", u)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	n.Observe(engine.Event{Status: store.Failed, Path: "/lib/x.mkv"})

	// Wait for the log line to appear.
	deadline := time.After(2 * time.Second)
	for !strings.Contains(buf.String(), "notification send failed") {
		select {
		case <-deadline:
			t.Fatal("no failure log line appeared")
		case <-time.After(10 * time.Millisecond):
		}
	}
	logged := buf.String()
	if strings.Contains(logged, "SUPERSECRETTOKEN") {
		t.Fatalf("credential leaked into logs:\n%s", logged)
	}
	if strings.Contains(logged, secretURL) {
		t.Fatalf("raw notify_url leaked into logs:\n%s", logged)
	}
	if !strings.Contains(logged, "discord://<redacted>") {
		t.Fatalf("expected a scheme-redacted service label; got:\n%s", logged)
	}
}

func TestRedactURL(t *testing.T) {
	cases := map[string]string{
		"discord://TOKEN@id":       "discord://<redacted>",
		"gotify://host/TOKEN":      "gotify://<redacted>",
		"slack://tokA/tokB/tokC":   "slack://<redacted>",
		"ntfy://ntfy.sh/topic?x=1": "ntfy://<redacted>",
		"":                         "",
		"garbage-no-scheme":        "<redacted>",
	}
	for in, want := range cases {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNotify_SendErrorNeverCrashesAndKeepsGoing(t *testing.T) {
	var calls atomic.Int32
	n := New("generic://example", discard())
	first := make(chan struct{})
	n.send = func(u, msg string) error {
		if calls.Add(1) == 1 {
			close(first)
			return errors.New("simulated send failure")
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	// First send errors — must be swallowed, and the worker must keep running.
	n.Observe(engine.Event{Status: store.Failed, Path: "/a"})
	<-first
	// A subsequent notification is still delivered (the loop survived the error).
	n.Observe(engine.Event{Status: store.Failed, Path: "/b"})
	deadline := time.After(2 * time.Second)
	for calls.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("worker stopped after a send error")
		case <-time.After(10 * time.Millisecond):
		}
	}
}
