-- One managed instance at a time (docs/specs/agent.md "Upgrade"): a bad
-- release reaches the next instance only after the first one has settled
-- (rollback included), never in parallel with it. The planner and the admin
-- retry check the same invariant under the instance lock; this index is the
-- database-level guard that also holds when two syncs or a retry race.
--
-- Only managed instances are covered: upgrading is the upgrade flow's
-- state (connected instances never enter it), and the partial index leaves
-- manual transitions of other management kinds untouched.

-- +goose Up
CREATE UNIQUE INDEX app_instances_upgrading_key ON app_instances ((management))
    WHERE state = 'upgrading' AND management = 'managed';

-- +goose Down
DROP INDEX app_instances_upgrading_key;
