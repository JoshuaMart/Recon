-- The index the live feed walks. The feed orders on (first_seen, asset_id) and nothing else
-- does (the list and its cursor use last_seen, the due-date passes their own columns).
-- Without it the feed scans the tenant every tick, the one shape a polling loop must not have.
--
-- first_seen is mutable, which is what makes the cursor safe: the upsert writes LEAST(old,
-- new), so it only moves backwards, and an asset doing so becomes older than a cursor already
-- passed, neither re-emitted nor missed. The day something moves first_seen forward, the feed
-- starts skipping discoveries silently.

-- +goose Up

CREATE INDEX asset_current_arrival_idx ON asset_current (org_id, first_seen, asset_id);

-- +goose Down

DROP INDEX IF EXISTS asset_current_arrival_idx;
