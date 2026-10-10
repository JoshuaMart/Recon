-- The render queue is a predicate, not a list. It is re-evaluated every tick and holds no
-- state between them, which makes the path idempotent: a render has no lease, the due date
-- is the queue, and a refused asset keeps a date in the past. Nothing to reconcile, and no
-- crash window between a refusal and a retry drops the work.

-- SelectDueRenders is one pass.
--
-- Priority sorts before the due date, protecting the urgent case: a first scan makes
-- thousands of baselines due at one instant, and a change-triggered render five minutes
-- later has a later date, so date alone would sort it behind all of them.
--
-- The authorization window is read here, not only at run provisioning: a render is a
-- request to someone's server, and an expired programme must not get one because a due
-- date said so.
--
-- @tenant: cross-org
-- @why: the render queue is one predicate over the whole inventory, and the
--       budget it competes for is the deployment's rather than a tenant's.
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

-- PromoteRender brings a render forward and never delays one. A detected change, a manual
-- request and the post-update sweep all use it; the one-way rule is load-bearing, since a
-- manual request lives in the high queue with an immediate date and a replan a second
-- later would bury it for a week without trace.
--
-- @tenant: keyed
-- name: PromoteRender :exec
UPDATE asset_current SET
    next_fingerprint_at  = LEAST(COALESCE(next_fingerprint_at, @at::timestamptz), @at::timestamptz),
    fingerprint_priority = LEAST(fingerprint_priority, @priority::smallint)
 WHERE asset_id = @asset_id::uuid
   AND scope_status = 'in_scope'
   AND lifecycle <> 'archived';

-- CountUnobservable is what the per-programme alert reads. A mass tip into unobservable
-- differs from one asset going quiet and usually says something about the observer (a
-- banned address, a broken egress, a renderer no longer clearing challenges); a per-asset
-- summary would hide it exactly when it matters.
--
-- @tenant: cross-org
-- @why: the mass tip alert is computed for every programme in one pass. It
--       groups by tenant and never merges two, which is what makes the crossing a
--       traversal rather than a leak.
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

-- ReplanRenders is the forced refresh after a major update of the service. The whole
-- inventory is replanned in the low queue, spread over days, restoring baselines without a
-- mass alert. It only brings a render forward, never delays one, and leaves assets that
-- left the scheduler alone.
--
-- The spread is the walk order, not a random draw, so a replan run twice lands every asset
-- on the same date instead of reshuffling an inventory already queued.
--
-- @tenant: scoped
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

-- AssetForRender reads the counters a render needs and creates nothing. The upsert path
-- exists because a report discovers things; a render is for an existing asset, and the same
-- statement would let the rendering service invent inventory, the one thing the isolated
-- component must not do.
--
-- @tenant: keyed
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

-- RescheduleRender moves a render out to its next due date. The ordinary direction, unlike
-- PromoteRender (forward only): the render happened, so the asset returns to the low queue
-- at its regime's cadence. A raised priority would keep a once-urgent asset ahead for
-- every later pass.
--
-- @tenant: keyed
-- name: RescheduleRender :exec
UPDATE asset_current SET
    next_fingerprint_at  = CASE WHEN scope_status = 'in_scope' AND lifecycle <> 'archived'
                                THEN @at::timestamptz END,
    fingerprint_priority = @priority::smallint
 WHERE asset_id = @asset_id::uuid;

-- EarnBaseline arms a service's first render, once. Unlike PromoteRender (earlier of two
-- dates, more urgent of two priorities), a baseline is the line being created: it applies
-- only where there is none and must not touch an asset queued for any other reason. The
-- promote path would leave it at the column's default priority and put a mass of first
-- renders in the queue that must stay short.
--
-- @tenant: keyed
-- name: EarnBaseline :exec
UPDATE asset_current SET
    next_fingerprint_at  = @at::timestamptz,
    fingerprint_priority = @priority::smallint
 WHERE asset_id = @asset_id::uuid
   AND scope_status = 'in_scope'
   AND lifecycle <> 'archived'
   AND next_fingerprint_at IS NULL;
