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
