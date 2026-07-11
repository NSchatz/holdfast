// Package config loads and validates the transcode YAML configuration.
//
// This is the genesis (TRANSCODE-0) config surface: a minimal, strictly-validated
// struct sufficient to prove the config-as-code contract and to guarantee the tool
// refuses to run against a dangerous or unspecified library root. The full schema —
// koanf-backed, env-overridable, with a CI schema self-test — arrives in TRANSCODE-2
// (see operations/roadmaps/transcode.md §8). Fields are deliberately few here; the
// invariant that matters now is Validate(): a delete-capable tool must never start
// pointed at "/" or a home directory.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the declarative, YAML-authored configuration for the transcoder.
type Config struct {
	// LibraryRoots are the directory trees the tool scans and, in later phases,
	// re-encodes files under. It is the ONLY place the tool ever mutates the
	// filesystem, so it is validated strictly (see Validate).
	LibraryRoots []string `yaml:"library_roots"`

	// LogLevel controls verbosity: debug|info|warn|error (default info).
	LogLevel string `yaml:"log_level"`

	// DryRun, when true, makes the tool report intended actions without changing
	// any file. Genesis always behaves as if the engine is unimplemented, but the
	// flag is carried so downstream phases inherit the field.
	DryRun bool `yaml:"dry_run"`
}

// ErrNoConfig is returned by Load when the path is empty.
var ErrNoConfig = errors.New("no config path provided")

// Load reads and parses a YAML config file. It does NOT validate — callers run
// Validate() explicitly so `validate` and `run` share one code path. Unknown
// keys are rejected (KnownFields) so a typo becomes a loud error, never a silent
// default — the fail-safe posture the whole tool is built on.
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, ErrNoConfig
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config %q: %w", path, err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	return &c, nil
}

// Validate refuses a configuration that could cause harm or is under-specified.
// The rules are conservative on purpose: the tool is delete-capable, so an
// ambiguous or dangerous root is a hard error, never a guessed default.
func (c *Config) Validate() error {
	if len(c.LibraryRoots) == 0 {
		return errors.New("library_roots is empty: refusing to run with nothing to scan")
	}

	home, _ := os.UserHomeDir()
	seen := make(map[string]struct{}, len(c.LibraryRoots))
	for i, root := range c.LibraryRoots {
		if root == "" {
			return fmt.Errorf("library_roots[%d] is empty", i)
		}
		if !filepath.IsAbs(root) {
			return fmt.Errorf("library_roots[%d] %q must be an absolute path", i, root)
		}
		clean := filepath.Clean(root)
		if clean == "/" {
			return fmt.Errorf("library_roots[%d] resolves to %q: refusing to operate on the filesystem root", i, clean)
		}
		if home != "" && clean == filepath.Clean(home) {
			return fmt.Errorf("library_roots[%d] resolves to the home directory %q: refusing", i, clean)
		}
		if _, dup := seen[clean]; dup {
			return fmt.Errorf("library_roots[%d] %q is a duplicate", i, clean)
		}
		seen[clean] = struct{}{}
	}

	switch c.LogLevel {
	case "", "debug", "info", "warn", "error":
		// ok
	default:
		return fmt.Errorf("log_level %q is not one of debug|info|warn|error", c.LogLevel)
	}
	return nil
}
