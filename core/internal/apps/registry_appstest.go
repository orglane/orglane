//go:build appstest

package apps

import "github.com/orglane/orglane/providers/testprovider"

// Only compiled into the UI acceptance binary (web/e2e), never production:
// the in-repo test provider lets Settings → Apps be exercised end to end.
func init() {
	compiled = append(compiled, testprovider.New())
}
