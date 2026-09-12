package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Per-library profiles: what an operator gets, and what they are refused.
//
// The compatibility proof leads, because it is the one that is not negotiable. A
// released version's configuration is a promise, and every configuration written against
// the flat `library_roots` list has to keep meaning exactly what it meant - so the first
// test here asserts the resolved values of a flat list against expectations COMMITTED
// IN THE TEST, which is the only comparison that stays available once the change exists
// (there is no earlier build left to diff against).

// loadYAML writes cfg to a temp file and loads it, failing the test on a refusal.
func loadYAML(t *testing.T, cfg string) *Config {
	t.Helper()
	c, err := load(t, cfg)
	if err != nil {
		t.Fatalf("Load(%s): %v", cfg, err)
	}
	return c
}

// load writes cfg to a temp file and loads it, returning whatever Load returned.
func load(t *testing.T, cfg string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return Load(p)
}

// rootByPath returns the resolved root with that cleaned path.
func rootByPath(t *testing.T, c *Config, clean string) Root {
	t.Helper()
	for _, r := range c.RootProfiles() {
		if r.Clean == clean {
			return r
		}
	}
	t.Fatalf("no resolved root %q (have %v)", clean, rootPaths(c))
	return Root{}
}

func rootPaths(c *Config) []string {
	var out []string
	for _, r := range c.RootProfiles() {
		out = append(out, r.Clean)
	}
	return out
}

