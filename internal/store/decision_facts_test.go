package store

// The store half of S0171: a row admitted to the encoder carries its decision facts. Graded
// on the columns themselves, where NULL can be told from "" and from 0.

import (
	"context"
	"strings"
	"testing"
)

// admitted is the facts every AdmitToEncoder case below hands in.
func admitted() DecisionFacts {
	size := int64(4_000_000)
	return DecisionFacts{
		Source:      SourceFacts{Codec: "h264", Width: factPx(352), Height: factPx(288)},
		SourceBytes: &size,
		Decision:    Decision{LibraryRoot: "/lib", ProfileDigest: "digest"},
	}
}

// TestS0171_AC1_AdmitToEncoderWritesTheSixFactsAndTheStatus grades the store half of
// [AC-1] and [AC-2]: one write moves a probing row to encoding with its six decision facts,
// each in its own column, touches no other outcome column, and Advance leaves them alone.
func TestS0171_AC1_AdmitToEncoderWritesTheSixFactsAndTheStatus(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if ok, err := s.Claim(ctx, "/lib/a.mkv", "fp", "w7", 3, sameConfig); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	if c := columnsOf(t, s, "/lib/a.mkv"); c.status != string(Probing) || c.codec != nil || c.bytes != nil ||
		c.width != nil || c.height != nil || c.root != nil || c.digest != nil {
		t.Fatalf("a probing row already carries decision facts: %+v", c)
	}
	if err := s.AdmitToEncoder(ctx, "/lib/a.mkv", "fp", admitted()); err != nil {
		t.Fatalf("AdmitToEncoder: %v", err)
	}

	check := func(want Status) {
		t.Helper()
		c := columnsOf(t, s, "/lib/a.mkv")
		if c.status != string(want) {
			t.Fatalf("status = %q, want %q", c.status, want)
		}
		if number(c.bytes) != 4_000_000 || text(c.codec) != "'h264'" || number(c.width) != 352 ||
			number(c.height) != 288 || text(c.root) != "'/lib'" || text(c.digest) != "'digest'" {
			t.Errorf("the %s row stores size %d codec %s %dx%d root %s digest %s", want, number(c.bytes),
				text(c.codec), number(c.width), number(c.height), text(c.root), text(c.digest))
		}
		if c.reason != nil || c.encoder != nil || c.outputBytes != nil || c.encodeMs != nil {
			t.Errorf("the %s row carries proof of an encode: reason %s encoder %s output %d ms %d", want,
				text(c.reason), text(c.encoder), number(c.outputBytes), number(c.encodeMs))
		}
	}
	check(Encoding)

	rows, err := s.List(ctx, []Status{Encoding}, 0)
	if err != nil || len(rows) != 1 || rows[0].Worker != "w7" {
		t.Fatalf("List(encoding) = %+v, err=%v; want the one row still held by w7", rows, err)
	}

	if err := s.Advance(ctx, "/lib/a.mkv", "fp", Verifying); err != nil {
		t.Fatal(err)
	}
	check(Verifying)

	// The terminal write defines the row's whole proof: a failure that records none of them
	// leaves none of them.
	if err := s.Finish(ctx, "/lib/a.mkv", "fp", Failed, &Outcome{Reason: "rejected"}, 3); err != nil {
		t.Fatal(err)
	}
	if c := columnsOf(t, s, "/lib/a.mkv"); c.bytes != nil || c.codec != nil || c.width != nil ||
		c.height != nil || c.root != nil || c.digest != nil {
		t.Errorf("the failed row keeps in-flight facts its terminal write did not record: %+v", c)
	}
}

// TestS0171_AC6_AdmitToEncoderStoresAnUnestablishedFactAsNull grades the store half of
// [AC-6]: a fact the decision did not establish is NULL, never "" and never 0, while the
// others are carried.
func TestS0171_AC6_AdmitToEncoderStoresAnUnestablishedFactAsNull(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if ok, err := s.Claim(ctx, "/lib/a.mkv", "fp", "w0", 3, sameConfig); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	d := admitted()
	d.Source.Width, d.Source.Height = nil, nil
	if err := s.AdmitToEncoder(ctx, "/lib/a.mkv", "fp", d); err != nil {
		t.Fatal(err)
	}
	c := columnsOf(t, s, "/lib/a.mkv")
	if c.width != nil || c.height != nil {
		t.Errorf("unestablished dimensions are stored as %dx%d, want NULL", number(c.width), number(c.height))
	}
	if number(c.bytes) != 4_000_000 || text(c.codec) != "'h264'" || text(c.root) != "'/lib'" {
		t.Errorf("the other facts were not carried: size %d codec %s root %s", number(c.bytes), text(c.codec), text(c.root))
	}

	if ok, err := s.Claim(ctx, "/lib/b.mkv", "fp", "w0", 3, sameConfig); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	if err := s.AdmitToEncoder(ctx, "/lib/b.mkv", "fp", DecisionFacts{}); err != nil {
		t.Fatal(err)
	}
	c = columnsOf(t, s, "/lib/b.mkv")
	if c.status != string(Encoding) {
		t.Errorf("status = %q, want encoding", c.status)
	}
	if c.bytes != nil || c.codec != nil || c.width != nil || c.height != nil || c.root != nil || c.digest != nil {
		t.Errorf("facts nobody established are stored as values, want NULL: %+v", c)
	}
}

