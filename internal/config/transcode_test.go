package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The settings half of S0079, graded from the configuration's own door: what Load
// refuses, what Validate refuses, what Notices says, and what a job's settings
// resolve to.

// writeCfg writes a config file whose library_roots points at a directory that
// really exists, so a Validate failure is never about the root.
func writeCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "media")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "config.yaml")
	full := "library_roots:\n  - " + root + "\n" + body
	if err := os.WriteFile(p, []byte(full), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// loadAndValidate is the exact pair cmd/holdfast's loadConfig runs, so a refusal
// here is a refusal the process exits non-zero on.
func loadAndValidate(t *testing.T, body string) (*Config, error) {
	t.Helper()
	cfg, err := Load(writeCfg(t, body))
	if err != nil {
		return nil, err
	}
	return cfg, cfg.Validate()
}

// AC-A3: bitrate_kbps that is negative, or is not a whole number of kbps, fails
// loading and names the key and the offending value.
//
// The whole-number half is checked against the RAW layered value rather than the
// decoded struct, because the decoder is weakly typed: it turns 8000.5 into 8000,
// "8000abc" into an error but "8000" into 8000, and true into 1 - three bitrates the
// operator did not write, on the knob that decides what the encoder aims at.
func TestBitrateKbps_RefusesAnythingThatIsNotAWholeNumberOfKbps(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"a fraction", "bitrate_kbps: 8000.5\n"},
		{"a word", "bitrate_kbps: fast\n"},
		{"a boolean", "bitrate_kbps: true\n"},
		{"a list", "bitrate_kbps: [8000]\n"},
		{"the key with no value", "bitrate_kbps:\n"},
		{"negative", "bitrate_kbps: -1\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadAndValidate(t, tc.body)
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), "bitrate_kbps") {
				t.Fatalf("the refusal does not name the key: %v", err)
			}
		})
	}

	// The control arm, without which the six above prove only that something failed:
	// a genuine whole number is ACCEPTED, in both spellings a layered load can
	// produce (a YAML integer, and the string an environment override arrives as).
	t.Run("a whole number is accepted", func(t *testing.T) {
		cfg, err := loadAndValidate(t, "bitrate_kbps: 8000\n")
		if err != nil {
			t.Fatalf("a whole number of kbps was refused: %v", err)
		}
		if cfg.BitrateKbps != 8000 {
			t.Fatalf("bitrate_kbps = %d, want 8000", cfg.BitrateKbps)
		}
	})
	t.Run("an environment override is accepted", func(t *testing.T) {
		t.Setenv("HOLDFAST_BITRATE_KBPS", "6000")
		cfg, err := loadAndValidate(t, "")
		if err != nil {
			t.Fatalf("an env override of a whole number was refused: %v", err)
		}
		if cfg.BitrateKbps != 6000 {
			t.Fatalf("bitrate_kbps = %d, want 6000", cfg.BitrateKbps)
		}
	})
}

