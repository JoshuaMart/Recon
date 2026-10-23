-- What the search language reads that was not stored yet: the last hop's
-- headers, the network enrichment, and your own triage state.

-- +goose Up
ALTER TABLE asset_current
  ADD COLUMN headers jsonb,
  ADD COLUMN asn     int,
  ADD COLUMN asn_org text,
  ADD COLUMN country text;

CREATE TABLE triage (
  asset_id uuid PRIMARY KEY REFERENCES asset (id),
  state    text NOT NULL DEFAULT 'new' CHECK (state IN ('new', 'seen', 'interesting', 'ignored')),
  tags     text[] NOT NULL DEFAULT '{}',
  note     text,
  seen_at  timestamptz
);
CREATE INDEX triage_state ON triage (state);
CREATE INDEX asset_current_first_seen ON asset_current (first_seen DESC, asset_id DESC);

-- +goose Down
DROP INDEX asset_current_first_seen;
DROP TABLE triage;
ALTER TABLE asset_current DROP COLUMN country, DROP COLUMN asn_org, DROP COLUMN asn, DROP COLUMN headers;
