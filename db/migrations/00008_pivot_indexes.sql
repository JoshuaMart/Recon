-- The pivot counts of the asset view: array containment and favicon equality.

-- +goose Up
CREATE INDEX asset_current_script_hashes ON asset_current USING gin (script_hashes);
CREATE INDEX asset_current_cookie_names ON asset_current USING gin (cookie_names);
CREATE INDEX asset_current_favicon_hash ON asset_current (favicon_hash) WHERE favicon_hash IS NOT NULL;

-- +goose Down
DROP INDEX asset_current_favicon_hash;
DROP INDEX asset_current_cookie_names;
DROP INDEX asset_current_script_hashes;
