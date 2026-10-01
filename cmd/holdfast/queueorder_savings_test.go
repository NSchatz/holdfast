package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
)

// `queue_order: savings_per_hour` (S0164) and queue priority at the command line: `validate`
// accepts and refuses it with the right exit status, `validate` prints priority where it
// prints the rules and profiles, and `plan --json` publishes nothing under savings_per_hour
// that it does not publish under path - no per-file estimated saving anywhere.

// TestValidate_AC1_SavingsPerHourIsAcceptedAndExitsZero is [AC-1]'s command half.
func TestValidate_AC1_SavingsPerHourIsAcceptedAndExitsZero(t *testing.T) {
	cfgPath := emptyLibraryConfig(t, "queue_order: savings_per_hour\n")
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"validate", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("validate exited %d on queue_order: savings_per_hour: %s", code, errOut.String())
	}
	if line := queueOrderLine(t, out.String()); !strings.Contains(line, "queue order: savings_per_hour") {
		t.Errorf("validate's queue-order line does not state savings_per_hour:\n%s", line)
	}
}

// TestValidate_AC2_ANearMissExitsNonZeroNamingAllSix is [AC-2]'s command half.
func TestValidate_AC2_ANearMissExitsNonZeroNamingAllSix(t *testing.T) {
	for _, v := range []string{"savings-per-hour", "Savings_Per_Hour", `"savings_per_hour "`, `""`} {
		t.Run(v, func(t *testing.T) {
			cfgPath := emptyLibraryConfig(t, "queue_order: "+v+"\n")
			var out, errOut bytes.Buffer
			if code := dispatch([]string{"validate", "--config", cfgPath}, &out, &errOut); code == 0 {
				t.Fatalf("validate exited 0 on queue_order: %s:\n%s", v, out.String())
			}
			for _, accepted := range config.QueueOrders {
				if !strings.Contains(errOut.String(), accepted) {
					t.Errorf("the refusal does not name %q:\n%s", accepted, errOut.String())
				}
			}
		})
	}
}

