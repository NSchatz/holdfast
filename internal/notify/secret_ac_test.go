package notify

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/store"
)

// resolvedURL builds the reference and value a real run hands New: the shoutrrr URL comes
// off disk through the production resolver, not from a hand-made Value.
func resolvedURL(t *testing.T, url string) (secret.Ref, secret.Value) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify.url")
	if err := os.WriteFile(path, []byte(url+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	ref, err := secret.ParseRef("notify_url", "file:"+path)
	if err != nil {
		t.Fatalf("ParseRef: %v", err)
	}
	v, err := ref.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return ref, v
}

// AC-1: the resolved value is used in the outbound request the key exists for, proved by a
// LOCAL ENDPOINT THAT RECEIVES THE VALUE.
//
// For notify_url the resolved value IS the destination, credential and all, so "the
// endpoint received the value" means the request arrived at the path and with the token the
// reference resolved to. The real shoutrrr send path runs - the SendFunc seam is NOT used
// here - and only the notification service itself is stood in for, which is the network.
func TestNotify_AC1_TheResolvedURLIsWhatTheOutboundRequestReaches(t *testing.T) {
	type got struct {
		method, path, token, body string
	}
	received := make(chan got, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 4096)
		n, _ := r.Body.Read(b)
		received <- got{r.Method, r.URL.Path, r.URL.Query().Get("tok"), string(b[:n])}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// A shoutrrr generic URL whose query carries a token, which is the ordinary shape of a
	// credential-bearing notification URL.
	const token = "RESOLVED-NOTIFY-TOKEN-MUST-NOT-BE-LOGGED"
	url := "generic+http://" + srv.Listener.Addr().String() + "/holdfast-hook?tok=" + token

	ref, v := resolvedURL(t, url)
	n := New(ref, v, discard())
	if !n.Enabled() {
		t.Fatal("a resolved notify_url must enable notifications")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	n.Observe(engine.Event{Status: store.Failed, Path: "/lib/movie.mkv"})

	select {
	case r := <-received:
		if r.method != http.MethodPost {
			t.Errorf("the endpoint saw %s, want POST", r.method)
		}
		if r.path != "/holdfast-hook" {
			t.Errorf("the endpoint saw path %q, want the resolved one", r.path)
		}
		if r.token != token {
			t.Fatalf("the endpoint received tok=%q, want the resolved value %q", r.token, token)
		}
		if !strings.Contains(r.body, "movie.mkv") {
			t.Errorf("the notification body does not carry the event: %q", r.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the local endpoint was never called, so nothing received the resolved value")
	}
}

// AC-2: the resolved URL appears in no log line at any level, including debug, on the
// SUCCESS path. AC-8 covers the failure path; this covers the one where nothing went wrong
// and the daemon is merely chatty.
func TestNotify_AC2_TheResolvedURLIsAbsentFromTheLogAtEveryLevel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const token = "RESOLVED-NOTIFY-TOKEN-MUST-NOT-BE-LOGGED"
	url := "generic+http://" + srv.Listener.Addr().String() + "/hook?tok=" + token
	ref, v := resolvedURL(t, url)

	buf := &syncBuf{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	n := New(ref, v, log)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.Run(ctx)

	n.Observe(engine.Event{Status: store.Failed, Path: "/lib/a.mkv"})
	n.ScanStarted()
	n.Observe(engine.Event{Status: store.Done, Path: "/lib/b.mkv"})
	n.ScanFinished()
	time.Sleep(300 * time.Millisecond)

	logged := buf.String()
	if strings.Contains(logged, token) {
		t.Fatalf("the resolved notification token reached the log:\n%s", logged)
	}
	if strings.Contains(logged, url) {
		t.Fatalf("the resolved notification URL reached the log:\n%s", logged)
	}
}

// AC-8: a full queue drops a message and logs it, and that log line must not carry the
// destination either. It is the one log line in this package that prints a message body,
// and a body is the operator's text rather than the credential - so this pins that the line
// stays free of the URL while remaining useful.
func TestNotify_AC8_TheQueueFullLineCarriesNoDestination(t *testing.T) {
	const token = "RESOLVED-NOTIFY-TOKEN-MUST-NOT-BE-LOGGED"
	ref, v := resolvedURL(t, "generic+http://127.0.0.1:1/hook?tok="+token)
	buf := &syncBuf{}
	n := New(ref, v, slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	// Run is deliberately NOT started, so nothing drains the queue and it fills.
	for i := 0; i < 200; i++ {
		n.Observe(engine.Event{Status: store.Failed, Path: "/lib/x.mkv"})
	}
	logged := buf.String()
	if !strings.Contains(logged, "queue full") {
		t.Fatalf("the queue never reported filling:\n%s", logged)
	}
	if strings.Contains(logged, token) || strings.Contains(logged, "generic+http") {
		t.Fatalf("the drop line carries the destination:\n%s", logged)
	}
}
