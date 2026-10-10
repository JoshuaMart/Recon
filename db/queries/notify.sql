-- Notifications. The event is written by ingestion, in its transaction; only
-- the sending is asynchronous.

-- WriteEvents inserts a whole batch in one statement. Ingestion has a round trip budget
-- per observation, enforced by a test; one INSERT per changed observation would blow it
-- on a first run where everything changes. The budget is per observation beyond a batch's
-- fixed cost.
--
-- Parallel arrays, not COPY (see AddRunTargets): COPY FROM is refused on a table with a
-- policy. Same round trip, with organization and programme as scalars of the statement,
-- constants of the batch for every caller.
--
-- @tenant: scoped
-- name: WriteEvents :execrows
INSERT INTO notification_event (
    org_id, program_id, asset_id, kind, priority, payload, created_at, suppressed)
SELECT @org_id::uuid, @program_id::uuid,
       (@asset_ids::uuid[])[i], (@kinds::text[])[i], (@priorities::text[])[i],
       (@payloads::jsonb[])[i], (@created_ats::timestamptz[])[i], (@suppressed::boolean[])[i]
  FROM generate_subscripts(@kinds::text[], 1) AS i;

-- PendingEvents is the notifier's queue. Ordered by priority so a takeover leaves before a
-- title change; the partial index means a closed month costs an empty index to consult,
-- not a month of rows to filter.
--
-- @tenant: cross-org
-- @why: the notifier drains every tenant's queue in one tick, ordered by
--       priority across all of them: a takeover leaves before a title change whoever
--       owns either.
-- name: PendingEvents :many
SELECT e.id, e.created_at, e.org_id, e.program_id, e.asset_id, e.kind, e.priority, e.payload,
       a.key AS asset_key, p.name AS program_name
  FROM notification_event e
  JOIN program p ON p.id = e.program_id
  LEFT JOIN asset a ON a.id = e.asset_id
 WHERE e.notified_at IS NULL
   AND NOT e.suppressed
 ORDER BY CASE e.priority
              WHEN 'critical' THEN 0 WHEN 'high' THEN 1
              WHEN 'medium' THEN 2 ELSE 3 END,
          e.created_at
 LIMIT @batch::int;

-- MarkNotified closes one event. By (id, created_at): the primary key is composite, and
-- omitting the second half scans every partition per mark.
--
-- @tenant: keyed
-- name: MarkNotified :exec
UPDATE notification_event SET notified_at = @at::timestamptz
 WHERE id = @id::bigint AND created_at = @created_at::timestamptz;

-- CountWindow is how many of a programme's events at one priority were sent inside the
-- window. Read from the table, not memory: an in-memory counter resets on restart,
-- reopening the tap exactly when an incident restarts the process.
--
-- An event with no asset does not consume the window: counting it would shrink the cap
-- the moment a programme went dark, when most room is needed.
--
-- @tenant: keyed
-- name: CountWindow :one
SELECT count(*)
  FROM notification_event
 WHERE program_id = @program_id::uuid
   AND priority = @priority::text
   AND asset_id IS NOT NULL
   AND notified_at >= @since::timestamptz;

-- SuppressWindow marks what the summary speaks for. The events stay in the database,
-- readable and unsent: an overflow must never produce the absence of a notification, how
-- an anti-flood turns into lost signal.
--
-- @tenant: keyed
-- name: SuppressWindow :execrows
UPDATE notification_event SET suppressed = true
 WHERE id = ANY(@ids::bigint[]) AND created_at = ANY(@created_ats::timestamptz[]);

-- ChannelsForOrg is where an organization's alerts go.
--
-- @tenant: scoped
-- name: ChannelsForOrg :many
SELECT id, url, secret_ref, template, min_priority, managed_by
  FROM notification_channel
 WHERE org_id = @org_id::uuid AND enabled
 ORDER BY created_at;

-- UpsertConfigChannel is the bootstrap of the configured output. Keyed on the
-- organization and the config marker, not the URL: otherwise changing the URL and
-- restarting inserts a second active row without disabling the first, and every alert goes
-- out twice, one to the replaced destination.
--
-- @tenant: scoped
-- name: UpsertConfigChannel :exec
INSERT INTO notification_channel (id, org_id, url, template, min_priority, managed_by)
VALUES (@id::uuid, @org_id::uuid, @url::text, sqlc.narg(template)::text,
        @min_priority::text, 'config')
