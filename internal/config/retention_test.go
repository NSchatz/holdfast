package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The ledger-retention key (LEDGER-5).
//
// Criterion 8: WHEN a configuration that does not set the retention key is loaded THE
// SYSTEM SHALL keep every terminal row.
//
// Criterion 9: IF the configured retention is negative or is not a whole number of rows
// THEN THE SYSTEM SHALL refuse to start, naming the key and the offending value, before it
// opens the job store.
//
// Both are decided here because this package is where a startup refusal happens: cmd's
// loadConfig runs Load and Validate, and only then does buildEngine open the store. A
// refusal from either function is therefore a refusal BEFORE the store is opened - which
// cmd/holdfast/retention_test.go's TestRetention_ARefusedRetentionOpensNoJobStore proves
// end to end, on the real dispatch path, since this package cannot see cmd's ordering.

// writeConfig writes a minimal valid configuration with body appended, and returns its
// path. library_roots is a real directory so Validate has something legitimate to accept.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "library")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir library root: %v", err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("library_roots:\n  - "+root+"\n"+body+"\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// --- criterion 8: an absent key keeps every row ----------------------------------------

func TestRetention_AConfigThatDoesNotSetTheKeyKeepsEveryTerminalRow(t *testing.T) {
	cfg, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.HistoryRetentionRows != 0 {
		t.Errorf("an absent history_retention_rows resolved to %d, want 0", cfg.HistoryRetentionRows)
	}
	if cfg.RetentionEnabled() {
		t.Error("an absent history_retention_rows enabled retention; a prune is an IRREVERSIBLE delete of " +
			"audit history and must never be something an operator gets without asking for it")
	}
}

func TestRetention_TheShippedDefaultLayerDisablesRetention(t *testing.T) {
	// The default layer is the single source of defaults, so this is where "ships
	// disabled" is actually decided. A future edit that made it non-zero would turn every
	// existing installation into one that deletes its own history on the next upgrade.
	v, ok := defaultLayer()[retentionKey]
	if !ok {
		t.Fatalf("%s is not in the default layer, so an absent key has no defined value", retentionKey)
	}
	if n, isInt := v.(int); !isInt || n != 0 {
		t.Errorf("the shipped default for %s is %#v, want the int 0 (retention disabled)", retentionKey, v)
	}
	if !knownKeys[retentionKey] {
		t.Errorf("%s is not a known key, so setting it would be rejected as a typo", retentionKey)
	}
}

