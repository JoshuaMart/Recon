-- A trigger never adds a row: it pulls the date in and raises the priority.
-- Only what can be rendered takes one: an in-scope web service or declared URL
-- of an active program, not archived.
-- name: TriggerRender :execrows
UPDATE asset_current ac
SET next_render_at = LEAST(COALESCE(ac.next_render_at, sqlc.arg(now)::timestamptz), sqlc.arg(now)::timestamptz),
    render_priority = GREATEST(ac.render_priority, sqlc.arg(priority)::smallint)
FROM program p
WHERE ac.asset_id = sqlc.arg(asset_id) AND p.id = ac.program_id AND p.state = 'active'
  AND ac.scope_status = 'in_scope' AND ac.lifecycle <> 'archived'
  AND ((ac.kind = 'service' AND ac.http_state IS NOT NULL)
       OR (ac.kind = 'url' AND EXISTS (
            SELECT 1 FROM asset u JOIN asset_current s ON s.asset_id = u.parent_asset_id
            WHERE u.id = ac.asset_id AND s.http_state IS NOT NULL)));

-- One page of the due renders, in queue order.
-- name: DueRenders :many
SELECT ac.asset_id, ac.program_id, ac.kind, ac.key, ac.host, ac.port, ac.scheme, ac.render_priority, ac.next_render_at
FROM asset_current ac JOIN program p ON p.id = ac.program_id
WHERE ac.next_render_at <= sqlc.arg(now)::timestamptz AND p.state = 'active'
  AND ac.scope_status = 'in_scope' AND ac.lifecycle <> 'archived'
ORDER BY ac.render_priority DESC, ac.next_render_at, ac.asset_id
LIMIT sqlc.arg(lim) OFFSET sqlc.arg(skip);

-- Counted with the same filters as DueRenders, so the depth is what will render.
-- name: RenderQueueDepth :many
SELECT ac.render_priority, count(*)::int AS depth
FROM asset_current ac JOIN program p ON p.id = ac.program_id
WHERE ac.next_render_at <= sqlc.arg(now)::timestamptz AND p.state = 'active'
  AND ac.scope_status = 'in_scope' AND ac.lifecycle <> 'archived'
GROUP BY ac.render_priority ORDER BY ac.render_priority DESC;

-- A render written, with the failures in a row it leaves: none, or one more
-- when the page gave no capture.
-- name: ScheduleRender :exec
UPDATE asset_current SET next_render_at = sqlc.arg(next)::timestamptz, render_priority = 0,
  last_render_at = COALESCE(sqlc.narg(rendered_at), last_render_at), render_failures = sqlc.arg(failures)::smallint
WHERE asset_id = sqlc.arg(asset_id);

-- A target the browser could not render: one more failure in a row, which
-- the caller turns into the next date.
-- name: RecordRenderFailure :one
UPDATE asset_current SET render_failures = LEAST(render_failures + 1, 32767), render_priority = 0
WHERE asset_id = $1
RETURNING render_failures;

-- name: SetNextRender :exec
UPDATE asset_current SET next_render_at = sqlc.arg(next)::timestamptz WHERE asset_id = sqlc.arg(asset_id);

-- name: SetRenderCurrent :exec
UPDATE asset_current
SET redirect_hosts = sqlc.narg(redirect_hosts), header_names = sqlc.narg(header_names),
    cookie_names = sqlc.narg(cookie_names), script_hashes = sqlc.narg(script_hashes),
    favicon_hash = sqlc.narg(favicon_hash), external_hosts = sqlc.narg(external_hosts),
    web_sockets = sqlc.narg(web_sockets), has_robots = sqlc.narg(has_robots),
    has_sitemap = sqlc.narg(has_sitemap), has_llms_txt = sqlc.narg(has_llms_txt), waf = sqlc.narg(waf),
    headers = sqlc.narg(headers)::jsonb
WHERE asset_id = sqlc.arg(asset_id);

-- name: ListTech :many
SELECT * FROM asset_tech WHERE asset_id = $1 ORDER BY name;

-- name: DeleteTech :exec
DELETE FROM asset_tech WHERE asset_id = $1 AND name = $2;

-- name: PutTech :exec
INSERT INTO asset_tech (asset_id, name, category, version_raw, version, cpe, proof)
VALUES ($1, $2, $3, $4, sqlc.narg(version)::int[], sqlc.narg(cpe), sqlc.narg(proof)::jsonb)
ON CONFLICT (asset_id, name) DO UPDATE
SET category = EXCLUDED.category, version_raw = EXCLUDED.version_raw, version = EXCLUDED.version,
    cpe = EXCLUDED.cpe, proof = EXCLUDED.proof;

-- name: LastScreenshot :one
SELECT * FROM screenshot WHERE asset_id = $1 ORDER BY taken_at DESC, id DESC LIMIT 1;

-- name: InsertScreenshot :one
INSERT INTO screenshot (asset_id, key, thumb_key, phash, bytes, width, height, taken_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, sqlc.arg(now)::timestamptz)
RETURNING *;

-- The screenshots of an asset beyond the newest n, to delete.
-- name: ScreenshotsBeyond :many
SELECT * FROM screenshot WHERE asset_id = $1 ORDER BY taken_at DESC, id DESC OFFSET sqlc.arg(keep);

-- name: DeleteScreenshot :exec
DELETE FROM screenshot WHERE id = $1;

-- name: ListScreenshots :many
SELECT * FROM screenshot WHERE asset_id = $1 ORDER BY taken_at DESC, id DESC;

-- name: ScreenshotKeysInUse :many
SELECT k::text FROM unnest(sqlc.arg(keys)::text[]) AS k
WHERE EXISTS (SELECT 1 FROM screenshot s WHERE s.key = k OR s.thumb_key = k);

-- name: ScreenshotKeyInUse :one
SELECT EXISTS (SELECT 1 FROM screenshot WHERE key = $1 OR thumb_key = $1)::bool;

-- name: ScreenshotStorage :one
SELECT count(*)::int AS count, COALESCE(sum(bytes), 0)::bigint AS bytes FROM screenshot;

-- name: GetRenderVersion :one
SELECT producer_version FROM render_state;

-- name: SetRenderVersion :exec
UPDATE render_state SET producer_version = $1;

-- A new major makes every rendered asset due, low priority.
-- name: RerenderAll :execrows
UPDATE asset_current SET next_render_at = LEAST(COALESCE(next_render_at, sqlc.arg(now)::timestamptz), sqlc.arg(now)::timestamptz)
WHERE last_render_at IS NOT NULL;


-- Declared URLs on a service earn their render once the service answers.
-- name: TriggerChildURLs :execrows
UPDATE asset_current ac
SET next_render_at = LEAST(COALESCE(ac.next_render_at, sqlc.arg(now)::timestamptz), sqlc.arg(now)::timestamptz),
    render_priority = GREATEST(ac.render_priority, sqlc.arg(priority)::smallint)
FROM asset a, program p
WHERE a.id = ac.asset_id AND a.parent_asset_id = sqlc.arg(service_id) AND ac.kind = 'url'
  AND p.id = ac.program_id AND p.state = 'active' AND ac.scope_status = 'in_scope' AND ac.lifecycle <> 'archived';