// TestLoad_FlatStringRootsAreUnchanged is the compatibility criterion, and it is asserted
// against values written down HERE rather than against another build: once profiles
// exist there is no pre-profile holdfast left to compare with, so a route that compared
// with one could only ever be claimed and never measured.
//
// Two halves, and both matter. Every root of a flat list resolves to a profile equal to
// the TOP-LEVEL value of every overridable knob - a string entry means "this root, all
// defaults", so nothing may shift under it. And every knob OUTSIDE the overridable set
// keeps its top-level value untouched, because a profile is not allowed to reach them.
func TestLoad_FlatStringRootsAreUnchanged(t *testing.T) {
	c := loadYAML(t, `
library_roots:
  - /mnt/tv
  - /mnt/movies
encoder: svtav1
crf: 18
preset: medium
pixel_format: yuv420p10le
container_ext: mkv
min_bitrate_kbps: 1200
min_savings_percent: 5
skip_hardlinked: false
vmaf_enable: true
min_vmaf: 93
vmaf_min_pool: 55
vmaf_min_chroma: 28
vmaf_subsample: 2
vmaf_model: version=vmaf_v0.6.1
workers: 4
state_dir: /var/lib/holdfast
undo_window_hours: 24
history_retention_rows: 500
max_failures: 7
duration_tolerance_sec: 2.5
run_window: 01:00-06:00
max_load: 1.5
`)

	// The expectations, committed. This is the whole configuration a flat list has always
	// produced, spelled out so a change to any of it is a red test and not a shrug.
	no := false
	yes := true
	want := Profile{
		Encoder:           "svtav1",
		CRF:               18,
		Preset:            "medium",
		PixelFormat:       "yuv420p10le",
		ContainerExt:      "mkv",
		MinBitrateKbps:    1200,
		MinSavingsPercent: 5,
		SkipHardlinked:    &no,
		VmafEnable:        &yes,
		MinVmaf:           93,
		VmafMinPool:       55,
		VmafMinChroma:     28,
		VmafSubsample:     2,
		VmafModel:         "version=vmaf_v0.6.1",
	}
	roots := c.RootProfiles()
	if len(roots) != 2 {
		t.Fatalf("resolved %d roots %v, want 2", len(roots), rootPaths(c))
	}
	for _, r := range roots {
		if !sameProfile(r.Profile, want) {
			t.Errorf("root %s resolved to %s, want the top-level values %s",
				r.Clean, showProfile(r.Profile), showProfile(want))
		}
		// Every knob came from the top level, and none from a profile: a string entry
		// carries none, so `validate` must never claim one did.
		for _, knob := range ProfileKnobs() {
			if got := r.LayerOf(knob); got != LayerTopLevel {
				t.Errorf("root %s knob %s says it came from the %s layer, want %s - a string entry "+
					"carries no profile", r.Clean, knob, got, LayerTopLevel)
			}
		}
	}

	// The knobs OUTSIDE the overridable set, at their top-level resolved values. A
	// profile can never reach these, so a flat list must leave every one of them alone.
	if c.Workers != 4 || c.StateDir != "/var/lib/holdfast" || c.UndoWindowHours != 24 ||
		c.HistoryRetentionRows != 500 || c.MaxFailures != 7 || c.DurationToleranceSec != 2.5 ||
		c.RunWindow != "01:00-06:00" || c.MaxLoad != 1.5 {
		t.Errorf("a daemon-level knob moved: workers=%d state_dir=%q undo_window_hours=%d "+
			"history_retention_rows=%d max_failures=%d duration_tolerance_sec=%g run_window=%q max_load=%g",
			c.Workers, c.StateDir, c.UndoWindowHours, c.HistoryRetentionRows, c.MaxFailures,
			c.DurationToleranceSec, c.RunWindow, c.MaxLoad)
	}
	if len(c.LibraryRoots) != 2 || c.LibraryRoots[0] != "/mnt/tv" || c.LibraryRoots[1] != "/mnt/movies" {
		t.Errorf("LibraryRoots = %v, want the paths as configured", c.LibraryRoots)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// TestLoad_StringRootsAndObjectRootsAreEquivalent is the operator's own test: the two
// spellings of ONE configuration must produce identical resolved profiles. Written once
// as a flat list with the knobs at the top, and once as mappings carrying those same
// knobs on every entry.
func TestLoad_StringRootsAndObjectRootsAreEquivalent(t *testing.T) {
	flat := loadYAML(t, `
library_roots:
  - /mnt/tv
  - /mnt/movies
encoder: cpu
crf: 19
preset: slower
min_bitrate_kbps: 900
vmaf_min_pool: 45
vmaf_model: auto
`)
	objects := loadYAML(t, `
library_roots:
  - path: /mnt/tv
    encoder: cpu
    crf: 19
    preset: slower
    min_bitrate_kbps: 900
    vmaf_min_pool: 45
    vmaf_model: auto
  - path: /mnt/movies
    encoder: cpu
    crf: 19
    preset: slower
    min_bitrate_kbps: 900
    vmaf_min_pool: 45
    vmaf_model: auto
`)

	for _, root := range []string{"/mnt/tv", "/mnt/movies"} {
		a := rootByPath(t, flat, root).Profile
		b := rootByPath(t, objects, root).Profile
		if !sameProfile(a, b) {
			t.Errorf("root %s resolved differently by spelling:\n  strings: %s\n  mappings: %s",
				root, showProfile(a), showProfile(b))
		}
		if a.Digest() != b.Digest() {
			t.Errorf("root %s digests differ by spelling: %s vs %s", root, a.Digest(), b.Digest())
		}
	}
}

// TestLoad_BareMappingRootEqualsTheStringForm: an entry that is a mapping carrying only
// `path` is the string form of that path, not merely something like it. It has its own
// test because it is its own claim - a mapping takes a different code path into the
// resolver, and "resolves the same" is exactly what could quietly stop being true.
func TestLoad_BareMappingRootEqualsTheStringForm(t *testing.T) {
	strForm := loadYAML(t, "library_roots:\n  - /mnt/tv\ncrf: 21\nvmaf_min_chroma: 26\n")
	mapForm := loadYAML(t, "library_roots:\n  - path: /mnt/tv\ncrf: 21\nvmaf_min_chroma: 26\n")

	a := rootByPath(t, strForm, "/mnt/tv")
	b := rootByPath(t, mapForm, "/mnt/tv")
	if !sameProfile(a.Profile, b.Profile) {
		t.Fatalf("a bare mapping root did not resolve like the string form:\n  string:  %s\n  mapping: %s",
			showProfile(a.Profile), showProfile(b.Profile))
	}
	if a.Path != b.Path || a.Clean != b.Clean {
		t.Errorf("path spellings differ: %q/%q vs %q/%q", a.Path, a.Clean, b.Path, b.Clean)
	}
	for _, knob := range ProfileKnobs() {
		if a.LayerOf(knob) != b.LayerOf(knob) {
			t.Errorf("knob %s: string form says %s, bare mapping says %s",
				knob, a.LayerOf(knob), b.LayerOf(knob))
		}
	}
}

// TestLoad_ProfileInheritsThreeLayers is the inheritance itself: built-in default <-
// top-level <- the entry's own profile, with the zero-vs-absent discipline koanf's
// defaults layer already gives at the top level. A knob PRESENT in a profile overrides
// whatever its value, so an explicit 0 is a 0 and not an inherit - which is the half a
// struct-zero default gets wrong, and the half that decides whether a bitrate guard runs
// at all.
func TestLoad_ProfileInheritsThreeLayers(t *testing.T) {
	c := loadYAML(t, `
library_roots:
  - path: /mnt/tv
    crf: 0
    min_bitrate_kbps: 0
    skip_hardlinked: false
    vmaf_enable: false
  - path: /mnt/movies
    crf: 16
  - /mnt/anime
crf: 20
preset: fast
`)
	tv := rootByPath(t, c, "/mnt/tv")
	movies := rootByPath(t, c, "/mnt/movies")
	anime := rootByPath(t, c, "/mnt/anime")

	// Present in the profile, at the type's zero: the profile's value wins.
	if tv.Profile.CRF != 0 {
		t.Errorf("/mnt/tv crf = %d, want the profile's explicit 0", tv.Profile.CRF)
	}
	if tv.Profile.MinBitrateKbps != 0 {
		t.Errorf("/mnt/tv min_bitrate_kbps = %d, want the profile's explicit 0", tv.Profile.MinBitrateKbps)
	}
	if tv.Profile.HardlinkSkip() {
		t.Error("/mnt/tv skip_hardlinked = true, want the profile's explicit false")
	}
	if tv.Profile.VmafGate() {
		t.Error("/mnt/tv vmaf_enable = true, want the profile's explicit false")
	}
	for _, knob := range []string{"crf", "min_bitrate_kbps", "skip_hardlinked", "vmaf_enable"} {
		if got := tv.LayerOf(knob); got != LayerProfile {
			t.Errorf("/mnt/tv knob %s came from %s, want %s", knob, got, LayerProfile)
		}
	}

	// Absent from the profile, present at the top level: the top-level value.
	if movies.Profile.Preset != "fast" {
		t.Errorf("/mnt/movies preset = %q, want the top-level \"fast\"", movies.Profile.Preset)
	}
	if got := movies.LayerOf("preset"); got != LayerTopLevel {
		t.Errorf("/mnt/movies preset came from %s, want %s", got, LayerTopLevel)
	}
	if movies.Profile.CRF != 16 {
		t.Errorf("/mnt/movies crf = %d, want the profile's 16", movies.Profile.CRF)
	}

	// Absent from both: the built-in default, and it says so.
	if movies.Profile.VmafMinPool != 60 {
		t.Errorf("/mnt/movies vmaf_min_pool = %g, want the built-in default 60", movies.Profile.VmafMinPool)
	}
	if got := movies.LayerOf("vmaf_min_pool"); got != LayerDefault {
		t.Errorf("/mnt/movies vmaf_min_pool came from %s, want %s", got, LayerDefault)
	}

	// A string entry beside mapping entries inherits the whole top level, unaffected by
	// what its neighbours overrode.
	if anime.Profile.CRF != 20 || anime.Profile.Preset != "fast" || anime.Profile.MinBitrateKbps != 2500 {
		t.Errorf("/mnt/anime resolved to %s, want crf 20, preset fast and the default bitrate floor",
			showProfile(anime.Profile))
	}
	if !anime.Profile.VmafGate() || !anime.Profile.HardlinkSkip() {
		t.Error("/mnt/anime inherited a neighbour's disabled gate")
	}
}

// TestLoad_ProfileBeatsEnvOverride pins the precedence the environment keeps and the
// precedence it loses. HOLDFAST_CRF still beats the FILE's top-level crf - that is
// unchanged - but the profile layer is INNER to it, so a root that states its own crf
// gets its own, and every root that does not gets the environment's.
func TestLoad_ProfileBeatsEnvOverride(t *testing.T) {
	t.Setenv("HOLDFAST_CRF", "26")
	c := loadYAML(t, `
library_roots:
  - path: /mnt/tv
    crf: 18
  - /mnt/movies
crf: 22
`)
	if got := rootByPath(t, c, "/mnt/tv").Profile.CRF; got != 18 {
		t.Errorf("/mnt/tv crf = %d, want the profile's 18 (the profile is inner to the environment)", got)
	}
	if got := rootByPath(t, c, "/mnt/movies").Profile.CRF; got != 26 {
		t.Errorf("/mnt/movies crf = %d, want the environment's 26 (it still beats the file's 22)", got)
	}
	if c.CRF != 26 {
		t.Errorf("top-level crf = %d, want the environment's 26", c.CRF)
	}
	if got := rootByPath(t, c, "/mnt/movies").LayerOf("crf"); got != LayerTopLevel {
		t.Errorf("an environment override reports the %s layer, want %s", got, LayerTopLevel)
	}
}

// TestProfileKnobSetIsClosedAndSingleSourced proves the set is enumerated ONCE and that
// everything which has to agree with it does.
//
// The failure this closes is silent in both directions. A knob added to Profile but not
// to profileKnobs is a field the resolver never seeds and never lets an entry set - it
// would read as the Go zero for every root. A knob added to profileKnobs but not to
// Profile is a key an entry may write that nothing ever reads. Neither shows up as an
// error anywhere; both show up here.
func TestProfileKnobSetIsClosedAndSingleSourced(t *testing.T) {
	knobs := ProfileKnobs()

	// The set is exactly what the criterion names, in one place.
	want := []string{
		"encoder", "crf", "preset", "pixel_format", "container_ext",
		"min_bitrate_kbps", "min_savings_percent", "skip_hardlinked",
		"vmaf_enable", "min_vmaf", "vmaf_min_pool", "vmaf_min_chroma",
		"vmaf_subsample", "vmaf_model",
	}
	if !reflect.DeepEqual(knobs, want) {
		t.Fatalf("ProfileKnobs() = %v, want %v", knobs, want)
	}

	// ... and it is exactly the `yaml` tags on Profile, which is what the RESOLVER
	// decodes into. This is the half that catches a field added without a list entry.
	tags := yamlTags(reflect.TypeOf(Profile{}))
	if !reflect.DeepEqual(tags, knobs) {
		t.Errorf("Profile's yaml tags are %v, want exactly the knob set %v - the resolver decodes into "+
			"this struct, so a field the list does not name is a knob nothing seeds", tags, knobs)
	}

	// Every knob is a real TOP-LEVEL key, or there would be nothing for a profile to
	// inherit from when the entry is silent.
	for _, knob := range knobs {
		if !knownKeys[knob] {
			t.Errorf("knob %q is not a top-level config key, so a root that does not set it has nothing "+
				"to inherit", knob)
		}
		if _, ok := defaultLayer()[knob]; !ok {
			t.Errorf("knob %q has no built-in default, so the innermost of the three layers is missing "+
				"its floor", knob)
		}
	}

	// The ACCEPTANCE check reads the same list: it takes every knob and refuses every
	// daemon-level key the criterion names.
	for _, knob := range knobs {
		if !isProfileKnob(knob) {
			t.Errorf("isProfileKnob(%q) = false for a knob in the set", knob)
		}
	}
	for _, daemon := range []string{
		"workers", "state_dir", "server_addr", "server_auth_token", "undo_window_hours",
		"history_retention_rows", "allow_non_local", "run_window", "max_load",
		"scan_interval_sec", "notify_url", "tautulli_url", "tautulli_api_key",
		"library_roots", "log_level", "dry_run", "video_exts", "metrics_enable",
		"duration_tolerance_sec", "max_failures",
	} {
		if isProfileKnob(daemon) {
			t.Errorf("isProfileKnob(%q) = true - it describes the process, not a library", daemon)
		}
	}
	// exclude_paths / include_paths are NOT overridable here: neither is a top-level key
	// of this build, so there is no global value for a profile to override.
	for _, absent := range []string{"exclude_paths", "include_paths"} {
		if isProfileKnob(absent) || knownKeys[absent] {
			t.Errorf("%q is treated as a knob, but this build has no such top-level key", absent)
		}
	}

	// TopLevelProfile copies every knob from the identically-tagged Config field. Proved
	// by giving each Config field a distinct value and reading the profile back, so a
	// knob forgotten in that function is a root silently inheriting a zero, here.
	var c Config
	cv := reflect.ValueOf(&c).Elem()
	for i, knob := range knobs {
		f := cv.FieldByName(configFieldFor(t, knob))
		setDistinct(t, f, i+1)
	}
	p := c.TopLevelProfile()
	pv := reflect.ValueOf(p)
	pt := reflect.TypeOf(p)
	for i := 0; i < pt.NumField(); i++ {
		knob := yamlTag(pt.Field(i))
		got := show(pv.Field(i))
		wantVal := show(cv.FieldByName(configFieldFor(t, knob)))
		if got != wantVal {
			t.Errorf("TopLevelProfile() knob %s = %s, want the Config field's %s", knob, got, wantVal)
		}
	}
}

// configFieldFor is the Config field carrying the same yaml tag as knob.
func configFieldFor(t *testing.T, knob string) string {
	t.Helper()
	ct := reflect.TypeOf(Config{})
	for i := 0; i < ct.NumField(); i++ {
		if yamlTag(ct.Field(i)) == knob {
			return ct.Field(i).Name
		}
	}
	t.Fatalf("no Config field carries the yaml tag %q", knob)
	return ""
}

func yamlTag(f reflect.StructField) string {
	tag := f.Tag.Get("yaml")
	if i := strings.IndexByte(tag, ','); i >= 0 {
		tag = tag[:i]
	}
	return tag
}

func yamlTags(t reflect.Type) []string {
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		out = append(out, yamlTag(t.Field(i)))
	}
	return out
}

// setDistinct gives f a value nothing else in the struct has, so a field copied from the
// wrong source is visible rather than coincidentally equal.
func setDistinct(t *testing.T, f reflect.Value, n int) {
	t.Helper()
	switch f.Kind() {
	case reflect.String:
		f.SetString(fmt.Sprintf("v%d", n))
	case reflect.Int:
		f.SetInt(int64(n))
	case reflect.Float64:
		f.SetFloat(float64(n) + 0.5)
	case reflect.Ptr:
		b := n%2 == 0
		f.Set(reflect.ValueOf(&b))
	default:
		t.Fatalf("setDistinct: unhandled kind %s", f.Kind())
	}
}

func show(v reflect.Value) string {
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return "nil"
		}
		return fmt.Sprintf("%v", v.Elem().Interface())
	}
	return fmt.Sprintf("%v", v.Interface())
}

