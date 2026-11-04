package apps

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/onboarding"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// EmployeeSteps returns one onboarding step per step the compiled adapters
// declare (docs/specs/providers.md "Employee steps"). A step is shown only
// to people with desired access to an instance of its provider; it is
// complete once a verified link exists in the instance's realm. Provider
// steps are never required for finishing welcome.
func (s *Service) EmployeeSteps() onboarding.Registry {
	var reg onboarding.Registry
	for _, a := range s.reg.Adapters() {
		for _, spec := range a.Manifest.EmployeeSteps {
			reg = append(reg, onboarding.CheckFunc(func(ctx context.Context, v onboarding.Viewer, personID uuid.UUID) (*onboarding.Result, error) {
				return s.checkStep(ctx, a, spec, v, personID)
			}))
		}
	}
	return reg
}

func (s *Service) checkStep(ctx context.Context, a *Adapter, spec provider.EmployeeStepSpec, v onboarding.Viewer, personID uuid.UUID) (*onboarding.Result, error) {
	if v.PersonID != personID && !v.ManagePeople {
		return nil, nil // only the person and people managers see app steps
	}
	q := store.New(s.pool)
	rows, err := q.ListAppInstancesInState(ctx, provisionable)
	if err != nil {
		return nil, fmt.Errorf("apps: list instances: %w", err)
	}
	relevant, complete := false, false
	for _, row := range rows {
		if row.Provider != string(a.Manifest.ID) {
			continue
		}
		inst, err := s.instance(row)
		if err != nil {
			return nil, err
		}
		d, err := s.computeDesired(ctx, q, inst, personID)
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		} else if err != nil {
			return nil, err
		}
		if !d.access.Access {
			continue
		}
		relevant = true
		_, err = q.GetVerifiedAccountLink(ctx, store.GetVerifiedAccountLinkParams{PersonID: personID, Provider: inst.Provider, Realm: a.Realm(inst.View())})
		switch {
		case err == nil:
			complete = true
		case !errors.Is(err, pgx.ErrNoRows):
			return nil, fmt.Errorf("apps: load link: %w", err)
		}
	}
	if !relevant {
		return nil, nil
	}
	r := &onboarding.Result{ID: spec.ID, Title: spec.Title, Description: spec.Description, Required: false, Complete: complete}
	if v.PersonID == personID {
		r.ActionURL = spec.ActionPath
	}
	return r, nil
}
