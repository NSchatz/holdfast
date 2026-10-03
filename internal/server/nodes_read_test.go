package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/node"
	"github.com/NSchatz/holdfast/internal/secret"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/version"
)

// GET /api/nodes: the worker nodes and their leases, as a READ. It sits behind the read
// gate and not the node gate, it serves no lease id, working path, digest or token, a
// figure that is not recorded is null and never zero, and reading it changes no lease.

// The recognisable values a lease row carries and the response must never serve. Each is a
// string no other part of a response could contain by accident.
const (
	secretLeaseID  = "1ea5e1d0000000000000000000000001"
	secretTemp     = "/lib/films/WORKINGFILE-7f3a.__transcoding__.mkv.holdfast-part"
	secretArgs     = "sha-256=:ARGSDIGEST7f3a:"
	secretOutput   = "sha-256=:OUTPUTDIGEST7f3a:"
	secretSource   = "sha-256=:SOURCEDIGEST7f3a:"
	secretJobKey   = "JOBKEY-7f3a:1700000000"
	nodesReadGrant = int64(1_800_000_000)
)

// seedLease grants one lease and, when to is not granted, moves it there.
func seedLease(t *testing.T, st *store.SQLite, id, path, nodeName string, grantedAt int64, to store.LeaseState, reason string, outputBytes int64) store.Lease {
	t.Helper()
	at := time.Unix(grantedAt, 0)
	l, err := st.GrantLease(context.Background(), path, func([]store.Lease, int64) (store.Lease, error) {
		return store.Lease{
			ID: id, Key: secretJobKey, Node: nodeName, State: store.LeaseGranted,
			ExpiresAt: at.Add(time.Minute), Temp: secretTemp, ReservedBytes: 9000, ArgsDigest: secretArgs,
			SourceSize: 123456, SourceModTime: time.Unix(1_700_000_000, 5), GrantedAt: at, UpdatedAt: at,
		}, nil
	})
	if err != nil {
		t.Fatalf("GrantLease(%s): %v", path, err)
	}
	if to == store.LeaseGranted {
		return l
	}
	l, err = st.UpdateLease(context.Background(), id, func(cur store.Lease) (store.Lease, error) {
		cur.State, cur.Reason, cur.UpdatedAt = to, reason, at.Add(10*time.Second)
		if outputBytes > 0 {
			cur.OutputBytes, cur.OutputDigest, cur.SourceDigest = outputBytes, secretOutput, secretSource
		}
		if !to.Live() {
			cur.EndedAt = at.Add(20 * time.Second)
		}
		return cur, nil
	})
	if err != nil {
		t.Fatalf("UpdateLease(%s): %v", id, err)
	}
	return l
}

// nodesHarness is a server with every credential configured (or none, with open) and a
// real lease hub recovered over whatever the seed wrote.
type nodesHarness struct {
	*harness
	ts  *httptest.Server
	hub *node.Hub
}

func newNodesHarness(t *testing.T, gated bool, seed func(st *store.SQLite)) *nodesHarness {
	t.Helper()
	var h *harness
	if gated {
		h = newHarnessWith(t, hookControlTok, hookReadTok)
	} else {
		h = newHarness(t, "")
	}
	h.srv.SetWebhookToken(secret.NewValue(hookTok))
	h.srv.SetNodeToken(secret.NewValue(nodeTok))
	if seed != nil {
		seed(h.st)
	}
	ctx, stop := context.WithCancel(context.Background())
	hub := node.New(node.Options{Ledger: h.st, BaseCtx: ctx, Version: version.Version,
		LongPoll: 30 * time.Second, Log: discard()})
	if _, err := hub.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	hub.Ready()
	h.srv.SetNodes(hub)
	ts := httptest.NewServer(h.srv)
	t.Cleanup(func() {
		stop() // releases every open poll, so Close does not wait one out
		ts.CloseClientConnections()
		ts.Close()
	})
	return &nodesHarness{harness: h, ts: ts, hub: hub}
}

