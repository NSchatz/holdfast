package memlimit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The S0157 limit discovery, graded on fixture cgroup hierarchies: the filesystem is outside
// this package's boundary, and a directory a test builds is how the unlimited, absent,
// unreadable and malformed paths are reached without a second host.

// s0157Levels builds one directory per level, most specific first, writing memory.max at
// each level whose entry in files is not "-" ("-" leaves the level without the file).
func s0157Levels(t *testing.T, files ...string) []string {
	t.Helper()
	root := t.TempDir()
	levels := make([]string, len(files))
	dir := root
	for i := len(files) - 1; i >= 0; i-- {
		if i < len(files)-1 {
			dir = filepath.Join(dir, "child")
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		levels[i] = dir
		if files[i] != "-" {
			if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(files[i]), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return levels
}

// TestS0157_AC3_TheThresholdIs85PercentOfTheSmallestLimitOnTheWalk grades what AC-3's
// "threshold" is: 85% of the effective limit, rounded down to a whole byte, where the
// effective limit is the smallest numeric memory.max from the process's cgroup up through
// its ancestors.
func TestS0157_AC3_TheThresholdIs85PercentOfTheSmallestLimitOnTheWalk(t *testing.T) {
	for _, tc := range []struct {
		name      string
		files     []string
		limit     int64
		threshold int64
		origin    int
	}{
		{"one level", []string{"8589934592\n"}, 8589934592, 7301444403, 0},
		{"the parent is tighter", []string{"8589934592\n", "max\n", "4294967296\n"}, 4294967296, 3650722201, 2},
		{"the child is tighter", []string{"1000\n", "2000\n"}, 1000, 850, 0},
		{"a level with no file imposes nothing", []string{"-", "99\n"}, 99, 84, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			levels := s0157Levels(t, tc.files...)
			lim, err := readLevels(levels)
			if err != nil {
				t.Fatalf("readLevels: %v", err)
			}
			if lim.Bytes != tc.limit || !lim.Set() {
				t.Errorf("limit = %d (set %v), want %d", lim.Bytes, lim.Set(), tc.limit)
			}
			if got := lim.Threshold(); got != tc.threshold {
				t.Errorf("threshold = %d, want %d", got, tc.threshold)
			}
			if want := filepath.Join(levels[tc.origin], "memory.max"); lim.Origin != want {
				t.Errorf("origin = %q, want %q", lim.Origin, want)
			}
		})
	}
}

// TestS0157_AC12_NoLimitIsEstablishedFromMaxAbsentUnreadableOrMalformed grades AC-12's
// discovery half: "max" everywhere and a walk with no memory.max at all are no limit and no
// error of the unusable kind; a file that exists and cannot be read, or holds neither "max"
// nor a positive byte count, is an InterfaceError naming the file and what it held. None of
// them yields a threshold.
func TestS0157_AC12_NoLimitIsEstablishedFromMaxAbsentUnreadableOrMalformed(t *testing.T) {
	t.Run("max", func(t *testing.T) {
		lim, err := readLevels(s0157Levels(t, "max\n", "max\n"))
		if err != nil || lim.Set() || lim.Threshold() != 0 {
			t.Errorf("max: limit %+v threshold %d err %v, want no limit and no error", lim, lim.Threshold(), err)
		}
	})
	t.Run("absent", func(t *testing.T) {
		lim, err := readLevels(s0157Levels(t, "-", "-"))
		if !errors.Is(err, ErrNoInterface) {
			t.Errorf("absent: err = %v, want ErrNoInterface", err)
		}
		var ie *InterfaceError
		if errors.As(err, &ie) {
			t.Errorf("absent: an absent interface was reported as an unusable one: %v", err)
		}
		if lim.Set() {
			t.Errorf("absent: limit %+v", lim)
		}
	})
	for _, tc := range []struct {
		name, body, read string
	}{
		{"malformed", "garbage\n", "garbage"},
		{"zero", "0\n", "0"},
		{"negative", "-5\n", "-5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			levels := s0157Levels(t, "1000\n", tc.body)
			lim, err := readLevels(levels)
			var ie *InterfaceError
			if !errors.As(err, &ie) {
				t.Fatalf("err = %v, want an InterfaceError", err)
			}
			if want := filepath.Join(levels[1], "memory.max"); ie.Path != want || ie.Content != tc.read {
				t.Errorf("InterfaceError names %q holding %q, want %q holding %q", ie.Path, ie.Content, want, tc.read)
			}
			if lim.Set() || errors.Is(err, ErrNoInterface) {
				t.Errorf("an unusable file yielded limit %+v / was reported absent: %v", lim, err)
			}
		})
	}
	t.Run("unreadable", func(t *testing.T) {
		levels := s0157Levels(t, "-")
		if err := os.Mkdir(filepath.Join(levels[0], "memory.max"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := readLevels(levels)
		var ie *InterfaceError
		if !errors.As(err, &ie) || ie.Path != filepath.Join(levels[0], "memory.max") || ie.Content != "" {
			t.Errorf("unreadable: err = %v, want an InterfaceError naming the file with nothing read", err)
		}
	})
}

// TestS0157_AC12_ReadWalksTheCgroupRootItWasHanded grades the seam AC-12 and AC-13 are
// steered through: Read takes the mount it is handed, not the host's.
func TestS0157_AC12_ReadWalksTheCgroupRootItWasHanded(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "memory.max"), []byte("4000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lim, err := Read(root)
	if err != nil || lim.Bytes != 4000 || lim.Threshold() != 3400 {
		t.Errorf("Read(%s) = %+v, %v; want a limit of 4000 and a threshold of 3400", root, lim, err)
	}
}
