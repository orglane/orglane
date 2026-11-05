package apps

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
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

// OpenURL returns the deep link that opens target in an app instance for
// a person (docs/specs/providers.md "Deep links and silent SSO"). target is
// validated here (ValidateTarget); the adapter's result must stay on the
// instance's base URL origin (CheckOpenURL), else it is an adapter bug.
// The person must be active (ErrForbidden) and the instance active or
// degraded (ErrInvalidState); instances whose provider has no deep links
// are not found.
func (s *Service) OpenURL(ctx context.Context, personID, instanceID uuid.UUID, target string) (*url.URL, error) {
	if err := ValidateTarget(target); err != nil {
		return nil, invalid("to", "must be a relative path starting with one /, without spaces, backslashes or control characters")
	}
	q := store.New(s.pool)
	p, err := q.GetPerson(ctx, personID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && p.Status != "active") {
		return nil, ErrForbidden
	} else if err != nil {
		return nil, fmt.Errorf("apps: load person: %w", err)
	}
	inst, err := s.loadInstance(ctx, q, instanceID, false)
	if err != nil {
		return nil, err
	}
	var dl provider.DeepLinker
	if inst.Adapter != nil {
		dl, _ = inst.Adapter.DeepLinker()
	}
	view := inst.View()
	if dl == nil || view.BaseURL == nil {
		return nil, fmt.Errorf("%w: deep links of this app", ErrNotFound)
	}
	if !inst.Provisionable() {
		return nil, ErrInvalidState
	}
	u, err := dl.OpenURL(view, target)
	if err != nil {
		return nil, fmt.Errorf("apps: deep link: %w", err)
	}
	if err := CheckOpenURL(view.BaseURL, u); err != nil {
		s.cfg.Logger.ErrorContext(ctx, "deep link to another origin refused", "provider", inst.Provider, "instance", inst.ID)
		return nil, err
	}
	return u, nil
}
