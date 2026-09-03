//go:build linux || darwin

package paths

import (
	"io/fs"
	"syscall"
)

// OnDisk is what a file actually occupies, which for a sparse qcow2 is a small
// fraction of the size it reports.
//
// A golden image is created twenty gigabytes wide and allocates only what has
// been written to it. Measuring the apparent size would put a fresh host over
// any sensible ceiling before it had built its second image.
func OnDisk(info fs.FileInfo) int64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Blocks * 512
	}
	return info.Size()
}
