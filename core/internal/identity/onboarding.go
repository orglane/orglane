package identity

import (
	"context"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/store"
)

// WelcomePending is true only for accepted invitations whose welcome was not
// finished. Resending/accepting invites cannot reset a finished welcome.
func (s *Service) WelcomePending(ctx context.Context, personID uuid.UUID) (bool, error) {
	row, err := s.queries().OnboardingWelcomePending(ctx, personID)
	return row.Invited && !row.Finished, err
}

// OnboardingAccountCompletion derives optional steps without loading avatar
// bytes, passkey credentials or depending on passkey-offer dismissal.
func (s *Service) OnboardingAccountCompletion(ctx context.Context, personID uuid.UUID) (avatar, passkey bool, err error) {
	row, err := s.queries().OnboardingAccountCompletion(ctx, personID)
	return row.Avatar, row.Passkey, err
}

// FinishWelcome records leaving the welcome flow once, atomically with its
// audit. The API checks current authorized required steps before calling it.
func (s *Service) FinishWelcome(ctx context.Context, actor audit.Actor, personID uuid.UUID) error {
	return s.inTx(ctx, func(q *store.Queries) error {
		if _, err := q.GetPersonForUpdate(ctx, personID); err != nil {
			return notFound(err, "person")
		}
		n, err := q.FinishOnboardingWelcome(ctx, personID)
		if err != nil || n == 0 {
			return err
		}
		_, err = audit.Record(ctx, q, audit.Entry{Actor: actor, Action: audit.OnboardingWelcomeFinished, Target: personTarget(personID)})
		return err
	})
}
