package apps

import (
	"errors"
	"net/url"
	"strings"
)

// MaxTargetLength bounds deep link targets.
const MaxTargetLength = 2048

// ErrInvalidTarget is returned for a deep link target that is not a safe
// relative path.
var ErrInvalidTarget = errors.New("apps: invalid deep link target")

// ValidateTarget checks a deep link target (`to`) from a client
// (docs/specs/providers.md "Deep links"): 1-2048 bytes, starts with one
// "/", no backslash, ASCII control characters or spaces, and parses as a
// URL without scheme, user info or host. Query and fragment are kept. The
// core never accepts an absolute URL from a client.
func ValidateTarget(to string) error {
	if to == "" || len(to) > MaxTargetLength || !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") {
		return ErrInvalidTarget
	}
	for i := 0; i < len(to); i++ {
		c := to[i]
		if c < 0x21 || c == 0x7f || c == '\\' {
			return ErrInvalidTarget
		}
	}
	u, err := url.Parse(to)
	if err != nil || u.Scheme != "" || u.User != nil || u.Host != "" || u.Opaque != "" {
		return ErrInvalidTarget
	}
	return nil
}

// CheckOpenURL checks an adapter's deep link: its scheme and host must
// equal the instance base URL's.
func CheckOpenURL(base, got *url.URL) error {
	if base == nil || got == nil || got.Scheme != base.Scheme || !strings.EqualFold(got.Host, base.Host) || got.User != nil {
		return errors.New("apps: adapter returned a link to another origin")
	}
	return nil
}