// TestValidate_PrintsPriorityWhereItPrintsRulesAndProfiles: a root's priority in its block,
// a rule's on its rule line, an encode profile's beside the queue order - and nothing at all
// about priority for a configuration that names none.
func TestValidate_PrintsPriorityWhereItPrintsRulesAndProfiles(t *testing.T) {
	cfgPath := emptyLibraryConfig(t, "")
	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(filepath.Dir(cfgPath), "media")
	with := strings.Replace(string(body), "  - "+lib+"\n", "  - path: "+lib+"\n    priority: 7\n"+
		"    rules:\n      - when: {max_source_height: 576}\n        priority: 30\n", 1) +
		"encode_profiles:\n  - name: anime\n    match: \"**/anime/**\"\n    priority: -3\n"
	withPath := filepath.Join(filepath.Dir(cfgPath), "with.yaml")
	if err := os.WriteFile(withPath, []byte(with), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"validate", "--config", withPath}, &out, &errOut); code != 0 {
		t.Fatalf("validate exited %d: %s", code, errOut.String())
	}
	for _, want := range []string{
		"priority: files are offered highest priority first",
		"priority: encode_profiles[0] (anime) priority -3",
		"priority             7",
		"source height any to 576: priority=30",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("validate does not print %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	errOut.Reset()
	if code := dispatch([]string{"validate", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("validate exited %d: %s", code, errOut.String())
	}
	if strings.Contains(out.String(), "priority") {
		t.Errorf("validate mentions priority for a configuration that names none:\n%s", out.String())
	}
}

// TestPlan_AC9_SavingsPerHourPublishesWhatPathPublishes is [AC-9]'s plan clause: over one
// library holding sources a run would transcode, one a guard skips, one whose saving is not
// positive and one whose key cannot be read, `plan --json` reports the same eligible files and
// bytes, the same skip per guard, the same unaccounted-for files and THE SAME SET OF FIELDS
// under savings_per_hour as under path. The plan document carries no per-file record at this
// pin, so the field-set clause is applied to the whole document's key set (S0164 verdict F2):
// no key appears under one order that is absent under the other, which is what keeps a
// per-file estimated saving out of the published surface.
func TestPlan_AC9_SavingsPerHourPublishesWhatPathPublishes(t *testing.T) {
	cfgPath, lib, _ := planLibrary(t, "")
	// One whose saving is not positive: a 40 kbit/s source is below what the model expects
	// any encode of a 320x240 picture to produce.
	low := filepath.Join(lib, "low", "trickle.mkv")
	if err := os.MkdirAll(filepath.Dir(low), 0o755); err != nil {
		t.Fatal(err)
	}
	ffmpeg := envOr("HOLDFAST_FFMPEG", "ffmpeg")
	if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10", "-c:v", "libx264", "-preset", "ultrafast",
		"-b:v", "40k", "-maxrate", "40k", "-bufsize", "40k", "-pix_fmt", "yuv420p", "--", low).CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", low, err, out)
	}
	t.Logf("the non-positive source probes at %d kbit/s",
		probe.New(ffmpeg, envOr("HOLDFAST_FFPROBE", "ffprobe")).VideoProps(t.Context(), low).BitrateKbps())
	// And one whose key cannot be read: not media at all.
	writeCensusFile(t, filepath.Join(lib, "broken.mkv"), "not a video\n")

	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	savingsPath := filepath.Join(filepath.Dir(cfgPath), "savings.yaml")
	if err := os.WriteFile(savingsPath, append(body, []byte("queue_order: savings_per_hour\n")...), 0o600); err != nil {
		t.Fatal(err)
	}

	docs := map[string]map[string]any{}
	for name, p := range map[string]string{"path": cfgPath, "savings_per_hour": savingsPath} {
		var out, errOut bytes.Buffer
		if code := dispatch([]string{"plan", "--json", "--config", p}, &out, &errOut); code != 0 {
			t.Fatalf("plan --json under %s exited %d: %s", name, code, errOut.String())
		}
		var doc map[string]any
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("plan --json under %s: %v", name, err)
		}
		docs[name] = doc
	}

	keysA, keysB := keyPaths(docs["path"], ""), keyPaths(docs["savings_per_hour"], "")
	if !slices.Equal(keysA, keysB) {
		t.Errorf("plan --json publishes different fields under savings_per_hour:\n path: %v\n savings: %v",
			keysA, keysB)
	}
	for _, k := range keysB {
		if strings.Contains(k, "saving") && !slices.Contains(keysA, k) {
			t.Errorf("plan publishes %q under savings_per_hour only", k)
		}
	}
	// The figures that decide a plan: the eligible set and its bytes, the skips per guard,
	// the files nothing could account for. The probe count is NOT compared: the ordering's
	// own probes are part of what a savings_per_hour plan costs, and it says so.
	for _, k := range []string{"total", "roots", "profiles", "declined"} {
		a, _ := json.Marshal(docs["path"][k])
		b, _ := json.Marshal(docs["savings_per_hour"][k])
		if !bytes.Equal(a, b) {
			t.Errorf("plan's %q differs between the orders:\n path: %s\n savings: %s", k, a, b)
		}
	}
	pa := docs["path"]["probe_snapshots_taken"].(float64)
	pb := docs["savings_per_hour"]["probe_snapshots_taken"].(float64)
	t.Logf("probe snapshots: %v under path, %v under savings_per_hour", pa, pb)
	if pb <= pa {
		t.Errorf("savings_per_hour's plan reports %v probe snapshots, path's %v: the ordering's own probes "+
			"are missing from the figure", pb, pa)
	}
}

// keyPaths lists every key path of a decoded JSON document, list indexes elided, sorted.
func keyPaths(v any, prefix string) []string {
	set := map[string]bool{}
	var walk func(v any, prefix string)
	walk = func(v any, prefix string) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				set[prefix+"."+k] = true
				walk(e, prefix+"."+k)
			}
		case []any:
			for _, e := range x {
				walk(e, prefix+"[]")
			}
		}
	}
	walk(v, prefix)
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
