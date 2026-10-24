-- Notifications: alert matches, the outbox with its windows, and the health
-- state of each component.

-- +goose Up
-- NULL until the alert's first evaluation, which records the matches silently.
ALTER TABLE saved_search ADD COLUMN evaluated_at timestamptz;

CREATE TABLE saved_search_match (
  saved_search_id uuid NOT NULL REFERENCES saved_search (id) ON DELETE CASCADE,
  asset_id        uuid NOT NULL REFERENCES asset (id),
  since           timestamptz NOT NULL,
  PRIMARY KEY (saved_search_id, asset_id)
);

-- One message per window: items are appended while it is not due.
CREATE TABLE notification (
  id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  kind       text NOT NULL CHECK (kind IN ('recon_finished', 'saved_search', 'broken', 'recovered')),
  group_key  text NOT NULL,
  title      text NOT NULL,
  items      text[] NOT NULL DEFAULT '{}',
  total      int NOT NULL DEFAULT 0,
  link       text,
  created_at timestamptz NOT NULL,
  due_at     timestamptz NOT NULL,
  sent_at    timestamptz,
  -- sent: delivered; no_channel: none configured; failed: gave up.
  outcome    text CHECK (outcome IN ('sent', 'no_channel', 'failed')),
  attempts   int NOT NULL DEFAULT 0,
  -- The channels that have it: a retry goes to the others only.
  delivered  text[] NOT NULL DEFAULT '{}',
  last_error text
);
CREATE INDEX notification_open ON notification (group_key, due_at) WHERE sent_at IS NULL;
CREATE INDEX notification_due ON notification (due_at) WHERE sent_at IS NULL;
CREATE INDEX notification_recent ON notification (created_at DESC);

CREATE TABLE health_state (
  component     text PRIMARY KEY,
  failing_since timestamptz,
  announced     boolean NOT NULL DEFAULT false,
  detail        text
);

-- +goose Down
DROP TABLE health_state;
DROP TABLE notification;
DROP TABLE saved_search_match;
ALTER TABLE saved_search DROP COLUMN evaluated_at;
