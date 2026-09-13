package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/store"
)

// AC-A10, the export half: `holdfast export` carries the profile field in the NDJSON
// row.
//
// It is graded HERE rather than in internal/store because this is where the row is
// produced: the export's format is defined as "what /api/history publishes for a
// row", which internal/store cannot see (server imports store, not the other way
// round). The store's own half - the column, its round trip and its clearing on a
// retry - is in internal/store/profile_test.go.
//
// The assertion is made on the RAW BYTES of the line, not on a decoded struct.
// Decoding into a struct would give "" for both a present-and-empty key and an absent
// one, which is precisely the distinction this field's serialization exists to keep:
// "" says the top-level settings ran, and a MISSING key would say this build does not
// report the profile at all.
func TestExport_TheNDJSONRowCarriesTheProfileThatSuppliedTheSettings(t *testing.T) {
	stateDir, cfgPath := exportFixture(t)
	st := openFixtureStore(t, stateDir)

	finishRow(t, st, "/lib/4k.mkv", store.Done, &store.Outcome{
		Encoder: "svtav1", Profile: "4k-av1",
		SourceBytes: ptrI(4096), OutputBytes: ptrI(1024),
	})
	finishRow(t, st, "/lib/plain.mkv", store.Done, &store.Outcome{
		Encoder: "cpu",
		// No profile: the top-level settings ran.
		SourceBytes: ptrI(4096), OutputBytes: ptrI(1024),
	})
	finishRow(t, st, "/lib/skipped.mkv", store.Skipped, &store.Outcome{
		Reason: "already-at-target-codec", Profile: "bulk-tv",
	})
	_ = st.Close()

	code, stdout, stderr := runExport(t, "--config", cfgPath)
	if code != 0 {
		t.Fatalf("export exited %d (stderr: %s)", code, stderr)
	}

	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 NDJSON rows, got %d:\n%s", len(lines), stdout)
	}

	want := map[string]string{
		"/lib/4k.mkv":      "4k-av1",
		"/lib/plain.mkv":   "",
		"/lib/skipped.mkv": "bulk-tv",
	}
	for _, line := range lines {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			t.Fatalf("row is not JSON: %v\n%s", err, line)
		}
		var path string
		if err := json.Unmarshal(raw["path"], &path); err != nil {
			t.Fatalf("row has no path: %s", line)
		}
		got, present := raw["profile"]
		if !present {
			t.Fatalf("the NDJSON row for %s carries NO profile key - a consumer cannot tell "+
				"\"the top-level settings ran\" from \"this build does not report it\":\n%s", path, line)
		}
		var profile string
		if err := json.Unmarshal(got, &profile); err != nil {
			t.Fatalf("the profile field of %s is not a string: %s", path, line)
		}
		if profile != want[path] {
			t.Fatalf("the NDJSON row for %s reports profile %q, want %q", path, profile, want[path])
		}
	}
}
