package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/encoder"
)

// The per-encoder quality keys, from the configuration's own door: what Load and Validate
// refuse, how the environment spells an entry, and which entry a job's settings carry.

// TestQuality_ValidateRedsAtEachScalesEdges: each hardware encoder's quality.<key> is
// accepted at both edges of its scale and refused one past each, and the refusal names the
// key and the scale. This is what `holdfast validate` runs.
func TestQuality_ValidateRedsAtEachScalesEdges(t *testing.T) {
	for _, key := range encoder.QualityKeys() {
		spec, _ := encoder.Lookup(key)
		q := spec.Quality
		for _, v := range []int{q.Min, q.Max} {
			if _, err := loadAndValidate(t, "quality:\n  "+key+": "+strconv.Itoa(v)+"\n"); err != nil {
				t.Errorf("quality.%s %d is on %s and was refused: %v", key, v, q, err)
			}
		}
		for _, v := range []int{q.Min - 1, q.Max + 1} {
			_, err := loadAndValidate(t, "quality:\n  "+key+": "+strconv.Itoa(v)+"\n")
			if err == nil {
				t.Errorf("quality.%s %d is off %s and was accepted", key, v, q)
				continue
			}
			for _, part := range []string{"quality." + key, q.String()} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("quality.%s %d: refusal %q does not name %q", key, v, err, part)
				}
			}
		}
	}
}

// TestQuality_RefusesKeysThatAreNotAHardwareEncoders: cpu and svtav1 are set by crf, and an
// unknown key or an ffmpeg alias names no scale; each is refused saying why.
func TestQuality_RefusesKeysThatAreNotAHardwareEncoders(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{"quality:\n  cpu: 22\n", "set by crf"},
		{"quality:\n  svtav1: 30\n", "set by crf"},
		{"quality:\n  nvidia: 22\n", "names no encoder"},
		{"quality:\n  hevc_vaapi: 22\n", "write quality.vaapi"},
	} {
		_, err := loadAndValidate(t, c.body)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want a refusal containing %q", c.body, err, c.want)
		}
	}
}

// TestQuality_RefusesAValueThatIsNotAWholeNumber: the decoder is weakly typed and would read
// 24.5 as 24, so the raw value is refused at Load, naming the entry; and a quality that is
// not a mapping at all is refused naming the shape.
func TestQuality_RefusesAValueThatIsNotAWholeNumber(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{"quality:\n  nvenc: 24.5\n", "quality.nvenc must be a whole number"},
		{"quality:\n  qsv: true\n", "quality.qsv must be a whole number"},
		{"quality:\n  vaapi: high\n", "quality.vaapi must be a whole number"},
		{"quality: 24\n", "quality must be a mapping"},
	} {
		_, err := loadAndValidate(t, c.body)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want a refusal containing %q", c.body, err, c.want)
		}
	}
}

// TestQuality_AbsentKeysLeaveTheSettingsAsTheyWere: a configuration with no quality key
// carries no entry for any encoder, so every encoder takes crf as it always did.
func TestQuality_AbsentKeysLeaveTheSettingsAsTheyWere(t *testing.T) {
	for _, key := range encoder.Known() {
		cfg, err := loadAndValidate(t, "encoder: "+key+"\ncrf: 24\n")
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		ts := cfg.TranscodeIn(cfg.TopLevelProfile(), "/srv/media/film.mkv")
		if ts.EncoderQualitySet || ts.EncoderQuality != 0 || ts.CRF != 24 {
			t.Errorf("%s: settings %+v carry a quality nobody wrote", key, ts)
		}
	}
}