ON CONFLICT (org_id) WHERE managed_by = 'config' DO UPDATE SET
    url          = EXCLUDED.url,
    template     = EXCLUDED.template,
    min_priority = EXCLUDED.min_priority,
    enabled      = true;

-- OneOrganization is the condition the configured channel is bootstrapped under. A
-- configuration file cannot name a tenant, so exactly one organization is the only
-- unambiguous case; beyond it, a global URL on one of several would leak one tenant's
-- alerts into another's channel.
--
-- @tenant: cross-org
-- @why: counting organizations is the one question that cannot be asked from
--       inside one, and the answer decides whether a configured channel may be
--       attached at all.
-- name: OneOrganization :many
SELECT id FROM org ORDER BY created_at LIMIT 2;

-- StuckEvents counts what has been neither notified nor suppressed for too long. A broken
-- notifier is silent by nature: ingestion keeps writing and the inventory stays correct,
-- until the takeover it misses.
--
-- @tenant: cross-org
-- @why: housekeeping watches the whole queue. A per tenant count cannot
--       detect a notifier that stopped, which is the failure this exists for.
-- name: StuckEvents :one
SELECT count(*) FROM notification_event
 WHERE notified_at IS NULL AND NOT suppressed AND created_at < @before::timestamptz;

-- PurgeSuppressed drops the onboarding and overflow noise. A targeted DELETE inside the
-- partitions, not a partition drop: those rows are written in waves and the delete is
-- bounded. Partitioning on suppressed would be the wrong axis.
--
-- @tenant: cross-org
-- @why: retention is a property of the deployment, applied to every tenant's
--       partitions at once.
-- name: PurgeSuppressed :execrows
DELETE FROM notification_event
 WHERE suppressed AND created_at < @before::timestamptz;

-- OnboardingDue finds the programmes whose first run is over and whose flood has never
-- been summarised. The summary is emitted when the grace ends, not during: one per batch
-- would each carry a count already wrong when written.
--
-- And it is emitted at all, which is the point: an overflow must never produce the
-- absence of a notification, and a first run saying nothing is the same loss of signal.
--
-- @tenant: cross-org
-- @why: the first run summary is due per programme, and the loop that emits it
--       serves every tenant in one tick.
-- name: OnboardingDue :many
WITH held AS (
    SELECT e.org_id, e.program_id, count(*) AS held
      FROM notification_event e
     WHERE e.suppressed
       AND e.kind = 'new_active'
       -- A window overflow writes a digest too, and excluding on any digest would silence a
       -- programme's onboarding summary for good: a large first run saturates the high window on
       -- port openings, which the grace does not hold, so the overflow digest lands first and the
       -- onboarding one never.
       AND NOT EXISTS (
             SELECT 1 FROM notification_event d
              WHERE d.program_id = e.program_id
                AND d.kind = 'digest'
                AND d.payload ->> 'reason' = 'first run')
       -- The grace is at most seven days, so nothing older can still be waiting. Without the bound
       -- this walks every suppressed row of the retention window, twice a minute, forever.
       AND e.created_at > @since::timestamptz
     GROUP BY e.org_id, e.program_id
)
SELECT h.org_id, h.program_id, p.name, p.created_at, h.held,
       EXISTS (SELECT 1 FROM run r
                WHERE r.program_id = p.id AND r.kind = 'discovery'
                  AND r.state = 'completed') AS completed_discovery,
       EXISTS (SELECT 1 FROM run r
                WHERE r.program_id = p.id AND r.kind = 'discovery') AS any_discovery,
       (SELECT count(*) FROM asset a WHERE a.program_id = p.id) AS assets
  FROM held h
  JOIN program p ON p.id = h.program_id
 ORDER BY h.program_id;

-- DigestInWindow reports whether a summary already speaks for this window. Without it
-- every suppressed event past the cap writes its own summary, and an anti-flood producing a
-- notification per suppressed notification is not one.
--
-- @tenant: keyed
-- name: DigestInWindow :one
SELECT count(*) FROM notification_event
 WHERE program_id = @program_id::uuid
   AND kind = 'digest'
   AND priority = @priority::text
   AND created_at >= @since::timestamptz;

-- SuppressedInWindow is what the summary stands for.
--
-- @tenant: keyed
-- name: SuppressedInWindow :one
SELECT count(*) FROM notification_event
 WHERE program_id = @program_id::uuid
   AND priority = @priority::text
   AND suppressed
   AND created_at >= @since::timestamptz;
