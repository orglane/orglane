// Package onboarding defines employee steps shared by built-ins and future
// statically compiled provider/module adapters (ADR 0002). No provider is
// enabled by defining this interface.
package onboarding

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/profile"
)

// Viewer carries the same disclosure permissions as profile reads. Sources
// must omit unauthorized steps and never return secret values or metadata.
type Viewer = profile.Viewer

// Result is an authorized current step, not a persisted checkbox. A required
// step must be actionable by the subject; administrator-only tasks are optional
// follow-up for the employee. ActionURL is authorized for the current viewer.
type Result struct {
	ID          string
	Title       string
	Description string
	Required    bool
	Complete    bool
	ActionURL   string
}

// Step is the extension boundary for future employee steps. Check derives
// current completion and action access for this viewer and person. A nil
// result omits the step (for example a feature not enabled in this milestone).
// IDs must be stable and namespaced by provider/module for future adapters.
type Step interface {
	Check(context.Context, Viewer, uuid.UUID) (*Result, error)
}

// CheckFunc adapts a built-in completion check to Step.
type CheckFunc func(context.Context, Viewer, uuid.UUID) (*Result, error)

// Check implements Step by calling the wrapped completion check.
func (f CheckFunc) Check(ctx context.Context, v Viewer, id uuid.UUID) (*Result, error) {
	return f(ctx, v, id)
}

// Registry evaluates its ordered steps on every read; it never caches or stores
// completion. The mobile step is deliberately absent until M4.
type Registry []Step

// Progress returns only authorized, valid employee steps. Action links are
// local app paths, keeping navigation within the existing public app routes.
func (r Registry) Progress(ctx context.Context, v Viewer, id uuid.UUID) ([]Result, error) {
	out := make([]Result, 0, len(r))
	seen := map[string]bool{}
	for _, source := range r {
		step, err := source.Check(ctx, v, id)
		if err != nil {
			return nil, err
		}
		if step == nil {
			continue
		}
		if step.ID == "" || step.Title == "" || seen[step.ID] {
			return nil, fmt.Errorf("onboarding: invalid or duplicate step %q", step.ID)
		}
		seen[step.ID] = true
		if step.ActionURL != "" {
			u, err := url.Parse(step.ActionURL)
			if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(step.ActionURL, "/") || strings.HasPrefix(step.ActionURL, "//") || strings.ContainsAny(step.ActionURL, "\\\r\n") {
				return nil, fmt.Errorf("onboarding: invalid action for %q", step.ID)
			}
		}
		if v.PersonID == id && step.Required && step.ActionURL == "" {
			return nil, fmt.Errorf("onboarding: required step %q has no action", step.ID)
		}
		out = append(out, *step)
	}
	return out, nil
}
