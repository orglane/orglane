-- The app routes the core has served Traefik (docs/specs/providers.md
-- "Route publication"). The process that answers Traefik's poll records
-- them; the setup job, which may run in another core process, activates
-- an installed instance only once its route has been served unchanged for
-- the settle time.
--
-- Recording fails closed. Before an answer is written, every row that is
-- not in it identically is deleted and the serve sequence advances; if
-- that fails, the answer is a 503 and Traefik keeps the configuration the
-- remaining rows describe. Once the answer was flushed, its new routes are
-- inserted only if no later serve has started since (same sequence), so a
-- late write never brings back a route a newer configuration dropped.
-- served_since is when the route was first served in its current form.

-- +goose Up
CREATE TABLE ingress_served_routes (
    instance_key text PRIMARY KEY,
    host         text NOT NULL,
    upstream     text NOT NULL,
    maintenance  boolean NOT NULL,
    served_since timestamptz NOT NULL
);

-- One row: the sequence number of the last serve that began.
CREATE TABLE ingress_serve_state (
    id  boolean PRIMARY KEY DEFAULT true CHECK (id),
    seq bigint NOT NULL
);
INSERT INTO ingress_serve_state (seq) VALUES (0);

-- +goose Down
DROP TABLE ingress_serve_state;
DROP TABLE ingress_served_routes;
