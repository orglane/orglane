//go:build !agenttest

package agent

// testCrashAt is the test crash hook's point; production builds have none.
func testCrashAt() string { return "" }
