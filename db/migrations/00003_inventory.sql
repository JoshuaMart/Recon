-- The inventory: assets, their observations and changes, their current state,
-- and the runs that produce them.

-- +goose Up
CREATE TABLE run (
  id           uuid PRIMARY KEY DEFAULT uuidv7(),
  program_id   uuid NOT NULL REFERENCES program (id),
  kind         text NOT NULL CHECK (kind IN ('recon', 'verification', 'candidate')),
  stage        text NOT NULL CHECK (stage IN ('enum', 'resolve', 'ports', 'full')),
  wildcard_id  uuid REFERENCES wildcard (id),
  state        text NOT NULL DEFAULT 'queued'
               CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'timed_out')),
  -- Why it was queued: you, auto-scan, the scheduler.
  trigger      text NOT NULL CHECK (trigger IN ('user', 'auto_scan', 'schedule')),
  queued_at    timestamptz NOT NULL,
  started_at   timestamptz,
  finished_at  timestamptz,
  deadline     timestamptz,
  error        text,
  -- FastRecon's own run section, sources, stats, warnings and degraded codes.
  report_meta  jsonb,
  -- What ingestion did with the report.
  ingest       jsonb,
  CHECK (kind <> 'recon' OR wildcard_id IS NOT NULL)
);
CREATE INDEX run_state ON run (state, queued_at);
CREATE INDEX run_program ON run (program_id, queued_at DESC);
-- One recon queued or running per wildcard.
CREATE UNIQUE INDEX run_wildcard_active ON run (wildcard_id) WHERE state IN ('queued', 'running');

ALTER TABLE wildcard ADD FOREIGN KEY (last_recon_run) REFERENCES run (id);

CREATE TABLE asset (
  id               uuid PRIMARY KEY DEFAULT uuidv7(),
  program_id       uuid NOT NULL REFERENCES program (id),
  kind             text NOT NULL CHECK (kind IN ('fqdn', 'ip', 'service', 'url')),
  key              text NOT NULL,
  -- Derived from the key at creation, never from an observation.
  host             text NOT NULL,
  port             int,
  scheme           text,
  parent_asset_id  uuid REFERENCES asset (id),
  -- The wildcard whose recon first found it, for the coverage counts.
  wildcard_id      uuid REFERENCES wildcard (id),
  discovery_source text NOT NULL CHECK (discovery_source IN ('fastrecon', 'certstream', 'bbot', 'scopesextractor', 'manual')),
  discovery_path   jsonb NOT NULL DEFAULT '[]',
  scope_status     text NOT NULL CHECK (scope_status IN ('in_scope', 'out_of_scope', 'unknown')),
  first_seen       timestamptz NOT NULL,
  last_seen        timestamptz NOT NULL,
  UNIQUE (program_id, kind, key)
);
CREATE INDEX asset_parent ON asset (parent_asset_id);
CREATE INDEX asset_wildcard ON asset (wildcard_id);
CREATE INDEX asset_host ON asset (program_id, host);

-- Immutable journal, deduplicated on write: a row is a state that held from
-- observed_at to last_confirmed_at. No DEFAULT partition, so a missing month
-- fails loudly instead of being absorbed.
CREATE TABLE observation (
  id                    bigint GENERATED ALWAYS AS IDENTITY,
  asset_id              uuid NOT NULL REFERENCES asset (id),
  layer                 text NOT NULL CHECK (layer IN ('dns', 'tcp', 'http', 'fingerprint')),
  outcome               text NOT NULL CHECK (outcome IN ('ok', 'fail', 'error')),
  data                  jsonb NOT NULL,
  observed_at           timestamptz NOT NULL,
  last_confirmed_at     timestamptz NOT NULL,
  run_id                uuid REFERENCES run (id),
  producer_version      text,
  last_producer_version text,
  PRIMARY KEY (id, observed_at)
) PARTITION BY RANGE (observed_at);
CREATE INDEX observation_last ON observation (asset_id, layer, observed_at DESC);

CREATE TABLE change (
  id         bigint GENERATED ALWAYS AS IDENTITY,
  asset_id   uuid NOT NULL REFERENCES asset (id),
  program_id uuid NOT NULL REFERENCES program (id),
  kind       text NOT NULL,
  layer      text,
  before     jsonb,
  after      jsonb,
  run_id     uuid REFERENCES run (id),
  at         timestamptz NOT NULL,
  PRIMARY KEY (id, at)
) PARTITION BY RANGE (at);
CREATE INDEX change_asset ON change (asset_id, at DESC);
CREATE INDEX change_program ON change (program_id, at DESC);
CREATE INDEX change_run ON change (run_id);

-- One partition per month for a partitioned table, named <table>_yYYYYmMM.
-- +goose StatementBegin
CREATE FUNCTION ensure_month_partition(parent text, month date) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
  start date := date_trunc('month', month)::date;
  name  text := format('%s_y%sm%s', parent, to_char(start, 'YYYY'), to_char(start, 'MM'));
BEGIN
  EXECUTE format('CREATE TABLE IF NOT EXISTS %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
                 name, parent, start, (start + interval '1 month')::date);
END $$;
-- +goose StatementEnd

-- The current state, one row per asset, flattened for search. Columns join it
-- as their phase adds them.
CREATE TABLE asset_current (
  asset_id        uuid PRIMARY KEY REFERENCES asset (id),
  program_id      uuid NOT NULL REFERENCES program (id),
  kind            text NOT NULL,
  key             text NOT NULL,
  host            text NOT NULL,
  port            int,
  scheme          text,
  scope_status    text NOT NULL,
  lifecycle       text NOT NULL CHECK (lifecycle IN ('candidate', 'active', 'flapping', 'inactive', 'unobservable', 'archived')),
  dns_state       text,
  tcp_state       text,
  http_state      text,
  ips             inet[],
  cname           text[],
  cdn             text,
  status_code     int,
  title           text,
  server          text,
  final_url       text,
  first_seen      timestamptz NOT NULL,
  last_seen       timestamptz NOT NULL,
  last_changed    timestamptz
);
CREATE INDEX asset_current_program ON asset_current (program_id, lifecycle);
CREATE INDEX asset_current_suffix ON asset_current (reverse(host));

-- +goose Down
DROP TABLE asset_current;
DROP FUNCTION ensure_month_partition(text, date);
DROP TABLE change;
DROP TABLE observation;
DROP TABLE asset;
ALTER TABLE wildcard DROP CONSTRAINT wildcard_last_recon_run_fkey;
DROP TABLE run;
