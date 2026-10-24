-- Renders that failed in a row on the target's side, so a dead service backs off.

-- +goose Up
ALTER TABLE asset_current ADD COLUMN render_failures smallint NOT NULL DEFAULT 0 CHECK (render_failures >= 0);

-- +goose Down
ALTER TABLE asset_current DROP COLUMN render_failures;