// TestLoad_RefusesAnUnknownProfileKey: a key inside an entry that is neither `path` nor
// a member of the closed set is a startup refusal naming it, exactly as an unknown
// top-level key is. It must NOT fall back to the top-level value for it - silently
// ignoring `crff: 30` leaves an operator believing a root has a crf it does not.
func TestLoad_RefusesAnUnknownProfileKey(t *testing.T) {
	_, err := load(t, "library_roots:\n  - path: /mnt/tv\n    crff: 30\ncrf: 22\n")
	if err == nil {
		t.Fatal("Load with an unknown key inside an entry = nil, want a refusal")
	}
	for _, want := range []string{"crff", "/mnt/tv"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q; got: %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Errorf("an unknown key should read as a typo, distinct from a daemon-level key; got: %v", err)
	}

	// A missing `path` is named by INDEX, because an entry with no path has no other
	// name an operator could find it by.
	_, err = load(t, "library_roots:\n  - crf: 30\n")
	if err == nil {
		t.Fatal("Load with an entry carrying knobs but no path = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "library_roots[0]") {
		t.Errorf("the refusal must name the entry's index; got: %v", err)
	}

	// Anti-vacuity: the same file with the key spelled correctly LOADS and takes effect,
	// so the refusal above is about the key and not about the shape of the file.
	c := loadYAML(t, "library_roots:\n  - path: /mnt/tv\n    crf: 30\ncrf: 22\n")
	if got := rootByPath(t, c, "/mnt/tv").Profile.CRF; got != 30 {
		t.Errorf("the correctly spelled key resolved to crf %d, want 30", got)
	}
}

// TestValidate_RefusesAProfileKnobThatIsDaemonLevel: a key that describes the PROCESS is
// refused inside an entry, and the message says which it is. `workers: 4` in a
// library_roots entry is not a spelling mistake - it is a misunderstanding of what a
// profile is - and a refusal that suggested a typo would send an operator hunting for a
// mistake that is not there.
func TestValidate_RefusesAProfileKnobThatIsDaemonLevel(t *testing.T) {
	daemon := []string{
		"workers", "state_dir", "server_addr", "server_auth_token", "undo_window_hours",
		"history_retention_rows", "run_window", "max_load", "scan_interval_sec",
		"notify_url", "tautulli_url", "tautulli_api_key",
	}
	for _, key := range daemon {
		t.Run(key, func(t *testing.T) {
			_, err := load(t, fmt.Sprintf("library_roots:\n  - path: /mnt/tv\n    %s: 4\n", key))
			if err == nil {
				t.Fatalf("Load with %s inside an entry = nil, want a refusal", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("the refusal must name %q; got: %v", key, err)
			}
			if !strings.Contains(err.Error(), "describes the process, not a library") {
				t.Errorf("the refusal must say the key describes the PROCESS rather than a library; got: %v", err)
			}
			if strings.Contains(err.Error(), "typo") {
				t.Errorf("a daemon-level key must not be reported as a typo; got: %v", err)
			}
		})
	}
	// The rule is DERIVED FROM knownKeys, not from the frozen list above, and that is what
	// makes it survive a top-level key this spec never saw. Every accepted top-level key
	// that is not a profile knob is refused inside an entry, so a later change that adds
	// one (preserve_mtime is the one that landed while this was in flight) is covered the
	// moment it is added rather than when somebody remembers to extend a list here.
	for key := range knownKeys {
		if isProfileKnob(key) || key == "library_roots" {
			continue
		}
		t.Run("derived/"+key, func(t *testing.T) {
			_, err := load(t, fmt.Sprintf("library_roots:\n  - path: /mnt/tv\n    %s: 4\n", key))
			if err == nil {
				t.Fatalf("Load with %s inside an entry = nil: a top-level key that is not an overridable "+
					"knob must be refused there, or an operator believes a root has a setting it does not", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("the refusal must name %q; got: %v", key, err)
			}
		})
	}

	// Anti-vacuity: the same key at the TOP level is perfectly legal.
	c := loadYAML(t, "library_roots:\n  - /mnt/tv\nworkers: 4\n")
	if c.Workers != 4 {
		t.Errorf("top-level workers = %d, want 4", c.Workers)
	}
	// ... including the one that landed alongside this change, which is the case the
	// derived loop above is really about.
	c = loadYAML(t, "library_roots:\n  - /mnt/tv\npreserve_mtime: false\n")
	if c.PreserveMtimeEnabled() {
		t.Error("top-level preserve_mtime: false did not take effect, so the loop above is refusing a " +
			"key this build does not actually accept anywhere")
	}
}

// TestLoad_RefusesAMalformedRootEntry covers every shape an entry may not be. Each
// refusal names the entry's INDEX, which is the only handle an operator has on an entry
// whose path is unusable or absent.
func TestLoad_RefusesAMalformedRootEntry(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{"a number", "library_roots:\n  - 42\n", "library_roots[0]"},
		{"a boolean", "library_roots:\n  - true\n", "library_roots[0]"},
		{"a list", "library_roots:\n  - [/mnt/tv]\n", "library_roots[0]"},
		{"a mapping with no path", "library_roots:\n  - crf: 20\n", "library_roots[0]"},
		{"an empty path", "library_roots:\n  - path: \"\"\n    crf: 20\n", "library_roots[0]"},
		{"a non-string path", "library_roots:\n  - path: 42\n", "library_roots[0]"},
		{"a relative path", "library_roots:\n  - path: media/tv\n", "library_roots[0]"},
		{"a relative string root", "library_roots:\n  - media/tv\n", "library_roots[0]"},
		{"a knob with no value", "library_roots:\n  - path: /mnt/tv\n    crf:\n", "crf"},
		{"the second entry", "library_roots:\n  - /mnt/tv\n  - 42\n", "library_roots[1]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, tc.yaml)
			if err == nil {
				t.Fatalf("Load(%q) = nil, want a refusal", tc.yaml)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal must name %q; got: %v", tc.want, err)
			}
		})
	}
}

// TestValidate_RefusesNestedRoots: a root that is a path-boundary prefix of another is
// refused, in either declaration order and in both spellings of an entry - because each
// root carries its own gates, and a file under two roots has two answers to what may be
// done to it.
func TestValidate_RefusesNestedRoots(t *testing.T) {
	cases := []struct {
		name, yaml string
	}{
		{"outer first, strings", "library_roots:\n  - /mnt/media\n  - /mnt/media/tv\n"},
		{"inner first, strings", "library_roots:\n  - /mnt/media/tv\n  - /mnt/media\n"},
		{"mappings", "library_roots:\n  - path: /mnt/media\n    crf: 20\n  - path: /mnt/media/tv\n    crf: 24\n"},
		{"mixed spellings", "library_roots:\n  - /mnt/media\n  - path: /mnt/media/tv\n    crf: 24\n"},
		{"uncleaned spelling", "library_roots:\n  - /mnt/media/\n  - /mnt/media/tv/../tv\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := loadYAML(t, tc.yaml)
			err := c.Validate()
			if err == nil {
				t.Fatal("Validate with nested roots = nil, want a refusal")
			}
			for _, want := range []string{"/mnt/media", "/mnt/media/tv", "nested"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal must name %q; got: %v", want, err)
				}
			}
		})
	}

	// NOT nested: a path-boundary comparison, so /mnt/mediatv is a sibling of /mnt/media
	// and not a child of it. A lexical prefix test without the boundary would refuse this.
	c := loadYAML(t, "library_roots:\n  - /mnt/media\n  - /mnt/mediatv\n")
	if err := c.Validate(); err != nil {
		t.Errorf("Validate(/mnt/media + /mnt/mediatv) = %v, want nil - a prefix is not a parent", err)
	}

	// A symlink that resolves INTO another root is the same overlap wearing another
	// name, and is refused too.
	dir := t.TempDir()
	outer := filepath.Join(dir, "media")
	inner := filepath.Join(outer, "tv")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "tv-link")
	if err := os.Symlink(inner, link); err != nil {
		t.Skipf("cannot symlink: %v", err)
	}
	linked := Config{LibraryRoots: []string{outer, link}}
	err := linked.Validate()
	if err == nil || !strings.Contains(err.Error(), "nested") {
		t.Errorf("Validate() with a root symlinked into another = %v, want a nesting refusal", err)
	}
}

