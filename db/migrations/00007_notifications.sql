-- What turns an inventory into a product.
--
-- Two tables and one rule about where each row is written. The event is
-- produced inside the ingestion transaction, by ingestion itself, because a
-- sweeper would have to re-derive what ingestion just computed and would miss
-- every transient state on the way. Only the sending is asynchronous.

-- +goose Up

CREATE TABLE notification_event (
    id           bigserial,
    org_id       uuid NOT NULL REFERENCES org(id) ON DELETE CASCADE,
    program_id   uuid NOT NULL REFERENCES program(id) ON DELETE CASCADE,
    -- NULL on a program event, which no asset carries: the mass tip into
    -- unobservable, an incident, and the summary.
    asset_id     uuid REFERENCES asset(id) ON DELETE CASCADE,
    kind         text NOT NULL,
    priority     text NOT NULL,
    -- The diff and the lineage, frozen at write time. A notification reflects
    -- the state at the moment of the event and not at the moment of sending: a
    -- notifier ten minutes behind rereading the projection would describe
    -- something other than what it announces.
    payload      jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    notified_at  timestamptz,
    suppressed   boolean NOT NULL DEFAULT false,

    PRIMARY KEY (id, created_at),

    CONSTRAINT notification_event_kind_known CHECK (kind IN (
        'takeover_candidate', 'external_host_dead', 'new_active', 'port_opened',
        'program_unobservable', 'tech_changed', 'chain_changed', 'went_inactive',
        'geo_anomaly', 'cert_changed', 'title_changed', 'detection_improved',
        'run_never_completed', 'digest')),
    CONSTRAINT notification_event_priority_known CHECK (
        priority IN ('critical', 'high', 'medium', 'low')),
    -- The nullability of asset_id is a rule rather than a permission. Without
    -- it the table is an open door to malformed events: a takeover with no
    -- asset, or a summary claiming to designate one.
    CONSTRAINT notification_event_scope_matches_kind CHECK (
        (kind IN ('program_unobservable', 'run_never_completed', 'digest'))
        = (asset_id IS NULL))
) PARTITION BY RANGE (created_at);

-- The notifier's queue. suppressed is in the predicate rather than only in the
-- query: without it a first run leaves a few thousand rows nothing will ever
-- notify and every tick rereads.
CREATE INDEX notification_event_pending_idx
    ON notification_event (program_id, priority, created_at)
    WHERE notified_at IS NULL AND NOT suppressed;

-- The window is counted from the table itself, never from memory a restart
-- clears, so the count has an index of its own.
CREATE INDEX notification_event_window_idx
    ON notification_event (program_id, priority, notified_at)
    WHERE notified_at IS NOT NULL;

-- Purging the suppressed rows is a targeted DELETE inside the partitions
-- rather than a partition drop: they are written in waves and bounded.
CREATE INDEX notification_event_suppressed_idx
    ON notification_event (created_at) WHERE suppressed;

SELECT ensure_monthly_partitions('notification_event', 2);

-- Where alerts go. One generic webhook and only that: Discord and Slack are a
-- payload template rather than a connector, and writing one in Go would freeze
-- what a template expresses and start over at the next one.
CREATE TABLE notification_channel (
    id           uuid PRIMARY KEY,
    org_id       uuid NOT NULL REFERENCES org(id) ON DELETE CASCADE,
    kind         text NOT NULL DEFAULT 'webhook',
    url          text NOT NULL,
    -- The name of a secret, never a secret. A column carrying one would put
    -- every tenant's credential behind a single SELECT.
    secret_ref   text,
    template     text,
    enabled      boolean NOT NULL DEFAULT true,
    -- Routes by criticality without multiplying components: a channel that
    -- only wants what is burning says so here rather than in a filter placed
    -- somewhere along the path.
    min_priority text NOT NULL DEFAULT 'low',
    -- config or console. Without this marker the bootstrap keys on the URL, so
    -- changing the configured URL and restarting inserts a second active row
    -- without disabling the first, and every alert goes out twice, one of them
    -- to the destination just replaced.
    managed_by   text NOT NULL DEFAULT 'console',
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT notification_channel_kind_known CHECK (kind = 'webhook'),
    CONSTRAINT notification_channel_priority_known CHECK (
        min_priority IN ('critical', 'high', 'medium', 'low')),
    CONSTRAINT notification_channel_managed_known CHECK (managed_by IN ('config', 'console'))
);
CREATE INDEX notification_channel_org_idx ON notification_channel (org_id) WHERE enabled;

-- One configured channel per organization, which is what makes the bootstrap
-- idempotent rather than duplicating on every restart.
CREATE UNIQUE INDEX notification_channel_one_config_idx
    ON notification_channel (org_id) WHERE managed_by = 'config';

-- +goose Down

DROP TABLE IF EXISTS notification_channel;
DROP TABLE IF EXISTS notification_event;
