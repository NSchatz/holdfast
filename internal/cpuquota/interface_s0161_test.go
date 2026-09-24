package cpuquota

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestS0161_AC5_AnAbsentInterfaceIsToldApartFromABrokenOne grades the reading half of
// AC-5 and AC-6: a host with no bandwidth interface at all is ErrNoInterface, which a
// caller answers by keeping the encoder's defaults quietly, and a file that EXISTS and
// cannot be used is never that - it is an InterfaceError naming the file and what it
// held, which a caller has to state.
func TestS0161_AC5_AnAbsentInterfaceIsToldApartFromABrokenOne(t *testing.T) {
	atRoot(t)

	t.Run("no interface under either layout", func(t *testing.T) {
		_, err := Read(fakeRoot(t, map[string]string{}))
		if !errors.Is(err, ErrNoInterface) {
			t.Fatalf("Read over an empty mount = %v, want ErrNoInterface", err)
		}
		if !errors.Is(err, ErrNoQuota) {
			t.Errorf("the absent case must still wrap ErrNoQuota: %v", err)
		}
		var ie *InterfaceError
		if errors.As(err, &ie) {
			t.Errorf("an absent interface carries an InterfaceError for %s: nothing exists to have failed", ie.Path)
		}
	})

	// content is what the file held, and "" where it could not be read at all.
	for _, tc := range []struct {
		name    string
		files   map[string]string
		path    string // relative to the root
		content string
	}{
		{"v2 malformed", map[string]string{"cpu.max": " lots 100000 \n"}, "cpu.max", "lots 100000"},
		{"v2 a directory in place of the file", map[string]string{"cpu.max/x": "y"}, "cpu.max", ""},
		{"v1 malformed quota", map[string]string{"cpu/cpu.cfs_quota_us": "plenty\n",
			"cpu/cpu.cfs_period_us": "100000"}, "cpu/cpu.cfs_quota_us", "plenty"},
		{"v1 quota without a period", map[string]string{"cpu/cpu.cfs_quota_us": "200000"},
			"cpu/cpu.cfs_period_us", ""},
		{"v1 malformed period", map[string]string{"cpu/cpu.cfs_quota_us": "200000",
			"cpu/cpu.cfs_period_us": "often"}, "cpu/cpu.cfs_period_us", "often"},
		{"v1 zero period", map[string]string{"cpu/cpu.cfs_quota_us": "200000",
			"cpu/cpu.cfs_period_us": "0"}, "cpu/cpu.cfs_quota_us", "quota 200000 period 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := fakeRoot(t, tc.files)
			_, err := Read(root)
			if err == nil {
				t.Fatal("Read of a broken interface returned no error")
			}
			if errors.Is(err, ErrNoInterface) {
				t.Errorf("a file that exists and cannot be used was reported as NO interface: %v", err)
			}
			if !errors.Is(err, ErrNoQuota) {
				t.Errorf("the error does not wrap ErrNoQuota: %v", err)
			}
			var ie *InterfaceError
			if !errors.As(err, &ie) {
				t.Fatalf("the error carries no InterfaceError, so a caller cannot name the file: %v", err)
			}
			if want := filepath.Join(root, tc.path); ie.Path != want {
				t.Errorf("InterfaceError.Path = %q, want %q", ie.Path, want)
			}
			if ie.Content != tc.content {
				t.Errorf("InterfaceError.Content = %q, want %q", ie.Content, tc.content)
			}
			if ie.Error() == "" || ie.Unwrap() == nil {
				t.Errorf("InterfaceError has no message or no cause: %q / %v", ie.Error(), ie.Unwrap())
			}
		})
	}
}
