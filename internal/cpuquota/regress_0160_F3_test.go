package cpuquota

// S0160 impl-gate 1, F3 (AC-5). Refuter artifact.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegress_0160_F3_UnreadableCpuMaxOnTheWalkIsSkipped(t *testing.T) {
	root := fakeRoot(t, map[string]string{"cpu.max": "max 100000\n", "hf/cpu.max": "200000 100000\n"})
	own := filepath.Join(root, "hf", "cpu.max")
	if err := os.Chmod(own, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(own); err == nil {
		t.Fatal("precondition: the file is still readable by this user")
	}
	self := filepath.Join(t.TempDir(), "self-cgroup")
	if err := os.WriteFile(self, []byte("0::/hf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := selfCgroup
	selfCgroup = self
	t.Cleanup(func() { selfCgroup = old })
	if q, err := Read(root); err == nil {
		t.Fatalf("Read = %+v, no error, though this process's own cpu.max (a 2-CPU limit) is "+
			"unreadable: Divide(q, 1) = %d and no warn (AC-5)", q, Divide(q, 1))
	}
}
