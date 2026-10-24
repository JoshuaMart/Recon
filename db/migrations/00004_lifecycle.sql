-- Verification: due dates, the per layer verdicts and counters, and the
-- target list a verification run carries.

-- +goose Up
ALTER TABLE asset_current
  ADD COLUMN next_resolve_at timestamptz,
  ADD COLUMN next_full_at    timestamptz,
  -- Position on the current backoff curve, 0 when on the nominal cadence.
  ADD COLUMN backoff_tier    int NOT NULL DEFAULT 0,
  ADD COLUMN lifecycle_since timestamptz,
  ADD COLUMN last_checked_at timestamptz;
UPDATE asset_current SET lifecycle_since = first_seen;
ALTER TABLE asset_current ALTER COLUMN lifecycle_since SET NOT NULL;
CREATE INDEX asset_current_due_resolve ON asset_current (next_resolve_at, asset_id) WHERE next_resolve_at IS NOT NULL;
CREATE INDEX asset_current_due_full ON asset_current (next_full_at, asset_id) WHERE next_full_at IS NOT NULL;

CREATE TABLE asset_layer (
  asset_id                 uuid NOT NULL REFERENCES asset (id),
  layer                    text NOT NULL CHECK (layer IN ('dns', 'tcp', 'http', 'fingerprint')),
  informative_failures     int NOT NULL DEFAULT 0,
  non_informative_failures int NOT NULL DEFAULT 0,
  first_failure_at         timestamptz,
  last_ok_at               timestamptz,
  last_checked_at          timestamptz NOT NULL,
  PRIMARY KEY (asset_id, layer)
);

ALTER TABLE run ADD COLUMN targets text[];
ALTER TABLE run ADD CONSTRAINT run_verification_targets
  CHECK (kind <> 'verification' OR cardinality(targets) > 0);
CREATE UNIQUE INDEX run_verification_active ON run (kind) WHERE kind = 'verification' AND state IN ('queued', 'running');

-- +goose Down
DROP INDEX run_verification_active;
ALTER TABLE run DROP CONSTRAINT run_verification_targets;
ALTER TABLE run DROP COLUMN targets;
DROP TABLE asset_layer;
DROP INDEX asset_current_due_full;
DROP INDEX asset_current_due_resolve;
ALTER TABLE asset_current
  DROP COLUMN last_checked_at,
  DROP COLUMN lifecycle_since,
  DROP COLUMN backoff_tier,
  DROP COLUMN next_full_at,
  DROP COLUMN next_resolve_at;
