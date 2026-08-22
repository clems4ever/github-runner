//go:build linux || darwin

package resources

import "syscall"

// statfs measures the filesystem a path is on.
//
// Used and total are df's "Used" and "1K-blocks" columns exactly: every block
// minus the free ones, against every block. They are not df's Use%, which
// divides by used plus available and so leaves out the reservation ext4 keeps
// for root — a few per cent on a large filesystem. The pair that is displayed
// beside the meter is the pair the meter is drawn from, which matters more here
// than agreeing with df's third number to the point.
func statfs(path string) (total, used int64, err error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, 0, err
	}
	size := int64(fs.Bsize)
	return int64(fs.Blocks) * size, int64(fs.Blocks-fs.Bfree) * size, nil
}
