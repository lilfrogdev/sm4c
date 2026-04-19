//go:build windows

package platform

import "io/fs"

func ownerUID(_ fs.FileInfo) (uint32, bool) { return 0, false }
