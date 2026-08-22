//go:build !linux && !darwin

package resources

import "errors"

// statfs has no answer on a platform without it. The daemon only ships for
// Linux; this exists so that the package still builds for anyone whose editor
// or CI crosses it against something else, and it degrades to one missing
// number rather than a compile error.
func statfs(string) (total, used int64, err error) {
	return 0, 0, errors.New("disk usage is not available on this platform")
}
