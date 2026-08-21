-- The render queue is a predicate, not a list.
--
-- It is re-evaluated on every tick and holds no state between two of them,
-- which is what makes the whole path idempotent: a render has no lease, the due
-- date is the queue, and a refused asset simply keeps a date in the past. There
-- is nothing to reconcile and no path where a crash between a refusal and a
-- retry drops the work.

-- SelectDueRenders is one pass.
--
-- Priority sorts before the due date, and that ordering is what protects the
-- urgent case. A first scan makes thousands of baselines due at the same
-- instant, and a render triggered by a detected change five minutes later
-- carries a later date: ordered on the date alone it would sort behind every
-- one of them.
--
-- The authorization window is read here rather than only when a run is
-- provisioned. A render is a request to somebody's server, and an expired
-- programme must not receive one because a due date said so.
--
-- name: SelectDueRenders :many
SELECT c.asset_id, c.org_id, c.program_id, c.kind, c.key, c.host, c.port, c.scheme,
       c.final_url, c.http_reachable, c.fingerprint_reachable,
       c.is_cdn, c.cdn_provider, p.rate_limit_rps
  FROM asset_current c
  JOIN program p ON p.id = c.program_id
 WHERE c.next_fingerprint_at <= @at::timestamptz
   AND c.lifecycle <> 'archived'
   AND c.scope_status = 'in_scope'
   AND p.state = 'active'
   AND p.authorized_from <= @at::timestamptz
   AND (p.authorized_to IS NULL OR p.authorized_to > @at::timestamptz)
 ORDER BY c.fingerprint_priority, c.next_fingerprint_at, c.asset_id
 LIMIT @batch_size::int;

-- PromoteRender brings a render forward and never delays one.
--
-- It is what a detected change, a manual request and the sweep after a service
-- update all go through, and the one-way rule is the load-bearing part: a
-- manual request lives in the high queue with an immediate due date, and a
-- replan triggered a second later would bury it for a week without trace.
--
-- name: PromoteRender :exec
UPDATE asset_current SET
    next_fingerprint_at  = LEAST(COALESCE(next_fingerprint_at, @at::timestamptz), @at::timestamptz),
    fingerprint_priority = LEAST(fingerprint_priority, @priority::smallint)
 WHERE asset_id = @asset_id::uuid
   AND scope_status = 'in_scope'
   AND lifecycle <> 'archived';

-- CountUnobservable is what the per programme alert reads.
--
-- A mass tip into unobservable is a different event from one asset going quiet,
-- and it usually says something about the observer rather than about the
-- targets: an address that got banned, an egress that broke, a renderer that
-- stopped clearing challenges. Swallowed by a per asset summary it is invisible
-- exactly when it matters.
--
-- name: CountUnobservable :many
SELECT c.program_id, c.org_id, p.name,
       count(*) FILTER (WHERE c.lifecycle = 'unobservable') AS unobservable,
       count(*) AS total
  FROM asset_current c
  JOIN program p ON p.id = c.program_id
 WHERE c.scope_status = 'in_scope'
   AND c.lifecycle <> 'archived'
 GROUP BY c.program_id, c.org_id, p.name
HAVING count(*) FILTER (WHERE c.lifecycle = 'unobservable') > 0
 ORDER BY c.program_id;

-- ReplanRenders is the forced refresh after a major update of the service.
--
-- The whole inventory is replanned in the low queue, spread over several days,
-- which restores baseline consistency without a mass alert. It may only bring a
-- render forward, never delay one, and it leaves untouched the assets that have
-- left the scheduler.
--
-- The spread is the walk order rather than a random draw, so a replan that runs
-- twice lands every asset on the same date both times instead of reshuffling an
-- inventory that is already queued.
--
-- name: ReplanRenders :execrows
WITH input AS (
    SELECT
        @org_id::uuid           AS org_id,
        @at::timestamptz        AS at,
        @spread_seconds::bigint AS spread
),
numbered AS (
    SELECT c.asset_id, row_number() OVER (ORDER BY c.asset_id) AS position
      FROM asset_current c, input i
     WHERE c.org_id = i.org_id
       AND c.next_fingerprint_at IS NOT NULL
       AND c.lifecycle <> 'archived'
)
UPDATE asset_current c SET
    next_fingerprint_at = LEAST(
        c.next_fingerprint_at,
        i.at + ((n.position % i.spread) * interval '1 second'))
  FROM numbered n, input i
 WHERE c.asset_id = n.asset_id;

-- AssetForRender reads the counters a render needs, and creates nothing.
--
-- The upsert path exists because a report discovers things. A render is made
-- for an asset that already exists, and routing it through the same statement
-- would let the rendering service invent inventory, which is the one thing the
-- isolated component must not be able to do.
--
-- name: AssetForRender :one
SELECT c.asset_id, c.org_id, c.program_id, c.kind, c.key, c.lifecycle,
       c.scope_status, c.port, c.backoff_tier, c.http_streak, c.fingerprint_streak,
       c.http_reachable, c.fingerprint_reachable, c.first_seen,
       (SELECT jsonb_object_agg(l.layer, jsonb_build_object(
                   'state', l.state,
                   'informative', l.informative_failures,
                   'non_informative', l.non_informative_failures,
                   'first_failure_at', l.first_failure_at,
                   'last_ok_at', l.last_ok_at,
                   'last_checked_at', l.last_checked_at))
          FROM asset_layer l WHERE l.asset_id = c.asset_id) AS layers
  FROM asset_current c
 WHERE c.asset_id = @asset_id::uuid;

-- RescheduleRender moves a render out to its next due date.
--
-- Distinct from PromoteRender, which only ever brings one forward. This is the
-- other direction and the ordinary one: the render happened, so the asset goes
-- back to the low queue at whatever cadence its regime earns. Leaving the
-- priority raised would keep an asset that was urgent once ahead of the queue
-- for every pass afterwards.
--
-- name: RescheduleRender :exec
UPDATE asset_current SET
    next_fingerprint_at  = CASE WHEN scope_status = 'in_scope' AND lifecycle <> 'archived'
                                THEN @at::timestamptz END,
    fingerprint_priority = @priority::smallint
 WHERE asset_id = @asset_id::uuid;

-- EarnBaseline arms a service's first render, once.
--
-- Distinct from PromoteRender, which takes the earlier of two dates and the
-- more urgent of two priorities. A baseline is neither: it is the line being
-- created, so it only applies where there is none, and it must not touch an
-- asset already queued for any other reason. Going through the promote path
-- would leave the baseline at whatever priority the column defaults to and
-- silently put a mass of first renders in the queue that exists to stay short.
--
-- name: EarnBaseline :exec
UPDATE asset_current SET
    next_fingerprint_at  = @at::timestamptz,
    fingerprint_priority = @priority::smallint
 WHERE asset_id = @asset_id::uuid
   AND scope_status = 'in_scope'
   AND lifecycle <> 'archived'
   AND next_fingerprint_at IS NULL;
