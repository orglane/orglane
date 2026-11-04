//go:build !linux && !darwin

package agent

// memory is unknown on this platform (the agent runs on Linux; macOS is
// supported for development).
func memory() (total, available uint64) { return 0, 0 }

// diskFree is unknown on this platform.
func diskFree(string) uint64 { return 0 }
