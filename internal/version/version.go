// Package version carries the build-stamped identity of the holdfast binary.
// The values are overridden at build time via -ldflags (see the Dockerfile and
// the release workflow); the defaults are what a plain `go build` produces.
package version

import "fmt"

var (
	// Version is the semantic version, set at build time (e.g. "v0.3.1").
	Version = "0.0.0-dev"
	// Commit is the short git SHA the binary was built from.
	Commit = "unknown"
	// Date is the build timestamp (RFC 3339).
	Date = "unknown"
)

// String renders a single-line human-readable version banner.
func String() string {
	return fmt.Sprintf("holdfast %s (commit %s, built %s)", Version, Commit, Date)
}
