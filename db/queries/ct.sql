-- ApexSet is what the Certificate Transparency matcher walks: apex include rules
-- alone. An fqdn rule already declared its host, so the asset exists with a due date
-- and the stream would create nothing; cidr and regex never named a thing.
--
-- The authorization window is checked here, not only p.state, for the discovery
-- cadence's reason one level earlier: an expired programme left in the set keeps
-- creating assets on a perimeter nobody may scan, whose runs get refused.
--
-- The instant is a parameter, not now(): valid_to and authorized_to are written by the
-- application, and the database clock would make the answer depend on two clocks.
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
-- Parallel arrays, like AddRunTargets: one round trip and no COPY on a table with a
-- policy. A flush covers every tenant, so the organization is a column.
--
-- The insert creates the row, so watched_since is set when an apex is first seen. The
-- counters only rise, and the two timestamps take the greatest, not the newest, write:
-- flushes are not ordered against each other.
--
-- @tenant: cross-org
-- @why: one flush covers every programme in the set, which is what makes it one
--       round trip instead of one per tenant.
-- name: BumpApexCounters :exec
INSERT INTO ct_apex (org_id, program_id, apex, watched_since, san_count, wildcard_count,
                     dropped, last_san_at, last_wildcard_at)
SELECT (@org_ids::uuid[])[i], (@program_ids::uuid[])[i], (@apexes::text[])[i], @at::timestamptz,
       (@sans::bigint[])[i], (@wildcards::bigint[])[i], (@dropped::bigint[])[i],
       CASE WHEN (@sans::bigint[])[i] > 0 THEN @at::timestamptz END,
       CASE WHEN (@wildcards::bigint[])[i] > 0 THEN @at::timestamptz END
  FROM generate_subscripts(@apexes::text[], 1) AS i
ON CONFLICT (program_id, apex) DO UPDATE
   SET san_count        = ct_apex.san_count + excluded.san_count,
       wildcard_count   = ct_apex.wildcard_count + excluded.wildcard_count,
       dropped          = ct_apex.dropped + excluded.dropped,
       last_san_at      = GREATEST(ct_apex.last_san_at, excluded.last_san_at),
       last_wildcard_at = GREATEST(ct_apex.last_wildcard_at, excluded.last_wildcard_at);

-- WatchApexes creates a row for every apex in the set, counting nothing. Without it an
-- apex that never produced a SAN has no row, and "watched, delivered nothing" reads like
-- "not watched at all", the difference coverage exists to state.
--
-- @tenant: cross-org
-- @why: the set spans every tenant, and so does the row it keeps per apex.
-- name: WatchApexes :exec
INSERT INTO ct_apex (org_id, program_id, apex, watched_since)
SELECT (@org_ids::uuid[])[i], (@program_ids::uuid[])[i], (@apexes::text[])[i], @at::timestamptz
  FROM generate_subscripts(@apexes::text[], 1) AS i
ON CONFLICT (program_id, apex) DO NOTHING;

-- ForgetApexesOutsideTheSet drops the counters of an apex nobody watches now.
-- watched_since would otherwise span a period of no watching, so an apex removed and
-- put back would read as continuously covered. The counts go too, acceptable like the
-- flush losing a minute: a metric, not the journal.
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

-- RecordFeedMinute says the feed was alive for this minute. Presence, not absence: a
-- dead process writes no minute, so a gap needs nothing to notice it and no
-- reconciliation after a restart.
--
-- @tenant: none
-- name: RecordFeedMinute :exec
INSERT INTO ct_feed_minute (minute, frames)
VALUES (date_trunc('minute', @at::timestamptz), @frames::bigint)
ON CONFLICT (minute) DO UPDATE
   SET frames = ct_feed_minute.frames + excluded.frames;

-- PurgeFeedMinutes bounds the feed's own record. A row a minute is half a million a
-- year: small but unbounded, and nothing else in the schema grows forever; coverage
-- never looks back past an apex's watch. A delete, not a partition drop: the table is
-- one column wide.
--
-- @tenant: none
-- name: PurgeFeedMinutes :execrows
DELETE FROM ct_feed_minute WHERE minute < @before::timestamptz;

-- ApexCoverage is what a programme's CT panel reads: counters since the apex was first
-- watched, never a rolling window, since an apex added yesterday and silent differs from
-- one watched a month and silent.
--
-- No score: the numbers and the span they cover. One confidence figure is the composite
-- score the console is built without, as "no certificate in thirty days" and "watched
-- since this morning" differ and one number cannot say so.
--
-- @tenant: keyed
-- name: ApexCoverage :many
SELECT apex, watched_since, san_count, wildcard_count, dropped,
       last_san_at, last_wildcard_at
  FROM ct_apex
 WHERE program_id = @program_id::uuid
 ORDER BY apex;

-- FeedUptime is how much of a span the feed was alive for. Without it a silent apex and
-- a down feed read alike, and the second is this deployment's problem, not a fact about
-- the logs. Presence is counted, so a missing minute is one where nothing arrived.
--
-- @tenant: none
-- name: FeedUptime :one
SELECT count(*)::bigint AS minutes, coalesce(sum(frames), 0)::bigint AS frames
  FROM ct_feed_minute
 WHERE minute >= date_trunc('minute', @since::timestamptz);
