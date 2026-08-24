-- The candidate lane, and the two things Certificate Transparency has to write.
--
-- The lane is the one that decides the phase. A CT candidate is due one minute
-- after it is created, and the whole aggressive curve rests on that first check
-- happening then. One live verification run per programme is a slot held for
-- the run's whole deadline, so a candidate arriving a minute into a sweep would
-- wait half an hour for a check the curve wanted at sixty seconds: the
-- freshness advantage spent on a queue.
--
-- It costs an index rather than a mechanism, because the reservation is already
-- per kind. That is the whole reason a third kind is the answer here and a
-- second reservation scheme is not.

-- +goose Up

-- A third kind. The constraint is replaced rather than widened in place,
-- because a CHECK is not something ALTER can extend.
ALTER TABLE run DROP CONSTRAINT run_kind_known;
ALTER TABLE run ADD CONSTRAINT run_kind_known
    CHECK (kind IN ('discovery', 'verification', 'candidate'));

-- Pinned to resolve, and the database says so rather than the caller.
--
-- One round trip to a resolver pool per name, and nothing at all sent to the
-- target, which is what lets a candidate run and a verification run be in
-- flight on the same programme without adding up to anything the rate budget
-- has an opinion about. Widened to full it would add up, and "the scheduler
-- only ever sets resolve" is a convention: the console reaches run creation
-- too, and a convention is what the next caller has no way to read.
ALTER TABLE run ADD CONSTRAINT run_candidate_resolves_only
    CHECK (kind <> 'candidate' OR scope = 'resolve');

CREATE UNIQUE INDEX run_one_live_candidate_idx ON run (program_id)
    WHERE kind = 'candidate' AND state IN ('pending', 'running');

-- What Certificate Transparency has actually delivered under one apex.
--
-- Counters per apex rather than a flag on the programme, because a programme
-- holds several apexes and they do not behave alike: one served entirely by a
-- wildcard, another issuing a certificate per host. A boolean would lose which
-- apex it spoke about, and it would never expire.
--
-- No coverage score here, on purpose. The reading is derived from these numbers
-- at query time, because a stored score is a number nobody can recompute the
-- day its formula changes, and this one is computed on data nobody has yet.
--
-- A row is deleted when its apex leaves the set. watched_since would otherwise
-- say a name has been watched since a date that spans a period nothing was
-- watching it, and an apex removed and put back would read as continuously
-- covered. The counts go with it, which is acceptable for the same reason the
-- flush below may lose a minute: this is a metric, not the journal.
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

-- One row per minute in which the feed delivered something.
--
-- The gap is the reason this exists. The aggregator keeps no history on Recon's
-- behalf, so a dropped socket loses whatever passed while it was down and
-- reconnecting resumes at the present. Without a record of when the feed was
-- alive, an outage and an apex the logs genuinely have nothing to say about
-- produce the same number, and the coverage reading above would call the first
-- one the second.
--
-- Presence rather than absence, which is the same rule as everywhere else here:
-- a process that dies writes no minute, so a gap needs nothing to notice it and
-- no reconciliation after a restart. Inferring the outage from its edges would
-- need the loop to survive the thing it is trying to record.
--
-- It carries no org_id and that is deliberate: one socket serves the whole
-- deployment, so this is a fact about the feed and not about a tenant. It is on
-- the exempt list in the schema test, with that reason.
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

-- A kind that no longer exists cannot be kept, so the rows go with it. That is
-- loss of a run and not of an observation: a run is replayable and its report
-- is not what this system refuses to lose (P3). Their frozen lists cascade.
DELETE FROM run WHERE kind = 'candidate';

ALTER TABLE run DROP CONSTRAINT run_kind_known;
ALTER TABLE run ADD CONSTRAINT run_kind_known
    CHECK (kind IN ('discovery', 'verification'));
