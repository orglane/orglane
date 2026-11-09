-- Durable effect dedupe for webhook deliveries (docs/specs/providers.md
-- "Replay and retention"): the signed body of a delivery that was considered
-- for effects is remembered for the instance's lifetime, so a replay after
-- the delivery row expired (30 days) cannot reapply a connection effect
-- (installation suspended, deleted or restored, hook deleted) or queue a
-- provisioning resync again.
--
-- One small row per delivery whose effects were evaluated, deleted with its
-- instance. Delivery retention never touches this table. A mark means
-- "considered", not "changed": a transition the instance's state did not
-- allow is not retried by a replay either.

-- +goose Up
CREATE TABLE webhook_effect_marks (
    instance_id   uuid NOT NULL REFERENCES app_instances (id) ON DELETE CASCADE,
    body_sha256   bytea NOT NULL CHECK (octet_length(body_sha256) = 32),
    -- What the effect was when it was considered, for operators.
    effect        text NOT NULL CHECK (effect IN ('degraded', 'disconnected', 'restored', 'resync', 'none')),
    code          text NOT NULL DEFAULT '' CHECK (code = '' OR code ~ '^[a-z][a-z0-9_]{0,63}$'),
    considered_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (instance_id, body_sha256),
    CHECK ((effect IN ('degraded', 'disconnected', 'restored')) = (code <> ''))
);

-- +goose Down
DROP TABLE webhook_effect_marks;
