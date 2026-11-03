-- +goose Up
-- Only welcome dismissal is stored; step completion is always derived.
CREATE TABLE person_onboarding (
    person_id uuid PRIMARY KEY REFERENCES people(id) ON DELETE CASCADE,
    welcome_finished_at timestamptz NOT NULL DEFAULT now()
);

-- People who accepted before this feature was installed have already joined
-- the workspace. Only invitations accepted after the upgrade start welcome.
INSERT INTO person_onboarding (person_id)
SELECT DISTINCT person_id
FROM invitations
WHERE accepted_at IS NOT NULL;

-- +goose Down
DROP TABLE person_onboarding;
