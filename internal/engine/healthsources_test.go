package engine

import (
	"path/filepath"
	"sort"
	"testing"
)

// TestHealthSources_OffersExactlyWhatAScanOffers: the health sweep's enumeration is the
// scan's, under both enumeration branches - the path filters apply, and with no filter
// every source is offered - and it stops when its consumer does. A sweep that read a set
// of its own would report on files the pipeline never sees and miss ones it does.
func TestHealthSources_OffersExactlyWhatAScanOffers(t *testing.T) {
	root, coverage := pathFilterFixture(t)
	for _, tc := range []struct {
		name    string
		exclude []string
	}{
		{"no filter", nil},
		{"an exclude pattern", []string{"movies/4k/**"}},
	} {
		for _, branch := range []struct {
			name     string
			coverage []string
		}{
			{"walking the library roots directly", nil},
			{"bounded by the startup walk", coverage},
		} {
			t.Run(tc.name+", "+branch.name, func(t *testing.T) {
				e := pathFilterEngine(t, root, branch.coverage, tc.exclude, nil)
				want := offeredRel(t, e, root)
				var got []string
				e.HealthSources(func() bool { return false }, func(p string) bool {
					rel, err := filepath.Rel(root, p)
					if err != nil {
						t.Fatal(err)
					}
					got = append(got, filepath.ToSlash(rel))
					return true
				})
				sort.Strings(got)
				if !equalStrings(got, want) || len(want) == 0 {
					t.Fatalf("the sweep was offered %v, a scan %v", got, want)
				}
				if tc.exclude != nil && len(got) != len(everyFileInTheFixture)-1 {
					t.Fatalf("the exclude pattern kept %d file(s) out, want 1: %v", len(everyFileInTheFixture)-len(got), got)
				}

				var once []string
				e.HealthSources(func() bool { return false }, func(p string) bool {
					once = append(once, p)
					return false
				})
				if len(once) != 1 {
					t.Errorf("a consumer that refused the first file was offered %d", len(once))
				}
				var none []string
				e.HealthSources(func() bool { return true }, func(p string) bool {
					none = append(none, p)
					return true
				})
				if len(none) != 0 {
					t.Errorf("a stopped sweep was offered %v", none)
				}
			})
		}
	}
}