// nodesWire is the response, every field raw enough to tell a null from a zero.
type nodesWire struct {
	Enabled *bool  `json:"enabled"`
	Now     *int64 `json:"now"`
	Nodes   []struct {
		Node         string          `json:"node"`
		Mode         *string         `json:"mode"`
		Encoders     json.RawMessage `json:"encoders"`
		Waiting      *bool           `json:"waiting"`
		CoolingUntil *int64          `json:"cooling_until"`
		LeasesActive *int            `json:"leases_active"`
	} `json:"nodes"`
	Leases []struct {
		Node        string  `json:"node"`
		Path        string  `json:"path"`
		State       string  `json:"state"`
		Epoch       int64   `json:"epoch"`
		GrantedAt   int64   `json:"granted_at"`
		UpdatedAt   int64   `json:"updated_at"`
		ExpiresAt   int64   `json:"expires_at"`
		EndedAt     *int64  `json:"ended_at"`
		Reason      *string `json:"reason"`
		SourceBytes *int64  `json:"source_bytes"`
		OutputBytes *int64  `json:"output_bytes"`
	} `json:"leases"`
	LeasesTotal rowTotalWire `json:"leases_total"`
}

// readNodes is one authorised GET /api/nodes: the body, decoded, and its raw top-level keys.
func readNodes(t *testing.T, base, auth string) (nodesWire, map[string]json.RawMessage, string) {
	t.Helper()
	resp, body := get(t, base, NodesReadPath, auth)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", NodesReadPath, resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("GET %s is %q, want JSON", NodesReadPath, ct)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &keys); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var out nodesWire
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out, keys, body
}

func objectKeys(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("not an object: %s", raw)
	}
	out := make([]string, 0, len(obj))
	for k := range obj {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- the gate ----------------------------------------------------------------------------

func TestNodesRead_IsBehindTheReadGateAndTheNodeTokenDoesNotOpenIt(t *testing.T) {
	h := newNodesHarness(t, true, func(st *store.SQLite) {
		seedLease(t, st, secretLeaseID, "/lib/films/a.mkv", "node-a", nodesReadGrant, store.LeaseGranted, "", 0)
	})
	if strings.HasPrefix(NodesReadPath, NodePathPrefix) {
		t.Fatalf("%s sits under the lease prefix %s, where the node token is the gate", NodesReadPath, NodePathPrefix)
	}

	for name, auth := range map[string]string{
		"no credential":         "",
		"the node token":        "Bearer " + nodeTok,
		"the webhook token":     "Bearer " + hookTok,
		"a near miss":           "Bearer " + hookReadTok + "x",
		"the read token, basic": "Basic " + hookReadTok,
	} {
		resp, body := get(t, h.ts.URL, NodesReadPath, auth)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401: %s", name, resp.StatusCode, body)
		}
		if strings.Contains(body, "node-a") || strings.Contains(body, "/lib/films") {
			t.Errorf("%s: a refused request was told about a node or a path: %s", name, body)
		}
	}
	for name, tok := range map[string]string{"the read token": hookReadTok, "the control token": hookControlTok} {
		got, _, _ := readNodes(t, h.ts.URL, "Bearer "+tok)
		if got.Enabled == nil || !*got.Enabled || len(got.Leases) != 1 {
			t.Errorf("%s: enabled=%v leases=%d, want the listing", name, got.Enabled, len(got.Leases))
		}
	}

	// With no read token configured the read surface is open, this route with it.
	open := newNodesHarness(t, false, nil)
	if got, _, _ := readNodes(t, open.ts.URL, ""); got.Enabled == nil || !*got.Enabled {
		t.Error("with no read token set the route did not answer an uncredentialled caller")
	}
	// And only GET is routed.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req, _ := http.NewRequest(method, open.ts.URL+NodesReadPath, strings.NewReader(`{}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405: the route is a read", method, NodesReadPath, resp.StatusCode)
		}
	}
}

// --- no hub wired ------------------------------------------------------------------------

