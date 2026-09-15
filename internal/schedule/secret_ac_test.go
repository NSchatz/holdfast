package schedule

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/secret"
)

// apiKeySentinel is what the resolved Tautulli api key is, everywhere below. It is
// deliberately not credential-SHAPED: the secret scanner reads this file, and a fixture
// that looked like an issued token would make `make secret-scan` red by construction.
const apiKeySentinel = "RESOLVED-TAUTULLI-KEY-MUST-NOT-BE-LOGGED"

// resolvedKey builds the reference and value a real run would hand NewTautulli: the
// reference comes from a file on disk, which is the production path, not a hand-made Ref.
func resolvedKey(t *testing.T, value string) (secret.Ref, secret.Value) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tautulli.key")
	if err := os.WriteFile(path, []byte(value+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	ref, err := secret.ParseRef("tautulli_api_key", "file:"+path)
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
// LOCAL ENDPOINT THAT RECEIVES THE VALUE. The client under test is the real one and it
// issues a real HTTP GET; only the Tautulli server itself is stood in for, which is the
// network and therefore outside the boundary.
func TestTautulli_AC1_TheResolvedKeyReachesTheOutboundRequest(t *testing.T) {
	got := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.URL.Query().Get("apikey")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":{"result":"success","data":{"stream_count":"1"}}}`))
	}))
	defer srv.Close()

	ref, v := resolvedKey(t, apiKeySentinel)
	taut := NewTautulli(srv.URL, ref, v)
	if taut == nil {
		t.Fatal("NewTautulli returned nil for a configured base URL and a resolved key")
	}
	streaming, err := taut.Streaming(context.Background())
	if err != nil {
		t.Fatalf("Streaming: %v", err)
	}
	if !streaming {
		t.Error("the endpoint reported one active stream and Streaming said no")
	}
	select {
	case received := <-got:
		if received != apiKeySentinel {
			t.Fatalf("the endpoint received apikey=%q, want the resolved value %q", received, apiKeySentinel)
		}
	default:
		t.Fatal("the local endpoint was never called, so nothing received the resolved value")
	}
}

// AC-8: a failing outbound request names the key and its reference, carries neither the
// value nor any credential-bearing component of the destination, and the existing FAIL-OPEN
// behaviour is unchanged.
//
// This is a regression test for a live leak. net/http wraps every transport failure in a
// *url.Error carrying the REQUEST URL, and Tautulli takes its api key in the QUERY STRING -
// so `log.Warn("tautulli check failed", "err", err)` on the fail-open path printed the api
// key into the log on every Tautulli outage.
func TestTautulli_AC8_AnOutageNamesTheReferenceAndLeaksNeitherKeyNorURL(t *testing.T) {
	// A listener that is closed immediately, so the address is real and refuses.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	ref, v := resolvedKey(t, apiKeySentinel)
	taut := NewTautulli(deadURL, ref, v)
	if taut == nil {
		t.Fatal("NewTautulli returned nil")
	}

	buf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := New(Window{}, 0, taut, log)

	ok, reason := s.MayRun(context.Background())
	if !ok {
		t.Fatalf("a Tautulli outage must FAIL OPEN and allow work; got ok=%v reason=%q", ok, reason)
	}

	logged := buf.String()
	if !strings.Contains(logged, "tautulli check failed") {
		t.Fatalf("the outage was not reported at all:\n%s", logged)
	}
	if strings.Contains(logged, apiKeySentinel) {
		t.Fatalf("THE API KEY REACHED THE LOG - this is the leak AC-8 exists for:\n%s", logged)
	}
	if strings.Contains(logged, "apikey") {
		t.Fatalf("the credential-bearing query string reached the log:\n%s", logged)
	}
	if !strings.Contains(logged, "tautulli_api_key") || !strings.Contains(logged, ref.String()) {
		t.Errorf("the report does not name the key and its reference:\n%s", logged)
	}
}

// AC-8: the same sanitation on a MALFORMED base URL, where the error comes from url.Parse
// rather than from the transport. url.Parse's error is also a *url.Error carrying the whole
// raw URL, query string included, so this path leaks exactly as readily as the other one.
func TestTautulli_AC8_AMalformedDestinationDoesNotLeakTheKeyEither(t *testing.T) {
	ref, v := resolvedKey(t, apiKeySentinel)
	taut := NewTautulli("http://[::1", ref, v) // an unclosed bracket: url.Parse refuses it
	if taut == nil {
		t.Fatal("NewTautulli returned nil")
	}
	_, err := taut.Streaming(context.Background())
	if err == nil {
		t.Fatal("a malformed base URL produced no error")
	}
	if strings.Contains(err.Error(), apiKeySentinel) || strings.Contains(err.Error(), "apikey") {
		t.Fatalf("THE API KEY REACHED THE ERROR:\n%v", err)
	}
	if !strings.Contains(err.Error(), "tautulli_api_key") {
		t.Errorf("the error does not name the key:\n%v", err)
	}
}

