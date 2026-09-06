//go:build !linux

package startup

import (
	"errors"
	"runtime"
)

// errUnsupportedPlatform is what every platform read answers where holdfast has
// no way to establish what storage a path sits on. It is deliberately a refusal
// and not a shrug: this build ships as a Linux container, and a host whose
// storage the tool cannot classify is one where the no-loss contract is stated
// and unchecked. Fail-safe means the operator is told, not that the check is
// skipped.
var errUnsupportedPlatform = errors.New("holdfast cannot establish what storage a path is on for GOOS=" + runtime.GOOS)

// System returns a Platform for a host this build cannot inspect. The seams are
// still honoured, so a test may substitute either one.
func System(typeLookup func(string) (string, error), mountLookup func(string) (bool, error)) Platform {
	return unsupported{typeLookup: typeLookup, mountLookup: mountLookup}
}

type unsupported struct {
	typeLookup  func(string) (string, error)
	mountLookup func(string) (bool, error)
}

func (unsupported) Inspect(string) (Info, error)    { return Info{}, errUnsupportedPlatform }
func (unsupported) ReadDir(string) ([]Entry, error) { return nil, errUnsupportedPlatform }
func (unsupported) Resolve(string) (string, error)  { return "", errUnsupportedPlatform }

func (u unsupported) FSType(path string) (string, error) {
	if u.typeLookup != nil {
		return u.typeLookup(path)
	}
	return "", errUnsupportedPlatform
}

func (u unsupported) MountPoint(path string) (bool, error) {
	if u.mountLookup != nil {
		return u.mountLookup(path)
	}
	return false, ErrMountInfoUnavailable
}