func TestNodesRead_WithNoNodeHubIsDisabledWithEmptyArrays(t *testing.T) {
	h := newHarness(t, "")
	// Lease rows from a time nodes were on: with no hub wired they are not read.
	seedLease(t, h.st, secretLeaseID, "/lib/films/a.mkv", "node-a", nodesReadGrant, store.LeaseGranted, "", 0)
	ts := httptest.NewServer(h.srv)
	defer ts.Close()

	got, keys, body := readNodes(t, ts.URL, "")
	if got.Enabled == nil || *got.Enabled {
		t.Errorf("enabled = %v, want an explicit false", got.Enabled)
	}
	if string(keys["nodes"]) != "[]" || string(keys["leases"]) != "[]" {
		t.Errorf("nodes = %s and leases = %s, want two empty arrays and never null", keys["nodes"], keys["leases"])
	}
	requireAvailable(t, "leases_total", got.LeasesTotal, 0, nodeLeasesLimit)
	if got.Now == nil || *got.Now < time.Now().Unix()-60 || *got.Now > time.Now().Unix()+60 {
		t.Errorf("now = %v, want the server's clock", got.Now)
	}
	if strings.Contains(body, "node-a") {
		t.Errorf("a server that takes no nodes served a lease: %s", body)
	}
	if want := []string{"enabled", "leases", "leases_total", "nodes", "now"}; !reflect.DeepEqual(objectKeys(t, []byte(body)), want) {
		t.Errorf("the body's keys are %v, want %v", objectKeys(t, []byte(body)), want)
	}

	// It reads no ledger at all: a reporting handle that fails every read still answers.
	ctrl := NewController(context.Background(), func(context.Context) error { return nil }, discard())
	srv := New(context.Background(), configZero(), secret.Value{}, secret.Value{}, failingStore{}, ctrl,
		NewHub(failingStore{}, ctrl, discard()), nil, discard())
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, NodesReadPath, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Errorf("with no hub and no readable ledger: %d %s", rec.Code, rec.Body.String())
	}
}

// --- the shape, and what is never served -----------------------------------------------

