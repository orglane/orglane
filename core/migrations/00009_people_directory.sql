-- Directory search, reporting lines and public cache invalidation (#25).
-- Migration 00008 is reserved for invitations.
-- +goose Up
CREATE EXTENSION pg_trgm;

ALTER TABLE people ADD COLUMN manager_id uuid REFERENCES people (id),
    ADD COLUMN directory_updated_at timestamptz NOT NULL DEFAULT now(),
    ADD CONSTRAINT people_manager_not_self CHECK (manager_id IS NULL OR manager_id <> id);
CREATE INDEX people_manager_idx ON people (manager_id) WHERE manager_id IS NOT NULL;
CREATE INDEX people_directory_search_idx ON people USING gin
    (lower(display_name || ' ' || username || ' ' || work_email || ' ' || coalesce(title, '')) gin_trgm_ops);
CREATE INDEX people_directory_updated_idx ON people (directory_updated_at, id);

-- Private contact-email changes must never advance the public projection clock.
-- +goose StatementBegin
CREATE FUNCTION directory_public_person_changed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.username, NEW.work_email, NEW.display_name, NEW.title, NEW.manager_id, NEW.status)
       IS DISTINCT FROM
       (OLD.username, OLD.work_email, OLD.display_name, OLD.title, OLD.manager_id, OLD.status) THEN
        NEW.directory_updated_at := clock_timestamp();
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER directory_public_person_changed
    BEFORE UPDATE OF username, work_email, display_name, title, manager_id, status ON people
    FOR EACH ROW EXECUTE FUNCTION directory_public_person_changed();

-- Related public data must invalidate directory pages and incremental caches.
-- +goose StatementBegin
CREATE FUNCTION directory_touch_person() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        UPDATE people SET updated_at = clock_timestamp(), directory_updated_at = clock_timestamp() WHERE id = OLD.person_id;
        RETURN OLD;
    END IF;
    UPDATE people SET updated_at = clock_timestamp(), directory_updated_at = clock_timestamp() WHERE id = NEW.person_id;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER directory_avatar_changed AFTER INSERT OR UPDATE OR DELETE ON person_avatars
    FOR EACH ROW EXECUTE FUNCTION directory_touch_person();
CREATE TRIGGER directory_roles_changed AFTER INSERT OR UPDATE OR DELETE ON person_roles
    FOR EACH ROW EXECUTE FUNCTION directory_touch_person();
CREATE TRIGGER directory_membership_changed AFTER INSERT OR UPDATE OR DELETE ON team_memberships
    FOR EACH ROW EXECUTE FUNCTION directory_touch_person();

-- +goose StatementBegin
CREATE FUNCTION directory_touch_team_members() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE people SET updated_at = clock_timestamp(), directory_updated_at = clock_timestamp()
    WHERE id IN (SELECT person_id FROM team_memberships WHERE team_id = NEW.id);
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER directory_team_changed AFTER UPDATE OF name, slug, archived_at ON teams
    FOR EACH ROW EXECUTE FUNCTION directory_touch_team_members();

-- +goose Down
DROP TRIGGER directory_public_person_changed ON people;
DROP FUNCTION directory_public_person_changed();
DROP TRIGGER directory_team_changed ON teams;
DROP FUNCTION directory_touch_team_members();
DROP TRIGGER directory_membership_changed ON team_memberships;
DROP TRIGGER directory_roles_changed ON person_roles;
DROP TRIGGER directory_avatar_changed ON person_avatars;
DROP FUNCTION directory_touch_person();
DROP INDEX people_directory_updated_idx;
DROP INDEX people_directory_search_idx;
DROP INDEX people_manager_idx;
ALTER TABLE people DROP CONSTRAINT people_manager_not_self, DROP COLUMN manager_id, DROP COLUMN directory_updated_at;
DROP EXTENSION pg_trgm;