// TestValidate_RefusesAnEmptyRootList: nothing to scan is a refusal, never a scan of
// nothing. Absent, an empty list, and a list whose every entry was dropped all reach the
// same existing refusal.
func TestValidate_RefusesAnEmptyRootList(t *testing.T) {
	for _, tc := range []struct{ name, yaml string }{
		{"absent", "crf: 20\n"},
		{"an empty list", "library_roots: []\n"},
		{"a null value", "library_roots:\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := loadYAML(t, tc.yaml)
			if n := len(c.RootProfiles()); n != 0 {
				t.Fatalf("resolved %d roots %v, want none", n, rootPaths(c))
			}
			err := c.Validate()
			if err == nil {
				t.Fatal("Validate with no roots = nil, want the nothing-to-scan refusal")
			}
			if !strings.Contains(err.Error(), "refusing to run with nothing to scan") {
				t.Errorf("want the existing nothing-to-scan refusal; got: %v", err)
			}
		})
	}
}

// TestValidate_AppliesEveryValueCheckPerProfile: every per-value refusal the top level
// has, run again against each root's RESOLVED profile and naming that root. The value a
// file is decided by is the root's, not the one at the top of the file, so a profile
// that carries a crf of 99 has to be refused even when the top level is fine.
func TestValidate_AppliesEveryValueCheckPerProfile(t *testing.T) {
	cases := []struct {
		name, knobs, want string
	}{
		{"unknown encoder", "encoder: quicksync_turbo", "not supported"},
		{"crf too high", "crf: 99", "crf 99 out of range (0-51)"},
		{"crf negative", "crf: -1", "crf -1 out of range (0-51)"},
		{"min_savings_percent out of range", "min_savings_percent: 100", "min_savings_percent 100 out of range (0-99)"},
		{"min_vmaf out of range", "min_vmaf: 140", "min_vmaf 140 out of range (0-100)"},
		{"vmaf_min_pool out of range", "vmaf_min_pool: 150", "vmaf_min_pool 150 out of range (0-100)"},
		{"vmaf_min_chroma out of range", "vmaf_min_chroma: 140", "vmaf_min_chroma 140 out of range"},
		{"negative vmaf_subsample", "vmaf_subsample: -1", "vmaf_subsample -1 must be >= 0"},
		{"container_ext with a dot", "container_ext: .mkv", "container_ext \".mkv\" must be a bare extension"},
		{"container_ext with a slash", "container_ext: mkv/x", "container_ext \"mkv/x\" must be a bare extension"},
		{
			"an enabled gate with every floor at zero",
			"vmaf_enable: true\n    min_vmaf: 0\n    vmaf_min_pool: 0\n    vmaf_min_chroma: 0",
			"the VMAF gate would never reject",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The top level stays valid throughout: only the SECOND root's profile is bad,
			// so a refusal proves the per-profile pass ran and not the top-level one.
			c := loadYAML(t, "library_roots:\n  - /mnt/tv\n  - path: /mnt/movies\n    "+tc.knobs+"\n")
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate with a profile carrying %q = nil, want a refusal", tc.knobs)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want the same refusal the top level gives (%q); got: %v", tc.want, err)
			}
			if !strings.Contains(err.Error(), "/mnt/movies") {
				t.Errorf("the refusal must name the root whose profile produced it; got: %v", err)
			}
			if strings.Contains(err.Error(), "/mnt/tv") {
				t.Errorf("the refusal names a root whose profile is fine; got: %v", err)
			}
		})
	}
}

