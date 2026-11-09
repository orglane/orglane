-- The app routes of the last Traefik configuration the core served
-- completely (docs/specs/providers.md "Route publication"). The process
-- that answers Traefik's poll records them; the setup job, which may run
-- in another core process, activates an installed instance only once its
-- route has been served unchanged for the settle time.
--
-- One row per routed instance key. A poll whose routes are unchanged
-- writes nothing; served_since is when the route was first served in its
-- current form.

-- +goose Up
CREATE TABLE ingress_served_routes (
    instance_key text PRIMARY KEY,
    host         text NOT NULL,
    upstream     text NOT NULL,
    maintenance  boolean NOT NULL,
    served_since timestamptz NOT NULL
);

-- +goose Down
DROP TABLE ingress_served_routes;
