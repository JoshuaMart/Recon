-- name: LastObservation :one
SELECT id, observed_at, outcome, data, (outcome = sqlc.arg(outcome) AND data = sqlc.arg(data)::jsonb)::bool AS same
FROM observation
WHERE asset_id = sqlc.arg(asset_id) AND layer = sqlc.arg(layer)
ORDER BY observed_at DESC, id DESC
LIMIT 1;

-- name: ConfirmObservation :exec
UPDATE observation
SET last_confirmed_at = GREATEST(last_confirmed_at, sqlc.arg(now)::timestamptz), last_producer_version = sqlc.narg(version)
WHERE id = sqlc.arg(id) AND observed_at = sqlc.arg(observed_at);

-- name: InsertObservation :exec
INSERT INTO observation (asset_id, layer, outcome, data, observed_at, last_confirmed_at, run_id, producer_version, last_producer_version)
VALUES (sqlc.arg(asset_id), sqlc.arg(layer), sqlc.arg(outcome), sqlc.arg(data)::jsonb, sqlc.arg(now)::timestamptz,
        sqlc.arg(now)::timestamptz, sqlc.narg(run_id), sqlc.narg(version), sqlc.narg(version));

-- name: InsertChange :exec
INSERT INTO change (asset_id, program_id, kind, layer, before, after, run_id, at)
VALUES (sqlc.arg(asset_id), sqlc.arg(program_id), sqlc.arg(kind), sqlc.narg(layer), sqlc.narg(before)::jsonb,
        sqlc.narg(after)::jsonb, sqlc.narg(run_id), sqlc.arg(now)::timestamptz);

-- name: ListAssetChanges :many
SELECT * FROM change WHERE asset_id = $1 ORDER BY at DESC, id DESC;