// TestWarnings_NameTheRootThatWeakenedTheGate: a weakened gate is announced once per
// affected root, with the root named, and never for a root that did not weaken it.
// One process may run over a film library with every floor in place and a grainy-anime
// library with the worst-frame floor off; an unattributed warning would leave an
// operator unable to tell which library lost it.
func TestWarnings_NameTheRootThatWeakenedTheGate(t *testing.T) {
	c := loadYAML(t, `
library_roots:
  - /mnt/tv
  - path: /mnt/anime
    vmaf_min_pool: 0
`)
	w := c.Warnings()
	if len(w) != 1 {
		t.Fatalf("Warnings() = %d warnings %v, want exactly 1 (only /mnt/anime weakened a gate)", len(w), w)
	}
	if !strings.Contains(w[0], "/mnt/anime") {
		t.Errorf("the warning must name the root that weakened the gate; got: %s", w[0])
	}
	if strings.Contains(w[0], "/mnt/tv") {
		t.Errorf("the warning names a root whose gates are intact; got: %s", w[0])
	}
	if !strings.Contains(w[0], "worst-frame floor is DISABLED") {
		t.Errorf("the warning must still say what was weakened; got: %s", w[0])
	}

	// Once per affected root: two roots weakening the same gate warn twice, each named.
	two := loadYAML(t, `
library_roots:
  - path: /mnt/anime
    vmaf_min_pool: 0
  - path: /mnt/grain
    vmaf_min_pool: 0
`)
	w = two.Warnings()
	if len(w) != 2 {
		t.Fatalf("Warnings() = %d warnings %v, want 2 - once per affected root", len(w), w)
	}
	if !strings.Contains(strings.Join(w, " "), "/mnt/anime") || !strings.Contains(strings.Join(w, " "), "/mnt/grain") {
		t.Errorf("both affected roots must be named; got: %v", w)
	}

	// A root that disables the gate entirely gets the loudest warning and only that one,
	// while its neighbour with intact gates gets none.
	off := loadYAML(t, `
library_roots:
  - /mnt/tv
  - path: /mnt/scratch
    vmaf_enable: false
    vmaf_min_pool: 0
    vmaf_subsample: 10
`)
	w = off.Warnings()
	if len(w) != 1 || !strings.Contains(w[0], "/mnt/scratch") || !strings.Contains(w[0], "there is NO perceptual gate") {
		t.Fatalf("Warnings() = %v, want one warning naming /mnt/scratch and no perceptual gate", w)
	}

	// The shipped defaults stay quiet, per root as they were globally: a default that
	// warns is how an operator learns to skip warnings.
	quiet := loadYAML(t, "library_roots:\n  - /mnt/tv\n  - path: /mnt/movies\n    crf: 18\n")
	if w := quiet.Warnings(); len(w) != 0 {
		t.Errorf("the shipped defaults warn per root: %v", w)
	}
}

