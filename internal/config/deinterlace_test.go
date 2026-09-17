package config

import (
	"strings"
	"testing"
)

// The `deinterlace` key: what it says at start, and what it refuses.
//
// It is the one knob in this configuration that changes what the replacement IS rather
// than which replacement is accepted, so both halves below are about the same thing - an
// operator learning, before the first file goes, exactly what this key does to a library
// they cannot get back.

// TestNotices_DeinterlaceEnabledSaysNotSameContent grades [AC-2] of
// S0107-holdfast-interlacing-decision: a configuration that enables `deinterlace` on any
// profile earns a startup notice stating that the replacement is no longer the same
// content as the source.
//
// It is a NOTICE rather than a warning because no gate is weakened - every floor still
// applies at its configured strictness - and this file's own rule is that a warning is
// always a weakened gate. What the notice carries is the thing no gate can say: the
// replacement is a CONVERTED file, the original fields are not recoverable from it, and
// the swap deletes the original.
//
// The default arm is not decoration: the notice must not fire for the shipped default, or
// it trains the reader to skip the list that also carries the undo window.
func TestNotices_DeinterlaceEnabledSaysNotSameContent(t *testing.T) {
	t.Run("a root that deinterlaces says so, naming the root", func(t *testing.T) {
		c := loadYAML(t, `
library_roots:
  - /mnt/tv
  - path: /mnt/broadcast
    deinterlace: yadif
`)
		notice := findNotice(t, c, deinterlaceKey)
		for _, want := range []string{
			"NO LONGER THE SAME CONTENT AS THE SOURCE",
			"/mnt/broadcast",
			"yadif",
		} {
			if !strings.Contains(notice, want) {
				t.Errorf("the deinterlace notice does not carry %q, so it does not state what the key "+
					"does to a library:\n%s", want, notice)
			}
		}
		// One root asked for it and one did not, so exactly one notice may name it: an
		// operator running over both libraries has to be able to tell which one converts.
		if n := countNotices(c, deinterlaceKey); n != 1 {
			t.Errorf("%d notices name %s, want exactly 1 - only /mnt/broadcast asked for a deinterlace",
				n, deinterlaceKey)
		}
		if strings.Contains(notice, "/mnt/tv") {
			t.Errorf("the notice names /mnt/tv, which deinterlaces nothing:\n%s", notice)
		}
	})

	t.Run("the shipped default says nothing", func(t *testing.T) {
		c := loadYAML(t, "library_roots:\n  - /mnt/tv\n")
		if n := countNotices(c, deinterlaceKey); n != 0 {
			t.Errorf("the shipped default earns %d deinterlace notice(s): %v - a default that "+
				"announces itself is how an operator learns to skip the list", n, c.Notices())
		}
	})
}

// TestValidate_RejectsUnknownDeinterlaceValue grades [AC-3] of
// S0107-holdfast-interlacing-decision: a `deinterlace` value this build does not accept is
// refused by Validate, naming the key and the profile it was written in, so the engine
// never starts against that configuration. `holdfast validate` exits non-zero on exactly
// this error, and `run`/`serve` refuse to start on it.
//
// The FIELD-DOUBLING arm is the one that matters most and is not an unknown value at all:
// the build understands `yadif=send_field` exactly, and refuses it because it emits one
// frame per field. That doubles the output's frame count, and packet-count parity and
// duration parity are graded against the source's count - so admitting it would mean
// weakening a gate that decides whether a source is deleted.
func TestValidate_RejectsUnknownDeinterlaceValue(t *testing.T) {
	roots := "library_roots:\n  - path: /mnt/broadcast\n    "

	t.Run("a value this build cannot resolve", func(t *testing.T) {
		c, err := load(t, roots+"deinterlace: nnedi\n")
		if err != nil {
			// A refusal at load time is equally a refusal, but this key is a profile knob
			// and is checked by Validate, so a load failure would mean something else broke.
			t.Fatalf("Load: %v", err)
		}
		err = c.Validate()
		if err == nil {
			t.Fatal("Validate() accepted deinterlace: nnedi - a filter this build does not ship would " +
				"then be discovered by an ffmpeg failure half way through a library")
		}
		for _, want := range []string{deinterlaceKey, "nnedi", "/mnt/broadcast"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not name %q, so it does not say which key in which profile "+
					"has to change: %v", want, err)
			}
		}
	})

	t.Run("a field-doubling mode is understood and still refused", func(t *testing.T) {
		c, err := load(t, roots+"deinterlace: yadif=send_field\n")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		err = c.Validate()
		if err == nil {
			t.Fatal("Validate() accepted a field-doubling deinterlace: the output would carry one " +
				"frame per field, which is not the frame count packet-count parity and duration " +
				"parity are graded against")
		}
		for _, want := range []string{deinterlaceKey, "send_field", "/mnt/broadcast", "one frame per FIELD"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not carry %q: %v", want, err)
			}
		}
	})

	t.Run("the accepted values are accepted", func(t *testing.T) {
		for _, v := range []string{"off", "yadif", "bwdif", "yadif=send_frame", "bwdif=send_frame_nospatial"} {
			c, err := load(t, roots+"deinterlace: "+v+"\n")
			if err != nil {
				t.Fatalf("Load(%s): %v", v, err)
			}
			if err := c.Validate(); err != nil {
				t.Errorf("Validate() refused deinterlace: %s, which is a frame-rate-preserving value "+
					"this build runs: %v", v, err)
			}
		}
	})
}

// findNotice returns the single notice carrying token, failing the test when none does.
func findNotice(t *testing.T, c *Config, token string) string {
	t.Helper()
	for _, n := range c.Notices() {
		if strings.Contains(n, token) {
			return n
		}
	}
	t.Fatalf("no notice carries %q; Notices() = %v", token, c.Notices())
	return ""
}

// countNotices reports how many notices carry token.
func countNotices(c *Config, token string) int {
	n := 0
	for _, s := range c.Notices() {
		if strings.Contains(s, token) {
			n++
		}
	}
	return n
}
