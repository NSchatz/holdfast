package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"
)

// The hub's reporting view (GET /api/nodes): what each node's last poll stated, whether a
// poll of its is open, its cool-off and its live leases - a copy of what the hub holds,
// that changes nothing.

func viewOf(t *testing.T, h *Hub, node string) (NodeView, bool) {
	t.Helper()
	for _, v := range h.View() {
		if v.Node == node {
			return v, true
		}
	}
	return NodeView{}, false
}

// pollAs sends one acquire for a node in a mode with encoders, in the background, and
// returns once the hub holds it queued.
func (f *fixture) pollAs(node, mode string, encoders ...string) chan reply {
	f.t.Helper()
	before := f.queuedPolls()
	answered := make(chan reply, 1)
	body, err := json.Marshal(AcquireRequest{Node: node, Version: testVersion, Slots: 1, Mode: mode, Encoders: encoders})
	if err != nil {
		f.t.Fatal(err)
	}
	go func() {
		// A poll still open when its test ends is cut off by the fixture's cleanup; that is
		// not a failure of anything, so an error here answers nothing rather than failing.
		resp, err := http.Post(f.srv.URL+mount+RouteLeases, "application/json", bytes.NewReader(body))
		if err != nil {
			return
		}
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		answered <- reply{status: resp.StatusCode, header: resp.Header, body: raw}
	}()
	eventually(f.t, "the poll of "+node+" is queued", func() bool { return f.queuedPolls() == before+1 })
	return answered
}

func TestView_AHubThatHasSeenNothingKnowsNoNode(t *testing.T) {
	f := newFixture(t, nil)
	if got := f.hub.View(); got == nil || len(got) != 0 {
		t.Fatalf("View() = %#v, want an empty, non-nil list", got)
	}
}

func TestView_ANodeIsWhatItsLastPollStatedByNameAscending(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.LongPoll = 30 * time.Second })
	zulu := f.pollAs("zulu", ModeHTTP, "libsvtav1")
	alpha := f.pollAs("alpha", ModeMapped, "libx265", "hevc_nvenc")
	mike := f.pollAs("mike", ModeMapped, "libx265")

	got := f.hub.View()
	want := []NodeView{
		{Node: "alpha", Mode: ModeMapped, Encoders: []string{"libx265", "hevc_nvenc"}, Waiting: true},
		{Node: "mike", Mode: ModeMapped, Encoders: []string{"libx265"}, Waiting: true},
		{Node: "zulu", Mode: ModeHTTP, Encoders: []string{"libsvtav1"}, Waiting: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("View() =\n %+v\nwant\n %+v", got, want)
	}

	// The view is a COPY: writing to it reaches nothing the hub holds.
	got[0].Encoders[0] = "tampered"
	got[0].Node = "tampered"
	if again := f.hub.View(); !reflect.DeepEqual(again, want) {
		t.Fatalf("a caller's write to the view changed the hub's own state:\n %+v", again)
	}

	// The engine shuts down: the polls are released, and the nodes are still known by what
	// they last stated, no longer waiting.
	f.stop()
	for _, ch := range []chan reply{zulu, alpha, mike} {
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatal("a poll was not released")
		}
	}
	for i := range want {
		want[i].Waiting = false
	}
	if got := f.hub.View(); !reflect.DeepEqual(got, want) {
		t.Fatalf("after the polls ended View() =\n %+v\nwant\n %+v", got, want)
	}
}

func TestView_ALaterPollReplacesWhatAnEarlierOneStated(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.LongPoll = 20 * time.Millisecond })
	if r := f.post(RouteLeases, AcquireRequest{Node: "node-a", Version: testVersion, Slots: 1, Mode: ModeMapped, Encoders: []string{"libx265"}}); r.status != http.StatusNoContent {
		t.Fatalf("the first poll answered %d %s", r.status, r.body)
	}
	v, ok := viewOf(t, f.hub, "node-a")
	if !ok || v.Mode != ModeMapped || fmt.Sprint(v.Encoders) != "[libx265]" || v.Waiting {
		t.Fatalf("after a poll that ran out: %+v (known %v), want mapped, [libx265], not waiting", v, ok)
	}
	if r := f.post(RouteLeases, AcquireRequest{Node: "node-a", Version: testVersion, Slots: 1, Mode: ModeHTTP, Encoders: []string{"libsvtav1", "libx264"}}); r.status != http.StatusNoContent {
		t.Fatalf("the second poll answered %d %s", r.status, r.body)
	}
	v, _ = viewOf(t, f.hub, "node-a")
	if v.Mode != ModeHTTP || fmt.Sprint(v.Encoders) != "[libsvtav1 libx264]" {
		t.Fatalf("the view still states the earlier poll: %+v", v)
	}
	if got := len(f.hub.View()); got != 1 {
		t.Fatalf("one node polled twice and the view lists %d nodes", got)
	}
}