// TestProfileDigest_DiffersIffAnyResolvedKnobDiffers: the digest is what keeps a
// terminal row interpretable after the profile that decided it was edited. Identical
// resolved values, however they were spelled, digest the same; any different resolved
// value digests differently.
func TestProfileDigest_DiffersIffAnyResolvedKnobDiffers(t *testing.T) {
	base := loadYAML(t, `
library_roots:
  - /mnt/tv
  - path: /mnt/movies
crf: 20
preset: slow
`)
	a := rootByPath(t, base, "/mnt/tv").Profile
	b := rootByPath(t, base, "/mnt/movies").Profile
	if a.Digest() != b.Digest() {
		t.Errorf("two roots with identical resolved values digest differently: %s vs %s", a.Digest(), b.Digest())
	}

	// Spelled differently, resolved identically: same digest. The top-level crf and a
	// profile crf of the same value are the same profile.
	spelled := loadYAML(t, "library_roots:\n  - path: /mnt/tv\n    crf: 20\n    preset: slow\n")
	if got := rootByPath(t, spelled, "/mnt/tv").Profile.Digest(); got != a.Digest() {
		t.Errorf("the same resolved profile spelled two ways digests %s and %s", got, a.Digest())
	}

	// A tri-state pointer that is absent and one that is explicitly at its default are
	// the same behaviour, so they must be the same digest.
	yes := true
	explicit := a
	explicit.SkipHardlinked = &yes
	if explicit.Digest() != a.Digest() {
		t.Errorf("an explicit skip_hardlinked: true digests differently from an absent one (%s vs %s)",
			explicit.Digest(), a.Digest())
	}

	// EVERY knob, one at a time: a different resolved value is a different digest. This
	// is the half that catches a digest computed over a subset of the knobs.
	for i, knob := range ProfileKnobs() {
		t.Run(knob, func(t *testing.T) {
			moved := a
			mv := reflect.ValueOf(&moved).Elem().Field(i)
			if got := yamlTag(reflect.TypeOf(moved).Field(i)); got != knob {
				t.Fatalf("field %d carries tag %q, want %q", i, got, knob)
			}
			bump(t, mv)
			if moved.Digest() == a.Digest() {
				t.Errorf("moving %s did not change the digest (%s): a row decided under it would be "+
					"indistinguishable from one decided under the original", knob, a.Digest())
			}
		})
	}
}

