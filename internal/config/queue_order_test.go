package config

import (
	"strings"
	"testing"
)

// The `queue_order` key as CONFIGURATION: what it resolves to, and what it refuses.
//
// The refusal is the half that matters here and it is S0095 AC-3: a value outside the
// accepted set refuses to START, with the same nonzero exit any other invalid configuration
// value produces, and names both the value it rejected and the five it accepts (cli L7).
// `validate` and `run` reach this through one code path - loadConfig - so a configuration
// one of them refuses is refused by the other.

// TestQueueOrder_EveryAcceptedValueLoads is [AC-1]'s configuration half: each of the five
// values this build offers is accepted and resolves to itself.
func TestQueueOrder_EveryAcceptedValueLoads(t *testing.T) {
	for _, order := range QueueOrders {
		c, err := load(t, "library_roots:\n  - /mnt/media\nqueue_order: "+order+"\n")
		if err != nil {
			t.Fatalf("Load refused queue_order %q: %v", order, err)
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate refused queue_order %q: %v", order, err)
		}
		if got := c.EffectiveQueueOrder(); got != order {
			t.Errorf("queue_order %q resolved to %q", order, got)
		}
	}
}

// TestQueueOrder_AnAbsentKeyResolvesToPath is [AC-2]'s configuration half: the key is
// optional and defaults to today's behaviour, so no existing install changes by upgrading
// into a build that has it.
func TestQueueOrder_AnAbsentKeyResolvesToPath(t *testing.T) {
	c, err := load(t, "library_roots:\n  - /mnt/media\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate refused a configuration that never mentions queue_order: %v", err)
	}
	if got := c.EffectiveQueueOrder(); got != QueueOrderPath {
		t.Errorf("an absent queue_order resolved to %q, want %q: an install that predates this "+
			"key must offer its files in exactly the sequence it always did", got, QueueOrderPath)
	}
	if c.QueueOrderNeedsKey() {
		t.Error("the default order reported that it needs a per-candidate ordering key; `path` is " +
			"the enumeration's own sequence and reads nothing")
	}

	// A Config assembled by hand carries no defaults layer at all, and every engine test
	// builds one. It must resolve the same way, or the default would be a property of the
	// loader rather than of the build.
	hand := Config{LibraryRoots: []string{"/mnt/media"}}
	if got := hand.EffectiveQueueOrder(); got != QueueOrderPath {
		t.Errorf("a hand-assembled Config resolved queue_order to %q, want %q", got, QueueOrderPath)
	}
}

// TestQueueOrder_RefusesAValueOutsideTheAcceptedSet is [AC-3]. Every refusal names the
// rejected value AND the five accepted ones, and the empty string is refused with them: the
// defaults layer fills an absent key, so an empty value is one the operator wrote.
func TestQueueOrder_RefusesAValueOutsideTheAcceptedSet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		written string
		echoes  string
	}{
		{name: "a plausible synonym", written: "queue_order: biggest\n", echoes: `"biggest"`},
		{name: "the empty string", written: "queue_order: \"\"\n", echoes: `""`},
		{name: "a key written with no value at all", written: "queue_order:\n", echoes: `""`},
		{name: "the right word in the wrong case", written: "queue_order: Largest\n", echoes: `"Largest"`},
		{name: "a value that is not a string", written: "queue_order: 3\n", echoes: `"3"`},
		{name: "whitespace around a real value", written: "queue_order: \" largest \"\n", echoes: `" largest "`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, "library_roots:\n  - /mnt/media\n"+tc.written)
			if err == nil {
				t.Fatalf("Load accepted %q: an order this build cannot read must refuse to START, "+
					"never resolve to something and spend the next pass processing the library in a "+
					"sequence nobody asked for", strings.TrimSpace(tc.written))
			}
			msg := err.Error()
			if !strings.Contains(msg, queueOrderKey) {
				t.Errorf("the refusal never names the key %q: %v", queueOrderKey, err)
			}
			if !strings.Contains(msg, tc.echoes) {
				t.Errorf("the refusal never names the value it rejected (%s): %v", tc.echoes, err)
			}
			for _, accepted := range QueueOrders {
				if !strings.Contains(msg, accepted) {
					t.Errorf("the refusal never names the accepted value %q, so an operator who has "+
						"just been refused still has to go and find the spelling: %v", accepted, err)
				}
			}
		})
	}
}

// TestQueueOrder_ValidateRefusesAValueNoLoadSaw is [AC-3] at the second door. Load is not
// the only way a Config is built - the engine's own callers assemble one - so the refusal
// lives at Validate as well, which is what `holdfast validate` and `holdfast run` share.
func TestQueueOrder_ValidateRefusesAValueNoLoadSaw(t *testing.T) {
	c := Config{LibraryRoots: []string{"/mnt/media"}, QueueOrder: "biggest"}
	err := c.Validate()
	if err == nil {
		t.Fatal("Validate accepted queue_order \"biggest\"")
	}
	if !strings.Contains(err.Error(), queueOrderKey) || !strings.Contains(err.Error(), "biggest") {
		t.Errorf("the refusal names neither the key nor the value: %v", err)
	}
	for _, accepted := range QueueOrders {
		if !strings.Contains(err.Error(), accepted) {
			t.Errorf("the refusal never names the accepted value %q: %v", accepted, err)
		}
	}

	// Anti-vacuity: the refusal is about the VALUE and not about the key being set at all.
	for _, order := range QueueOrders {
		ok := Config{LibraryRoots: []string{"/mnt/media"}, QueueOrder: order}
		if err := ok.Validate(); err != nil {
			t.Errorf("Validate refused the accepted value %q: %v", order, err)
		}
	}
}

// TestQueueOrder_TheEnvironmentIsRefusedTheSameWay is [AC-3] over the third layer. The
// environment overrides the file, so an unreadable order written there has to refuse in the
// same words rather than falling through to whatever the file said.
func TestQueueOrder_TheEnvironmentIsRefusedTheSameWay(t *testing.T) {
	t.Setenv("HOLDFAST_QUEUE_ORDER", "biggest")
	_, err := load(t, "library_roots:\n  - /mnt/media\nqueue_order: largest\n")
	if err == nil {
		t.Fatal("HOLDFAST_QUEUE_ORDER=biggest was accepted over a valid file value")
	}
	if !strings.Contains(err.Error(), "biggest") {
		t.Errorf("the refusal names the file's value rather than the environment's: %v", err)
	}
}
