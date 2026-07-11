package webui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The UI must be baked into the binary (go:embed) — non-empty and real HTML.
func TestEmbeddedIndexIsPresent(t *testing.T) {
	if len(indexHTML) == 0 {
		t.Fatal("index.html was not embedded (empty)")
	}
	s := string(indexHTML)
	for _, want := range []string{"<!DOCTYPE html>", "/api/events", "id=\"queue\"", "id=\"history\""} {
		if !strings.Contains(s, want) {
			t.Fatalf("embedded index.html missing %q", want)
		}
	}
}

func TestHandlerServesIndexAtRoot(t *testing.T) {
	h := Handler()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: code %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp == "" {
		t.Fatal("missing Content-Security-Policy header")
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "<title>transcode</title>") {
		t.Fatal("served body is not the dashboard page")
	}
}

func TestHandler404sOtherPaths(t *testing.T) {
	h := Handler()
	req := httptest.NewRequest(http.MethodGet, "/nope.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /nope.js: code %d, want 404", rec.Code)
	}
}
