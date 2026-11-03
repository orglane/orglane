-- +goose Up
-- Only welcome dismissal is stored; step completion is always derived.
CREATE TABLE person_onboarding (
    person_id uuid PRIMARY KEY REFERENCES people(id) ON DELETE CASCADE,
    welcome_finished_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE person_onboarding;