func TestNodesRead_ServesTheContractShapeAndNeverALeaseIDPathDigestOrToken(t *testing.T) {
	const idAsReason = "1ea5e1d0000000000000000000000004"
	// Seeded AFTER the hub recovered, so each row is served exactly as it was written: a
	// recovery gives a live lease its grace and ends an uploaded one, which is the next
	// test's subject and not this one's.
	h := newNodesHarness(t, true, nil)
	func(st *store.SQLite) {
		seedLease(t, st, secretLeaseID, "/lib/films/granted.mkv", "node-a", nodesReadGrant+4, store.LeaseGranted, "", 0)
		seedLease(t, st, "1ea5e1d0000000000000000000000002", "/lib/films/uploaded.mkv", "node-a", nodesReadGrant+3, store.LeaseUploaded, "", 777)
		seedLease(t, st, "1ea5e1d0000000000000000000000003", "/lib/films/completed.mkv", "node-b", nodesReadGrant+2, store.LeaseCompleted, "", 888)
		// A node that stated its own lease id as its failure reason.
		seedLease(t, st, idAsReason, "/lib/films/failed.mkv", "node-b", nodesReadGrant+1, store.LeaseFailed, idAsReason, 0)
		seedLease(t, st, "1ea5e1d0000000000000000000000005", "/lib/films/expired.mkv", "node-c", nodesReadGrant, store.LeaseExpired, "expired", 0)
		seedLease(t, st, "1ea5e1d0000000000000000000000006", "/lib/films/failed2.mkv", "node-c", nodesReadGrant-1, store.LeaseFailed, "encode_failed", 0)
	}(h.st)
	got, keys, body := readNodes(t, h.ts.URL, "Bearer "+hookReadTok)

	// Never served, whatever the rows hold.
	for name, secretValue := range map[string]string{
		"a lease id": secretLeaseID, "a lease id stated as a reason": idAsReason, "the lease id prefix": "1ea5e1d0",
		"a working path": secretTemp, "the working file suffix": "holdfast-part",
		"the argument digest": secretArgs, "the output digest": secretOutput, "the source digest": secretSource,
		"any digest": "sha-256", "the job key": secretJobKey,
		"the node token": nodeTok, "the read token": hookReadTok, "the control token": hookControlTok, "the webhook token": hookTok,
	} {
		if strings.Contains(body, secretValue) {
			t.Errorf("the response carries %s (%q): %s", name, secretValue, body)
		}
	}
	// And the closed key sets: there is no field for any of them to arrive in.
	var rawLeases, rawNodes []json.RawMessage
	if err := json.Unmarshal(keys["leases"], &rawLeases); err != nil || len(rawLeases) != 6 {
		t.Fatalf("leases: %d entries, err %v", len(rawLeases), err)
	}
	if err := json.Unmarshal(keys["nodes"], &rawNodes); err != nil || len(rawNodes) != 3 {
		t.Fatalf("nodes: %d entries, err %v", len(rawNodes), err)
	}
	wantLeaseKeys := []string{"ended_at", "epoch", "expires_at", "granted_at", "node", "output_bytes", "path",
		"reason", "source_bytes", "state", "updated_at"}
	for i, raw := range rawLeases {
		if got := objectKeys(t, raw); !reflect.DeepEqual(got, wantLeaseKeys) {
			t.Errorf("lease %d carries %v, want exactly %v", i, got, wantLeaseKeys)
		}
	}
	wantNodeKeys := []string{"cooling_until", "encoders", "leases_active", "mode", "node", "waiting"}
	for i, raw := range rawNodes {
		if got := objectKeys(t, raw); !reflect.DeepEqual(got, wantNodeKeys) {
			t.Errorf("node %d carries %v, want exactly %v", i, got, wantNodeKeys)
		}
	}
	if want := []string{"enabled", "leases", "leases_total", "nodes", "now"}; !reflect.DeepEqual(objectKeys(t, []byte(body)), want) {
		t.Errorf("the body's keys are %v, want %v", objectKeys(t, []byte(body)), want)
	}

	// The leases: newest grant first, each field what the row holds, null where not recorded.
	type row struct {
		path, node, state string
		granted           int64
		ended             bool
		reason            string // "" is null
		output            int64  // 0 is null
	}
	want := []row{
		{"/lib/films/granted.mkv", "node-a", "granted", nodesReadGrant + 4, false, "", 0},
		{"/lib/films/uploaded.mkv", "node-a", "uploaded", nodesReadGrant + 3, false, "", 777},
		{"/lib/films/completed.mkv", "node-b", "completed", nodesReadGrant + 2, true, "", 888},
		{"/lib/films/failed.mkv", "node-b", "failed", nodesReadGrant + 1, true, "node_failed", 0},
		{"/lib/films/expired.mkv", "node-c", "expired", nodesReadGrant, true, "expired", 0},
		{"/lib/films/failed2.mkv", "node-c", "failed", nodesReadGrant - 1, true, "encode_failed", 0},
	}
	for i, w := range want {
		l := got.Leases[i]
		if l.Path != w.path || l.Node != w.node || l.State != w.state || l.Epoch != 1 || l.GrantedAt != w.granted {
			t.Errorf("lease %d = %+v, want %+v", i, l, w)
			continue
		}
		if l.SourceBytes == nil || *l.SourceBytes != 123456 {
			t.Errorf("%s: source_bytes = %v, want 123456", w.path, l.SourceBytes)
		}
		if l.ExpiresAt == 0 || l.UpdatedAt < l.GrantedAt {
			t.Errorf("%s: expires_at %d, updated_at %d", w.path, l.ExpiresAt, l.UpdatedAt)
		}
		if w.state == "granted" {
			if l.UpdatedAt != w.granted || l.ExpiresAt != w.granted+60 {
				t.Errorf("%s: updated_at %d and expires_at %d, want %d and %d", w.path, l.UpdatedAt, l.ExpiresAt, w.granted, w.granted+60)
			}
		} else if l.UpdatedAt != w.granted+10 {
			t.Errorf("%s: updated_at %d, want %d", w.path, l.UpdatedAt, w.granted+10)
		}
		switch {
		case w.ended && (l.EndedAt == nil || *l.EndedAt != w.granted+20):
			t.Errorf("%s: ended_at = %v, want %d", w.path, l.EndedAt, w.granted+20)
		case !w.ended && l.EndedAt != nil:
			t.Errorf("%s is live and carries ended_at %d, want null", w.path, *l.EndedAt)
		}
		switch {
		case w.reason == "" && l.Reason != nil:
			t.Errorf("%s carries reason %q, want null", w.path, *l.Reason)
		case w.reason != "" && (l.Reason == nil || *l.Reason != w.reason):
			t.Errorf("%s: reason = %v, want %q", w.path, l.Reason, w.reason)
		}
		switch {
		case w.output == 0 && l.OutputBytes != nil:
			t.Errorf("%s carries output_bytes %d before any upload was admitted, want null", w.path, *l.OutputBytes)
		case w.output != 0 && (l.OutputBytes == nil || *l.OutputBytes != w.output):
			t.Errorf("%s: output_bytes = %v, want %d", w.path, l.OutputBytes, w.output)
		}
	}
	requireAvailable(t, "leases_total", got.LeasesTotal, 6, nodeLeasesLimit)
	if got.LeasesTotal.Covers != "every lease in the ledger" {
		t.Errorf("leases_total covers %q", got.LeasesTotal.Covers)
	}

	// The nodes: each named by a listed lease alone, by name ascending, with nothing this
	// process was never told served as a value. The hub holds none of these leases - it
	// recovered before they were written - so none counts as active.
	for i, name := range []string{"node-a", "node-b", "node-c"} {
		n := got.Nodes[i]
		if n.Node != name {
			t.Fatalf("nodes[%d] = %s, want %s: the list is by name ascending", i, n.Node, name)
		}
		if n.Mode != nil || string(n.Encoders) != "null" || n.CoolingUntil != nil {
			t.Errorf("%s: mode %v, encoders %s, cooling_until %v; none is known, so each is null", name, n.Mode, n.Encoders, n.CoolingUntil)
		}
		if n.Waiting == nil || *n.Waiting {
			t.Errorf("%s: waiting = %v, want false", name, n.Waiting)
		}
		if n.LeasesActive == nil || *n.LeasesActive != 0 {
			t.Errorf("%s: leases_active = %v, want 0", name, n.LeasesActive)
		}
	}

	// The live body is what the surface document declares for the route.
	doc, err := h.srv.Surface()
	if err != nil {
		t.Fatal(err)
	}
	violations, err := doc.ValidateResponse(http.MethodGet, NodesReadPath, http.StatusOK, []byte(body))
	if err != nil || len(violations) > 0 {
		t.Errorf("the live body against the surface document: %v %v", err, violations)
	}
}

