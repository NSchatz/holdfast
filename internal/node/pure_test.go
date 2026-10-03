package node

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

var t0 = time.Unix(1900000000, 0)

const ttl = time.Minute

func grantedAt(now time.Time) Lease {
	l, err := decideGrant(Lease{ID: "L1", Path: "/lib/a.mkv", Key: "k", Node: "node-a", Temp: "/lib/a.part",
		SourceSize: 1000, SourceModTime: time.Unix(1700000000, 5),
		ArgsDigest: ArgsDigest("libx265", []string{"-nostdin"}, []string{"-c:v", "libx265"})},
		nil, caps{global: 4, perNode: 1}, now, ttl)
	if err != nil {
		panic(err)
	}
	l.Epoch = 1
	return l
}

// TestNodeLease_StateMachineAgainstAFakeClock walks one path's leases through the whole
// machine: grant, renew, expire, re-grant at a higher epoch, admit, complete, fail.
func TestNodeLease_StateMachineAgainstAFakeClock(t *testing.T) {
	// Grant.
	l := grantedAt(t0)
	if l.State != store.LeaseGranted || !l.ExpiresAt.Equal(t0.Add(ttl)) || !l.GrantedAt.Equal(t0) ||
		!l.UpdatedAt.Equal(t0) || !l.EndedAt.IsZero() {
		t.Fatalf("a grant is %+v, want granted, expiring one TTL from now, not ended", l)
	}

	// Renew just before the expiry: one TTL from the renewal.
	at := t0.Add(ttl - time.Second)
	r, err := decideRenew(l, 1, at, ttl, true)
	if err != nil || !r.ExpiresAt.Equal(at.Add(ttl)) || !r.UpdatedAt.Equal(at) || r.State != store.LeaseGranted {
		t.Fatalf("a renewal one second before the expiry: %+v, %v", r, err)
	}
	// A renewal that does not extend confirms the lease and leaves it exactly as it was.
	if same, err := decideRenew(l, 1, at, ttl, false); err != nil || same != l {
		t.Errorf("a non-extending renewal changed the lease: %+v, %v", same, err)
	}
	// AT the expiry instant the lease is expired, and one second before it is not.
	if _, err := decideRenew(l, 1, t0.Add(ttl), ttl, true); !errors.Is(err, ErrGone) {
		t.Errorf("a renewal at the expiry instant returned %v, want ErrGone", err)
	}
	// A stale epoch is gone however live the lease is, and so is a later one.
	for _, epoch := range []int64{0, 2} {
		if _, err := decideRenew(l, epoch, t0, ttl, true); !errors.Is(err, ErrGone) {
			t.Errorf("a renewal at epoch %d of a lease at epoch 1 returned %v, want ErrGone", epoch, err)
		}
	}

	// Expire: not due one second before, due at the instant.
	if _, err := decideExpire(l, t0.Add(ttl-time.Second)); !errors.Is(err, errNotDue) {
		t.Errorf("an expiry one second early returned %v, want errNotDue", err)
	}
	due := t0.Add(ttl)
	x, err := decideExpire(l, due)
	if err != nil || x.State != store.LeaseExpired || x.Reason != string(ReasonExpired) || !x.EndedAt.Equal(due) || !x.UpdatedAt.Equal(due) {
		t.Fatalf("an expiry at the instant: %+v, %v", x, err)
	}
	// A terminal lease is never expired again, renewed, admitted on, completed or failed.
	if _, err := decideExpire(x, due.Add(time.Hour)); !errors.Is(err, errNotDue) {
		t.Errorf("expiring an expired lease returned %v", err)
	}
	if _, err := decideRenew(x, 1, t0, ttl, true); !errors.Is(err, ErrGone) {
		t.Errorf("renewing an expired lease returned %v, want ErrGone", err)
	}
	if _, err := decideAdmit(x, 1, t0, "d", 5); !errors.Is(err, ErrGone) {
		t.Errorf("admitting on an expired lease returned %v, want ErrGone", err)
	}
	if _, err := decideComplete(x, 1, t0, "d", 5, "s"); !errors.Is(err, ErrGone) {
		t.Errorf("completing an expired lease returned %v, want ErrGone", err)
	}
	if _, err := decideFail(x, 1, t0, "why"); !errors.Is(err, ErrGone) {
		t.Errorf("failing an expired lease returned %v, want ErrGone", err)
	}
	if _, err := decideEnd(x, t0, ReasonCanceled); !errors.Is(err, ErrGone) {
		t.Errorf("ending an expired lease returned %v, want ErrGone", err)
	}
	if _, err := decideGrace(x, t0, ttl); !errors.Is(err, ErrGone) {
		t.Errorf("grace on an expired lease returned %v, want ErrGone", err)
	}

	// Re-grant: the expired lease no longer holds the path. (The ledger assigns the epoch.)
	g2, err := decideGrant(Lease{ID: "L2", Path: l.Path, Node: "node-b", SourceSize: 1000}, nil, caps{4, 1}, due, ttl)
	if err != nil || g2.State != store.LeaseGranted || !g2.ExpiresAt.Equal(due.Add(ttl)) {
		t.Fatalf("a re-grant after the expiry: %+v, %v", g2, err)
	}
	g2.Epoch = 2

	// Admit, then complete.
	up, err := decideAdmit(g2, 2, due.Add(time.Second), "sha-256=:out:", 900)
	if err != nil || up.State != store.LeaseUploaded || up.OutputDigest != "sha-256=:out:" || up.OutputBytes != 900 ||
		!up.UpdatedAt.Equal(due.Add(time.Second)) || !up.EndedAt.IsZero() {
		t.Fatalf("an admission: %+v, %v", up, err)
	}
	if _, err := decideAdmit(up, 2, due.Add(time.Second), "sha-256=:out:", 900); !errors.Is(err, ErrDigestConflict) {
		t.Errorf("a second admission returned %v, want ErrDigestConflict", err)
	}
	if _, err := decideAdmit(g2, 1, due, "d", 1); !errors.Is(err, ErrGone) {
		t.Errorf("an admission at a stale epoch returned %v, want ErrGone", err)
	}
	if _, err := decideAdmit(g2, 2, due.Add(ttl), "d", 1); !errors.Is(err, ErrGone) {
		t.Errorf("an admission at the expiry instant returned %v, want ErrGone", err)
	}
	doneAt := due.Add(2 * time.Second)
	done, err := decideComplete(up, 2, doneAt, "sha-256=:out:", 900, "sha-256=:src:")
	if err != nil || done.State != store.LeaseCompleted || done.SourceDigest != "sha-256=:src:" ||
		!done.EndedAt.Equal(doneAt) || !done.UpdatedAt.Equal(doneAt) {
		t.Fatalf("a completion: %+v, %v", done, err)
	}
	// A repeat with the recorded figures is answered from the record, whatever the clock.
	if again, err := decideComplete(done, 2, doneAt.Add(time.Hour), "sha-256=:out:", 900, "sha-256=:src:"); !errors.Is(err, errAlready) || again != done {
		t.Errorf("a repeated completion returned %+v, %v; want the recorded row and errAlready", again, err)
	}
	for name, c := range map[string]struct {
		out   string
		bytes int64
		src   string
	}{
		"another output digest": {"sha-256=:other:", 900, "sha-256=:src:"},
		"another length":        {"sha-256=:out:", 901, "sha-256=:src:"},
		"another source digest": {"sha-256=:out:", 900, "sha-256=:other:"},
	} {
		if _, err := decideComplete(done, 2, doneAt, c.out, c.bytes, c.src); !errors.Is(err, ErrDigestConflict) {
			t.Errorf("a repeated completion with %s returned %v, want ErrDigestConflict", name, err)
		}
	}
	if _, err := decideComplete(done, 1, doneAt, "sha-256=:out:", 900, "sha-256=:src:"); !errors.Is(err, ErrGone) {
		t.Errorf("a completion at a stale epoch returned %v, want ErrGone", err)
	}
	// A completion needs an admitted output, and the figures the upload recorded.
	if _, err := decideComplete(g2, 2, due, "sha-256=:out:", 900, "s"); !errors.Is(err, ErrNotUploaded) {
		t.Errorf("completing a lease nothing was uploaded on returned %v, want ErrNotUploaded", err)
	}
	if _, err := decideComplete(up, 2, due, "sha-256=:other:", 900, "s"); !errors.Is(err, ErrDigestConflict) {
		t.Errorf("completing with another digest returned %v, want ErrDigestConflict", err)
	}
	if _, err := decideComplete(up, 2, due, "sha-256=:out:", 899, "s"); !errors.Is(err, ErrDigestConflict) {
		t.Errorf("completing with another length returned %v, want ErrDigestConflict", err)
	}
	if _, err := decideComplete(up, 2, up.ExpiresAt, "sha-256=:out:", 900, "s"); !errors.Is(err, ErrGone) {
		t.Errorf("completing at the expiry instant returned %v, want ErrGone", err)
	}

	// Fail.
	failed, err := decideFail(l, 1, t0.Add(time.Second), "encoder_crashed")
	if err != nil || failed.State != store.LeaseFailed || failed.Reason != "encoder_crashed" ||
		!failed.EndedAt.Equal(t0.Add(time.Second)) || !failed.UpdatedAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("a fail: %+v, %v", failed, err)
	}
	if again, err := decideFail(failed, 1, t0.Add(time.Hour), "other"); !errors.Is(err, errAlready) || again != failed {
		t.Errorf("a repeated fail returned %+v, %v; want the recorded row and errAlready", again, err)
	}
	if _, err := decideFail(failed, 2, t0, "other"); !errors.Is(err, ErrGone) {
		t.Errorf("a fail at another epoch of a failed lease returned %v, want ErrGone", err)
	}
	if _, err := decideFail(l, 2, t0, "why"); !errors.Is(err, ErrGone) {
		t.Errorf("a fail at a stale epoch returned %v, want ErrGone", err)
	}
	if _, err := decideFail(l, 1, l.ExpiresAt, "why"); !errors.Is(err, ErrGone) {
		t.Errorf("a fail at the expiry instant returned %v, want ErrGone", err)
	}
	if f2, err := decideFail(up, 2, due, "why"); err != nil || f2.State != store.LeaseFailed {
		t.Errorf("failing an uploaded lease: %+v, %v", f2, err)
	}
}