// TestS0171_AC4_AdmitToEncoderTouchesOnlyARowStillProbing grades the store half of
// [AC-4] and [AC-8]: the write lands only on a row its caller still holds in probing. A
// terminal row keeps its own proof and its status, a pending row stays pending, and a path
// with no row gets none.
func TestS0171_AC4_AdmitToEncoderTouchesOnlyARowStillProbing(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	srcBytes, outBytes := int64(1000), int64(400)
	if ok, err := s.Claim(ctx, "/lib/done.mkv", "fp", "w0", 3, sameConfig); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	if err := s.Finish(ctx, "/lib/done.mkv", "fp", Done,
		&Outcome{Encoder: "cpu", SourceBytes: &srcBytes, OutputBytes: &outBytes}, 3); err != nil {
		t.Fatal(err)
	}
	pendingRow(t, s, "/lib/pending.mkv")
	before, err := s.ReclaimedTotal(ctx)
	if err != nil || before != 600 {
		t.Fatalf("precondition: reclaimed total = %d err=%v, want 600", before, err)
	}

	for _, path := range []string{"/lib/done.mkv", "/lib/pending.mkv", "/lib/absent.mkv"} {
		if err := s.AdmitToEncoder(ctx, path, "fp", admitted()); err != nil {
			t.Fatalf("AdmitToEncoder(%s): %v", path, err)
		}
	}

	done := columnsOf(t, s, "/lib/done.mkv")
	if done.status != string(Done) || number(done.bytes) != 1000 || done.codec != nil || text(done.encoder) != "'cpu'" {
		t.Errorf("a done row was rewritten: status %q size %d codec %s encoder %s", done.status,
			number(done.bytes), text(done.codec), text(done.encoder))
	}
	pending := columnsOf(t, s, "/lib/pending.mkv")
	if pending.status != string(Pending) || pending.bytes != nil || pending.codec != nil {
		t.Errorf("a pending row was written to: status %q size %d codec %s", pending.status,
			number(pending.bytes), text(pending.codec))
	}
	if _, _, exists, err := s.Get(ctx, "/lib/absent.mkv", "fp"); err != nil || exists {
		t.Errorf("a row was created for a path that had none (exists=%v err=%v)", exists, err)
	}
	if after, err := s.ReclaimedTotal(ctx); err != nil || after != before {
		t.Errorf("the reclaimed total moved from %d to %d (err=%v)", before, after, err)
	}

	// A different fingerprint is a different row.
	if ok, err := s.Claim(ctx, "/lib/keyed.mkv", "fp", "w0", 3, sameConfig); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	if err := s.AdmitToEncoder(ctx, "/lib/keyed.mkv", "other-fp", admitted()); err != nil {
		t.Fatal(err)
	}
	if c := columnsOf(t, s, "/lib/keyed.mkv"); c.status != string(Probing) || c.codec != nil {
		t.Errorf("a row keyed by another fingerprint was written to: status %q codec %s", c.status, text(c.codec))
	}
}

// TestS0171_AC7_AdmitToEncoderReportsAStoreError grades the store half of [AC-7]: a
// write the database refuses comes back as an error naming the operation, for the caller to
// survive.
func TestS0171_AC7_AdmitToEncoderReportsAStoreError(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if ok, err := s.Claim(ctx, "/lib/a.mkv", "fp", "w0", 3, sameConfig); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	err := s.AdmitToEncoder(cancelled, "/lib/a.mkv", "fp", admitted())
	if err == nil {
		t.Fatal("a write on a cancelled context reported success")
	}
	if !strings.HasPrefix(err.Error(), "store: admit to encoder") {
		t.Errorf("the error does not name the operation: %q", err)
	}
	if c := columnsOf(t, s, "/lib/a.mkv"); c.status != string(Probing) || c.codec != nil {
		t.Errorf("a refused write changed the row: status %q codec %s", c.status, text(c.codec))
	}
}
