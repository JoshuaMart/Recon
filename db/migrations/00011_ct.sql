-- Certificate Transparency: the lane its names are probed in, the connected
-- periods of the feed, and the wildcard certificates counted per wildcard.

-- +goose Up
-- A host a certificate brought, probed by candidate runs until it is
-- measured or archived, whether verification is on or not.
ALTER TABLE asset_current ADD COLUMN ct_lane boolean NOT NULL DEFAULT false;
CREATE INDEX asset_current_ct_lane ON asset_current (program_id) WHERE ct_lane;

ALTER TABLE wildcard ADD COLUMN ct_wildcards int NOT NULL DEFAULT 0;

-- One row per connected period: a gap between two reads as a gap.
CREATE TABLE ct_session (
  id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  connected_at    timestamptz NOT NULL,
  disconnected_at timestamptz,
  error           text,
  certificates    bigint NOT NULL DEFAULT 0,
  matched         bigint NOT NULL DEFAULT 0,
  created         bigint NOT NULL DEFAULT 0,
  revived         bigint NOT NULL DEFAULT 0,
  overflow        bigint NOT NULL DEFAULT 0,
  -- Matching certificates the writer could not keep up with: a gap too.
  dropped         bigint NOT NULL DEFAULT 0
);

ALTER TABLE run DROP CONSTRAINT run_trigger_check;
ALTER TABLE run ADD CONSTRAINT run_trigger_check CHECK (trigger IN ('user', 'auto_scan', 'schedule', 'ct'));
ALTER TABLE run ADD CONSTRAINT run_candidate_targets CHECK (kind <> 'candidate' OR cardinality(targets) > 0);
CREATE UNIQUE INDEX run_candidate_active ON run (kind) WHERE kind = 'candidate' AND state IN ('queued', 'running');

ALTER TABLE notification DROP CONSTRAINT notification_kind_check;
ALTER TABLE notification ADD CONSTRAINT notification_kind_check
  CHECK (kind IN ('recon_finished', 'saved_search', 'broken', 'recovered', 'ct_live'));

-- +goose Down
ALTER TABLE notification DROP CONSTRAINT notification_kind_check;
ALTER TABLE notification ADD CONSTRAINT notification_kind_check
  CHECK (kind IN ('recon_finished', 'saved_search', 'broken', 'recovered'));
DROP INDEX run_candidate_active;
ALTER TABLE run DROP CONSTRAINT run_candidate_targets;
ALTER TABLE run DROP CONSTRAINT run_trigger_check;
ALTER TABLE run ADD CONSTRAINT run_trigger_check CHECK (trigger IN ('user', 'auto_scan', 'schedule'));
DROP TABLE ct_session;
ALTER TABLE wildcard DROP COLUMN ct_wildcards;
DROP INDEX asset_current_ct_lane;
ALTER TABLE asset_current DROP COLUMN ct_lane;
