-- ApexSet is what the Certificate Transparency matcher walks.
--
-- Apex include rules alone. An fqdn rule already declared its host, so the
-- asset exists and carries a due date and this stream would find the same name
-- and create nothing; putting it in the set would buy a lookup per certificate
-- for a row that is already there. cidr and regex never named a thing at all.
--
-- The authorization window is here and not only p.state, which is the same two
-- lines the discovery cadence carries and for the same reason one level
-- earlier: an expired programme left in the set keeps creating assets with due
-- dates on a perimeter nobody may scan, so the first thing each one does is
-- have its run refused.
--
-- The instant is a parameter rather than now(), because valid_to and
-- authorized_to are written by the application: comparing them against the
-- database's clock would make the answer depend on two clocks agreeing.
--
-- @tenant: cross-org
-- @why: the apex set spans every tenant by construction. One socket serves the
--       whole deployment, and this is the query that decides which organization
--       a name belongs to, so it cannot be filtered by one.
-- name: ApexSet :many
SELECT p.org_id, p.id AS program_id, r.pattern AS apex
  FROM scope_rule r
  JOIN program p ON p.id = r.program_id
 WHERE r.kind = 'include'
   AND r.matcher = 'apex'
   AND r.valid_from <= @at
   AND (r.valid_to IS NULL OR r.valid_to > @at)
   AND p.state = 'active'
   AND p.authorized_from <= @at
   AND (p.authorized_to IS NULL OR p.authorized_to > @at)
 ORDER BY p.id, r.pattern;

-- BumpApexCounters records what the stream delivered under each apex.
--
-- Parallel arrays for the same reason AddRunTargets uses them: one round trip,
-- and no COPY on a table carrying a policy. A flush covers every tenant at
-- once, so the organization is a column here rather than a scalar.
--
-- The insert is what creates the row, so watched_since is set the first time an
-- apex is seen rather than by a separate pass. The counters only ever move
-- upwards, and the two timestamps take the greatest rather than the newest
-- write: a flush is not ordered against another flush.
--
-- @tenant: cross-org
-- @why: one flush covers every programme in the set, which is what makes it one
--       round trip instead of one per tenant.
-- name: BumpApexCounters :exec
INSERT INTO ct_apex (org_id, program_id, apex, watched_since, san_count, wildcard_count,
                     last_san_at, last_wildcard_at)
SELECT (@org_ids::uuid[])[i], (@program_ids::uuid[])[i], (@apexes::text[])[i], @at::timestamptz,
       (@sans::bigint[])[i], (@wildcards::bigint[])[i],
       CASE WHEN (@sans::bigint[])[i] > 0 THEN @at::timestamptz END,
       CASE WHEN (@wildcards::bigint[])[i] > 0 THEN @at::timestamptz END
  FROM generate_subscripts(@apexes::text[], 1) AS i
ON CONFLICT (program_id, apex) DO UPDATE
   SET san_count        = ct_apex.san_count + excluded.san_count,
       wildcard_count   = ct_apex.wildcard_count + excluded.wildcard_count,
       last_san_at      = GREATEST(ct_apex.last_san_at, excluded.last_san_at),
       last_wildcard_at = GREATEST(ct_apex.last_wildcard_at, excluded.last_wildcard_at);

-- WatchApexes creates a row for every apex in the set, counting nothing.
--
-- Without it an apex that has never produced a SAN has no row, and "watched
-- since, delivered nothing" reads identically to "not watched at all". That is
-- the difference the coverage reading exists to state.
--
-- @tenant: cross-org
-- @why: the set spans every tenant, and so does the row it keeps per apex.
-- name: WatchApexes :exec
INSERT INTO ct_apex (org_id, program_id, apex, watched_since)
SELECT (@org_ids::uuid[])[i], (@program_ids::uuid[])[i], (@apexes::text[])[i], @at::timestamptz
  FROM generate_subscripts(@apexes::text[], 1) AS i
ON CONFLICT (program_id, apex) DO NOTHING;

-- ForgetApexesOutsideTheSet drops the counters of an apex nobody watches now.
--
-- watched_since would otherwise span a period during which nothing was
-- watching, so an apex removed and put back would read as continuously covered.
-- The counts go with it, which is acceptable for the same reason the flush may
-- lose a minute: this is a metric and not the journal.
--
-- @tenant: cross-org
-- @why: it reconciles the whole table against the whole set, in one statement.
-- name: ForgetApexesOutsideTheSet :execrows
DELETE FROM ct_apex
 WHERE NOT EXISTS (
       SELECT 1
         FROM generate_subscripts(@apexes::text[], 1) AS i
        WHERE (@program_ids::uuid[])[i] = ct_apex.program_id
          AND (@apexes::text[])[i] = ct_apex.apex);

-- RecordFeedMinute says the feed was alive for this minute.
--
-- Presence rather than absence. A process that dies writes no minute, so a gap
-- needs nothing to notice it and no reconciliation after a restart, where
-- inferring an outage from its edges would need the loop to survive the thing
-- it is recording.
--
-- @tenant: none
-- name: RecordFeedMinute :exec
INSERT INTO ct_feed_minute (minute, frames)
VALUES (date_trunc('minute', @at::timestamptz), @frames::bigint)
ON CONFLICT (minute) DO UPDATE
   SET frames = ct_feed_minute.frames + excluded.frames;