// A poll the hub refuses before it reads it as a poll of this build - a bad body, another
// version, a mode this build does not serve - states nothing the view repeats.
func TestView_ARefusedPollIsNotANodeTheHubKnows(t *testing.T) {
	f := newFixture(t, nil)
	for name, req := range map[string]AcquireRequest{
		"another version":  {Node: "node-v", Version: "v9.9.9", Slots: 1, Mode: ModeMapped, Encoders: []string{"libx265"}},
		"an unserved mode": {Node: "node-m", Version: testVersion, Slots: 1, Mode: "carrier-pigeon", Encoders: []string{"libx265"}},
		"no encoders":      {Node: "node-e", Version: testVersion, Slots: 1, Mode: ModeMapped},
	} {
		if r := f.post(RouteLeases, req); r.status < 400 || r.status >= 500 {
			t.Fatalf("%s answered %d, want a 4xx", name, r.status)
		}
	}
	if got := f.hub.View(); len(got) != 0 {
		t.Fatalf("refused polls left the hub knowing %+v", got)
	}

	// A hub that is not leasing yet answers 503 and records nothing either.
	cold := &fixture{t: t, dir: t.TempDir(), clk: &clock{now: t0}, space: &space{free: 1 << 40}}
	cold.dbPath = newLedgerFile(t)
	cold.open(nil)
	if r := cold.acquire("node-a"); r.status != http.StatusServiceUnavailable {
		t.Fatalf("a poll before Recover answered %d", r.status)
	}
	if got := cold.hub.View(); len(got) != 0 {
		t.Fatalf("a hub that is not leasing knows %+v", got)
	}
}

func TestView_CountsLiveLeasesAndSaysWhoIsWaiting(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.LongPoll = 30 * time.Second; o.MaxLeasesPerNode = 2 })
	a1, call1 := f.grant("node-a", f.job("one", 4000))
	_, call2 := f.grant("node-a", f.job("two", 4000))

	v, ok := viewOf(t, f.hub, "node-a")
	if !ok || v.LiveLeases != 2 || v.Waiting {
		t.Fatalf("with two leases granted and no poll open: %+v (known %v)", v, ok)
	}
	if v.Mode != ModeMapped || fmt.Sprint(v.Encoders) != "[libx265 libsvtav1]" {
		t.Errorf("the granted node's last poll is not on its view: %+v", v)
	}

	// One lease ends: the count follows, and the lease's ending is not the view's doing.
	if r := f.fail(a1.LeaseID, a1.Epoch, "encode_failed"); r.status != http.StatusOK {
		t.Fatalf("fail answered %d %s", r.status, r.body)
	}
	if _, err := call1.wait(t); err == nil {
		t.Fatal("the failed lease's engine call returned no error")
	}
	if v, _ := viewOf(t, f.hub, "node-a"); v.LiveLeases != 1 {
		t.Fatalf("one of two leases ended and the view counts %d live", v.LiveLeases)
	}

	// A second node waits; nothing is offered it, so its poll stays open.
	f.pollAs("node-b", ModeHTTP, "libx265")
	if v, _ := viewOf(t, f.hub, "node-b"); !v.Waiting || v.LiveLeases != 0 {
		t.Fatalf("a node with an open poll and no lease: %+v", v)
	}
	if v, _ := viewOf(t, f.hub, "node-a"); v.Waiting {
		t.Fatal("node-b's open poll made node-a read as waiting")
	}
	call2.cancel()
	if _, err := call2.wait(t); err == nil {
		t.Fatal("the cancelled lease's engine call returned no error")
	}
	if v, _ := viewOf(t, f.hub, "node-a"); v.LiveLeases != 0 {
		t.Fatalf("every lease ended and the view counts %d live", v.LiveLeases)
	}
}