// bump moves a knob to a different value of the same type, so "different resolved value"
// is exercised for every field rather than for the ones a hand-written table remembered.
func bump(t *testing.T, f reflect.Value) {
	t.Helper()
	switch f.Kind() {
	case reflect.String:
		f.SetString(f.String() + "-moved")
	case reflect.Int:
		f.SetInt(f.Int() + 1)
	case reflect.Float64:
		f.SetFloat(f.Float() + 1)
	case reflect.Ptr:
		// The tri-states are read RESOLVED, so the move has to be one that changes what
		// the gate does: flip the effective value.
		cur := f.IsNil() || f.Elem().Bool()
		next := !cur
		f.Set(reflect.ValueOf(&next))
	default:
		t.Fatalf("bump: unhandled kind %s", f.Kind())
	}
}

// sameProfile compares two profiles by their RESOLVED values, which is what decides a
// file - so an absent tri-state and an explicit one at the same effective value are the
// same profile, exactly as the digest treats them.
func sameProfile(a, b Profile) bool {
	av, bv := a.values(), b.values()
	for i := range av {
		if av[i] != bv[i] {
			return false
		}
	}
	return true
}

func showProfile(p Profile) string {
	parts := make([]string, 0, len(profileKnobs))
	for i, knob := range profileKnobs {
		parts = append(parts, knob+"="+p.values()[i])
	}
	return "{" + strings.Join(parts, " ") + "}"
}
