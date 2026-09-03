//go:build !linux && !darwin

package paths

import "io/fs"

// OnDisk falls back to the apparent size where the allocated size cannot be
// asked for. The daemon only ships for Linux; this keeps the package building
// anywhere an editor or a cross-compiling CI points at it.
func OnDisk(info fs.FileInfo) int64 { return info.Size() }
