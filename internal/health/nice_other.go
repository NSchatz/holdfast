//go:build !linux

package health

// lowerPriority is a no-op where the process priority is not set (see nice_linux.go).
func lowerPriority(int) {}
