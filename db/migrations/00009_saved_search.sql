-- Saved searches: your settings, not inventory, so they can be deleted. The
-- alert flag is read by the notifier.

-- +goose Up
CREATE TABLE saved_search (
  id         uuid PRIMARY KEY DEFAULT uuidv7(),
  name       text NOT NULL UNIQUE CHECK (name <> ''),
  query      text NOT NULL,
  view       text NOT NULL DEFAULT 'services' CHECK (view IN ('services', 'hosts')),
  alert      boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL,
  updated_at timestamptz NOT NULL
);

CREATE INDEX change_feed ON change (at DESC, id DESC);

-- +goose Down
DROP INDEX change_feed;
DROP TABLE saved_search;
