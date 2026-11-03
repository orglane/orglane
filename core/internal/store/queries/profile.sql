-- Profile fields and their values (core/migrations/00006_profile_fields.sql).
-- Written only by package profile.

-- name: CreateProfileField :one
INSERT INTO profile_fields (
    key, library, label, type, handle_kind, options, section, visibility,
    required_at_onboarding, editable_by, user_can_hide, sensitive, position
) VALUES (
    @key, @library, @label, @type, sqlc.narg(handle_kind), @options, @section, @visibility,
    @required_at_onboarding, @editable_by, @user_can_hide, @sensitive,
    (SELECT coalesce(max(position), 0) + 1 FROM profile_fields)
)
RETURNING *;

-- name: GetProfileFieldForUpdate :one
SELECT * FROM profile_fields WHERE id = @id FOR UPDATE;

-- name: GetProfileFieldByKeyForUpdate :one
SELECT * FROM profile_fields WHERE key = @key FOR UPDATE;

-- name: ListProfileFields :many
SELECT * FROM profile_fields ORDER BY position, id;

-- name: ListActiveProfileFieldsForShare :many
-- The fields a profile change may touch, locked against concurrent
-- definition changes (sensitivity, options, archiving) until it commits.
SELECT * FROM profile_fields WHERE archived_at IS NULL ORDER BY position, id FOR SHARE;

-- name: UpdateProfileField :one
UPDATE profile_fields
SET label = @label,
    options = @options,
    section = @section,
    visibility = @visibility,
    required_at_onboarding = @required_at_onboarding,
    editable_by = @editable_by,
    user_can_hide = @user_can_hide,
    sensitive = @sensitive,
    position = @position,
    archived_at = sqlc.narg(archived_at),
    updated_at = now()
WHERE id = @id
RETURNING *;

-- name: ListProfileFieldValues :many
SELECT * FROM profile_field_values WHERE person_id = @person_id;

-- name: ListValuesOfFieldForUpdate :many
SELECT * FROM profile_field_values WHERE field_id = @field_id ORDER BY person_id FOR UPDATE;

-- name: PutProfileFieldValue :exec
INSERT INTO profile_field_values (person_id, field_id, value, value_sealed, hidden)
VALUES (@person_id, @field_id, sqlc.narg(value), sqlc.narg(value_sealed), @hidden)
ON CONFLICT (person_id, field_id) DO UPDATE
SET value = EXCLUDED.value, value_sealed = EXCLUDED.value_sealed, hidden = EXCLUDED.hidden, updated_at = now();

-- name: DeleteProfileFieldValue :execrows
DELETE FROM profile_field_values WHERE person_id = @person_id AND field_id = @field_id;
