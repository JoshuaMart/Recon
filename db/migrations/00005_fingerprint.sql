-- Fingerprinting: the render queue, the technologies a render found, the
-- screenshots, and the render state projected for search.

-- +goose Up
ALTER TABLE asset_current
  ADD COLUMN next_render_at  timestamptz,
  -- 0 low (periodic), 1 normal (first contact, a change), 2 high (you, CT).
  ADD COLUMN render_priority smallint NOT NULL DEFAULT 0 CHECK (render_priority BETWEEN 0 AND 2),
  ADD COLUMN last_render_at  timestamptz,
  ADD COLUMN redirect_hosts  text[],
  ADD COLUMN header_names    text[],
  ADD COLUMN cookie_names    text[],
  ADD COLUMN script_hashes   text[],
  ADD COLUMN favicon_hash    text,
  ADD COLUMN external_hosts  text[],
  ADD COLUMN web_sockets     text[],
  ADD COLUMN has_robots      boolean,
  ADD COLUMN has_sitemap     boolean,
  ADD COLUMN has_llms_txt    boolean,
  ADD COLUMN waf             text;
CREATE INDEX asset_current_render_due ON asset_current (render_priority DESC, next_render_at, asset_id)
  WHERE next_render_at IS NOT NULL;
-- Web services found before renders existed get their first contact now.
UPDATE asset_current SET next_render_at = now(), render_priority = 1
WHERE kind = 'service' AND http_state IS NOT NULL AND scope_status = 'in_scope' AND lifecycle <> 'archived';

CREATE TABLE asset_tech (
  asset_id    uuid NOT NULL REFERENCES asset (id),
  name        text NOT NULL,
  category    text NOT NULL DEFAULT '',
  version_raw text NOT NULL DEFAULT '',
  -- Parsed for comparison, NULL when the raw version does not parse.
  version     int[],
  cpe         text,
  proof       jsonb,
  PRIMARY KEY (asset_id, name)
);
CREATE INDEX asset_tech_name ON asset_tech (lower(name), version);

CREATE TABLE screenshot (
  id        bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  asset_id  uuid NOT NULL REFERENCES asset (id),
  key       text NOT NULL,
  thumb_key text NOT NULL,
  -- 64-bit perceptual (average) hash, compared by Hamming distance.
  phash     bigint NOT NULL,
  bytes     int NOT NULL,
  width     int NOT NULL,
  height    int NOT NULL,
  taken_at  timestamptz NOT NULL
);
CREATE INDEX screenshot_asset ON screenshot (asset_id, taken_at DESC);

-- The last Fingerprinter version seen, so a major bump makes everything due.
CREATE TABLE render_state (
  id               boolean PRIMARY KEY DEFAULT true CHECK (id),
  producer_version text
);
INSERT INTO render_state DEFAULT VALUES;

-- +goose Down
DROP TABLE render_state;
DROP TABLE screenshot;
DROP TABLE asset_tech;
DROP INDEX asset_current_render_due;
ALTER TABLE asset_current
  DROP COLUMN waf, DROP COLUMN has_llms_txt, DROP COLUMN has_sitemap, DROP COLUMN has_robots,
  DROP COLUMN web_sockets, DROP COLUMN external_hosts, DROP COLUMN favicon_hash, DROP COLUMN script_hashes,
  DROP COLUMN cookie_names, DROP COLUMN header_names, DROP COLUMN redirect_hosts, DROP COLUMN last_render_at,
  DROP COLUMN render_priority, DROP COLUMN next_render_at;
