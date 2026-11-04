//go:build linux || darwin

package agent

import "golang.org/x/sys/unix"

// diskFree returns the free bytes of the file system holding path.
func diskFree(path string) uint64 {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0
	}
	return st.Bavail * uint64(st.Bsize) //nolint:gosec,unconvert // block size is positive; its type differs per platform
}
