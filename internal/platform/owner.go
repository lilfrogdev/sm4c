// Package platform holds tiny OS-specific helpers that would otherwise
// litter other packages with build-tag files.
package platform

import "io/fs"

// OwnerUID returns the owning UID of fi on Unix-like systems. The second
// return is false if the platform-specific stat_t is unavailable (e.g.
// Windows, or a wrapped FileInfo from a test shim).
func OwnerUID(fi fs.FileInfo) (uint32, bool) { return ownerUID(fi) }
