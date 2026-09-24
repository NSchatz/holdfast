// Package diskfree reports the space available on the filesystem holding a path.
//
// It exists as its own package because TWO parts of holdfast have to agree about
// the answer: the startup check, which refuses a run whose scratch directory is
// already below the configured floor, and the engine, which refuses one JOB whose
// source is larger than the space left at the moment it is about to encode. Two
// implementations of "how much room is there" would be two answers, and the second
// one is taken hours after the first with an operator's original at stake.
//
// It reports the space available to THIS process (an unprivileged caller), not the
// filesystem's total free space: the reserved blocks a filesystem keeps for root
// are not room holdfast can write into, and counting them would make the check pass
// on a device the encode then fails on.
package diskfree

// Bytes returns the number of bytes available to this process on the filesystem
// holding path. A path that cannot be inspected is an error, never a zero or a
// large number that would make a caller's comparison silently meaningless.
func Bytes(path string) (uint64, error) { return bytes(path) }

// ID names the filesystem holding path, so that two paths on one filesystem - two
// directories of it included - answer with the same string. The engine keys the bytes
// its in-flight jobs are about to write by it, because Bytes cannot see a write that
// has not happened yet. A path whose filesystem cannot be named is an error, never an
// invented identity that would let two jobs on one device forget each other.
func ID(path string) (string, error) { return id(path) }
