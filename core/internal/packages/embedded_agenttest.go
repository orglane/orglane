//go:build agenttest

package packages

import agenttest "github.com/orglane/orglane/providers/agenttest/managed"

func init() { sources = append(sources, agenttest.FS) }