// AC-A4: a positive bitrate_kbps announces, in the NOTICE list `validate` prints and
// `run`/`serve` log, that the quality target is not in use - and it is a notice and
// never a warning, because no safety gate has been weakened.
func TestBitrateKbps_AnnouncesThatTheQualityTargetIsNotInUse_AsANoticeNotAWarning(t *testing.T) {
	says := func(list []string, want string) bool {
		for _, s := range list {
			if strings.Contains(strings.ToLower(s), strings.ToLower(want)) {
				return true
			}
		}
		return false
	}

	t.Run("the top-level setting", func(t *testing.T) {
		cfg, err := loadAndValidate(t, "bitrate_kbps: 8000\n")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if !says(cfg.Notices(), "bitrate_kbps") {
			t.Fatalf("no notice mentions bitrate_kbps: %v", cfg.Notices())
		}
		if !says(cfg.Notices(), "quality target is not in use") {
			t.Fatalf("the notice does not say the quality target is not in use: %v", cfg.Notices())
		}
		for _, w := range cfg.Warnings() {
			if strings.Contains(strings.ToLower(w), "bitrate_kbps") {
				t.Fatalf("bitrate_kbps produced a WARNING, which says a safety gate was weakened: %q", w)
			}
		}
	})

	// A profile is the other way a target bitrate reaches a job, and an announcement
	// that only covered the top-level key would go silent for exactly the
	// configuration that most needs it - one where SOME files run at a bitrate.
	t.Run("a profile that sets it", func(t *testing.T) {
		cfg, err := loadAndValidate(t, "transcode_profiles:\n  - name: bulk\n    match: '**/TV/**'\n    bitrate_kbps: 3000\n")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if !says(cfg.Notices(), "bitrate_kbps") {
			t.Fatalf("a profile's bitrate produced no notice: %v", cfg.Notices())
		}
	})

	// The anti-vacuity arm: with the setting at its default the notice is ABSENT, so
	// the assertions above are about the setting and not about a line that is always
	// printed.
	t.Run("absent by default", func(t *testing.T) {
		cfg, err := loadAndValidate(t, "")
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if says(cfg.Notices(), "bitrate_kbps") {
			t.Fatalf("the default configuration announces a bitrate it is not using: %v", cfg.Notices())
		}
	})
}

