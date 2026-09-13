package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F11 (S0079 impl gate). AC-A8: "IF a profile carries a key the schema does not
// define, AN UNKNOWN ENCODER, [...] THEN loading the config SHALL fail naming the
// profile and the offending key and value".
//
// An encode profile whose `encoder` is the empty string carries an encoder this build
// does not ship: it is absent from encoder.Known(), encoder.Lookup rejects it, and the
// capability preflight this branch adds names it `unknown encoder ""`. Loading it
// nevertheless succeeds and Validate passes, so `holdfast validate` reports `config OK`
// for a configuration `holdfast run` refuses to start on.
//
// The empty string cannot mean "inherit" here: every override on an EncodeProfile is a
// POINTER so that "not mentioned" is spelled nil, and TranscodeIn copies an explicit ""
// into the job's effective settings.
//
// The CONTROL arm makes this an experiment rather than an assertion: the same profile
// carrying `encoder: nope` IS refused, naming the profile and the value.
func TestRegressS0079F11_AnEncodeProfileWithAnEmptyEncoderIsRefusedAtLoad(t *testing.T) {
	write := func(t *testing.T, encoderLine string) string {
		t.Helper()
		d := t.TempDir()
		root := filepath.Join(d, "media")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(d, "config.yaml")
		body := "library_roots:\n  - " + root + "\n" +
			"encode_profiles:\n  - name: fourk\n    match: \"**/4K/**\"\n    " + encoderLine + "\n"
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("control: a word this build does not ship is refused", func(t *testing.T) {
		c, err := Load(write(t, "encoder: nope"))
		if err != nil {
			return
		}
		verr := c.Validate()
		if verr == nil {
			t.Fatalf("CONTROL BROKEN: encoder: nope was accepted, so this test measures nothing")
		}
		if !strings.Contains(verr.Error(), "fourk") || !strings.Contains(verr.Error(), "nope") {
			t.Fatalf("CONTROL BROKEN: the refusal %q does not name the profile and the value", verr)
		}
	})

	t.Run("an explicit empty encoder is accepted, and reaches the job's settings", func(t *testing.T) {
		c, err := Load(write(t, `encoder: ""`))
		if err != nil {
			return
		}
		if verr := c.Validate(); verr != nil {
			return
		}
		// Prove the value is LIVE and not merely tolerated, so the failure cannot be
		// read as a harmless unused field.
		if len(c.EncodeProfiles) != 1 || c.EncodeProfiles[0].Encoder == nil {
			t.Fatalf("the fixture did not decode into one profile carrying an explicit encoder: %#v", c.EncodeProfiles)
		}
		ts := c.TranscodeIn(c.TopLevelProfile(), "/srv/media/4K/film.mkv")
		if ts.Profile != "fourk" {
			t.Fatalf("the fixture's profile did not match the source (profile %q)", ts.Profile)
		}
		t.Errorf("AC-A8: an encode profile carrying an unknown encoder (the empty string) loaded and validated "+
			"without a refusal. The job resolves to encoder=%q (top level %q), and the capability preflight is "+
			"handed %#v, where encoder.Lookup refuses it as `unknown encoder \"\"`. AC-A8 requires loading to "+
			"FAIL, naming the profile and the offending key and value; instead `holdfast validate` exits 0 with "+
			"`config OK` for a configuration `holdfast run` refuses to start on.",
			ts.Encoder, c.Encoder, c.EncodeProfileEncoders())
	})
}
