//go:build !linux

package diskfree

import (
	"errors"
	"runtime"
)

// bytes refuses on a platform this build has no free-space call for, exactly as
// internal/startup refuses to classify storage there. This build ships as a Linux
// container, and a host whose free space holdfast cannot establish is one where the
// floor and the per-job pre-check would be answering from nothing. Fail-safe means
// the operator is told, not that the number is guessed.
func bytes(string) (uint64, error) {
	return 0, errors.New("holdfast cannot establish the free space on a filesystem for GOOS=" + runtime.GOOS)
}

// id refuses for the same reason: a caller that cannot name the filesystem counts the
// path together with every other one it could not name, which is the fail-safe
// direction, rather than trusting a guess.
func id(string) (string, error) {
	return "", errors.New("holdfast cannot establish which filesystem holds a path for GOOS=" + runtime.GOOS)
}
