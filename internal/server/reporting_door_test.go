package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/NSchatz/holdfast/internal/secret"
)

// [AC-7] WHEN the server reads the ledger to publish a frame or answer a read endpoint THE
// SYSTEM SHALL read through a handle the database itself refuses every write on.
//
// The cmd-side test of the same name proves the door IS read-only. This one proves the
// server's reads actually go through it: the hub is built on one handle and the Server on
// another, and every read endpoint plus the frame path has to land on the hub's. The two
// doubles are distinguishable only by which one was called, so a read that went to the
// wrong handle is visible and nothing else about the response changes.
func TestServe_ReadsGoThroughAReadOnlyHandle(t *testing.T) {
	reads, write := &countingStore{}, &countingStore{}
	ctrl := NewController(context.Background(), nil, discard())
	hub := NewHub(reads, ctrl, discard())
	srv := New(context.Background(), configZero(), secret.Value{}, secret.Value{},
		write, ctrl, hub, nil, discard())
	reads.reset()
	write.reset()

	ts := httptest.NewServer(srv)
	defer ts.Close()

	for _, path := range []string{"/api/summary", "/api/queue", "/api/history"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		code := resp.StatusCode
		_ = resp.Body.Close()
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, code)
		}
	}
	// And the frame path, which is the other half of the criterion.
	if _, err := hub.SnapshotJSON(context.Background()); err != nil {
		t.Fatalf("SnapshotJSON: %v", err)
	}

	if reads.reads() == 0 {
		t.Error("nothing was read through the reporting door at all")
	}
	if got := write.reads(); got != 0 {
		t.Errorf("the read endpoints and the frame path issued %d reads through the WRITE handle "+
			"(summary=%d list=%d count_rows=%d aggregates=%d held=%d): a reporting read is occupying "+
			"the connection the engine's writes queue on",
			got, write.summary, write.list, write.countRows, write.aggregates, write.held)
	}
}

// [AC-7] The complement: a build with no separate door still works. `reads` degrades to
// whatever handle the hub was given (AC-8's path), and the read endpoints answer from it -
// so the degrade is a change of DOOR and never a loss of the endpoint.
func TestServe_ReadsGoThroughAReadOnlyHandle_DegradedBuildStillAnswers(t *testing.T) {
	one := &countingStore{}
	ctrl := NewController(context.Background(), nil, discard())
	hub := NewHub(one, ctrl, discard())
	srv := New(context.Background(), configZero(), secret.Value{}, secret.Value{},
		one, ctrl, hub, nil, discard())
	one.reset()

	ts := httptest.NewServer(srv)
	defer ts.Close()

	for _, path := range []string{"/api/summary", "/api/queue", "/api/history"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		code := resp.StatusCode
		_ = resp.Body.Close()
		if code != http.StatusOK {
			t.Errorf("GET %s on a degraded build = %d, want 200", path, code)
		}
	}
	if one.reads() == 0 {
		t.Error("a degraded build read nothing: the endpoints went with the door")
	}
}