// --- what the hub knows ------------------------------------------------------------------

// A lease a restart recovered: the hub holds it, so its node has an active lease, and the
// hub has seen no poll from that node, so its mode and encoders are not known. An uploaded
// lease does not survive a restart, and the listing says how it ended.
func TestNodesRead_ARecoveredLeaseIsActiveForANodeWhoseModeIsNotKnown(t *testing.T) {
	h := newNodesHarness(t, false, func(st *store.SQLite) {
		seedLease(t, st, secretLeaseID, "/lib/films/one.mkv", "node-a", nodesReadGrant+2, store.LeaseGranted, "", 0)
		seedLease(t, st, "1ea5e1d0000000000000000000000002", "/lib/films/two.mkv", "node-a", nodesReadGrant+1, store.LeaseGranted, "", 0)
		seedLease(t, st, "1ea5e1d0000000000000000000000003", "/lib/films/up.mkv", "node-b", nodesReadGrant, store.LeaseUploaded, "", 555)
	})
	got, _, raw := readNodes(t, h.ts.URL, "")
	if len(got.Nodes) != 2 || got.Nodes[0].Node != "node-a" || got.Nodes[1].Node != "node-b" {
		t.Fatalf("nodes = %s", raw)
	}
	a, b := got.Nodes[0], got.Nodes[1]
	if a.LeasesActive == nil || *a.LeasesActive != 2 {
		t.Errorf("node-a: leases_active = %v, want its 2 recovered leases", a.LeasesActive)
	}
	if a.Mode != nil || string(a.Encoders) != "null" {
		t.Errorf("node-a has not polled since the restart: mode %v encoders %s, want null", a.Mode, a.Encoders)
	}
	if b.LeasesActive == nil || *b.LeasesActive != 0 {
		t.Errorf("node-b: leases_active = %v, want 0: its uploaded lease ended at the restart", b.LeasesActive)
	}
	if len(got.Leases) != 3 {
		t.Fatalf("leases = %s", raw)
	}
	for _, l := range got.Leases[:2] {
		if l.State != string(store.LeaseGranted) || l.EndedAt != nil || l.Reason != nil || l.OutputBytes != nil {
			t.Errorf("%s: %+v, want a granted lease with nothing ended and nothing uploaded", l.Path, l)
		}
	}
	up := got.Leases[2]
	if up.State != string(store.LeaseExpired) || up.Reason == nil || *up.Reason != string(node.ReasonRestart) || up.EndedAt == nil {
		t.Errorf("the uploaded lease after a restart: %+v, want expired with reason %s", up, node.ReasonRestart)
	}
}

