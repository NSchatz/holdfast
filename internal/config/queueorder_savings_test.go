package config

import (
	"strings"
	"testing"
)

// `queue_order: savings_per_hour` as CONFIGURATION (S0164 AC-1, AC-2). The ordering itself is
// graded in internal/engine; here it is the sixth value of a closed set, spelled exactly one
// way.

// TestQueueOrder_AC1_SavingsPerHourIsAcceptedFromTheFileAndTheEnvironment is [AC-1]: the
// value starts from the YAML file and from HOLDFAST_QUEUE_ORDER alike, and resolves to
// itself.
func TestQueueOrder_AC1_SavingsPerHourIsAcceptedFromTheFileAndTheEnvironment(t *testing.T) {
	c, err := load(t, "library_roots:\n  - /mnt/media\nqueue_order: savings_per_hour\n")
	if err != nil {
		t.Fatalf("Load refused queue_order: savings_per_hour: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate refused queue_order: savings_per_hour: %v", err)
	}
	if got := c.EffectiveQueueOrder(); got != QueueOrderSavingsPerHour {
		t.Errorf("queue_order resolved to %q, want %q", got, QueueOrderSavingsPerHour)
	}
	if !c.QueueOrderNeedsKey() {
		t.Error("savings_per_hour reported that it needs no per-candidate key")
	}

	t.Setenv("HOLDFAST_QUEUE_ORDER", "savings_per_hour")
	env, err := load(t, "library_roots:\n  - /mnt/media\n")
	if err != nil {
		t.Fatalf("Load refused HOLDFAST_QUEUE_ORDER=savings_per_hour: %v", err)
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("Validate refused HOLDFAST_QUEUE_ORDER=savings_per_hour: %v", err)
	}
	if got := env.EffectiveQueueOrder(); got != QueueOrderSavingsPerHour {
		t.Errorf("HOLDFAST_QUEUE_ORDER=savings_per_hour resolved to %q", got)
	}

	// The closed set is SIX values, savings_per_hour last, and the list every refusal and the
	// example configuration name carries it.
	if len(QueueOrders) != 6 || QueueOrders[5] != "savings_per_hour" {
		t.Errorf("QueueOrders = %v, want the five earlier values then savings_per_hour", QueueOrders)
	}
	if got, want := QueueOrderList(), "path|largest|smallest|newest|oldest|savings_per_hour"; got != want {
		t.Errorf("QueueOrderList() = %q, want %q", got, want)
	}
}

// TestQueueOrder_AC2_ANearMissOfSavingsPerHourIsRefusedNamingAllSix is [AC-2]: every
// spelling other than the exact one refuses to start, naming the rejected value and all six
// accepted values. The brief's pre-approval name (savings_rate) is not an alias.
func TestQueueOrder_AC2_ANearMissOfSavingsPerHourIsRefusedNamingAllSix(t *testing.T) {
	for _, tc := range []struct {
		name, written, echoes string
	}{
		{"hyphens", "queue_order: savings-per-hour\n", `"savings-per-hour"`},
		{"mixed case", "queue_order: Savings_Per_Hour\n", `"Savings_Per_Hour"`},
		{"a trailing space", "queue_order: \"savings_per_hour \"\n", `"savings_per_hour "`},
		{"the empty string", "queue_order: \"\"\n", `""`},
		{"the pre-approval name", "queue_order: savings_rate\n", `"savings_rate"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, "library_roots:\n  - /mnt/media\n"+tc.written)
			if err == nil {
				t.Fatalf("Load accepted %q", tc.written)
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.echoes) {
				t.Errorf("the refusal does not name the rejected value %s:\n%s", tc.echoes, msg)
			}
			for _, v := range QueueOrders {
				if !strings.Contains(msg, v) {
					t.Errorf("the refusal does not name accepted value %q:\n%s", v, msg)
				}
			}
		})
	}

	// A Config assembled by hand meets the same refusal through Validate.
	hand := Config{LibraryRoots: []string{"/mnt/media"}, QueueOrder: "savings-per-hour"}
	if err := hand.Validate(); err == nil || !strings.Contains(err.Error(), "savings_per_hour") {
		t.Errorf("Validate on a hand-assembled savings-per-hour = %v, want a refusal naming the six", err)
	}
}