// TestNodeLease_AGrantIsHeldToThePathAndBothCaps.
func TestNodeLease_AGrantIsHeldToThePathAndBothCaps(t *testing.T) {
	live := []Lease{
		{ID: "x1", Path: "/lib/x.mkv", Node: "node-a", Epoch: 3},
		{ID: "y1", Path: "/lib/y.mkv", Node: "node-b", Epoch: 1},
	}
	ask := func(path, node string, c caps) error {
		_, err := decideGrant(Lease{ID: "new", Path: path, Node: node}, live, c, t0, ttl)
		return err
	}
	if err := ask("/lib/x.mkv", "node-c", caps{9, 9}); !errors.Is(err, ErrLeaseHeld) {
		t.Errorf("a grant of a path with a live lease returned %v, want ErrLeaseHeld", err)
	} else if !strings.Contains(err.Error(), "x1") || !strings.Contains(err.Error(), "node-a") || !strings.Contains(err.Error(), "epoch 3") {
		t.Errorf("the refusal must name the holding lease, its node and its epoch: %v", err)
	}
	if err := ask("/lib/z.mkv", "node-c", caps{2, 9}); !errors.Is(err, ErrGlobalCap) {
		t.Errorf("a grant at the global cap returned %v, want ErrGlobalCap", err)
	}
	if err := ask("/lib/z.mkv", "node-c", caps{3, 9}); err != nil {
		t.Errorf("a grant one under the global cap was refused: %v", err)
	}
	if err := ask("/lib/z.mkv", "node-a", caps{9, 1}); !errors.Is(err, ErrNodeCap) {
		t.Errorf("a grant to a node at its cap returned %v, want ErrNodeCap", err)
	}
	if err := ask("/lib/z.mkv", "node-a", caps{9, 2}); err != nil {
		t.Errorf("a grant to a node one under its cap was refused: %v", err)
	}
	if err := ask("/lib/z.mkv", "node-c", caps{9, 1}); err != nil {
		t.Errorf("a grant to a node holding nothing was refused: %v", err)
	}
	// A grant starts clean whatever the caller left in the row.
	l, err := decideGrant(Lease{ID: "new", Path: "/lib/z.mkv", Node: "node-c", State: store.LeaseFailed,
		OutputDigest: "d", OutputBytes: 5, SourceDigest: "s", UploadAttempts: 2, Reason: "r", EndedAt: t0}, nil, caps{1, 1}, t0, ttl)
	if err != nil || l.State != store.LeaseGranted || l.OutputDigest != "" || l.OutputBytes != 0 || l.SourceDigest != "" ||
		l.UploadAttempts != 0 || l.Reason != "" || !l.EndedAt.IsZero() {
		t.Errorf("a grant did not start clean: %+v, %v", l, err)
	}
}