func TestNodesRead_ANodeThatPolledStatesItsModeEncodersWaitAndCoolOff(t *testing.T) {
	h := newNodesHarness(t, true, nil)

	// node-w polls and waits; nothing is offered, so its poll stays open.
	body := fmt.Sprintf(`{"node":"node-w","version":%q,"slots":1,"mode":"http","encoders":["libsvtav1","libx265"]}`, version.Version)
	go func() {
		req, err := http.NewRequest(http.MethodPost, h.ts.URL+NodePathPrefix+node.RouteLeases, bytes.NewReader([]byte(body)))
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+nodeTok)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if v := h.hub.View(); len(v) == 1 && v[0].Waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the poll never reached the hub")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// node-c cools off: three of its leases ended in a row.
	before := time.Now().Unix()
	for i := 0; i < 3; i++ {
		h.hub.Report("node-c", "encode_failed")
	}

	got, _, raw := readNodes(t, h.ts.URL, "Bearer "+hookReadTok)
	if len(got.Nodes) != 2 || got.Nodes[0].Node != "node-c" || got.Nodes[1].Node != "node-w" {
		t.Fatalf("nodes = %s", raw)
	}
	c, w := got.Nodes[0], got.Nodes[1]
	if w.Mode == nil || *w.Mode != node.ModeHTTP {
		t.Errorf("node-w: mode = %v, want %q, the hub's own word", w.Mode, node.ModeHTTP)
	}
	if string(w.Encoders) != `["libsvtav1","libx265"]` {
		t.Errorf("node-w: encoders = %s", w.Encoders)
	}
	if w.Waiting == nil || !*w.Waiting {
		t.Errorf("node-w has a poll open and waiting = %v", w.Waiting)
	}
	if w.CoolingUntil != nil {
		t.Errorf("node-w is not cooling off and carries cooling_until %d", *w.CoolingUntil)
	}
	if w.LeasesActive == nil || *w.LeasesActive != 0 {
		t.Errorf("node-w: leases_active = %v, want 0", w.LeasesActive)
	}
	wantUntil := before + int64(node.DefaultCoolOff/time.Second)
	if c.CoolingUntil == nil || *c.CoolingUntil < wantUntil || *c.CoolingUntil > wantUntil+60 {
		t.Errorf("node-c: cooling_until = %v, want about %d", c.CoolingUntil, wantUntil)
	}
	if c.Mode != nil || string(c.Encoders) != "null" {
		t.Errorf("node-c never polled: mode %v encoders %s, want null", c.Mode, c.Encoders)
	}
	if string(mustKey(t, raw, "leases")) != "[]" {
		t.Errorf("no lease was granted and leases is %s, want []", mustKey(t, raw, "leases"))
	}
	requireAvailable(t, "leases_total", got.LeasesTotal, 0, nodeLeasesLimit)
}

func mustKey(t *testing.T, body, key string) json.RawMessage {
	t.Helper()
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &keys); err != nil {
		t.Fatal(err)
	}
	return keys[key]
}

// --- ordering, cap and total -----------------------------------------------------------

func TestNodesRead_ListsTheNewestLeasesUnderTheCapAndTotalsThemAll(t *testing.T) {
	const rows = nodeLeasesLimit + 7
	h := newNodesHarness(t, false, func(st *store.SQLite) {
		for i := 0; i < rows; i++ {
			// Four to a second, written out of order, all ended so the hub holds none.
			k := (i * 11) % rows
			seedLease(t, st, fmt.Sprintf("%032x", k+1), fmt.Sprintf("/lib/cap/%04d.mkv", k),
				fmt.Sprintf("node-%d", k%3), nodesReadGrant-int64(k/4), store.LeaseExpired, "expired", 0)
		}
	})
	got, _, _ := readNodes(t, h.ts.URL, "")
	if len(got.Leases) != nodeLeasesLimit {
		t.Fatalf("%d leases listed, want the cap of %d", len(got.Leases), nodeLeasesLimit)
	}
	for i, l := range got.Leases {
		// Position k in the order is path %04d of k: newest second first, then path.
		if want := fmt.Sprintf("/lib/cap/%04d.mkv", i); l.Path != want {
			t.Fatalf("leases[%d] is %s, want %s: newest grant first, then path ascending", i, l.Path, want)
		}
	}
	requireAvailable(t, "leases_total", got.LeasesTotal, rows, nodeLeasesLimit)
	if len(got.Nodes) != 3 {
		t.Errorf("%d nodes, want the three the listed leases name", len(got.Nodes))
	}
}

