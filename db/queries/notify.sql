-- Notifications. The event is written by ingestion, in its transaction; only
-- the sending is asynchronous.

-- WriteEvents inserts a whole batch in one statement.
--
-- Ingestion has a round trip budget per observation and a test fails past it.
-- One INSERT per changed observation would blow it on a first run, where
-- everything changes, which is exactly the scenario the milestone measures. So
-- the budget is counted per observation beyond the fixed cost of a batch.
--
-- name: WriteEvents :copyfrom
INSERT INTO notification_event (
    org_id, program_id, asset_id, kind, priority, payload, created_at, suppressed)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- PendingEvents is the notifier's queue.
--
-- Ordered by priority so a takeover leaves before a title change, and the
-- partial index means a closed month costs an empty index to consult rather
-- than a month of rows to filter.
--
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

-- MarkNotified closes one event.
--
-- By (id, created_at) rather than id alone: the primary key is composite, and
-- forgetting the second half scans every partition on each mark.
--
-- name: MarkNotified :exec
UPDATE notification_event SET notified_at = @at::timestamptz
 WHERE id = @id::bigint AND created_at = @created_at::timestamptz;

-- CountWindow is how many of a programme's events at one priority were sent
-- inside the window.
--
-- Read from the table rather than from state the notifier holds in memory. An
-- in-memory counter resets on restart, which reopens the tap exactly when one
-- restarts because of an incident.
--
-- An event with no asset does not consume the window either: counting it would
-- shrink the cap at the precise moment a programme went dark, which is when the
-- most room is needed.
--
-- name: CountWindow :one
SELECT count(*)
  FROM notification_event
 WHERE program_id = @program_id::uuid
   AND priority = @priority::text
   AND asset_id IS NOT NULL
   AND notified_at >= @since::timestamptz;

-- SuppressWindow marks what the summary speaks for.
--
-- The individual events stay in the database, readable and not sent. An
-- overflow must never produce the absence of a notification, which is how an
-- anti-flood turns into a loss of signal.
--
-- name: SuppressWindow :execrows
UPDATE notification_event SET suppressed = true
 WHERE id = ANY(@ids::bigint[]) AND created_at = ANY(@created_ats::timestamptz[]);

-- ChannelsForOrg is where an organization's alerts go.
--
-- name: ChannelsForOrg :many
SELECT id, url, secret_ref, template, min_priority, managed_by
  FROM notification_channel
 WHERE org_id = @org_id::uuid AND enabled
 ORDER BY created_at;

-- UpsertConfigChannel is the bootstrap of the configured output.
--
-- Keyed on the organization and the config marker rather than on the URL.
-- Without that, changing the configured URL and restarting inserts a second
-- active row without disabling the first, and every alert goes out twice, one
-- of them to the destination just replaced.
--
-- name: UpsertConfigChannel :exec
INSERT INTO notification_channel (id, org_id, url, template, min_priority, managed_by)
VALUES (@id::uuid, @org_id::uuid, @url::text, sqlc.narg(template)::text,
        @min_priority::text, 'config')
ON CONFLICT (org_id) WHERE managed_by = 'config' DO UPDATE SET
    url          = EXCLUDED.url,
    template     = EXCLUDED.template,
    min_priority = EXCLUDED.min_priority,
    enabled      = true;

-- OneOrganization is the condition the configured channel is bootstrapped
-- under.
--
-- A configuration file has no way to name a tenant, so exactly one organization
-- is the only case where the intent is unambiguous. Beyond it, giving a global
-- URL to one of several organizations would leak one tenant's alerts into
-- another's channel.
--
-- name: OneOrganization :many
SELECT id FROM org ORDER BY created_at LIMIT 2;

-- StuckEvents counts what has been neither notified nor suppressed for too
-- long.
--
-- A broken notifier is a silent failure by nature: nothing else announces it,
-- ingestion keeps writing and the inventory stays correct. It is the kind of
-- outage that goes unnoticed until the takeover it misses.
--
-- name: StuckEvents :one
SELECT count(*) FROM notification_event
 WHERE notified_at IS NULL AND NOT suppressed AND created_at < @before::timestamptz;

-- PurgeSuppressed drops the onboarding and overflow noise.
--
-- A targeted DELETE inside the partitions rather than a partition drop: those
-- rows are written in waves and the delete is bounded. Partitioning on
-- suppressed would be the wrong axis.
--
-- name: PurgeSuppressed :execrows
DELETE FROM notification_event
 WHERE suppressed AND created_at < @before::timestamptz;

-- OnboardingDue finds the programmes whose first run is over and whose flood
-- has never been summarised.
--
-- The summary is emitted when the grace ends, not during. Summarising while the
-- run is going would produce one summary per batch, each carrying a count
-- already wrong by the time it is written.
--
-- And it is emitted at all, which is the point: an overflow must never produce
-- the absence of a notification. That is how an anti-flood turns into a loss of
-- signal, and a first run that says nothing at all is the same failure wearing
-- a different name.
--
-- name: OnboardingDue :many
WITH held AS (
    SELECT e.org_id, e.program_id, count(*) AS held
      FROM notification_event e
     WHERE e.suppressed
       AND e.kind = 'new_active'
       AND NOT EXISTS (
             SELECT 1 FROM notification_event d
              WHERE d.program_id = e.program_id AND d.kind = 'digest')
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
