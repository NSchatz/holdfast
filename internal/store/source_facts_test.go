package store

// The store half of S0167: a skip recorded before the claim carries what the snapshot read
// about its source. It is graded where NULL can be told from "" and from 0: on the columns
// themselves.

import (
	"context"
	"testing"
)

func factPx(v int) *int { return &v }

// factColumns reads the six decision columns of one row exactly as they are stored, so a
// NULL is a NULL here and not the zero value a scan would turn it into.
type factColumns struct {
	codec, root, digest   *string
	bytes                 *int64
	width, height         *int64
	status                string
	reason, encoder       *string
	outputBytes, encodeMs *int64
}

func columnsOf(t *testing.T, s *SQLite, path string) factColumns {
	t.Helper()
	var c factColumns
	err := s.db.QueryRowContext(context.Background(),
		`SELECT source_codec, library_root, profile_digest, source_bytes, source_width, source_height,
			status, reason, encoder, output_bytes, encode_ms
		 FROM jobs WHERE path = ?`, path).Scan(&c.codec, &c.root, &c.digest, &c.bytes, &c.width,
		&c.height, &c.status, &c.reason, &c.encoder, &c.outputBytes, &c.encodeMs)
	if err != nil {
		t.Fatalf("read the columns of %s: %v", path, err)
	}
	return c
}

func text(p *string) string {
	if p == nil {
		return "NULL"
	}
	return "'" + *p + "'"
}

func number(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

// pendingRow leaves path as a pending row, the state RecordSkip CONVERTS rather than
// inserts over: a claim leaves it probing and RecoverStale is what a restart does to it.
func pendingRow(t *testing.T, s *SQLite, path string) {
	t.Helper()
	ctx := context.Background()
	if ok, err := s.Claim(ctx, path, "fp", "w0", 3, sameConfig); err != nil || !ok {
		t.Fatalf("Claim(%s): ok=%v err=%v", path, ok, err)
	}
	if _, err := s.RecoverStale(ctx); err != nil {
		t.Fatalf("RecoverStale: %v", err)
	}
	if st, _, _, err := s.Get(ctx, path, "fp"); err != nil || st != Pending {
		t.Fatalf("%s is status=%q err=%v, want pending", path, st, err)
	}
}

// TestS0167_AC3_RecordSkipCarriesTheSourceFactsItIsHanded grades the store half of
// [AC-3]: a skip recorded before the claim stores the codec and the dimensions it was handed,
// on a fresh row and on a converted one, each in its own column.
func TestS0167_AC3_RecordSkipCarriesTheSourceFactsItIsHanded(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	pendingRow(t, s, "/lib/converted.mkv")

	src := SourceFacts{Codec: "h264", Width: factPx(352), Height: factPx(288)}
	for _, path := range []string{"/lib/fresh.mkv", "/lib/converted.mkv"} {
		changed, err := s.RecordSkip(ctx, path, "fp", "hardlinked",
			Decision{LibraryRoot: "/lib", ProfileDigest: "digest"}, "bulk", src)
		if err != nil || !changed {
			t.Fatalf("RecordSkip(%s): changed=%v err=%v", path, changed, err)
		}
		c := columnsOf(t, s, path)
		if c.status != string(Skipped) || text(c.reason) != "'hardlinked'" {
			t.Fatalf("%s is %q/%s, want skipped/'hardlinked'", path, c.status, text(c.reason))
		}
		if text(c.codec) != "'h264'" || number(c.width) != 352 || number(c.height) != 288 {
			t.Errorf("%s stores codec %s and %dx%d, want 'h264' and 352x288", path, text(c.codec),
				number(c.width), number(c.height))
		}
		if text(c.root) != "'/lib'" || text(c.digest) != "'digest'" {
			t.Errorf("%s stores root %s digest %s, want the decision's", path, text(c.root), text(c.digest))
		}
		if c.bytes != nil {
			t.Errorf("%s stores a source size (%d) no skip measured", path, *c.bytes)
		}
	}

	// Read back through the row projection too, which is what /api/history serves.
	rows, err := s.List(ctx, []Status{Skipped}, 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("List: %d rows, err=%v", len(rows), err)
	}
	for _, r := range rows {
		o := r.Outcome
		if o.SourceCodec != "h264" || o.SourceWidth == nil || *o.SourceWidth != 352 ||
			o.SourceHeight == nil || *o.SourceHeight != 288 {
			t.Errorf("%s reads back as %q %v x %v", r.Path, o.SourceCodec, o.SourceWidth, o.SourceHeight)
		}
	}
}

// TestS0167_AC5_RecordSkipWithNoSnapshotStoresNulls grades the store half of [AC-5] and of
// [AC-6]: facts that were not read are stored NULL - never "" and never 0 - on a fresh row,
// and on a converted row they REPLACE whatever an earlier attempt left there.
func TestS0167_AC5_RecordSkipWithNoSnapshotStoresNulls(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	// The converted row arrives pending WITH an earlier attempt's facts on it, which is what a
	// row that was in flight when the daemon stopped can look like.
	pendingRow(t, s, "/lib/converted.mkv")
	if _, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET source_codec = 'vp9', source_width = 7, source_height = 9, source_bytes = 999,
			library_root = '/stale-root', profile_digest = 'stale' WHERE path = ?`, "/lib/converted.mkv"); err != nil {
		t.Fatalf("seed the earlier attempt's facts: %v", err)
	}
	if c := columnsOf(t, s, "/lib/converted.mkv"); text(c.codec) != "'vp9'" {
		t.Fatalf("precondition: the pending row carries codec %s, want the earlier attempt's", text(c.codec))
	}

	for _, path := range []string{"/lib/fresh.mkv", "/lib/converted.mkv"} {
		if changed, err := s.RecordSkip(ctx, path, "fp", "hardlinked", Decision{}, "", SourceFacts{}); err != nil || !changed {
			t.Fatalf("RecordSkip(%s): changed=%v err=%v", path, changed, err)
		}
		c := columnsOf(t, s, path)
		if c.codec != nil || c.width != nil || c.height != nil {
			t.Errorf("%s stores codec %s and %dx%d, want all three NULL", path, text(c.codec),
				number(c.width), number(c.height))
		}
		if c.bytes != nil || c.root != nil || c.digest != nil {
			t.Errorf("%s keeps an earlier attempt's size %d, root %s or digest %s", path,
				number(c.bytes), text(c.root), text(c.digest))
		}
	}

	// A codec with no dimensions is recorded as exactly that.
	if _, err := s.RecordSkip(ctx, "/lib/codec-only.mkv", "fp", "hardlinked", Decision{}, "",
		SourceFacts{Codec: "mpeg2video"}); err != nil {
		t.Fatal(err)
	}
	if c := columnsOf(t, s, "/lib/codec-only.mkv"); text(c.codec) != "'mpeg2video'" || c.width != nil || c.height != nil {
		t.Errorf("a codec-only skip stores codec %s and %dx%d", text(c.codec), number(c.width), number(c.height))
	}
}
