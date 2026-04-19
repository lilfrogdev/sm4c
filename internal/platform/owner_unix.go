//go:build !windows

package platform

import (
	"io/fs"
	"syscall"
)

func ownerUID(fi fs.FileInfo) (uint32, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}
