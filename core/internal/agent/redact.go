package agent

import (
	"bytes"
	"cmp"
	"slices"
)

// redacted replaces secret values in logs and command output.
const redacted = "[redacted]"

// redact replaces every secret value in b with [redacted], longest first so
// a secret that contains another is replaced whole.
func redact(b []byte, secrets []string) []byte {
	s := slices.Clone(secrets)
	slices.SortFunc(s, func(x, y string) int { return cmp.Compare(len(y), len(x)) })
	for _, v := range s {
		if v != "" {
			b = bytes.ReplaceAll(b, []byte(v), []byte(redacted))
		}
	}
	return b
}