// --- the route changes no lease --------------------------------------------------------

func TestNodesRead_TwentyReadsChangeNoLeaseAndNothingTheHubHolds(t *testing.T) {
	h := newNodesHarness(t, false, func(st *store.SQLite) {
		seedLease(t, st, secretLeaseID, "/lib/films/granted.mkv", "node-a", nodesReadGrant, store.LeaseGranted, "", 0)
		seedLease(t, st, "1ea5e1d0000000000000000000000002", "/lib/films/uploaded.mkv", "node-b", nodesReadGrant, store.LeaseFailed, "encode_failed", 0)
	})
	ledger := func() []store.Lease {
		t.Helper()
		ls, err := h.st.RecentLeases(context.Background(), 100)
		if err != nil {
			t.Fatal(err)
		}
		return ls
	}
	before, viewBefore := ledger(), h.hub.View()
	jobsBefore, err := h.st.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _, first := readNodes(t, h.ts.URL, "")
	for i := 0; i < 20; i++ {
		if _, _, body := readNodes(t, h.ts.URL, ""); !sameJSONIgnoringNow(t, body, first) {
			t.Fatalf("read %d differs from the first:\n%s\n%s", i, body, first)
		}
	}
	if after := ledger(); !reflect.DeepEqual(after, before) {
		t.Fatalf("twenty reads changed the lease ledger:\n %+v\n %+v", before, after)
	}
	if after := h.hub.View(); !reflect.DeepEqual(after, viewBefore) {
		t.Fatalf("twenty reads changed what the hub holds:\n %+v\n %+v", viewBefore, after)
	}
	jobsAfter, err := h.st.List(context.Background(), nil, 0)
	if err != nil || !reflect.DeepEqual(jobsAfter, jobsBefore) {
		t.Fatalf("twenty reads changed the jobs table (err %v)", err)
	}
}

// --- a ledger that cannot be read --------------------------------------------------------

// leaseFailingStore fails exactly the lease reads it is told to, and is the real ledger for
// everything else.
type leaseFailingStore struct {
	*store.SQLite
	failList, failCount bool
}

func (s leaseFailingStore) RecentLeases(ctx context.Context, limit int) ([]store.Lease, error) {
	if s.failList {
		return nil, errors.New("simulated: the lease ledger could not be read")
	}
	return s.SQLite.RecentLeases(ctx, limit)
}

func (s leaseFailingStore) CountLeases(ctx context.Context) store.RowTotal {
	if s.failCount {
		return store.RowTotal{Coverage: store.Coverage{Set: "every lease in the ledger"},
			Err: errors.New("simulated: the lease ledger could not be counted")}
	}
	return s.SQLite.CountLeases(ctx)
}

// nodesServerOver serves reads through the given reporting handle, with a real lease hub.
func nodesServerOver(t *testing.T, st *store.SQLite, reads store.Store) *Server {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	ctrl := NewController(ctx, func(context.Context) error { return nil }, discard())
	srv := New(ctx, configZero(), secret.Value{}, secret.Value{}, st, ctrl, NewHub(reads, ctrl, discard()), nil, discard())
	hub := node.New(node.Options{Ledger: st, BaseCtx: ctx, Version: version.Version, Log: discard()})
	if _, err := hub.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	srv.SetNodes(hub)
	return srv
}

