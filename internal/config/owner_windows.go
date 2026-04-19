//go:build windows

package config

import "io/fs"

// ownerUID is a no-op on Windows. sm4c does not support Windows in v1; this
// stub only exists so `go vet ./...` and `go build ./...` succeed on
// windows hosts used for IDE cross-indexing.
func ownerUID(_ fs.FileInfo) (uint32, bool) { return 0, false }
