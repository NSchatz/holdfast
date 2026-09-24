package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The S0157 startup record is graded on what a REAL `holdfast run` child process writes to
// its own standard error, with the cgroup hierarchy it reads handed to it through
// HOLDFAST_CGROUP_ROOT (s0161Run builds both).

// s0157Memory returns the records the memory bound's derivation emitted, at any level.
func s0157Memory(recs []s0161Record) []s0161Record {
	var out []s0161Record
	for _, r := range recs {
		if r.attrs["component"] == "encode.memory" {
			out = append(out, r)
		}
	}
	return out
}

// s0157One runs `holdfast run` under files and returns its one encode.memory record, failing
// unless the run exits 0, says so exactly once, and records nothing at error.
func s0157One(t *testing.T, files map[string]string) s0161Record {
	t.Helper()
	recs, _, code := s0161Run(t, files, "")
	if code != 0 {
		t.Fatalf("the run exited %d: a cgroup memory reading must never fail the run", code)
	}
	for _, r := range recs {
		if strings.EqualFold(r.level, "ERROR") {
			t.Errorf("the run recorded at error: %s", r.raw)
		}
	}
	mem := s0157Memory(recs)
	if len(mem) != 1 {
		var all []string
		for _, r := range recs {
			all = append(all, r.raw)
		}
		t.Fatalf("%d encode.memory records, want exactly 1:\n%s", len(mem), strings.Join(all, "\n"))
	}
	return mem[0]
}

// TestS0157_AC13_AnEstablishedLimitIsOneInfoRecordWithTheFigures grades AC-13's startup half:
// one info record carrying the limit and the threshold, in bytes, as attributes.
func TestS0157_AC13_AnEstablishedLimitIsOneInfoRecordWithTheFigures(t *testing.T) {
	r := s0157One(t, map[string]string{"memory.max": "8589934592\n"})
	if !strings.EqualFold(r.level, "INFO") || r.msg != "encode memory watchdog armed" {
		t.Errorf("the record is %s %q, want INFO %q\n%s", r.level, r.msg, "encode memory watchdog armed", r.raw)
	}
	s0161Want(t, r, map[string]string{
		"memory_limit_bytes":     "8589934592",
		"memory_threshold_bytes": "7301444403",
	})
}

// TestS0157_AC12_NoLimitIsSaidOnceAtTheRightLevel grades AC-12's startup half: no limit set
// is one info record; a memory.max that exists and is unreadable or malformed is one warn
// naming the cgroup memory interface, what was read there and that encodes run unwatched.
func TestS0157_AC12_NoLimitIsSaidOnceAtTheRightLevel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{"max", map[string]string{"memory.max": "max\n"}},
		{"absent", map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := s0157One(t, tc.files)
			if !strings.EqualFold(r.level, "INFO") || !strings.Contains(r.msg, "not armed") {
				t.Errorf("the record is %s %q, want an INFO saying the watchdog is not armed\n%s", r.level, r.msg, r.raw)
			}
			if !strings.Contains(r.attrs["next_action"], "unwatched") {
				t.Errorf("the record does not say encodes run unwatched\n%s", r.raw)
			}
		})
	}
	for _, tc := range []struct {
		name  string
		files map[string]string
		read  string
	}{
		{"malformed", map[string]string{"memory.max": "garbage\n"}, "garbage"},
		{"unreadable", map[string]string{"memory.max/x": "a directory where the file should be"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := s0157One(t, tc.files)
			if !strings.EqualFold(r.level, "WARN") {
				t.Fatalf("the record is %s, want WARN\n%s", r.level, r.raw)
			}
			if !strings.Contains(r.attrs["dependency"], "cgroup memory interface") {
				t.Errorf("the warn does not name the cgroup memory interface as its dependency\n%s", r.raw)
			}
			if !strings.HasSuffix(r.attrs["interface"], filepath.Join("cgroup", "memory.max")) {
				t.Errorf("the warn names interface %q, want the memory.max it tried\n%s", r.attrs["interface"], r.raw)
			}
			if r.attrs["read"] != tc.read {
				t.Errorf("the warn says it read %q there, want %q\n%s", r.attrs["read"], tc.read, r.raw)
			}
			if !strings.Contains(r.attrs["next_action"], "unwatched") {
				t.Errorf("the warn does not say encodes run unwatched\n%s", r.raw)
			}
		})
	}
}
