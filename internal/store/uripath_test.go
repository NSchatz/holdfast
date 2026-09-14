package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The `file:` DSN is a URI, and a path is not.
//
// Found while grading S0103's read-only criterion: a state directory whose path carries a
// '#' had `store.Open` create NOTHING at that path and open the truncated prefix instead, so
// two installs differing only after the '#' silently shared one ledger and each read the
// other's rows as its own. On a tool that deletes originals the ledger is the record of what
// it retained and what it removed, so that is a data-safety fault and not a cosmetic one.
//
// Every door in this package is covered, because each builds its own DSN: Open (the
// daemon's, which creates), OpenReadOnly (export's and validate's) and OpenSnapshot (the one
// `analyze` and `plan` use, which must create nothing at all).

// hashPathStore lays out two state directories differing ONLY after a '#', each with a
// ledger, and returns the two paths.
func hashPathStores(t *testing.T) (a, b string) {
	t.Helper()
	base := t.TempDir()
	a = filepath.Join(base, "state#1", "jobs.db")
	b = filepath.Join(base, "state#2", "jobs.db")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return a, b
}

// TestOpen_APathCarryingAHashIsTheFileItNames is the regression: Open creates the database
// AT THE PATH IT WAS GIVEN, and two ledgers whose paths differ only after a '#' are two
// ledgers.
func TestOpen_APathCarryingAHashIsTheFileItNames(t *testing.T) {
	a, b := hashPathStores(t)
	ctx := context.Background()

	sa, err := Open(a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sa.Claim(ctx, "/only/in/a.mkv", "fa", "w", 3, DecisionInputs{}); err != nil {
		t.Fatal(err)
	}
	if err := sa.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(a); err != nil {
		t.Fatalf("Open created no database at the path it was given (%s): %v", a, err)
	}

	sb, err := Open(b)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sb.Close() }()
	rows, err := sb.List(ctx, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("a second ledger at %s reads %d row(s) written to %s - the two paths resolved to "+
			"one file", b, len(rows), a)
	}
}

// TestReaders_APathCarryingAHashIsTheFileItNames covers the two read-only doors, including
// the snapshot door `analyze` and `plan` open: each must read the database it was NAMED.
func TestReaders_APathCarryingAHashIsTheFileItNames(t *testing.T) {
	a, b := hashPathStores(t)
	ctx := context.Background()

	for _, p := range []string{a, b} {
		st, err := Open(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Claim(ctx, "/in/"+filepath.Base(filepath.Dir(p))+".mkv", "f", "w", 3,
			DecisionInputs{}); err != nil {
			t.Fatal(err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}

	for _, open := range []struct {
		name string
		fn   func(string) (*SQLite, error)
	}{
		{"OpenReadOnly", OpenReadOnly},
		{"OpenSnapshot", OpenSnapshot},
	} {
		t.Run(open.name, func(t *testing.T) {
			st, err := open.fn(b)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			rows, err := st.List(ctx, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || !strings.Contains(rows[0].Path, "state#2") {
				t.Fatalf("%s(%s) read %v, not the one row written to that database", open.name, b, rows)
			}
		})
	}
}

// TestURIPath_EncodesOnlyWhatAURIWouldMisread pins the encoding itself, including the
// ordering that makes it reversible: '%' first, so an operator's literal "%23" does not come
// back as a '#'.
func TestURIPath_EncodesOnlyWhatAURIWouldMisread(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/srv/media/jobs.db", "/srv/media/jobs.db"},
		{"/srv/media#2/jobs.db", "/srv/media%232/jobs.db"},
		{"/srv/what?/jobs.db", "/srv/what%3F/jobs.db"},
		{"/srv/50%/jobs.db", "/srv/50%25/jobs.db"},
		{"/srv/%23literal/jobs.db", "/srv/%2523literal/jobs.db"},
		{"/srv/with space/jobs.db", "/srv/with space/jobs.db"},
	} {
		if got := uriPath(tc.in); got != tc.want {
			t.Fatalf("uriPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
