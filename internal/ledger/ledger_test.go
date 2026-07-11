package ledger

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLedger(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "sub", "l.ledger"))

	// Missing file: no rows, no error.
	if s, err := l.Status("/x", "k"); err != nil || s != "" {
		t.Fatalf("Status(missing) = %q,%v want \"\",nil", s, err)
	}
	if n, err := l.FailCount("/x", "k"); err != nil || n != 0 {
		t.Fatalf("FailCount(missing) = %d,%v want 0,nil", n, err)
	}

	// Record creates the parent dir and appends.
	for _, r := range []struct{ status, key, path string }{
		{Failed, "k1", "/a"},
		{Failed, "k1", "/a"},
		{Done, "k1", "/a"}, // later status wins for /a,k1
		{Skipped, "k2", "/b"},
	} {
		if err := l.Record(r.status, r.key, r.path); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	// Status is last-wins.
	if s, _ := l.Status("/a", "k1"); s != Done {
		t.Errorf("Status(/a,k1) = %q, want done (last-wins over two failed)", s)
	}
	if s, _ := l.Status("/b", "k2"); s != Skipped {
		t.Errorf("Status(/b,k2) = %q, want skipped", s)
	}
	// A different key does not match.
	if s, _ := l.Status("/a", "other"); s != "" {
		t.Errorf("Status(/a,other) = %q, want \"\"", s)
	}

	// FailCount counts only 'failed' rows for the exact path+key.
	if n, _ := l.FailCount("/a", "k1"); n != 2 {
		t.Errorf("FailCount(/a,k1) = %d, want 2", n)
	}
	if n, _ := l.FailCount("/b", "k2"); n != 0 {
		t.Errorf("FailCount(/b,k2) = %d, want 0", n)
	}
}

func TestLedgerToleratesMalformedLines(t *testing.T) {
	p := filepath.Join(t.TempDir(), "l.ledger")
	// A malformed line (no tabs) must be skipped, not crash the scan.
	if err := os.WriteFile(p, []byte("garbage line\ndone\tk\t/a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := New(p)
	if s, err := l.Status("/a", "k"); err != nil || s != Done {
		t.Fatalf("Status = %q,%v want done,nil (malformed line skipped)", s, err)
	}
}