func TestRetention_AnExplicitZeroIsAcceptedAndKeepsEveryRow(t *testing.T) {
	cfg, err := Load(writeConfig(t, "history_retention_rows: 0"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.RetentionEnabled() {
		t.Error("an explicit history_retention_rows: 0 enabled retention")
	}
}

func TestRetention_APositiveValueIsTheMaximumNumberOfRetainedRows(t *testing.T) {
	cfg, err := Load(writeConfig(t, "history_retention_rows: 25000"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.HistoryRetentionRows != 25000 || !cfg.RetentionEnabled() {
		t.Errorf("history_retention_rows: 25000 loaded as %d (enabled=%v)", cfg.HistoryRetentionRows, cfg.RetentionEnabled())
	}
}

func TestRetention_TheEnvironmentOverridesTheFile(t *testing.T) {
	// HOLDFAST_* beats the YAML file for every other key; a retention that ignored the
	// environment would be the one knob an operator could not set the usual way.
	t.Setenv("HOLDFAST_HISTORY_RETENTION_ROWS", "40")
	cfg, err := Load(writeConfig(t, "history_retention_rows: 10"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.HistoryRetentionRows != 40 {
		t.Errorf("the environment override loaded as %d, want 40", cfg.HistoryRetentionRows)
	}
}

// --- criterion 9: negative, or not a whole number of rows, is a refusal ------------------

func TestRetention_ANegativeValueIsARefusalNamingTheKeyAndTheValue(t *testing.T) {
	for _, c := range []struct{ body, wantValue string }{
		{"history_retention_rows: -1", "-1"},
		{"history_retention_rows: -25000", "-25000"},
	} {
		cfg, err := Load(writeConfig(t, c.body))
		if err != nil {
			// A negative whole number decodes faithfully; it is Validate's to refuse.
			t.Fatalf("Load(%q) failed before Validate could refuse it: %v", c.body, err)
		}
		err = cfg.Validate()
		if err == nil {
			t.Fatalf("Validate accepted %q; a negative retention is neither a bound nor 'keep everything'", c.body)
		}
		requireNamesKeyAndValue(t, err, c.body, c.wantValue)
	}
}

func TestRetention_AValueThatIsNotAWholeNumberOfRowsIsARefusalNamingTheKeyAndTheValue(t *testing.T) {
	// Every one of these would otherwise be SILENTLY coerced by the weakly-typed decoder:
	// 3.7 truncates to 3, "many" and true both become 0 (retention off), and a key with
	// no value at all reads as nothing. On a knob whose effect is an irreversible delete,
	// a retention the operator did not write is the failure mode to refuse.
	for _, c := range []struct {
		body      string
		wantValue string // the offending value, as the refusal must spell it
	}{
		{"history_retention_rows: 3.7", "3.7"},
		{"history_retention_rows: -0.5", "-0.5"},
		{"history_retention_rows: 1e400", "1e400"},
		{"history_retention_rows: many", "many"},
		{"history_retention_rows: true", "true"},
		{`history_retention_rows: ""`, ""},
		{"history_retention_rows:", ""},
		{"history_retention_rows: [10]", "10"},
		{"history_retention_rows:\n  rows: 10", "10"},
	} {
		_, err := Load(writeConfig(t, c.body))
		if err == nil {
			t.Errorf("Load accepted %q; a value that is not a whole number of rows must be a refusal", c.body)
			continue
		}
		requireNamesKeyAndValue(t, err, c.body, c.wantValue)
	}
}

func TestRetention_ANonIntegerEnvironmentOverrideIsARefusalToo(t *testing.T) {
	t.Setenv("HOLDFAST_HISTORY_RETENTION_ROWS", "3.7")
	_, err := Load(writeConfig(t, "history_retention_rows: 10"))
	if err == nil {
		t.Fatal("Load accepted a non-integer environment override; env beats the file, so it needs the same refusal")
	}
	requireNamesKeyAndValue(t, err, "HOLDFAST_HISTORY_RETENTION_ROWS=3.7", "3.7")
}

func TestRetention_AWholeNumberSpelledAsAStringIsAccepted(t *testing.T) {
	// An environment override arrives as a string and a YAML author may quote one. Both
	// are whole numbers of rows and neither is the ambiguity the refusal is about.
	for _, body := range []string{`history_retention_rows: "12"`, `history_retention_rows: '12'`} {
		cfg, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatalf("Load(%q): %v", body, err)
		}
		if cfg.HistoryRetentionRows != 12 {
			t.Errorf("%q loaded as %d, want 12", body, cfg.HistoryRetentionRows)
		}
	}
}

// requireNamesKeyAndValue is the criterion's own wording: the refusal names the key AND the
// offending value. A message that said only "invalid config" would leave an operator
// grepping a YAML file for which of thirty keys it meant, and one that named the key alone
// would not tell them which of several spellings on the line was rejected.
//
// wantValue is "" where there is no value TO name (an empty or absent one), and the key
// alone is then the whole of what can be said.
func requireNamesKeyAndValue(t *testing.T, err error, subject, wantValue string) {
	t.Helper()
	msg := err.Error()
	if !strings.Contains(msg, "history_retention_rows") {
		t.Errorf("the refusal for %q does not name the key: %q", subject, msg)
	}
	if wantValue != "" && !strings.Contains(msg, wantValue) {
		t.Errorf("the refusal for %q does not name the offending value %q: %q", subject, wantValue, msg)
	}
}
