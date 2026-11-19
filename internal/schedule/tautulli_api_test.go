package schedule

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestTautulli_API_SendsTheCommandAndItsParametersWithTheKey: API is the activity check's own
// request for another command - the resolved key and the command in the query, every caller
// parameter beside them - and returns the body as Tautulli sent it.
func TestTautulli_API_SendsTheCommandAndItsParametersWithTheKey(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2" {
			t.Errorf("path %q, want /api/v2", r.URL.Path)
		}
		got = r.URL.Query()
		_, _ = w.Write([]byte(`{"response":{"result":"success"}}`))
	}))
	defer srv.Close()

	ref, v := resolvedKey(t, apiKeySentinel)
	taut := NewTautulli(srv.URL+"/", ref, v)
	params := url.Values{"rating_key": {"77"}, "order": {"a", "b"}}
	body, err := taut.API(context.Background(), "get_metadata", params)
	if err != nil {
		t.Fatalf("API: %v", err)
	}
	if string(body) != `{"response":{"result":"success"}}` {
		t.Errorf("body %q", body)
	}
	if got.Get("apikey") != apiKeySentinel || got.Get("cmd") != "get_metadata" || got.Get("rating_key") != "77" ||
		strings.Join(got["order"], ",") != "a,b" {
		t.Errorf("query %v", got)
	}
	if strings.Join(params["order"], ",") != "a,b" || params.Get("apikey") != "" {
		t.Errorf("the caller's parameters were changed: %v", params)
	}
}

// TestTautulli_API_AFailureNamesTheCommandAndTheReferenceAndNeverTheKey: an error status and an
// outage each come back naming the command and the key's reference, and neither carries the key
// or the request URL. A caller that tries to set apikey or cmd itself is refused before anything
// is sent.
func TestTautulli_API_AFailureNamesTheCommandAndTheReferenceAndNeverTheKey(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	ref, v := resolvedKey(t, apiKeySentinel)
	taut := NewTautulli(srv.URL, ref, v)

	_, err := taut.API(context.Background(), "get_history", url.Values{"length": {"100"}})
	if err == nil {
		t.Fatal("a 401 was not a failure")
	}
	for _, want := range []string{"the get_history request", "HTTP 401", ref.String()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("failure %q does not name %q", err, want)
		}
	}
	var ae *APIError
	if !errors.As(err, &ae) || !ae.Answered() {
		t.Errorf("a 401 is an answer: %#v", err)
	}
	for _, leak := range []string{apiKeySentinel, "apikey=", "length=100"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("failure %q carries %q", err, leak)
		}
	}

	for _, k := range []string{"apikey", "cmd"} {
		before := calls
		_, err := taut.API(context.Background(), "get_history", url.Values{k: {"x"}})
		if err == nil || !strings.Contains(err.Error(), "the caller passed "+k) {
			t.Errorf("a caller-set %s: %v", k, err)
		}
		if calls != before {
			t.Errorf("a caller-set %s was sent", k)
		}
	}

	srv.Close() // now an outage: the transport error wraps the request URL
	_, err = taut.API(context.Background(), "get_history", nil)
	if err == nil {
		t.Fatal("an outage was not a failure")
	}
	if !strings.Contains(err.Error(), "the get_history request") || strings.Contains(err.Error(), apiKeySentinel) ||
		strings.Contains(err.Error(), "apikey") {
		t.Errorf("outage failure %q", err)
	}
	if !errors.As(err, &ae) || ae.Answered() {
		t.Errorf("an outage is not an answer: %#v", err)
	}
	// The activity check still names itself as it always has.
	if _, err := taut.Streaming(context.Background()); err == nil || !strings.Contains(err.Error(), "the activity check") {
		t.Errorf("Streaming's failure: %v", err)
	}
}
