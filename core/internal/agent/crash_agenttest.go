//go:build agenttest

package agent

import "os"

// testCrashAt reads ORGLANE_AGENT_TEST_CRASH_AT=<phase> (docs/specs/agent.md
// "Testing"): only builds with the agenttest tag, which also embed the test
// package, have the hook.
func testCrashAt() string { return os.Getenv("ORGLANE_AGENT_TEST_CRASH_AT") }