// TestQuality_TheJobCarriesItsOwnEncodersEntry: a job carries the entry for the encoder IT
// resolved to - through an encode profile that switches encoder, and through an ffmpeg alias -
// and no other encoder's.
func TestQuality_TheJobCarriesItsOwnEncodersEntry(t *testing.T) {
	cfg, err := loadAndValidate(t, "encoder: qsv\nquality:\n  nvenc: 24\n  qsv: 30\n  vaapi: 40\n"+
		"encode_profiles:\n  - name: gpu\n    match: \"**/Films/**\"\n    encoder: hevc_nvenc\n"+
		"  - name: cpu\n    match: \"**/TV/**\"\n    encoder: cpu\n")
	if err != nil {
		t.Fatal(err)
	}
	top := cfg.TopLevelProfile()
	for _, c := range []struct {
		path string
		set  bool
		want int
	}{
		{"/srv/Films/a.mkv", true, 24},
		{"/srv/TV/b.mkv", false, 0},
		{"/srv/Other/c.mkv", true, 30},
	} {
		ts := cfg.TranscodeIn(top, c.path)
		if ts.EncoderQualitySet != c.set || ts.EncoderQuality != c.want {
			t.Errorf("%s (encoder %s): quality %d set=%v, want %d set=%v",
				c.path, ts.Encoder, ts.EncoderQuality, ts.EncoderQualitySet, c.want, c.set)
		}
	}
	if b := cfg.BaseTranscode(top); !b.EncoderQualitySet || b.EncoderQuality != 30 {
		t.Errorf("BaseTranscode carries %d set=%v, want qsv's 30", b.EncoderQuality, b.EncoderQualitySet)
	}
}

// TestQuality_TheEnvironmentSetsOneEntry: HOLDFAST_QUALITY_<KEY> sets quality.<key> - a
// registry key's own underscore included - over the file's value, and is validated exactly
// as the file's is.
func TestQuality_TheEnvironmentSetsOneEntry(t *testing.T) {
	t.Setenv("HOLDFAST_QUALITY_AV1_NVENC", "40")
	t.Setenv("HOLDFAST_QUALITY_NVENC", "26")
	cfg, err := loadAndValidate(t, "quality:\n  nvenc: 24\n  qsv: 30\n")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"av1_nvenc": 40, "nvenc": 26, "qsv": 30}
	if len(cfg.Quality) != len(want) {
		t.Errorf("quality = %v, want %v", cfg.Quality, want)
	}
	for k, v := range want {
		if cfg.Quality[k] != v {
			t.Errorf("quality.%s = %d, want %d (all: %v)", k, cfg.Quality[k], v, cfg.Quality)
		}
	}

	t.Setenv("HOLDFAST_QUALITY_VAAPI", "0")
	if _, err := loadAndValidate(t, ""); err == nil || !strings.Contains(err.Error(), "quality.vaapi") {
		t.Errorf("HOLDFAST_QUALITY_VAAPI=0 is off the scale: got %v", err)
	}
	t.Setenv("HOLDFAST_QUALITY_VAAPI", "26.5")
	if _, err := loadAndValidate(t, ""); err == nil || !strings.Contains(err.Error(), "quality.vaapi must be a whole number") {
		t.Errorf("HOLDFAST_QUALITY_VAAPI=26.5 is not whole: got %v", err)
	}
	t.Setenv("HOLDFAST_QUALITY_VAAPI", "26")
	t.Setenv("HOLDFAST_QUALITY_CPU", "22")
	if _, err := loadAndValidate(t, ""); err == nil || !strings.Contains(err.Error(), "set by crf") {
		t.Errorf("HOLDFAST_QUALITY_CPU is not a key: got %v", err)
	}
}

// TestEnvKey spells each variable's key.
func TestEnvKey(t *testing.T) {
	for in, want := range map[string]string{
		"HOLDFAST_CRF":               "crf",
		"HOLDFAST_QUALITY_NVENC":     "quality.nvenc",
		"HOLDFAST_QUALITY_AV1_NVENC": "quality.av1_nvenc",
		"HOLDFAST_QUALITY_":          "quality_",
		"HOLDFAST_QUALITY":           "quality",
		"HOLDFAST_X265_CPUS":         "x265_cpus",
	} {
		if got := envKey(in); got != want {
			t.Errorf("envKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestQuality_IsTopLevelOnly: a library root or an encode profile carrying quality is refused
// as the unknown key it is there, rather than silently ignored.
func TestQuality_IsTopLevelOnly(t *testing.T) {
	_, err := loadAndValidate(t, "encode_profiles:\n  - name: gpu\n    match: \"*.mkv\"\n    quality:\n      nvenc: 24\n")
	if err == nil || !strings.Contains(err.Error(), "quality") {
		t.Errorf("an encode profile carrying quality: got %v, want a refusal naming it", err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - path: " + dir + "\n    quality:\n      nvenc: 24\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Load(p)
	if err == nil || !strings.Contains(err.Error(), "quality") {
		t.Errorf("a library root carrying quality: got %v, want a refusal naming it", err)
	}
}
