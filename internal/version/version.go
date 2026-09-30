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

// Packaging is how this binary is distributed: PackagingImage in the container image, "" for
// any other build. The image's build sets it through the holdfast_image build tag
// (packaging_image.go), which the Dockerfile's go build passes and nothing else does. It
// decides one thing: the image cannot carry AMD's AMF runtime, so `encoder: amf` is refused
// there with the reason (internal/encoder.RefusedInImage), and works as before on a host
// install.
var Packaging = packaging

// PackagingImage is the Packaging of the container image's binary.
const PackagingImage = "image"