// A poll RESERVED for a job (the engine holds its ticket and has not granted yet) is still a
// poll that is open; one whose long-poll ran out while it was reserved is not.
func TestView_AReservedPollIsWaitingUntilItsNodeIsToldThereIsNoWork(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.LongPoll = 400 * time.Millisecond })
	answered := f.pollAs("node-a", ModeMapped, "libx265")
	ticket, err := f.hub.WaitDemand(context.Background())
	if err != nil {
		t.Fatalf("WaitDemand: %v", err)
	}
	if f.queuedPolls() != 0 {
		t.Fatal("the reserved poll is still queued")
	}
	if v, _ := viewOf(t, f.hub, "node-a"); !v.Waiting {
		t.Fatalf("a poll reserved for a job is not waiting: %+v", v)
	}
	select {
	case r := <-answered:
		if r.status != http.StatusNoContent {
			t.Fatalf("the long-poll ran out and answered %d", r.status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the long-poll never ran out")
	}
	if v, _ := viewOf(t, f.hub, "node-a"); v.Waiting {
		t.Fatalf("the node was told there is no work and still reads as waiting: %+v", v)
	}
	f.hub.Release(ticket, nil)
	if v, ok := viewOf(t, f.hub, "node-a"); !ok || v.Waiting {
		t.Fatalf("after the spent ticket was released: %+v (known %v)", v, ok)
	}
}

func TestView_CoolingUntilIsTheCoolOffInForceAndZeroOtherwise(t *testing.T) {
	f := newFixture(t, nil)
	f.hub.Report("node-a", "encode_failed")
	f.hub.Report("node-a", "encode_failed")
	// Two in a row is a run, not a cool-off: nothing is in force, and a node known only
	// by a run that has not tripped is not listed for it.
	if v, ok := viewOf(t, f.hub, "node-a"); ok {
		t.Fatalf("a run of two listed the node: %+v", v)
	}
	f.hub.Report("node-a", "encode_failed")
	want := t0.Add(5 * time.Minute)
	v, ok := viewOf(t, f.hub, "node-a")
	if !ok || !v.CoolingUntil.Equal(want) {
		t.Fatalf("after three in a row: %+v (known %v), want cooling until %s", v, ok, want)
	}
	if v.Mode != "" || v.Encoders != nil || v.Waiting || v.LiveLeases != 0 {
		t.Errorf("a node known only by its cool-off states things nobody told the hub: %+v", v)
	}

	f.clk.advance(5*time.Minute - time.Second)
	if v, _ := viewOf(t, f.hub, "node-a"); !v.CoolingUntil.Equal(want) {
		t.Fatalf("one second before the cool-off ends it is not in force: %+v", v)
	}
	f.clk.advance(time.Second)
	if v, ok := viewOf(t, f.hub, "node-a"); ok && !v.CoolingUntil.IsZero() {
		t.Fatalf("the cool-off ran out and the view still states it: %+v", v)
	}
}

// A node known only by a lease a restart recovered: the hub has seen no poll from it, so
// its mode and its encoders are not known, and it holds one live lease.
func TestView_ARecoveredLeaseNamesANodeWhoseModeIsNotKnown(t *testing.T) {
	f := newFixture(t, nil)
	a, _ := f.grant("node-a", f.job("one", 4000))
	dbPath := f.dbPath
	// The restart is a KILLED process: the server and the ledger go, and the hub is not
	// stopped first. A stopping hub ends its lease as cancelled, in the engine call's own
	// goroutine, and that write raced the ledger's close - where it won, there was no live
	// lease left to recover (it did, on a loaded CI runner).
	f.srv.Close()
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}

	again := &fixture{t: t, dir: f.dir, clk: f.clk, space: f.space, dbPath: dbPath}
	again.open(nil)
	if got := again.hub.View(); len(got) != 0 {
		t.Fatalf("before Recover the hub knows %+v", got)
	}
	kept, err := again.hub.Recover(context.Background())
	if err != nil || len(kept) != 1 || kept[0].ID != a.LeaseID {
		t.Fatalf("Recover: %v, %d kept", err, len(kept))
	}
	got := again.hub.View()
	want := []NodeView{{Node: "node-a", LiveLeases: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("View() = %+v, want %+v", got, want)
	}
}

// The view changes nothing: the ledger's rows and the hub's own queue are what they were.
func TestView_ReadingItChangesNoLeaseAndNoPoll(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.LongPoll = 30 * time.Second })
	a, _ := f.grant("node-a", f.job("one", 4000))
	f.pollAs("node-b", ModeMapped, "libx265")
	before := f.row(a.LeaseID)
	first := f.hub.View()
	for i := 0; i < 20; i++ {
		if got := f.hub.View(); !reflect.DeepEqual(got, first) {
			t.Fatalf("read %d differs from the first:\n %+v\n %+v", i, got, first)
		}
	}
	if after := f.row(a.LeaseID); after != before {
		t.Fatalf("reading the view changed a lease row:\n %+v\n %+v", before, after)
	}
	if f.queuedPolls() != 1 {
		t.Fatalf("reading the view moved a poll: %d queued", f.queuedPolls())
	}
}

