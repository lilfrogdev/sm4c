//go:build !windows

package config

import (
	"io/fs"
	"syscall"
)

// ownerUID returns the owning UID of fi on unix-like systems. The second
// return value is false if the platform-specific stat_t is not available
// (e.g. wrapped FileInfo from a test shim).
func ownerUID(fi fs.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}