// AC-8: the third trigger the criterion names, "returns an error status". The two graders
// above reach a refused connection and a destination url.Parse refuses; neither reaches a
// RESPONSE, which is the branch where naming the key matters most - an HTTP 401 from
// Tautulli means the api key itself is wrong, and the fail-open path logs this error
// verbatim as the whole report an operator gets. The no-leak half is asserted too, so the
// naming half cannot be bought by quoting the request URL.
func TestTautulli_AC8_AnErrorStatusNamesTheKeyAndItsReference(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			}))
			defer srv.Close()

			ref, v := resolvedKey(t, apiKeySentinel)
			taut := NewTautulli(srv.URL, ref, v)
			if taut == nil {
				t.Fatal("NewTautulli returned nil")
			}

			_, err := taut.Streaming(context.Background())
			if err == nil {
				t.Fatalf("an HTTP %d response produced no error", status)
			}
			if strings.Contains(err.Error(), apiKeySentinel) || strings.Contains(err.Error(), "apikey") {
				t.Fatalf("THE API KEY OR ITS QUERY STRING REACHED THE ERROR:\n%v", err)
			}
			if !strings.Contains(err.Error(), "tautulli_api_key") {
				t.Errorf("AC-8: the error for an HTTP %d response does not name the key:\n%v", status, err)
			}
			if !strings.Contains(err.Error(), ref.String()) {
				t.Errorf("AC-8: the error for an HTTP %d response does not name the reference %q:\n%v",
					status, ref.String(), err)
			}

			// And the same through the reporter an operator actually reads, where
			// fail-open must still hold.
			buf := &bytes.Buffer{}
			log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			s := New(Window{}, 0, NewTautulli(srv.URL, ref, v), log)
			ok, reason := s.MayRun(context.Background())
			if !ok {
				t.Fatalf("an HTTP %d from Tautulli must FAIL OPEN; got ok=%v reason=%q", status, ok, reason)
			}
			logged := buf.String()
			if strings.Contains(logged, apiKeySentinel) {
				t.Fatalf("the api key reached the log:\n%s", logged)
			}
			if !strings.Contains(logged, "tautulli_api_key") || !strings.Contains(logged, ref.String()) {
				t.Errorf("AC-8: the HTTP %d report does not name the key and its reference:\n%s", status, logged)
			}
			// observability O4: which dependency, what it tried, what happens next.
			if !strings.Contains(logged, "tautulli") ||
				!strings.Contains(logged, "activity check") ||
				!strings.Contains(logged, "allowing work") {
				t.Errorf("O4: the HTTP %d record does not state the dependency, the attempt and what follows:\n%s",
					status, logged)
			}
		})
	}
}

// AC-8: "any credential-bearing component of the destination" is not only the api key this
// client puts in the query string. tautulli_url is not a secret-bearing key, so nothing
// refuses an operator who writes basic-auth userinfo into it, and a failure report that
// quoted the configured URL whole would disclose it.
func TestTautulli_AC8_TheNamedDestinationCarriesNoUserinfo(t *testing.T) {
	const userinfoSentinel = "OPERATOR-BASIC-AUTH-MUST-NOT-BE-REPORTED"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	ref, v := resolvedKey(t, apiKeySentinel)
	taut := NewTautulli("http://operator:"+userinfoSentinel+"@"+host, ref, v)
	if taut == nil {
		t.Fatal("NewTautulli returned nil")
	}

	_, err := taut.Streaming(context.Background())
	if err == nil {
		t.Fatal("an HTTP 500 response produced no error")
	}
	if strings.Contains(err.Error(), userinfoSentinel) {
		t.Fatalf("THE DESTINATION'S OWN CREDENTIAL REACHED THE ERROR:\n%v", err)
	}
	if !strings.Contains(err.Error(), host) {
		t.Errorf("the report names no destination at all, so it says nothing about what failed:\n%v", err)
	}
	if !strings.Contains(err.Error(), "tautulli_api_key") || !strings.Contains(err.Error(), ref.String()) {
		t.Errorf("AC-8: the report does not name the key and its reference:\n%v", err)
	}
}

// AC-2: the api key does not appear in the scheduler's log at ANY level, including debug -
// the level an operator may legitimately turn all the way up.
func TestTautulli_AC2_TheKeyIsAbsentFromTheLogAtEveryLevel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"response":{"result":"success","data":{"stream_count":"2"}}}`))
	}))
	defer srv.Close()

	ref, v := resolvedKey(t, apiKeySentinel)
	buf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := New(Window{}, 0, NewTautulli(srv.URL, ref, v), log)
	if ok, _ := s.MayRun(context.Background()); ok {
		t.Error("two active streams should refuse new work")
	}
	if strings.Contains(buf.String(), apiKeySentinel) {
		t.Fatalf("the api key reached the log on the SUCCESS path:\n%s", buf.String())
	}
}
