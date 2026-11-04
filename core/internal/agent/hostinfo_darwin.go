//go:build darwin

package agent

import "golang.org/x/sys/unix"

// memory returns the host's total memory; the available memory is unknown
// on macOS (development only).
func memory() (total, available uint64) {
	total, _ = unix.SysctlUint64("hw.memsize")
	return total, 0
}
