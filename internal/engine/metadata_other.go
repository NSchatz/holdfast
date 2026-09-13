//go:build !linux

package engine

import "io/fs"

// ownerOf reports NO ownership on a platform this build does not read one on.
//
// The shipped artefact is a linux/amd64 + linux/arm64 distroless image, and where the
// platform has no ownership model there is nothing for a swap to carry: the replacement
// gets whatever the filesystem gives a newly created file, exactly as it did before this
// existed. It is deliberately not a refusal - unlike internal/startup's unsupported
// platform, which refuses because holdfast cannot establish what storage it is on and the
// no-loss contract depends on the answer. Nothing about no-loss depends on this one, so an
// absent ownership model costs the ownership and nothing else. The mode and the
// modification time are still carried on every platform.
func ownerOf(fs.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