// The hub remembers a bounded number of nodes' last polls: past the bound the one seen
// longest ago is forgotten, and among those seen at one instant the lesser name.
func TestView_TheRememberedPollsAreBounded(t *testing.T) {
	f := newFixture(t, nil)
	h := f.hub
	saw := func(name string) {
		h.mu.Lock()
		h.sawLocked(name, ModeMapped, []string{"libx265"})
		h.mu.Unlock()
	}
	known := func(name string) bool { _, ok := viewOf(t, h, name); return ok }

	// "b-old" and "a-old" are seen at one instant, everything else later.
	saw("b-old")
	saw("a-old")
	for i := 0; i < maxSeenNodes-2; i++ {
		f.clk.advance(time.Second)
		saw(fmt.Sprintf("node-%03d", i))
	}
	if got := len(h.View()); got != maxSeenNodes {
		t.Fatalf("the fixture holds %d nodes, want the bound of %d", got, maxSeenNodes)
	}

	// A node already known polls again at the bound: nothing is forgotten.
	f.clk.advance(time.Second)
	saw("node-000")
	if got := len(h.View()); got != maxSeenNodes || !known("a-old") || !known("b-old") {
		t.Fatalf("a known node's poll at the bound forgot something: %d nodes", got)
	}

	// A NEW node at the bound: the oldest goes, and of the two seen first, the lesser name.
	f.clk.advance(time.Second)
	saw("newcomer-1")
	if got := len(h.View()); got != maxSeenNodes {
		t.Fatalf("past the bound the hub remembers %d nodes, want %d", got, maxSeenNodes)
	}
	if known("a-old") || !known("b-old") || !known("newcomer-1") {
		t.Fatalf("a-old known=%v b-old known=%v newcomer-1 known=%v; want a-old forgotten alone",
			known("a-old"), known("b-old"), known("newcomer-1"))
	}
	saw("newcomer-2")
	if known("b-old") || !known("node-001") || !known("newcomer-1") || !known("newcomer-2") {
		t.Fatal("the second newcomer did not take the place of the node now seen longest ago")
	}
	// node-000 polled again after node-001 was first seen, so node-001 is the next to go.
	saw("newcomer-3")
	if known("node-001") || !known("node-000") || !known("node-002") {
		t.Fatal("a node's later poll did not move it behind the nodes seen before that poll")
	}

	// What is remembered is a copy of what the poll stated.
	enc := []string{"libx265"}
	h.mu.Lock()
	h.sawLocked("copied", ModeHTTP, enc)
	h.mu.Unlock()
	enc[0] = "tampered"
	if v, _ := viewOf(t, h, "copied"); fmt.Sprint(v.Encoders) != "[libx265]" || v.Mode != ModeHTTP {
		t.Fatalf("the hub kept the caller's slice: %+v", v)
	}
}
