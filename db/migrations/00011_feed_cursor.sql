-- The index the live feed walks.
--
-- The feed orders on (first_seen, asset_id) and nothing else in the system
-- does: the list and its cursor order on last_seen, and the due date passes
-- order on their own columns. Without this the feed is a scan of the tenant on
-- every tick, which is the one shape a polling loop must not have.
--
-- first_seen is mutable and that is exactly what makes this cursor safe. The
-- upsert writes LEAST(old, new), so the column can only move backwards, and an
-- asset moving backwards becomes older than a cursor already passed: it is
-- neither re-emitted nor missed. The day something makes first_seen move
-- forward, this feed starts skipping discoveries in silence.

-- +goose Up

CREATE INDEX asset_current_arrival_idx ON asset_current (org_id, first_seen, asset_id);

-- +goose Down

DROP INDEX IF EXISTS asset_current_arrival_idx;
