-- The candidate lane, and the two things Certificate Transparency has to write.
--
-- A CT candidate is due one minute after creation and the aggressive curve rests on that
-- check. One live verification run per programme holds a slot for the run's whole deadline,
-- so a candidate arriving a minute into a sweep would wait half an hour: the freshness
-- advantage spent on a queue.
--
-- It costs an index, not a mechanism, as the reservation is already per kind: a third kind
-- is the answer, not a second reservation scheme.

-- +goose Up

-- A third kind. The constraint is replaced rather than widened in place,
-- because a CHECK is not something ALTER can extend.
ALTER TABLE run DROP CONSTRAINT run_kind_known;
ALTER TABLE run ADD CONSTRAINT run_kind_known
    CHECK (kind IN ('discovery', 'verification', 'candidate'));

-- Pinned to resolve, and the database says so, not the caller. One resolver round trip per
-- name and nothing sent to the target is what lets candidate and verification runs be in
-- flight on one programme without adding up against the rate budget; widened to full they
-- would. "The scheduler only sets resolve" is a convention, and the console reaches run
-- creation too.
ALTER TABLE run ADD CONSTRAINT run_candidate_resolves_only
    CHECK (kind <> 'candidate' OR scope = 'resolve');

CREATE UNIQUE INDEX run_one_live_candidate_idx ON run (program_id)
    WHERE kind = 'candidate' AND state IN ('pending', 'running');

-- What CT has actually delivered under one apex. Counters per apex, not a flag on the
-- programme: apexes behave differently (one all wildcard, another a certificate per host), and
-- a boolean would lose which it spoke of and never expire. No coverage score: the reading is
-- derived at query time, as a stored score cannot be recomputed when its formula changes.
--
-- A row is deleted when its apex leaves the set: watched_since would otherwise span a period
-- of no watching, and an apex removed and put back would read as continuously covered. The
-- counts go too, acceptable like the flush losing a minute: a metric, not the journal.
CREATE TABLE ct_apex (
    org_id           uuid NOT NULL REFERENCES org(id) ON DELETE CASCADE,
    program_id       uuid NOT NULL REFERENCES program(id) ON DELETE CASCADE,
    apex             text NOT NULL,
    watched_since    timestamptz NOT NULL DEFAULT now(),
    san_count        bigint NOT NULL DEFAULT 0,
    wildcard_count   bigint NOT NULL DEFAULT 0,
    last_san_at      timestamptz,
    last_wildcard_at timestamptz,
    PRIMARY KEY (program_id, apex),
    CONSTRAINT ct_apex_counts_are_counts CHECK (san_count >= 0 AND wildcard_count >= 0)
);

SELECT apply_tenant_policy('ct_apex');

-- One row per minute in which the feed delivered something. The aggregator keeps no history
-- for Recon, so a dropped socket loses what passed while it was down; without a record of when
-- the feed was alive, an outage and an apex the logs have nothing to say about give the same
-- number. Presence, not absence: a dead process writes no minute, so a gap needs nothing to
-- notice it and no reconciliation after a restart.
--
-- No org_id, deliberately: one socket serves the deployment, so this is a fact about the feed,
-- not a tenant. It is on the exempt list in the schema test, with that reason.
CREATE TABLE ct_feed_minute (
    minute timestamptz PRIMARY KEY,
    frames bigint NOT NULL DEFAULT 0
);

COMMENT ON TABLE ct_feed_minute IS
    'One row per minute the Certificate Transparency feed delivered a frame. Coverage over a '
    'window reads this to tell an outage from an apex the logs are silent on.';

-- +goose Down

DROP TABLE IF EXISTS ct_feed_minute;
DROP TABLE IF EXISTS ct_apex;

DROP INDEX IF EXISTS run_one_live_candidate_idx;
ALTER TABLE run DROP CONSTRAINT IF EXISTS run_candidate_resolves_only;

-- A kind that no longer exists cannot be kept, so its rows go. That loses a run, not an
-- observation: a run is replayable and its report is not what this system refuses to lose.
-- Their frozen lists cascade.
DELETE FROM run WHERE kind = 'candidate';

ALTER TABLE run DROP CONSTRAINT run_kind_known;
ALTER TABLE run ADD CONSTRAINT run_kind_known
    CHECK (kind IN ('discovery', 'verification'));
