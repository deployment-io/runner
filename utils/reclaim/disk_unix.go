//go:build !windows

package reclaim

import "syscall"

// freeDiskBytes returns the space available to an unprivileged writer on the
// filesystem holding path. Reports ok=false when the path can't be statted —
// the caller then logs nothing rather than logging a zero.
//
// Bavail (not Bfree) is the right field: it excludes the reserved blocks only
// root may use, which is what a build actually gets to write into.
func freeDiskBytes(path string) (uint64, bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, false
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), true
}