// TestNodeLease_AnUploadIsClassifiedBeforeAByteIsRead.
func TestNodeLease_AnUploadIsClassifiedBeforeAByteIsRead(t *testing.T) {
	granted := grantedAt(t0) // source 1000 bytes: at most 999
	uploaded, _ := decideAdmit(granted, 1, t0, "sha-256=:out:", 500)
	completed, _ := decideComplete(uploaded, 1, t0, "sha-256=:out:", 500, "sha-256=:src:")
	failed, _ := decideFail(granted, 1, t0, "why")
	expired, _ := decideExpire(granted, granted.ExpiresAt)
	late := granted.ExpiresAt

	for _, tc := range []struct {
		name     string
		cur      Lease
		epoch    int64
		now      time.Time
		digest   string
		declared int64
		want     uploadAction
	}{
		{"a live granted lease", granted, 1, t0, "d", 500, uploadProceed},
		{"the largest output the cap admits", granted, 1, t0, "d", 999, uploadProceed},
		{"one byte past the cap", granted, 1, t0, "d", 1000, uploadTooLarge},
		{"a stale epoch", granted, 0, t0, "d", 500, uploadGone},
		{"a later epoch", granted, 2, t0, "d", 500, uploadGone},
		{"a granted lease at its expiry instant", granted, 1, late, "d", 500, uploadGone},
		{"an oversized upload on an expired lease is gone, not too large", granted, 1, late, "d", 5000, uploadGone},
		{"an expired lease", expired, 1, t0, "d", 500, uploadGone},
		{"a failed lease", failed, 1, t0, "d", 500, uploadGone},
		{"the recorded output again", uploaded, 1, t0, "sha-256=:out:", 500, uploadDuplicate},
		{"another digest after acceptance", uploaded, 1, t0, "sha-256=:other:", 500, uploadConflict},
		{"another length after acceptance", uploaded, 1, t0, "sha-256=:out:", 501, uploadConflict},
		{"the recorded output on an uploaded lease past its expiry", uploaded, 1, late, "sha-256=:out:", 500, uploadGone},
		{"the recorded output at a stale epoch", uploaded, 0, t0, "sha-256=:out:", 500, uploadGone},
		{"the recorded output after completion, whatever the clock", completed, 1, late.Add(time.Hour), "sha-256=:out:", 500, uploadDuplicate},
		{"another digest after completion", completed, 1, t0, "sha-256=:other:", 500, uploadConflict},
		{"a stale epoch after completion", completed, 2, t0, "sha-256=:out:", 500, uploadGone},
	} {
		if got := classifyUpload(tc.cur, tc.epoch, tc.now, tc.digest, tc.declared); got != tc.want {
			t.Errorf("%s: classified %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestNodeLease_ADigestMismatchCountsToABoundAndThenFailsTheLease.
func TestNodeLease_ADigestMismatchCountsToABoundAndThenFailsTheLease(t *testing.T) {
	l := grantedAt(t0)
	at := t0.Add(time.Second)
	for attempt := 1; attempt <= 2; attempt++ {
		var err error
		l, err = decideMismatch(l, 1, at, 3)
		if err != nil || l.UploadAttempts != attempt || l.State != store.LeaseGranted || !l.EndedAt.IsZero() || !l.UpdatedAt.Equal(at) {
			t.Fatalf("mismatch %d: %+v, %v; want the lease still granted", attempt, l, err)
		}
	}
	l, err := decideMismatch(l, 1, at, 3)
	if err != nil || l.UploadAttempts != 3 || l.State != store.LeaseFailed || l.Reason != string(ReasonDigestMismatch) || !l.EndedAt.Equal(at) {
		t.Fatalf("the mismatch at the bound: %+v, %v; want the lease failed", l, err)
	}
	if _, err := decideMismatch(l, 1, at, 3); !errors.Is(err, ErrGone) {
		t.Errorf("a mismatch on the failed lease returned %v, want ErrGone", err)
	}
	if _, err := decideMismatch(grantedAt(t0), 2, at, 3); !errors.Is(err, ErrGone) {
		t.Errorf("a mismatch at a stale epoch returned %v, want ErrGone", err)
	}
}

// TestNodeLease_TheServerEndsALeaseWhateverTheClock: decideEnd and decideGrace.
func TestNodeLease_TheServerEndsALeaseWhateverTheClock(t *testing.T) {
	l := grantedAt(t0)
	at := t0.Add(time.Second)
	e, err := decideEnd(l, at, ReasonCanceled)
	if err != nil || e.State != store.LeaseExpired || e.Reason != "canceled" || !e.EndedAt.Equal(at) || !e.UpdatedAt.Equal(at) {
		t.Fatalf("ending a live lease: %+v, %v", e, err)
	}
	up, _ := decideAdmit(l, 1, t0, "d", 5)
	if e, err := decideEnd(up, at, ReasonRestart); err != nil || e.State != store.LeaseExpired || e.Reason != "server_restart" {
		t.Errorf("ending an uploaded lease: %+v, %v", e, err)
	}
	// Grace is one TTL from NOW, even for a lease whose own expiry is long past.
	long := l.ExpiresAt.Add(24 * time.Hour)
	g, err := decideGrace(l, long, ttl)
	if err != nil || !g.ExpiresAt.Equal(long.Add(ttl)) || g.State != store.LeaseGranted || !g.UpdatedAt.Equal(long) {
		t.Errorf("grace: %+v, %v", g, err)
	}
}

// TestNodeLease_AdoptTakesBackOnlyTheLeasedJob.
func TestNodeLease_AdoptTakesBackOnlyTheLeasedJob(t *testing.T) {
	l := grantedAt(t0)
	job := Job{Path: l.Path, Key: "k", Temp: "/lib/a.second.part", Encoder: "libx265",
		Pre: []string{"-nostdin"}, Body: []string{"-c:v", "libx265"},
		SourceSize: 1000, SourceModTime: time.Unix(1700000000, 5), ReservedBytes: 77}
	at := t0.Add(time.Second)
	a, err := decideAdopt(l, job, at)
	if err != nil || a.Temp != "/lib/a.second.part" || a.ReservedBytes != 77 || !a.ExpiresAt.Equal(l.ExpiresAt) ||
		!a.UpdatedAt.Equal(at) || a.State != store.LeaseGranted {
		t.Fatalf("adopting the leased job: %+v, %v", a, err)
	}
	for name, mutate := range map[string]func(*Job){
		"another path":             func(j *Job) { j.Path = "/lib/b.mkv" },
		"another encoder":          func(j *Job) { j.Encoder = "libsvtav1" },
		"another option before -i": func(j *Job) { j.Pre = []string{"-nostdin", "-y"} },
		"another body option":      func(j *Job) { j.Body = []string{"-c:v", "libx265", "-crf", "20"} },
		"another source size":      func(j *Job) { j.SourceSize = 1001 },
		"another source mtime":     func(j *Job) { j.SourceModTime = time.Unix(1700000000, 6) },
	} {
		j := job
		mutate(&j)
		if _, err := decideAdopt(l, j, at); !errors.Is(err, ErrNotSameJob) {
			t.Errorf("adopting a job with %s returned %v, want ErrNotSameJob", name, err)
		}
	}
	if _, err := decideAdopt(l, job, l.ExpiresAt); !errors.Is(err, ErrGone) {
		t.Errorf("adopting at the expiry instant returned %v, want ErrGone", err)
	}
	up, _ := decideAdmit(l, 1, t0, "d", 5)
	if _, err := decideAdopt(up, job, at); !errors.Is(err, ErrGone) {
		t.Errorf("adopting an uploaded lease returned %v, want ErrGone", err)
	}
}

// TestNodeSizeCap_TheLargestOutputIsOneByteLessThanTheSource: the strictly-smaller gate
// accepts nothing else.
func TestNodeSizeCap_TheLargestOutputIsOneByteLessThanTheSource(t *testing.T) {
	for size, want := range map[int64]int64{1000: 999, 2: 1, 1: 0, 0: -1} {
		if got := MaxOutputBytes(size); got != want {
			t.Errorf("MaxOutputBytes(%d) = %d, want %d", size, got, want)
		}
	}
}

// TestNodeDigest_ContentDigestIsParsedAndFormattedPerRFC9530. The vector is RFC 9530's own
// (appendix D: the sha-256 of `{"hello": "world"}`).
func TestNodeDigest_ContentDigestIsParsedAndFormattedPerRFC9530(t *testing.T) {
	sum := sha256.Sum256([]byte(`{"hello": "world"}`))
	const member = "sha-256=:X48E9qOokqqrvdts8nOJRJN3OWDUoyWxBf7kbu9DBPE=:"
	if got := FormatDigest(sum[:]); got != member {
		t.Fatalf("FormatDigest = %q, want %q", got, member)
	}
	for _, value := range []string{
		member,
		"  " + member + "  ",
		"sha-512=:YMAam51Jz/jOATT6/zvHrLVgOYTGFy1d6GJiOHTohq4yP+pgk4vf2aCsyRZOtw8MjkM7iw7yZ/WkppmM44T3qg==:, " + member,
		member + ",md5=:abc:",
		"unknown, " + member,
	} {
		got, err := ParseDigest(value)
		if err != nil || string(got) != string(sum[:]) {
			t.Errorf("ParseDigest(%q) = %x, %v; want the sum", value, got, err)
		}
		if canon, err := CanonicalDigest(value); err != nil || canon != member {
			t.Errorf("CanonicalDigest(%q) = %q, %v; want %q", value, canon, err, member)
		}
	}
	for name, value := range map[string]string{
		"empty":                     "",
		"no sha-256 member":         "sha-512=:YMAam51J:",
		"two sha-256 members":       member + ", " + member,
		"not a byte sequence":       "sha-256=X48E9qOokqqrvdts8nOJRJN3OWDUoyWxBf7kbu9DBPE=",
		"no closing colon":          "sha-256=:X48E9qOokqqrvdts8nOJRJN3OWDUoyWxBf7kbu9DBPE=",
		"no opening colon":          "sha-256=X48E9qOokqqrvdts8nOJRJN3OWDUoyWxBf7kbu9DBPE=:",
		"a lone colon":              "sha-256=:",
		"not base64":                "sha-256=:!!!!:",
		"31 bytes":                  "sha-256=:" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==" + ":",
		"33 bytes":                  "sha-256=:" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" + ":",
		"a parameter on the member": member + ";x=1",
		"an upper-case key":         "SHA-256=:X48E9qOokqqrvdts8nOJRJN3OWDUoyWxBf7kbu9DBPE=:",
		"hex":                       "sha-256=5f8f04f6a3a892aaabbddb6cf273894493773960d4a325b105fee46eef4304f1",
	} {
		if got, err := ParseDigest(value); !errors.Is(err, ErrBadDigest) || got != nil {
			t.Errorf("ParseDigest of %s returned %x, %v; want ErrBadDigest", name, got, err)
		}
		if canon, err := CanonicalDigest(value); err == nil || canon != "" {
			t.Errorf("CanonicalDigest of %s returned %q, %v; want a refusal", name, canon, err)
		}
	}
}

// TestNodeArgsDigest_NoTwoArgumentListsShareADigest.
func TestNodeArgsDigest_NoTwoArgumentListsShareADigest(t *testing.T) {
	base := ArgsDigest("libx265", []string{"-a", "b"}, []string{"-c"})
	if base != ArgsDigest("libx265", []string{"-a", "b"}, []string{"-c"}) {
		t.Fatal("the digest of one list is not stable")
	}
	if _, err := ParseDigest(base); err != nil {
		t.Errorf("an argument-list digest is not in the recorded digest form: %q", base)
	}
	seen := map[string]string{base: "the base list"}
	for name, d := range map[string]string{
		"another encoder":                 ArgsDigest("libsvtav1", []string{"-a", "b"}, []string{"-c"}),
		"an option moved across the seam": ArgsDigest("libx265", []string{"-a"}, []string{"b", "-c"}),
		"two options joined":              ArgsDigest("libx265", []string{"-ab"}, []string{"-c"}),
		"a boundary moved inside pre":     ArgsDigest("libx265", []string{"-", "ab"}, []string{"-c"}),
		"an empty option added":           ArgsDigest("libx265", []string{"-a", "b", ""}, []string{"-c"}),
		"the encoder joined to pre":       ArgsDigest("libx265-a", []string{"b"}, []string{"-c"}),
		"empty lists":                     ArgsDigest("libx265", nil, nil),
	} {
		if prev, dup := seen[d]; dup {
			t.Errorf("%s has the digest of %s", name, prev)
		}
		seen[d] = name
	}
}

// TestNodePathMap_ASourceNoEntryMapsIsRefusedNeverGuessed.
func TestNodePathMap_ASourceNoEntryMapsIsRefusedNeverGuessed(t *testing.T) {
	m := config.PathMap{{From: "/mnt/media", To: "/data/media"}, {From: "/mnt/media/tv", To: "/data/shows"},
		{From: "/same", To: "/same"}}
	for in, want := range map[string]string{
		"/mnt/media/film/a.mkv": "/data/media/film/a.mkv",
		"/mnt/media/tv/s/e.mkv": "/data/shows/s/e.mkv", // the longest prefix wins
		"/same/a.mkv":           "/same/a.mkv",         // an identity entry is a mapping
	} {
		got, err := MapSource(m, in)
		if err != nil || got != want {
			t.Errorf("MapSource(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for name, in := range map[string]string{
		"a path no entry covers":         "/elsewhere/a.mkv",
		"a sibling of a mapped prefix":   "/mnt/media 2/a.mkv",
		"a relative path":                "mnt/media/a.mkv",
		"a path with a parent segment":   "/mnt/media/../secret/a.mkv",
		"a path with a doubled slash":    "/mnt/media//a.mkv",
		"a path with a trailing slash":   "/mnt/media/a.mkv/",
		"the empty path":                 "",
		"a path with a current segment":  "/mnt/media/./a.mkv",
		"an uncovered path that is root": "/",
	} {
		got, err := MapSource(m, in)
		if !errors.Is(err, ErrUnmapped) || got != "" {
			t.Errorf("MapSource of %s returned %q, %v; want ErrUnmapped and no path", name, got, err)
		}
	}
	// An EMPTY map covers nothing: the path is never handed back unchanged as if mapped.
	if got, err := MapSource(nil, "/mnt/media/a.mkv"); !errors.Is(err, ErrUnmapped) || got != "" {
		t.Errorf("an empty map answered %q, %v; want ErrUnmapped", got, err)
	}
}

// TestNodeJob_AJobTheHubCannotLeaseIsRefused.
func TestNodeJob_AJobTheHubCannotLeaseIsRefused(t *testing.T) {
	good := Job{Path: "/lib/a.mkv", Key: "k", Temp: "/lib/a.part", Encoder: "libx265",
		SourceSize: 2, SourceModTime: time.Unix(1, 0)}
	if err := good.validate(); err != nil {
		t.Fatalf("a leasable job was refused: %v", err)
	}
	for name, mutate := range map[string]func(*Job){
		"a relative source":           func(j *Job) { j.Path = "lib/a.mkv" },
		"an unclean source":           func(j *Job) { j.Path = "/lib/../a.mkv" },
		"a relative working file":     func(j *Job) { j.Temp = "a.part" },
		"an unclean working file":     func(j *Job) { j.Temp = "/lib//a.part" },
		"the source as working file":  func(j *Job) { j.Temp = j.Path },
		"no store key":                func(j *Job) { j.Key = "" },
		"no encoder":                  func(j *Job) { j.Encoder = "" },
		"a one-byte source":           func(j *Job) { j.SourceSize = 1 },
		"a negative reservation":      func(j *Job) { j.ReservedBytes = -1 },
		"no source modification time": func(j *Job) { j.SourceModTime = time.Time{} },
	} {
		j := good
		mutate(&j)
		if err := j.validate(); !errors.Is(err, ErrBadJob) {
			t.Errorf("a job with %s returned %v, want ErrBadJob", name, err)
		}
	}
	zero := good
	zero.ReservedBytes = 0
	if err := zero.validate(); err != nil {
		t.Errorf("a reservation of zero bytes was refused: %v", err)
	}
}

// TestNodeHelpers_TheSmallArithmetic.
func TestNodeHelpers_TheSmallArithmetic(t *testing.T) {
	for d, want := range map[time.Duration]int{0: 1, time.Millisecond: 1, time.Second: 1,
		1500 * time.Millisecond: 2, 5 * time.Second: 5, -time.Second: 1} {
		if got := retryAfterSeconds(d); got != want {
			t.Errorf("retryAfterSeconds(%v) = %d, want %d", d, got, want)
		}
	}
	for d, want := range map[time.Duration]int{0: 1, 999 * time.Millisecond: 1, time.Second: 1,
		1999 * time.Millisecond: 1, 2 * time.Second: 2, time.Minute: 60, -time.Second: 1} {
		if got := wholeSeconds(d); got != want {
			t.Errorf("wholeSeconds(%v) = %d, want %d", d, got, want)
		}
	}
	nan := 0.0
	nan = nan / nan
	for in, want := range map[float64]float64{-0.1: 0, 0: 0, 0.5: 0.5, 1: 1, 1.1: 1, nan: 0} {
		if got := clampFraction(in); got != want {
			t.Errorf("clampFraction(%v) = %v, want %v", in, got, want)
		}
	}
	// 30 s of grace, plus one second for every 64 KiB at the slowest allowed rate.
	if got := uploadBudget(64<<10*10, 64<<10, 30*time.Second); got != 40*time.Second {
		t.Errorf("uploadBudget = %v, want 40s", got)
	}
	if got := uploadBudget(1, 64<<10, 30*time.Second); got != 30*time.Second {
		t.Errorf("uploadBudget of one byte = %v, want the grace alone", got)
	}
	for id, want := range map[string]bool{
		"0123456789abcdef0123456789abcdef":  true,
		"0123456789abcdef0123456789abcde":   false,
		"0123456789abcdef0123456789abcdef0": false,
		"0123456789abcdef0123456789abcdeg":  false,
		"":                                  false,
		"../../../../etc/passwd/0123456789": false,
	} {
		if got := validLeaseID(id); got != want {
			t.Errorf("validLeaseID(%q) = %v, want %v", id, got, want)
		}
	}
	id, err := newLeaseID()
	other, _ := newLeaseID()
	if err != nil || !validLeaseID(id) || id == other {
		t.Errorf("newLeaseID gave %q then %q (%v), want two distinct valid ids", id, other, err)
	}
}

// TestNodeAcquire_ABodyOutsideTheProtocolIsNamed.
func TestNodeAcquire_ABodyOutsideTheProtocolIsNamed(t *testing.T) {
	good := AcquireRequest{Node: "node-a", Version: "v1", Slots: 1, Mode: ModeMapped, Encoders: []string{"libx265"}}
	if msg := validAcquire(good); msg != "" {
		t.Fatalf("a good acquire was refused: %s", msg)
	}
	many := make([]string, 65)
	for i := range many {
		many[i] = "e"
	}
	for name, tc := range map[string]struct {
		mutate func(*AcquireRequest)
		names  string
	}{
		"an empty node name":     {func(r *AcquireRequest) { r.Node = "" }, "node"},
		"a node name with a /":   {func(r *AcquireRequest) { r.Node = "a/b" }, "node"},
		"no version":             {func(r *AcquireRequest) { r.Version = "" }, "version"},
		"a 65-character version": {func(r *AcquireRequest) { r.Version = strings.Repeat("v", 65) }, "version"},
		"no free slot":           {func(r *AcquireRequest) { r.Slots = 0 }, "slots"},
		"no encoders":            {func(r *AcquireRequest) { r.Encoders = nil }, "encoders"},
		"65 encoders":            {func(r *AcquireRequest) { r.Encoders = many }, "encoders"},
		"an empty encoder":       {func(r *AcquireRequest) { r.Encoders = []string{"libx265", ""} }, "encoders"},
		"a 65-character encoder": {func(r *AcquireRequest) { r.Encoders = []string{strings.Repeat("e", 65)} }, "encoders"},
	} {
		r := good
		tc.mutate(&r)
		if msg := validAcquire(r); !strings.HasPrefix(msg, tc.names) {
			t.Errorf("%s: refused with %q, want a refusal naming %s", name, msg, tc.names)
		}
	}
	edge := good
	edge.Version, edge.Encoders = strings.Repeat("v", 64), many[:64]
	edge.Encoders[0] = strings.Repeat("e", 64)
	if msg := validAcquire(edge); msg != "" {
		t.Errorf("an acquire at every bound was refused: %s", msg)
	}
}

// TestNodeLeaseError_NamesTheNodeTheEpochAndTheReason.
func TestNodeLeaseError_NamesTheNodeTheEpochAndTheReason(t *testing.T) {
	l := grantedAt(t0)
	l.Epoch = 7
	expired, _ := decideExpire(l, l.ExpiresAt)
	e := leaseErrorOf(expired)
	if e.Node != "node-a" || e.Epoch != 7 || e.LeaseID != "L1" || e.Reason != ReasonExpired || e.Detail != "" {
		t.Errorf("an expired lease's error is %+v", e)
	}
	for _, want := range []string{"node-a", "L1", "epoch 7", "expired"} {
		if !strings.Contains(e.Error(), want) {
			t.Errorf("the error %q does not name %q", e.Error(), want)
		}
	}
	failed, _ := decideFail(l, 7, t0, "encoder_crashed")
	e = leaseErrorOf(failed)
	if e.Reason != ReasonNodeFailed || e.Detail != "encoder_crashed" || !strings.Contains(e.Error(), "(encoder_crashed)") {
		t.Errorf("a node-failed lease's error is %+v: %s", e, e.Error())
	}
	var m Lease
	m = l
	for i := 0; i < 3; i++ {
		m, _ = decideMismatch(m, 7, t0, 3)
	}
	e = leaseErrorOf(m)
	if e.Reason != ReasonDigestMismatch || e.Detail != "" {
		t.Errorf("a lease failed on digest mismatches has the error %+v", e)
	}
}