// AC-A5: with no transcode_profiles, every job resolves to exactly the top-level
// settings and the recorded profile name is empty.
func TestTranscodeFor_NoProfilesResolvesToTheTopLevelSettingsAndAnEmptyName(t *testing.T) {
	cfg, err := loadAndValidate(t, "encoder: cpu\ncrf: 22\npreset: slow\npixel_format: auto\ncontainer_ext: source\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	top := cfg.TopLevelProfile()
	for _, p := range []string{"/srv/media/a.mkv", "/srv/media/4K/b.mp4", "/anything/else.ts"} {
		got := cfg.TranscodeIn(top, p)
		if got != cfg.BaseTranscode(top) {
			t.Fatalf("%s resolved to %+v, want the inherited settings %+v", p, got, cfg.BaseTranscode(top))
		}
		if got.Profile != "" {
			t.Fatalf("%s recorded profile %q with no profiles configured", p, got.Profile)
		}
	}
}

// AC-A8: every per-profile refusal, each naming the profile and the offending key
// and value. The unknown-key refusal is asserted to bite INSIDE a profile, which is
// the limb the top-level check structurally cannot reach.
func TestTranscodeProfiles_RefuseEveryMalformedProfileByName(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		names []string // substrings the refusal must carry
	}{
		{
			name:  "a key the schema does not define",
			body:  "transcode_profiles:\n  - name: p\n    encodr: svtav1\n",
			names: []string{"encodr", "transcode_profiles[0]"},
		},
		{
			name:  "an unknown encoder",
			body:  "transcode_profiles:\n  - name: p\n    encoder: notacodec\n",
			names: []string{"p", "notacodec"},
		},
		{
			name:  "a crf above the range",
			body:  "transcode_profiles:\n  - name: p\n    crf: 52\n",
			names: []string{"p", "crf", "52"},
		},
		{
			name:  "a crf below the range",
			body:  "transcode_profiles:\n  - name: p\n    crf: -1\n",
			names: []string{"p", "crf"},
		},
		{
			name:  "a container_ext carrying a dot",
			body:  "transcode_profiles:\n  - name: p\n    container_ext: .mkv\n",
			names: []string{"p", "container_ext", ".mkv"},
		},
		{
			name:  "a container_ext carrying a slash",
			body:  "transcode_profiles:\n  - name: p\n    container_ext: a/b\n",
			names: []string{"p", "container_ext"},
		},
		{
			name:  "a negative bitrate",
			body:  "transcode_profiles:\n  - name: p\n    bitrate_kbps: -5\n",
			names: []string{"p", "bitrate_kbps"},
		},
		{
			name:  "a non-whole bitrate",
			body:  "transcode_profiles:\n  - name: p\n    bitrate_kbps: 3000.5\n",
			names: []string{"transcode_profiles[0].bitrate_kbps", "3000.5"},
		},
		{
			name:  "an empty name",
			body:  "transcode_profiles:\n  - name: ''\n    crf: 30\n",
			names: []string{"transcode_profiles[0]", "name"},
		},
		{
			name:  "a duplicate name",
			body:  "transcode_profiles:\n  - name: p\n    crf: 30\n  - name: p\n    crf: 31\n",
			names: []string{"p", "duplicate"},
		},
		{
			name:  "a match pattern that cannot be parsed",
			body:  "transcode_profiles:\n  - name: p\n    match: '**/[unclosed/*.mkv'\n",
			names: []string{"p", "match"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadAndValidate(t, tc.body)
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			for _, want := range tc.names {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
		})
	}

	// The control arm: a well-formed profile carrying every key is ACCEPTED, so the
	// eleven refusals above are about what is wrong with each one and not about the
	// list existing at all.
	t.Run("a well-formed profile is accepted", func(t *testing.T) {
		cfg, err := loadAndValidate(t, "transcode_profiles:\n"+
			"  - name: everything\n"+
			"    match: '**/4K/**'\n"+
			"    encoder: svtav1\n"+
			"    crf: 32\n"+
			"    preset: slow\n"+
			"    pixel_format: auto\n"+
			"    container_ext: mkv\n"+
			"    bitrate_kbps: 4000\n")
		if err != nil {
			t.Fatalf("a well-formed profile was refused: %v", err)
		}
		if len(cfg.TranscodeProfiles) != 1 {
			t.Fatalf("want 1 profile, got %d", len(cfg.TranscodeProfiles))
		}
	})
}

// A profile that overrides a key to its ZERO value must be distinguishable from one
// that does not mention it. Zero is legal and meaningful for both numeric overrides
// (crf 0 is lossless; bitrate_kbps 0 is "use the quality target"), so a schema that
// could not tell the two apart would silently drop half the settings an operator can
// write.
func TestTranscodeProfiles_AnExplicitZeroOverridesAndAnAbsentKeyDoesNot(t *testing.T) {
	cfg, err := loadAndValidate(t, "crf: 22\nbitrate_kbps: 5000\n"+
		"transcode_profiles:\n"+
		"  - name: lossless\n"+
		"    match: 'keep-*.mkv'\n"+
		"    crf: 0\n"+
		"    bitrate_kbps: 0\n"+
		"  - name: silent\n"+
		"    match: 'other-*.mkv'\n")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	top := cfg.TopLevelProfile()
	zero := cfg.TranscodeIn(top, "/srv/media/keep-a.mkv")
	if zero.Profile != "lossless" || zero.CRF != 0 || zero.BitrateKbps != 0 {
		t.Fatalf("an explicit zero did not override: %+v", zero)
	}
	quiet := cfg.TranscodeIn(top, "/srv/media/other-a.mkv")
	if quiet.Profile != "silent" || quiet.CRF != 22 || quiet.BitrateKbps != 5000 {
		t.Fatalf("an absent key did not keep its top-level value: %+v", quiet)
	}
}

// The match grammar, asserted directly so the resolution tests above rest on
// something stated rather than assumed.
func TestMatchSource_TheGrammarIsWhatTheDocumentationSays(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"*.mkv", "/srv/media/tv/ep.mkv", true},              // no slash: the basename
		{"*.mkv", "/srv/media/tv/ep.mp4", false},             //
		{"ep?.mkv", "/srv/media/ep1.mkv", true},              //
		{"**/4K/**", "/srv/media/4K/film.mkv", true},         // ** spans segments
		{"**/4K/**", "/srv/media/a/b/4K/c/film.mkv", true},   //
		{"**/4K/**", "/srv/media/HD/film.mkv", false},        //
		{"/srv/media/*.mkv", "/srv/media/film.mkv", true},    // * does not cross a separator
		{"/srv/media/*.mkv", "/srv/media/a/film.mkv", false}, //
		{"**/TV/**/*.mkv", "/srv/TV/s1/ep.mkv", true},        //
		{"**/TV/**/*.mkv", "/srv/TV/s1/ep.mp4", false},       //
		{"", "/anything/at/all.mkv", true},                   // empty: the catch-all
	}
	for _, tc := range cases {
		got, err := MatchSource(tc.pattern, tc.path)
		if err != nil {
			t.Errorf("MatchSource(%q, %q): %v", tc.pattern, tc.path, err)
			continue
		}
		if got != tc.want {
			t.Errorf("MatchSource(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}

	// A malformed pattern is an ERROR at every path, not a silent non-match for the
	// ones that happen to fall out of the scan early. A pattern that answered "no"
	// for one file and "that is malformed" for another would be a profile that
	// quietly stopped applying to part of a library.
	for _, p := range []string{"/srv/media/film.mkv", "/other/film.mkv", "/srv/x.mkv"} {
		if _, err := MatchSource("/srv/[unclosed/*.mkv", p); err == nil {
			t.Errorf("a malformed pattern matched %s without an error", p)
		} else if !IsBadPattern(err) {
			t.Errorf("a malformed pattern gave %v, want a bad-pattern error", err)
		}
	}
}

// AC-B17: the shipped example carries every setting this item adds, commented with
// its default, and loading and validating it against a real directory succeeds.
func TestConfigExample_CarriesTheNewSettingsAndStillLoads(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("reading the shipped example: %v", err)
	}
	example := string(b)

	// Each key, commented, with the default spelled as today's behaviour.
	for _, want := range []string{
		"# bitrate_kbps: 0",
		"# transcode_profiles:",
		`# scratch_dir: ""`,
		"# scratch_min_free_gb: 50",
	} {
		if !strings.Contains(example, want) {
			t.Errorf("config.example.yaml does not carry %q", want)
		}
	}
	// And the defaults it spells are the defaults this build ships, so the file
	// cannot drift into documenting a behaviour the code no longer has.
	d := defaultLayer()
	if d["bitrate_kbps"] != 0 {
		t.Errorf("the shipped default for bitrate_kbps is %v, not the 0 the example documents", d["bitrate_kbps"])
	}
	if d["scratch_dir"] != "" {
		t.Errorf("the shipped default for scratch_dir is %v, not the \"\" the example documents", d["scratch_dir"])
	}
	if d["scratch_min_free_gb"] != 50 {
		t.Errorf("the shipped default for scratch_min_free_gb is %v, not the 50 the example documents", d["scratch_min_free_gb"])
	}

	// It still loads and validates, with library_roots pointed at a directory that
	// exists. That is the half a text check cannot give: an example that documents
	// the keys and refuses to load is worse than one that documents nothing.
	dir := t.TempDir()
	root := filepath.Join(dir, "media")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	pointed := strings.Replace(example, "library_roots:\n  - /mnt/media", "library_roots:\n  - "+root, 1)
	if pointed == example {
		t.Fatal("the example's library_roots line is not what this test expects to repoint")
	}
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(pointed), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("the shipped example does not load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the shipped example does not validate: %v", err)
	}
}

// The scratch settings' own configuration-level refusals, and the default that has
// to keep meaning "beside the source".
func TestScratchDir_DefaultsToBesideTheSourceAndRefusesWhatItMust(t *testing.T) {
	cfg, err := loadAndValidate(t, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ScratchDir != "" {
		t.Fatalf("scratch_dir defaults to %q, want \"\" (beside the source)", cfg.ScratchDir)
	}
	if cfg.ScratchMinFreeGB != 50 {
		t.Fatalf("scratch_min_free_gb defaults to %d, want 50", cfg.ScratchMinFreeGB)
	}

	for _, tc := range []struct {
		name, body, want string
	}{
		{"a relative scratch_dir", "scratch_dir: cache\n", "scratch_dir"},
		{"the filesystem root", "scratch_dir: /\n", "filesystem root"},
		{"a negative floor", "scratch_min_free_gb: -1\n", "scratch_min_free_gb"},
		{"a non-whole floor", "scratch_min_free_gb: 1.5\n", "scratch_min_free_gb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadAndValidate(t, tc.body)
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal does not name %q: %v", tc.want, err)
			}
		})
	}
}
