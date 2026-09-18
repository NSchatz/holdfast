package config

import (
	"errors"
	"strings"
	"testing"
)

// The `max_height` key: what it says at start, and what it refuses.
//
// It is the bluntest knob in this configuration - it throws pixels away on purpose - so
// both halves below are about the same thing: an operator learning, before the first file
// goes, exactly what this key does to a library they cannot get back, and being refused
// outright when they wrote a height this build cannot target.

// TestNotices_MaxHeightSaysNotSameContent grades [AC-3]: a configuration that sets
// `max_height` on any profile or rule earns a record at NOTICE level, distinguishable from
// a warning and from an error, stating that the replacement will no longer be the same
// content as the source - and the configuration still validates, so `holdfast validate`
// exits zero on it.
//
// The three channels are three different claims and this build keeps them apart: Validate
// returning an error is "refuse to start", Warnings() is "a safety gate has been weakened",
// and Notices() is "here is what your configuration MEANS". A downscale is the third: no
// floor moves, the perceptual gate still scores at the source's own resolution, and a
// rejected encode still leaves the source untouched. So the notice must appear in the notice
// list, must NOT appear in the warning list, and must not stop the configuration validating.
//
// The default arm is not decoration: the notice must not fire for the shipped default, or it
// trains the reader to skip the list that also carries the undo window.
//
// MUTATION: route the statement through Warnings() instead and the second sub-test reds;
// emit it for the shipped default and the last one does.
func TestNotices_MaxHeightSaysNotSameContent(t *testing.T) {
	t.Run("a root that sets a ceiling says so, naming the root", func(t *testing.T) {
		c := loadYAML(t, `
library_roots:
  - /mnt/tv
  - path: /mnt/uhd
    max_height: 1080
`)
		notice := findNotice(t, c, maxHeightKey)
		for _, want := range []string{
			"NO LONGER THE SAME CONTENT AS THE SOURCE",
			"/mnt/uhd",
			"1080",
		} {
			if !strings.Contains(notice, want) {
				t.Errorf("the ceiling notice does not carry %q, so it does not state what the key does "+
					"to a library:\n%s", want, notice)
			}
		}
		if n := countNotices(c, maxHeightKey); n != 1 {
			t.Errorf("%d notices name %s, want exactly 1 - only /mnt/uhd set a ceiling", n, maxHeightKey)
		}
		if strings.Contains(notice, "/mnt/tv") {
			t.Errorf("the notice names /mnt/tv, which scales nothing:\n%s", notice)
		}
	})

	t.Run("it is a NOTICE and not a warning, and validate still passes", func(t *testing.T) {
		c := loadYAML(t, "library_roots:\n  - path: /mnt/uhd\n    max_height: 1080\n")
		if countNotices(c, maxHeightKey) == 0 {
			t.Fatalf("no notice names %s; Notices() = %v", maxHeightKey, c.Notices())
		}
		for _, w := range c.Warnings() {
			if strings.Contains(w, maxHeightKey) {
				t.Errorf("setting %s produced a WARNING, which in this build always means a safety "+
					"gate was weakened - no floor moved here, and a default-shaped statement in the "+
					"warning list is how an operator learns to skip warnings:\n%s", maxHeightKey, w)
			}
		}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate() refused a configuration whose only unusual key is a legal %s, so "+
				"`holdfast validate` would exit non-zero on a configuration this build supports: %v",
				maxHeightKey, err)
		}
	})

	t.Run("a ceiling written in a RULE earns the notice too", func(t *testing.T) {
		c := loadYAML(t, `
library_roots:
  - path: /mnt/mixed
    rules:
      - when:
          min_source_height: 1440
        max_height: 1080
`)
		notice := findNotice(t, c, maxHeightKey)
		if !strings.Contains(notice, "NO LONGER THE SAME CONTENT AS THE SOURCE") {
			t.Errorf("a ceiling set in a rule earns a notice that does not state the loss:\n%s", notice)
		}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate() refused a legal per-band ceiling: %v", err)
		}
	})

	t.Run("the shipped default says nothing", func(t *testing.T) {
		c := loadYAML(t, "library_roots:\n  - /mnt/tv\n")
		if n := countNotices(c, maxHeightKey); n != 0 {
			t.Errorf("the shipped default earns %d ceiling notice(s): %v - a default that announces "+
				"itself is how an operator learns to skip the list", n, c.Notices())
		}
	})
}

// TestNotices_MaxHeightWithoutAcknowledgementSaysFilesAreSkipped grades [AC-3] beside
// [AC-4]: the notice a configuration earns has to describe the configuration it IS, and a
// ceiling set with the undo window closed and no acknowledgement is one under which every
// file the key would scale is skipped rather than encoded.
//
// Without this the notice would announce a downscale to an operator whose library is not
// being downscaled at all, which is a worse failure than silence.
func TestNotices_MaxHeightWithoutAcknowledgementSaysFilesAreSkipped(t *testing.T) {
	unacked := loadYAML(t, "library_roots:\n  - path: /mnt/uhd\n    max_height: 1080\n")
	notice := findNotice(t, unacked, maxHeightKey)
	for _, want := range []string{downscaleAckKey, "SKIPPED"} {
		if !strings.Contains(notice, want) {
			t.Errorf("the notice for an unacknowledged ceiling under a closed undo window does not "+
				"carry %q, so it announces a downscale that will not happen:\n%s", want, notice)
		}
	}

	acked := loadYAML(t, "library_roots:\n  - path: /mnt/uhd\n    max_height: 1080\n    downscale_acknowledged: true\n")
	if n := findNotice(t, acked, maxHeightKey); strings.Contains(n, "SKIPPED") {
		t.Errorf("an ACKNOWLEDGED ceiling still says its files are skipped, which is the opposite of "+
			"what this configuration does:\n%s", n)
	}
}

