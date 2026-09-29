//go:build !linux

package engine

import (
	"errors"
	"os"
)

// ownerLocksSupported: this build takes no open-file-description lock off Linux, so it
// writes no owner record there, and a record it meets answers "cannot decide". The shipped
// artefact is a linux/amd64 + linux/arm64 image; elsewhere a temp is swept exactly as it
// was before owner records existed.
const ownerLocksSupported = false

func lockRecord(*os.File) error {
	return errors.New("open-file-description locks are not taken on this platform")
}
