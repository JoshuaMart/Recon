-- name: GetLayers :many
SELECT * FROM asset_layer WHERE asset_id = $1;

-- name: PutLayer :exec
INSERT INTO asset_layer (asset_id, layer, informative_failures, non_informative_failures, first_failure_at, last_ok_at, last_checked_at)
VALUES ($1, $2, $3, $4, sqlc.narg(first_failure_at), sqlc.narg(last_ok_at), sqlc.arg(now)::timestamptz)
ON CONFLICT (asset_id, layer) DO UPDATE
SET informative_failures = EXCLUDED.informative_failures, non_informative_failures = EXCLUDED.non_informative_failures,
    first_failure_at = EXCLUDED.first_failure_at, last_ok_at = EXCLUDED.last_ok_at, last_checked_at = EXCLUDED.last_checked_at;

-- name: SetSchedule :exec
UPDATE asset_current
SET next_resolve_at = sqlc.narg(next_resolve_at), next_full_at = sqlc.narg(next_full_at), backoff_tier = $2,
    last_checked_at = COALESCE(sqlc.narg(checked_at), last_checked_at)
WHERE asset_id = $1;

-- The program holding the oldest due date, among active programs.
-- name: NextDueProgram :one
SELECT ac.program_id FROM asset_current ac JOIN program p ON p.id = ac.program_id
WHERE p.state = 'active' AND ac.kind = 'fqdn'
  AND (ac.next_full_at <= sqlc.arg(now)::timestamptz OR ac.next_resolve_at <= sqlc.arg(now)::timestamptz)
ORDER BY LEAST(COALESCE(ac.next_full_at, 'infinity'), COALESCE(ac.next_resolve_at, 'infinity')), ac.asset_id
LIMIT 1;

-- name: DueFull :many
SELECT key FROM asset_current
WHERE program_id = $1 AND kind = 'fqdn' AND next_full_at <= sqlc.arg(now)::timestamptz
ORDER BY next_full_at, asset_id
LIMIT sqlc.arg(lim);

-- name: DueResolve :many
SELECT key FROM asset_current
WHERE program_id = $1 AND kind = 'fqdn' AND next_resolve_at <= sqlc.arg(now)::timestamptz
  AND (next_full_at IS NULL OR next_full_at > sqlc.arg(now)::timestamptz)
ORDER BY next_resolve_at, asset_id
LIMIT sqlc.arg(lim);

-- name: EnqueueVerification :one
INSERT INTO run (program_id, kind, stage, targets, trigger, queued_at)
VALUES ($1, 'verification', $2, sqlc.arg(targets)::text[], 'schedule', sqlc.arg(now)::timestamptz)
ON CONFLICT (kind) WHERE kind = 'verification' AND state IN ('queued', 'running') DO NOTHING
RETURNING *;

-- Due dates follow scope and program state: none outside the perimeter or on a
-- paused or archived program.
-- name: ClearProgramSchedule :execrows
UPDATE asset_current ac SET next_resolve_at = NULL, next_full_at = NULL, backoff_tier = 0
FROM program p
WHERE ac.program_id = p.id AND p.id = $1
  AND (p.state <> 'active' OR ac.scope_status <> 'in_scope' OR ac.kind <> 'fqdn' OR ac.lifecycle = 'archived')
  AND (ac.next_resolve_at IS NOT NULL OR ac.next_full_at IS NOT NULL);

-- An in-scope host of an active program with no due date gets a resolve within
-- the jitter window.
-- name: FillProgramSchedule :execrows
UPDATE asset_current ac SET next_resolve_at = sqlc.arg(now)::timestamptz + make_interval(secs => random() * sqlc.arg(jitter_seconds)::float8)
FROM program p
WHERE ac.program_id = p.id AND p.id = $1 AND p.state = 'active'
  AND ac.scope_status = 'in_scope' AND ac.kind = 'fqdn' AND ac.lifecycle <> 'archived'
  AND ac.next_resolve_at IS NULL AND ac.next_full_at IS NULL;

-- Render dates follow the same rule: none for what may not be rendered.
-- name: ClearRenderSchedule :execrows
UPDATE asset_current ac SET next_render_at = NULL, render_priority = 0
FROM program p
WHERE ac.program_id = p.id AND p.id = $1 AND ac.next_render_at IS NOT NULL
  AND (p.state <> 'active' OR ac.scope_status <> 'in_scope' OR ac.lifecycle = 'archived');

-- A web service or declared URL that may be rendered and has no date gets one,
-- low priority: back in the perimeter, or its program active again.
-- name: FillRenderSchedule :execrows
UPDATE asset_current ac
SET next_render_at = sqlc.arg(now)::timestamptz + make_interval(secs => random() * sqlc.arg(jitter_seconds)::float8)
FROM program p
WHERE ac.program_id = p.id AND p.id = $1 AND p.state = 'active'
  AND ac.scope_status = 'in_scope' AND ac.lifecycle <> 'archived' AND ac.next_render_at IS NULL
  AND ((ac.kind = 'service' AND ac.http_state IS NOT NULL) OR ac.kind = 'url');

-- name: ScheduleFullNow :exec
UPDATE asset_current SET next_full_at = sqlc.arg(now)::timestamptz
WHERE program_id = $1 AND kind = 'fqdn' AND key = $2 AND scope_status = 'in_scope';

-- name: ArchiveStale :many
WITH old AS (
  SELECT asset_id, program_id, lifecycle FROM asset_current
  WHERE lifecycle IN ('inactive', 'unobservable') AND lifecycle_since < sqlc.arg(before)::timestamptz
  FOR UPDATE
)
UPDATE asset_current ac
SET lifecycle = 'archived', lifecycle_since = sqlc.arg(now)::timestamptz, next_resolve_at = NULL, next_full_at = NULL,
    backoff_tier = 0, last_changed = sqlc.arg(now)::timestamptz
FROM old WHERE ac.asset_id = old.asset_id
RETURNING old.asset_id, old.program_id, old.lifecycle;

-- name: GetAssetByKey :one
SELECT a.id, a.first_seen, a.scope_status FROM asset a WHERE a.program_id = $1 AND a.kind = $2 AND a.key = $3;

-- name: ListChildServices :many
SELECT a.id, a.port FROM asset a WHERE a.parent_asset_id = $1 AND a.kind = 'service';
