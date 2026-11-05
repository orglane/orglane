-- One core-side operation per pushed desired generation (#75): the
-- orglane.apps.agent_apply job of (instance, generation) is the only writer
-- of that row after it was recorded with the desired spec, and finds it by
-- this key. Snapshot and restore operations (#60, M7) are not pushes of a
-- desired spec and are not covered.

-- +goose Up
CREATE UNIQUE INDEX app_operations_generation_key ON app_operations (instance_id, generation)
    WHERE generation IS NOT NULL AND kind IN ('apply', 'upgrade', 'remove');

-- +goose Down
DROP INDEX app_operations_generation_key;
