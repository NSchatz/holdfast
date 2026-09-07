//go:build !linux

package fsclass

import "errors"

// errUnsupported is what LookupType reports where this build has no way to ask the
// platform. It classifies Undetermined and therefore NOT LOCAL - the same fail-safe an
// unreadable path gets, which is the only honest answer on a platform whose filesystem
// types this build cannot enumerate. holdfast ships as a Linux container; this file
// exists so the package still builds (and still fails safe) anywhere else.
var errUnsupported = errors.New("fsclass: filesystem-type lookup is not implemented on this platform")

// LookupType reports that the type is unknown on a non-Linux platform.
func LookupType(path string) (string, error) { return "", errUnsupported }

// unknownType is referenced by the Linux lookup only; it is defined in fsclass.go and
// kept compiling here by this file's presence in the same package.
var _ = unknownType