// TestValidate_RefusesAMaxHeightThisBuildCannotTarget grades [AC-10]: a `max_height` that
// is present but not a positive integer, or is a value the encoder cannot target, is refused
// with a TYPED error naming the key and the offending value, at every place the key may be
// written - so a run refuses to start rather than falling back to an unset key.
//
// The typed half matters as much as the message: a caller that had to match on message text
// to tell this refusal from any other would go on matching the day the message improved.
// Every arm therefore asserts errors.Is against the one sentinel.
//
// The ODD arm is the "value the encoder cannot target" half, and it is not a limitation of
// this build's plumbing: every pixel format it encodes to is 4:2:0 chroma-subsampled and has
// no representation for an odd dimension. Rounding 1081 to 1080 or 1082 would be holdfast
// choosing which of two libraries the operator gets, on the one knob that decides how many
// pixels their replacements keep.
//
// MUTATION: drop the raw-value check and the `0` and `1080.5` arms red; drop the odd check
// and the `1081` arms red; drop the sentinel wrap and every arm reds.
func TestValidate_RefusesAMaxHeightThisBuildCannotTarget(t *testing.T) {
	bad := []struct {
		value string
		// token is what the refusal has to quote back, so an operator can find the line.
		token string
	}{
		{"0", "0"},
		{"-720", "-720"},
		{"1080.5", "1080.5"},
		{"1081", "1081"},
		{"tall", "tall"},
		{"true", "true"},
	}

	for _, tc := range bad {
		t.Run("top level "+tc.value, func(t *testing.T) {
			err := startRefusal(t, "library_roots:\n  - /mnt/tv\nmax_height: "+tc.value+"\n")
			assertCeilingRefusal(t, err, tc.token)
		})
		t.Run("entry "+tc.value, func(t *testing.T) {
			err := startRefusal(t, "library_roots:\n  - path: /mnt/tv\n    max_height: "+tc.value+"\n")
			assertCeilingRefusal(t, err, tc.token)
		})
		t.Run("rule "+tc.value, func(t *testing.T) {
			err := startRefusal(t, "library_roots:\n  - path: /mnt/tv\n    rules:\n"+
				"      - when:\n          min_source_height: 1440\n        max_height: "+tc.value+"\n")
			assertCeilingRefusal(t, err, tc.token)
		})
	}

	t.Run("a legal even ceiling is accepted everywhere it may be written", func(t *testing.T) {
		for _, yaml := range []string{
			"library_roots:\n  - /mnt/tv\nmax_height: 1080\n",
			"library_roots:\n  - path: /mnt/tv\n    max_height: 720\n",
			"library_roots:\n  - path: /mnt/tv\n    rules:\n" +
				"      - when:\n          min_source_height: 1440\n        max_height: 1080\n",
		} {
			c, err := load(t, yaml)
			if err != nil {
				t.Fatalf("Load refused a legal ceiling:\n%s\n%v", yaml, err)
			}
			if err := c.Validate(); err != nil {
				t.Errorf("Validate refused a legal ceiling:\n%s\n%v", yaml, err)
			}
		}
	})
}

// TestMaxHeightDefault_IsReadOffTheShippedDefaults grades [AC-9]'s second half at its
// source: the number the shipped documentation is graded against comes off the defaults
// layer this build actually loads, not off a constant beside the check.
//
// Without that, changing the default would move what the build does and leave every
// documentation check passing on a statement that had stopped being true.
func TestMaxHeightDefault_IsReadOffTheShippedDefaults(t *testing.T) {
	if got := MaxHeightDefault(); got != 0 {
		t.Errorf("MaxHeightDefault() = %d, want 0 - this build ships NO ceiling, and the README's "+
			"anchored statement says so", got)
	}
	c := loadYAML(t, "library_roots:\n  - /mnt/tv\n")
	if got := rootByPath(t, c, "/mnt/tv").Profile.MaxHeight; got != MaxHeightDefault() {
		t.Errorf("a root that sets nothing resolves to max_height %d while MaxHeightDefault() says "+
			"%d - the documented default and the one a file is decided by are two numbers",
			got, MaxHeightDefault())
	}
}

// startRefusal loads yaml and returns the ERROR VALUE the configuration earns, from Load or
// from Validate. Both are refusals to start, and which of the two catches a given spelling is
// an implementation detail of where the value is still raw. It returns the error rather than
// its text (as the rules refusal beside it does) because the typed half of AC-10 is graded
// with errors.Is, which a string cannot answer.
func startRefusal(t *testing.T, yaml string) error {
	t.Helper()
	c, err := load(t, yaml)
	if err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	t.Fatalf("the configuration was accepted, so a run would start against it:\n%s", yaml)
	return nil
}

// assertCeilingRefusal holds one refusal to the whole of AC-10: the typed error, the key,
// and the offending value quoted back.
func assertCeilingRefusal(t *testing.T, err error, token string) {
	t.Helper()
	if !errors.Is(err, ErrMaxHeightUnusable) {
		t.Errorf("the refusal is not ErrMaxHeightUnusable, so a caller has to match on message text "+
			"to know which key refused: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, maxHeightKey) {
		t.Errorf("the refusal does not name %s, so it does not say which key has to change: %s",
			maxHeightKey, msg)
	}
	if !strings.Contains(msg, token) {
		t.Errorf("the refusal does not quote the offending value %q back, so an operator cannot find "+
			"the line that produced it: %s", token, msg)
	}
}