func TestNodesRead_AFailedLedgerReadAnswersAsTheOtherReadsDo(t *testing.T) {
	st := newStore(t)
	seedLease(t, st, secretLeaseID, "/lib/films/a.mkv", "node-a", nodesReadGrant, store.LeaseExpired, "expired", 0)
	serve := func(reads store.Store) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		nodesServerOver(t, st, reads).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, NodesReadPath, nil))
		return rec
	}

	t.Run("the listing cannot be read", func(t *testing.T) {
		rec := serve(leaseFailingStore{SQLite: st, failList: true})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code %d, want 500: %s", rec.Code, rec.Body.String())
		}
		if got := strings.TrimSpace(rec.Body.String()); got != "internal error reading nodes" {
			t.Errorf("body %q, want the fixed words the other reads answer with", got)
		}
		if strings.Contains(rec.Body.String(), "simulated") || strings.Contains(rec.Body.String(), "node-a") {
			t.Errorf("the refusal leaks the driver's error or a row: %s", rec.Body.String())
		}
	})

	t.Run("the reporting handle holds no lease ledger", func(t *testing.T) {
		rec := serve(failingStore{})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("code %d, want 500: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("only the total cannot be read: the leases still ship and the count is null", func(t *testing.T) {
		rec := serve(leaseFailingStore{SQLite: st, failCount: true})
		if rec.Code != http.StatusOK {
			t.Fatalf("code %d, want 200: %s", rec.Code, rec.Body.String())
		}
		var got nodesWire
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Leases) != 1 || got.Leases[0].Path != "/lib/films/a.mkv" {
			t.Errorf("an unreadable total cost the caller its leases: %s", rec.Body.String())
		}
		requireUnavailable(t, "leases_total", got.LeasesTotal)
		if got.LeasesTotal.Cap != nodeLeasesLimit || got.LeasesTotal.Covers == "" {
			t.Errorf("leases_total = %+v: it still states its cap and its set", got.LeasesTotal)
		}
		if !strings.Contains(rec.Body.String(), `"count":null`) || !strings.Contains(rec.Body.String(), `"age_seconds":null`) {
			t.Errorf("an unreadable total must carry an explicit null count and age: %s", rec.Body.String())
		}
	})

	t.Run("readable: the leases are read through the reporting handle", func(t *testing.T) {
		rec := serve(leaseFailingStore{SQLite: st})
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"count":1`) || !strings.Contains(rec.Body.String(), `"age_seconds":0`) {
			t.Fatalf("code %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// --- the surface document and the root page --------------------------------------------

func TestNodesRead_IsDescribedByTheSurfaceDocumentAndNamedOnTheRootPage(t *testing.T) {
	doc, err := ReferenceSurface()
	if err != nil {
		t.Fatal(err)
	}
	var statuses []int
	var ok Response
	for _, ep := range doc.Endpoints {
		if ep.Path != NodesReadPath {
			continue
		}
		if ep.Method != http.MethodGet {
			t.Errorf("the document describes %s %s; the route is GET only", ep.Method, ep.Path)
		}
		for _, r := range ep.Responses {
			statuses = append(statuses, r.Status)
			if r.Status == http.StatusOK {
				ok = r
			}
		}
	}
	if fmt.Sprint(statuses) != "[200 401 405 500]" {
		t.Fatalf("GET %s is declared with %v, want 200, 401 (the read gate), 405 and 500", NodesReadPath, statuses)
	}
	if ok.MediaType != mediaJSON || ok.Body.Kind != kindObject {
		t.Fatalf("the 200 is %q / %s", ok.MediaType, ok.Body.Kind)
	}
	declared := map[string]Shape{}
	for _, f := range ok.Body.Fields {
		declared[f.Name] = f.Type
	}
	for _, name := range []string{"enabled", "now", "nodes", "leases", "leases_total"} {
		if _, found := declared[name]; !found {
			t.Errorf("the 200 body does not declare %s", name)
		}
	}
	nullable := func(arr Shape, name string) (bool, bool) {
		if arr.Elem == nil {
			return false, false
		}
		for _, f := range arr.Elem.Fields {
			if f.Name == name {
				return f.Type.Nullable, true
			}
		}
		return false, false
	}
	for _, name := range []string{"mode", "encoders", "cooling_until"} {
		if isNull, found := nullable(declared["nodes"], name); !found || !isNull {
			t.Errorf("nodes[].%s must be declared nullable (declared %v)", name, found)
		}
	}
	for _, name := range []string{"ended_at", "reason", "output_bytes"} {
		if isNull, found := nullable(declared["leases"], name); !found || !isNull {
			t.Errorf("leases[].%s must be declared nullable (declared %v)", name, found)
		}
	}
	for _, name := range []string{"id", "lease_id", "temp", "temp_path", "args_digest", "output_digest", "source_digest", "job_key"} {
		if _, found := nullable(declared["leases"], name); found {
			t.Errorf("the document declares leases[].%s: that is never served", name)
		}
	}

	rec := httptest.NewRecorder()
	RootHandler()(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), NodesReadPath) {
		t.Errorf("the plain-text root page does not name %s: %s", NodesReadPath, rec.Body.String())
	}
}
