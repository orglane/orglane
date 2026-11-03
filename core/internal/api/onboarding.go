package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/api/apigen"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/onboarding"
	"github.com/orglane/orglane/core/internal/profile"
)

func (s *server) onboardingProgress(ctx context.Context, personID uuid.UUID) (apigen.OnboardingProgress, error) {
	svc, err := s.profiles()
	if err != nil {
		return apigen.OnboardingProgress{}, err
	}
	viewer, actor, err := s.viewer(ctx)
	if err != nil {
		return apigen.OnboardingProgress{}, err
	}
	a, err := s.auth()
	if err != nil {
		return apigen.OnboardingProgress{}, err
	}
	person, err := a.Identity().GetPerson(ctx, personID)
	if errors.Is(err, identity.ErrNotFound) || (err == nil && person.Status == string(identity.StatusOffboarded)) {
		return apigen.OnboardingProgress{}, problemErr(http.StatusNotFound, "")
	}
	if err != nil {
		return apigen.OnboardingProgress{}, err
	}
	completion, err := svc.RequiredCompletion(ctx, actor, viewer, personID)
	if err != nil {
		return apigen.OnboardingProgress{}, profileError(err)
	}
	avatar, passkey, err := a.Identity().OnboardingAccountCompletion(ctx, personID)
	if err != nil {
		return apigen.OnboardingProgress{}, err
	}
	profileURL := "/people/" + personID.String()
	registry := onboarding.Registry{
		onboarding.CheckFunc(func(context.Context, profile.Viewer, uuid.UUID) (*onboarding.Result, error) {
			return &onboarding.Result{ID: "profile", Title: "Complete your profile", Description: "Fill in the required profile fields you can edit.", Required: true, Complete: completion.EmployeeComplete, ActionURL: profileURL}, nil
		}),
		onboarding.CheckFunc(func(context.Context, profile.Viewer, uuid.UUID) (*onboarding.Result, error) {
			if !completion.HasAdminFields {
				return nil, nil
			}
			action := ""
			if viewer.ManagePeople {
				action = profileURL
			}
			return &onboarding.Result{ID: "profile-admin", Title: "Administrator follow-up", Description: "Some required profile information needs an administrator. You can continue welcome.", Complete: completion.AdminComplete, ActionURL: action}, nil
		}),
		onboarding.CheckFunc(func(context.Context, profile.Viewer, uuid.UUID) (*onboarding.Result, error) {
			return &onboarding.Result{ID: "avatar", Title: "Upload a photo", Description: "Help your teammates recognize you.", Complete: avatar, ActionURL: profileURL}, nil
		}),
		onboarding.CheckFunc(func(context.Context, profile.Viewer, uuid.UUID) (*onboarding.Result, error) {
			action := ""
			if viewer.PersonID == personID {
				action = "/settings/security"
			}
			return &onboarding.Result{ID: "passkey", Title: "Add a passkey", Description: "Sign in with your fingerprint, face or screen lock. You can do this later.", Complete: passkey, ActionURL: action}, nil
		}),
	}
	registry = append(registry, s.opts.EmployeeSteps...)
	steps, err := registry.Progress(ctx, viewer, personID)
	if err != nil {
		return apigen.OnboardingProgress{}, err
	}
	progress := apigen.OnboardingProgress{PersonId: personID, Steps: make([]apigen.OnboardingStep, 0, len(steps))}
	// Welcome status is personal. Exposing it to admins could reveal whether a
	// required field visible only to the person was completed during welcome.
	if viewer.PersonID == personID {
		progress.WelcomePending, err = a.Identity().WelcomePending(ctx, personID)
		if err != nil {
			return apigen.OnboardingProgress{}, err
		}
	}
	for _, step := range steps {
		item := apigen.OnboardingStep{Id: step.ID, Title: step.Title, Description: step.Description, Required: step.Required, Complete: step.Complete}
		if step.ActionURL != "" {
			item.ActionUrl = &step.ActionURL
		}
		progress.Steps = append(progress.Steps, item)
	}
	return progress, nil
}

func (s *server) GetMyOnboarding(ctx context.Context, _ apigen.GetMyOnboardingRequestObject) (apigen.GetMyOnboardingResponseObject, error) {
	id, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	progress, err := s.onboardingProgress(ctx, id)
	if err != nil {
		return nil, err
	}
	return apigen.GetMyOnboarding200JSONResponse(progress), nil
}

func (s *server) GetPersonOnboarding(ctx context.Context, req apigen.GetPersonOnboardingRequestObject) (apigen.GetPersonOnboardingResponseObject, error) {
	progress, err := s.onboardingProgress(ctx, req.PersonId)
	if err != nil {
		return nil, err
	}
	return apigen.GetPersonOnboarding200JSONResponse(progress), nil
}

func (s *server) FinishMyOnboarding(ctx context.Context, _ apigen.FinishMyOnboardingRequestObject) (apigen.FinishMyOnboardingResponseObject, error) {
	id, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	pending, err := a.Identity().WelcomePending(ctx, id)
	if err != nil {
		return nil, err
	}
	if !pending {
		return apigen.FinishMyOnboarding204Response{}, nil
	}
	progress, err := s.onboardingProgress(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, step := range progress.Steps {
		if step.Required && !step.Complete {
			return nil, problemErr(http.StatusConflict, "Complete your required profile fields before continuing.")
		}
	}
	_, actor, err := s.viewer(ctx)
	if err != nil {
		return nil, err
	}
	if err := a.Identity().FinishWelcome(ctx, actor, id); err != nil {
		return nil, err
	}
	return apigen.FinishMyOnboarding204Response{}, nil
}
