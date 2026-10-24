-- The key lookups of screenshot serving and garbage collection.

-- +goose Up
CREATE INDEX screenshot_key ON screenshot (key);
CREATE INDEX screenshot_thumb_key ON screenshot (thumb_key);

-- +goose Down
DROP INDEX screenshot_thumb_key;
DROP INDEX screenshot_key;
