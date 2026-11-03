-- name: OnboardingWelcomePending :one
SELECT EXISTS (SELECT 1 FROM invitations i WHERE i.person_id = @person_id AND accepted_at IS NOT NULL) AS invited,
       EXISTS (SELECT 1 FROM person_onboarding o WHERE o.person_id = @person_id) AS finished;

-- name: FinishOnboardingWelcome :execrows
INSERT INTO person_onboarding (person_id) VALUES (@person_id)
ON CONFLICT (person_id) DO NOTHING;

-- name: OnboardingAccountCompletion :one
SELECT EXISTS (SELECT 1 FROM person_avatars a WHERE a.person_id = @person_id) AS avatar,
       EXISTS (SELECT 1 FROM passkeys p WHERE p.person_id = @person_id) AS passkey;
