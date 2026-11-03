package profile

import (
	"context"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/store"
)

// OnboardingCompletion contains no field metadata or values. EmployeeComplete
// covers only required fields the employee can see and edit. AdminComplete
// covers visible required fields that need an administrator, so these cannot
// trap the employee in welcome. All booleans are relative to the viewer.
type OnboardingCompletion struct {
	EmployeeComplete bool
	AdminComplete    bool
	HasAdminFields   bool
}

// RequiredCompletion uses the exact profile disclosure and sensitive-read audit
// path. It deliberately excludes fields the viewer cannot see, even from counts.
func (s *Service) RequiredCompletion(ctx context.Context, actor audit.Actor, viewer Viewer, personID uuid.UUID) (OnboardingCompletion, error) {
	entries, err := s.Profile(ctx, actor, viewer, personID)
	if err != nil {
		return OnboardingCompletion{}, err
	}
	permissions, err := store.New(s.pool).ListPersonPermissions(ctx, personID)
	if err != nil {
		return OnboardingCompletion{}, err
	}
	subject := Viewer{PersonID: personID}
	for _, p := range permissions {
		if p == string(authz.PeopleManage) {
			subject.ManagePeople = true
		}
		if p == string(authz.PeopleSensitiveRead) {
			subject.ReadSensitive = true
		}
	}
	result := OnboardingCompletion{EmployeeComplete: true, AdminComplete: true}
	for _, entry := range entries {
		if !entry.Field.RequiredAtOnboarding {
			continue
		}
		if decide(entry.Field, subject, relation{self: true}).edit {
			if entry.Value == "" {
				result.EmployeeComplete = false
			}
		} else {
			result.HasAdminFields = true
			if entry.Value == "" {
				result.AdminComplete = false
			}
		}
	}
	return result, nil
}
