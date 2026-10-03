package store

import (
	"reflect"
	"testing"
)

// TestS0172_AC10_ReportedCountsFoldsOnlyWhatIsThereAndFabricatesNothing grades the shared
// vocabulary rule behind [AC-1], [AC-3], [AC-5], [AC-6] and [AC-10] of S0172, case by
// case: a live engine counts would-transcode rows inside pending and reports no such key,
// a dry one reports the ledger as it is, the total is conserved, no key is invented, and
// the caller's map is never modified.
func TestS0172_AC10_ReportedCountsFoldsOnlyWhatIsThereAndFabricatesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		sum  map[Status]int
		live bool
		want map[Status]int
	}{
		{name: "live folds would-transcode into pending",
			sum: map[Status]int{Pending: 2, WouldTranscode: 3, Done: 5, Skipped: 1}, live: true,
			want: map[Status]int{Pending: 5, Done: 5, Skipped: 1}},
		{name: "live with no pending row creates pending from the fold",
			sum: map[Status]int{WouldTranscode: 3, Done: 5}, live: true,
			want: map[Status]int{Pending: 3, Done: 5}},
		{name: "live with no would-transcode row reports the ledger as it is",
			sum: map[Status]int{Pending: 2, Done: 5}, live: true,
			want: map[Status]int{Pending: 2, Done: 5}},
		{name: "live with neither fabricates no pending key",
			sum: map[Status]int{Done: 5, Failed: 1}, live: true,
			want: map[Status]int{Done: 5, Failed: 1}},
		{name: "live with a zero would-transcode count drops the key and fabricates no pending",
			sum: map[Status]int{WouldTranscode: 0, Done: 5}, live: true,
			want: map[Status]int{Done: 5}},
		{name: "live over an empty ledger", sum: map[Status]int{}, live: true, want: map[Status]int{}},
		{name: "live over an unread ledger", sum: nil, live: true, want: map[Status]int{}},
		{name: "dry reports would-transcode as itself",
			sum: map[Status]int{Pending: 2, WouldTranscode: 3, Done: 5}, live: false,
			want: map[Status]int{Pending: 2, WouldTranscode: 3, Done: 5}},
		{name: "dry over an empty ledger", sum: map[Status]int{}, live: false, want: map[Status]int{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := make(map[Status]int, len(tc.sum))
			total := 0
			for st, n := range tc.sum {
				before[st] = n
				total += n
			}

			got := ReportedCounts(tc.sum, tc.live)

			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ReportedCounts = %v, want %v", got, tc.want)
			}
			reported := 0
			for _, n := range got {
				reported += n
			}
			if reported != total {
				t.Errorf("the reported counts sum to %d, the ledger holds %d rows", reported, total)
			}
			if tc.sum != nil && !reflect.DeepEqual(tc.sum, before) {
				t.Errorf("the caller's map was modified: %v, was %v", tc.sum, before)
			}
		})
	}
}
