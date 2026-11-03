-- Profile fields (ADR 0015, #24): admin-defined fields of a person's
-- profile, next to the fixed base fields on people, and their values.
--
-- A field comes from the library of ready-made fields (library = true; its
-- key is the library key and clients translate its label by key) or is a
-- custom field defined by an admin. Fields are never deleted, only archived:
-- archived fields and their values are hidden everywhere and come back when
-- the field is restored.
--
-- Values of sensitive fields are encrypted at rest (value_sealed: AES-256-GCM
-- under a key derived from the instance master secret, bound to the person
-- and the field); all other values are plain text (value). Exactly one of
-- the two is set. Who may read and change a value is decided in one place,
-- package profile, which also writes the audit entries (changes, and every
-- read of a sensitive value) in the same transaction.

-- +goose Up
CREATE TABLE profile_fields (
    id                     uuid PRIMARY KEY DEFAULT uuidv7(),
    key                    text NOT NULL UNIQUE CHECK (key ~ '^[a-z][a-z0-9_]{0,62}$'),
    library                boolean NOT NULL DEFAULT false,
    -- English label; library fields keep the library's label.
    label                  text NOT NULL CHECK (btrim(label) <> '' AND length(label) <= 100),
    type                   text NOT NULL CHECK (type IN ('text', 'long_text', 'date', 'select', 'phone', 'url', 'handle')),
    -- Which service a handle belongs to; set exactly for handles.
    handle_kind            text CHECK (handle_kind IN ('telegram', 'github', 'gitlab', 'x', 'mastodon')),
    -- Choices of a select field: [{"key": "...", "label": "..."}, ...].
    options                jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(options) = 'array'),
    section                text NOT NULL CHECK (section IN ('contacts', 'personal', 'hr', 'custom')),
    visibility             text NOT NULL CHECK (visibility IN ('everyone', 'team', 'admins')),
    required_at_onboarding boolean NOT NULL DEFAULT false,
    editable_by            text NOT NULL CHECK (editable_by IN ('self', 'admins')),
    user_can_hide          boolean NOT NULL DEFAULT false,
    sensitive              boolean NOT NULL DEFAULT false,
    position               integer NOT NULL DEFAULT 0,
    archived_at            timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CHECK ((type = 'handle') = (handle_kind IS NOT NULL)),
    CHECK (type = 'select' OR options = '[]'::jsonb)
);

CREATE TABLE profile_field_values (
    person_id    uuid NOT NULL REFERENCES people (id) ON DELETE CASCADE,
    field_id     uuid NOT NULL REFERENCES profile_fields (id) ON DELETE RESTRICT,
    value        text CHECK (value IS NULL OR (value <> '' AND length(value) <= 2000)),
    value_sealed bytea CHECK (value_sealed IS NULL OR octet_length(value_sealed) <= 8192),
    -- The person hid the value from everyone but themselves and admins
    -- (only for fields with user_can_hide).
    hidden       boolean NOT NULL DEFAULT false,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (person_id, field_id),
    CHECK ((value IS NULL) <> (value_sealed IS NULL))
);

CREATE INDEX profile_field_values_field_id_idx ON profile_field_values (field_id);

-- +goose Down
DROP TABLE profile_field_values;
DROP TABLE profile_fields;
