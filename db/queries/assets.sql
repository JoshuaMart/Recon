-- UpsertAsset creates an asset or, when it exists, records the sighting and the
-- scope status in force now. old_scope_status is empty for a new asset.
-- name: UpsertAsset :one
WITH old AS (
  SELECT a.scope_status FROM asset a
  WHERE a.program_id = sqlc.arg(program_id) AND a.kind = sqlc.arg(kind) AND a.key = sqlc.arg(key)
)
INSERT INTO asset (program_id, kind, key, host, port, scheme, parent_asset_id, wildcard_id,
                   discovery_source, discovery_path, scope_status, first_seen, last_seen)
VALUES (sqlc.arg(program_id), sqlc.arg(kind), sqlc.arg(key), sqlc.arg(host), sqlc.narg(port), sqlc.narg(scheme),
        sqlc.narg(parent_asset_id), sqlc.narg(wildcard_id), sqlc.arg(discovery_source), sqlc.arg(discovery_path),
        sqlc.arg(scope_status), sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
ON CONFLICT (program_id, kind, key) DO UPDATE
SET last_seen = CASE WHEN sqlc.arg(sighting)::bool THEN GREATEST(asset.last_seen, EXCLUDED.last_seen) ELSE asset.last_seen END,
    scope_status = EXCLUDED.scope_status
RETURNING asset.id, asset.first_seen, asset.scope_status, COALESCE((SELECT scope_status FROM old), '')::text AS old_scope_status;

-- name: InsertAssetCurrent :exec
INSERT INTO asset_current (asset_id, program_id, kind, key, host, port, scheme, scope_status, lifecycle, first_seen, last_seen)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
ON CONFLICT (asset_id) DO NOTHING;

-- name: GetAssetCurrent :one
SELECT * FROM asset_current WHERE asset_id = $1;

-- name: TouchAssetCurrent :exec
UPDATE asset_current
SET scope_status = $2, last_seen = GREATEST(last_seen, sqlc.arg(now)::timestamptz)
WHERE asset_id = $1;

-- name: SetLifecycle :exec
UPDATE asset_current SET lifecycle = $2, last_changed = sqlc.arg(now)::timestamptz WHERE asset_id = $1;

-- name: SetDNSCurrent :exec
UPDATE asset_current SET dns_state = $2, ips = $3, cname = $4, cdn = sqlc.narg(cdn) WHERE asset_id = $1;

-- name: SetTCPCurrent :exec
UPDATE asset_current SET tcp_state = $2 WHERE asset_id = $1;

-- name: SetHTTPCurrent :exec
UPDATE asset_current
SET http_state = $2, status_code = sqlc.narg(status_code), title = sqlc.narg(title), server = sqlc.narg(server),
    final_url = sqlc.narg(final_url), scheme = COALESCE(sqlc.narg(scheme), scheme), ips = sqlc.narg(ips), cdn = sqlc.narg(cdn)
WHERE asset_id = $1;

-- name: MarkChanged :exec
UPDATE asset_current SET last_changed = sqlc.arg(now)::timestamptz WHERE asset_id = $1;

-- name: ListProgramAssets :many
SELECT id, kind, key, host, port, scheme, scope_status FROM asset WHERE program_id = $1 ORDER BY id;

-- name: SetAssetScope :exec
WITH a AS (UPDATE asset SET scope_status = $2 WHERE asset.id = $1 RETURNING asset.id)
UPDATE asset_current SET scope_status = $2 WHERE asset_id = $1;

-- name: CountWildcardAssets :one
SELECT count(*)::int FROM asset WHERE wildcard_id = $1;
